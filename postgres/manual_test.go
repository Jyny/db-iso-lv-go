package postgres

// Claims from PostgreSQL 18, Chapter 13 "Concurrency Control" that the other files in this
// package do not already cover. Each test names the section it checks:
// https://www.postgresql.org/docs/18/mvcc.html
//
// README §7 maps every section of the chapter to the tests that cover it.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jyny/db-iso-lv-go/internal/lab"
)

func setupRows(L *lab.Lab) {
	L.Exec("DROP TABLE IF EXISTS r", "CREATE TABLE r (id int primary key, v int)",
		"INSERT INTO r VALUES (1, 0), (2, 0)")
}

func hasMessage(sub string) func(error) bool {
	return func(err error) bool { return strings.Contains(lab.Describe(err), sub) }
}

// 13.1: reading never blocks writing and writing never blocks reading; only two writers
// of the same row wait for each other.
func TestMVCC_ReadersAndWritersDontBlock(t *testing.T) {
	L := newLab(t)
	setupRows(L)
	w, rd, w2 := L.Session("W"), L.Session("RD"), L.Session("W2")
	w.Do("BEGIN")
	w.Do("UPDATE r SET v = 1 WHERE id = 1")
	rd.Do("SELECT v FROM r WHERE id = 1").MustNotBlock().MustRows("(0)")
	w2.Do("UPDATE r SET v = 2 WHERE id = 2").MustNotBlock().MustOK()
	upd := w2.Do("UPDATE r SET v = 3 WHERE id = 1").MustBlock()
	w.Do("COMMIT")
	upd.MustAffect(1)

	rd.Do("BEGIN ISOLATION LEVEL REPEATABLE READ")
	rd.Do("SELECT v FROM r WHERE id = 1").MustRows("(3)")
	L.Note("and an open reader does not block a writer")
	w.Do("UPDATE r SET v = 4 WHERE id = 1").MustNotBlock().MustOK()
	rd.Do("COMMIT")
}

// 13.2.1: in READ COMMITTED every statement takes a new snapshot. A transaction sees its own
// uncommitted changes, and two SELECTs in one transaction can disagree.
func TestIso_RC_EachStatementTakesNewSnapshot(t *testing.T) {
	L := newLab(t)
	setupRows(L)
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("BEGIN")
	t1.Do("UPDATE r SET v = 10 WHERE id = 2")
	t1.Do("SELECT v FROM r ORDER BY id").MustRows("(0) (10)")
	t2.Do("UPDATE r SET v = 5 WHERE id = 1")
	L.Note("T2 committed between T1's two SELECTs: a nonrepeatable read")
	t1.Do("SELECT v FROM r ORDER BY id").MustRows("(5) (10)")
	t1.Do("COMMIT")
}

// 13.2.1: an UPDATE that waited for a concurrent writer decides what to do from that writer's
// outcome: rollback → proceed with the original row; committed DELETE → skip the row;
// committed UPDATE → re-check WHERE against the new version.
func TestIso_RC_WaitThenRecheck(t *testing.T) {
	cases := []struct {
		name, first, end string
		affected         int64
		final            string
	}{
		{"rollback", "UPDATE r SET v = 5 WHERE id = 1", "ROLLBACK", 1, "(100)"},
		{"committed-delete", "DELETE FROM r WHERE id = 1", "COMMIT", 0, "(no rows)"},
		{"committed-update-still-matches", "UPDATE r SET v = 5 WHERE id = 1", "COMMIT", 1, "(105)"},
		{"committed-update-no-longer-matches", "UPDATE r SET v = 50 WHERE id = 1", "COMMIT", 0, "(50)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			L := newLab(t)
			setupRows(L)
			t1, t2 := L.Session("T1"), L.Session("T2")
			t1.Do("BEGIN")
			t1.Do(c.first)
			upd := t2.Do("UPDATE r SET v = v + 100 WHERE id = 1 AND v < 10").MustBlock()
			t1.Do(c.end)
			upd.MustAffect(c.affected)
			t2.Do("SELECT v FROM r WHERE id = 1").MustRows(c.final)
		})
	}
}

// 13.2.1: INSERT ... ON CONFLICT DO UPDATE always ends in an insert or an update, even when
// the conflicting row is uncommitted when it starts; DO NOTHING skips it.
func TestIso_RC_InsertOnConflict(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS kv", "CREATE TABLE kv (k int primary key, v text)")
	t1, t2 := L.Session("T1"), L.Session("T2")

	t1.Do("BEGIN")
	t1.Do("INSERT INTO kv VALUES (1, 't1')")
	up := t2.Do("INSERT INTO kv VALUES (1, 't2') ON CONFLICT (k) DO UPDATE SET v = excluded.v").MustBlock()
	t1.Do("COMMIT")
	L.Note("T1's row became visible after the wait, so T2 took the UPDATE path")
	up.MustAffect(1)
	t2.Do("SELECT v FROM kv WHERE k = 1").MustRows("(t2)")

	t1.Do("BEGIN")
	t1.Do("INSERT INTO kv VALUES (2, 't1')")
	skip := t2.Do("INSERT INTO kv VALUES (2, 't2') ON CONFLICT DO NOTHING").MustBlock()
	t1.Do("COMMIT")
	skip.MustAffect(0)
	t2.Do("SELECT v FROM kv WHERE k = 2").MustRows("(t1)")
}

