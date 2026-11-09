package lab

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Session is a dedicated connection (= one PG backend / one MySQL thread) that runs Ops in order.
type Session struct {
	lab  *Lab
	Name string
	idx  int
	conn *sql.Conn
	// ID is the PG backend pid or the MySQL connection id, used to observe the session from the DB side.
	ID int64

	ctx       context.Context
	cancel    context.CancelFunc
	ops       chan *Op
	exited    chan struct{}
	closeOnce sync.Once
	closing   atomic.Bool

	mu       sync.Mutex
	inflight *Op
	pending  int  // ops issued but not finished yet
	inTx     bool // between BEGIN and COMMIT/ROLLBACK
	abortErr error
}

// Session opens a new connection.
func (l *Lab) Session(name string) *Session {
	l.T.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	conn, err := l.Admin.Conn(ctx)
	if err != nil {
		cancel()
		l.T.Fatal(err)
	}
	s := &Session{
		lab: l, Name: name, idx: len(l.sessions), conn: conn,
		ctx: ctx, cancel: cancel,
		ops: make(chan *Op, 64), exited: make(chan struct{}),
	}
	switch l.Kind {
	case Postgres:
		mustExecConn(l.T, conn, "SET deadlock_timeout = '"+pgDeadlockTimeout.String()+"'")
		mustExecConn(l.T, conn, "SET application_name = 'lab:"+name+"'")
		_ = conn.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&s.ID)
	case MySQL:
		mustExecConn(l.T, conn, "SET SESSION innodb_lock_wait_timeout = 10")
		_ = conn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&s.ID)
	}
	l.sessions = append(l.sessions, s)
	go s.worker()
	l.T.Cleanup(s.close)
	return s
}

