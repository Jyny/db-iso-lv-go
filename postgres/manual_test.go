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
	"time"

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
// the conflicting row is uncommitted when it starts; DO NOTHING skips it. "The UPDATE clause
// will affect that row, even though possibly no version of that row is conventionally
// visible to the command": the RETURNING subquery runs on the statement's snapshot, taken
// before T1 committed, and still counts an empty table.
func TestIso_RC_InsertOnConflict(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS kv", "CREATE TABLE kv (k int primary key, v text)")
	t1, t2 := L.Session("T1"), L.Session("T2")

	t1.Do("BEGIN")
	t1.Do("INSERT INTO kv VALUES (1, 't1')")
	up := t2.Do("INSERT INTO kv VALUES (1, 't2') ON CONFLICT (k) DO UPDATE SET v = excluded.v RETURNING v, (SELECT count(*) FROM kv)").MustBlock()
	t1.Do("COMMIT")
	L.Note("T2 took the UPDATE path on T1's row, which its own snapshot cannot see")
	up.MustRows("(t2,0)")
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
// (The docs quote one message, "... due to concurrent update", for a row that was "updated
// or deleted". A committed DELETE reports "... due to concurrent delete"; the SQLSTATE is the same.)
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
		{"select-for-share", []string{"UPDATE r SET v = 1 WHERE id = 1"}, "COMMIT",
			"SELECT * FROM r WHERE id = 1 FOR SHARE", "could not serialize access due to concurrent update"},
		{"delete", []string{"UPDATE r SET v = 1 WHERE id = 1"}, "COMMIT",
			"DELETE FROM r WHERE id = 1", "could not serialize access due to concurrent update"},
		{"merge", []string{"UPDATE r SET v = 1 WHERE id = 1"}, "COMMIT",
			"MERGE INTO r USING (VALUES (1)) s(id) ON r.id = s.id WHEN MATCHED THEN UPDATE SET v = r.v + 1",
			"could not serialize access due to concurrent update"},
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

	// 13.3.2: "Within a REPEATABLE READ or SERIALIZABLE transaction, however, an error will be
	// thrown if a row to be locked has changed since the transaction started", also when the
	// change committed before the locking read, so there is nothing to wait for. FOR KEY SHARE
	// is the exception: it fails only if the key changed or the row was deleted. Over a
	// committed non-key UPDATE it succeeds and returns the snapshot's version of the row.
	for _, level := range []lab.Level{lab.RepeatableRead, lab.Serializable} {
		for _, c := range []struct{ change, mode string }{
			{"UPDATE r SET v = 1 WHERE id = 1", "FOR UPDATE"},
			{"UPDATE r SET v = 1 WHERE id = 1", "FOR NO KEY UPDATE"},
			{"UPDATE r SET v = 1 WHERE id = 1", "FOR SHARE"},
			{"UPDATE r SET v = 1 WHERE id = 1", "FOR KEY SHARE"},
			{"UPDATE r SET id = 3 WHERE id = 1", "FOR KEY SHARE"},
			{"DELETE FROM r WHERE id = 1", "FOR KEY SHARE"},
		} {
			name := fmt.Sprintf("changed-before/%s/%s/%s", level.Short(), strings.Fields(c.change)[0]+"-"+strings.Fields(c.change)[3], strings.ReplaceAll(c.mode, " ", "-"))
			t.Run(name, func(t *testing.T) {
				L := newLab(t)
				setupRows(L)
				t1, t2 := L.Session("T1"), L.Session("T2")
				t2.Begin(level)
				t2.Do("SELECT v FROM r WHERE id = 1").MustRows("(0)")
				t1.Do(c.change).MustAffect(1)
				op := t2.Do("SELECT v FROM r WHERE id = 1 " + c.mode).MustNotBlock()
				if c.mode == "FOR KEY SHARE" && strings.HasPrefix(c.change, "UPDATE r SET v") {
					op.MustRows("(0)")
					L.Note("the lock is on a row version T1 has replaced; writing the row still fails")
					t2.Do("UPDATE r SET v = 5 WHERE id = 1").MustFail(lab.IsSerialization)
				} else {
					op.MustFail(lab.IsSerialization)
				}
				t2.Do("ROLLBACK")
			})
		}
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

// 13.2.3: data read in a SERIALIZABLE transaction is not valid until the transaction has
// committed, "even for read-only transactions, except that data read within a deferrable
// read-only transaction is known to be valid as soon as it is read".
//
// The deposit report (Fekete's read-only anomaly, with the pivot committing early): T1 adds a
// receipt to batch 1 after reading the batch number, T2 closes batch 1. RO then sees batch 1
// closed. T1 can still commit (serial order T1 → T2), but after that no serial order has room
// for RO: it saw T2's work and not T1's.
//   - READ ONLY: what it already read (batch 1 is closed) turns out to be invalid; its next
//     read fails with 40001.
//   - READ ONLY DEFERRABLE: it waits for T1 at its first statement. T1's commit proves the
//     snapshot it had taken unsafe, so, unlike in TestIso_SER_ReadOnlyDeferrable, it takes a
//     new one: it sees T1's receipt and cannot fail.
func TestIso_SER_ReadOnlyCanFailDeferrableCannot(t *testing.T) {
	for _, deferrable := range []bool{false, true} {
		name := "read-only"
		if deferrable {
			name = "deferrable"
		}
		t.Run(name, func(t *testing.T) {
			L := newLab(t)
			L.Exec("DROP TABLE IF EXISTS control", "DROP TABLE IF EXISTS receipt",
				"CREATE TABLE control (deposit_no int)", "INSERT INTO control VALUES (1)",
				"CREATE TABLE receipt (deposit_no int, amount int)")
			t1, t2, ro := L.Session("T1"), L.Session("T2"), L.Session("RO")
			t1.Begin(lab.Serializable)
			t1.Do("SELECT deposit_no FROM control").MustRows("(1)")
			t1.Do("INSERT INTO receipt VALUES (1, 100)").MustOK()
			t2.Begin(lab.Serializable)
			t2.Do("UPDATE control SET deposit_no = 2").MustAffect(1)
			t2.Do("COMMIT").MustOK()

			const total = "SELECT coalesce(sum(amount), 0) FROM receipt WHERE deposit_no = 1"
			if deferrable {
				ro.Do("BEGIN ISOLATION LEVEL SERIALIZABLE READ ONLY DEFERRABLE")
				batch := ro.Do("SELECT deposit_no FROM control").MustBlock()
				t1.Do("COMMIT").MustOK()
				batch.MustRows("(2)")
				ro.Do(total).MustRows("(100)")
				ro.Do("COMMIT").MustOK()
				return
			}
			ro.Do("BEGIN ISOLATION LEVEL SERIALIZABLE READ ONLY")
			ro.Do("SELECT deposit_no FROM control").MustNotBlock().MustRows("(2)")
			t1.Do("COMMIT").MustOK()
			L.Note("batch 1 is closed, so its total should be final; a 0 here would be wrong forever")
			ro.Do(total).MustFail(func(err error) bool {
				return lab.IsSerialization(err) && hasMessage("read/write dependencies among transactions")(err)
			})
			ro.Do("ROLLBACK")
			L.Note("the retry sees T1's receipt")
			ro.Do("BEGIN ISOLATION LEVEL SERIALIZABLE READ ONLY")
			ro.Do(total).MustRows("(100)")
			ro.Do("COMMIT").MustOK()
		})
	}
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
		{"SELECT * FROM tl FOR NO KEY UPDATE", "tl", "RowShareLock"},
		{"SELECT * FROM tl FOR SHARE", "tl", "RowShareLock"},
		{"SELECT * FROM tl FOR KEY SHARE", "tl", "RowShareLock"},
		{"SELECT * FROM tl, tl2 FOR UPDATE OF tl", "tl2", "AccessShareLock"},
		{"UPDATE tl SET v = (SELECT count(*) FROM tl2)", "tl2", "AccessShareLock"},
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
		{"ALTER INDEX tl_pkey RENAME TO tl_pk2", "tl_pkey", "ShareUpdateExclusiveLock"},
		{"ALTER INDEX tl_pkey SET TABLESPACE pg_default", "tl_pkey", "AccessExclusiveLock"},
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
//   - any other UPDATE (including of a column with only a PARTIAL or EXPRESSION unique
//     index) → FOR NO KEY UPDATE
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
		{"UPDATE rl SET e = 7 WHERE id = 1", "No Key Update", false, true},
		{"UPDATE rl SET u = 7 WHERE id = 1", "Update", true, true},
		{"DELETE FROM rl WHERE id = 1", "Update", true, true},
	}
	for _, c := range cases {
		t.Run(strings.Fields(c.stmt)[0]+"-"+strings.Fields(c.stmt)[3], func(t *testing.T) {
			L := newLab(t)
			L.Exec("DROP TABLE IF EXISTS rl",
				"CREATE TABLE rl (id int primary key, u int unique, p int, v int, e int)",
				"CREATE UNIQUE INDEX rl_p ON rl (p) WHERE p > 0",
				"CREATE UNIQUE INDEX rl_e ON rl ((e + 0))",
				"INSERT INTO rl VALUES (1, 1, 1, 0, 1)")
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

// 13.3.5: a locking function must only run on the rows you mean to lock. The docs give three
// queries: "ok", "danger!" (the LIMIT "is not guaranteed to be applied before the locking
// function is executed") and "ok" (LIMIT in a subquery). They run here verbatim, on a foo
// with ids 12301..13300.
//
// PG 18 does apply the LIMIT first: a volatile function in the SELECT list is evaluated
// after LIMIT, and after ORDER BY ... LIMIT too, so the "danger!" query locks only its 100
// rows. The danger is real with the function in WHERE: there it runs for every row the scan
// visits, before ORDER BY ... LIMIT picks one. That is the classic job-queue bug, and the
// locks on the other 954 rows stay until the session ends.
func TestLock_AdvisoryLockFilterPitfall(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS foo", "CREATE TABLE foo (id int primary key, prio int)",
		"INSERT INTO foo SELECT g, g FROM generate_series(12301, 13300) g", "ANALYZE foo")
	s := L.Session("W")
	for _, c := range []struct {
		what, sql string
		held      int // advisory locks the session holds afterwards
	}{
		{"docs: ok", "SELECT pg_advisory_lock(id) FROM foo WHERE id = 12345", 1},
		{"docs: danger!", "SELECT pg_advisory_lock(id) FROM foo WHERE id > 12345 LIMIT 100", 100},
		{"docs: ok", "SELECT pg_advisory_lock(q.id) FROM (SELECT id FROM foo WHERE id > 12345 LIMIT 100) q", 100},
		{"with a sort before the LIMIT", "SELECT pg_advisory_lock(id) FROM foo WHERE id > 12345 ORDER BY prio DESC LIMIT 100", 100},
		{"lock in WHERE", "SELECT id FROM foo WHERE id > 12345 AND pg_try_advisory_lock(id) ORDER BY prio LIMIT 1", 955},
		{"lock in WHERE, LIMIT first", "SELECT q.id FROM (SELECT id FROM foo WHERE id > 12345 ORDER BY prio LIMIT 1) q WHERE pg_try_advisory_lock(q.id)", 1},
	} {
		L.Quiet() // up to 100 result rows per query
		s.Do(c.sql).MustOK()
		held := s.Do("SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND pid = pg_backend_pid()").Int()
		s.Do("SELECT pg_advisory_unlock_all()")
		L.Loud()
		L.Note("%-28s  %3d lock(s) held   %s", c.what, held, c.sql)
		if held != c.held {
			t.Errorf("%s: %d advisory locks held, want %d: %s", c.what, held, c.held, c.sql)
		}
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

// 13.4.2: the docs' bank check: does the sum of credits in one table equal the sum of debits
// in another, while both are being updated? Two successive SELECT sum(...) "will not work
// reliably in Read Committed mode, since the second query will likely include the results of
// transactions not counted by the first". In one REPEATABLE READ transaction both sums come
// from the same snapshot.
func TestIso_SumCheckAcrossTwoTables(t *testing.T) {
	for _, c := range []struct {
		level  lab.Level
		debits string
	}{{lab.ReadCommitted, "(150)"}, {lab.RepeatableRead, "(100)"}} {
		t.Run(c.level.Short(), func(t *testing.T) {
			L := newLab(t)
			L.Exec("DROP TABLE IF EXISTS credits", "DROP TABLE IF EXISTS debits",
				"CREATE TABLE credits (amount int)", "CREATE TABLE debits (amount int)",
				"INSERT INTO credits VALUES (100)", "INSERT INTO debits VALUES (100)")
			chk, wr := L.Session("CHK"), L.Session("WR")
			chk.Begin(c.level)
			chk.Do("SELECT sum(amount) FROM credits").MustRows("(100)")
			wr.Do("BEGIN")
			wr.Do("INSERT INTO credits VALUES (50)")
			wr.Do("INSERT INTO debits VALUES (50)")
			wr.Do("COMMIT")
			L.Note("the books balance before and after WR's transaction (100 = 100, 150 = 150)")
			chk.Do("SELECT sum(amount) FROM debits").MustRows(c.debits)
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
// REPEATABLE READ or SERIALIZABLE snapshot can be queried (its committed rows stay
// invisible), while a query on pg_class itself, which does use the snapshot, does not show it.
func TestMVCC_CatalogAccessIgnoresSnapshot(t *testing.T) {
	for _, level := range []lab.Level{lab.RepeatableRead, lab.Serializable} {
		t.Run(level.Short(), func(t *testing.T) {
			L := newLab(t)
			L.Exec("DROP TABLE IF EXISTS newt")
			rd, dd := L.Session("RD"), L.Session("DDL")
			rd.Begin(level)
			rd.Do("SELECT 1")
			dd.Do("CREATE TABLE newt (id int)")
			dd.Do("INSERT INTO newt VALUES (1)")
			rd.Do("SELECT count(*) FROM newt").MustRows("(0)")
			rd.Do("SELECT count(*) FROM pg_class WHERE relname = 'newt'").MustRows("(0)")
			rd.Do("COMMIT")
		})
	}
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

// 13.2.1: a READ COMMITTED query sees the database as of the instant it began. A change
// committed while the query is still running stays invisible to it. The query is held up in
// the middle by an advisory lock, and its subquery is correlated with the lock call, so it
// reads r only after the lock is granted. The control: a VOLATILE function in the same query
// takes a new snapshot for each statement it runs, and does see the change.
func TestIso_RC_QueryKeepsItsSnapshotWhileRunning(t *testing.T) {
	L := newLab(t)
	setupRows(L)
	L.Exec("CREATE OR REPLACE FUNCTION r_v1() RETURNS int VOLATILE LANGUAGE sql AS 'SELECT v FROM r WHERE id = 1'")
	rd, w := L.Session("RD"), L.Session("W")
	w.Do("SELECT pg_advisory_lock(7)")
	sel := rd.Do(`SELECT (SELECT v FROM r WHERE id = 1 AND l.x IS NOT NULL) AS query_snapshot, r_v1() AS new_snapshot
FROM (SELECT pg_advisory_xact_lock(7) AS x OFFSET 0) l`).MustBlock()
	w.Do("UPDATE r SET v = 1 WHERE id = 1").MustOK()
	w.Do("SELECT pg_advisory_unlock(7)")
	sel.MustRows("(0,1)")
	rd.Do("SELECT v FROM r WHERE id = 1").MustRows("(1)")
}

// 13.2.1: a waiting UPDATE "will only find target rows that were committed as of the command
// start time". Once unblocked it re-checks the row it waited for, but does not look for new
// ones: the matching row T1 inserted is left alone. "Subsequent commands in the same
// transaction will see the effects of the committed concurrent transaction in any case."
func TestIso_RC_WaitDoesNotFindNewRows(t *testing.T) {
	L := newLab(t)
	setupRows(L)
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("BEGIN")
	t1.Do("UPDATE r SET v = 1 WHERE id = 1")
	t1.Do("INSERT INTO r VALUES (3, 1)")
	t2.Do("BEGIN")
	upd := t2.Do("UPDATE r SET v = v + 100 WHERE v < 10").MustBlock()
	t1.Do("COMMIT")
	upd.MustAffect(2)
	t2.Do("SELECT id, v FROM r ORDER BY id").MustRows("(1,101) (2,100) (3,1)")
	t2.Do("UPDATE r SET v = v + 100 WHERE v < 10").MustAffect(1)
	t2.Do("COMMIT")
}

// 13.2.1 / 13.3.2: SELECT FOR UPDATE / FOR SHARE that waited for a concurrent writer locks
// and returns the UPDATED version of the row if it still matches, and no row if it was
// deleted or no longer matches.
func TestIso_RC_LockingReadReturnsUpdatedRow(t *testing.T) {
	for _, mode := range []string{"FOR UPDATE", "FOR SHARE"} {
		for _, c := range []struct{ name, first, want string }{
			{"updated", "UPDATE r SET v = 5 WHERE id = 1", "(5)"},
			{"no-longer-matches", "UPDATE r SET v = 50 WHERE id = 1", "(no rows)"},
			{"deleted", "DELETE FROM r WHERE id = 1", "(no rows)"},
		} {
			t.Run(strings.ReplaceAll(mode, " ", "-")+"/"+c.name, func(t *testing.T) {
				L := newLab(t)
				setupRows(L)
				t1, t2 := L.Session("T1"), L.Session("T2")
				t1.Do("BEGIN")
				t1.Do(c.first)
				t2.Do("BEGIN")
				sel := t2.Do("SELECT v FROM r WHERE id = 1 AND v < 10 " + mode).MustBlock()
				t1.Do("COMMIT")
				sel.MustRows(c.want)
				t2.Do("COMMIT")
			})
		}
	}
}

// 13.2.1: the ON CONFLICT guarantees hold "only in Read Committed mode". In REPEATABLE READ
// a conflict with a row the snapshot cannot see is a serialization failure, for DO UPDATE
// and DO NOTHING alike.
func TestIso_RR_InsertOnConflictFails(t *testing.T) {
	for _, action := range []string{"DO UPDATE SET v = excluded.v", "DO NOTHING"} {
		t.Run(strings.ReplaceAll(action[:strings.Index(action+" SET", " SET")], " ", "-"), func(t *testing.T) {
			L := newLab(t)
			L.Exec("DROP TABLE IF EXISTS kv", "CREATE TABLE kv (k int primary key, v text)")
			t1, t2 := L.Session("T1"), L.Session("T2")
			t2.Do("BEGIN ISOLATION LEVEL REPEATABLE READ")
			t2.Do("SELECT count(*) FROM kv").MustRows("(0)")
			t1.Do("INSERT INTO kv VALUES (1, 't1')")
			t2.Do("INSERT INTO kv VALUES (1, 't2') ON CONFLICT (k) " + action).MustFail(lab.IsSerialization)
			t2.Do("ROLLBACK")
		})
	}
}

// 13.2.1: what MERGE does in READ COMMITTED when the row it matched changes while it waits.
//   - still joins: the WHEN MATCHED conditions are re-evaluated from the FIRST action, so a
//     different action than the one originally chosen can run;
//   - no longer joins (key changed): the NOT MATCHED BY SOURCE and NOT MATCHED actions run;
//   - deleted: the NOT MATCHED action runs.
func TestIso_RC_MergeConcurrentChange(t *testing.T) {
	for _, c := range []struct{ name, first, merge, final string }{
		{"still-joins-reevaluates-from-first-action", "UPDATE m SET v = 10 WHERE k = 1",
			`MERGE INTO m USING (VALUES (1)) s(k) ON m.k = s.k
WHEN MATCHED AND m.v >= 10 THEN DELETE
WHEN MATCHED THEN UPDATE SET v = m.v + 1`, "(no rows)"},
		{"join-fails-runs-not-matched-actions", "UPDATE m SET k = 2 WHERE k = 1",
			`MERGE INTO m USING (VALUES (1)) s(k) ON m.k = s.k
WHEN MATCHED THEN UPDATE SET v = 99
WHEN NOT MATCHED BY SOURCE THEN UPDATE SET v = 77
WHEN NOT MATCHED THEN INSERT VALUES (s.k, -1)`, "(1,-1) (2,77)"},
		{"deleted-runs-not-matched-action", "DELETE FROM m WHERE k = 1",
			`MERGE INTO m USING (VALUES (1)) s(k) ON m.k = s.k
WHEN MATCHED THEN UPDATE SET v = 99
WHEN NOT MATCHED THEN INSERT VALUES (s.k, -1)`, "(1,-1)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			L := newLab(t)
			L.Exec("DROP TABLE IF EXISTS m", "CREATE TABLE m (k int primary key, v int)", "INSERT INTO m VALUES (1, 0)")
			t1, t2 := L.Session("T1"), L.Session("T2")
			t1.Do("BEGIN")
			t1.Do(c.first)
			merge := t2.Do(c.merge).MustBlock()
			t1.Do("COMMIT")
			merge.MustOK()
			t2.Do("SELECT k, v FROM m ORDER BY k").MustRows(c.final)
		})
	}
}

// 13.2.2: a REPEATABLE READ query sees the transaction's own uncommitted changes on top of
// its snapshot.
func TestIso_RR_SeesOwnChanges(t *testing.T) {
	L := newLab(t)
	setupRows(L)
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("BEGIN ISOLATION LEVEL REPEATABLE READ")
	t1.Do("UPDATE r SET v = 1 WHERE id = 1")
	t2.Do("UPDATE r SET v = 2 WHERE id = 2")
	t1.Do("SELECT v FROM r ORDER BY id").MustRows("(1) (0)")
	t1.Do("COMMIT")
}

// 13.2.3: SIRead predicate locks cover what was actually read, and fine-grained locks are
// combined into coarser ones: more than max_pred_locks_per_page tuple locks on one page
// become a page lock, and more than max_pred_locks_per_relation page or tuple locks on one
// relation become a relation lock.
func TestIso_SER_PredicateLockPromotion(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS pl", "CREATE TABLE pl (id int primary key, pad text)",
		"INSERT INTO pl SELECT g, repeat('x', 500) FROM generate_series(1, 2000) g", "ANALYZE pl")
	L.Show("SELECT current_setting('max_pred_locks_per_page') AS per_page, current_setting('max_pred_locks_per_relation') AS per_relation, current_setting('max_pred_locks_per_transaction') AS per_transaction")
	if L.QueryString("SELECT current_setting('max_pred_locks_per_page') || ',' || current_setting('max_pred_locks_per_relation') || ',' || current_setting('max_pred_locks_per_transaction')") != "2,-2,64" {
		t.Skip("the thresholds below assume the default max_pred_locks_* settings")
	}
	s := L.Session("T1")
	s.Do("SET enable_seqscan = off")
	s.Do("SET enable_bitmapscan = off")
	heapLocks := func() string {
		return s.Do(`SELECT string_agg(locktype || ' x' || n, ', ' ORDER BY locktype) FROM (
  SELECT locktype, count(*) AS n FROM pg_locks
  WHERE mode = 'SIReadLock' AND pid = pg_backend_pid() AND relation = 'pl'::regclass GROUP BY 1) x`).Str()
	}
	expect := func(want string) {
		t.Helper()
		if got := heapLocks(); got != want {
			t.Fatalf("SIRead locks on pl: %s, want %s", got, want)
		}
	}
	s.Do("BEGIN ISOLATION LEVEL SERIALIZABLE")
	s.Do("SELECT length(pad) FROM pl WHERE id = 1")
	expect("tuple x1")
	s.Do("SELECT length(pad) FROM pl WHERE id = 2")
	expect("tuple x2")
	L.Note("a third tuple on the same page exceeds max_pred_locks_per_page = 2")
	s.Do("SELECT length(pad) FROM pl WHERE id = 3")
	expect("page x1")
	L.Note("one row from each of many other pages exceeds max_pred_locks_per_relation (64 / 2 = 32)")
	s.Do("SELECT count(pad) FROM pl WHERE id = ANY (SELECT g * 40 FROM generate_series(1, 40) g)")
	expect("relation x1")
	s.Do("COMMIT")
}

// 13.2.3: a READ ONLY serializable transaction that starts while no read-write serializable
// transaction is active takes no predicate locks at all. One that starts while such a
// transaction is active takes them, and "may be able to release its SIRead locks before
// completion": they are gone at its next read after that transaction ended. A read-write one
// must keep its SIRead locks past its own COMMIT, until the transactions that overlapped it
// are done.
func TestIso_SER_PredicateLockLifetime(t *testing.T) {
	L := newLab(t)
	setupRows(L)
	sireads := func() string {
		return L.QueryString("SELECT count(*) FROM pg_locks WHERE mode = 'SIReadLock' AND relation IN ('r'::regclass, 'r_pkey'::regclass)")
	}
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("BEGIN ISOLATION LEVEL SERIALIZABLE READ ONLY")
	t1.Do("SELECT * FROM r").MustOK()
	if n := sireads(); n != "0" {
		t.Fatalf("READ ONLY transaction with no concurrent writer holds %s SIRead locks, want 0", n)
	}
	t1.Do("COMMIT")

	L.Note("a READ ONLY transaction that overlaps a read-write one does take them, and releases\n" +
		"them before it completes, once that transaction is gone and no conflict can still occur")
	t2.Do("BEGIN ISOLATION LEVEL SERIALIZABLE")
	t2.Do("SELECT 1")
	t1.Do("BEGIN ISOLATION LEVEL SERIALIZABLE READ ONLY")
	t1.Do("SELECT * FROM r").MustOK()
	if sireads() == "0" {
		t.Fatal("READ ONLY transaction that overlaps a read-write one took no SIRead locks")
	}
	t2.Do("COMMIT")
	t1.Do("SELECT * FROM r").MustOK()
	if n := sireads(); n != "0" {
		t.Fatalf("READ ONLY transaction still holds %s SIRead locks after the overlapping transaction ended", n)
	}
	t1.Do("COMMIT")

	t2.Do("BEGIN ISOLATION LEVEL SERIALIZABLE")
	t2.Do("SELECT 1")
	t1.Do("BEGIN ISOLATION LEVEL SERIALIZABLE")
	t1.Do("SELECT * FROM r").MustOK()
	if sireads() == "0" {
		t.Fatal("read-write transaction took no SIRead locks")
	}
	t1.Do("COMMIT")
	L.Note("T1 committed, but T2 overlapped it and is still running")
	L.ShowAs("pg_locks (SIReadLock)", "SELECT locktype, relation::regclass, pid FROM pg_locks WHERE mode = 'SIReadLock'")
	if sireads() == "0" {
		t.Fatal("SIRead locks were released at COMMIT although an overlapping transaction is still active")
	}
	t2.Do("COMMIT")
	if n := sireads(); n != "0" {
		t.Fatalf("%s SIRead locks left after the last overlapping transaction ended", n)
	}
}

// 13.2.3: idle_in_transaction_session_timeout disconnects a session that sits idle inside a
// transaction; its locks are released and its work is rolled back.
func TestTx_IdleInTransactionSessionTimeout(t *testing.T) {
	L := newLab(t)
	setupRows(L)
	idle, other := L.Session("IDLE"), L.Session("O")
	idle.Do("SET idle_in_transaction_session_timeout = '300ms'")
	idle.Do("BEGIN")
	idle.Do("UPDATE r SET v = 1 WHERE id = 1")
	waitBackendGone(t, L, idle.ID)
	other.Do("UPDATE r SET v = v + 10 WHERE id = 1").MustNotBlock().MustAffect(1)
	other.Do("SELECT v FROM r WHERE id = 1").MustRows("(10)")
}

func waitBackendGone(t *testing.T, L *lab.Lab, pid int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for L.QueryString("SELECT count(*) FROM pg_stat_activity WHERE pid = $1", pid) != "0" {
		if time.Now().After(deadline) {
			t.Fatalf("backend %d is still connected", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 13.3.1: the commands that cannot run inside a transaction block. Their lock is read from
// pg_locks while they wait for it behind a SHARE UPDATE EXCLUSIVE lock (which lets VACUUM's
// initial ACCESS SHARE lookup through).
func TestLock_TableLockRequestedByNonTransactionalCommands(t *testing.T) {
	for _, c := range []struct{ sql, want string }{
		{"VACUUM tl", "ShareUpdateExclusiveLock"},
		{"CREATE INDEX CONCURRENTLY tl_c ON tl (v)", "ShareUpdateExclusiveLock"},
		{"REINDEX TABLE CONCURRENTLY tl", "ShareUpdateExclusiveLock"},
		{"VACUUM FULL tl", "AccessExclusiveLock"},
	} {
		t.Run(strings.ReplaceAll(c.sql, " ", "_"), func(t *testing.T) {
			L := newLab(t)
			L.Exec("DROP TABLE IF EXISTS tl2", "DROP TABLE IF EXISTS tl CASCADE",
				"CREATE TABLE tl (id int primary key, v int)", "INSERT INTO tl VALUES (1, 0)")
			t1, t2 := L.Session("T1"), L.Session("T2")
			t1.Do("BEGIN")
			t1.Do("LOCK TABLE tl IN SHARE UPDATE EXCLUSIVE MODE")
			op := t2.Do(c.sql).MustBlock()
			got := L.QueryString("SELECT mode FROM pg_locks WHERE pid = $1 AND NOT granted AND relation = 'tl'::regclass", t2.ID)
			if got != c.want {
				t.Errorf("%s waits for %s, want %s", c.sql, got, c.want)
			}
			t1.Do("ROLLBACK")
			op.MustOK()
		})
	}
}

// 13.3.1: an error that escapes a PL/pgSQL exception block releases the locks taken inside
// the block, like ROLLBACK TO SAVEPOINT does. Without the error the lock is kept.
func TestLock_TableLockReleasedByExceptionBlock(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS tl2", "DROP TABLE IF EXISTS tl CASCADE", "CREATE TABLE tl (id int primary key)")
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("BEGIN")
	t1.Do(`DO $$ BEGIN
  LOCK TABLE tl IN ACCESS EXCLUSIVE MODE;
  RAISE EXCEPTION 'leave the block';
EXCEPTION WHEN OTHERS THEN NULL;
END $$`).MustOK()
	t2.Do("SELECT count(*) FROM tl").MustNotBlock().MustOK()
	t1.Do(`DO $$ BEGIN
  LOCK TABLE tl IN ACCESS EXCLUSIVE MODE;
EXCEPTION WHEN OTHERS THEN NULL;
END $$`).MustOK()
	sel := t2.Do("SELECT count(*) FROM tl").MustBlock()
	t1.Do("COMMIT")
	sel.MustOK()
}

// 13.3.5: session-level advisory locks are cleaned up by the server when the session ends.
func TestLock_AdvisoryReleasedAtSessionEnd(t *testing.T) {
	L := newLab(t)
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("SELECT pg_advisory_lock(9)")
	t2.Do("SELECT pg_try_advisory_lock(9)").MustRows("(false)")
	L.Exec(fmt.Sprintf("SELECT pg_terminate_backend(%d)", t1.ID))
	waitBackendGone(t, L, t1.ID)
	t2.Do("SELECT pg_try_advisory_lock(9)").MustRows("(true)")
	t2.Do("SELECT pg_advisory_unlock(9)")
}

// 13.3.2: what each shared row lock stops other transactions' writes from doing.
// FOR KEY SHARE blocks DELETE and an UPDATE that changes the key, but not other UPDATEs;
// FOR SHARE blocks every UPDATE and DELETE.
func TestLock_SharedRowLocksBlockWriters(t *testing.T) {
	for _, c := range []struct {
		mode, write string
		blocks      bool
	}{
		{"FOR KEY SHARE", "UPDATE r SET v = 1 WHERE id = 1", false},
		{"FOR KEY SHARE", "UPDATE r SET id = 100 WHERE id = 1", true},
		{"FOR KEY SHARE", "DELETE FROM r WHERE id = 1", true},
		{"FOR SHARE", "UPDATE r SET v = 1 WHERE id = 1", true},
		{"FOR SHARE", "DELETE FROM r WHERE id = 1", true},
	} {
		t.Run(strings.ReplaceAll(c.mode+"/"+strings.Join(strings.Fields(c.write)[:4], " "), " ", "-"), func(t *testing.T) {
			L := newLab(t)
			setupRows(L)
			t1, t2 := L.Session("T1"), L.Session("T2")
			t1.Do("BEGIN")
			t1.Do("SELECT * FROM r WHERE id = 1 " + c.mode)
			op := t2.Do(c.write)
			if c.blocks {
				op.MustBlock()
			} else {
				op.MustNotBlock()
			}
			t1.Do("COMMIT")
			op.MustAffect(1)
		})
	}
}

// 13.5: "In cases involving a conflicting prepared transaction, it may not be possible to
// make progress until the prepared transaction commits or rolls back." A prepared
// transaction belongs to no session, yet keeps its row locks and its place in SSI's
// dependency graph: a retry of the transaction that conflicts with it fails every time, and
// a plain writer of the row it changed waits, until COMMIT PREPARED.
func TestTx_PreparedTransactionBlocksRetries(t *testing.T) {
	L := newLab(t)
	if L.QueryString("SHOW max_prepared_transactions") == "0" {
		t.Skip("max_prepared_transactions = 0")
	}
	const gid = "lab_prepared"
	rollbackPrepared := func() { _, _ = L.Admin.Exec("ROLLBACK PREPARED '" + gid + "'") }
	rollbackPrepared() // left over from an interrupted run; it would block DROP TABLE
	t.Cleanup(rollbackPrepared)
	setupRows(L)
	t1, t2, w := L.Session("T1"), L.Session("T2"), L.Session("W")

	// The write skew of TestIso_WriteSkew_OnCallDoctors: each reads both rows, each updates one.
	t1.Begin(lab.Serializable)
	t1.Do("SELECT sum(v) FROM r").MustRows("(0)")
	t1.Do("UPDATE r SET v = 1 WHERE id = 1")
	t1.Do("PREPARE TRANSACTION '" + gid + "'").MustOK()
	L.Note("T1's session is free again; the transaction lives on in pg_prepared_xacts")
	t1.Do("SELECT gid FROM pg_prepared_xacts WHERE gid = '" + gid + "'").MustRows("(" + gid + ")")

	attempt := func() []*lab.Op {
		t2.Begin(lab.Serializable)
		return []*lab.Op{
			t2.Do("SELECT sum(v) FROM r"),
			t2.Do("UPDATE r SET v = 1 WHERE id = 2"),
			t2.Do("COMMIT"),
		}
	}
	for i := 1; i <= 3; i++ {
		failed := false
		for _, op := range attempt() {
			if op.Wait(); op.Err != nil && !op.Skipped {
				failed = true
				if !lab.IsSerialization(op.Err) {
					t.Fatalf("attempt %d: %q failed with %s, want 40001", i, op.SQL, lab.Describe(op.Err))
				}
			}
		}
		if !failed {
			t.Fatalf("attempt %d committed although it conflicts with the prepared transaction", i)
		}
	}
	L.Note("three attempts, three serialization failures: retrying cannot help while T1 is prepared")

	upd := w.Do("UPDATE r SET v = v + 10 WHERE id = 1").MustBlock()
	L.ShowLocks()
	t1.Do("COMMIT PREPARED '" + gid + "'").MustOK()
	upd.MustAffect(1)
	for _, op := range attempt() {
		op.MustOK()
	}
	t2.Do("SELECT id, v FROM r ORDER BY id").MustRows("(1,11) (2,1)")
}