// 13.2.1: MERGE gives no such guarantee. Its WHEN NOT MATCHED branch was chosen from the
// snapshot, so a concurrent insert of the same key turns into a unique violation.
func TestIso_RC_MergeConcurrentInsertFails(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS kv", "CREATE TABLE kv (k int primary key, v text)")
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("BEGIN")
	t1.Do("INSERT INTO kv VALUES (1, 't1')")
	m := t2.Do(`MERGE INTO kv USING (VALUES (1, 't2')) s(k, v) ON kv.k = s.k
WHEN MATCHED THEN UPDATE SET v = s.v
WHEN NOT MATCHED THEN INSERT VALUES (s.k, s.v)`).MustBlock()
	t1.Do("COMMIT")
	m.MustFail(lab.IsUniqueViolation)
	L.Note("INSERT ... ON CONFLICT DO UPDATE handles the same race (TestIso_RC_InsertOnConflict)")
}

// 13.2.2 / 13.3.2: in REPEATABLE READ, touching a row that a concurrent transaction changed
// after our snapshot fails with 40001, unless that transaction rolled back or only LOCKED the
// row. 13.4.2: a no-op UPDATE under the lock is what makes the conflict stick.
func TestIso_RR_ConcurrentChangeOutcomes(t *testing.T) {
	cases := []struct {
		name  string
		other []string // run by T1 while T2's statement waits (or before it starts)
		end   string
		stmt  string
		fail  string // expected error message part; "" = succeeds
	}{
		{"committed-update", []string{"UPDATE r SET v = 1 WHERE id = 1"}, "COMMIT",
			"UPDATE r SET v = v + 1 WHERE id = 1", "could not serialize access due to concurrent update"},
		{"committed-delete", []string{"DELETE FROM r WHERE id = 1"}, "COMMIT",
			"UPDATE r SET v = v + 1 WHERE id = 1", "could not serialize access due to concurrent delete"},
		{"rolled-back-update", []string{"UPDATE r SET v = 1 WHERE id = 1"}, "ROLLBACK",
			"UPDATE r SET v = v + 1 WHERE id = 1", ""},
		{"lock-only", []string{"SELECT * FROM r WHERE id = 1 FOR UPDATE"}, "COMMIT",
			"UPDATE r SET v = v + 1 WHERE id = 1", ""},
		{"lock-plus-noop-update", []string{"SELECT * FROM r WHERE id = 1 FOR UPDATE", "UPDATE r SET v = v WHERE id = 1"}, "COMMIT",
			"UPDATE r SET v = v + 1 WHERE id = 1", "could not serialize access due to concurrent update"},
		{"select-for-update", []string{"UPDATE r SET v = 1 WHERE id = 1"}, "COMMIT",
			"SELECT * FROM r WHERE id = 1 FOR UPDATE", "could not serialize access due to concurrent update"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			L := newLab(t)
			setupRows(L)
			t1, t2 := L.Session("T1"), L.Session("T2")
			t2.Do("BEGIN ISOLATION LEVEL REPEATABLE READ")
			t2.Do("SELECT v FROM r WHERE id = 1")
			t1.Do("BEGIN")
			for _, q := range c.other {
				t1.Do(q)
			}
			op := t2.Do(c.stmt).MustBlock()
			t1.Do(c.end)
			if c.fail == "" {
				op.MustOK()
			} else {
				op.MustFail(func(err error) bool { return lab.IsSerialization(err) && hasMessage(c.fail)(err) })
			}
			t2.Do("ROLLBACK")
		})
	}

	t.Run("read-only-never-conflicts", func(t *testing.T) {
		L := newLab(t)
		setupRows(L)
		t1, t2 := L.Session("T1"), L.Session("T2")
		t2.Do("BEGIN ISOLATION LEVEL REPEATABLE READ")
		t2.Do("SELECT v FROM r WHERE id = 1").MustRows("(0)")
		t1.Do("UPDATE r SET v = 1 WHERE id = 1")
		t1.Do("DELETE FROM r WHERE id = 2")
		t2.Do("SELECT v FROM r ORDER BY id").MustRows("(0) (0)")
		t2.Do("COMMIT").MustOK()
	})
}