func mustExecConn(t testing.TB, c *sql.Conn, q string) {
	t.Helper()
	if _, err := c.ExecContext(context.Background(), q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func (s *Session) close() { s.closeOnce.Do(s.doClose) }

// ErrSessionClosed replaces the driver error of a statement that was still running (usually
// still blocked) when the test ended and the harness killed its session.
var ErrSessionClosed = errors.New("still running when the test ended; session closed")

func (s *Session) doClose() {
	s.closing.Store(true)
	// Kill the connection server-side first: if it is stuck waiting for a lock,
	// closing the socket alone does not make the server release its locks promptly.
	switch s.lab.Kind {
	case Postgres:
		_, _ = s.lab.Admin.Exec("SELECT pg_terminate_backend($1)", s.ID)
	case MySQL:
		_, _ = s.lab.Admin.Exec(fmt.Sprintf("KILL %d", s.ID))
	}
	s.cancel()
	close(s.ops)
	<-s.exited
	s.lab.flush()
	// The connection is dead server-side; discard it instead of returning it to the pool.
	_ = s.conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = s.conn.Close()
}

type sessState int

const (
	stateIdle sessState = iota
	stateRunning
	stateBlocked
)

func (s *Session) state() sessState {
	s.mu.Lock()
	pending := s.pending
	s.mu.Unlock()
	if pending == 0 {
		return stateIdle
	}
	if s.WaitingForLock() {
		return stateBlocked
	}
	// Re-check: the op may have finished while we were asking the DB.
	s.mu.Lock()
	pending = s.pending
	s.mu.Unlock()
	if pending == 0 {
		return stateIdle
	}
	return stateRunning
}

// WaitingForLock asks the DB whether this connection is currently waiting for a lock.
func (s *Session) WaitingForLock() bool {
	var waiting bool
	switch s.lab.Kind {
	case Postgres:
		// wait_event_type = 'Lock' covers heavyweight locks: relation / tuple / transactionid / advisory ...
		_ = s.lab.Admin.QueryRow(
			"SELECT coalesce(wait_event_type = 'Lock', false) FROM pg_stat_activity WHERE pid = $1", s.ID,
		).Scan(&waiting)
	case MySQL:
		// InnoDB row-lock waits are in performance_schema.data_lock_waits; MDL / table / user-lock
		// waits show up in the processlist STATE ("Waiting for table metadata lock", ...).
		// Match case-sensitively: "waiting for handler commit" is a normal commit, not a lock wait.
		// Do NOT poll information_schema.innodb_trx: it is a cache that only refreshes when
		// it has not been read for 100ms, so high-frequency polling keeps it stale forever.
		_ = s.lab.Admin.QueryRow(`
SELECT EXISTS(SELECT 1 FROM performance_schema.data_lock_waits w
              JOIN performance_schema.threads t ON t.THREAD_ID = w.REQUESTING_THREAD_ID
              WHERE t.PROCESSLIST_ID = ?)
    OR EXISTS(SELECT 1 FROM performance_schema.processlist WHERE ID = ?
              AND (STATE COLLATE utf8mb4_bin LIKE 'Waiting for %lock' OR STATE = 'User lock'))`,
			s.ID, s.ID).Scan(&waiting)
	}
	return waiting
}

func (s *Session) waitIdle(t testing.TB) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		p := s.pending
		s.mu.Unlock()
		if p == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s: ops still unfinished after %v (stuck?)", s.Name, waitTimeout)
}

// Op is one SQL statement sent to a Session, plus its result.
type Op struct {
	S    *Session
	Seq  int
	SQL  string
	Args []any

	done     chan struct{}
	mu       sync.Mutex
	detached bool // Do() already returned (blocked); print the result separately when done

	// Results; read only after done.
	Cols     []string
	Rows     [][]string
	Affected int64
	Err      error
	Skipped  bool // the transaction had already been aborted by the DB; not sent
	Blocked  bool // was observed waiting for a lock
	Elapsed  time.Duration
}

// Do sends a statement, waits until it either finishes or is confirmed blocked,
// then waits for all sessions to become quiescent before returning.
func (s *Session) Do(query string, args ...any) *Op {
	s.lab.T.Helper()
	op := &Op{S: s, Seq: s.lab.nextSeq(), SQL: query, Args: args, done: make(chan struct{})}
	s.lab.mu.Lock()
	s.lab.history = append(s.lab.history, op)
	s.lab.mu.Unlock()
	s.mu.Lock()
	s.pending++
	queuedBehind := s.pending > 1
	s.mu.Unlock()
	s.ops <- op

	deadline := time.Now().Add(stepTimeout)
	for {
		select {
		case <-op.done:
			s.lab.printStep(s.prefix(op.Seq) + op.SQL + op.resultSuffix())
			s.lab.settle()
			return op
		default:
		}
		if queuedBehind {
			op.markDetached(func() {
				s.lab.printf("%s%s   ⏳ queued behind a blocked statement", s.prefix(op.Seq), op.SQL)
			})
			s.lab.settle()
			return op
		}
		if s.isInflight(op) && s.WaitingForLock() {
			op.Blocked = true
			if op.markDetached(func() {
				s.lab.printf("%s%s   ⏳ BLOCKED (waiting for lock)", s.prefix(op.Seq), op.SQL)
			}) {
				s.lab.settle()
				return op
			}
			// It finished just now; loop around to print the result.
			continue
		}
		if time.Now().After(deadline) {
			op.markDetached(func() {
				s.lab.printf("%s%s   … still running after %v", s.prefix(op.Seq), op.SQL, stepTimeout)
			})
			return op
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// markDetached marks op as detached and prints msg if it has not finished yet; reports whether it did.
func (op *Op) markDetached(msg func()) bool {
	op.mu.Lock()
	defer op.mu.Unlock()
	select {
	case <-op.done:
		return false
	default:
	}
	op.detached = true
	msg()
	return true
}

func (s *Session) isInflight(op *Op) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inflight == op
}

func (s *Session) prefix(seq int) string {
	return fmt.Sprintf("%3d  %s%s │ ", seq, strings.Repeat("        ", s.idx), s.Name)
}

func (s *Session) worker() {
	defer close(s.exited)
	for op := range s.ops {
		s.mu.Lock()
		s.inflight = op
		s.mu.Unlock()

		s.run(op)
		if s.closing.Load() && op.Err != nil && !op.Skipped {
			// Not a result of the experiment: the harness killed the connection at cleanup.
			op.Err = ErrSessionClosed
		}

		// Mark the session idle BEFORE signalling done: whoever waits on op.done may
		// immediately issue the next Do, which must not see this op as still pending.
		s.mu.Lock()
		s.inflight = nil
		s.pending--
		s.mu.Unlock()

		op.mu.Lock()
		close(op.done)
		detached := op.detached
		op.mu.Unlock()
		if detached {
			s.lab.deferLine(fmt.Sprintf("     %s%s │ ↳ #%d done (waited %v)%s", strings.Repeat("        ", s.idx), s.Name,
				op.Seq, op.Elapsed.Round(time.Millisecond), op.resultSuffix()))
		}
	}
}

func isTxStart(q string) bool {
	u := strings.ToUpper(strings.TrimSpace(q))
	return strings.HasPrefix(u, "BEGIN") || strings.HasPrefix(u, "START TRANSACTION")
}

func isTxEnd(q string) bool {
	u := strings.ToUpper(strings.TrimSpace(q))
	return strings.HasPrefix(u, "COMMIT") || strings.HasPrefix(u, "ABORT") ||
		strings.HasPrefix(u, "ROLLBACK") && !isRollbackToSavepoint(q)
}

// isRollbackToSavepoint matches ROLLBACK [WORK | TRANSACTION] TO [SAVEPOINT] name,
// which undoes part of the transaction but does not end it.
func isRollbackToSavepoint(q string) bool {
	f := strings.Fields(strings.ToUpper(q))
	if len(f) < 3 || f[0] != "ROLLBACK" {
		return false
	}
	if f[1] == "WORK" || f[1] == "TRANSACTION" {
		f = f[1:]
	}
	return f[1] == "TO"
}

func isSetTransaction(q string) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(q)), "SET TRANSACTION")
}

