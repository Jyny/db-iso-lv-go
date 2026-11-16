// Package hermitage ports Martin Kleppmann's Hermitage (https://github.com/ept/hermitage)
// to self-checking Go tests: every anomaly runs on every DB × isolation level, and the test
// decides whether the anomaly happened and, if it was prevented, by which mechanism:
//
//	mvcc     : snapshot reads, the transaction simply never sees the other's writes
//	lock     : a transaction was blocked by a lock and forced to queue (serial execution)
//	abort    : the DB detected a conflict and failed a transaction (serialization failure)
//	deadlock : locking produced a deadlock and the DB killed one transaction
//
// Run: go test ./hermitage -v -run 'TestHermitage/mysql/RR/P4'
package hermitage

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jyny/db-iso-lv-go/internal/lab"
)

const dbname = "lab_hermitage"

type env struct {
	L          *lab.Lab
	level      lab.Level
	T1, T2, T3 *lab.Session
}

// scenario runs one Hermitage test case and reports whether the anomaly was observed.
// The statements are the documents' own (postgres.md / mysql.md), character for character,
// in the documented order. Where the two documents differ, each DB runs its own document's
// script (e.pg()); where one document changes the order per level, the scenario says which
// block it follows. The per-level blocks themselves are replayed by TestHermitageDocs.
type scenario struct {
	name string
	desc string
	run  func(e *env) bool
}

// begin starts a transaction in each session with the documents' own preamble.
func (e *env) begin(ss ...*lab.Session) {
	level := strings.ToLower(string(e.level))
	for _, s := range ss {
		if e.pg() {
			s.Do("begin")
			s.Do("set transaction isolation level " + level)
		} else {
			s.Do("set session transaction isolation level " + level)
			s.Do("begin")
		}
	}
}

func (e *env) pg() bool { return e.L.Kind == lab.Postgres }

func (e *env) final() string {
	return lab.FormatRows(e.L.Query("SELECT id, value FROM test ORDER BY id"))
}

// committed reports whether the session's last COMMIT succeeded (not skipped, no error).
func committed(s *lab.Session, L *lab.Lab) bool {
	h := L.History()
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].S == s && strings.EqualFold(h[i].SQL, "commit") {
			return h[i].OK()
		}
	}
	return false
}