// 13.2.3: the docs' mytab example. Each transaction sums one class and inserts into the other.
// Every serial order would make one of them see the other's insert, so SERIALIZABLE fails one
// commit. REPEATABLE READ commits both. SIRead predicate locks never block.
func TestIso_SER_MytabDocsExample(t *testing.T) {
	for _, level := range []lab.Level{lab.RepeatableRead, lab.Serializable} {
		t.Run(level.Short(), func(t *testing.T) {
			L := newLab(t)
			L.Exec("DROP TABLE IF EXISTS mytab", "CREATE TABLE mytab (class int, value int)",
				"INSERT INTO mytab VALUES (1, 10), (1, 20), (2, 100), (2, 200)")
			a, b := L.Session("A"), L.Session("B")
			a.Begin(level)
			b.Begin(level)
			a.Do("SELECT SUM(value) FROM mytab WHERE class = 1").MustRows("(30)")
			b.Do("SELECT SUM(value) FROM mytab WHERE class = 2").MustRows("(300)")
			a.Do("INSERT INTO mytab VALUES (2, 30)").MustNotBlock()
			b.Do("INSERT INTO mytab VALUES (1, 300)").MustNotBlock()
			a.Do("COMMIT").MustOK()
			cb := b.Do("COMMIT")
			if level == lab.RepeatableRead {
				cb.MustOK()
				return
			}
			cb.MustFail(func(err error) bool {
				return lab.IsSerialization(err) && hasMessage("read/write dependencies among transactions")(err)
			})
		})
	}
}

// 13.2.3: SERIALIZABLE READ ONLY DEFERRABLE waits at its first statement until it can take a
// snapshot that no concurrent read-write serializable transaction can invalidate. It is the
// only case where SERIALIZABLE blocks and REPEATABLE READ does not; after that it can never fail.
// It does not take a newer snapshot after the wait: it keeps the one it took at the start
// once that has been proven safe, so it still does not see RW's update.
func TestIso_SER_ReadOnlyDeferrable(t *testing.T) {
	L := newLab(t)
	setupRows(L)
	rw, ro := L.Session("RW"), L.Session("RO")
	rw.Do("BEGIN ISOLATION LEVEL SERIALIZABLE")
	rw.Do("UPDATE r SET v = 1 WHERE id = 1")

	ro.Do("BEGIN ISOLATION LEVEL SERIALIZABLE READ ONLY")
	ro.Do("SELECT sum(v) FROM r").MustNotBlock().MustRows("(0)")
	ro.Do("COMMIT")

	ro.Do("BEGIN ISOLATION LEVEL SERIALIZABLE READ ONLY DEFERRABLE")
	sel := ro.Do("SELECT sum(v) FROM r").MustBlock()
	L.ShowAs("pg_stat_activity", "SELECT application_name, wait_event_type, wait_event FROM pg_stat_activity WHERE application_name = 'lab:RO'")
	rw.Do("COMMIT")
	sel.MustRows("(0)")
	ro.Do("COMMIT")
}

// 13.2.3: under SERIALIZABLE, a transaction that checked for a key before inserting it gets a
// serialization failure (40001, retryable) instead of a unique violation (23505) when a
// concurrent transaction inserts the same key.
func TestIso_SER_CheckBeforeInsertTurnsUniqueViolationInto40001(t *testing.T) {
	for _, check := range []bool{false, true} {
		name := "no-check"
		if check {
			name = "check-first"
		}
		t.Run(name, func(t *testing.T) {
			L := newLab(t)
			L.Exec("DROP TABLE IF EXISTS users", "CREATE TABLE users (username text primary key)")
			t1, t2 := L.Session("T1"), L.Session("T2")
			for _, s := range []*lab.Session{t1, t2} {
				s.Begin(lab.Serializable)
				if check {
					s.Do("SELECT 1 FROM users WHERE username = 'alice'").MustRows("(no rows)")
				}
			}
			t1.Do("INSERT INTO users VALUES ('alice')")
			ins := t2.Do("INSERT INTO users VALUES ('alice')").MustBlock()
			t1.Do("COMMIT")
			if check {
				ins.MustFail(lab.IsSerialization)
			} else {
				ins.MustFail(lab.IsUniqueViolation)
			}
		})
	}
}

// 13.2 (Important box): sequence changes are visible immediately to everyone and are never
// rolled back, so serial columns get gaps.
func TestMVCC_SequencesAreNotTransactional(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS sq", "CREATE TABLE sq (id serial primary key, v int)")
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("BEGIN ISOLATION LEVEL REPEATABLE READ")
	t1.Do("INSERT INTO sq (v) VALUES (1) RETURNING id").MustRows("(1)")
	t2.Do("INSERT INTO sq (v) VALUES (2) RETURNING id").MustNotBlock().MustRows("(2)")
	L.Note("T1's snapshot does not hide T2's nextval either")
	t1.Do("SELECT nextval('sq_id_seq')").MustRows("(3)")
	t1.Do("ROLLBACK")
	t2.Do("INSERT INTO sq (v) VALUES (3) RETURNING id").MustRows("(4)")
	t2.Do("SELECT id FROM sq ORDER BY id").MustRows("(2) (4)")
}

var tableLockModes = []string{"ACCESS SHARE", "ROW SHARE", "ROW EXCLUSIVE", "SHARE UPDATE EXCLUSIVE",
	"SHARE", "SHARE ROW EXCLUSIVE", "EXCLUSIVE", "ACCESS EXCLUSIVE"}

// tableLockConflicts is Table 13.2 of the docs, in tableLockModes order.
var tableLockConflicts = [][]bool{
	{false, false, false, false, false, false, false, true},
	{false, false, false, false, false, false, true, true},
	{false, false, false, false, true, true, true, true},
	{false, false, false, true, true, true, true, true},
	{false, false, true, true, false, true, true, true},
	{false, false, true, true, true, true, true, true},
	{false, true, true, true, true, true, true, true},
	{true, true, true, true, true, true, true, true},
}

