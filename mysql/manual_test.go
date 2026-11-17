package mysql

// Claims from the MySQL 8.4 manual, section 17.7 "InnoDB Locking and Transaction Model",
// that the other files in this package do not already cover. Each test names the
// subsection it checks:
// https://dev.mysql.com/doc/refman/8.4/en/innodb-locking-transaction-model.html
//
// README §7 maps every subsection to the tests that cover it.

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jyny/db-iso-lv-go/internal/lab"
)

// recordLocks lists a session's record locks as "MODE data", sorted.
func recordLocks(locks []lab.Lock, session string) []string {
	var out []string
	for _, k := range locks {
		if k.Session == session && k.Type == "RECORD" {
			out = append(out, k.Mode+" "+k.Data)
		}
	}
	slices.Sort(out)
	return out
}

// innodbDeadlocks reads the number of deadlocks InnoDB has detected since the server started.
func innodbDeadlocks(L *lab.Lab) int {
	L.T.Helper()
	var n int
	fmt.Sscan(L.QueryString(`SELECT count FROM information_schema.INNODB_METRICS WHERE NAME = "lock_deadlocks"`), &n)
	return n
}

func setGlobal(t *testing.T, L *lab.Lab, name, value string) {
	t.Helper()
	old := L.QueryString("SELECT @@GLOBAL." + name)
	L.Exec("SET GLOBAL " + name + " = " + value)
	t.Cleanup(func() { _, _ = L.Admin.Exec("SET GLOBAL " + name + " = " + old) })
}

// 17.7.1 Intention Locks: the table-level compatibility matrix. IS and IX come from
// FOR SHARE / FOR UPDATE on a row (a different row for each session, so only the table
// level can conflict); table-level S and X come from LOCK TABLES ... READ / WRITE with
// autocommit=0, which is when InnoDB also takes its own table lock (checked first, in
// data_locks).
//
// The measured conflicts match the manual's matrix, but every one of them is decided by the
// server's metadata locks before InnoDB is asked: FOR SHARE / FOR UPDATE take MDL
// SHARED_READ / SHARED_WRITE, LOCK TABLES READ / WRITE take SHARED_READ_ONLY /
// SHARED_NO_READ_WRITE, and those conflict exactly where IS / IX / S / X do. The test
// asserts that each blocked request waits on a metadata lock and not in data_lock_waits.
// InnoDB's own table-lock matrix is not reachable from SQL.
func TestLock_IntentionLockMatrix(t *testing.T) {
	L := newLab(t)
	setupT(L)
	modes := []string{"X", "IX", "S", "IS"}
	acquire := func(s *lab.Session, mode string, id int) *lab.Op {
		switch mode {
		case "IS":
			s.Do("START TRANSACTION")
			return s.Do(fmt.Sprintf("SELECT * FROM t WHERE id = %d FOR SHARE", id))
		case "IX":
			s.Do("START TRANSACTION")
			return s.Do(fmt.Sprintf("SELECT * FROM t WHERE id = %d FOR UPDATE", id))
		case "S":
			s.Do("SET autocommit = 0")
			return s.Do("LOCK TABLES t READ")
		default:
			s.Do("SET autocommit = 0")
			return s.Do("LOCK TABLES t WRITE")
		}
	}
	release := func(s *lab.Session) {
		s.Do("ROLLBACK")
		s.Do("UNLOCK TABLES")
		s.Do("SET autocommit = 1")
	}
	// The manual's table: rows = held, columns = requested.
	want := map[string]string{"X": "CCCC", "IX": "C.C.", "S": "CC..", "IS": "C..."}

	t1, t2 := L.Session("T1"), L.Session("T2")
	for _, m := range modes {
		acquire(t1, m, 10)
		var table []string
		for _, k := range L.Locks() {
			if k.Session == "T1" && k.Type == "TABLE" {
				table = append(table, k.Mode)
			}
		}
		L.Note("%s held → InnoDB table lock in data_locks: %v", m, table)
		if !slices.Contains(table, m) {
			t.Errorf("holding %s: data_locks shows table locks %v", m, table)
		}
		release(t1)
	}

	L.Quiet()
	var b strings.Builder
	b.WriteString("\nheld \\ requested   X   IX  S   IS\n")
	for _, held := range modes {
		fmt.Fprintf(&b, "%-18s", held)
		for j, req := range modes {
			acquire(t1, held, 10)
			op := acquire(t2, req, 20)
			blocked := op.Blocked
			if blocked {
				mdl := L.QueryString(`SELECT count(*) FROM performance_schema.metadata_locks ml
					JOIN performance_schema.threads th ON th.THREAD_ID = ml.OWNER_THREAD_ID
					WHERE th.PROCESSLIST_ID = ? AND ml.LOCK_STATUS = 'PENDING'`, t2.ID)
				innodb := L.QueryString("SELECT count(*) FROM performance_schema.data_lock_waits")
				if mdl == "0" || innodb != "0" {
					t.Errorf("held %s, requested %s: %s pending metadata locks, %s InnoDB lock waits; want a metadata-lock wait only", held, req, mdl, innodb)
				}
			}
			release(t1)
			op.Wait()
			release(t2)
			cell := "."
			if blocked {
				cell = "C"
			}
			fmt.Fprintf(&b, " %-3s", cell)
			if wantC := want[held][j] == 'C'; blocked != wantC {
				t.Errorf("held %s, requested %s: blocked=%v, want %v", held, req, blocked, wantC)
			}
		}
		b.WriteString("\n")
	}
	t.Log(b.String())
}

// 17.7.1 Record Locks: "Record locks always lock index records, even if a table is defined
// with no indexes": InnoDB locks its hidden clustered index, GEN_CLUST_INDEX.
func TestLock_RecordLocksOnHiddenClusteredIndex(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS nopk", "CREATE TABLE nopk (c int)", "INSERT INTO nopk VALUES (1), (2), (3)")
	t1 := L.Session("T1")
	t1.Do("START TRANSACTION")
	t1.Do("SELECT * FROM nopk WHERE c = 2 FOR UPDATE")
	n := 0
	for _, k := range L.ShowLocks() {
		if k.Type == "RECORD" {
			n++
			if k.Object != "nopk.GEN_CLUST_INDEX" {
				t.Fatalf("record lock on %s, want nopk.GEN_CLUST_INDEX", k.Object)
			}
		}
	}
	if n == 0 {
		t.Fatal("no record locks")
	}
	t1.Do("ROLLBACK")
}

// 17.7.1 Gap Locks: gap locks are purely inhibitive. Shared and exclusive gap locks on the
// same gap coexist; together they still block an insert into it.
func TestLock_GapLocksSharedAndExclusiveCoexist(t *testing.T) {
	L := newLab(t)
	setupT(L)
	t1, t2, t3 := L.Session("T1"), L.Session("T2"), L.Session("T3")
	t1.Do("START TRANSACTION")
	t1.Do("SELECT * FROM t WHERE id = 15 FOR SHARE").MustRows("(no rows)")
	t2.Do("START TRANSACTION")
	t2.Do("SELECT * FROM t WHERE id = 16 FOR UPDATE").MustNotBlock().MustRows("(no rows)")
	locks := L.ShowLocks()
	mustHaveLock(t, locks, "T1", "S,GAP", "20")
	mustHaveLock(t, locks, "T2", "X,GAP", "20")
	ins := t3.Do("INSERT INTO t VALUES (12, 12, 0)").MustBlock()
	t1.Do("ROLLBACK")
	L.Settle()
	ins.MustBlock()
	t2.Do("ROLLBACK")
	ins.MustOK()
}

// 17.7.1 Gap Locks: a unique-index search for one row needs no gap lock, unless it uses only
// a prefix of a multi-column unique index.
func TestLock_MultiColumnUniquePrefixTakesGapLocks(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS mu", "CREATE TABLE mu (id int PRIMARY KEY, a int, b int, UNIQUE KEY ab (a, b))",
		"INSERT INTO mu VALUES (1, 1, 1), (2, 1, 3), (3, 2, 1)")
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("START TRANSACTION")
	t1.Do("SELECT id FROM mu WHERE a = 1 AND b = 1 FOR UPDATE").MustRows("(1)")
	L.Note("full unique key: a record lock only, so the gap next to (1,1) stays open")
	if gaps := recordLocks(L.ShowLocks(), "T1"); slices.ContainsFunc(gaps, func(s string) bool { return !strings.Contains(s, "REC_NOT_GAP") }) {
		t.Fatalf("expected only REC_NOT_GAP locks, got %v", gaps)
	}
	t2.Do("INSERT INTO mu VALUES (10, 1, 2)").MustNotBlock().MustOK()
	t2.Do("DELETE FROM mu WHERE id = 10")
	t1.Do("ROLLBACK")

	t1.Do("START TRANSACTION")
	t1.Do("SELECT id FROM mu WHERE a = 1 FOR UPDATE").MustRows("(1) (2)")
	L.Note("prefix a = 1: next-key locks, so (1,2) cannot be inserted between (1,1) and (1,3)")
	L.ShowLocks()
	ins := t2.Do("INSERT INTO mu VALUES (10, 1, 2)").MustBlock()
	t1.Do("ROLLBACK")
	ins.MustOK()
}

// 17.7.1 Next-Key Locks: with index values 10, 11, 13, 20 the next-key intervals are
// (-inf,10] (10,11] (11,13] (13,20] (20,+inf). A locking read of id > 15 locks (13,20] and (20,+inf),
// the last one on the supremum pseudo-record.
func TestLock_NextKeyIntervalsDocsExample(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS nk", "CREATE TABLE nk (id int PRIMARY KEY)", "INSERT INTO nk VALUES (10), (11), (13), (20)")
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("START TRANSACTION")
	t1.Do("SELECT id FROM nk WHERE id > 15 FOR UPDATE").MustRows("(20)")
	if got, want := recordLocks(L.ShowLocks(), "T1"), []string{"X 20", "X supremum pseudo-record"}; !slices.Equal(got, want) {
		t.Fatalf("locks %v, want %v", got, want)
	}
	t2.Do("START TRANSACTION")
	t2.Do("INSERT INTO nk VALUES (12)").MustNotBlock().MustOK()
	a := t2.Do("INSERT INTO nk VALUES (14)").MustBlock()
	t1.Do("ROLLBACK")
	a.MustOK()
	t2.Do("ROLLBACK")

	t1.Do("START TRANSACTION")
	t1.Do("SELECT id FROM nk WHERE id > 15 FOR UPDATE")
	b := t2.Do("INSERT INTO nk VALUES (1000)").MustBlock()
	t1.Do("ROLLBACK")
	b.MustOK()
	// A fresh table: a deleted row waiting for purge would add locks of its own.
	L.Exec("DROP TABLE nk", "CREATE TABLE nk (id int PRIMARY KEY)", "INSERT INTO nk VALUES (10), (11), (13), (20)")

	L.Note("all five intervals for a full scan, and the other intervals one locking read each")
	for _, c := range []struct {
		where   string
		locks   []string
		blocked int // an insert into the locked interval; 0 = none to try
	}{
		{"id IS NOT NULL", []string{"X 10", "X 11", "X 13", "X 20", "X supremum pseudo-record"}, 15},
		{"id <= 10", []string{"X 10"}, 5},
		{"id > 10 AND id <= 11", []string{"X 11"}, 0},
		{"id > 11 AND id <= 13", []string{"X 13"}, 12},
	} {
		t1.Do("START TRANSACTION")
		t1.Do("SELECT id FROM nk WHERE " + c.where + " FOR UPDATE").MustOK()
		if got := recordLocks(L.ShowLocks(), "T1"); !slices.Equal(got, c.locks) {
			t.Errorf("%s: locks %v, want %v", c.where, got, c.locks)
		}
		if c.blocked != 0 {
			t2.Do("START TRANSACTION")
			ins := t2.Do(fmt.Sprintf("INSERT INTO nk VALUES (%d)", c.blocked)).MustBlock()
			t1.Do("ROLLBACK")
			ins.MustOK()
			t2.Do("ROLLBACK")
		} else {
			t1.Do("ROLLBACK")
		}
	}
}

// 17.7.1 Insert Intention Locks and 17.7.4 Phantom Rows: the manual's child table (90, 102).
// "id > 100 FOR UPDATE" locks the gap before 102 and the gap after it, so inserting 101 waits
// with an insert intention lock and A's repeated locking read sees no phantom. A share-mode
// read of a missing key likewise reserves it (next-key locking "enables you to “lock” the nonexistence of something in your table").
func TestLock_InsertIntentionAndPhantomDocsExample(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS child", "CREATE TABLE child (id int NOT NULL, PRIMARY KEY (id))",
		"INSERT INTO child (id) VALUES (90), (102)")
	a, b := L.Session("A"), L.Session("B")
	a.Do("START TRANSACTION")
	a.Do("SELECT * FROM child WHERE id > 100 FOR UPDATE").MustRows("(102)")
	// 17.7.4: "the locks set by InnoDB include a lock on the gap following id value 102".
	if got, want := recordLocks(L.ShowLocks(), "A"), []string{"X 102", "X supremum pseudo-record"}; !slices.Equal(got, want) {
		t.Fatalf("A's locks %v, want %v", got, want)
	}
	b.Do("START TRANSACTION")
	ins := b.Do("INSERT INTO child (id) VALUES (101)").MustBlock()
	mustHaveLock(t, L.ShowLocks(), "B", "X,GAP,INSERT_INTENTION", "102")
	after := L.Session("C").Do("INSERT INTO child (id) VALUES (103)").MustBlock()
	a.Do("SELECT * FROM child WHERE id > 100 FOR UPDATE").MustRows("(102)")
	a.Do("COMMIT")
	ins.MustOK()
	after.MustOK()
	b.Do("ROLLBACK")
	L.Exec("DELETE FROM child WHERE id = 103")

	L.Note("17.7.1: index records 4 and 7; inserts of 5 and 6 both take insert intention locks on the gap and do not block each other")
	L.Exec("DROP TABLE IF EXISTS ii", "CREATE TABLE ii (id int PRIMARY KEY)", "INSERT INTO ii VALUES (4), (7)")
	a.Do("START TRANSACTION")
	b.Do("START TRANSACTION")
	a.Do("INSERT INTO ii VALUES (5)").MustOK()
	b.Do("INSERT INTO ii VALUES (6)").MustNotBlock().MustOK()
	a.Do("ROLLBACK")
	b.Do("ROLLBACK")

	L.Note("insert intention locks on one gap do not conflict: 91 and 92 both go into (90,102)")
	a.Do("START TRANSACTION")
	b.Do("START TRANSACTION")
	a.Do("INSERT INTO child VALUES (91)").MustOK()
	b.Do("INSERT INTO child VALUES (92)").MustNotBlock().MustOK()
	a.Do("ROLLBACK")
	b.Do("ROLLBACK")

	L.Note("read a missing key FOR SHARE, then insert it: nobody can insert the same key meanwhile")
	a.Do("START TRANSACTION")
	a.Do("SELECT * FROM child WHERE id = 105 FOR SHARE").MustRows("(no rows)")
	dup := b.Do("INSERT INTO child VALUES (105)").MustBlock()
	a.Do("INSERT INTO child VALUES (105)").MustOK()
	a.Do("COMMIT")
	dup.MustFail(lab.IsUniqueViolation)
}

// 17.7.1 AUTO-INC Locks and 17.7.3 AUTO_INCREMENT: MySQL 8.0+ defaults to
// innodb_autoinc_lock_mode = 2 ("interleaved"), which takes no table-level AUTO-INC lock,
// so concurrent inserts (even INSERT ... SELECT) never wait for each other and a
// rolled-back insert leaves a gap. An AUTO-INC lock is held only to the end of the
// statement, so T1's INSERT ... SELECT is kept running (stuck on a source row T3 has
// locked) while T2 inserts: in modes 0 and 1 T2 would wait for it. Those modes need a
// server restart to observe.
func TestLock_AutoIncInterleavedMode(t *testing.T) {
	L := newLab(t)
	if m := L.QueryString("SELECT @@innodb_autoinc_lock_mode"); m != "2" {
		t.Skipf("innodb_autoinc_lock_mode = %s; this test checks mode 2", m)
	}
	setupT(L)
	L.Exec("DROP TABLE IF EXISTS ai", "CREATE TABLE ai (id int AUTO_INCREMENT PRIMARY KEY, v int)")
	t1, t2, t3 := L.Session("T1"), L.Session("T2"), L.Session("T3")
	t3.Do("START TRANSACTION")
	t3.Do("SELECT * FROM t WHERE id = 30 FOR UPDATE")
	t1.Do("START TRANSACTION")
	t1.Do("INSERT INTO ai (v) VALUES (1)")
	bulk := t1.Do("INSERT INTO ai (v) SELECT v FROM t").MustBlock()
	L.Note("T1's INSERT ... SELECT is still running (waiting for row 30)")
	for _, k := range L.ShowLocks() {
		if strings.Contains(k.Mode, "AUTO_INC") {
			t.Fatalf("unexpected AUTO-INC lock: %v", k)
		}
	}
	t2.Do("INSERT INTO ai (v) VALUES (2)").MustNotBlock().MustOK()
	t3.Do("COMMIT")
	bulk.MustAffect(3)
	t1.Do("ROLLBACK")
	L.Note("T1's ids were consumed and are not reused: T2's row does not get id 1")
	if id := t2.Do("SELECT id FROM ai").Int(); id <= 1 {
		t.Fatalf("T2's id = %d; id 1 was reserved by T1's first insert", id)
	}
}

