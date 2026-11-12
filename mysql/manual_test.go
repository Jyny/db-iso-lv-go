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
	"slices"
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

func setGlobal(t *testing.T, L *lab.Lab, name, value string) {
	t.Helper()
	old := L.QueryString("SELECT @@GLOBAL." + name)
	L.Exec("SET GLOBAL " + name + " = " + value)
	t.Cleanup(func() { _, _ = L.Admin.Exec("SET GLOBAL " + name + " = " + old) })
}

// 17.7.1 Intention Locks: the table-level compatibility matrix. IS and IX come from
// FOR SHARE / FOR UPDATE on a row (a different row for each session, so only the table
// level can conflict); table-level S and X come from LOCK TABLES ... READ / WRITE with
// autocommit=0, which is when InnoDB also takes its own table lock.
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
// (-inf,10] (10,11] (11,13] (13,20] (20,+inf). "id > 15" locks (13,20] and (20,+inf),
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
}

// 17.7.1 Insert Intention Locks and 17.7.4 Phantom Rows: the manual's child table (90, 102).
// "id > 100 FOR UPDATE" locks the gap before 102 and the gap after it, so inserting 101 waits
// with an insert intention lock and A's repeated locking read sees no phantom. A share-mode
// read of a missing key likewise reserves it ("locks the nonexistence of something").
func TestLock_InsertIntentionAndPhantomDocsExample(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS child", "CREATE TABLE child (id int NOT NULL, PRIMARY KEY (id))",
		"INSERT INTO child (id) VALUES (90), (102)")
	a, b := L.Session("A"), L.Session("B")
	a.Do("START TRANSACTION")
	a.Do("SELECT * FROM child WHERE id > 100 FOR UPDATE").MustRows("(102)")
	b.Do("START TRANSACTION")
	ins := b.Do("INSERT INTO child (id) VALUES (101)").MustBlock()
	mustHaveLock(t, L.ShowLocks(), "B", "X,GAP,INSERT_INTENTION", "102")
	a.Do("SELECT * FROM child WHERE id > 100 FOR UPDATE").MustRows("(102)")
	a.Do("COMMIT")
	ins.MustOK()
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
// rolled-back insert leaves a gap. Modes 0 and 1 need a server restart to observe.
func TestLock_AutoIncInterleavedMode(t *testing.T) {
	L := newLab(t)
	if m := L.QueryString("SELECT @@innodb_autoinc_lock_mode"); m != "2" {
		t.Skipf("innodb_autoinc_lock_mode = %s; this test checks mode 2", m)
	}
	setupT(L)
	L.Exec("DROP TABLE IF EXISTS ai", "CREATE TABLE ai (id int AUTO_INCREMENT PRIMARY KEY, v int)")
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("START TRANSACTION")
	t1.Do("INSERT INTO ai (v) VALUES (1)")
	t1.Do("INSERT INTO ai (v) SELECT v FROM t")
	for _, k := range L.ShowLocks() {
		if strings.Contains(k.Mode, "AUTO_INC") {
			t.Fatalf("unexpected AUTO-INC lock: %v", k)
		}
	}
	t2.Do("INSERT INTO ai (v) VALUES (2)").MustNotBlock().MustOK()
	t1.Do("ROLLBACK")
	L.Note("T1's ids were consumed and are not reused: T2's row does not get id 1")
	if id := t2.Do("SELECT id FROM ai").Int(); id <= 4 {
		t.Fatalf("T2's id = %d; ids 1-4 were reserved by T1's inserts", id)
	}
}

// 17.7.1 Predicate Locks for Spatial Indexes: a locking read through a SPATIAL index locks
// the query's MBR (a predicate lock, LOCK_MODE "S,PREDICATE" in data_locks) instead of
// next-key ranges.
// An insert of a point inside the MBR waits; one far outside does not.
func TestLock_SpatialPredicateLock(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS geo",
		"CREATE TABLE geo (id int PRIMARY KEY, g POINT NOT NULL SRID 0, SPATIAL INDEX (g))",
		"INSERT INTO geo VALUES (1, ST_GeomFromText('POINT(1 1)')), (2, ST_GeomFromText('POINT(100 100)'))")
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("SET SESSION TRANSACTION ISOLATION LEVEL SERIALIZABLE")
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