func isQuery(q string) bool {
	u := strings.ToUpper(strings.TrimSpace(q))
	for _, p := range []string{"SELECT", "SHOW", "WITH", "VALUES", "TABLE", "EXPLAIN"} {
		if strings.HasPrefix(u, p) {
			return true
		}
	}
	return strings.Contains(u, "RETURNING")
}

func (s *Session) run(op *Op) {
	start := time.Now()
	defer func() { op.Elapsed = time.Since(start) }()

	s.mu.Lock()
	abortErr := s.abortErr
	s.mu.Unlock()

	// The transaction was aborted by the DB. On MySQL a deadlock victim's whole transaction
	// is rolled back and any following statement would silently run in autocommit mode,
	// corrupting the experiment, so skip everything until the transaction ends.
	if abortErr != nil {
		switch {
		case isTxEnd(op.SQL):
			_, _ = s.conn.ExecContext(s.ctx, "ROLLBACK")
			s.mu.Lock()
			s.abortErr, s.inTx = nil, false
			s.mu.Unlock()
			op.Skipped, op.Err = true, abortErr
			return
		case s.lab.Kind == MySQL && (isTxStart(op.SQL) || isSetTransaction(op.SQL)):
			// On MySQL the aborted transaction is already gone, so the application starting
			// its retry (SET TRANSACTION / START TRANSACTION) begins a fresh one; run it.
			// (On PG the session stays in the aborted transaction until ROLLBACK.)
			s.mu.Lock()
			s.abortErr, s.inTx = nil, false
			s.mu.Unlock()
		case s.lab.Kind == Postgres && isRollbackToSavepoint(op.SQL):
			// PG leaves the aborted state by rolling back to a savepoint taken before the error.
			// (On MySQL a deadlock already rolled back the whole transaction, savepoints included.)
		default:
			op.Skipped, op.Err = true, abortErr
			return
		}
	}

	if isQuery(op.SQL) {
		rows, err := s.conn.QueryContext(s.ctx, op.SQL, op.Args...)
		if err == nil {
			op.Cols, op.Rows, err = scanAll(rows)
		}
		op.Err = err
	} else {
		res, err := s.conn.ExecContext(s.ctx, op.SQL, op.Args...)
		if err == nil {
			op.Affected, _ = res.RowsAffected()
		}
		op.Err = err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case isTxStart(op.SQL) && op.Err == nil:
		s.inTx = true
	case isTxEnd(op.SQL):
		s.inTx = false
	case isRollbackToSavepoint(op.SQL) && op.Err == nil:
		s.abortErr = nil
	case op.Err != nil && s.inTx && s.abortsTx(op.Err):
		s.abortErr = op.Err
	}
}

// abortsTx reports whether err invalidates the whole transaction.
//   - PG: any error inside a transaction puts it in the aborted state (only ROLLBACK is accepted).
//   - MySQL: only a deadlock (1213) rolls back the whole transaction; a lock wait
//     timeout (1205) rolls back just the statement by default.
func (s *Session) abortsTx(err error) bool {
	if s.lab.Kind == Postgres {
		return true
	}
	return IsDeadlock(err)
}

func scanAll(rows *sql.Rows) ([]string, [][]string, error) {
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, nil, err
	}
	var out [][]string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, nil, err
		}
		row := make([]string, len(cols))
		for i, v := range vals {
			row[i] = formatValue(v)
		}
		out = append(out, row)
	}
	return cols, out, rows.Err()
}

func formatValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return string(x)
	case time.Time:
		return x.Format(time.RFC3339)
	default:
		return fmt.Sprint(x)
	}
}

func (op *Op) resultSuffix() string {
	switch {
	case op.Skipped:
		if isTxEnd(op.SQL) {
			return "   → transaction was already aborted; ROLLBACK issued instead"
		}
		return "   → (skipped: transaction already aborted)"
	case errors.Is(op.Err, ErrSessionClosed):
		return "   → (" + ErrSessionClosed.Error() + ")"
	case op.Err != nil:
		return "   ✗ " + Describe(op.Err)
	case op.Cols != nil:
		return "   → " + FormatRows(op.Rows)
	case op.Affected > 0 || isDML(op.SQL):
		return fmt.Sprintf("   → %d row(s)", op.Affected)
	default:
		return ""
	}
}