// 17.7.1 Predicate Locks for Spatial Indexes: a locking read through a SPATIAL index locks
// the query's MBR (a predicate lock, LOCK_MODE "S,PREDICATE" in data_locks) instead of
// next-key ranges.
// An insert of a point inside the MBR waits; one far outside does not. The manual names
// REPEATABLE READ and SERIALIZABLE; READ COMMITTED takes no predicate lock
// (TestLock_SpatialNoPredicateLockInReadCommitted).
func TestLock_SpatialPredicateLock(t *testing.T) {
	for _, level := range []string{"SERIALIZABLE", "REPEATABLE READ"} {
		t.Run(strings.ReplaceAll(level, " ", "-"), func(t *testing.T) { spatialPredicateLock(t, level) })
	}
}

func spatialPredicateLock(t *testing.T, level string) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS geo",
		"CREATE TABLE geo (id int PRIMARY KEY, g POINT NOT NULL SRID 0, SPATIAL INDEX (g))",
		"INSERT INTO geo VALUES (1, ST_GeomFromText('POINT(1 1)')), (2, ST_GeomFromText('POINT(100 100)'))")
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("SET SESSION TRANSACTION ISOLATION LEVEL " + level)
	t1.Do("START TRANSACTION")
	t1.Do("SELECT id FROM geo FORCE INDEX (g) WHERE MBRContains(ST_GeomFromText('POLYGON((0 0,0 5,5 5,5 0,0 0))'), g) FOR UPDATE").MustRows("(1)")
	locks := L.ShowLocks()
	if !slices.ContainsFunc(locks, func(k lab.Lock) bool { return strings.Contains(k.Mode, "PREDICATE") }) {
		t.Fatalf("no predicate lock in %v", locks)
	}
	t2.Do("START TRANSACTION")
	t2.Do("INSERT INTO geo VALUES (4, ST_GeomFromText('POINT(1000 1000)'))").MustNotBlock().MustOK()
	ins := t2.Do("INSERT INTO geo VALUES (3, ST_GeomFromText('POINT(2 2)'))").MustBlock()
	L.Note("moving an existing row into the MBR is blocked as well")
	t3 := L.Session("T3")
	mv := t3.Do("UPDATE geo SET g = ST_GeomFromText('POINT(3 3)') WHERE id = 2").MustBlock()
	t1.Do("COMMIT")
	ins.MustOK()
	t2.Do("ROLLBACK")
	mv.MustOK()
}

// 17.7.2.1 READ COMMITTED: the manual's semi-consistent read example, statement for
// statement (its table t is called sc here, and session B runs in an explicit transaction
// so that the locks it keeps can be read). With no
// index, B's UPDATE scans rows A has locked; in READ COMMITTED it reads their latest committed
// version, sees they don't match, and skips them instead of waiting. REPEATABLE READ waits.
// With an index on b, locks are taken on the index, so B waits even under READ COMMITTED.
func TestLock_SemiConsistentReadDocsExample(t *testing.T) {
	for _, c := range []struct {
		name    string
		level   lab.Level
		indexed bool
		blocks  bool
	}{
		{"RC", lab.ReadCommitted, false, false},
		{"RR", lab.RepeatableRead, false, true},
		{"RC-indexed", lab.ReadCommitted, true, true},
		// "If the row matches (must be updated), MySQL reads the row again and this time
		// InnoDB either locks it or waits for a lock on it."
		{"RC-latest-committed-matches", lab.ReadCommitted, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			L := newLab(t)
			a, b := L.Session("A"), L.Session("B")
			var bStmt string
			if c.indexed {
				L.Exec("DROP TABLE IF EXISTS sc", "CREATE TABLE sc (a INT NOT NULL, b INT, c INT, INDEX (b)) ENGINE = InnoDB",
					"INSERT INTO sc VALUES (1,2,3),(2,2,4)")
				a.Begin(c.level)
				a.Do("UPDATE sc SET b = 3 WHERE b = 2 AND c = 3").MustAffect(1)
				bStmt = "UPDATE sc SET b = 4 WHERE b = 2 AND c = 4"
			} else {
				L.Exec("DROP TABLE IF EXISTS sc", "CREATE TABLE sc (a INT NOT NULL, b INT) ENGINE = InnoDB",
					"INSERT INTO sc VALUES (1,2),(2,3),(3,2),(4,3),(5,2)")
				a.Begin(c.level)
				a.Do("UPDATE sc SET b = 5 WHERE b = 3").MustAffect(2)
				bStmt = "UPDATE sc SET b = 4 WHERE b = 2"
				if c.name == "RC-latest-committed-matches" {
					bStmt = "UPDATE sc SET b = 4 WHERE b = 3"
				}
				// READ COMMITTED keeps x-locks only on the two rows it changed; REPEATABLE
				// READ keeps one on every row it read (5 rows + the supremum).
				want := 2
				if c.level == lab.RepeatableRead {
					want = 6
				}
				if got := recordLocks(L.ShowLocks(), "A"); len(got) != want {
					t.Fatalf("A holds %d record locks %v, want %d", len(got), got, want)
				}
				// Which rows: the manual's listing is "x-lock(1,2); unlock(1,2)", "x-lock(2,3);
				// update(2,3) to (2,5); retain x-lock", ... for READ COMMITTED, and "retain
				// x-lock" on all five rows for REPEATABLE READ. SKIP LOCKED returns the unlocked ones.
				p := L.Session("P")
				p.Do("SET SESSION TRANSACTION ISOLATION LEVEL READ COMMITTED")
				unlocked := "(1,2) (3,2) (5,2)"
				if c.level == lab.RepeatableRead {
					unlocked = "(no rows)"
				}
				p.Do("SELECT a, b FROM sc FOR UPDATE SKIP LOCKED").MustRows(unlocked)
			}
			b.Begin(c.level)
			op := b.Do(bStmt)
			switch {
			case c.name == "RC-latest-committed-matches":
				L.Note("rows 2 and 4 are locked by A, but their latest committed b = 3 matches: B waits")
				op.MustBlock()
				a.Do("COMMIT")
				L.Note("after A commits they read b = 5 and no longer match")
				op.MustAffect(0)
			case c.blocks:
				op.MustBlock()
				if !c.indexed {
					// "x-lock(1,2); block and wait for first UPDATE to commit or roll back"
					var waiting, granted int
					for _, k := range L.ShowLocks() {
						if k.Session == "B" && k.Type == "RECORD" {
							if k.Granted {
								granted++
							} else {
								waiting++
							}
						}
					}
					if waiting != 1 || granted != 0 {
						t.Errorf("B has %d waiting and %d granted record locks, want 1 and 0: it waits at the first row", waiting, granted)
					}
				}
				a.Do("COMMIT")
				op.MustOK()
			default:
				op.MustNotBlock().MustAffect(3)
				L.Note("B retains x-locks on the three rows it updated and unlocks the two it skipped")
				if got := recordLocks(L.ShowLocks(), "B"); len(got) != 3 {
					t.Errorf("B holds record locks %v, want 3", got)
				}
				L.Note("every row is now locked by A (2 rows) or B (3 rows)")
				L.Session("P").Do("SELECT a, b FROM sc FOR UPDATE SKIP LOCKED").MustRows("(no rows)")
				a.Do("COMMIT")
			}
			b.Do("COMMIT")
		})
	}
}

// 17.7.2.1 READ COMMITTED: "Gap locking is only used for foreign-key constraint checking
// and duplicate-key checking." A failed duplicate check on a unique secondary index leaves
// a next-key S lock (record + gap) even in READ COMMITTED.
func TestLock_RC_GapLockForDuplicateKeyCheck(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS du", "CREATE TABLE du (id int PRIMARY KEY, u int, UNIQUE KEY uu (u))",
		"INSERT INTO du VALUES (1, 10), (2, 20)")
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Begin(lab.ReadCommitted)
	t1.Do("INSERT INTO du VALUES (5, 20)").MustFail(lab.IsUniqueViolation)
	mustHaveLock(t, L.ShowLocks(), "T1", "S", "20, 2")
	t2.Begin(lab.ReadCommitted)
	ins := t2.Do("INSERT INTO du VALUES (6, 15)").MustBlock()
	t1.Do("ROLLBACK")
	ins.MustOK()
	t2.Do("ROLLBACK")
}

