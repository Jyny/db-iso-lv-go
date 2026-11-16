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

	L.Note(`"even when providing the strictest level of transaction isolation through the use of
an innovative Serializable Snapshot Isolation (SSI) level": SIRead locks never block`)
	rd.Do("BEGIN ISOLATION LEVEL SERIALIZABLE")
	w.Do("BEGIN ISOLATION LEVEL SERIALIZABLE")
	rd.Do("SELECT v FROM r WHERE id = 1").MustRows("(4)")
	w.Do("UPDATE r SET v = 5 WHERE id = 1").MustNotBlock().MustOK()
	rd.Do("SELECT v FROM r WHERE id = 1").MustNotBlock().MustRows("(4)")
	w.Do("COMMIT").MustOK()
	rd.Do("COMMIT").MustOK()
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

// 13.2.1: an UPDATE or DELETE that waited for a concurrent writer decides what to do from
// that writer's outcome: rollback → proceed with the original row; committed DELETE → skip
// the row; committed UPDATE → re-check WHERE against the new version.
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
	for _, verb := range []string{"UPDATE", "DELETE"} {
		for _, c := range cases {
			t.Run(verb+"/"+c.name, func(t *testing.T) {
				stmt, final := "UPDATE r SET v = v + 100 WHERE id = 1 AND v < 10", c.final
				if verb == "DELETE" {
					stmt = "DELETE FROM r WHERE id = 1 AND v < 10"
					if c.affected == 1 {
						final = "(no rows)"
					}
				}
				L := newLab(t)
				setupRows(L)
				t1, t2 := L.Session("T1"), L.Session("T2")
				t1.Do("BEGIN")
				t1.Do(c.first)
				w := t2.Do(stmt).MustBlock()
				t1.Do(c.end)
				w.MustAffect(c.affected)
				t2.Do("SELECT v FROM r WHERE id = 1").MustRows(final)
			})
		}
	}
}

