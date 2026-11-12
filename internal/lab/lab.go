// Package lab is a harness for running interleaved multi-session SQL experiments.
//
// Each Session is a dedicated DB connection driven by its own worker goroutine.
// A test issues statements along a timeline with s.Do(sql):
//   - if the statement finishes quickly, Do returns its result;
//   - if it gets stuck on a lock, the harness confirms with the DB that the session
//     is really waiting for a lock and returns immediately with op.Blocked = true,
//     so the test can keep driving other sessions. When the statement is later
//     unblocked, its result is printed on the timeline as "↳ #N done".
//
// After every step the harness waits until all sessions are quiescent (idle, or
// confirmed by the DB to be waiting for a lock), so interleavings are deterministic
// without guessing with sleeps.
package lab

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func init() {
	// Sessions are KILLed server-side at cleanup; don't log the resulting "unexpected EOF".
	_ = mysql.SetLogger(mysql.Logger(nopLogger{}))
}

type nopLogger struct{}

func (nopLogger) Print(...any) {}

type Kind string

const (
	Postgres Kind = "postgres"
	MySQL    Kind = "mysql"
)

func (k Kind) String() string { return string(k) }

type Level string

const (
	ReadUncommitted Level = "READ UNCOMMITTED"
	ReadCommitted   Level = "READ COMMITTED"
	RepeatableRead  Level = "REPEATABLE READ"
	Serializable    Level = "SERIALIZABLE"
)

var AllLevels = []Level{ReadUncommitted, ReadCommitted, RepeatableRead, Serializable}

// Short is the abbreviation used in result tables.
func (l Level) Short() string {
	switch l {
	case ReadUncommitted:
		return "RU"
	case ReadCommitted:
		return "RC"
	case RepeatableRead:
		return "RR"
	case Serializable:
		return "SER"
	}
	return string(l)
}

const (
	// stepTimeout: a statement that neither finishes nor is seen waiting for a lock
	// within this time is reported as "still running".
	stepTimeout = 5 * time.Second
	// waitTimeout: how long op.Wait() waits before failing the test.
	waitTimeout = 15 * time.Second
	// Lower PG's deadlock_timeout (default 1s) so deadlocks are detected quickly.
	pgDeadlockTimeout = 100 * time.Millisecond
)

var (
	poolsMu sync.Mutex
	pools   = map[string]*sql.DB{}
)

func dsn(kind Kind, dbname string) string {
	switch kind {
	case Postgres:
		base := envOr("LAB_PG_DSN", "postgres://postgres:lab@127.0.0.1:55432/%s?sslmode=disable")
		return withParams(fmt.Sprintf(base, dbname), "default_query_exec_mode=simple_protocol")
	case MySQL:
		base := envOr("LAB_MYSQL_DSN", "root:lab@tcp(127.0.0.1:53306)/%s")
		return withParams(fmt.Sprintf(base, dbname), "interpolateParams=true&parseTime=true")
	}
	panic("unknown kind " + kind)
}

// DSN returns the DSN of database dbname, for experiments that need a connection of their
// own, outside the shared pool.
func DSN(kind Kind, dbname string) string { return dsn(kind, dbname) }

// DSNAs returns the DSN of database dbname with the user and password replaced, for
// experiments that need a connection with fewer privileges.
func DSNAs(kind Kind, dbname, user, password string) string {
	d := dsn(kind, dbname)
	start := 0
	if kind == Postgres {
		start = strings.Index(d, "://") + 3
	}
	// The credentials end at the LAST '@' before the path: a password may contain '@'.
	at := start + strings.LastIndex(d[start:start+strings.Index(d[start:], "/")], "@")
	return d[:start] + user + ":" + password + d[at:]
}

// withParams appends query parameters to a DSN that may or may not already have some.
func withParams(dsn, params string) string {
	if strings.Contains(dsn, "?") {
		return dsn + "&" + params
	}
	return dsn + "?" + params
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func driverName(kind Kind) string {
	if kind == Postgres {
		return "pgx"
	}
	return "mysql"
}

// openPool returns (creating if needed) the pool for a database. A missing database
// is created through the bootstrap database "lab".
func openPool(t testing.TB, kind Kind, dbname string) *sql.DB {
	t.Helper()
	poolsMu.Lock()
	defer poolsMu.Unlock()
	key := string(kind) + "/" + dbname
	if p, ok := pools[key]; ok {
		return p
	}
	boot, err := sql.Open(driverName(kind), dsn(kind, "lab"))
	if err != nil {
		t.Fatal(err)
	}
	defer boot.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := boot.PingContext(ctx); err != nil {
		t.Skipf("cannot connect to %s (run `make up` first): %v", kind, err)
	}
	switch kind {
	case Postgres:
		var exists bool
		_ = boot.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)", dbname).Scan(&exists)
		if !exists {
			if _, err := boot.ExecContext(ctx, "CREATE DATABASE "+dbname); err != nil {
				t.Fatal(err)
			}
		}
	case MySQL:
		if _, err := boot.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS "+dbname); err != nil {
			t.Fatal(err)
		}
	}
	p, err := sql.Open(driverName(kind), dsn(kind, dbname))
	if err != nil {
		t.Fatal(err)
	}
	pools[key] = p
	return p
}

// Lab is the environment of one experiment: an admin pool, some Sessions and a timeline.
type Lab struct {
	T    testing.TB
	Kind Kind
	// Admin is an autocommit pool used for setup, lock inspection and final checks.
	Admin *sql.DB

	mu       sync.Mutex
	seq      int
	sessions []*Session
	quiet    bool     // guarded by mu: deferLine runs on worker goroutines
	deferred []string // "unblocked" messages, printed after the step that caused them
	history  []*Op
}