// 13.3.1 Table 13.2: the eight table-level lock modes, measured with LOCK TABLE.
func TestLock_TableLockConflictMatrix(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS tl CASCADE", "CREATE TABLE tl (id int primary key)")
	L.Quiet()
	t1, t2 := L.Session("T1"), L.Session("T2")
	abbr := []string{"AS", "RS", "RE", "SUE", "S", "SRE", "E", "AE"}
	var b strings.Builder
	fmt.Fprintf(&b, "\n%-24s %s\n", "held \\ requested", strings.Join(abbr, "  "))
	for i, held := range tableLockModes {
		fmt.Fprintf(&b, "%-24s", held)
		for j, req := range tableLockModes {
			t1.Do("BEGIN")
			t1.Do("LOCK TABLE tl IN " + held + " MODE")
			t2.Do("BEGIN")
			op := t2.Do("LOCK TABLE tl IN " + req + " MODE")
			blocked := op.Blocked
			t1.Do("ROLLBACK")
			op.Wait()
			t2.Do("ROLLBACK")
			cell := "."
			if blocked {
				cell = "X"
			}
			fmt.Fprintf(&b, " %-*s", len(abbr[j])+1, cell)
			if blocked != tableLockConflicts[i][j] {
				t.Errorf("%s vs %s: blocked=%v, want %v", held, req, blocked, tableLockConflicts[i][j])
			}
		}
		b.WriteString("\n")
	}
	t.Log(b.String())
}

// lockRank orders pg_locks modes from weakest to strongest.
var lockRank = map[string]int{
	"AccessShareLock": 1, "RowShareLock": 2, "RowExclusiveLock": 3, "ShareUpdateExclusiveLock": 4,
	"ShareLock": 5, "ShareRowExclusiveLock": 6, "ExclusiveLock": 7, "AccessExclusiveLock": 8,
}

// 13.3.1: which table lock each command takes, read from pg_locks inside the transaction.
// The assertion is on the strongest mode the command holds on the relation.
func TestLock_TableLockAcquiredByCommands(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS tl2", "DROP TABLE IF EXISTS tl CASCADE",
		"CREATE TABLE tl (id int primary key, v int)", "INSERT INTO tl VALUES (1, 0)",
		"CREATE TABLE tl2 (id int primary key, tl_id int)",
		"CREATE MATERIALIZED VIEW tl_mv AS SELECT id FROM tl", "CREATE UNIQUE INDEX ON tl_mv (id)",
		"CREATE OR REPLACE FUNCTION tl_noop() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'")
	cases := []struct{ sql, rel, want string }{
		{"SELECT * FROM tl", "tl", "AccessShareLock"},
		{"SELECT * FROM tl FOR UPDATE", "tl", "RowShareLock"},
		{"SELECT * FROM tl FOR KEY SHARE", "tl", "RowShareLock"},
		{"INSERT INTO tl VALUES (2, 0)", "tl", "RowExclusiveLock"},
		{"UPDATE tl SET v = 1", "tl", "RowExclusiveLock"},
		{"DELETE FROM tl", "tl", "RowExclusiveLock"},
		{"MERGE INTO tl USING (VALUES (1)) s(id) ON tl.id = s.id WHEN MATCHED THEN UPDATE SET v = 2", "tl", "RowExclusiveLock"},
		{"ANALYZE tl", "tl", "ShareUpdateExclusiveLock"},
		{"CREATE STATISTICS tl_st ON id, v FROM tl", "tl", "ShareUpdateExclusiveLock"},
		{"COMMENT ON TABLE tl IS 'x'", "tl", "ShareUpdateExclusiveLock"},
		{"ALTER TABLE tl SET (fillfactor = 50)", "tl", "ShareUpdateExclusiveLock"},
		{"CREATE INDEX tl_v ON tl (v)", "tl", "ShareLock"},
		{"CREATE TRIGGER tl_trg BEFORE INSERT ON tl FOR EACH ROW EXECUTE FUNCTION tl_noop()", "tl", "ShareRowExclusiveLock"},
		{"ALTER TABLE tl2 ADD FOREIGN KEY (tl_id) REFERENCES tl (id)", "tl", "ShareRowExclusiveLock"},
		{"REFRESH MATERIALIZED VIEW CONCURRENTLY tl_mv", "tl_mv", "ExclusiveLock"},
		{"REFRESH MATERIALIZED VIEW tl_mv", "tl_mv", "AccessExclusiveLock"},
		{"REINDEX INDEX tl_pkey", "tl_pkey", "AccessExclusiveLock"},
		{"REINDEX TABLE tl", "tl", "ShareLock"},
		{"CLUSTER tl USING tl_pkey", "tl", "AccessExclusiveLock"},
		{"ALTER TABLE tl ADD COLUMN c int", "tl", "AccessExclusiveLock"},
		{"TRUNCATE tl", "tl", "AccessExclusiveLock"},
		{"LOCK TABLE tl", "tl", "AccessExclusiveLock"},
		{"DROP TABLE tl CASCADE", "tl", "AccessExclusiveLock"},
	}
	s := L.Session("T1")
	for _, c := range cases {
		oid := L.QueryString("SELECT $1::regclass::oid", c.rel)
		s.Do("BEGIN")
		s.Do(c.sql).MustOK()
		modes := s.Do("SELECT string_agg(mode, ',' ORDER BY mode) FROM pg_locks WHERE pid = pg_backend_pid() AND locktype = 'relation' AND relation = " + oid).Str()
		s.Do("ROLLBACK")
		strongest := ""
		for _, m := range strings.Split(modes, ",") {
			if lockRank[m] > lockRank[strongest] {
				strongest = m
			}
		}
		if strongest != c.want {
			t.Errorf("%s: strongest lock on %s is %q (all: %s), want %s", c.sql, c.rel, strongest, modes, c.want)
		}
	}
}

