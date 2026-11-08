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
type scenario struct {
	name string
	desc string
	// initial rows; defaults to (1,10),(2,20)
	rows string
	run  func(e *env) bool
}

func (e *env) begin() {
	e.T1.Begin(e.level)
	e.T2.Begin(e.level)
	e.T3.Begin(e.level)
}

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
			e.begin()
			e.T1.Do("update test set value = 11 where id = 1")
			e.T2.Do("update test set value = 12 where id = 1")
			e.T1.Do("update test set value = 21 where id = 2")
			e.T1.Do("commit")
			e.T2.Do("update test set value = 22 where id = 2")
			e.T2.Do("commit")
			e.L.WaitAll()
			f := e.final()
			// a mix of both transactions' writes = anomaly
			return f == "(1,12) (2,21)" || f == "(1,11) (2,22)"
		},
	},
	{
		name: "G1a", desc: "Aborted Reads (dirty read): reading data that is later rolled back",
		run: func(e *env) bool {
			e.begin()
			e.T1.Do("update test set value = 101 where id = 1")
			r1 := e.T2.Do("select * from test order by id")
			e.T1.Do("rollback")
			r2 := e.T2.Do("select * from test order by id")
			e.T2.Do("commit")
			e.L.WaitAll()
			return strings.Contains(r1.Text(), "101") || strings.Contains(r2.Text(), "101")
		},
	},
	{
		name: "G1b", desc: "Intermediate Reads: reading another transaction's intermediate state",
		run: func(e *env) bool {
			e.begin()
			e.T1.Do("update test set value = 101 where id = 1")
			r1 := e.T2.Do("select * from test order by id")
			e.T1.Do("update test set value = 11 where id = 1")
			e.T1.Do("commit")
			r2 := e.T2.Do("select * from test order by id")
			e.T2.Do("commit")
			e.L.WaitAll()
			return strings.Contains(r1.Text(), "101") || strings.Contains(r2.Text(), "101")
		},
	},
	{
		name: "G1c", desc: "Circular Information Flow: each transaction reads the other's uncommitted write",
		run: func(e *env) bool {
			e.begin()
			e.T1.Do("update test set value = 11 where id = 1")
			e.T2.Do("update test set value = 22 where id = 2")
			a := e.T1.Do("select value from test where id = 2")
			b := e.T2.Do("select value from test where id = 1")
			e.T1.Do("commit")
			e.T2.Do("commit")
			e.L.WaitAll()
			return a.Str() == "22" && b.Str() == "11"
		},
	},
	{
		name: "OTV", desc: "Observed Transaction Vanishes: after seeing T2's write, later reads see values T2 overwrote",
		run: func(e *env) bool {
			e.begin()
			e.T1.Do("update test set value = 11 where id = 1")
			e.T1.Do("update test set value = 19 where id = 2")
			e.T2.Do("update test set value = 12 where id = 1")
			e.T1.Do("commit")
			r1 := e.T3.Do("select value from test where id = 1")
			r2 := e.T3.Do("select value from test where id = 2")
			e.T2.Do("update test set value = 18 where id = 2")
			e.T2.Do("commit")
			r3 := e.T3.Do("select value from test where id = 2")
			r4 := e.T3.Do("select value from test where id = 1")
			e.T3.Do("commit")
			e.L.WaitAll()
			sawT2 := false
			for _, r := range []*lab.Op{r1, r2, r3, r4} {
				switch r.Str() {
				case "12", "18":
					sawT2 = true
				case "11", "19":
					if sawT2 {
						return true
					}
				}
			}
			return false
		},
	},
	{
		name: "PMP", desc: "Predicate-Many-Preceders (phantom read): the same predicate read twice returns a newly inserted row",
		run: func(e *env) bool {
			e.begin()
			r1 := e.T1.Do("select * from test where value = 30")
			e.T2.Do("insert into test (id, value) values (3, 30)")
			e.T2.Do("commit")
			r2 := e.T1.Do("select * from test where value % 3 = 0")
			e.T1.Do("commit")
			e.L.WaitAll()
			return len(r1.Rows) == 0 && strings.Contains(r2.Text(), "(3,30)")
		},
	},
	{
		name: "PMP-write", desc: "Predicate-Many-Preceders (write): DELETE's predicate mixes old and new row versions",
		run: func(e *env) bool {
			e.begin()
			e.T1.Do("update test set value = value + 10")
			r := e.T2.Do("select * from test where value = 20")
			e.T2.Do("delete from test where value = 20")
			e.T1.Do("commit")
			e.T2.Do("select * from test")
			e.T2.Do("commit")
			e.L.WaitAll()
			if !committed(e.T2, e.L) {
				return false
			}
			// Only two serializable outcomes exist:
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
			e.begin()
			e.T1.Do("select * from test where id = 1")
			e.T2.Do("select * from test where id = 1")
			e.T1.Do("update test set value = 11 where id = 1")
			e.T2.Do("update test set value = 11 where id = 1")
			e.T1.Do("commit")
			e.T2.Do("commit")
			e.L.WaitAll()
			// both wrote 11 based on reading 10 and both committed → one increment lost
			return committed(e.T1, e.L) && committed(e.T2, e.L)
		},
	},
	{
		name: "G-single", desc: "Single Anti-dependency Cycles (read skew): x from before T2 commits, y from after",
		run: func(e *env) bool {
			e.begin()
			r1 := e.T1.Do("select value from test where id = 1")
			e.T2.Do("select * from test where id = 1")
			e.T2.Do("select * from test where id = 2")
			e.T2.Do("update test set value = 12 where id = 1")
			e.T2.Do("update test set value = 18 where id = 2")
			e.T2.Do("commit")
			r2 := e.T1.Do("select value from test where id = 2")
			e.T1.Do("commit")
			e.L.WaitAll()
			// 10 + 18 = 28 ≠ 30: a state that never existed at any point in time
			return r1.Str() == "10" && r2.Str() == "18"
		},
	},
	{
		name: "G-single-write", desc: "Read Skew (write): x is read from an old snapshot, but DELETE's predicate acts on the new y",
		run: func(e *env) bool {
			e.begin()
			r1 := e.T1.Do("select value from test where id = 1")
			e.T2.Do("select * from test")
			e.T2.Do("update test set value = 12 where id = 1")
			e.T2.Do("update test set value = 18 where id = 2")
			e.T2.Do("commit")
			e.T1.Do("delete from test where value = 20")
			e.T1.Do("select * from test")
			e.T1.Do("commit")
			e.L.WaitAll()
			// T1 saw x=10 (before T2) yet acted on y after T2 → dependency cycle
			return r1.Str() == "10" && committed(e.T1, e.L) && committed(e.T2, e.L)
		},
	},
	{
		name: "G2-item", desc: "Item Anti-dependency Cycles (write skew): both read two rows, each updates a different one",
		run: func(e *env) bool {
			e.begin()
			e.T1.Do("select * from test where id in (1,2)")
			e.T2.Do("select * from test where id in (1,2)")
			e.T1.Do("update test set value = 11 where id = 1")
			e.T2.Do("update test set value = 21 where id = 2")
			e.T1.Do("commit")
			e.T2.Do("commit")
			e.L.WaitAll()
			return committed(e.T1, e.L) && committed(e.T2, e.L)
		},
	},
	{
		name: "G2", desc: "Predicate Anti-dependency Cycles: both read a predicate, each inserts a row matching the other's",
		run: func(e *env) bool {
			e.begin()
			e.T1.Do("select * from test where value % 3 = 0")
			e.T2.Do("select * from test where value % 3 = 0")
			e.T1.Do("insert into test (id, value) values (3, 30)")
			e.T2.Do("insert into test (id, value) values (4, 42)")
			e.T1.Do("commit")
			e.T2.Do("commit")
			e.L.WaitAll()
			return committed(e.T1, e.L) && committed(e.T2, e.L)
		},
	},
	{
		name: "RO-anomaly", rows: "(1,0),(2,0)",
		desc: "Read-only Transaction Anomaly (Fekete 2004): a read-only transaction sees an impossible state",
		run: func(e *env) bool {
			// x = checking (id1), y = savings (id2). T2 withdraws 10 from x and charges
			// a penalty of 1 if x+y-10 < 0. T1 deposits 20 into y. T3 is a read-only report.
			e.begin()
			e.T2.Do("select * from test where id in (1,2)") // sees 0,0 → decides to charge the penalty
			e.T1.Do("update test set value = value + 20 where id = 2")
			e.T1.Do("commit")
			r3 := e.T3.Do("select * from test order by id") // report: x=0, y=20
			e.T3.Do("commit")
			e.T2.Do("update test set value = -11 where id = 1") // withdraw 10 + penalty 1
			e.T2.Do("commit")
			e.L.WaitAll()
			// the report saw the deposit, yet T2 still charged the penalty → no serial order explains both
			return committed(e.T1, e.L) && committed(e.T2, e.L) && committed(e.T3, e.L) &&
				r3.Text() == "(1,0) (2,20)"
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
		//                      G0  G1a G1b G1c OTV PMP PMPw P4 Gs  Gsw G2i G2 RO
		lab.ReadUncommitted: {lk, mv, mv, mv, lk, A, A, A, A, A, A, A, A},
		lab.ReadCommitted:   {lk, mv, mv, mv, lk, A, A, A, A, A, A, A, A},
		lab.RepeatableRead:  {ab, mv, mv, mv, ab, mv, ab, ab, mv, ab, A, A, A},
		lab.Serializable:    {ab, mv, mv, ab, ab, mv, ab, ab, mv, ab, ab, ab, ab},
	},
	lab.MySQL: {
		// RU passes PMP-write by luck: T2's SELECT dirty-reads T1's uncommitted (1,20)
		// and deletes id=1, which happens to equal serial T1→T2. RU is not safe (see G1a).
		lab.ReadUncommitted: {lk, A, A, A, A, A, lk, A, A, A, A, A, A},
		lab.ReadCommitted:   {lk, mv, mv, mv, lk, A, A, A, A, A, A, A, A},
		lab.RepeatableRead:  {lk, mv, mv, mv, lk, mv, A, A, mv, A, A, A, A},
		lab.Serializable:    {lk, lk, lk, dl, lk, lk, lk, dl, lk, dl, dl, dl, dl},
	},
}