// 13.2.1: INSERT ... ON CONFLICT DO UPDATE always ends in an insert or an update, even when
// the conflicting row is uncommitted when it starts; DO NOTHING skips it. "[T]he UPDATE clause
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

	L.Note(`"each row proposed for insertion will either insert or update": if T1 rolls back, the insert path`)
	t1.Do("BEGIN")
	t1.Do("INSERT INTO kv VALUES (3, 't1')")
	up = t2.Do("INSERT INTO kv VALUES (3, 't2') ON CONFLICT (k) DO UPDATE SET v = excluded.v || '-upd' RETURNING v").MustBlock()
	t1.Do("ROLLBACK")
	up.MustRows("(t2)")
	t1.Do("BEGIN")
	t1.Do("INSERT INTO kv VALUES (4, 't1')")
	skip = t2.Do("INSERT INTO kv VALUES (4, 't2') ON CONFLICT DO NOTHING").MustBlock()
	t1.Do("ROLLBACK")
	skip.MustAffect(1)
	t2.Do("SELECT v FROM kv WHERE k = 4").MustRows("(t2)")
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
// "If either transaction were running at the Repeatable Read isolation level, both would be
// allowed to commit": the mixed subtests run one side at each level.
func TestIso_SER_MytabDocsExample(t *testing.T) {
	for _, c := range []struct{ a, b lab.Level }{
		{lab.Serializable, lab.RepeatableRead}, {lab.RepeatableRead, lab.Serializable},
	} {
		t.Run("A="+c.a.Short()+",B="+c.b.Short(), func(t *testing.T) {
			L := newLab(t)
			L.Exec("DROP TABLE IF EXISTS mytab", "CREATE TABLE mytab (class int, value int)",
				"INSERT INTO mytab VALUES (1, 10), (1, 20), (2, 100), (2, 200)")
			a, b := L.Session("A"), L.Session("B")
			a.Begin(c.a)
			b.Begin(c.b)
			a.Do("SELECT SUM(value) FROM mytab WHERE class = 1").MustRows("(30)")
			b.Do("SELECT SUM(value) FROM mytab WHERE class = 2").MustRows("(300)")
			a.Do("INSERT INTO mytab VALUES (2, 30)").MustNotBlock()
			b.Do("INSERT INTO mytab VALUES (1, 300)").MustNotBlock()
			a.Do("COMMIT").MustOK()
			b.Do("COMMIT").MustOK()
			a.Do("SELECT class, value FROM mytab ORDER BY class, value").
				MustRows("(1,10) (1,20) (1,300) (2,30) (2,100) (2,200)")
		})
	}
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

	L.Note("DEFERRABLE has no effect at REPEATABLE READ: no wait")
	ro.Do("BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY DEFERRABLE")
	ro.Do("SELECT sum(v) FROM r").MustNotBlock().MustRows("(0)")
	ro.Do("COMMIT")

	ro.Do("BEGIN ISOLATION LEVEL SERIALIZABLE READ ONLY DEFERRABLE")
	sel := ro.Do("SELECT sum(v) FROM r").MustBlock()
	wait := L.ShowAs("pg_stat_activity", "SELECT application_name, wait_event_type, wait_event FROM pg_stat_activity WHERE application_name = 'lab:RO'")
	if got := lab.FormatRows(wait); got != "(lab:RO,IPC,SafeSnapshot)" {
		t.Errorf("the deferrable transaction waits as %s, want (lab:RO,IPC,SafeSnapshot)", got)
	}
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

// 13.2.3 and 13.5: the check only turns 23505 into 40001 when every inserter follows the
// protocol, at SERIALIZABLE.
//   - 13.2.3: "If some Serializable transactions insert new keys directly without following
//     this protocol, unique constraints violations might be reported even in cases where they
//     could not occur in a serial execution". T2 checked and found no key, yet gets 23505
//     because T1 inserted without checking. In either serial order T2 would not fail: after
//     T1 its check finds the key and it skips the insert.
//   - 13.5: at READ COMMITTED / REPEATABLE READ the same check-then-insert is "effectively a
//     serialization failure, but the server will not detect it as such because it cannot
//     'see' the connection between the inserted value and the previous reads": 23505.
func TestIso_CheckBeforeInsertNeedsEveryInserterAndSerializable(t *testing.T) {
	for _, c := range []struct {
		name           string
		level          lab.Level
		check1, check2 bool
	}{
		{"SER/first-inserter-skips-check", lab.Serializable, false, true},
		{"SER/second-inserter-skips-check", lab.Serializable, true, false},
		{"RC/both-check", lab.ReadCommitted, true, true},
		{"RR/both-check", lab.RepeatableRead, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			L := newLab(t)
			L.Exec("DROP TABLE IF EXISTS users", "CREATE TABLE users (username text primary key)")
			t1, t2 := L.Session("T1"), L.Session("T2")
			for i, s := range []*lab.Session{t1, t2} {
				s.Begin(c.level)
				if []bool{c.check1, c.check2}[i] {
					s.Do("SELECT 1 FROM users WHERE username = 'alice'").MustRows("(no rows)")
				}
			}
			t1.Do("INSERT INTO users VALUES ('alice')")
			ins := t2.Do("INSERT INTO users VALUES ('alice')").MustBlock()
			t1.Do("COMMIT")
			ins.MustFail(lab.IsUniqueViolation)
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
	t1, t2, t3 := L.Session("T1"), L.Session("T2"), L.Session("T3")
	t1.Do("BEGIN")
	t1.Do("SELECT * FROM r WHERE id = 1 FOR SHARE")
	t1.Do("SAVEPOINT a")
	t1.Do("SELECT * FROM r WHERE id = 1 FOR UPDATE").MustNotBlock().MustOK()
	L.Show("SELECT locked_row, multi, modes FROM pgrowlocks('r')")
	L.Note("FOR SHARE is compatible with T1's FOR SHARE, so only the FOR UPDATE can make it fail")
	t3.Do("SELECT 1 FROM r WHERE id = 1 FOR SHARE NOWAIT").MustFail(lab.IsLockNotAvailable)
	t1.Do("ROLLBACK TO SAVEPOINT a")
	L.Note("the FOR UPDATE is released: FOR SHARE now succeeds at once")
	t3.Do("SELECT 1 FROM r WHERE id = 1 FOR SHARE NOWAIT").MustOK()
	L.Note("but the FOR SHARE taken before the savepoint still blocks an UPDATE")
	upd := t2.Do("UPDATE r SET v = 1 WHERE id = 1").MustBlock()
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

	L.Note(`"regardless of whether the existing lock hold and new request are at session level or
transaction level": now the hold is transaction-level and the new request session-level`)
	t1.Do("BEGIN")
	t1.Do("SELECT pg_advisory_xact_lock(5)")
	y := t2.Do("SELECT pg_advisory_lock(5)").MustBlock()
	t1.Do("SELECT pg_advisory_lock(5)").MustNotBlock().MustOK()
	t1.Do("COMMIT")
	L.Note("COMMIT ended only the transaction-level hold; the session-level one still keeps T2 waiting")
	L.Settle()
	y.MustBlock()
	t1.Do("SELECT pg_advisory_unlock(5)").MustRows("(true)")
	y.MustOK()
	t2.Do("SELECT pg_advisory_unlock(5)").MustRows("(true)")

	L.Note("a transaction-level lock is released by ROLLBACK as well")
	t1.Do("BEGIN")
	t1.Do("SELECT pg_advisory_xact_lock(6)")
	t1.Do("ROLLBACK")
	if !free(6) {
		t.Fatal("transaction-level lock outlived ROLLBACK")
	}
}

// 13.3.5: a locking function must only run on the rows you mean to lock. The docs give three
// queries: "ok", "danger!" (the LIMIT "is not guaranteed to be applied before the locking
// function is executed") and "ok" (LIMIT in a subquery). They run here verbatim, on a foo
// with ids 12301..13300.
//
// "Not guaranteed" is the point. On PG 18 the docs' exact "danger!" query happens to lock
// just its 100 rows: Limit stops pulling rows from the scan, and with a sort the SELECT list
// is evaluated above it. But the same SELECT-list call with an OFFSET locks the 10 skipped
// rows too, and with the function in WHERE it runs for every row the scan visits, before
// ORDER BY ... LIMIT picks one. That is the classic job-queue bug, and the locks on the
// other 954 rows stay until the session ends.
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
		{"with an OFFSET", "SELECT pg_advisory_lock(id) FROM foo WHERE id > 12345 OFFSET 10 LIMIT 100", 110},
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
// A blocked UPDATE or DELETE then proceeds. (In READ COMMITTED it would proceed even if T1
// had updated the row: it re-checks the newest version. The docs' "unless an actual UPDATE
// of the row was performed" is about REPEATABLE READ / SERIALIZABLE; see
// TestIso_RR_ConcurrentChangeOutcomes.)
func TestLock_ForUpdateLocksOnlyReturnedRows(t *testing.T) {
	L := newLab(t)
	setupRows(L)
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("BEGIN")
	t1.Do("SELECT * FROM r WHERE id = 1 FOR UPDATE")
	t2.Do("UPDATE r SET v = 1 WHERE id = 2").MustNotBlock().MustOK()
	upd := t2.Do("UPDATE r SET v = 1 WHERE id = 1").MustBlock()
	t1.Do("COMMIT")
	L.Note("T1's lock is gone at COMMIT, so T2's blocked UPDATE proceeds")
	upd.MustAffect(1)

	t1.Do("BEGIN")
	t1.Do("SELECT * FROM r WHERE id = 2 FOR UPDATE")
	del := t2.Do("DELETE FROM r WHERE id = 2").MustBlock()
	t1.Do("COMMIT")
	L.Note("the same for a blocked DELETE")
	del.MustAffect(1)
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
// inserter waits for the first transaction's outcome. Such an error "might represent
// persistent error conditions": unlike a 40001, retrying the same INSERT fails again.
func TestLock_ExclusionConstraintWaitsForOutcome(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS booking", "CREATE TABLE booking (room int, during int4range, EXCLUDE USING gist (during WITH &&))")
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("BEGIN")
	t1.Do("INSERT INTO booking VALUES (1, '[1,5)')")
	ins := t2.Do("INSERT INTO booking VALUES (1, '[3,8)')").MustBlock()
	t1.Do("COMMIT")
	ins.MustFail(func(err error) bool { return lab.Code(err) == "23P01" })
	L.Note("retried unchanged, it fails the same way; only a different range goes in")
	t2.Do("INSERT INTO booking VALUES (1, '[3,8)')").MustFail(func(err error) bool { return lab.Code(err) == "23P01" })
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
	t.Run("inconsistent-with-other-tables", func(t *testing.T) {
		// "they could cause visible inconsistency between the contents of the target table
		// and other tables in the database": one transaction empties both big5 and its mirror, so the
		// only committed states are (5,5) and (0,0); the old snapshot sees (0,5).
		L := newLab(t)
		L.Exec("DROP TABLE IF EXISTS big5, mirror5", "CREATE TABLE big5 (v int)", "INSERT INTO big5 SELECT generate_series(1, 5)",
			"CREATE TABLE mirror5 AS SELECT * FROM big5")
		rd, dd := L.Session("RD"), L.Session("DDL")
		rd.Do("BEGIN ISOLATION LEVEL REPEATABLE READ")
		rd.Do("SELECT 1")
		dd.Do("BEGIN")
		dd.Do("TRUNCATE big5")
		dd.Do("DELETE FROM mirror5")
		dd.Do("COMMIT").MustOK()
		rd.Do("SELECT (SELECT count(*) FROM big5), (SELECT count(*) FROM mirror5)").MustRows("(0,5)")
		rd.Do("COMMIT")
	})
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
// ones: the matching row T1 inserted is left alone. "[S]ubsequent commands in the same
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
			{"rolled-back", "UPDATE r SET v = 5 WHERE id = 1", "(0)"},
		} {
			t.Run(strings.ReplaceAll(mode, " ", "-")+"/"+c.name, func(t *testing.T) {
				L := newLab(t)
				setupRows(L)
				t1, t2 := L.Session("T1"), L.Session("T2")
				t1.Do("BEGIN")
				t1.Do(c.first)
				t2.Do("BEGIN")
				sel := t2.Do("SELECT v FROM r WHERE id = 1 AND v < 10 " + mode).MustBlock()
				if c.name == "rolled-back" {
					t1.Do("ROLLBACK")
				} else {
					t1.Do("COMMIT")
				}
				sel.MustRows(c.want)
				if c.name == "updated" {
					// "it is the updated version of the row that is locked and returned"
					if n := L.QueryString("SELECT count(*) FROM pgrowlocks('r') p JOIN r ON r.ctid = p.locked_row WHERE r.id = 1"); n != "1" {
						t.Errorf("the new row version carries %s row locks, want 1", n)
					}
					t1.Do("SELECT 1 FROM r WHERE id = 1 FOR UPDATE NOWAIT").MustFail(lab.IsLockNotAvailable)
				}
				t2.Do("COMMIT")
			})
		}
	}
}

// 13.2.1: the ON CONFLICT guarantees hold in READ COMMITTED alone ("this is only the case in Read Committed mode"). In REPEATABLE READ
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
// become a page lock, and when the page and tuple locks on one relation reach the
// max_pred_locks_per_relation threshold (the default -2 means max_pred_locks_per_transaction
// / 2 = 32: the 32nd lock promotes) they become a relation lock.
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
	L.Note("one row from each of 40 other pages passes the max_pred_locks_per_relation threshold (64 / 2 = 32)")
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
	// The lookup itself: behind an ACCESS EXCLUSIVE lock, the first thing VACUUM and
	// VACUUM FULL wait for is ACCESS SHARE, not the mode the docs list for them.
	for _, sql := range []string{"VACUUM tl", "VACUUM FULL tl"} {
		t.Run(strings.ReplaceAll(sql, " ", "_")+"/lookup", func(t *testing.T) {
			L := newLab(t)
			L.Exec("DROP TABLE IF EXISTS tl2", "DROP TABLE IF EXISTS tl CASCADE",
				"CREATE TABLE tl (id int primary key, v int)", "INSERT INTO tl VALUES (1, 0)")
			t1, t2 := L.Session("T1"), L.Session("T2")
			t1.Do("BEGIN")
			t1.Do("LOCK TABLE tl IN ACCESS EXCLUSIVE MODE")
			op := t2.Do(sql).MustBlock()
			got := L.QueryString("SELECT mode FROM pg_locks WHERE pid = $1 AND NOT granted AND relation = 'tl'::regclass", t2.ID)
			if got != "AccessShareLock" {
				t.Errorf("%s first waits for %s, want AccessShareLock", sql, got)
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

// 13.3.2: what each row lock taken by a locking SELECT stops other transactions' writes
// from doing. FOR KEY SHARE blocks DELETE and an UPDATE that changes the key, but not other
// UPDATEs; FOR SHARE, FOR NO KEY UPDATE and FOR UPDATE block every UPDATE and DELETE.
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
		{"FOR NO KEY UPDATE", "UPDATE r SET v = 1 WHERE id = 1", true},
		{"FOR NO KEY UPDATE", "DELETE FROM r WHERE id = 1", true},
		{"FOR UPDATE", "UPDATE r SET v = 1 WHERE id = 1", true},
		{"FOR UPDATE", "DELETE FROM r WHERE id = 1", true},
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

	L.Note(`"...or rolls back": ROLLBACK PREPARED frees a waiting writer just the same`)
	t1.Do("BEGIN")
	t1.Do("UPDATE r SET v = 0 WHERE id = 1")
	t1.Do("PREPARE TRANSACTION '" + gid + "'").MustOK()
	upd = w.Do("UPDATE r SET v = v + 10 WHERE id = 1").MustBlock()
	t1.Do("ROLLBACK PREPARED '" + gid + "'").MustOK()
	upd.MustAffect(1)
	t2.Do("SELECT v FROM r WHERE id = 1").MustRows("(21)")
}

// 13.2, Table 13.1, by the docs' own definitions of the four phenomena, at each of the four
// levels that can be requested. READ UNCOMMITTED is accepted and behaves like READ COMMITTED;
// REPEATABLE READ allows no phantoms ("Allowed, but not in PG").
func TestIso_Table13_1(t *testing.T) {
	for _, c := range []struct {
		level                           lab.Level
		nonrepeatable, phantom, anomaly bool
	}{
		{lab.ReadUncommitted, true, true, true},
		{lab.ReadCommitted, true, true, true},
		{lab.RepeatableRead, false, false, true},
		{lab.Serializable, false, false, false},
	} {
		t.Run(c.level.Short(), func(t *testing.T) {
			L := newLab(t)
			L.Exec("DROP TABLE IF EXISTS t131", "CREATE TABLE t131 (id int primary key, v int)",
				"INSERT INTO t131 VALUES (1, 10), (2, 20)")
			t1, t2 := L.Session("T1"), L.Session("T2")
			pick := func(occurs bool, yes, no string) string {
				if occurs {
					return yes
				}
				return no
			}

			L.Note("dirty read: reading data written by a concurrent uncommitted transaction. Not possible at any level")
			t2.Do("BEGIN")
			t2.Do("UPDATE t131 SET v = 99 WHERE id = 1")
			t1.Begin(c.level)
			t1.Do("SHOW transaction_isolation").MustRows("(" + strings.ToLower(string(c.level)) + ")")
			t1.Do("SELECT v FROM t131 WHERE id = 1").MustNotBlock().MustRows("(10)")
			t1.Do("COMMIT")
			t2.Do("ROLLBACK")

			L.Note("nonrepeatable read: re-reading a row finds it modified by a transaction that committed since the first read")
			t1.Begin(c.level)
			t1.Do("SELECT v FROM t131 WHERE id = 1").MustRows("(10)")
			t2.Do("UPDATE t131 SET v = 11 WHERE id = 1")
			t1.Do("SELECT v FROM t131 WHERE id = 1").MustRows(pick(c.nonrepeatable, "(11)", "(10)"))
			t1.Do("COMMIT")

			L.Note("phantom read: re-executing a query finds that the set of rows satisfying its condition has changed")
			t1.Begin(c.level)
			t1.Do("SELECT count(*) FROM t131 WHERE v >= 10").MustRows("(2)")
			t2.Do("INSERT INTO t131 VALUES (3, 30)")
			t1.Do("SELECT count(*) FROM t131 WHERE v >= 10").MustRows(pick(c.phantom, "(3)", "(2)"))
			t1.Do("COMMIT")

			L.Note("serialization anomaly: both commit with a result no serial order gives (each counts, then inserts)")
			t1.Begin(c.level)
			t2.Begin(c.level)
			t1.Do("SELECT count(*) FROM t131").MustRows("(3)")
			t2.Do("SELECT count(*) FROM t131").MustRows("(3)")
			t1.Do("INSERT INTO t131 VALUES (4, 3)")
			t2.Do("INSERT INTO t131 VALUES (5, 3)")
			t1.Do("COMMIT").MustOK()
			c2 := t2.Do("COMMIT")
			if c.anomaly {
				c2.MustOK()
			} else {
				c2.MustFail(lab.IsSerialization)
			}
		})
	}
}

// 13.2.1: the re-check does not need a lock wait. The target row "might have already been
// updated (or deleted or locked) by another concurrent transaction by the time it is found";
// the updater waits only "if it is still in progress". Here T2's UPDATE is held up inside its
// own WHERE clause (on an advisory lock) while W updates the row and commits. T2 never waits
// for a row lock, and still works on W's version: it re-checks v < 10 against it.
func TestIso_RC_RecheckWithoutWait(t *testing.T) {
	for _, c := range []struct{ name, concurrent, want, final string }{
		{"still-matches", "UPDATE r SET v = 5 WHERE id = 1", "(105)", "(105)"},
		{"no-longer-matches", "UPDATE r SET v = 50 WHERE id = 1", "(no rows)", "(50)"},
		{"deleted", "DELETE FROM r WHERE id = 1", "(no rows)", "(no rows)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			L := newLab(t)
			setupRows(L)
			w, t2 := L.Session("W"), L.Session("T2")
			w.Do("SELECT pg_advisory_lock(7)")
			upd := t2.Do("UPDATE r SET v = v + 100 WHERE id = 1 AND pg_advisory_xact_lock(7) IS NOT NULL AND v < 10 RETURNING v").MustBlock()
			wait := L.QueryString("SELECT locktype FROM pg_locks WHERE pid = $1 AND NOT granted", t2.ID)
			if wait != "advisory" {
				t.Fatalf("T2 waits for a %s lock, want only the advisory lock", wait)
			}
			w.Do(c.concurrent).MustNotBlock().MustAffect(1)
			L.Note("W has committed; nothing holds the row any more when T2 is let go")
			w.Do("SELECT pg_advisory_unlock(7)")
			upd.MustRows(c.want)
			t2.Do("SELECT v FROM r WHERE id = 1").MustRows(c.final)
		})
	}
}

// 13.2.1: the docs' transfer between accounts 12345 and 7534, statement for statement, with
// a concurrent debit of 7534 held open across the second UPDATE. "balance = balance - 100.00"
// waits and is then applied to the updated row: both changes survive.
func TestIso_RC_AccountsTransferDocsExample(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS accounts", "CREATE TABLE accounts (acctnum int primary key, balance numeric)",
		"INSERT INTO accounts VALUES (12345, 1000.00), (7534, 1000.00)")
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("BEGIN")
	t1.Do("UPDATE accounts SET balance = balance + 100.00 WHERE acctnum = 12345").MustAffect(1)
	t2.Do("BEGIN")
	t2.Do("UPDATE accounts SET balance = balance - 50.00 WHERE acctnum = 7534").MustAffect(1)
	upd := t1.Do("UPDATE accounts SET balance = balance - 100.00 WHERE acctnum = 7534").MustBlock()
	t2.Do("COMMIT")
	upd.MustAffect(1)
	t1.Do("COMMIT").MustOK()
	t1.Do("SELECT acctnum, balance FROM accounts ORDER BY acctnum").MustRows("(7534,850.00) (12345,1100.00)")
}

// 13.2.2: "even a read-only transaction at this level may see a control record updated to
// show that a batch has been completed but not see one of the detail records which is
// logically part of the batch because it read an earlier revision of the control record."
// The deposit report of TestIso_SER_ReadOnlyCanFailDeferrableCannot at REPEATABLE READ:
// the report sees batch 1 closed with a total of 0, commits, and the total later becomes 100.
func TestIso_RR_ReadOnlySeesClosedBatchWithoutReceipt(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS control", "DROP TABLE IF EXISTS receipt",
		"CREATE TABLE control (deposit_no int)", "INSERT INTO control VALUES (1)",
		"CREATE TABLE receipt (deposit_no int, amount int)")
	t1, t2, ro := L.Session("T1"), L.Session("T2"), L.Session("RO")
	const total = "SELECT coalesce(sum(amount), 0) FROM receipt WHERE deposit_no = 1"
	t1.Begin(lab.RepeatableRead)
	t1.Do("SELECT deposit_no FROM control").MustRows("(1)")
	t1.Do("INSERT INTO receipt VALUES (1, 100)").MustOK()
	t2.Begin(lab.RepeatableRead)
	t2.Do("UPDATE control SET deposit_no = 2").MustAffect(1)
	t2.Do("COMMIT").MustOK()
	ro.Do("BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY")
	ro.Do("SELECT deposit_no FROM control").MustRows("(2)")
	ro.Do(total).MustRows("(0)")
	t1.Do("COMMIT").MustOK()
	ro.Do(total).MustRows("(0)")
	ro.Do("COMMIT").MustOK()
	L.Note("the report said: batch 1 is closed, total 0. Nothing failed, and the total is now 100")
	ro.Do(total).MustRows("(100)")
}

// 13.2.2: at REPEATABLE READ, business rules "are not likely to work correctly without
// careful use of explicit locks to block conflicting transactions". The on-call doctors of
// TestIso_WriteSkew_OnCallDoctors, with the check made under an explicit lock:
//   - FOR UPDATE on the rows the rule depends on: B waits for A, then fails with 40001
//     (a row it wants to lock changed after its snapshot); its retry sees one doctor on call;
//   - LOCK TABLE before the first query: B waits, and its snapshot, taken after the wait,
//     already shows one doctor on call.
func TestIso_RR_WriteSkewFixedByExplicitLocks(t *testing.T) {
	setup := func(t *testing.T) (*lab.Lab, *lab.Session, *lab.Session) {
		L := newLab(t)
		L.Exec("DROP TABLE IF EXISTS doctors", "CREATE TABLE doctors (name text primary key, on_call bool)",
			"INSERT INTO doctors VALUES ('alice', true), ('bob', true)")
		return L, L.Session("A"), L.Session("B")
	}
	t.Run("FOR-UPDATE", func(t *testing.T) {
		L, a, b := setup(t)
		const check = "SELECT count(*) FROM (SELECT 1 FROM doctors WHERE on_call FOR UPDATE) x"
		a.Begin(lab.RepeatableRead)
		b.Begin(lab.RepeatableRead)
		a.Do(check).MustRows("(2)")
		chk := b.Do(check).MustBlock()
		a.Do("UPDATE doctors SET on_call = false WHERE name = 'alice'")
		a.Do("COMMIT").MustOK()
		chk.MustFail(lab.IsSerialization)
		b.Do("ROLLBACK")
		L.Note("B retries: only one doctor is on call now, so bob stays")
		b.Begin(lab.RepeatableRead)
		b.Do(check).MustRows("(1)")
		b.Do("COMMIT")
	})
	t.Run("LOCK-TABLE", func(t *testing.T) {
		_, a, b := setup(t)
		a.Begin(lab.RepeatableRead)
		b.Begin(lab.RepeatableRead)
		a.Do("LOCK TABLE doctors IN SHARE ROW EXCLUSIVE MODE")
		lock := b.Do("LOCK TABLE doctors IN SHARE ROW EXCLUSIVE MODE").MustBlock()
		a.Do("SELECT count(*) FROM doctors WHERE on_call").MustRows("(2)")
		a.Do("UPDATE doctors SET on_call = false WHERE name = 'alice'")
		a.Do("COMMIT").MustOK()
		lock.MustOK()
		b.Do("SELECT count(*) FROM doctors WHERE on_call").MustRows("(1)")
		b.Do("COMMIT")
	})
}

// 13.2.3: the other way to generate a key, "selecting the maximum existing key and adding
// one". Both transactions read max(id) + 1 = 3 and insert it; the loser gets a serialization
// failure (40001), not a unique violation.
func TestIso_SER_MaxPlusOneKeyTurnsUniqueViolationInto40001(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS k", "CREATE TABLE k (id int primary key)", "INSERT INTO k VALUES (1), (2)")
	t1, t2 := L.Session("T1"), L.Session("T2")
	for _, s := range []*lab.Session{t1, t2} {
		s.Begin(lab.Serializable)
		s.Do("SELECT max(id) + 1 FROM k").MustRows("(3)")
	}
	t1.Do("INSERT INTO k VALUES (3)")
	ins := t2.Do("INSERT INTO k VALUES (3)").MustBlock()
	t1.Do("COMMIT")
	ins.MustFail(lab.IsSerialization)
}

// 13.2.3: when page-level predicate locks are combined into one relation-level lock, "an
// increase in the rate of serialization failures may occur". T1 and T2 read and update
// disjoint rows (ids up to 800, ids above 1200: different heap and index pages). Reading 20
// rows each, they keep tuple locks and both commit. Reading 40 rows each, on 40 pages, their
// locks are promoted to the whole relation, each one's UPDATE now conflicts with the other's
// read, and one of them fails. (The remedy, raising max_pred_locks_per_*, needs a server
// reload and is not run here.)
func TestIso_SER_PromotedPredicateLockCausesFalsePositive(t *testing.T) {
	for _, rows := range []int{20, 40} {
		t.Run(fmt.Sprintf("%d-rows-each", rows), func(t *testing.T) {
			L := newLab(t)
			L.Exec("DROP TABLE IF EXISTS pl", "CREATE TABLE pl (id int primary key, pad text)",
				"INSERT INTO pl SELECT g, repeat('x', 500) FROM generate_series(1, 2000) g", "ANALYZE pl")
			if L.QueryString("SELECT current_setting('max_pred_locks_per_page') || ',' || current_setting('max_pred_locks_per_relation') || ',' || current_setting('max_pred_locks_per_transaction')") != "2,-2,64" {
				t.Skip("the thresholds below assume the default max_pred_locks_* settings")
			}
			t1, t2 := L.Session("T1"), L.Session("T2")
			for i, s := range []*lab.Session{t1, t2} {
				s.Do("SET enable_seqscan = off")
				s.Do("SET enable_bitmapscan = off")
				s.Begin(lab.Serializable)
				s.Do(fmt.Sprintf("SELECT count(pad) FROM pl WHERE id = ANY (SELECT %d + g * 20 FROM generate_series(1, %d) g)", i*1200, rows)).
					MustRows(fmt.Sprintf("(%d)", rows))
			}
			relLocks := L.QueryString("SELECT count(*) FROM pg_locks WHERE mode = 'SIReadLock' AND locktype = 'relation' AND relation = 'pl'::regclass")
			promoted := rows == 40
			if want := map[bool]string{false: "0", true: "2"}[promoted]; relLocks != want {
				t.Fatalf("relation-level SIRead locks on pl: %s, want %s", relLocks, want)
			}
			ops := []*lab.Op{
				t1.Do("UPDATE pl SET pad = 'a' WHERE id = 20"),
				t2.Do("UPDATE pl SET pad = 'b' WHERE id = 1220"),
				t1.Do("COMMIT"),
				t2.Do("COMMIT"),
			}
			failures := 0
			for _, op := range ops {
				if op.Wait(); op.Err != nil && !op.Skipped {
					failures++
					if !lab.IsSerialization(op.Err) {
						t.Fatalf("%q failed with %s, want 40001", op.SQL, lab.Describe(op.Err))
					}
				}
			}
			if want := map[bool]int{false: 0, true: 1}[promoted]; failures != want {
				t.Fatalf("%d serialization failures, want %d", failures, want)
			}
			t2.Do("ROLLBACK")
		})
	}
}

// 13.3.1 Tip: "Only an ACCESS EXCLUSIVE lock blocks a SELECT (without FOR UPDATE/SHARE)
// statement."
func TestLock_OnlyAccessExclusiveBlocksSelect(t *testing.T) {
	L := newLab(t)
	setupRows(L)
	t1, t2 := L.Session("T1"), L.Session("T2")
	for _, mode := range tableLockModes {
		t1.Do("BEGIN")
		t1.Do("LOCK TABLE r IN " + mode + " MODE")
		sel := t2.Do("SELECT count(*) FROM r")
		if mode == "ACCESS EXCLUSIVE" {
			sel.MustBlock()
		} else {
			sel.MustNotBlock()
		}
		t1.Do("ROLLBACK")
		sel.MustRows("(2)")
	}
}

// 13.3.2: a waiting writer and NOWAIT. T2's UPDATE waits for T1's FOR SHARE: in pg_locks it
// holds the "tuple" lock and waits for T1's transaction id. That does not stop a compatible
// FOR SHARE NOWAIT from a third session: it joins T1's lock in the tuple header.
//
// It does fail (55P03) in one shape: T1 took FOR UPDATE in a subtransaction and rolled it
// back. pgrowlocks then still lists that FOR UPDATE in the tuple's multixact, and (measured)
// with a waiter in the queue the NOWAIT request fails. Without the waiter the same request
// succeeds (TestLock_RowLockSavepoints).
func TestLock_WaitingWriterAndShareNowait(t *testing.T) {
	for _, rolledBackForUpdate := range []bool{false, true} {
		name := "holder-for-share"
		if rolledBackForUpdate {
			name = "holder-for-share-plus-rolled-back-for-update"
		}
		t.Run(name, func(t *testing.T) {
			L := newLab(t)
			setupRows(L)
			t1, t2, t3 := L.Session("T1"), L.Session("T2"), L.Session("T3")
			t1.Do("BEGIN")
			t1.Do("SELECT * FROM r WHERE id = 1 FOR SHARE")
			if rolledBackForUpdate {
				t1.Do("SAVEPOINT a")
				t1.Do("SELECT * FROM r WHERE id = 1 FOR UPDATE")
				t1.Do("ROLLBACK TO SAVEPOINT a")
			}
			upd := t2.Do("UPDATE r SET v = 1 WHERE id = 1").MustBlock()
			locks := L.ShowLocks()
			if _, ok := lab.FindLock(locks, "T2", "ExclusiveLock", ""); !ok {
				t.Errorf("T2 does not hold the tuple lock while it waits")
			}
			L.Show("SELECT locked_row, multi, modes FROM pgrowlocks('r')")
			probe := t3.Do("SELECT 1 FROM r WHERE id = 1 FOR SHARE NOWAIT")
			if rolledBackForUpdate {
				probe.MustFail(lab.IsLockNotAvailable)
			} else {
				probe.MustRows("(1)")
			}
			t1.Do("COMMIT")
			upd.MustAffect(1)
		})
	}
}

// 13.3.2: "locking a row might cause a disk write, e.g., SELECT FOR UPDATE modifies selected
// rows to mark them locked, and so will result in disk writes." EXPLAIN (ANALYZE, WAL) shows
// the WAL record the lock writes; the same SELECT without FOR UPDATE writes none.
func TestLock_RowLockCausesDiskWrite(t *testing.T) {
	L := newLab(t)
	setupRows(L)
	s := L.Session("T1")
	s.Do("SELECT * FROM r") // sets the hint bits, so that the control below has nothing to write
	const explain = "EXPLAIN (ANALYZE, WAL, COSTS OFF, TIMING OFF, SUMMARY OFF) SELECT * FROM r WHERE id = 2"
	if plain := s.Do(explain).Text(); strings.Contains(plain, "WAL:") {
		t.Fatalf("a plain SELECT wrote WAL: %s", plain)
	}
	s.Do("BEGIN")
	locking := s.Do(explain + " FOR UPDATE").Text()
	s.Do("ROLLBACK")
	if !strings.Contains(locking, "WAL: records=1") {
		t.Fatalf("SELECT FOR UPDATE of one row should write one WAL record: %s", locking)
	}
}

// 13.4.2: "use Read Committed mode, or in Repeatable Read mode be careful to obtain locks
// before performing queries". In READ COMMITTED the order does not matter: a query run after
// the lock was granted takes a new snapshot and sees what the lock waited for.
func TestIso_RC_QueryAfterLockSeesWaitedForWrites(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS credits", "CREATE TABLE credits (amount int)", "INSERT INTO credits VALUES (100)")
	chk, wr := L.Session("CHK"), L.Session("WR")
	wr.Do("BEGIN")
	wr.Do("INSERT INTO credits VALUES (50)")
	chk.Begin(lab.ReadCommitted)
	chk.Do("SELECT sum(amount) FROM credits").MustRows("(100)")
	lock := chk.Do("LOCK TABLE credits IN SHARE MODE").MustBlock()
	wr.Do("COMMIT")
	lock.MustOK()
	chk.Do("SELECT sum(amount) FROM credits").MustRows("(150)")
	chk.Do("COMMIT")
}

// 13.4.2: the bank check done with explicit locks: "lock all tables needed for the check".
// A SHARE lock "guarantee[s] that there are no uncommitted changes in the locked table,
// other than those of the current transaction": the lock waits for the writer that is in
// the middle of changing both tables, the two sums then agree, and the checker's own
// uncommitted insert is not in its own way.
func TestIso_SumCheckWithShareLocks(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS credits", "DROP TABLE IF EXISTS debits",
		"CREATE TABLE credits (amount int)", "CREATE TABLE debits (amount int)",
		"INSERT INTO credits VALUES (100)", "INSERT INTO debits VALUES (100)")
	chk, wr := L.Session("CHK"), L.Session("WR")
	wr.Do("BEGIN")
	wr.Do("INSERT INTO credits VALUES (50)")
	chk.Begin(lab.ReadCommitted)
	lock := chk.Do("LOCK TABLE credits, debits IN SHARE MODE").MustBlock()
	wr.Do("INSERT INTO debits VALUES (50)").MustNotBlock().MustOK()
	wr.Do("COMMIT")
	lock.MustOK()
	chk.Do("SELECT (SELECT sum(amount) FROM credits), (SELECT sum(amount) FROM debits)").MustRows("(150,150)")
	ins := wr.Do("INSERT INTO credits VALUES (1)").MustBlock()
	L.Note("no one else can change the tables now, but the lock holder itself can")
	chk.Do("INSERT INTO debits VALUES (7)").MustNotBlock().MustOK()
	chk.Do("SELECT sum(amount) FROM debits").MustRows("(157)")
	chk.Do("ROLLBACK")
	ins.MustOK()
}

// 13.5: "There are also some corner cases in which the server will issue a unique-key or
// exclusion constraint error even though in principle it has enough information to determine
// that a serialization problem is the underlying cause." Both SERIALIZABLE transactions check
// for an overlapping booking before inserting one. With a unique key that protocol turns
// the error into 40001 (TestIso_SER_CheckBeforeInsertTurnsUniqueViolationInto40001); with
// an exclusion constraint the loser still gets 23P01.
func TestIso_SER_ExclusionConstraintErrorStays23P01(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS booking", "CREATE TABLE booking (room int, during int4range, EXCLUDE USING gist (during WITH &&))")
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Begin(lab.Serializable)
	t1.Do("SELECT count(*) FROM booking WHERE during && '[1,5)'").MustRows("(0)")
	t1.Do("INSERT INTO booking VALUES (1, '[1,5)')")
	t2.Begin(lab.Serializable)
	t2.Do("SELECT count(*) FROM booking WHERE during && '[3,8)'").MustRows("(0)")
	ins := t2.Do("INSERT INTO booking VALUES (1, '[3,8)')").MustBlock()
	t1.Do("COMMIT").MustOK()
	ins.MustFail(func(err error) bool { return lab.Code(err) == "23P01" })
	t2.Do("ROLLBACK")
}

// 13.7: "insertion of a GIN-indexed value usually produces several index key insertions per
// row". One row holding a five-element array adds five entries to the index. (The page- and
// bucket-level locks the section is about are not visible from SQL.)
func TestMVCC_GinIndexSeveralKeysPerRow(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS g", "CREATE TABLE g (a int[])",
		"CREATE INDEX g_gin ON g USING gin (a) WITH (fastupdate = off)",
		"INSERT INTO g VALUES ('{1,2,3,4,5}')", "VACUUM g")
	rows := L.Show("SELECT (SELECT count(*) FROM g) AS table_rows, n_entries AS index_entries FROM gin_metapage_info(get_raw_page('g_gin', 0))")
	if got := lab.FormatRows(rows); got != "(1,5)" {
		t.Fatalf("rows, index entries = %s, want (1,5)", got)
	}
}