// 13.3.1: a transaction never conflicts with itself, and a table lock taken after a savepoint
// is released when the transaction rolls back to that savepoint.
func TestLock_TableLockOwnTxnAndSavepoint(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS tl CASCADE", "CREATE TABLE tl (id int primary key)")
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("BEGIN")
	t1.Do("LOCK TABLE tl IN ACCESS EXCLUSIVE MODE")
	t1.Do("LOCK TABLE tl IN SHARE MODE").MustNotBlock().MustOK()
	t1.Do("SELECT count(*) FROM tl").MustNotBlock().MustOK()
	t1.Do("COMMIT")

	t1.Do("BEGIN")
	t1.Do("SAVEPOINT a")
	t1.Do("LOCK TABLE tl IN ACCESS EXCLUSIVE MODE")
	sel := t2.Do("SELECT count(*) FROM tl").MustBlock()
	t1.Do("ROLLBACK TO SAVEPOINT a")
	L.Note("the lock is gone although T1's transaction is still open")
	sel.MustOK()
	t1.Do("COMMIT")
}

// 13.3.2: which row lock each statement takes, seen through pgrowlocks, and what it blocks.
//   - DELETE, and UPDATE of a column with a unique index usable by a foreign key → FOR UPDATE
//   - any other UPDATE (including of a column with only a PARTIAL unique index) → FOR NO KEY UPDATE
//
// Row locks never block plain reads.
func TestLock_RowLocksTakenByStatements(t *testing.T) {
	cases := []struct {
		stmt, mode  string
		blocksKeySh bool
		blocksShare bool
	}{
		{"UPDATE rl SET v = 1 WHERE id = 1", "No Key Update", false, true},
		{"UPDATE rl SET p = 7 WHERE id = 1", "No Key Update", false, true},
		{"UPDATE rl SET u = 7 WHERE id = 1", "Update", true, true},
		{"DELETE FROM rl WHERE id = 1", "Update", true, true},
	}
	for _, c := range cases {
		t.Run(strings.Fields(c.stmt)[0]+"-"+strings.Fields(c.stmt)[3], func(t *testing.T) {
			L := newLab(t)
			L.Exec("DROP TABLE IF EXISTS rl",
				"CREATE TABLE rl (id int primary key, u int unique, p int, v int)",
				"CREATE UNIQUE INDEX rl_p ON rl (p) WHERE p > 0",
				"INSERT INTO rl VALUES (1, 1, 1, 0)")
			t1, t2 := L.Session("T1"), L.Session("T2")
			t1.Do("BEGIN")
			t1.Do(c.stmt)
			modes := L.ShowAs("pgrowlocks('rl')", "SELECT locked_row, modes FROM pgrowlocks('rl')")
			if len(modes) != 1 || strings.Trim(modes[0][1], `{}"`) != c.mode {
				t.Fatalf("pgrowlocks = %v, want mode %q", modes, c.mode)
			}
			t2.Do("SELECT v FROM rl WHERE id = 1").MustNotBlock().MustOK()
			t2.Do("BEGIN")
			if op := t2.Do("SELECT 1 FROM rl WHERE id = 1 FOR KEY SHARE NOWAIT"); c.blocksKeySh {
				op.MustFail(lab.IsLockNotAvailable)
			} else {
				op.MustOK()
			}
			t2.Do("ROLLBACK")
			t2.Do("BEGIN")
			if op := t2.Do("SELECT 1 FROM rl WHERE id = 1 FOR SHARE NOWAIT"); c.blocksShare {
				op.MustFail(lab.IsLockNotAvailable)
			} else {
				op.MustOK()
			}
			t2.Do("ROLLBACK")
			t1.Do("ROLLBACK")
		})
	}
}

// 13.3.2: a transaction may hold conflicting row locks on one row in different
// subtransactions, and rolling back to a savepoint releases the row locks taken after it.
func TestLock_RowLockSavepoints(t *testing.T) {
	L := newLab(t)
	setupRows(L)
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("BEGIN")
	t1.Do("SELECT * FROM r WHERE id = 1 FOR SHARE")
	t1.Do("SAVEPOINT a")
	t1.Do("SELECT * FROM r WHERE id = 1 FOR UPDATE").MustNotBlock().MustOK()
	L.Show("SELECT locked_row, multi, modes FROM pgrowlocks('r')")
	upd := t2.Do("UPDATE r SET v = 1 WHERE id = 1").MustBlock()
	t1.Do("ROLLBACK TO SAVEPOINT a")
	L.Note("FOR UPDATE is gone, but the FOR SHARE taken before the savepoint still blocks T2")
	L.Settle()
	upd.MustBlock()
	t1.Do("COMMIT")
	upd.MustAffect(1)
}