var scenarios = []scenario{
	{
		name: "G0", desc: "Write Cycles (dirty write): two transactions overwrite each other's uncommitted data",
		run: func(e *env) bool {
			e.begin(e.T1, e.T2)
			e.T1.Do("update test set value = 11 where id = 1")
			e.T2.Do("update test set value = 12 where id = 1")
			e.T1.Do("update test set value = 21 where id = 2")
			e.T1.Do("commit")
			e.T1.Do("select * from test")
			e.T2.Do("update test set value = 22 where id = 2")
			e.T2.Do("commit")
			e.T1.Do("select * from test") // the documents' "either"
			e.L.WaitAll()
			f := e.final()
			// a mix of both transactions' writes = anomaly
			return f == "(1,12) (2,21)" || f == "(1,11) (2,22)"
		},
	},
	{
		name: "G1a", desc: "Aborted Reads (dirty read): reading data that is later rolled back",
		run: func(e *env) bool {
			e.begin(e.T1, e.T2)
			e.T1.Do("update test set value = 101 where id = 1")
			r1 := e.T2.Do("select * from test")
			if e.pg() {
				e.T1.Do("abort")
			} else {
				e.T1.Do("rollback")
			}
			r2 := e.T2.Do("select * from test")
			e.T2.Do("commit")
			e.L.WaitAll()
			return strings.Contains(r1.Text(), "101") || strings.Contains(r2.Text(), "101")
		},
	},
	{
		name: "G1b", desc: "Intermediate Reads: reading another transaction's intermediate state",
		run: func(e *env) bool {
			e.begin(e.T1, e.T2)
			e.T1.Do("update test set value = 101 where id = 1")
			r1 := e.T2.Do("select * from test")
			e.T1.Do("update test set value = 11 where id = 1")
			e.T1.Do("commit")
			r2 := e.T2.Do("select * from test")
			e.T2.Do("commit")
			e.L.WaitAll()
			return strings.Contains(r1.Text(), "101") || strings.Contains(r2.Text(), "101")
		},
	},
	{
		name: "G1c", desc: "Circular Information Flow: each transaction reads the other's uncommitted write",
		run: func(e *env) bool {
			e.begin(e.T1, e.T2)
			e.T1.Do("update test set value = 11 where id = 1")
			e.T2.Do("update test set value = 22 where id = 2")
			a := e.T1.Do("select * from test where id = 2")
			b := e.T2.Do("select * from test where id = 1")
			e.T1.Do("commit")
			e.T2.Do("commit")
			e.L.WaitAll()
			return a.Text() == "(2,22)" && b.Text() == "(1,11)"
		},
	},
	{
		name: "OTV", desc: "Observed Transaction Vanishes: after seeing T2's write, later reads see values T2 overwrote",
		run: func(e *env) bool {
			e.begin(e.T1, e.T2, e.T3)
			e.T1.Do("update test set value = 11 where id = 1")
			e.T1.Do("update test set value = 19 where id = 2")
			e.T2.Do("update test set value = 12 where id = 1")
			e.T1.Do("commit")
			// T3 reads at the three points the documents read at: after T1 commits, after
			// T2's (still uncommitted) second write, and after T2 commits. postgres.md reads
			// one row at a time, mysql.md the whole table (its "read committed" block; the
			// "read uncommitted" block stops before the last read).
			var reads []*lab.Op
			if e.pg() {
				reads = append(reads, e.T3.Do("select * from test where id = 1"))
				e.T2.Do("update test set value = 18 where id = 2")
				reads = append(reads, e.T3.Do("select * from test where id = 2"))
				e.T2.Do("commit")
				reads = append(reads, e.T3.Do("select * from test where id = 2"))
				reads = append(reads, e.T3.Do("select * from test where id = 1"))
			} else {
				reads = append(reads, e.T3.Do("select * from test"))
				e.T2.Do("update test set value = 18 where id = 2")
				reads = append(reads, e.T3.Do("select * from test"))
				e.T2.Do("commit")
				reads = append(reads, e.T3.Do("select * from test"))
			}
			e.T3.Do("commit")
			e.L.WaitAll()
			sawT2 := false
			for _, r := range reads {
				t := r.Text()
				t1 := strings.Contains(t, "(1,11)") || strings.Contains(t, "(2,19)")
				t2 := strings.Contains(t, "(1,12)") || strings.Contains(t, "(2,18)")
				// one read mixing both transactions' writes, or T1's values
				// reappearing after T2's were seen = anomaly
				if t1 && (t2 || sawT2) {
					return true
				}
				sawT2 = sawT2 || t2
			}
			return false
		},
	},
	{
		name: "PMP", desc: "Predicate-Many-Preceders (phantom read): a later predicate read (value % 3 = 0) sees a row a concurrent transaction inserted after an earlier one (value = 30) found none",
		run: func(e *env) bool {
			e.begin(e.T1, e.T2)
			r1 := e.T1.Do("select * from test where value = 30")
			e.T2.Do("insert into test (id, value) values(3, 30)")
			e.T2.Do("commit")
			r2 := e.T1.Do("select * from test where value % 3 = 0")
			e.T1.Do("commit")
			e.L.WaitAll()
			return len(r1.Rows) == 0 && strings.Contains(r2.Text(), "(3,30)")
		},
	},
	{
		name: "PMP-write", desc: "Predicate-Many-Preceders (write): DELETE's predicate mixes old and new row versions",
		// PostgreSQL runs postgres.md's script (the same at both of its levels): T2 does not
		// read before its DELETE. MySQL runs mysql.md's repeatable-read block on every level:
		// T2's SELECT comes after T1's UPDATE. mysql.md's read-committed block reads the whole
		// table instead, and its serializable block puts the SELECT before the UPDATE, which
		// the document (5.6) says deadlocks; on 9.7 T1 just waits for T2's commit. Those two
		// variants are replayed in TestHermitageDocs (see docNow).
		run: func(e *env) bool {
			e.begin(e.T1, e.T2)
			e.T1.Do("update test set value = value + 10")
			if e.pg() {
				e.T2.Do("delete from test where value = 20")
				e.T1.Do("commit")
				e.T2.Do("select * from test where value = 20")
				e.T2.Do("commit")
				e.L.WaitAll()
				if !committed(e.T2, e.L) {
					return false
				}
				// Only two serializable outcomes exist:
				//   T1→T2: T2 deletes id=1 (now 20) → (2,30) remains
				//   T2→T1: T2 deletes id=2 (still 20) → (1,20) remains
				f := e.final()
				return f != "(2,30)" && f != "(1,20)"
			}
			r := e.T2.Do("select * from test where value = 20")
			e.T2.Do("delete from test where value = 20")
			e.T1.Do("commit")
			e.T2.Do("select * from test")
			e.T2.Do("commit")
			e.L.WaitAll()
			if !committed(e.T2, e.L) {
				return false
			}
			// The same two outcomes, and T2's read must agree with what it deleted:
			//   T1→T2: T2 sees (1,20) and deletes id=1 → (2,30) remains
			//   T2→T1: T2 sees (2,20) and deletes id=2 → (1,20) remains
			saw, f := r.Text(), e.final()
			ok := (saw == "(1,20)" && f == "(2,30)") || (saw == "(2,20)" && f == "(1,20)")
			return !ok
		},
	},
	{
		name: "P4", desc: "Lost Update: both read the same value and write back; one update is lost",
		run: func(e *env) bool {
			e.begin(e.T1, e.T2)
			r1 := e.T1.Do("select * from test where id = 1")
			r2 := e.T2.Do("select * from test where id = 1")
			e.T1.Do("update test set value = 11 where id = 1")
			e.T2.Do("update test set value = 11 where id = 1")
			e.T1.Do("commit")
			e.T2.Do("commit")
			e.L.WaitAll()
			// both wrote 11 based on reading 10 and both committed → one increment lost
			return r1.Text() == "(1,10)" && r2.Text() == "(1,10)" &&
				committed(e.T1, e.L) && committed(e.T2, e.L)
		},
	},
	{
		name: "G-single", desc: "Single Anti-dependency Cycles (read skew): x from before T2 commits, y from after",
		run: func(e *env) bool {
			e.begin(e.T1, e.T2)
			r1 := e.T1.Do("select * from test where id = 1")
			e.T2.Do("select * from test where id = 1")
			e.T2.Do("select * from test where id = 2")
			e.T2.Do("update test set value = 12 where id = 1")
			e.T2.Do("update test set value = 18 where id = 2")
			e.T2.Do("commit")
			r2 := e.T1.Do("select * from test where id = 2")
			e.T1.Do("commit")
			e.L.WaitAll()
			// 10 + 18 = 28 ≠ 30: a state that never existed at any point in time
			return r1.Text() == "(1,10)" && r2.Text() == "(2,18)"
		},
	},
	{
		name: "G-single-pred", desc: "Read Skew (predicate): a second predicate read sees a row a concurrent transaction changed",
		run: func(e *env) bool {
			e.begin(e.T1, e.T2)
			r1 := e.T1.Do("select * from test where value % 5 = 0")
			e.T2.Do("update test set value = 12 where value = 10")
			e.T2.Do("commit")
			r2 := e.T1.Do("select * from test where value % 3 = 0")
			e.T1.Do("commit")
			e.L.WaitAll()
			// T1 saw id=1 as 10 (before T2), then as 12 (after T2)
			return strings.Contains(r1.Text(), "(1,10)") && strings.Contains(r2.Text(), "(1,12)")
		},
	},
	{
		name: "G-single-write", desc: "Read Skew (write): x is read from an old snapshot, but DELETE's predicate acts on the new y",
		// mysql.md's repeatable-read block on every MySQL level (its serializable block moves
		// T1's DELETE before T2's second UPDATE: TestHermitageDocs). postgres.md's block is
		// the same without T1's last read; where the document ends with "abort", the DELETE
		// has failed and the harness rolls back in place of the commit.
		run: func(e *env) bool {
			e.begin(e.T1, e.T2)
			r1 := e.T1.Do("select * from test where id = 1")
			e.T2.Do("select * from test")
			e.T2.Do("update test set value = 12 where id = 1")
			e.T2.Do("update test set value = 18 where id = 2")
			e.T2.Do("commit")
			d := e.T1.Do("delete from test where value = 20")
			if !e.pg() {
				e.T1.Do("select * from test where id = 2")
			}
			e.T1.Do("commit")
			e.L.WaitAll()
			// T1 saw x=10 (before T2) yet DELETE's predicate saw y=18 (after T2) and deleted
			// nothing → dependency cycle. Deleting id=2 instead is the serial order T1→T2.
			return r1.Text() == "(1,10)" && d.OK() && d.Affected == 0 &&
				committed(e.T1, e.L) && committed(e.T2, e.L)
		},
	},
	{
		name: "G2-item", desc: "Item Anti-dependency Cycles (write skew): both read two rows, each updates a different one",
		run: func(e *env) bool {
			e.begin(e.T1, e.T2)
			r1 := e.T1.Do("select * from test where id in (1,2)")
			r2 := e.T2.Do("select * from test where id in (1,2)")
			e.T1.Do("update test set value = 11 where id = 1")
			e.T2.Do("update test set value = 21 where id = 2")
			e.T1.Do("commit")
			e.T2.Do("commit")
			e.L.WaitAll()
			// both decided on the original rows, neither saw the other's write
			return sameRows(r1.Text(), "(1,10) (2,20)") && sameRows(r2.Text(), "(1,10) (2,20)") &&
				committed(e.T1, e.L) && committed(e.T2, e.L)
		},
	},
	{
		name: "G2", desc: "Anti-dependency Cycles (write skew on a predicate read): both read a predicate, each inserts a row matching the other's",
		run: func(e *env) bool {
			e.begin(e.T1, e.T2)
			r1 := e.T1.Do("select * from test where value % 3 = 0")
			r2 := e.T2.Do("select * from test where value % 3 = 0")
			e.T1.Do("insert into test (id, value) values(3, 30)")
			e.T2.Do("insert into test (id, value) values(4, 42)")
			e.T1.Do("commit")
			e.T2.Do("commit")
			e.T1.Do("select * from test where value % 3 = 0") // the documents' "Either"
			e.L.WaitAll()
			// both saw an empty predicate, neither saw the other's insert
			return len(r1.Rows) == 0 && len(r2.Rows) == 0 && r1.OK() && r2.OK() &&
				committed(e.T1, e.L) && committed(e.T2, e.L)
		},
	},
	{
		name: "RO-anomaly",
		desc: "Read-only Transaction Anomaly (Hermitage's G2 with two anti-dependency edges, Fekete et al.): a read-only transaction sees an impossible state",
		run: func(e *env) bool {
			// postgres.md's script on both DBs: transactions start where it starts them, T2
			// after T1's read, T3 after T2 commits. mysql.md has only a serializable block,
			// with the same statements but T2's COMMIT (blocked there) after T1's:
			// TestHermitageDocs. Where the document ends with "abort", T1's UPDATE has
			// failed and the harness rolls back in place of the commit.
			e.begin(e.T1)
			r1 := e.T1.Do("select * from test") // 10, 20
			e.begin(e.T2)
			e.T2.Do("update test set value = value + 5 where id = 2")
			e.T2.Do("commit")
			e.begin(e.T3)
			r3 := e.T3.Do("select * from test") // read-only: 10, 25
			e.T3.Do("commit")
			e.T1.Do("update test set value = 0 where id = 1")
			e.T1.Do("commit")
			e.L.WaitAll()
			// T1 read y before T2's write (T1→T2), T3 saw T2's write (T2→T3) but not
			// T1's (T3→T1) → no serial order explains all three
			return committed(e.T1, e.L) && committed(e.T2, e.L) && committed(e.T3, e.L) &&
				sameRows(r1.Text(), "(1,10) (2,20)") && sameRows(r3.Text(), "(1,10) (2,25)")
		},
	},
}