// 17.7.2.1 READ COMMITTED: the manual's semi-consistent read example, verbatim. With no
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
				// READ COMMITTED keeps x-locks only on the two rows it changed; REPEATABLE
				// READ keeps one on every row it read (5 rows + the supremum).
				want := 2
				if c.level == lab.RepeatableRead {
					want = 6
				}
				if got := recordLocks(L.ShowLocks(), "A"); len(got) != want {
					t.Fatalf("A holds %d record locks %v, want %d", len(got), got, want)
				}
			}
			b.Begin(c.level)
			op := b.Do(bStmt)
			if c.blocks {
				op.MustBlock()
				a.Do("COMMIT")
				op.MustOK()
			} else {
				op.MustNotBlock().MustAffect(3)
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

// 17.7.2.3: the manual's timeline. With autocommit=0, A only sees B's committed row after A
// itself commits and so takes a new snapshot.
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
// statement-based replay gives the same result; READ COMMITTED reads S without locks.
func TestLock_InsertSelectLocksSource(t *testing.T) {
	for _, level := range []lab.Level{lab.RepeatableRead, lab.ReadCommitted} {
		for _, stmt := range []string{
			"INSERT INTO dst SELECT id FROM t FORCE INDEX (PRIMARY) WHERE id <= 20",
			"UPDATE dst SET id = id + 1000 WHERE id IN (SELECT id FROM t FORCE INDEX (PRIMARY) WHERE id <= 20)",
			"REPLACE INTO dst SELECT id FROM t FORCE INDEX (PRIMARY) WHERE id <= 20",
		} {
			t.Run(level.Short()+"/"+strings.Fields(stmt)[0], func(t *testing.T) {
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
				if level == lab.RepeatableRead {
					// INSERT ... SELECT range-scans t: next-key locks, as the manual says.
					// UPDATE ... IN (SELECT ...) is rewritten to a semijoin that looks t up by
					// primary key, so on 9.7 it takes record-only S locks instead.
					want := []string{"S 10", "S 20"}
					if strings.HasPrefix(stmt, "UPDATE") {
						want = []string{"S,REC_NOT_GAP 10", "S,REC_NOT_GAP 20"}
					}
					if !slices.Contains(onSource, want[0]) || !slices.Contains(onSource, want[1]) {
						t.Fatalf("source locks %v, want %v", onSource, want)
					}
					upd.MustBlock()
				} else {
					if len(onSource) != 0 {
						t.Fatalf("READ COMMITTED should not lock the source, got %v", onSource)
					}
					upd.MustNotBlock()
				}
				t1.Do("ROLLBACK")
				upd.MustOK()
			})
		}
	}
}

// 17.7.2.4 Locking Reads.
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
		s3.Do("SELECT * FROM tn WHERE i = 1 LOCK IN SHARE MODE NOWAIT").MustFail(func(err error) bool { return lab.Code(err) == "1064" })
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
		L.Exec("DROP TABLE IF EXISTS parent", "CREATE TABLE parent (id int PRIMARY KEY, name varchar(10))",
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
		L.Note("Jones cannot be deleted until T1 has inserted its child row and committed")
		del := t2.Do("DELETE FROM parent WHERE id = 1").MustBlock()
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
		L.Exec("DROP TABLE IF EXISTS child_codes", "CREATE TABLE child_codes (counter_field int)", "INSERT INTO child_codes VALUES (0)")
		t1, t2 := L.Session("T1"), L.Session("T2")
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
// Where 9.7 sets more than the manual lists, the case says why:
//   - delete-unique: deleting a parent row checks the FK in the child table; the empty child
//     index gets a shared lock on its supremum (the manual's "shared record-level locks on
//     the records examined", on the child side).
//   - fk-check-fails-RC, delete-unique-RC: the manual says READ COMMITTED still uses gap locks
//     for foreign-key checks. Neither check leaves one on 9.7: the failed child insert leaves
//     nothing (REPEATABLE READ leaves S,GAP), the parent delete no lock in the child index.
//   - for-update-*-nonmatching: the manual says a locking read releases the locks of rows that
//     fail the WHERE clause. That happens only in READ COMMITTED, and not for a row found by a
//     constant primary-key lookup; REPEATABLE READ keeps every lock. The UNION exception
//     (rows stay locked) does not show up for this simple UNION.
//   - insert-duplicate-unique: the clustered record for id 9 is inserted before the duplicate
//     in uu is found. Rolling it back leaves its gap locked (lock inheritance): X on the
//     PRIMARY supremum. odku-duplicate-unique leaves the same inherited lock.
func TestLock_LocksSetByStatement(t *testing.T) {
	cases := []struct {
		name, stmt string
		level      lab.Level
		fails      bool
		want       []string
	}{
		{"select-RR", "SELECT * FROM t WHERE id BETWEEN 10 AND 20", lab.RepeatableRead, false, nil},
		{"select-SER-unique", "SELECT * FROM t WHERE id = 20", lab.Serializable, false, []string{"t.PRIMARY S,REC_NOT_GAP 20"}},
		{"select-SER-range", "SELECT * FROM t WHERE id > 15", lab.Serializable, false,
			[]string{"t.PRIMARY S 20", "t.PRIMARY S 30", "t.PRIMARY S supremum pseudo-record"}},
		{"update-unique", "UPDATE t SET v = 1 WHERE id = 20", "", false, []string{"t.PRIMARY X,REC_NOT_GAP 20"}},
		{"update-range", "UPDATE t SET v = 1 WHERE id > 15", "", false,
			[]string{"t.PRIMARY X 20", "t.PRIMARY X 30", "t.PRIMARY X supremum pseudo-record"}},
		{"delete-unique", "DELETE FROM t WHERE id = 20", "", false,
			[]string{"fkc.pid S supremum pseudo-record", "t.PRIMARY X,REC_NOT_GAP 20"}},
		{"delete-missing", "DELETE FROM t WHERE id = 25", "", false, []string{"t.PRIMARY X,GAP 30"}},
		{"insert", "INSERT INTO t VALUES (25, 25, 0)", "", false, nil},
		{"insert-duplicate-pk", "INSERT INTO t VALUES (20, 0, 0)", "", true, []string{"t.PRIMARY S,REC_NOT_GAP 20"}},
		{"insert-duplicate-unique", "INSERT INTO tu (id, u) VALUES (9, 20)", "", true,
			[]string{"tu.PRIMARY X supremum pseudo-record", "tu.uu S 20, 2"}},
		{"odku-duplicate-pk", "INSERT INTO t VALUES (20, 0, 0) ON DUPLICATE KEY UPDATE v = 9", "", false, []string{"t.PRIMARY X,REC_NOT_GAP 20"}},
		{"odku-duplicate-unique", "INSERT INTO tu (id, u) VALUES (9, 20) ON DUPLICATE KEY UPDATE v = 9", "", false,
			[]string{"tu.PRIMARY X supremum pseudo-record", "tu.PRIMARY X,REC_NOT_GAP 2", "tu.uu X 20, 2"}},
		{"replace-new", "REPLACE INTO t VALUES (25, 25, 0)", "", false, nil},
		{"replace-collision", "REPLACE INTO t VALUES (20, 0, 0)", "", false, []string{"t.PRIMARY X,REC_NOT_GAP 20"}},
		{"fk-check", "INSERT INTO fkc VALUES (1, 20)", "", false, []string{"t.PRIMARY S,REC_NOT_GAP 20"}},
		{"fk-check-fails", "INSERT INTO fkc VALUES (1, 25)", "", true, []string{"t.PRIMARY S,GAP 30"}},
		{"fk-check-fails-RC", "INSERT INTO fkc VALUES (1, 25)", lab.ReadCommitted, true, nil},
		{"delete-unique-RC", "DELETE FROM t WHERE id = 20", lab.ReadCommitted, false, []string{"t.PRIMARY X,REC_NOT_GAP 20"}},
		{"update-range-RC", "UPDATE t SET v = 1 WHERE id > 15", lab.ReadCommitted, false,
			[]string{"t.PRIMARY X,REC_NOT_GAP 20", "t.PRIMARY X,REC_NOT_GAP 30"}},
		{"for-update-const-nonmatching", "SELECT * FROM t WHERE id = 20 AND v = 99 FOR UPDATE", "", false, []string{"t.PRIMARY X,REC_NOT_GAP 20"}},
		{"for-update-const-nonmatching-RC", "SELECT * FROM t WHERE id = 20 AND v = 99 FOR UPDATE", lab.ReadCommitted, false, []string{"t.PRIMARY X,REC_NOT_GAP 20"}},
		{"for-update-range-nonmatching", "SELECT * FROM t WHERE id >= 20 AND v = 99 FOR UPDATE", "", false,
			[]string{"t.PRIMARY X 30", "t.PRIMARY X supremum pseudo-record", "t.PRIMARY X,REC_NOT_GAP 20"}},
		{"for-update-range-nonmatching-RC", "SELECT * FROM t WHERE id >= 20 AND v = 99 FOR UPDATE", lab.ReadCommitted, false, nil},
		{"for-update-union-nonmatching-RC", "(SELECT * FROM t WHERE id >= 20 AND v = 99 FOR UPDATE) UNION (SELECT * FROM t WHERE id <= 10 AND v = 99 FOR UPDATE)", lab.ReadCommitted, false, nil},
		{"replace-collision-unique", "REPLACE INTO tu VALUES (9, 20, 0)", "", false,
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
			if op := s.Do(c.stmt); c.fails {
				op.MustFail(nil)
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
	deadlocks := func() int {
		var n int
		fmt.Sscan(L.QueryString(`SELECT count FROM information_schema.INNODB_METRICS WHERE NAME = "lock_deadlocks"`), &n)
		return n
	}
	before := deadlocks()
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
	upd := b.Do("UPDATE Animals SET value=30 WHERE name='Aardvark'").MustBlock()
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
	upd.MustAffect(1)
	b.Do("COMMIT")

	if got := deadlocks() - before; got != 1 {
		t.Fatalf("lock_deadlocks went up by %d, want 1", got)
	}
	logged := L.QueryString("SELECT count(*) FROM performance_schema.error_log WHERE LOGGED >= ? AND DATA LIKE '%deadlock%'", start)
	L.Note("error_log entries mentioning the deadlock (innodb_print_all_deadlocks=ON): %s", logged)
	if logged == "0" {
		t.Fatal("innodb_print_all_deadlocks did not write to the error log")
	}
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

// 17.7.5.2: by default a lock wait timeout rolls back just the statement; the rest of the
// transaction can still commit.
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
// "semaphore" row that each updates first. Deadlock detection keeps working because it is a
// row lock.
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
}

// 17.7.5.3: the last-resort fix, serializing with table locks. LOCK TABLES with
// autocommit = 0, and UNLOCK TABLES only after COMMIT.
func TestDeadlock_AvoidedByLockTables(t *testing.T) {
	L := newLab(t)
	setupT(L)
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("SET autocommit = 0")
	t2.Do("SET autocommit = 0")
	t1.Do("LOCK TABLES t WRITE")
	w := t2.Do("LOCK TABLES t WRITE").MustBlock()
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
//   - the manual says that with 0, LOCK TABLES "completes even if other transactions hold row
//     locks"; it still waits, on the metadata lock the other transaction holds until it
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
	check := func(what string) {
		t.Helper()
		var onUser, onT int
		for _, k := range L.ShowLocks() {
			switch {
			case strings.HasPrefix(k.Object, "user"):
				onUser++
			case strings.HasPrefix(k.Object, "t"):
				onT++
			}
		}
		L.Note("%s: %d lock(s) on t, %d on mysql.user", what, onT, onUser)
		if onUser != 0 {
			t.Errorf("%s: %d lock(s) on mysql.user", what, onUser)
		}
	}
	s.Begin(lab.Serializable)
	s.Do("SELECT count(*) FROM t WHERE EXISTS (SELECT 1 FROM mysql.user WHERE user = 'root')").MustOK()
	check("SERIALIZABLE SELECT joining mysql.user")
	s.Do("ROLLBACK")
	s.Begin(lab.RepeatableRead)
	s.Do("INSERT INTO dst SELECT user FROM mysql.user").MustOK()
	check("INSERT ... SELECT FROM mysql.user")
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
		"intention lock":        "TABLE LOCK table `" + dbname + "`.`child` trx id",
		"intention lock mode":   "lock mode IX",
		"record lock":           "lock_mode X locks rec but not gap",
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

// 17.7.5: "the possibility of deadlocks is not affected by the isolation level": two
// transactions that update the same two rows in opposite order deadlock at every level.
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
	deadlocks := func() string {
		return L.QueryString(`SELECT count FROM information_schema.INNODB_METRICS WHERE NAME = "lock_deadlocks"`)
	}
	before := deadlocks()
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
	if after := deadlocks(); after != before {
		t.Fatalf("lock_deadlocks went from %s to %s: InnoDB should not have seen this cycle", before, after)
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

// 17.7.5.2 / 17.7.5.3: the manual says InnoDB cannot detect a deadlock that involves a
// LOCK TABLES table lock, so only a timeout resolves it. InnoDB indeed does not see this
// one (lock_deadlocks stays put), but on 9.7 LOCK TABLES waits on metadata locks, and the
// metadata lock subsystem has a deadlock detector of its own: T1 fails at once with the
// same ERROR 1213 and is rolled back.
// (LOCK TABLES takes its metadata locks in name order: it holds aa while it waits for t.)
func TestDeadlock_LockTablesDetectedByMetadataLocks(t *testing.T) {
	L := newLab(t)
	setupT(L)
	L.Exec("DROP TABLE IF EXISTS aa", "CREATE TABLE aa (id int PRIMARY KEY, v int)", "INSERT INTO aa VALUES (1, 0)")
	deadlocks := func() string {
		return L.QueryString(`SELECT count FROM information_schema.INNODB_METRICS WHERE NAME = "lock_deadlocks"`)
	}
	before := deadlocks()
	t1, t2 := L.Session("T1"), L.Session("T2")
	t2.Do("SET autocommit = 0")
	t1.Do("START TRANSACTION")
	t1.Do("UPDATE t SET v = 1 WHERE id = 10")
	lock := t2.Do("LOCK TABLES aa WRITE, t WRITE").MustBlock()
	L.ShowMetadataLocks()
	t1.Do("UPDATE aa SET v = 1 WHERE id = 1").MustFail(lab.IsDeadlock)
	lock.MustOK()
	t2.Do("SELECT v FROM t WHERE id = 10").MustRows("(0)")
	t2.Do("UNLOCK TABLES")
	if after := deadlocks(); after != before {
		t.Fatalf("lock_deadlocks went from %s to %s: this deadlock is not InnoDB's to detect", before, after)
	}
}