// 13.3.2: row locks live in the tuples, not in the shared lock table, so there is no limit on
// how many rows a transaction can lock. Locking 100,000 rows adds no entries to pg_locks.
func TestLock_RowLocksNotInLockTable(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS big", "CREATE TABLE big (id int primary key)",
		"INSERT INTO big SELECT generate_series(1, 100000)")
	s := L.Session("T1")
	s.Do("BEGIN")
	before := s.Do("SELECT count(*) FROM pg_locks WHERE pid = pg_backend_pid()").Int()
	s.Do("SELECT count(*) FROM (SELECT id FROM big FOR UPDATE) x").MustRows("(100000)")
	after := s.Do("SELECT count(*) FROM pg_locks WHERE pid = pg_backend_pid()").Int()
	locked := L.QueryString("SELECT count(*) FROM pgrowlocks('big')")
	L.Note("pg_locks entries for T1: %d before, %d after; pgrowlocks: %s locked rows", before, after, locked)
	if locked != "100000" || after-before > 3 {
		t.Fatalf("expected 100000 tuple-level locks and a constant number of pg_locks entries; got %s and %d → %d", locked, before, after)
	}
	s.Do("ROLLBACK")
}

// 13.3.4: deadlocks are not only about rows. Two transactions that lock two tables in
// opposite order deadlock on table locks.
func TestDeadlock_TableLocksOppositeOrder(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS ta", "DROP TABLE IF EXISTS tb", "CREATE TABLE ta (id int)", "CREATE TABLE tb (id int)")
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("BEGIN")
	t2.Do("BEGIN")
	t1.Do("LOCK TABLE ta IN EXCLUSIVE MODE")
	t2.Do("LOCK TABLE tb IN EXCLUSIVE MODE")
	a := t1.Do("LOCK TABLE tb IN EXCLUSIVE MODE")
	b := t2.Do("LOCK TABLE ta IN EXCLUSIVE MODE")
	L.ShowDeadlock(lab.ExpectOneDeadlockVictim(t, a, b))
	t1.Do("ROLLBACK")
	t2.Do("ROLLBACK")
}

// 13.3.4: "the first lock acquired on an object in a transaction [should be] the most
// restrictive mode that will be needed". TestDeadlock_TableLockUpgrade deadlocks because both
// start with SHARE and then need ROW EXCLUSIVE. Starting with the self-conflicting
// SHARE ROW EXCLUSIVE makes the second transaction queue instead.
func TestDeadlock_AvoidedByStrongestLockFirst(t *testing.T) {
	L := newLab(t)
	setupAccounts(L)
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("BEGIN")
	t2.Do("BEGIN")
	t1.Do("LOCK TABLE acct IN SHARE ROW EXCLUSIVE MODE")
	w := t2.Do("LOCK TABLE acct IN SHARE ROW EXCLUSIVE MODE").MustBlock()
	t1.Do("INSERT INTO acct VALUES (10, 0)").MustOK()
	t1.Do("COMMIT")
	w.MustOK()
	t2.Do("INSERT INTO acct VALUES (11, 0)").MustOK()
	t2.Do("COMMIT")
}

// 13.3.5: advisory lock semantics.
func TestLock_AdvisorySemantics(t *testing.T) {
	L := newLab(t)
	t1, t2 := L.Session("T1"), L.Session("T2")
	free := func(key int) bool {
		t.Helper()
		ok := t2.Do(fmt.Sprintf("SELECT pg_try_advisory_lock(%d)", key)).Str() == "true"
		if ok {
			t2.Do(fmt.Sprintf("SELECT pg_advisory_unlock(%d)", key))
		}
		return ok
	}

	L.Note("session-level locks ignore transaction semantics: a ROLLBACK does not release them")
	t1.Do("BEGIN")
	t1.Do("SELECT pg_advisory_lock(1)")
	t1.Do("ROLLBACK")
	if free(1) {
		t.Fatal("session-level lock was released by ROLLBACK")
	}

	L.Note("…and an unlock takes effect even if the transaction that ran it fails")
	t1.Do("BEGIN")
	t1.Do("SELECT pg_advisory_unlock(1)").MustRows("(true)")
	t1.Do("SELECT 1/0").MustFail(nil)
	t1.Do("ROLLBACK")
	if !free(1) {
		t.Fatal("unlock was undone by the failed transaction")
	}

	L.Note("locks stack: two acquisitions need two unlocks")
	t1.Do("SELECT pg_advisory_lock(2)")
	t1.Do("SELECT pg_advisory_lock(2)")
	t1.Do("SELECT pg_advisory_unlock(2)").MustRows("(true)")
	if free(2) {
		t.Fatal("lock released after one of two unlocks")
	}
	t1.Do("SELECT pg_advisory_unlock(2)").MustRows("(true)")
	if !free(2) {
		t.Fatal("lock still held after two unlocks")
	}

	L.Note("transaction-level locks end with the transaction and cannot be unlocked")
	t1.Do("BEGIN")
	t1.Do("SELECT pg_advisory_xact_lock(3)")
	t1.Do("SELECT pg_advisory_unlock(3)").MustRows("(false)")
	if free(3) {
		t.Fatal("transaction-level lock not held")
	}
	t1.Do("COMMIT")
	if !free(3) {
		t.Fatal("transaction-level lock outlived COMMIT")
	}

	L.Note("session- and transaction-level requests for one key block each other across sessions")
	t1.Do("SELECT pg_advisory_lock(4)")
	t2.Do("BEGIN")
	x := t2.Do("SELECT pg_advisory_xact_lock(4)").MustBlock()
	L.Note("but the holder's own requests always succeed, even with T2 waiting")
	t1.Do("BEGIN")
	t1.Do("SELECT pg_advisory_xact_lock(4)").MustNotBlock().MustOK()
	t1.Do("COMMIT")
	t1.Do("SELECT pg_advisory_unlock(4)")
	x.MustOK()
	t2.Do("COMMIT")
}