// mechanism tells how an anomaly was prevented.
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
				t.Run(level.Short(), func(t *testing.T) {
					for i, sc := range scenarios {
						t.Run(sc.name, func(t *testing.T) {
							L := lab.New(t, kind, dbname)
							rows := sc.rows
							if rows == "" {
								rows = "(1,10),(2,20)"
							}
							L.Exec("DROP TABLE IF EXISTS test",
								"CREATE TABLE test (id int primary key, value int)",
								"INSERT INTO test (id, value) VALUES "+rows)
							L.Note("%s @ %s: %s", sc.name, level, sc.desc)
							e := &env{L: L, level: level, T1: L.Session("T1"), T2: L.Session("T2"), T3: L.Session("T3")}
							anomaly := sc.run(e)
							c := cell{got: P, how: mechanism(L)}
							if anomaly {
								c = cell{got: A}
							}
							results[kind][level] = append(results[kind][level], c)
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
			if len(cells) == 0 {
				continue
			}
			strs := make([]string, len(cells))
			for i, c := range cells {
				strs[i] = strings.TrimSpace(c.got + " " + c.how)
			}
			fmt.Fprintf(&b, "| %s | %s | %s |\n", kind, level.Short(), strings.Join(strs, " | "))
		}
	}
	t.Log(b.String())
}
