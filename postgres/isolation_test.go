package postgres

import (
	"testing"

	"github.com/jyny/db-iso-lv-go/internal/lab"
)

// READ COMMITTED re-checks the WHERE clause on the newest row version after waiting
// ("EvalPlanQual"), but only for rows its own snapshot found. Straight from the docs:
// https://www.postgresql.org/docs/18/transaction-iso.html#XACT-READ-COMMITTED
func TestIso_RC_EvalPlanQualDocsExample(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS website", "CREATE TABLE website (id int primary key, hits int)",
		"INSERT INTO website VALUES (1, 9), (2, 10)")
	t1, t2 := L.Session("T1"), L.Session("T2")

	t1.Do("BEGIN")
	t1.Do("UPDATE website SET hits = hits + 1")
	t2.Do("BEGIN")
	del := t2.Do("DELETE FROM website WHERE hits = 10").MustBlock()
	t1.Do("COMMIT")
	L.Note("T2's snapshot only matched id=2 (hits=10). After waiting it re-checks id=2 → now 11 → skipped.\n" +
		"id=1 now has hits=10, but T2's snapshot saw 9, so it is never considered. 0 rows deleted.")
	del.MustAffect(0)
	t2.Do("COMMIT")
	t2.Do("SELECT * FROM website ORDER BY id").MustRows("(1,10) (2,11)")
}

// In READ COMMITTED, "SET v = v + 1" is safe: the UPDATE waits, then re-reads the newest
// version. The lost update (Hermitage P4) only happens with read-modify-write in the client.
func TestIso_RC_InPlaceIncrementIsSafe(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS ctr", "CREATE TABLE ctr (id int primary key, n int)", "INSERT INTO ctr VALUES (1, 0)")
	t1, t2 := L.Session("T1"), L.Session("T2")

	t1.Do("BEGIN")
	t2.Do("BEGIN")
	t1.Do("UPDATE ctr SET n = n + 1 WHERE id = 1")
	inc := t2.Do("UPDATE ctr SET n = n + 1 WHERE id = 1").MustBlock()
	t1.Do("COMMIT")
	inc.MustAffect(1)
	t2.Do("COMMIT")
	t2.Do("SELECT n FROM ctr").MustRows("(2)")

	L.Note("the same thing done in the client (read, then write a literal) loses an update")
	t1.Do("BEGIN")
	t2.Do("BEGIN")
	n1 := t1.Do("SELECT n FROM ctr WHERE id = 1").Int()
	n2 := t2.Do("SELECT n FROM ctr WHERE id = 1").Int()
	t1.Do("UPDATE ctr SET n = $1 WHERE id = 1", n1+1)
	w := t2.Do("UPDATE ctr SET n = $1 WHERE id = 1", n2+1).MustBlock()
	t1.Do("COMMIT")
	w.MustOK()
	t2.Do("COMMIT")
	t2.Do("SELECT n FROM ctr").MustRows("(3)") // two increments, but 3 not 4
}

// REPEATABLE READ is snapshot isolation with "first updater wins": updating a row that
// someone else changed after our snapshot fails with 40001, and the app must retry.
func TestIso_RR_FirstUpdaterWinsAndRetry(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS ctr", "CREATE TABLE ctr (id int primary key, n int)", "INSERT INTO ctr VALUES (1, 0)")
	t1, t2 := L.Session("T1"), L.Session("T2")

	t1.Do("BEGIN ISOLATION LEVEL REPEATABLE READ")
	t2.Do("BEGIN ISOLATION LEVEL REPEATABLE READ")
	t1.Do("SELECT n FROM ctr WHERE id = 1")
	t2.Do("SELECT n FROM ctr WHERE id = 1")
	t1.Do("UPDATE ctr SET n = n + 1 WHERE id = 1")
	upd := t2.Do("UPDATE ctr SET n = n + 1 WHERE id = 1").MustBlock()
	t1.Do("COMMIT")
	upd.MustFail(lab.IsSerialization)
	t2.Do("ROLLBACK")

	L.Note("retry the whole transaction with a fresh snapshot")
	t2.Do("BEGIN ISOLATION LEVEL REPEATABLE READ")
	t2.Do("UPDATE ctr SET n = n + 1 WHERE id = 1").MustAffect(1)
	t2.Do("COMMIT")
	t2.Do("SELECT n FROM ctr").MustRows("(2)")

	L.Note("if the first updater ROLLS BACK instead, the waiter proceeds normally")
	t1.Do("BEGIN ISOLATION LEVEL REPEATABLE READ")
	t2.Do("BEGIN ISOLATION LEVEL REPEATABLE READ")
	t2.Do("SELECT n FROM ctr")
	t1.Do("UPDATE ctr SET n = 100 WHERE id = 1")
	upd = t2.Do("UPDATE ctr SET n = n + 1 WHERE id = 1").MustBlock()
	t1.Do("ROLLBACK")
	upd.MustAffect(1)
	t2.Do("COMMIT")
}