// 13.3.5: a locking function must only run on the rows you mean to lock. In a WHERE clause
// it runs for every row the scan visits, before ORDER BY ... LIMIT picks one, which is the
// classic job-queue bug: the locks on the other rows stay until the session ends.
// (PG 18 evaluates a volatile function in the SELECT list after LIMIT, so the docs' own
// "danger" query happens to lock only the limited rows here.)
func TestLock_AdvisoryLockFilterPitfall(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS foo", "CREATE TABLE foo (id int primary key, prio int)",
		"INSERT INTO foo SELECT g, g FROM generate_series(1, 1000) g", "ANALYZE foo")
	s := L.Session("W")
	held := func() int {
		return s.Do("SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND pid = pg_backend_pid()").Int()
	}
	s.Do("SELECT id FROM foo WHERE id > 100 AND pg_try_advisory_lock(id) ORDER BY prio LIMIT 1").MustRows("(101)")
	danger := held()
	s.Do("SELECT pg_advisory_unlock_all()")
	s.Do("SELECT q.id FROM (SELECT id FROM foo WHERE id > 100 ORDER BY prio LIMIT 1) q WHERE pg_try_advisory_lock(q.id)").MustRows("(101)")
	safe := held()
	s.Do("SELECT pg_advisory_unlock_all()")
	L.Note("advisory locks held: %d with the lock in WHERE, %d with LIMIT applied first in a subquery", danger, safe)
	if danger != 900 || safe != 1 {
		t.Fatalf("held %d and %d locks, want 900 and 1", danger, safe)
	}
}

// 13.4.2: SELECT FOR UPDATE locks only the returned rows, and only until the transaction ends.
func TestLock_ForUpdateLocksOnlyReturnedRows(t *testing.T) {
	L := newLab(t)
	setupRows(L)
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("BEGIN")
	t1.Do("SELECT * FROM r WHERE id = 1 FOR UPDATE")
	t2.Do("UPDATE r SET v = 1 WHERE id = 2").MustNotBlock().MustOK()
	upd := t2.Do("UPDATE r SET v = 1 WHERE id = 1").MustBlock()
	t1.Do("COMMIT")
	L.Note("T1 never updated the row, so T2's blocked UPDATE simply proceeds")
	upd.MustAffect(1)
}

// 13.4.2: in REPEATABLE READ the snapshot is frozen by the first query, not by BEGIN, so a
// LOCK TABLE taken before it waits out all writers and the snapshot then includes their
// work. Taken after the first query, the lock waits just the same, but the snapshot is
// already stale. A SHARE lock guarantees no one else has uncommitted changes in the table.
func TestIso_RR_LockTableBeforeSnapshot(t *testing.T) {
	for _, lockFirst := range []bool{true, false} {
		name := "lock-then-query"
		if !lockFirst {
			name = "query-then-lock"
		}
		t.Run(name, func(t *testing.T) {
			L := newLab(t)
			L.Exec("DROP TABLE IF EXISTS credits", "CREATE TABLE credits (amount int)", "INSERT INTO credits VALUES (100)")
			chk, wr := L.Session("CHK"), L.Session("WR")
			wr.Do("BEGIN")
			wr.Do("INSERT INTO credits VALUES (50)")
			chk.Do("BEGIN ISOLATION LEVEL REPEATABLE READ")
			if !lockFirst {
				chk.Do("SELECT sum(amount) FROM credits").MustRows("(100)")
			}
			lock := chk.Do("LOCK TABLE credits IN SHARE MODE").MustBlock()
			wr.Do("COMMIT")
			lock.MustOK()
			want := "(150)"
			if !lockFirst {
				want = "(100)"
			}
			chk.Do("SELECT sum(amount) FROM credits").MustRows(want)
			chk.Do("COMMIT")
		})
	}
}