// New creates an experiment. Each package passes its own dbname so that
// `go test ./...` running packages in parallel does not make them interfere.
func New(t testing.TB, kind Kind, dbname string) *Lab {
	t.Helper()
	return &Lab{T: t, Kind: kind, Admin: openPool(t, kind, dbname)}
}

// Quiet disables timeline output; Loud re-enables it. "↳ done" messages of statements
// unblocked while quiet are dropped, not printed after Loud as part of a later step.
func (l *Lab) Quiet() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.flushLocked()
	l.quiet = true
}

func (l *Lab) Loud() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.quiet = false
}

// History returns every op issued in this experiment, in issue order.
func (l *Lab) History() []*Op {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]*Op(nil), l.history...)
}

func (l *Lab) printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.quiet {
		return
	}
	l.flushLocked()
	fmt.Fprintf(l.T.Output(), format+"\n", args...)
}

// printStep prints a step's line, then any "unblocked" messages the step caused.
func (l *Lab) printStep(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.quiet {
		return
	}
	fmt.Fprintln(l.T.Output(), line)
	l.flushLocked()
}

func (l *Lab) deferLine(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.quiet {
		return
	}
	l.deferred = append(l.deferred, line)
}

func (l *Lab) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.quiet {
		return
	}
	l.flushLocked()
}

func (l *Lab) flushLocked() {
	for _, d := range l.deferred {
		fmt.Fprintln(l.T.Output(), d)
	}
	l.deferred = nil
}

// Note inserts an explanation into the timeline, formatted as a SQL comment.
func (l *Lab) Note(format string, args ...any) {
	for _, line := range strings.Split(fmt.Sprintf(format, args...), "\n") {
		l.printf("     -- %s", line)
	}
}

// Exec runs statements on the admin pool (autocommit); any error fails the test.
func (l *Lab) Exec(stmts ...string) {
	l.T.Helper()
	for _, s := range stmts {
		if _, err := l.Admin.Exec(s); err != nil {
			l.T.Fatalf("setup %q: %v", s, err)
		}
	}
}

// Query runs a query on the admin pool and returns the rows as strings.
func (l *Lab) Query(q string, args ...any) [][]string {
	l.T.Helper()
	rows, err := l.Admin.Query(q, args...)
	if err != nil {
		l.T.Fatalf("query %q: %v", q, err)
	}
	_, out, err := scanAll(rows)
	if err != nil {
		l.T.Fatalf("query %q: %v", q, err)
	}
	return out
}

// QueryString returns the first column of the first row.
func (l *Lab) QueryString(q string, args ...any) string {
	l.T.Helper()
	r := l.Query(q, args...)
	if len(r) == 0 || len(r[0]) == 0 {
		return ""
	}
	return r[0][0]
}

// Show runs a query on the admin pool and prints the result as a table on the timeline.
func (l *Lab) Show(q string, args ...any) [][]string {
	return l.ShowAs("", q, args...)
}

// ShowAs is Show with a title printed instead of the query text.
func (l *Lab) ShowAs(title, q string, args ...any) [][]string {
	l.T.Helper()
	rows, err := l.Admin.Query(q, args...)
	if err != nil {
		l.T.Fatalf("query %q: %v", q, err)
	}
	cols, out, err := scanAll(rows)
	if err != nil {
		l.T.Fatalf("query %q: %v", q, err)
	}
	if title == "" {
		title = strings.Join(strings.Fields(q), " ")
	}
	l.printf("     ┌─ %s", title)
	l.printf("     │ %s", strings.Join(cols, " | "))
	for _, r := range out {
		l.printf("     │ %s", strings.Join(r, " | "))
	}
	l.printf("     └─")
	return out
}

// Begin starts a transaction at the given isolation level using each DB's syntax.
func (s *Session) Begin(level Level) *Op {
	if s.lab.Kind == MySQL {
		// MySQL: SET TRANSACTION applies to the next transaction only.
		s.Do("SET TRANSACTION ISOLATION LEVEL " + string(level))
		return s.Do("START TRANSACTION")
	}
	return s.Do("BEGIN ISOLATION LEVEL " + string(level))
}

func (l *Lab) nextSeq() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	return l.seq
}

// settle waits until every session is idle or confirmed waiting for a lock.
// If two or more sessions are waiting at the same time (a possible deadlock),
// it waits a little longer so the DB's deadlock detector can run first.
func (l *Lab) settle() {
	deadline := time.Now().Add(stepTimeout)
	extraWaited := false
	for time.Now().Before(deadline) {
		blocked, busy := 0, 0
		for _, s := range l.sessions {
			switch s.state() {
			case stateBlocked:
				blocked++
			case stateRunning:
				busy++
			}
		}
		if busy == 0 {
			if blocked < 2 || extraWaited {
				l.flush()
				return
			}
			extraWaited = true
			if l.Kind == Postgres {
				time.Sleep(4 * pgDeadlockTimeout)
			} else {
				time.Sleep(50 * time.Millisecond)
			}
			continue
		}
		time.Sleep(10 * time.Millisecond)
	}
	l.flush()
}

// Settle waits until every session is quiescent.
func (l *Lab) Settle() { l.settle() }

// WaitAll waits for every issued op to finish, including blocked ones.
func (l *Lab) WaitAll() {
	l.T.Helper()
	for _, s := range l.sessions {
		s.waitIdle(l.T)
	}
	l.flush()
}