// Write skew: the on-call doctors example. Each doctor checks "at least one other doctor is
// on call" and goes off call. Under REPEATABLE READ both succeed → nobody is on call.
// SERIALIZABLE (SSI) detects the rw-dependency cycle and aborts one.
func TestIso_WriteSkew_OnCallDoctors(t *testing.T) {
	for _, level := range []lab.Level{lab.RepeatableRead, lab.Serializable} {
		t.Run(level.Short(), func(t *testing.T) {
			L := newLab(t)
			L.Exec("DROP TABLE IF EXISTS doctors", "CREATE TABLE doctors (name text primary key, on_call bool)",
				"INSERT INTO doctors VALUES ('alice', true), ('bob', true)")
			a, b := L.Session("A"), L.Session("B")
			a.Begin(level)
			b.Begin(level)
			a.Do("SELECT count(*) FROM doctors WHERE on_call").MustRows("(2)")
			b.Do("SELECT count(*) FROM doctors WHERE on_call").MustRows("(2)")
			a.Do("UPDATE doctors SET on_call = false WHERE name = 'alice'")
			b.Do("UPDATE doctors SET on_call = false WHERE name = 'bob'")
			ca := a.Do("COMMIT")
			cb := b.Do("COMMIT")
			onCall := L.QueryString("SELECT count(*) FROM doctors WHERE on_call")
			L.Note("doctors on call afterwards: %s", onCall)
			if level == lab.RepeatableRead {
				ca.MustOK()
				cb.MustOK()
				if onCall != "0" {
					t.Fatalf("expected write skew under RR, got %s on call", onCall)
				}
			} else {
				ca.MustOK()
				cb.MustFail(lab.IsSerialization)
				L.Note("SSI noticed A→B and B→A rw-dependencies and failed the later commit")
			}
		})
	}
}

// SSI tracks reads with SIRead predicate locks. Their granularity depends on the plan:
// a sequential scan locks the whole relation, so two transactions working on disjoint
// groups still get a (false positive) serialization failure. An index scan locks only
// the index pages/tuples it touched, so the same workload commits fine.
func TestIso_SER_FalsePositiveFromSeqScan(t *testing.T) {
	for _, useIndex := range []bool{false, true} {
		name := "seqscan"
		if useIndex {
			name = "indexscan"
		}
		t.Run(name, func(t *testing.T) {
			L := newLab(t)
			L.Exec("DROP TABLE IF EXISTS ledger", "CREATE TABLE ledger (id serial primary key, grp int, amount int)",
				// groups 1 and 3 are separated by many group-2 rows so they land on different index pages
				"INSERT INTO ledger (grp, amount) SELECT 1, 1 FROM generate_series(1, 500)",
				"INSERT INTO ledger (grp, amount) SELECT 2, 1 FROM generate_series(1, 5000)",
				"INSERT INTO ledger (grp, amount) SELECT 3, 1 FROM generate_series(1, 500)")
			if useIndex {
				L.Exec("CREATE INDEX ledger_grp ON ledger (grp)")
			}
			L.Exec("ANALYZE ledger")
			t1, t2 := L.Session("T1"), L.Session("T2")
			for _, s := range []*lab.Session{t1, t2} {
				if useIndex {
					s.Do("SET enable_seqscan = off")
				} else {
					s.Do("SET enable_indexscan = off")
					s.Do("SET enable_bitmapscan = off")
				}
			}
			t1.Do("BEGIN ISOLATION LEVEL SERIALIZABLE")
			t2.Do("BEGIN ISOLATION LEVEL SERIALIZABLE")
			t1.Do("SELECT sum(amount) FROM ledger WHERE grp = 1")
			t2.Do("SELECT sum(amount) FROM ledger WHERE grp = 3")
			L.Note("SIRead locks are predicate locks; they never block anyone, they only record what was read")
			L.ShowAs("pg_locks (SIReadLock)", `
SELECT a.application_name, l.locktype, l.relation::regclass, l.page, l.tuple
FROM pg_locks l JOIN pg_stat_activity a USING (pid)
WHERE l.mode = 'SIReadLock' AND a.application_name LIKE 'lab:%' AND l.locktype = 'relation'
ORDER BY 1, 2, 3`)
			t1.Do("INSERT INTO ledger (grp, amount) VALUES (1, 10)")
			t2.Do("INSERT INTO ledger (grp, amount) VALUES (3, 10)")
			t1.Do("COMMIT").MustOK()
			c2 := t2.Do("COMMIT")
			if useIndex {
				c2.MustOK()
			} else {
				c2.MustFail(lab.IsSerialization)
			}
		})
	}
}

// Counterpart of MySQL's "phantom appears after UPDATE": in PostgreSQL REPEATABLE READ the
// UPDATE scans with the same snapshot, so a row inserted after the snapshot is not matched.
func TestIso_RR_UpdateUsesSnapshotNoPhantom(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS t", "CREATE TABLE t (id int primary key, k int, v int)",
		"INSERT INTO t VALUES (10, 10, 0), (20, 20, 0), (30, 30, 0)")
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("BEGIN ISOLATION LEVEL REPEATABLE READ")
	t1.Do("SELECT id FROM t WHERE k >= 40").MustRows("(no rows)")
	t2.Do("INSERT INTO t VALUES (40, 40, 0)")
	t1.Do("UPDATE t SET v = 7 WHERE k >= 40").MustAffect(0)
	t1.Do("SELECT id FROM t WHERE k >= 40").MustRows("(no rows)")
	t1.Do("COMMIT")
	L.Note("MySQL RR updates 1 row here and then 'sees' the phantom (mysql/TestMVCC_RR_PhantomAppearsAfterUpdate)")
}