// 17.7.2.2 autocommit, Commit, and Rollback: the manual's customer example, an implicit
// commit by DDL, and the rollback of an autocommit=0 session that disconnects.
func TestTx_AutocommitCommitRollback(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS customer", "DROP TABLE IF EXISTS t_implicit",
		"CREATE TABLE customer (a INT, b CHAR (20), INDEX (a))")
	s := L.Session("S")
	s.Do("SELECT @@autocommit").MustRows("(1)")
	s.Do("START TRANSACTION")
	s.Do("INSERT INTO customer VALUES (10, 'Heikki')")
	s.Do("COMMIT")
	s.Do("SET autocommit = 0")
	s.Do("INSERT INTO customer VALUES (15, 'John')")
	s.Do("INSERT INTO customer VALUES (20, 'Paul')")
	s.Do("DELETE FROM customer WHERE b = 'Heikki'")
	s.Do("ROLLBACK")
	s.Do("SELECT * FROM customer").MustRows("(10,Heikki)")

	L.Note("DDL commits implicitly: ROLLBACK afterwards cannot undo the insert")
	s.Do("INSERT INTO customer VALUES (70, 'Tony')")
	s.Do("CREATE TABLE t_implicit (x INT)")
	s.Do("ROLLBACK")
	if n := L.QueryString("SELECT count(*) FROM customer WHERE a = 70"); n != "1" {
		t.Fatalf("row 70 count = %s, want 1 (committed by the DDL)", n)
	}

	L.Note("a session with autocommit=0 that ends without COMMIT is rolled back")
	s.Do("INSERT INTO customer VALUES (60, 'Pete')")
	L.Exec(fmt.Sprintf("KILL %d", s.ID))
	deadline := time.Now().Add(5 * time.Second)
	for L.QueryString("SELECT count(*) FROM performance_schema.processlist WHERE ID = ?", s.ID) != "0" {
		if time.Now().After(deadline) {
			t.Fatal("killed session still present")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := L.QueryString("SELECT count(*) FROM customer WHERE a = 60"); n != "0" {
		t.Fatalf("row 60 count = %s, want 0", n)
	}
}

// 17.7.2.3 Consistent Nonlocking Reads: the snapshot applies to SELECT only. DELETE and
// UPDATE are current reads and can change rows the transaction cannot see; once updated,
// those rows become visible to it. (The manual's xyz / abc example.)
func TestMVCC_DMLSeesRowsTheSnapshotCannot(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS t1", "CREATE TABLE t1 (id int AUTO_INCREMENT PRIMARY KEY, c1 varchar(10), c2 varchar(10))")
	a, b := L.Session("A"), L.Session("B")
	a.Do("START TRANSACTION WITH CONSISTENT SNAPSHOT")
	b.Do("INSERT INTO t1 (c1, c2) VALUES ('xyz', ''), ('xyz', ''), ('xyz', '')")
	b.Do("INSERT INTO t1 (c1, c2) VALUES " + strings.Repeat("('', 'abc'), ", 9) + "('', 'abc')")
	a.Do("SELECT COUNT(c1) FROM t1 WHERE c1 = 'xyz'").MustRows("(0)")
	a.Do("DELETE FROM t1 WHERE c1 = 'xyz'").MustAffect(3)
	a.Do("SELECT COUNT(c2) FROM t1 WHERE c2 = 'abc'").MustRows("(0)")
	a.Do("UPDATE t1 SET c2 = 'cba' WHERE c2 = 'abc'").MustAffect(10)
	a.Do("SELECT COUNT(c2) FROM t1 WHERE c2 = 'cba'").MustRows("(10)")
	a.Do("COMMIT")
}

// 17.7.2.3: in REPEATABLE READ "all consistent reads within the same transaction read the
// snapshot established by the first such read"; in READ COMMITTED "each consistent read
// within a transaction sets and reads its own fresh snapshot".
func TestMVCC_SnapshotPerTransactionOrPerRead(t *testing.T) {
	for _, c := range []struct {
		level  lab.Level
		second string
	}{{lab.RepeatableRead, "(0)"}, {lab.ReadCommitted, "(1)"}} {
		t.Run(c.level.Short(), func(t *testing.T) {
			L := newLab(t)
			setupT(L)
			a, b := L.Session("A"), L.Session("B")
			a.Begin(c.level)
			a.Do("SELECT v FROM t WHERE id = 10").MustRows("(0)")
			b.Do("UPDATE t SET v = 1 WHERE id = 10")
			a.Do("SELECT v FROM t WHERE id = 10").MustRows(c.second)
			a.Do("COMMIT")
		})
	}
}

// 17.7.2.3: a query also "sees the changes made by earlier statements within the same
// transaction", and that exception "causes the following anomaly: if you update some rows in
// a table, a SELECT sees the latest version of the updated rows, but it might also see older
// versions of any rows ... you might see the table in a state that never existed".
// B moves 10 from row 10 to row 20; the total is 300 before and after. A, whose snapshot
// predates B, adds 1 to row 10 and then sees its own new row 10 (B's 90, plus 1) next to
// the snapshot's old row 20: a total of 291, when 301 is the only value A's +1 can explain.
func TestMVCC_OwnUpdateMixesNewAndOldVersions(t *testing.T) {
	L := newLab(t)
	setupT(L)
	L.Exec("UPDATE t SET v = 100")
	a, b := L.Session("A"), L.Session("B")
	a.Do("START TRANSACTION")
	a.Do("SELECT sum(v) FROM t").MustRows("(300)")
	b.Do("START TRANSACTION")
	b.Do("UPDATE t SET v = v - 10 WHERE id = 10")
	b.Do("UPDATE t SET v = v + 10 WHERE id = 20")
	b.Do("COMMIT")
	a.Do("SELECT sum(v) FROM t").MustRows("(300)")
	a.Do("UPDATE t SET v = v + 1 WHERE id = 10").MustAffect(1)
	a.Do("SELECT id, v FROM t ORDER BY id").MustRows("(10,91) (20,100) (30,100)")
	a.Do("SELECT sum(v) FROM t").MustRows("(291)")
	a.Do("COMMIT")
	a.Do("SELECT sum(v) FROM t").MustRows("(301)")
}

// 17.7.2.3: the SELECT inside INSERT INTO ... SELECT is not a consistent read of the
// transaction's snapshot. In REPEATABLE READ it "acts like READ COMMITTED, where each
// consistent read ... sets and reads its own fresh snapshot" (and takes shared locks:
// TestLock_InsertSelectLocksSource), so it copies a row the transaction itself cannot see.
func TestMVCC_InsertSelectReadsLatestCommitted(t *testing.T) {
	L := newLab(t)
	setupT(L)
	L.Exec("DROP TABLE IF EXISTS dst", "CREATE TABLE dst (id int PRIMARY KEY)")
	a, b := L.Session("A"), L.Session("B")
	a.Do("START TRANSACTION")
	a.Do("SELECT id FROM t ORDER BY id").MustRows("(10) (20) (30)")
	b.Do("INSERT INTO t VALUES (40, 40, 0)")
	a.Do("INSERT INTO dst SELECT id FROM t").MustAffect(4)
	a.Do("SELECT id FROM t ORDER BY id").MustRows("(10) (20) (30)")
	a.Do("SELECT id FROM dst ORDER BY id").MustRows("(10) (20) (30) (40)")
	a.Do("COMMIT")
}

// 17.7.2.3: the manual's timeline (its table t is called tp here). With autocommit=0, A only
// sees B's committed row after A itself commits and so takes a new snapshot.
func TestMVCC_TimepointAdvancesAfterCommitDocsExample(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS tp", "CREATE TABLE tp (a int, b int)")
	a, b := L.Session("A"), L.Session("B")
	a.Do("SET autocommit = 0")
	b.Do("SET autocommit = 0")
	a.Do("SELECT * FROM tp").MustRows("(no rows)")
	b.Do("INSERT INTO tp VALUES (1, 2)")
	a.Do("SELECT * FROM tp").MustRows("(no rows)")
	b.Do("COMMIT")
	a.Do("SELECT * FROM tp").MustRows("(no rows)")
	a.Do("COMMIT")
	a.Do("SELECT * FROM tp").MustRows("(1,2)")
	a.Do("COMMIT")
}

// 17.7.2.3: a consistent read does not work across DROP TABLE or a table-copying ALTER TABLE.
func TestMVCC_ConsistentReadOverDDL(t *testing.T) {
	L := newLab(t)
	setupT(L)
	rd, ddl := L.Session("RD"), L.Session("DDL")
	rd.Do("START TRANSACTION WITH CONSISTENT SNAPSHOT")
	ddl.Do("ALTER TABLE t FORCE, ALGORITHM=COPY").MustOK()
	rd.Do("SELECT count(*) FROM t").MustFail(func(err error) bool { return lab.Code(err) == "1412" })
	rd.Do("COMMIT")

	rd.Do("START TRANSACTION WITH CONSISTENT SNAPSHOT")
	ddl.Do("DROP TABLE t").MustOK()
	rd.Do("SELECT count(*) FROM t").MustFail(func(err error) bool { return lab.Code(err) == "1146" })
	rd.Do("COMMIT")
}

// 17.7.2.3 / 17.7.3: INSERT INTO T SELECT ... FROM S (and UPDATE ... WHERE col IN
// (SELECT ... FROM S)) read S with shared next-key locks in REPEATABLE READ, so that
// statement-based replay gives the same result. The locks are next-key locks when the
// subquery scans a range of S (the idx_k variant), and record-only when it is resolved by
// unique lookups (the semijoin on the primary key): the manual's own rule that "only an index
// record lock is required for statements that lock rows using a unique index to search for a
// unique row". READ COMMITTED and READ UNCOMMITTED read S without locks; the manual says so
// only for INSERT ... SELECT, for REPLACE and UPDATE it is measured.
func TestLock_InsertSelectLocksSource(t *testing.T) {
	for _, level := range []lab.Level{lab.RepeatableRead, lab.ReadCommitted, lab.ReadUncommitted} {
		for _, stmt := range []string{
			"INSERT INTO dst SELECT id FROM t FORCE INDEX (PRIMARY) WHERE id <= 20",
			"UPDATE dst SET id = id + 1000 WHERE id IN (SELECT id FROM t FORCE INDEX (PRIMARY) WHERE id <= 20)",
			"REPLACE INTO dst SELECT id FROM t FORCE INDEX (PRIMARY) WHERE id <= 20",
			"UPDATE dst SET id = id + 1000 WHERE id IN (SELECT k FROM t WHERE k <= 20)",
		} {
			byRange := strings.Contains(stmt, "SELECT k")
			name := level.Short() + "/" + strings.Fields(stmt)[0]
			if byRange {
				name += "-subquery-range-scan"
			}
			t.Run(name, func(t *testing.T) {
				L := newLab(t)
				setupT(L)
				L.Exec("DROP TABLE IF EXISTS dst", "CREATE TABLE dst (id int PRIMARY KEY)", "INSERT INTO dst VALUES (10), (20)")
				t1, t2 := L.Session("T1"), L.Session("T2")
				t1.Begin(level)
				if strings.HasPrefix(stmt, "INSERT") {
					t1.Do("DELETE FROM dst")
				}
				t1.Do(stmt).MustOK()
				var onSource []string
				for _, k := range L.ShowLocks() {
					if k.Session == "T1" && k.Type == "RECORD" && strings.HasPrefix(k.Object, "t.") {
						onSource = append(onSource, k.Mode+" "+k.Data)
					}
				}
				upd := t2.Do("UPDATE t SET v = 1 WHERE id = 10")
				if byRange {
					// The S locks are on idx_k only (a shared lock on a covering secondary
					// index does not lock the clustered record), so probe the locked gap.
					upd.MustOK()
					upd = t2.Do("INSERT INTO t VALUES (15, 15, 0)")
				}
				if level == lab.RepeatableRead {
					// INSERT ... SELECT range-scans t: next-key locks, as the manual says.
					// UPDATE ... IN (SELECT id ...) is rewritten to a semijoin that looks t up
					// by primary key: unique lookups, so record-only S locks.
					want := []string{"S 10", "S 20"}
					switch {
					case byRange:
						want = []string{"S 10, 10", "S 20, 20"}
					case strings.HasPrefix(stmt, "UPDATE"):
						want = []string{"S,REC_NOT_GAP 10", "S,REC_NOT_GAP 20"}
					}
					if !slices.Contains(onSource, want[0]) || !slices.Contains(onSource, want[1]) {
						t.Fatalf("source locks %v, want %v", onSource, want)
					}
					upd.MustBlock()
				} else {
					if len(onSource) != 0 {
						t.Fatalf("%s should not lock the source, got %v", level, onSource)
					}
					upd.MustNotBlock()
				}
				t1.Do("ROLLBACK")
				upd.MustOK()
			})
		}
	}
}

// 17.7.2.4 Locking Reads. The manual's NOWAIT / SKIP LOCKED table t is called tn here.
func TestLock_LockingReadsDocs(t *testing.T) {
	t.Run("nowait-skip-locked", func(t *testing.T) {
		L := newLab(t)
		L.Exec("DROP TABLE IF EXISTS tn", "CREATE TABLE tn (i INT, PRIMARY KEY (i)) ENGINE = InnoDB", "INSERT INTO tn (i) VALUES (1),(2),(3)")
		s1, s2, s3 := L.Session("S1"), L.Session("S2"), L.Session("S3")
		s1.Do("START TRANSACTION")
		s1.Do("SELECT * FROM tn WHERE i = 2 FOR UPDATE").MustRows("(2)")
		s2.Do("START TRANSACTION")
		s2.Do("SELECT * FROM tn WHERE i = 2 FOR UPDATE NOWAIT").MustFail(func(err error) bool { return lab.Code(err) == "3572" })
		s3.Do("START TRANSACTION")
		s3.Do("SELECT * FROM tn FOR UPDATE SKIP LOCKED").MustRows("(1) (3)")
		s3.Do("SELECT * FROM tn WHERE i = 2 FOR SHARE NOWAIT").MustFail(func(err error) bool { return lab.Code(err) == "3572" })
		s3.Do("SELECT * FROM tn FOR SHARE SKIP LOCKED").MustRows("(1) (3)")
		L.Note("the old spelling takes none of the new options")
		for _, opt := range []string{"NOWAIT", "SKIP LOCKED", "OF tn"} {
			s3.Do("SELECT * FROM tn WHERE i = 1 LOCK IN SHARE MODE " + opt).MustFail(func(err error) bool { return lab.Code(err) == "1064" })
		}
	})

	t.Run("nowait-applies-to-row-locks-only", func(t *testing.T) {
		L := newLab(t)
		L.Exec("DROP TABLE IF EXISTS tn", "CREATE TABLE tn (i INT, PRIMARY KEY (i)) ENGINE = InnoDB", "INSERT INTO tn (i) VALUES (1),(2),(3)")
		s1, s2 := L.Session("S1"), L.Session("S2")
		s1.Do("LOCK TABLES tn WRITE")
		L.Note("a table lock is not a row lock: NOWAIT waits for it")
		nw := s2.Do("SELECT * FROM tn WHERE i = 2 FOR UPDATE NOWAIT").MustBlock()
		s1.Do("UNLOCK TABLES")
		nw.MustRows("(2)")
	})

	t.Run("nowait-skip-locked-unsafe-for-statement-binlog", func(t *testing.T) {
		L := newLab(t)
		if L.QueryString("SELECT @@log_bin") != "1" {
			t.Skip("binary logging is off")
		}
		L.Exec("DROP TABLE IF EXISTS tn", "DROP TABLE IF EXISTS tn2", "CREATE TABLE tn (i INT, PRIMARY KEY (i)) ENGINE = InnoDB",
			"CREATE TABLE tn2 (i INT)", "INSERT INTO tn (i) VALUES (1),(2),(3)")
		s := L.Session("S")
		s.Do("SET SESSION binlog_format = 'STATEMENT'")
		s.Do("START TRANSACTION")
		s.Do("INSERT INTO tn2 SELECT i FROM tn FOR UPDATE SKIP LOCKED").MustOK()
		w := s.Do("SHOW WARNINGS").Text()
		if !strings.Contains(w, "1592") {
			t.Fatalf("expected the unsafe-statement warning 1592, got %q", w)
		}
		s.Do("ROLLBACK")
	})

	t.Run("lock-in-share-mode-equals-for-share", func(t *testing.T) {
		L := newLab(t)
		setupT(L)
		t1 := L.Session("T1")
		var got [][]string
		for _, q := range []string{"SELECT * FROM t WHERE id = 10 FOR SHARE", "SELECT * FROM t WHERE id = 10 LOCK IN SHARE MODE"} {
			t1.Do("START TRANSACTION")
			t1.Do(q)
			got = append(got, recordLocks(L.ShowLocks(), "T1"))
			t1.Do("ROLLBACK")
		}
		if !slices.Equal(got[0], got[1]) || !slices.Equal(got[0], []string{"S,REC_NOT_GAP 10"}) {
			t.Fatalf("FOR SHARE %v vs LOCK IN SHARE MODE %v", got[0], got[1])
		}
	})

	t.Run("autocommit-locking-read-releases-at-once", func(t *testing.T) {
		L := newLab(t)
		setupT(L)
		t1, t2 := L.Session("T1"), L.Session("T2")
		t1.Do("SELECT * FROM t WHERE id = 10 FOR UPDATE").MustOK()
		t2.Do("UPDATE t SET v = 1 WHERE id = 10").MustNotBlock().MustOK()
	})

	t.Run("subquery-rows-not-locked-unless-asked", func(t *testing.T) {
		L := newLab(t)
		L.Exec("DROP TABLE IF EXISTS sq1", "DROP TABLE IF EXISTS sq2",
			"CREATE TABLE sq1 (c1 int PRIMARY KEY)", "CREATE TABLE sq2 (c1 int PRIMARY KEY)",
			"INSERT INTO sq1 VALUES (1)", "INSERT INTO sq2 VALUES (1)")
		t1, t2 := L.Session("T1"), L.Session("T2")
		t1.Do("START TRANSACTION")
		t1.Do("SELECT * FROM sq1 WHERE c1 = (SELECT c1 FROM sq2) FOR UPDATE").MustRows("(1)")
		t2.Do("UPDATE sq2 SET c1 = c1 WHERE c1 = 1").MustNotBlock().MustOK()
		t1.Do("ROLLBACK")
		t1.Do("START TRANSACTION")
		t1.Do("SELECT * FROM sq1 WHERE c1 = (SELECT c1 FROM sq2 FOR UPDATE) FOR UPDATE").MustRows("(1)")
		upd := t2.Do("UPDATE sq2 SET c1 = c1 WHERE c1 = 1").MustBlock()
		t1.Do("ROLLBACK")
		upd.MustOK()
	})

	t.Run("for-update-of-table", func(t *testing.T) {
		L := newLab(t)
		L.Exec("DROP TABLE IF EXISTS sq1", "DROP TABLE IF EXISTS sq2",
			"CREATE TABLE sq1 (c1 int PRIMARY KEY)", "CREATE TABLE sq2 (c1 int PRIMARY KEY)",
			"INSERT INTO sq1 VALUES (1)", "INSERT INTO sq2 VALUES (1)")
		t1, t2 := L.Session("T1"), L.Session("T2")
		t1.Do("START TRANSACTION")
		t1.Do("SELECT * FROM sq1 JOIN sq2 USING (c1) FOR UPDATE OF sq1").MustRows("(1)")
		t2.Do("UPDATE sq2 SET c1 = c1 WHERE c1 = 1").MustNotBlock().MustOK()
		upd := t2.Do("UPDATE sq1 SET c1 = c1 WHERE c1 = 1").MustBlock()
		t1.Do("ROLLBACK")
		upd.MustOK()
	})

	t.Run("parent-child-for-share", func(t *testing.T) {
		L := newLab(t)
		L.Exec("DROP TABLE IF EXISTS child", "DROP TABLE IF EXISTS parent",
			"CREATE TABLE parent (id int PRIMARY KEY, name varchar(10))", "CREATE TABLE child (id int PRIMARY KEY, parent_name varchar(10))",
			"INSERT INTO parent VALUES (1, 'Jones')")
		t1, t2 := L.Session("T1"), L.Session("T2")
		L.Note("a consistent read protects nothing: Jones can be deleted right after T1 saw it")
		t1.Do("START TRANSACTION")
		t1.Do("SELECT * FROM parent WHERE name = 'Jones'").MustRows("(1,Jones)")
		t2.Do("DELETE FROM parent WHERE id = 1").MustNotBlock().MustAffect(1)
		t1.Do("ROLLBACK")
		t2.Do("INSERT INTO parent VALUES (1, 'Jones')")
		t1.Do("START TRANSACTION")
		t1.Do("SELECT * FROM parent WHERE name = 'Jones' FOR SHARE").MustRows("(1,Jones)")
		L.Note("Jones cannot be deleted until T1 has added its child row and committed")
		del := t2.Do("DELETE FROM parent WHERE id = 1").MustBlock()
		t1.Do("INSERT INTO child VALUES (1, 'Jones')").MustNotBlock().MustOK()
		t1.Do("COMMIT")
		del.MustAffect(1)
	})

	t.Run("for-share-waits-for-uncommitted-writer", func(t *testing.T) {
		L := newLab(t)
		setupT(L)
		t1, t2 := L.Session("T1"), L.Session("T2")
		t1.Do("START TRANSACTION")
		t1.Do("UPDATE t SET v = 7 WHERE id = 10")
		t2.Do("START TRANSACTION")
		t2.Do("SELECT v FROM t WHERE id = 10").MustNotBlock().MustRows("(0)")
		sh := t2.Do("SELECT v FROM t WHERE id = 10 FOR SHARE").MustBlock()
		t1.Do("COMMIT")
		sh.MustRows("(7)")
		t2.Do("COMMIT")
	})

	t.Run("counter-for-update", func(t *testing.T) {
		L := newLab(t)
		L.Exec("DROP TABLE IF EXISTS child_codes", "CREATE TABLE child_codes (counter_field int)", "INSERT INTO child_codes VALUES (0)",
			"DROP TABLE IF EXISTS child", "CREATE TABLE child (id int PRIMARY KEY)")
		t1, t2 := L.Session("T1"), L.Session("T2")
		L.Note("a consistent read is no good for this: both sessions see the same value, and\n" +
			"\"a duplicate-key error occurs if two transactions attempt to add rows with the same identifier\"")
		t1.Do("START TRANSACTION")
		t2.Do("START TRANSACTION")
		t1.Do("SELECT counter_field FROM child_codes").MustRows("(0)")
		t2.Do("SELECT counter_field FROM child_codes").MustNotBlock().MustRows("(0)")
		t1.Do("INSERT INTO child VALUES (0 + 1)").MustOK()
		dup := t2.Do("INSERT INTO child VALUES (0 + 1)").MustBlock()
		t1.Do("COMMIT")
		dup.MustFail(lab.IsUniqueViolation)
		t2.Do("ROLLBACK")
		t1.Do("START TRANSACTION")
		t1.Do("SELECT counter_field FROM child_codes FOR UPDATE").MustRows("(0)")
		t2.Do("START TRANSACTION")
		r2 := t2.Do("SELECT counter_field FROM child_codes FOR UPDATE").MustBlock()
		t1.Do("UPDATE child_codes SET counter_field = counter_field + 1")
		t1.Do("COMMIT")
		r2.MustRows("(1)")
		t2.Do("UPDATE child_codes SET counter_field = counter_field + 1")
		t2.Do("COMMIT")

		L.Note("the single-statement form: LAST_INSERT_ID(expr) remembers the value per connection")
		t1.Do("UPDATE child_codes SET counter_field = LAST_INSERT_ID(counter_field + 1)")
		t2.Do("UPDATE child_codes SET counter_field = LAST_INSERT_ID(counter_field + 1)")
		t1.Do("SELECT LAST_INSERT_ID()").MustRows("(3)")
		t2.Do("SELECT LAST_INSERT_ID()").MustRows("(4)")
	})
}

// 17.7.3 Locks Set by Different SQL Statements: the record locks each statement leaves
// behind, as reported by performance_schema.data_locks on MySQL 9.7, as "index MODE data".
//
// Tables: t (id PK, k non-unique) = 10/20/30, tu (id PK, u UNIQUE, v) = (1,10) (2,20) (3,30),
// and fkc (id PK, pid → t.id), empty.
//
// update-duplicate-unique is the manual's "UPDATE ... takes shared locks on affected secondary
// index records when performing duplicate check scans": the failed check leaves S on uu's 30.
//
// Where 9.7 sets more than the manual lists, the case says why:
//   - delete-unique: deleting a parent row checks the FK in the child table; the empty child
//     index gets a shared lock on its supremum (the manual's "shared record-level locks on
//     the records that it looks at to check the constraint", on the child side).
//   - fk-check-fails-RC, delete-unique-RC: the manual says READ COMMITTED still uses gap locks
//     for foreign-key checks. Neither check leaves one on 9.7: the failed child insert leaves
//     nothing (REPEATABLE READ leaves S,GAP), the parent delete no lock in the child index.
//   - for-update-*-nonmatching: the manual says a locking read releases the locks of rows that
//     fail the WHERE clause. That happens only in READ COMMITTED, and not for a row found by a
//     constant primary-key lookup; REPEATABLE READ keeps every lock. The UNION exception
//     (rows that go into a temporary table before they are evaluated stay locked) needs the
//     condition to be evaluated after the temporary table: with "v = 99" inside each branch
//     the rows are released (for-update-union-nonmatching-RC); with it outside the UNION,
//     and kept from being pushed down, all three stay locked (for-update-union-outer-nonmatching-RC).
//   - replace-collision: the manual says "an exclusive next-key lock is placed on the row to be
//     replaced". For a primary-key collision it is a record lock without the gap
//     (X,REC_NOT_GAP); a collision on a unique secondary index does take next-key locks
//     (replace-collision-unique).
//   - insert-duplicate-unique: the clustered record for id 9 is inserted before the duplicate
//     in uu is found. Rolling it back leaves its gap locked (lock inheritance): X on the
//     PRIMARY supremum. odku-duplicate-unique leaves the same inherited lock.
func TestLock_LocksSetByStatement(t *testing.T) {
	cases := []struct {
		name, stmt string
		level      lab.Level
		fails      string // expected error number; "" = succeeds
		want       []string
	}{
		{"select-RR", "SELECT * FROM t WHERE id BETWEEN 10 AND 20", lab.RepeatableRead, "", nil},
		{"select-SER-unique", "SELECT * FROM t WHERE id = 20", lab.Serializable, "", []string{"t.PRIMARY S,REC_NOT_GAP 20"}},
		{"select-SER-range", "SELECT * FROM t WHERE id > 15", lab.Serializable, "",
			[]string{"t.PRIMARY S 20", "t.PRIMARY S 30", "t.PRIMARY S supremum pseudo-record"}},
		{"update-unique", "UPDATE t SET v = 1 WHERE id = 20", "", "", []string{"t.PRIMARY X,REC_NOT_GAP 20"}},
		{"update-range", "UPDATE t SET v = 1 WHERE id > 15", "", "",
			[]string{"t.PRIMARY X 20", "t.PRIMARY X 30", "t.PRIMARY X supremum pseudo-record"}},
		{"delete-unique", "DELETE FROM t WHERE id = 20", "", "",
			[]string{"fkc.pid S supremum pseudo-record", "t.PRIMARY X,REC_NOT_GAP 20"}},
		{"delete-missing", "DELETE FROM t WHERE id = 25", "", "", []string{"t.PRIMARY X,GAP 30"}},
		{"insert", "INSERT INTO t VALUES (25, 25, 0)", "", "", nil},
		{"insert-duplicate-pk", "INSERT INTO t VALUES (20, 0, 0)", "", "1062", []string{"t.PRIMARY S,REC_NOT_GAP 20"}},
		{"insert-duplicate-unique", "INSERT INTO tu (id, u) VALUES (9, 20)", "", "1062",
			[]string{"tu.PRIMARY X supremum pseudo-record", "tu.uu S 20, 2"}},
		{"odku-duplicate-pk", "INSERT INTO t VALUES (20, 0, 0) ON DUPLICATE KEY UPDATE v = 9", "", "", []string{"t.PRIMARY X,REC_NOT_GAP 20"}},
		{"odku-duplicate-unique", "INSERT INTO tu (id, u) VALUES (9, 20) ON DUPLICATE KEY UPDATE v = 9", "", "",
			[]string{"tu.PRIMARY X supremum pseudo-record", "tu.PRIMARY X,REC_NOT_GAP 2", "tu.uu X 20, 2"}},
		{"update-duplicate-unique", "UPDATE tu SET u = 30 WHERE id = 2", "", "1062",
			[]string{"tu.PRIMARY X,REC_NOT_GAP 2", "tu.uu S 30, 3"}},
		{"replace-new", "REPLACE INTO t VALUES (25, 25, 0)", "", "", nil},
		{"replace-collision", "REPLACE INTO t VALUES (20, 0, 0)", "", "", []string{"t.PRIMARY X,REC_NOT_GAP 20"}},
		{"fk-check", "INSERT INTO fkc VALUES (1, 20)", "", "", []string{"t.PRIMARY S,REC_NOT_GAP 20"}},
		{"fk-check-fails", "INSERT INTO fkc VALUES (1, 25)", "", "1452", []string{"t.PRIMARY S,GAP 30"}},
		{"fk-check-fails-RC", "INSERT INTO fkc VALUES (1, 25)", lab.ReadCommitted, "1452", nil},
		{"delete-unique-RC", "DELETE FROM t WHERE id = 20", lab.ReadCommitted, "", []string{"t.PRIMARY X,REC_NOT_GAP 20"}},
		{"update-range-RC", "UPDATE t SET v = 1 WHERE id > 15", lab.ReadCommitted, "",
			[]string{"t.PRIMARY X,REC_NOT_GAP 20", "t.PRIMARY X,REC_NOT_GAP 30"}},
		{"for-update-const-nonmatching", "SELECT * FROM t WHERE id = 20 AND v = 99 FOR UPDATE", "", "", []string{"t.PRIMARY X,REC_NOT_GAP 20"}},
		{"for-update-const-nonmatching-RC", "SELECT * FROM t WHERE id = 20 AND v = 99 FOR UPDATE", lab.ReadCommitted, "", []string{"t.PRIMARY X,REC_NOT_GAP 20"}},
		{"for-update-range-nonmatching", "SELECT * FROM t WHERE id >= 20 AND v = 99 FOR UPDATE", "", "",
			[]string{"t.PRIMARY X 30", "t.PRIMARY X supremum pseudo-record", "t.PRIMARY X,REC_NOT_GAP 20"}},
		{"for-update-range-nonmatching-RC", "SELECT * FROM t WHERE id >= 20 AND v = 99 FOR UPDATE", lab.ReadCommitted, "", nil},
		{"for-update-union-nonmatching-RC", "(SELECT * FROM t WHERE id >= 20 AND v = 99 FOR UPDATE) UNION (SELECT * FROM t WHERE id <= 10 AND v = 99 FOR UPDATE)", lab.ReadCommitted, "", nil},
		{"for-update-union-outer-nonmatching-RC", "SELECT /*+ NO_DERIVED_CONDITION_PUSHDOWN(d) */ * FROM ((SELECT * FROM t WHERE id >= 20 FOR UPDATE) UNION (SELECT * FROM t WHERE id <= 10 FOR UPDATE)) d WHERE v = 99", lab.ReadCommitted, "",
			[]string{"t.PRIMARY X,REC_NOT_GAP 10", "t.PRIMARY X,REC_NOT_GAP 20", "t.PRIMARY X,REC_NOT_GAP 30"}},
		{"delete-range", "DELETE FROM t WHERE id > 15", "", "",
			[]string{"fkc.pid S supremum pseudo-record", "t.PRIMARY X 20", "t.PRIMARY X 30", "t.PRIMARY X supremum pseudo-record"}},
		{"replace-collision-unique", "REPLACE INTO tu VALUES (9, 20, 0)", "", "",
			[]string{"tu.PRIMARY X supremum pseudo-record", "tu.PRIMARY X,GAP 9", "tu.PRIMARY X,REC_NOT_GAP 2",
				"tu.uu X 20, 2", "tu.uu X 30, 3", "tu.uu X,GAP 20, 9"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			L := newLab(t)
			setupT(L)
			L.Exec("DROP TABLE IF EXISTS fkc", "DROP TABLE IF EXISTS tu",
				"CREATE TABLE tu (id int PRIMARY KEY, u int, v int, UNIQUE KEY uu (u))",
				"INSERT INTO tu VALUES (1, 10, 0), (2, 20, 0), (3, 30, 0)",
				"CREATE TABLE fkc (id int PRIMARY KEY, pid int, FOREIGN KEY (pid) REFERENCES t (id))")
			s := L.Session("T1")
			level := c.level
			if level == "" {
				level = lab.RepeatableRead
			}
			s.Begin(level)
			if op := s.Do(c.stmt); c.fails != "" {
				op.MustFail(func(err error) bool { return lab.Code(err) == c.fails })
			} else {
				op.MustOK()
			}
			var got []string
			for _, k := range L.ShowLocks() {
				if k.Session == "T1" && k.Type == "RECORD" {
					got = append(got, k.Object+" "+k.Mode+" "+k.Data)
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, c.want) {
				t.Fatalf("record locks %v, want %v", got, c.want)
			}
			s.Do("ROLLBACK")
		})
	}
}

// 17.7.5.1 An InnoDB Deadlock Example: the manual's Animals / Birds walk-through, including
// what data_locks, data_lock_waits, INNODB_METRICS and the error log show.
func TestDeadlock_ManualAnimalsBirds(t *testing.T) {
	L := newLab(t)
	setGlobal(t, L, "innodb_print_all_deadlocks", "ON")
	setGlobal(t, L, "log_error_verbosity", "3") // the deadlock dump is logged at Note level
	L.Exec("DROP TABLE IF EXISTS Animals", "DROP TABLE IF EXISTS Birds",
		"CREATE TABLE Animals (name VARCHAR(10) PRIMARY KEY, value INT) ENGINE = InnoDB",
		"CREATE TABLE Birds (name VARCHAR(10) PRIMARY KEY, value INT) ENGINE = InnoDB",
		`INSERT INTO Animals (name,value) VALUES ("Aardvark",10)`,
		`INSERT INTO Birds (name,value) VALUES ("Buzzard",20)`)
	before := innodbDeadlocks(L)
	start := L.QueryString("SELECT NOW(6)")

	a, b := L.Session("A"), L.Session("B")
	a.Do("START TRANSACTION")
	a.Do("SELECT value FROM Animals WHERE name='Aardvark' FOR SHARE").MustRows("(10)")
	b.Do("START TRANSACTION")
	b.Do("SELECT value FROM Birds WHERE name='Buzzard' FOR SHARE").MustRows("(20)")
	locks := L.ShowLocks()
	mustHaveLock(t, locks, "A", "IS", "")
	mustHaveLock(t, locks, "A", "S,REC_NOT_GAP", "'Aardvark'")
	mustHaveLock(t, locks, "B", "IS", "")
	mustHaveLock(t, locks, "B", "S,REC_NOT_GAP", "'Buzzard'")
	if len(locks) != 4 {
		t.Fatalf("data_locks has %d rows, the manual shows 4: %v", len(locks), locks)
	}
	// "InnoDB only uses sequential transaction ids when a transaction attempts to modify the
	// database": B's id changes from a read-only one (>= 2^48) to a sequential one as soon as
	// its UPDATE starts, while it is still waiting. A has only read so far.
	trxID := func(s *lab.Session) uint64 {
		lab.FreshInnoDBTrx()
		var id uint64
		fmt.Sscan(L.QueryString("SELECT trx_id FROM information_schema.innodb_trx WHERE trx_mysql_thread_id = ?", s.ID), &id)
		return id
	}
	const readOnlyIDs = uint64(1) << 48
	if id := trxID(b); id < readOnlyIDs {
		t.Fatalf("B has only read, yet its trx_id %d is a sequential one", id)
	}
	upd := b.Do("UPDATE Animals SET value=30 WHERE name='Aardvark'").MustBlock()
	idA, idB := trxID(a), trxID(b)
	L.Note("trx_id while B's UPDATE waits: A %d (read-only), B %d (sequential)", idA, idB)
	if idA < readOnlyIDs || idB >= readOnlyIDs {
		t.Fatalf("trx ids A %d, B %d: want A read-only (>= 2^48) and B sequential", idA, idB)
	}
	locks = L.ShowLocks()
	mustHaveLock(t, locks, "B", "IX", "")
	if k, ok := lab.FindLock(locks, "B", "X,REC_NOT_GAP", "'Aardvark'"); !ok || k.Granted {
		t.Fatalf("B should be WAITING for X,REC_NOT_GAP on 'Aardvark': %v", locks)
	}
	if len(locks) != 6 {
		t.Fatalf("data_locks has %d rows, the manual shows 6: %v", len(locks), locks)
	}
	waits := L.Show(`SELECT r.PROCESSLIST_ID AS requesting, b.PROCESSLIST_ID AS blocking
FROM performance_schema.data_lock_waits w
JOIN performance_schema.threads r ON r.THREAD_ID = w.REQUESTING_THREAD_ID
JOIN performance_schema.threads b ON b.THREAD_ID = w.BLOCKING_THREAD_ID`)
	if len(waits) != 1 || waits[0][0] != fmt.Sprint(b.ID) || waits[0][1] != fmt.Sprint(a.ID) {
		t.Fatalf("data_lock_waits = %v, want one row: B (%d) waiting for A (%d)", waits, b.ID, a.ID)
	}
	dl := a.Do("UPDATE Birds SET value=40 WHERE name='Buzzard'")
	dl.MustFail(lab.IsDeadlock)
	if !strings.Contains(lab.Describe(dl.Err), "ERROR 1213 (40001): Deadlock found when trying to get lock; try restarting transaction") {
		t.Fatalf("unexpected error text: %s", lab.Describe(dl.Err))
	}
	L.ShowDeadlock(dl)
	status := L.Query("SHOW ENGINE INNODB STATUS")[0][2]
	if i := strings.Index(status, "LATEST DETECTED DEADLOCK"); i < 0 {
		t.Fatal("SHOW ENGINE INNODB STATUS has no LATEST DETECTED DEADLOCK section")
	} else {
		status = status[i:]
	}
	for _, want := range []string{
		"UPDATE Animals SET value=30 WHERE name='Aardvark'", "UPDATE Birds SET value=40 WHERE name='Buzzard'",
		"lock mode S locks rec but not gap", "lock_mode X locks rec but not gap waiting",
		"*** WE ROLL BACK TRANSACTION (2)",
	} {
		if !strings.Contains(status, want) {
			t.Errorf("LATEST DETECTED DEADLOCK does not contain %q", want)
		}
	}
	// "It also shows that the read-only transaction id ... changes to sequential transaction
	// id": in the dump B is transaction (1), under the id it had while waiting, and A is
	// transaction (2), under a sequential id it got when its UPDATE started.
	ids := regexp.MustCompile(`\*\*\* \((\d)\) TRANSACTION:\nTRANSACTION (\d+),`).FindAllStringSubmatch(status, -1)
	if len(ids) != 2 || ids[0][2] != fmt.Sprint(idB) {
		t.Fatalf("deadlock dump lists transactions %v, want two with B's id %d first", ids, idB)
	}
	if dumpA, _ := strconv.ParseUint(ids[1][2], 10, 64); dumpA == 0 || dumpA >= readOnlyIDs {
		t.Fatalf("A appears in the dump as transaction %s, want a sequential id (its read-only id was %d)", ids[1][2], idA)
	}
	upd.MustAffect(1)
	b.Do("COMMIT")

	if got := innodbDeadlocks(L) - before; got != 1 {
		t.Fatalf("lock_deadlocks went up by %d, want 1", got)
	}
	logged := L.QueryString("SELECT count(*) FROM performance_schema.error_log WHERE LOGGED >= ? AND DATA LIKE '%deadlock%'", start)
	L.Note("error_log entries mentioning the deadlock (innodb_print_all_deadlocks=ON): %s", logged)
	if logged == "0" {
		t.Fatal("innodb_print_all_deadlocks did not write to the error log")
	}

	L.Note(`17.7.5.3: "Always be prepared to re-issue a transaction if it fails due to deadlock. Deadlocks are
not dangerous. Just try again." A's transaction is gone, so it starts a new one`)
	a.Do("START TRANSACTION")
	a.Do("SELECT value FROM Animals WHERE name='Aardvark' FOR SHARE").MustRows("(30)")
	a.Do("UPDATE Birds SET value=40 WHERE name='Buzzard'").MustNotBlock().MustAffect(1)
	a.Do("COMMIT").MustOK()
	a.Do("SELECT value FROM Birds WHERE name='Buzzard'").MustRows("(40)")
}

// 17.7.5.2 Deadlock Detection: InnoDB rolls back the smaller transaction (fewer rows
// inserted, updated or deleted), whichever one happened to close the cycle.
func TestDeadlock_VictimIsSmallerTransaction(t *testing.T) {
	for _, bigClosesCycle := range []bool{false, true} {
		name := "small-closes-cycle"
		if bigClosesCycle {
			name = "big-closes-cycle"
		}
		t.Run(name, func(t *testing.T) {
			L := newLab(t)
			L.Exec("DROP TABLE IF EXISTS w", "CREATE TABLE w (id int PRIMARY KEY, v int)",
				"INSERT INTO w SELECT id, 0 FROM (SELECT 1 id UNION SELECT 2 UNION SELECT 3 UNION SELECT 4 UNION SELECT 5 UNION SELECT 6) x",
				"INSERT INTO w VALUES (100, 0), (200, 0)")
			big, small := L.Session("BIG"), L.Session("SMALL")
			big.Do("START TRANSACTION")
			small.Do("START TRANSACTION")
			big.Do("UPDATE w SET v = 1 WHERE id <= 6").MustAffect(6)
			big.Do("UPDATE w SET v = 1 WHERE id = 100")
			small.Do("UPDATE w SET v = 2 WHERE id = 200")
			var ops []*lab.Op
			if bigClosesCycle {
				ops = append(ops, small.Do("UPDATE w SET v = 2 WHERE id = 100"), big.Do("UPDATE w SET v = 1 WHERE id = 200"))
			} else {
				ops = append(ops, big.Do("UPDATE w SET v = 1 WHERE id = 200"), small.Do("UPDATE w SET v = 2 WHERE id = 100"))
			}
			victim := lab.ExpectOneDeadlockVictim(t, ops...)
			L.ShowDeadlock(victim)
			if victim.S != small {
				t.Fatalf("victim is %s, want SMALL", victim.S.Name)
			}
			big.Do("ROLLBACK")
		})
	}
}

// 17.7.5.2: "a wait-for list that exceeds 200 transactions is treated as a deadlock and the
// transaction attempting to check the wait-for list is rolled back". Measured on 9.7: it is
// not. 250 transactions each hold a row and wait for the previous one's row, a wait-for
// chain 249 transactions deep, and every one of them keeps waiting (lock_deadlocks does not
// move). Closing the chain into a 250-transaction cycle is detected at once: exactly one
// transaction gets 1213.
//
// The chain runs on plain connections from a pool of its own: the harness polls every
// blocked session after every step, which is quadratic in the number of waiters.
func TestDeadlock_LongWaitForChainIsNotADeadlock(t *testing.T) {
	const n = 250
	L := newLab(t)
	setGlobal(t, L, "max_connections", strconv.Itoa(n+100))
	L.Exec("DROP TABLE IF EXISTS wchain", "CREATE TABLE wchain (id int PRIMARY KEY, v int)",
		fmt.Sprintf("INSERT INTO wchain WITH RECURSIVE s(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM s WHERE i < %d) SELECT i, 0 FROM s", n))
	pool, err := sql.Open("mysql", lab.DSN(lab.MySQL, dbname))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx := context.Background()
	conns := make([]*sql.Conn, n+1) // conns[i] holds row i
	for i := 1; i <= n; i++ {
		c, err := pool.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		for _, q := range []string{"SET SESSION innodb_lock_wait_timeout = 60", "START TRANSACTION",
			fmt.Sprintf("UPDATE wchain SET v = %d WHERE id = %d", i, i)} {
			if _, err := c.ExecContext(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		conns[i] = c
	}
	waits := func() int {
		var w int
		if err := L.Admin.QueryRow(`SELECT count(*) FROM performance_schema.data_locks
WHERE OBJECT_SCHEMA = DATABASE() AND OBJECT_NAME = 'wchain' AND LOCK_STATUS = 'WAITING'`).Scan(&w); err != nil {
			t.Fatal(err)
		}
		return w
	}
	type result struct {
		i   int
		err error
	}
	results := make(chan result, n)
	wait := func(i, row int) {
		go func() {
			_, err := conns[i].ExecContext(ctx, fmt.Sprintf("UPDATE wchain SET v = %d WHERE id = %d", i, row))
			results <- result{i, err}
		}()
	}
	before := innodbDeadlocks(L)
	// Ti waits for T(i-1), one at a time, so each new waiter sits at the end of a chain one
	// transaction longer than the last.
	for i := 2; i <= n; i++ {
		wait(i, i-1)
		deadline := time.Now().Add(10 * time.Second)
		for waits() < i-1 {
			select {
			case r := <-results:
				t.Fatalf("T%d's wait (chain depth %d) ended with %v", r.i, r.i-1, r.err)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("T%d never showed up in data_lock_waits", i)
			}
			time.Sleep(time.Millisecond)
		}
	}
	time.Sleep(200 * time.Millisecond) // give the background deadlock detector a chance
	select {
	case r := <-results:
		t.Fatalf("T%d's wait ended with %v", r.i, r.err)
	default:
	}
	L.Note("a wait-for chain %d transactions deep: %d waits, lock_deadlocks moved by %d", n-1, waits(), innodbDeadlocks(L)-before)
	if w := waits(); w != n-1 {
		t.Fatalf("%d lock waits, want %d", w, n-1)
	}
	if d := innodbDeadlocks(L) - before; d != 0 {
		t.Fatalf("lock_deadlocks moved by %d on a chain without a cycle", d)
	}

	L.Note("T1 now waits for T%d: a cycle of %d transactions", n, n)
	wait(1, n)
	// Unwind: whenever a statement returns, roll its transaction back, which grants the
	// next waiter in the chain.
	victims := 0
	for done := 0; done < n; done++ {
		var r result
		select {
		case r = <-results:
		case <-time.After(20 * time.Second):
			t.Fatalf("%d of %d statements returned; the rest are still waiting", done, n)
		}
		if r.err != nil {
			if !lab.IsDeadlock(r.err) {
				t.Fatalf("T%d: %v", r.i, r.err)
			}
			victims++
			L.Note("victim: T%d (%s)", r.i, lab.Describe(r.err))
		}
		if _, err := conns[r.i].ExecContext(ctx, "ROLLBACK"); err != nil {
			t.Fatal(err)
		}
	}
	if d := innodbDeadlocks(L) - before; victims != 1 || d != 1 {
		t.Fatalf("%d victims, lock_deadlocks moved by %d; want 1 and 1", victims, d)
	}
}

// 17.7.5.2: with innodb_deadlock_detect = OFF nothing breaks the cycle except
// innodb_lock_wait_timeout, and the timeout (1205) rolls back only the waiting STATEMENT
// (innodb_rollback_on_timeout is OFF by default): the transaction keeps its locks until
// the application rolls it back.
func TestDeadlock_DetectionDisabledFallsBackToTimeout(t *testing.T) {
	L := newLab(t)
	setupT(L)
	setGlobal(t, L, "innodb_deadlock_detect", "OFF")
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("SET SESSION innodb_lock_wait_timeout = 1")
	t2.Do("SET SESSION innodb_lock_wait_timeout = 5")
	t1.Do("START TRANSACTION")
	t2.Do("START TRANSACTION")
	t1.Do("UPDATE t SET v = 1 WHERE id = 10")
	t2.Do("UPDATE t SET v = 2 WHERE id = 20")
	a := t1.Do("UPDATE t SET v = 1 WHERE id = 20").MustBlock()
	b := t2.Do("UPDATE t SET v = 2 WHERE id = 10").MustBlock()
	L.Note("a real cycle, but nobody detects it")
	a.MustFail(lab.IsLockNotAvailable)
	L.Settle()
	b.MustBlock()
	L.Note("T1 still holds id=10: its timed-out statement was rolled back, its transaction was not")
	t1.Do("ROLLBACK")
	b.MustOK()
	t2.Do("COMMIT")
}

// 17.7.5.2 says that with deadlock detection disabled InnoDB relies on innodb_lock_wait_timeout
// "for transaction rollback". By default (innodb_rollback_on_timeout = OFF) the timeout
// (ERROR 1205) rolls back just the statement; the rest of the transaction can still commit.
func TestTx_LockWaitTimeoutRollsBackOnlyStatement(t *testing.T) {
	L := newLab(t)
	setupT(L)
	L.Show("SELECT @@innodb_rollback_on_timeout, @@innodb_lock_wait_timeout")
	t1, t2 := L.Session("T1"), L.Session("T2")
	t2.Do("START TRANSACTION")
	t2.Do("UPDATE t SET v = 2 WHERE id = 20")
	t1.Do("SET SESSION innodb_lock_wait_timeout = 1")
	t1.Do("START TRANSACTION")
	t1.Do("INSERT INTO t VALUES (40, 40, 0)")
	t1.Do("UPDATE t SET v = 1 WHERE id = 20").MustFail(lab.IsLockNotAvailable)
	t1.Do("COMMIT").MustOK()
	t2.Do("ROLLBACK")
	if n := L.QueryString("SELECT count(*) FROM t WHERE id = 40"); n != "1" {
		t.Fatalf("the INSERT before the timeout was lost")
	}
}

// 17.7.5.3 How to Minimize and Handle Deadlocks: serialize conflicting transactions on a
// "semaphore" row that each updates first. "The InnoDB instant deadlock detection algorithm
// also works in this case, because the serializing lock is a row-level lock": a transaction
// that skips the protocol and closes a cycle through the semaphore row is caught at once
// (ERROR 1213), not after innodb_lock_wait_timeout.
func TestDeadlock_AvoidedBySemaphoreRow(t *testing.T) {
	L := newLab(t)
	setupT(L)
	L.Exec("DROP TABLE IF EXISTS sem", "CREATE TABLE sem (id int PRIMARY KEY)", "INSERT INTO sem VALUES (1)")
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("START TRANSACTION")
	t2.Do("START TRANSACTION")
	t1.Do("UPDATE sem SET id = id WHERE id = 1")
	w := t2.Do("UPDATE sem SET id = id WHERE id = 1").MustBlock()
	t1.Do("DELETE FROM t WHERE id = 10")
	t1.Do("DELETE FROM t WHERE id = 20")
	t1.Do("COMMIT")
	w.MustOK()
	t2.Do("DELETE FROM t WHERE id = 20").MustAffect(0)
	t2.Do("DELETE FROM t WHERE id = 10").MustAffect(0)
	t2.Do("COMMIT")

	L.Note("T2 locks a row before the semaphore; T1, holding the semaphore, then needs that row")
	t1.Do("START TRANSACTION")
	t2.Do("START TRANSACTION")
	t1.Do("UPDATE sem SET id = id WHERE id = 1")
	t2.Do("UPDATE t SET v = 2 WHERE id = 30")
	a := t2.Do("UPDATE sem SET id = id WHERE id = 1").MustBlock()
	before := innodbDeadlocks(L)
	b := t1.Do("UPDATE t SET v = 1 WHERE id = 30")
	lab.ExpectOneDeadlockVictim(t, a, b)
	if n := innodbDeadlocks(L); n != before+1 {
		t.Errorf("lock_deadlocks moved by %d, want 1", n-before)
	}
	t1.Do("ROLLBACK")
	t2.Do("ROLLBACK")
}

// 17.7.5.3: the last-resort fix, serializing with table locks. LOCK TABLES with
// autocommit = 0, and UNLOCK TABLES only after COMMIT.
func TestDeadlock_AvoidedByLockTables(t *testing.T) {
	L := newLab(t)
	setupT(L)
	L.Exec("DROP TABLE IF EXISTS lt2", "CREATE TABLE lt2 (id int PRIMARY KEY)", "INSERT INTO lt2 VALUES (1)")
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("SET autocommit = 0")
	t2.Do("SET autocommit = 0")
	// The manual's "LOCK TABLES t1 WRITE, t2 READ, ...;" with t and lt2.
	t1.Do("LOCK TABLES t WRITE, lt2 READ")
	w := t2.Do("LOCK TABLES t WRITE, lt2 READ").MustBlock()
	t1.Do("SELECT id FROM lt2").MustRows("(1)")
	t1.Do("DELETE FROM t WHERE id = 10")
	t1.Do("DELETE FROM t WHERE id = 20")
	t1.Do("COMMIT")
	t1.Do("UNLOCK TABLES")
	w.MustOK()
	t2.Do("DELETE FROM t WHERE id = 20").MustAffect(0)
	t2.Do("COMMIT")
	t2.Do("UNLOCK TABLES")
}

// 17.7.6 Transaction Scheduling: CATS grants a released lock to the waiter that blocks the
// most other transactions (TRX_SCHEDULE_WEIGHT), not to the one that has waited longest.
func TestLock_CATSSchedulesHeaviestWaiterFirst(t *testing.T) {
	L := newLab(t)
	setupT(L)
	holder, lone, busy := L.Session("H"), L.Session("LONE"), L.Session("BUSY")
	f1, f2 := L.Session("F1"), L.Session("F2")
	for _, s := range []*lab.Session{holder, lone, busy, f1, f2} {
		s.Do("START TRANSACTION")
	}
	holder.Do("UPDATE t SET v = 1 WHERE id = 10")
	first := lone.Do("UPDATE t SET v = 2 WHERE id = 10").MustBlock()
	busy.Do("UPDATE t SET v = 3 WHERE id = 20")
	second := busy.Do("UPDATE t SET v = 3 WHERE id = 10").MustBlock()
	f1.Do("UPDATE t SET v = 4 WHERE id = 20").MustBlock()
	f2.Do("UPDATE t SET v = 5 WHERE id = 20").MustBlock()
	L.Note("LONE has waited longer, but BUSY blocks two other transactions")

	weight := func(s *lab.Session) int {
		lab.FreshInnoDBTrx()
		var w int
		fmt.Sscan(L.QueryString("SELECT trx_schedule_weight FROM information_schema.innodb_trx WHERE trx_mysql_thread_id = ?", s.ID), &w)
		return w
	}
	deadline := time.Now().Add(5 * time.Second)
	for weight(busy) <= weight(lone) && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	lab.FreshInnoDBTrx()
	L.Show(`SELECT trx_mysql_thread_id, trx_state, trx_schedule_weight FROM information_schema.innodb_trx ORDER BY trx_started`)
	if weight(busy) <= weight(lone) {
		t.Fatalf("BUSY's weight %d is not above LONE's %d", weight(busy), weight(lone))
	}
	if lab.FreshInnoDBTrx(); L.QueryString("SELECT trx_schedule_weight IS NULL FROM information_schema.innodb_trx WHERE trx_mysql_thread_id = ?", holder.ID) != "1" {
		t.Fatal("a transaction that is not waiting should have a NULL weight")
	}
	// "Waiting transactions are those in a LOCK WAIT transaction execution state".
	for s, want := range map[*lab.Session]string{holder: "RUNNING", lone: "LOCK WAIT", busy: "LOCK WAIT", f1: "LOCK WAIT", f2: "LOCK WAIT"} {
		lab.FreshInnoDBTrx()
		if got := L.QueryString("SELECT trx_state FROM information_schema.innodb_trx WHERE trx_mysql_thread_id = ?", s.ID); got != want {
			t.Errorf("%s: trx_state %s, want %s", s.Name, got, want)
		}
	}
	holder.Do("COMMIT")
	second.MustOK()
	L.Settle()
	first.MustBlock()
	for _, s := range []*lab.Session{busy, lone, f1, f2} {
		s.Do("ROLLBACK")
	}
	L.WaitAll()
}

// 17.7.3 LOCK TABLES: it commits the open transaction implicitly, and locking more tables
// later in the transaction implicitly unlocks the earlier ones (it does UNLOCK TABLES first).
func TestTx_LockTablesImplicitCommit(t *testing.T) {
	L := newLab(t)
	setupT(L)
	L.Exec("DROP TABLE IF EXISTS lt2", "CREATE TABLE lt2 (id int PRIMARY KEY)")
	s, other := L.Session("S"), L.Session("O")
	s.Do("START TRANSACTION")
	s.Do("INSERT INTO t VALUES (40, 40, 0)")
	s.Do("LOCK TABLES t READ")
	s.Do("ROLLBACK")
	s.Do("UNLOCK TABLES")
	if n := L.QueryString("SELECT count(*) FROM t WHERE id = 40"); n != "1" {
		t.Fatal("LOCK TABLES did not commit the open transaction: the INSERT was rolled back")
	}

	s.Do("LOCK TABLES t WRITE")
	sel := other.Do("SELECT count(*) FROM t").MustBlock()
	s.Do("LOCK TABLES lt2 READ")
	L.Note("locking lt2 released the lock on t")
	sel.MustOK()
	s.Do("UNLOCK TABLES")
}

// 17.7.3 innodb_table_locks: with autocommit = 0, LOCK TABLES also takes an InnoDB table lock
// unless innodb_table_locks = 0. Two measured differences from the 8.4 manual on 9.7:
//   - the manual says the variable has no effect on LOCK TABLES ... WRITE; on 9.7, 0 drops
//     the InnoDB table lock for WRITE too.
//   - the manual says that with 0, LOCK TABLES "completes even if some records of the
//     tables are being locked by other transactions"; it still waits, on the metadata lock the other transaction holds until it
//     ends, before InnoDB is even asked.
func TestLock_InnoDBTableLocksVariable(t *testing.T) {
	L := newLab(t)
	setupT(L)
	s, other := L.Session("S"), L.Session("O")
	innodbTableLock := func() string {
		for _, k := range L.Locks() {
			if k.Session == "S" && k.Type == "TABLE" {
				return k.Mode
			}
		}
		return ""
	}
	s.Do("LOCK TABLES t WRITE")
	if got := innodbTableLock(); got != "" {
		t.Errorf("autocommit = 1: InnoDB table lock %q, want none (it is released at once)", got)
	}
	s.Do("UNLOCK TABLES")
	s.Do("SET autocommit = 0")
	for _, c := range []struct{ setting, stmt, want string }{
		{"1", "LOCK TABLES t READ", "S"},
		{"0", "LOCK TABLES t READ", ""},
		{"1", "LOCK TABLES t WRITE", "X"},
		{"0", "LOCK TABLES t WRITE", ""},
	} {
		s.Do("SET SESSION innodb_table_locks = " + c.setting)
		s.Do(c.stmt)
		got := innodbTableLock()
		L.Note("innodb_table_locks = %s, %s → InnoDB table lock %q", c.setting, c.stmt, got)
		if got != c.want {
			t.Errorf("innodb_table_locks = %s, %s: InnoDB table lock %q, want %q", c.setting, c.stmt, got, c.want)
		}
		s.Do("COMMIT")
		s.Do("UNLOCK TABLES")
	}

	other.Do("START TRANSACTION")
	other.Do("UPDATE t SET v = 1 WHERE id = 10")
	lock := s.Do("LOCK TABLES t READ").MustBlock()
	state := L.QueryString("SELECT STATE FROM performance_schema.processlist WHERE ID = ?", s.ID)
	L.Note("innodb_table_locks = 0, waiting in state %q", state)
	if state != "Waiting for table metadata lock" {
		t.Fatalf("waiting in state %q, want the metadata lock", state)
	}
	other.Do("COMMIT")
	lock.MustOK()
	s.Do("UNLOCK TABLES")
}

// 17.7.2.1: READ COMMITTED supports only row-based binary logging. With
// binlog_format = STATEMENT an InnoDB write under READ COMMITTED fails (ERROR 1665); with
// MIXED the server logs it as row events, while the same UPDATE under REPEATABLE READ is
// logged as a statement.
func TestTx_ReadCommittedRequiresRowBinlog(t *testing.T) {
	L := newLab(t)
	if L.QueryString("SELECT @@log_bin") != "1" {
		t.Skip("binary logging is off")
	}
	setupT(L)
	s := L.Session("S")
	s.Do("SET SESSION binlog_format = 'STATEMENT'")
	s.Begin(lab.ReadCommitted)
	s.Do("UPDATE t SET v = v + 1 WHERE id = 10").MustFail(func(err error) bool { return lab.Code(err) == "1665" })
	s.Do("ROLLBACK")
	s.Begin(lab.RepeatableRead)
	s.Do("UPDATE t SET v = v + 1 WHERE id = 10").MustOK()
	s.Do("COMMIT")

	s.Do("SET SESSION binlog_format = 'MIXED'")
	for _, c := range []struct {
		level lab.Level
		event string
	}{{lab.RepeatableRead, "Query"}, {lab.ReadCommitted, "Update_rows"}} {
		pos := L.Query("SHOW BINARY LOG STATUS")[0]
		s.Begin(c.level)
		s.Do("UPDATE t SET v = v + 1 WHERE id = 20").MustOK()
		s.Do("COMMIT")
		var types []string
		for _, e := range L.Query("SHOW BINLOG EVENTS IN '" + pos[0] + "' FROM " + pos[1]) {
			if e[2] == "Query" && !strings.Contains(e[5], "UPDATE") {
				continue // BEGIN / COMMIT markers
			}
			if e[2] == "Query" || strings.HasSuffix(e[2], "_rows") {
				types = append(types, e[2])
			}
		}
		L.Note("binlog_format = MIXED, %s: UPDATE logged as %v", c.level, types)
		if !slices.Equal(types, []string{c.event}) {
			t.Errorf("%s: UPDATE logged as %v, want [%s]", c.level, types, c.event)
		}
	}
}

// 17.7.2.1 / 17.7.2.4: DML that only reads the grant tables in mysql takes no locks on them,
// at any isolation level (even where reads of ordinary tables lock, as in SERIALIZABLE or
// INSERT ... SELECT).
func TestLock_GrantTablesAreReadWithoutLocks(t *testing.T) {
	L := newLab(t)
	setupT(L)
	L.Exec("DROP TABLE IF EXISTS dst", "CREATE TABLE dst (u varchar(64))")
	s := L.Session("S")
	check := func(what string, wantT bool) {
		t.Helper()
		var onUser, onT int
		for _, k := range L.ShowLocks() {
			switch table, _, _ := strings.Cut(k.Object, "."); table {
			case "user":
				onUser++
			case "t":
				onT++
			}
		}
		L.Note("%s: %d lock(s) on t, %d on mysql.user", what, onT, onUser)
		if onUser != 0 {
			t.Errorf("%s: %d lock(s) on mysql.user", what, onUser)
		}
		if wantT && onT == 0 {
			t.Errorf("%s: no locks on t; the control should lock it", what)
		}
	}
	s.Begin(lab.Serializable)
	s.Do("SELECT count(*) FROM t WHERE EXISTS (SELECT 1 FROM mysql.user WHERE user = 'root')").MustOK()
	check("SERIALIZABLE SELECT joining mysql.user", true)
	s.Do("ROLLBACK")
	s.Begin(lab.RepeatableRead)
	s.Do("INSERT INTO dst SELECT user FROM mysql.user").MustOK()
	check("INSERT ... SELECT FROM mysql.user", false)
	s.Do("ROLLBACK")

	L.Note(`17.7.2.4: "SELECT ... FOR SHARE statements do not acquire read locks on MySQL grant tables"`)
	s.Begin(lab.RepeatableRead)
	s.Do("SELECT user FROM mysql.user FOR SHARE").MustOK()
	check("SELECT ... FROM mysql.user FOR SHARE", false)
	s.Do("SELECT id FROM t FOR SHARE").MustOK()
	check("control: SELECT ... FROM t FOR SHARE", true)
	s.Do("ROLLBACK")
	L.Note("control for the subquery position: an ordinary table read there under SERIALIZABLE is locked")
	L.Exec("INSERT INTO dst VALUES ('x')")
	s.Begin(lab.Serializable)
	s.Do("SELECT count(*) FROM mysql.db WHERE EXISTS (SELECT 1 FROM t WHERE id = 10)").MustOK()
	check("SERIALIZABLE SELECT FROM mysql.db WHERE EXISTS (… FROM t)", true)
	s.Do("ROLLBACK")
}

// 17.7.2.4: FOR SHARE needs the SELECT privilege; FOR UPDATE needs SELECT plus at least one
// of DELETE, LOCK TABLES or UPDATE.
func TestLock_LockingReadPrivileges(t *testing.T) {
	L := newLab(t)
	setupT(L)
	L.Exec("DROP USER IF EXISTS labro", "CREATE USER labro IDENTIFIED BY 'ro'",
		"GRANT SELECT ON "+dbname+".t TO labro")
	t.Cleanup(func() { _, _ = L.Admin.Exec("DROP USER IF EXISTS labro") })
	db, err := sql.Open("mysql", lab.DSNAs(lab.MySQL, dbname, "labro", "ro"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Database-level grants (LOCK TABLES ON db.*) only apply to new connections, so never reuse one.
	db.SetMaxIdleConns(0)
	try := func(q string) error {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		rows, err := tx.Query(q)
		if err == nil {
			rows.Close()
		}
		return err
	}
	if err := try("SELECT * FROM t WHERE id = 10 FOR SHARE"); err != nil {
		t.Fatalf("FOR SHARE with SELECT only: %v", err)
	}
	err = try("SELECT * FROM t WHERE id = 10 FOR UPDATE")
	L.Note("FOR UPDATE with SELECT only: %v", lab.Describe(err))
	if lab.Code(err) != "1142" {
		t.Fatalf("FOR UPDATE with SELECT only: got %v, want ERROR 1142", err)
	}
	for _, grant := range []string{"UPDATE ON " + dbname + ".t", "DELETE ON " + dbname + ".t", "LOCK TABLES ON " + dbname + ".*"} {
		L.Exec("REVOKE ALL PRIVILEGES, GRANT OPTION FROM labro", "GRANT SELECT ON "+dbname+".t TO labro", "GRANT "+grant+" TO labro")
		if err := try("SELECT * FROM t WHERE id = 10 FOR UPDATE"); err != nil {
			t.Errorf("FOR UPDATE with SELECT + %s: %v", grant, err)
		} else {
			L.Note("FOR UPDATE with SELECT + %s: ok", strings.Fields(grant)[0])
		}
	}
}

// 17.7.1: the lock descriptions the manual quotes from SHOW ENGINE INNODB STATUS (with
// innodb_status_output_locks = ON), on the manual's child table (90, 102).
func TestLock_MonitorOutputDocsExamples(t *testing.T) {
	L := newLab(t)
	setGlobal(t, L, "innodb_status_output_locks", "ON")
	L.Exec("DROP TABLE IF EXISTS child", "CREATE TABLE child (id int NOT NULL, PRIMARY KEY (id))",
		"INSERT INTO child (id) VALUES (90), (102)")
	a, b := L.Session("A"), L.Session("B")
	a.Do("START TRANSACTION")
	a.Do("SELECT * FROM child WHERE id = 90 FOR UPDATE")
	a.Do("SELECT * FROM child WHERE id > 100 FOR UPDATE")
	b.Do("START TRANSACTION")
	ins := b.Do("INSERT INTO child (id) VALUES (101)").MustBlock()
	status := L.Query("SHOW ENGINE INNODB STATUS")[0][2]
	for what, want := range map[string]string{
		"intention lock":      "TABLE LOCK table `" + dbname + "`.`child` trx id",
		"intention lock mode": "lock mode IX",
		"record lock":         "lock_mode X locks rec but not gap",
		// 17.7.1 prints index `PRIMARY`; 9.7 drops the backquotes around the index name, as
		// the manual's own (newer) dump in 17.7.5.1 does
		"record lock header":    "index PRIMARY of table `" + dbname + "`.`child`",
		"next-key lock":         "lock_mode X\n",
		"supremum":              "supremum",
		"insert intention lock": "lock_mode X locks gap before rec insert intention waiting",
	} {
		if !strings.Contains(status, want) {
			t.Errorf("%s: monitor output does not contain %q", what, want)
		}
	}
	if t.Failed() {
		if i := strings.Index(status, "\nTRANSACTIONS\n"); i >= 0 {
			t.Log(status[i:min(len(status), i+6000)])
		}
	}
	a.Do("ROLLBACK")
	ins.MustOK()
	b.Do("ROLLBACK")
}

// 17.7.2: lock information is stored compactly, so InnoDB never escalates row locks to a
// table lock. A transaction that locks every one of 10,000 rows still holds 10,000 record
// locks and only an intention lock on the table, and others can keep inserting.
func TestLock_NoLockEscalation(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS big", "DROP TABLE IF EXISTS dg", "CREATE TABLE dg (n int)",
		"INSERT INTO dg VALUES (0),(1),(2),(3),(4),(5),(6),(7),(8),(9)",
		"CREATE TABLE big (id int PRIMARY KEY, v int)",
		"INSERT INTO big SELECT a.n + 10 * b.n + 100 * c.n + 1000 * d.n + 1, 0 FROM dg a, dg b, dg c, dg d")
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Begin(lab.ReadCommitted)
	t1.Do("UPDATE big SET v = 1").MustAffect(10000)
	got := L.Show(`SELECT LOCK_TYPE, LOCK_MODE, count(*) FROM performance_schema.data_locks
WHERE OBJECT_SCHEMA = DATABASE() AND OBJECT_NAME = 'big' GROUP BY 1, 2 ORDER BY 1, 2`)
	if lab.FormatRows(got) != "(RECORD,X,REC_NOT_GAP,10000) (TABLE,IX,1)" {
		t.Fatalf("locks on big: %s, want 10000 record locks and one IX", lab.FormatRows(got))
	}
	t2.Do("INSERT INTO big VALUES (10001, 0)").MustNotBlock().MustOK()
	t1.Do("ROLLBACK")
}

// 17.7.2.1: REPEATABLE READ is the default level. SET SESSION TRANSACTION changes it for
// this session only; SET GLOBAL changes it for connections made afterwards.
// The global part runs on connections of its own, and nothing else touches the server while
// the global level is changed, so that no pooled connection starts life at SERIALIZABLE.
func TestTx_IsolationLevelDefaultAndScope(t *testing.T) {
	L := newLab(t)
	if g := L.QueryString("SELECT @@GLOBAL.transaction_isolation"); g != "REPEATABLE-READ" {
		t.Skipf("the server was started with transaction_isolation = %s", g)
	}
	s, other := L.Session("S"), L.Session("O")
	s.Do("SELECT @@transaction_isolation").MustRows("(REPEATABLE-READ)")
	s.Do("SET SESSION TRANSACTION ISOLATION LEVEL READ COMMITTED")
	s.Do("SELECT @@transaction_isolation").MustRows("(READ-COMMITTED)")
	other.Do("SELECT @@transaction_isolation").MustRows("(REPEATABLE-READ)")

	db, err := sql.Open("mysql", lab.DSN(lab.MySQL, dbname))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := t.Context()
	level := func(c *sql.Conn) string {
		t.Helper()
		var v string
		if err := c.QueryRowContext(ctx, "SELECT @@transaction_isolation").Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	old, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if _, err := old.ExecContext(ctx, "SET GLOBAL transaction_isolation = 'SERIALIZABLE'"); err != nil {
		t.Fatal(err)
	}
	defer old.ExecContext(context.Background(), "SET GLOBAL transaction_isolation = 'REPEATABLE-READ'")
	fresh, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	got := [2]string{level(old), level(fresh)}
	if _, err := old.ExecContext(ctx, "SET GLOBAL transaction_isolation = 'REPEATABLE-READ'"); err != nil {
		t.Fatal(err)
	}
	L.Note("after SET GLOBAL ... SERIALIZABLE: the existing connection stays at %s, a new one starts at %s", got[0], got[1])
	if got != [2]string{"REPEATABLE-READ", "SERIALIZABLE"} {
		t.Fatalf("existing / new connection at %v, want REPEATABLE-READ / SERIALIZABLE", got)
	}
}

// 17.7.2.1 READ COMMITTED: DELETE also keeps locks only on the rows it deletes, but the
// semi-consistent read is for UPDATE only: a second DELETE waits at the first locked row
// where an UPDATE would have skipped it.
func TestLock_RC_DeleteReleasesNonMatchingRowsButWaits(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS sc", "CREATE TABLE sc (a INT NOT NULL, b INT) ENGINE = InnoDB",
		"INSERT INTO sc VALUES (1,2),(2,3),(3,2),(4,3),(5,2)")
	a, b := L.Session("A"), L.Session("B")
	a.Begin(lab.ReadCommitted)
	a.Do("DELETE FROM sc WHERE b = 3").MustAffect(2)
	if got := recordLocks(L.ShowLocks(), "A"); len(got) != 2 {
		t.Fatalf("A holds %d record locks %v, want 2", len(got), got)
	}
	b.Begin(lab.ReadCommitted)
	del := b.Do("DELETE FROM sc WHERE b = 2").MustBlock()
	a.Do("COMMIT")
	del.MustAffect(3)
	b.Do("COMMIT")
}

// 17.7.2.3: rows deleted, updated or inserted by transactions that commit after the
// snapshot's timepoint look unchanged to the snapshot.
func TestMVCC_SnapshotIgnoresLaterDeleteUpdateInsert(t *testing.T) {
	L := newLab(t)
	setupT(L)
	a, b := L.Session("A"), L.Session("B")
	a.Do("START TRANSACTION")
	a.Do("SELECT id, v FROM t ORDER BY id").MustRows("(10,0) (20,0) (30,0)")
	b.Do("DELETE FROM t WHERE id = 10")
	b.Do("UPDATE t SET v = 9 WHERE id = 20")
	b.Do("INSERT INTO t VALUES (40, 40, 0)")
	a.Do("SELECT id, v FROM t ORDER BY id").MustRows("(10,0) (20,0) (30,0)")
	a.Do("COMMIT")
	a.Do("SELECT id, v FROM t ORDER BY id").MustRows("(20,9) (30,0) (40,0)")
}

// 17.7.2.3 / 17.7.3: CREATE TABLE ... SELECT reads its source like INSERT ... SELECT: with
// shared locks in REPEATABLE READ (so it waits for a writer), as a consistent read in READ
// COMMITTED.
func TestLock_CreateTableSelectLocksSource(t *testing.T) {
	for _, level := range []lab.Level{lab.RepeatableRead, lab.ReadCommitted} {
		t.Run(level.Short(), func(t *testing.T) {
			L := newLab(t)
			setupT(L)
			L.Exec("DROP TABLE IF EXISTS ctas")
			w, c := L.Session("W"), L.Session("C")
			w.Do("START TRANSACTION")
			w.Do("UPDATE t SET v = 1 WHERE id = 10")
			c.Do("SET SESSION TRANSACTION ISOLATION LEVEL " + string(level))
			op := c.Do("CREATE TABLE ctas AS SELECT id, v FROM t")
			if level == lab.RepeatableRead {
				op.MustBlock()
				mustHaveLock(t, L.ShowLocks(), "C", "S", "10")
				w.Do("COMMIT")
				op.MustOK()
				c.Do("SELECT v FROM ctas WHERE id = 10").MustRows("(1)")
			} else {
				op.MustNotBlock().MustOK()
				c.Do("SELECT v FROM ctas WHERE id = 10").MustRows("(0)")
				w.Do("COMMIT")
			}
		})
	}
}

// 17.7.3 INSERT / UPDATE: the lock on a freshly inserted row, and on the secondary index
// entries an UPDATE touches, is implicit: nothing is listed in data_locks until another
// transaction runs into it, and then it shows up as an explicit record lock. The inserted
// row's lock is a record lock without a gap, so the gap before it stays open.
func TestLock_InsertAndUpdateImplicitLocks(t *testing.T) {
	L := newLab(t)
	setupT(L)
	L.Exec("DROP TABLE IF EXISTS tu", "CREATE TABLE tu (id int PRIMARY KEY, u int, v int, UNIQUE KEY uu (u))",
		"INSERT INTO tu VALUES (1, 10, 0), (2, 20, 0), (3, 30, 0)")
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("START TRANSACTION")
	t1.Do("INSERT INTO t VALUES (25, 25, 0)")
	if got := recordLocks(L.ShowLocks(), "T1"); len(got) != 0 {
		t.Fatalf("explicit record locks after INSERT: %v, want none", got)
	}
	t2.Do("START TRANSACTION")
	t2.Do("INSERT INTO t VALUES (24, 24, 0)").MustNotBlock().MustOK()
	rd := t2.Do("SELECT id FROM t WHERE id = 25 FOR SHARE").MustBlock()
	mustHaveLock(t, L.ShowLocks(), "T1", "X,REC_NOT_GAP", "25")
	t1.Do("COMMIT")
	rd.MustRows("(25)")
	t2.Do("ROLLBACK")

	L.Note("INSERT INTO T SELECT ...: the same record lock without a gap on each row inserted into T")
	L.Exec("DROP TABLE IF EXISTS ins", "CREATE TABLE ins (id int PRIMARY KEY)", "INSERT INTO ins VALUES (5)")
	t1.Do("START TRANSACTION")
	t1.Do("INSERT INTO ins SELECT id FROM t WHERE id IN (10, 20)").MustAffect(2)
	t2.Do("START TRANSACTION")
	t2.Do("INSERT INTO ins VALUES (15)").MustNotBlock().MustOK()
	rd = t2.Do("SELECT id FROM ins WHERE id = 20 FOR SHARE").MustBlock()
	mustHaveLock(t, L.ShowLocks(), "T1", "X,REC_NOT_GAP", "20")
	t1.Do("COMMIT")
	rd.MustRows("(20)")
	t2.Do("ROLLBACK")

	t1.Do("START TRANSACTION")
	t1.Do("UPDATE tu SET u = 25 WHERE id = 2")
	if got, want := recordLocks(L.ShowLocks(), "T1"), []string{"X,REC_NOT_GAP 2"}; !slices.Equal(got, want) {
		t.Fatalf("explicit record locks after UPDATE: %v, want %v (only the clustered record)", got, want)
	}
	t2.Do("START TRANSACTION")
	rd = t2.Do("SELECT id FROM tu FORCE INDEX (uu) WHERE u = 25 FOR SHARE").MustBlock()
	mustHaveLock(t, L.ShowLocks(), "T1", "X,REC_NOT_GAP", "25, 2")
	t1.Do("COMMIT")
	rd.MustRows("(2)")
	t2.Do("ROLLBACK")
}

// 17.7.5: "The possibility of deadlocks is not affected by the isolation level, because the
// isolation level changes the behavior of read operations, while deadlocks occur because of
// write operations." That holds for this write-write cycle: two transactions that update the
// same two rows in opposite order deadlock at every level. It is not true in general: a
// deadlock that goes through gap locks or through the S locks of SERIALIZABLE reads depends
// on the level (TestDeadlock/CheckThenInsert_GapLocks vs CheckThenInsert_ReadCommitted,
// Serializable_ReadThenUpdate vs RepeatableRead_ReadThenUpdate), and 17.7.5.3 itself advises
// "a lower isolation level such as READ COMMITTED".
func TestDeadlock_AtEveryIsolationLevel(t *testing.T) {
	for _, level := range lab.AllLevels {
		t.Run(level.Short(), func(t *testing.T) {
			L := newLab(t)
			setupT(L)
			t1, t2 := L.Session("T1"), L.Session("T2")
			t1.Begin(level)
			t2.Begin(level)
			t1.Do("UPDATE t SET v = 1 WHERE id = 10")
			t2.Do("UPDATE t SET v = 2 WHERE id = 20")
			a := t1.Do("UPDATE t SET v = 1 WHERE id = 20")
			b := t2.Do("UPDATE t SET v = 2 WHERE id = 10")
			lab.ExpectOneDeadlockVictim(t, a, b)
			t1.Do("ROLLBACK")
			t2.Do("ROLLBACK")
		})
	}
}

// 17.7.5.2: InnoDB cannot detect a deadlock that involves a lock set by another storage
// engine. T2 holds MyISAM's table lock on my and waits for T1's InnoDB row lock; T1 waits
// for the MyISAM table lock. Nothing reports a deadlock: the cycle ends only when T2's
// innodb_lock_wait_timeout expires.
func TestDeadlock_UndetectedAcrossStorageEngines(t *testing.T) {
	L := newLab(t)
	setupT(L)
	L.Exec("DROP TABLE IF EXISTS my", "CREATE TABLE my (id int) ENGINE = MyISAM", "INSERT INTO my VALUES (10)")
	before := innodbDeadlocks(L)
	t1, t2 := L.Session("T1"), L.Session("T2")
	t2.Do("SET SESSION innodb_lock_wait_timeout = 2")
	t1.Do("START TRANSACTION")
	t1.Do("UPDATE t SET v = 1 WHERE id = 10")
	sel := t2.Do("UPDATE my SET id = id + 1 WHERE id IN (SELECT id FROM t WHERE id = 10)").MustBlock()
	rd := t1.Do("SELECT id FROM my").MustBlock()
	L.Show("SELECT ID, STATE, INFO FROM performance_schema.processlist WHERE ID IN (?, ?) ORDER BY ID", t1.ID, t2.ID)
	sel.MustFail(func(err error) bool { return lab.Code(err) == "1205" })
	rd.MustRows("(10)")
	t1.Do("ROLLBACK")
	if after := innodbDeadlocks(L); after != before {
		t.Fatalf("lock_deadlocks went from %d to %d: InnoDB should not have seen this cycle", before, after)
	}
}

// 17.7.6: among waiters of equal weight the one that has waited longest gets the lock.
// The INNODB_METRICS counters the section lists count the scheduler's work.
func TestLock_CATSEqualWeightIsFIFO(t *testing.T) {
	L := newLab(t)
	setupT(L)
	counters := []string{"lock_rec_release_attempts", "lock_rec_grant_attempts", "lock_schedule_refreshes"}
	for _, c := range counters {
		if L.QueryString("SELECT status FROM information_schema.INNODB_METRICS WHERE NAME = ?", c) != "enabled" {
			L.Exec("SET GLOBAL innodb_monitor_enable = '" + c + "'")
			t.Cleanup(func() { _, _ = L.Admin.Exec("SET GLOBAL innodb_monitor_disable = '" + c + "'") })
		}
	}
	read := func() map[string]int {
		m := map[string]int{}
		for _, r := range L.Query("SELECT NAME, COUNT FROM information_schema.INNODB_METRICS WHERE NAME LIKE 'lock_rec_%_attempts' OR NAME = 'lock_schedule_refreshes'") {
			var n int
			fmt.Sscan(r[1], &n)
			m[r[0]] = n
		}
		return m
	}
	before := read()
	holder, w1, w2 := L.Session("H"), L.Session("W1"), L.Session("W2")
	for _, s := range []*lab.Session{holder, w1, w2} {
		s.Do("START TRANSACTION")
	}
	holder.Do("UPDATE t SET v = 1 WHERE id = 10")
	first := w1.Do("UPDATE t SET v = 2 WHERE id = 10").MustBlock()
	second := w2.Do("UPDATE t SET v = 3 WHERE id = 10").MustBlock()
	holder.Do("COMMIT")
	first.MustOK()
	L.Settle()
	second.MustBlock()
	w1.Do("COMMIT")
	second.MustOK()
	w2.Do("COMMIT")
	after := read()
	L.Note("scheduler counters: before %v, after %v", before, after)
	if len(after) != len(counters) {
		t.Fatalf("INNODB_METRICS has %d of the 3 scheduling counters: %v", len(after), after)
	}
	for _, c := range []string{"lock_rec_release_attempts", "lock_rec_grant_attempts"} {
		if after[c] <= before[c] {
			t.Errorf("%s did not increase: %d → %d", c, before[c], after[c])
		}
	}
}

// 17.7.3 / 17.7.5.2 / 17.7.5.3: deadlocks that involve a LOCK TABLES table lock. The manual:
// InnoDB is aware of such table locks only with innodb_table_locks = 1 and autocommit = 0;
// "otherwise" it cannot detect the deadlock, and "with MySQL table-level locks, the timeout
// method must be used". On 9.7 this cycle never reaches InnoDB in either mode
// (lock_deadlocks stays put, no InnoDB table lock is involved): LOCK TABLES waits on the
// metadata lock that T1's open transaction holds, and the metadata lock subsystem has a
// deadlock detector of its own. T1 fails at once with the same ERROR 1213 and is rolled
// back; nothing waits for a timeout.
// (LOCK TABLES takes its metadata locks in name order: it holds aa while it waits for t.)
func TestDeadlock_LockTablesDetectedByMetadataLocks(t *testing.T) {
	for _, autocommit := range []string{"0", "1"} {
		t.Run("autocommit="+autocommit, func(t *testing.T) {
			L := newLab(t)
			setupT(L)
			L.Exec("DROP TABLE IF EXISTS aa", "CREATE TABLE aa (id int PRIMARY KEY, v int)", "INSERT INTO aa VALUES (1, 0)")
			before := innodbDeadlocks(L)
			t1, t2 := L.Session("T1"), L.Session("T2")
			t2.Do("SET autocommit = " + autocommit)
			t1.Do("START TRANSACTION")
			t1.Do("UPDATE t SET v = 1 WHERE id = 10")
			lock := t2.Do("LOCK TABLES aa WRITE, t WRITE").MustBlock()
			L.ShowMetadataLocks()
			if state := L.QueryString("SELECT STATE FROM performance_schema.processlist WHERE ID = ?", t2.ID); state != "Waiting for table metadata lock" {
				t.Fatalf("LOCK TABLES waits in state %q, want the metadata lock", state)
			}
			start := time.Now()
			dl := t1.Do("UPDATE aa SET v = 1 WHERE id = 1").MustFail(lab.IsDeadlock)
			if waited := time.Since(start); waited > 2*time.Second {
				t.Fatalf("the deadlock error took %v: that is a timeout, not detection", waited)
			}
			L.Note("%s", lab.Describe(dl.Err))
			lock.MustOK()
			L.Note("T1 was rolled back as a whole: its UPDATE of t is gone")
			t2.Do("SELECT v FROM t WHERE id = 10").MustRows("(0)")
			t2.Do("UNLOCK TABLES")
			if after := innodbDeadlocks(L); after != before {
				t.Fatalf("lock_deadlocks went from %d to %d: this deadlock is not InnoDB's to detect", before, after)
			}
		})
	}
}

// 17.7.1 Gap Locks: "The reason conflicting gap locks are allowed is that if a record is
// purged from an index, the gap locks held on the record by different transactions must be
// merged." T1 (S) and T2 (X) both lock the gap before 20; once 20 is deleted and purged,
// both gap locks are inherited by the next record, 30, and now cover (10,30).
func TestLock_GapLocksMergedWhenRecordPurged(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS gp", "CREATE TABLE gp (id int PRIMARY KEY)", "INSERT INTO gp VALUES (10), (20), (30)")
	t1, t2, t3 := L.Session("T1"), L.Session("T2"), L.Session("T3")
	t1.Do("START TRANSACTION")
	t1.Do("SELECT * FROM gp WHERE id = 15 FOR SHARE").MustRows("(no rows)")
	t2.Do("START TRANSACTION")
	t2.Do("SELECT * FROM gp WHERE id = 15 FOR UPDATE").MustNotBlock().MustRows("(no rows)")
	locks := L.ShowLocks()
	mustHaveLock(t, locks, "T1", "S,GAP", "20")
	mustHaveLock(t, locks, "T2", "X,GAP", "20")
	L.Note("record locks and gap locks do not conflict: 20 itself can be deleted")
	t3.Do("DELETE FROM gp WHERE id = 20").MustNotBlock().MustAffect(1)

	deadline := time.Now().Add(20 * time.Second)
	for {
		locks = L.Locks()
		_, s1 := lab.FindLock(locks, "T1", "S,GAP", "30")
		_, s2 := lab.FindLock(locks, "T2", "X,GAP", "30")
		if s1 && s2 {
			break
		}
		if time.Now().After(deadline) {
			L.ShowLocks()
			t.Fatalf("the gap locks were not moved to 30 after 20 was purged")
		}
		// wake the purge coordinator (see TestMVCC_LongReadViewBlocksPurge)
		L.Exec("UPDATE gp SET id = id WHERE id = 10")
		time.Sleep(200 * time.Millisecond)
	}
	L.Note("20 was purged: both gap locks now sit on 30, i.e. cover (10,30)")
	L.ShowLocks()
	ins := t3.Do("INSERT INTO gp VALUES (25)").MustBlock()
	t1.Do("COMMIT")
	L.Settle()
	ins.MustBlock()
	t2.Do("COMMIT")
	ins.MustOK()
}

// 17.7.5.3: "When modifying multiple tables within a transaction, or different sets of rows
// in the same table, do those operations in a consistent order each time." The two opposite-order
// transfers of TestDeadlock_AtEveryIsolationLevel, each first locking both rows in id order
// (ORDER BY id FOR UPDATE): the second queues behind the first instead of deadlocking.
func TestDeadlock_AvoidedByLockOrdering(t *testing.T) {
	L := newLab(t)
	setupT(L)
	t1, t2 := L.Session("T1"), L.Session("T2")
	before := innodbDeadlocks(L)
	t1.Do("START TRANSACTION")
	t2.Do("START TRANSACTION")
	t1.Do("SELECT id FROM t WHERE id IN (10, 20) ORDER BY id FOR UPDATE").MustRows("(10) (20)")
	w := t2.Do("SELECT id FROM t WHERE id IN (10, 20) ORDER BY id FOR UPDATE").MustBlock()
	t1.Do("UPDATE t SET v = v - 1 WHERE id = 10").MustAffect(1)
	t1.Do("UPDATE t SET v = v + 1 WHERE id = 20").MustAffect(1)
	t1.Do("COMMIT")
	w.MustRows("(10) (20)")
	t2.Do("UPDATE t SET v = v - 1 WHERE id = 20").MustAffect(1)
	t2.Do("UPDATE t SET v = v + 1 WHERE id = 10").MustAffect(1)
	t2.Do("COMMIT").MustOK()
	if n := innodbDeadlocks(L); n != before {
		t.Errorf("lock_deadlocks moved by %d, want 0", n-before)
	}
}

// 17.7.5 / 17.7.5.3: "Add well-chosen indexes to your tables so that your queries scan fewer
// index records and set fewer locks." Without an index on c, each "UPDATE ... WHERE c = ?"
// scans (and locks) the whole table and runs into the row the other transaction already
// changed: a deadlock. With the index each touches only its own row.
func TestDeadlock_AvoidedByIndex(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		name := "no-index"
		if indexed {
			name = "indexed"
		}
		t.Run(name, func(t *testing.T) {
			L := newLab(t)
			L.Exec("DROP TABLE IF EXISTS x", "CREATE TABLE x (id int PRIMARY KEY, c int, v int)",
				"INSERT INTO x VALUES (10, 10, 0), (20, 20, 0), (30, 30, 0), (40, 40, 0)")
			if indexed {
				L.Exec("ALTER TABLE x ADD KEY (c)")
			}
			before := innodbDeadlocks(L)
			t1, t2 := L.Session("T1"), L.Session("T2")
			t1.Do("START TRANSACTION")
			t2.Do("START TRANSACTION")
			t1.Do("UPDATE x SET v = 1 WHERE id = 10").MustAffect(1)
			t2.Do("UPDATE x SET v = 2 WHERE id = 40").MustAffect(1)
			a := t1.Do("UPDATE x SET v = 1 WHERE c = 20")
			if !indexed {
				a.MustBlock() // its scan reaches id = 40, which T2 changed
			}
			b := t2.Do("UPDATE x SET v = 2 WHERE c = 30")
			if indexed {
				a.MustNotBlock().MustAffect(1)
				b.MustNotBlock().MustAffect(1)
			} else {
				lab.ExpectOneDeadlockVictim(t, a, b)
			}
			t1.Do("ROLLBACK")
			t2.Do("ROLLBACK")
			if got, want := innodbDeadlocks(L)-before, map[bool]int{false: 1, true: 0}[indexed]; got != want {
				t.Errorf("lock_deadlocks moved by %d, want %d", got, want)
			}
		})
	}
}

// 17.7.1 Predicate Locks: they exist "to support REPEATABLE READ or SERIALIZABLE". The same
// locking read in READ COMMITTED takes no predicate lock, and an insert into the MBR goes in.
func TestLock_SpatialNoPredicateLockInReadCommitted(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS geo",
		"CREATE TABLE geo (id int PRIMARY KEY, g POINT NOT NULL SRID 0, SPATIAL INDEX (g))",
		"INSERT INTO geo VALUES (1, ST_GeomFromText('POINT(1 1)')), (2, ST_GeomFromText('POINT(100 100)'))")
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Begin(lab.ReadCommitted)
	t1.Do("SELECT id FROM geo FORCE INDEX (g) WHERE MBRContains(ST_GeomFromText('POLYGON((0 0,0 5,5 5,5 0,0 0))'), g) FOR UPDATE").MustRows("(1)")
	if locks := L.ShowLocks(); slices.ContainsFunc(locks, func(k lab.Lock) bool { return strings.Contains(k.Mode, "PREDICATE") }) {
		t.Fatalf("READ COMMITTED took a predicate lock: %v", locks)
	}
	t2.Do("INSERT INTO geo VALUES (3, ST_GeomFromText('POINT(2 2)'))").MustNotBlock().MustOK()
	t1.Do("ROLLBACK")
}

// 17.7.1 Gap Locks: "SELECT c1 FROM t WHERE c1 BETWEEN 10 and 20 FOR UPDATE; prevents other
// transactions from inserting a value of 15 into column t.c1, whether or not there was
// already any such value in the column, because the gaps between all existing values in the
// range are locked." (The manual's table t is called c1n here.) Both ways: c1 = 15 present
// (non-unique index) and absent (unique index).
// The next-key lock on 10 also covers the gap below the range, (5, 10): 7 waits too. Only
// below 5 is free.
func TestLock_GapLockBetweenDocsExample(t *testing.T) {
	for _, c := range []struct{ name, ddl, rows string }{
		{"15-exists", "CREATE TABLE c1n (id int AUTO_INCREMENT PRIMARY KEY, c1 int, KEY (c1))", "(5), (10), (15), (20), (30)"},
		{"15-missing", "CREATE TABLE c1n (id int AUTO_INCREMENT PRIMARY KEY, c1 int, UNIQUE KEY (c1))", "(5), (10), (20), (30)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			L := newLab(t)
			L.Exec("DROP TABLE IF EXISTS c1n", c.ddl, "INSERT INTO c1n (c1) VALUES "+c.rows)
			t1, t2, t3 := L.Session("T1"), L.Session("T2"), L.Session("T3")
			t1.Do("START TRANSACTION")
			t1.Do("SELECT c1 FROM c1n WHERE c1 BETWEEN 10 and 20 FOR UPDATE").MustOK()
			L.ShowLocks()
			ins := t2.Do("INSERT INTO c1n (c1) VALUES (15)").MustBlock()
			other := t3.Do("INSERT INTO c1n (c1) VALUES (12)").MustBlock()
			below := L.Session("T4").Do("INSERT INTO c1n (c1) VALUES (7)").MustBlock()
			L.Session("T5").Do("INSERT INTO c1n (c1) VALUES (3)").MustNotBlock().MustOK()
			t1.Do("ROLLBACK")
			L.WaitAll()
			ins.MustOK()
			other.MustOK()
			below.MustOK()
		})
	}
}

// 17.7.1 Record Locks: "SELECT c1 FROM t WHERE c1 = 10 FOR UPDATE; prevents any other
// transaction from inserting, updating, or deleting rows where the value of t.c1 is 10."
// (The manual's table t is called c1r here.)
func TestLock_RecordLockBlocksInsertUpdateDelete(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS c1r", "CREATE TABLE c1r (c1 int PRIMARY KEY, v int)", "INSERT INTO c1r VALUES (10, 0)")
	t1 := L.Session("T1")
	t1.Do("START TRANSACTION")
	t1.Do("SELECT c1 FROM c1r WHERE c1 = 10 FOR UPDATE").MustRows("(10)")
	upd := L.Session("UPD").Do("UPDATE c1r SET v = 1 WHERE c1 = 10").MustBlock()
	del := L.Session("DEL").Do("DELETE FROM c1r WHERE c1 = 10").MustBlock()
	ins := L.Session("INS").Do("INSERT INTO c1r VALUES (10, 2)").MustBlock()
	L.ShowLocks()
	t1.Do("ROLLBACK")
	L.WaitAll()
	upd.MustOK()
	del.MustOK()
	if ins.Err != nil && !lab.IsUniqueViolation(ins.Err) {
		t.Fatalf("the INSERT failed with %s", lab.Describe(ins.Err))
	}
}

// 17.7.3: "If a secondary index is used in a search and the index record locks to be set are
// exclusive, InnoDB also retrieves the corresponding clustered index records and sets locks
// on them." Only if they are exclusive: a FOR SHARE read that the secondary index covers
// locks idx_k alone, and an UPDATE of the row by primary key goes through. After FOR UPDATE
// the same UPDATE waits (TestLock_NonUniqueSecondaryIndex shows the clustered record lock).
func TestLock_SecondaryIndexShareLockSkipsClusteredRecord(t *testing.T) {
	for _, c := range []struct {
		mode   string
		locks  []string
		blocks bool
	}{
		{"FOR SHARE", []string{"S 20, 20", "S,GAP 30, 30"}, false},
		{"FOR UPDATE", []string{"X 20, 20", "X,GAP 30, 30", "X,REC_NOT_GAP 20"}, true},
	} {
		t.Run(strings.ReplaceAll(c.mode, " ", "-"), func(t *testing.T) {
			L := newLab(t)
			setupT(L)
			t1, t2 := L.Session("T1"), L.Session("T2")
			t1.Do("START TRANSACTION")
			t1.Do("SELECT id FROM t WHERE k = 20 " + c.mode).MustRows("(20)")
			if got := recordLocks(L.ShowLocks(), "T1"); !slices.Equal(got, c.locks) {
				t.Fatalf("locks %v, want %v", got, c.locks)
			}
			upd := t2.Do("UPDATE t SET v = 1 WHERE id = 20")
			if c.blocks {
				upd.MustBlock()
			} else {
				upd.MustNotBlock()
			}
			t1.Do("ROLLBACK")
			upd.MustAffect(1)
		})
	}
}

// 17.7.3 FOREIGN KEY: "any insert, update, or delete that requires the constraint condition
// to be checked sets shared record-level locks on the records that it looks at to check the
// constraint. InnoDB also sets these locks in the case where the constraint fails."
// fkc holds (1, 20), a child of t's row 20. Each statement must leave at least these locks.
func TestLock_ForeignKeyCheckLocks(t *testing.T) {
	for _, c := range []struct {
		name, stmt, fails string
		want              []string
	}{
		{"parent-delete-fails", "DELETE FROM t WHERE id = 20", "1451", []string{"fkc.pid S 20, 1", "t.PRIMARY X,REC_NOT_GAP 20"}},
		{"parent-key-update-fails", "UPDATE t SET id = 21 WHERE id = 20", "1451", []string{"fkc.pid S 20, 1"}},
		{"child-update", "UPDATE fkc SET pid = 30 WHERE id = 1", "", []string{"t.PRIMARY S,REC_NOT_GAP 30"}},
		{"child-update-fails", "UPDATE fkc SET pid = 25 WHERE id = 1", "1452", []string{"t.PRIMARY S,GAP 30"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			L := newLab(t)
			setupT(L)
			L.Exec("CREATE TABLE fkc (id int PRIMARY KEY, pid int, FOREIGN KEY (pid) REFERENCES t (id))", "INSERT INTO fkc VALUES (1, 20)")
			s := L.Session("T1")
			s.Do("START TRANSACTION")
			if op := s.Do(c.stmt); c.fails != "" {
				op.MustFail(func(err error) bool { return lab.Code(err) == c.fails })
			} else {
				op.MustOK()
			}
			var got []string
			for _, k := range L.ShowLocks() {
				if k.Session == "T1" && k.Type == "RECORD" {
					got = append(got, k.Object+" "+k.Mode+" "+k.Data)
				}
			}
			for _, w := range c.want {
				if !slices.Contains(got, w) {
					t.Errorf("no lock %q; record locks: %v", w, got)
				}
			}
			s.Do("ROLLBACK")
		})
	}
}

// 17.7.3 innodb_table_locks: 0 "does have an effect for tables locked for read or write by
// LOCK TABLES ... WRITE implicitly (for example, through triggers)". ta has a trigger that
// writes tb, so LOCK TABLES ta WRITE locks tb implicitly. With innodb_table_locks = 1 and
// autocommit = 0 InnoDB holds a table lock on both; with 0 on neither.
func TestLock_InnoDBTableLocksImplicitThroughTrigger(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS ta", "DROP TABLE IF EXISTS tb", "CREATE TABLE ta (id int PRIMARY KEY)", "CREATE TABLE tb (id int PRIMARY KEY)",
		"CREATE TRIGGER ta_ai AFTER INSERT ON ta FOR EACH ROW INSERT INTO tb VALUES (NEW.id)")
	s := L.Session("S")
	s.Do("SET autocommit = 0")
	for _, c := range []struct{ setting, want string }{{"1", "ta X, tb X"}, {"0", ""}} {
		s.Do("SET SESSION innodb_table_locks = " + c.setting)
		s.Do("LOCK TABLES ta WRITE").MustOK()
		var got []string
		for _, k := range L.ShowLocks() {
			if k.Session == "S" && k.Type == "TABLE" {
				got = append(got, k.Object+" "+k.Mode)
			}
		}
		slices.Sort(got)
		if g := strings.Join(got, ", "); g != c.want {
			t.Errorf("innodb_table_locks = %s: InnoDB table locks %q, want %q", c.setting, g, c.want)
		}
		s.Do("UNLOCK TABLES")
		s.Do("COMMIT")
	}
}

// 17.7.1 Predicate Locks: "Other transactions cannot insert or modify a row that would match
// the query condition." Measured on 9.7.4: that stops being true once another transaction
// has inserted a row into the spatial index and rolled back. T2's rolled-back insert of a
// point far outside the MBR takes T1's S,PREDICATE lock with it: it is gone from data_locks,
// in a transaction that is still open. After that an insert into the MBR and an UPDATE that
// moves a row into it go through without waiting, and T1's repeated locking read sees them:
// phantoms at REPEATABLE READ. Without the rolled-back insert both wait
// (TestLock_SpatialPredicateLock).
func TestLock_SpatialPredicateLockLostAfterRolledBackInsert(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS geo",
		"CREATE TABLE geo (id int PRIMARY KEY, g POINT NOT NULL SRID 0, SPATIAL INDEX (g))",
		"INSERT INTO geo VALUES (1, ST_GeomFromText('POINT(1 1)')), (2, ST_GeomFromText('POINT(100 100)'))")
	const read = "SELECT id FROM geo FORCE INDEX (g) WHERE MBRContains(ST_GeomFromText('POLYGON((0 0,0 5,5 5,5 0,0 0))'), g) ORDER BY id FOR UPDATE"
	t1, t2, t3 := L.Session("T1"), L.Session("T2"), L.Session("T3")
	t1.Begin(lab.RepeatableRead)
	t1.Do(read).MustRows("(1)")
	hasPredicateLock := func() bool {
		return slices.ContainsFunc(L.ShowLocks(), func(k lab.Lock) bool { return k.Session == "T1" && strings.Contains(k.Mode, "PREDICATE") })
	}
	if !hasPredicateLock() {
		t.Fatal("T1 took no predicate lock")
	}
	t2.Do("START TRANSACTION")
	t2.Do("INSERT INTO geo VALUES (4, ST_GeomFromText('POINT(1000 1000)'))").MustNotBlock().MustOK()
	if !hasPredicateLock() {
		t.Fatal("T1's predicate lock is gone before T2 rolled back")
	}
	t2.Do("ROLLBACK")
	if hasPredicateLock() {
		t.Fatal("T1 still holds its predicate lock after T2's rollback; on 9.7.4 it was dropped")
	}
	t3.Do("INSERT INTO geo VALUES (3, ST_GeomFromText('POINT(2 2)'))").MustNotBlock().MustOK()
	t3.Do("UPDATE geo SET g = ST_GeomFromText('POINT(3 3)') WHERE id = 2").MustNotBlock().MustOK()
	t1.Do(read).MustRows("(1) (2) (3)")
	t1.Do("ROLLBACK")
}