// ✗ = anomaly happens, ✓ = prevented
const (
	A = "✗" // anomaly
	P = "✓" // prevented
)

// Expected cells: the anomaly happens (A), or it is prevented by one of these mechanisms.
// The mechanism is part of the result: it is what each isolation level is built on.
const (
	mv = "✓ mvcc"     // snapshot read: the other transaction's writes are simply invisible
	lk = "✓ lock"     // a lock wait forced the transactions into a serial order
	ab = "✓ abort"    // the DB detected the conflict and failed one transaction (40001)
	dl = "✓ deadlock" // locking turned the would-be cycle into a deadlock (40P01 / 1213)
)

// expected[db][level], in scenarios order.
var expected = map[lab.Kind]map[lab.Level][]string{
	lab.Postgres: {
		// SER G1c is "abort", not "mvcc" as under RR: neither transaction sees the other's
		// uncommitted write (snapshot), but each also reads the row the other wrote, an
		// rw/rw cycle (G2-item) that SSI rejects with 40001.
		//
		//                      G0  G1a G1b G1c OTV PMP PMPw P4 Gs  Gsp Gsw G2i G2 RO
		lab.ReadUncommitted: {lk, mv, mv, mv, lk, A, A, A, A, A, A, A, A, A},
		lab.ReadCommitted:   {lk, mv, mv, mv, lk, A, A, A, A, A, A, A, A, A},
		lab.RepeatableRead:  {ab, mv, mv, mv, ab, mv, ab, ab, mv, mv, ab, A, A, A},
		lab.Serializable:    {ab, mv, mv, ab, ab, mv, ab, ab, mv, mv, ab, ab, ab, ab},
	},
	lab.MySQL: {
		// RU passes PMP-write by luck: T2's SELECT dirty-reads T1's uncommitted (1,20)
		// and deletes id=1, which happens to equal serial T1→T2. RU is not safe (see G1a).
		lab.ReadUncommitted: {lk, A, A, A, A, A, lk, A, A, A, A, A, A, A},
		lab.ReadCommitted:   {lk, mv, mv, mv, lk, A, A, A, A, A, A, A, A, A},
		lab.RepeatableRead:  {lk, mv, mv, mv, lk, mv, A, A, mv, mv, A, A, A, A},
		lab.Serializable:    {lk, lk, lk, dl, lk, lk, lk, dl, lk, lk, dl, dl, dl, dl},
	},
}