func isDML(q string) bool {
	u := strings.ToUpper(strings.TrimSpace(q))
	for _, p := range []string{"INSERT", "UPDATE", "DELETE"} {
		if strings.HasPrefix(u, p) {
			return true
		}
	}
	return false
}

// FormatRows renders rows as "(1,10) (2,20)".
func FormatRows(rows [][]string) string {
	if len(rows) == 0 {
		return "(no rows)"
	}
	parts := make([]string, len(rows))
	for i, r := range rows {
		parts[i] = "(" + strings.Join(r, ",") + ")"
	}
	return strings.Join(parts, " ")
}

// ---- Op result accessors and assertions ----

// Wait waits until the op has really finished (including after being unblocked).
func (op *Op) Wait() *Op {
	op.S.lab.T.Helper()
	select {
	case <-op.done:
	case <-time.After(waitTimeout):
		op.S.lab.T.Fatalf("#%d %s: %q not finished after %v", op.Seq, op.S.Name, op.SQL, waitTimeout)
	}
	op.S.lab.flush()
	return op
}

// Done reports whether the op has finished, without waiting.
func (op *Op) Done() bool {
	select {
	case <-op.done:
		return true
	default:
		return false
	}
}

// OK waits for the op and reports whether it succeeded (no error, not skipped).
func (op *Op) OK() bool {
	op.Wait()
	return op.Err == nil && !op.Skipped
}

// MustOK asserts the op succeeded.
func (op *Op) MustOK() *Op {
	t := op.S.lab.T
	t.Helper()
	op.Wait()
	if op.Err != nil {
		t.Fatalf("#%d %s: %q expected success, got %s", op.Seq, op.S.Name, op.SQL, Describe(op.Err))
	}
	return op
}

// MustBlock asserts the op is currently blocked on a lock.
func (op *Op) MustBlock() *Op {
	t := op.S.lab.T
	t.Helper()
	if op.Done() {
		t.Fatalf("#%d %s: %q expected to block, but it finished%s", op.Seq, op.S.Name, op.SQL, op.resultSuffix())
	}
	if !op.Blocked {
		t.Fatalf("#%d %s: %q expected to block, but no lock wait was observed", op.Seq, op.S.Name, op.SQL)
	}
	return op
}

// MustNotBlock asserts the op finished immediately (successfully or not).
func (op *Op) MustNotBlock() *Op {
	t := op.S.lab.T
	t.Helper()
	if !op.Done() || op.Blocked {
		t.Fatalf("#%d %s: %q expected not to block, but it blocked", op.Seq, op.S.Name, op.SQL)
	}
	return op
}

// MustFail asserts the op failed with an error matching match (nil matches any error).
func (op *Op) MustFail(match func(error) bool) *Op {
	t := op.S.lab.T
	t.Helper()
	op.Wait()
	if op.Err == nil {
		t.Fatalf("#%d %s: %q expected to fail, but succeeded%s", op.Seq, op.S.Name, op.SQL, op.resultSuffix())
	}
	if match != nil && !match(op.Err) {
		t.Fatalf("#%d %s: %q unexpected error: %s", op.Seq, op.S.Name, op.SQL, Describe(op.Err))
	}
	return op
}

// MustRows asserts the query result, compared in FormatRows format.
func (op *Op) MustRows(want string) *Op {
	t := op.S.lab.T
	t.Helper()
	op.MustOK()
	if got := FormatRows(op.Rows); got != want {
		t.Fatalf("#%d %s: %q\n  got  %s\n  want %s", op.Seq, op.S.Name, op.SQL, got, want)
	}
	return op
}

// MustAffect asserts the number of affected rows.
func (op *Op) MustAffect(n int64) *Op {
	t := op.S.lab.T
	t.Helper()
	op.MustOK()
	if op.Affected != n {
		t.Fatalf("#%d %s: %q affected %d row(s), want %d", op.Seq, op.S.Name, op.SQL, op.Affected, n)
	}
	return op
}

// Int returns the first column of the first row as an int.
func (op *Op) Int() int {
	op.Wait()
	if len(op.Rows) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(op.Rows[0][0])
	return n
}

// Str returns the first column of the first row.
func (op *Op) Str() string {
	op.Wait()
	if len(op.Rows) == 0 {
		return ""
	}
	return op.Rows[0][0]
}

// Text returns the result in FormatRows format.
func (op *Op) Text() string {
	op.Wait()
	return FormatRows(op.Rows)
}