// 13.5: an exclusion-constraint conflict (23P01) behaves like a unique one: the second
// inserter waits for the first transaction's outcome.
func TestLock_ExclusionConstraintWaitsForOutcome(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS booking", "CREATE TABLE booking (room int, during int4range, EXCLUDE USING gist (during WITH &&))")
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("BEGIN")
	t1.Do("INSERT INTO booking VALUES (1, '[1,5)')")
	ins := t2.Do("INSERT INTO booking VALUES (1, '[3,8)')").MustBlock()
	t1.Do("COMMIT")
	ins.MustFail(func(err error) bool { return lab.Code(err) == "23P01" })
	t2.Do("INSERT INTO booking VALUES (1, '[5,8)')").MustOK()
}

// 13.6: TRUNCATE and table-rewriting ALTER TABLE are not MVCC-safe. A REPEATABLE READ
// transaction whose snapshot predates them, but which had not touched the table yet, sees it
// empty. A transaction that already read the table holds ACCESS SHARE and blocks them.
func TestMVCC_TruncateIsNotMVCCSafe(t *testing.T) {
	for _, ddl := range []string{"TRUNCATE big5", "ALTER TABLE big5 ALTER COLUMN v TYPE bigint", "DELETE FROM big5"} {
		t.Run(strings.Fields(ddl)[0], func(t *testing.T) {
			L := newLab(t)
			L.Exec("DROP TABLE IF EXISTS big5", "CREATE TABLE big5 (v int)", "INSERT INTO big5 SELECT generate_series(1, 5)")
			rd, dd := L.Session("RD"), L.Session("DDL")
			rd.Do("BEGIN ISOLATION LEVEL REPEATABLE READ")
			rd.Do("SELECT 1")
			dd.Do(ddl).MustNotBlock().MustOK()
			want := "(0)"
			if strings.HasPrefix(ddl, "DELETE") {
				want = "(5)"
				L.Note("DELETE is MVCC-safe: the old snapshot still sees all 5 rows")
			}
			rd.Do("SELECT count(*) FROM big5").MustRows(want)
			rd.Do("COMMIT")
		})
	}
	t.Run("blocked-if-already-read", func(t *testing.T) {
		L := newLab(t)
		L.Exec("DROP TABLE IF EXISTS big5", "CREATE TABLE big5 (v int)", "INSERT INTO big5 SELECT generate_series(1, 5)")
		rd, dd := L.Session("RD"), L.Session("DDL")
		rd.Do("BEGIN ISOLATION LEVEL REPEATABLE READ")
		rd.Do("SELECT count(*) FROM big5").MustRows("(5)")
		tr := dd.Do("TRUNCATE big5").MustBlock()
		rd.Do("SELECT count(*) FROM big5").MustRows("(5)")
		rd.Do("COMMIT")
		tr.MustOK()
	})
}

// 13.6: catalog lookups do not use the transaction's snapshot. A table created after a
// REPEATABLE READ snapshot can be queried (its committed rows stay invisible), while a
// query on pg_class itself, which does use the snapshot, does not show it.
func TestMVCC_CatalogAccessIgnoresSnapshot(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS newt")
	rd, dd := L.Session("RD"), L.Session("DDL")
	rd.Do("BEGIN ISOLATION LEVEL REPEATABLE READ")
	rd.Do("SELECT 1")
	dd.Do("CREATE TABLE newt (id int)")
	dd.Do("INSERT INTO newt VALUES (1)")
	rd.Do("SELECT count(*) FROM newt").MustRows("(0)")
	rd.Do("SELECT count(*) FROM pg_class WHERE relname = 'newt'").MustRows("(0)")
	rd.Do("COMMIT")
}

// 13.4.1: the docs' advice for relying on SERIALIZABLE: make it the default
// (default_transaction_isolation) and guard against transactions at other levels, for
// example with a trigger that checks transaction_isolation.
func TestIso_DefaultSerializableAndTriggerGuard(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS guarded", "CREATE TABLE guarded (id int primary key)",
		`CREATE OR REPLACE FUNCTION require_serializable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF current_setting('transaction_isolation') <> 'serializable' THEN
    RAISE EXCEPTION 'writes to % must run at SERIALIZABLE, not %', TG_TABLE_NAME, current_setting('transaction_isolation');
  END IF;
  RETURN NEW;
END $$`,
		"CREATE TRIGGER guard BEFORE INSERT OR UPDATE OR DELETE ON guarded FOR EACH ROW EXECUTE FUNCTION require_serializable()")
	s := L.Session("S")
	s.Do("BEGIN")
	s.Do("INSERT INTO guarded VALUES (1)").MustFail(hasMessage("must run at SERIALIZABLE, not read committed"))
	s.Do("ROLLBACK")

	s.Do("SET default_transaction_isolation = 'serializable'")
	s.Do("BEGIN")
	s.Do("SHOW transaction_isolation").MustRows("(serializable)")
	s.Do("INSERT INTO guarded VALUES (1)").MustOK()
	s.Do("COMMIT")
	L.Note("an autocommit statement is a transaction too, and uses the default")
	s.Do("INSERT INTO guarded VALUES (2)").MustOK()
	s.Do("SELECT count(*) FROM guarded").MustRows("(2)")
}