// mechanism labels a prevented cell by what was observed: the first serialization failure or
// deadlock anywhere in the history, otherwise "lock" if any statement waited, otherwise "mvcc".
// It is a label of the run, not a proof that this is what decided the anomaly: in OTV the
// "lock" is T2 waiting on T1's row (G0 protection), while T3's reads see only committed data.
func mechanism(L *lab.Lab) string {
	blocked := false
	for _, op := range L.History() {
		if op.Err != nil && !op.Skipped && lab.IsTxConflict(op.Err) {
			if lab.IsDeadlock(op.Err) {
				return "deadlock"
			}
			return "abort"
		}
		blocked = blocked || op.Blocked
	}
	if blocked {
		return "lock"
	}
	return "mvcc"
}

func TestHermitage(t *testing.T) {
	type cell struct{ got, how string }
	results := map[lab.Kind]map[lab.Level][]cell{}

	for _, kind := range []lab.Kind{lab.Postgres, lab.MySQL} {
		results[kind] = map[lab.Level][]cell{}
		t.Run(kind.String(), func(t *testing.T) {
			for _, level := range lab.AllLevels {
				results[kind][level] = make([]cell, len(scenarios))
				t.Run(level.Short(), func(t *testing.T) {
					for i, sc := range scenarios {
						t.Run(sc.name, func(t *testing.T) {
							L := lab.New(t, kind, dbname)
							// The documents' setup block; mysql.md's has "engine=innodb".
							create := "create table test (id int primary key, value int)"
							if kind == lab.MySQL {
								create += " engine=innodb"
							}
							L.Exec("DROP TABLE IF EXISTS test", create,
								"insert into test (id, value) values (1, 10), (2, 20)")
							L.Note("%s @ %s: %s", sc.name, level, sc.desc)
							e := &env{L: L, level: level, T1: L.Session("T1"), T2: L.Session("T2"), T3: L.Session("T3")}
							anomaly := sc.run(e)
							c := cell{got: P, how: mechanism(L)}
							if anomaly {
								c = cell{got: A}
							}
							results[kind][level][i] = c
							L.Note("result: %s %s   final rows: %s", c.got, c.how, e.final())
							got := strings.TrimSpace(c.got + " " + c.how)
							if want := expected[kind][level][i]; want != got {
								t.Errorf("%s %s %s: want %s, got %s", kind, level.Short(), sc.name, want, got)
							}
						})
					}
				})
			}
		})
	}

	// print the summary matrix
	var b strings.Builder
	names := make([]string, len(scenarios))
	for i, sc := range scenarios {
		names[i] = sc.name
	}
	fmt.Fprintf(&b, "\n| DB | Level | %s |\n|---|---|%s\n", strings.Join(names, " | "), strings.Repeat("---|", len(names)))
	for _, kind := range []lab.Kind{lab.Postgres, lab.MySQL} {
		for _, level := range lab.AllLevels {
			cells := results[kind][level]
			ran := false
			strs := make([]string, len(cells))
			for i, c := range cells {
				strs[i] = strings.TrimSpace(c.got + " " + c.how)
				if strs[i] == "" {
					strs[i] = "–" // filtered out by -run, skipped, or failed before a result
				} else {
					ran = true
				}
			}
			if !ran {
				continue
			}
			fmt.Fprintf(&b, "| %s | %s | %s |\n", kind, level.Short(), strings.Join(strs, " | "))
		}
	}
	t.Log(b.String())
}
