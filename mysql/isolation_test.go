package mysql

import (
	"testing"

	"github.com/jyny/db-iso-lv-go/internal/lab"
)

// MySQL REPEATABLE READ does NOT prevent lost updates (Hermitage P4): unlike PostgreSQL,
// an UPDATE of a row changed after the snapshot just waits and then overwrites it.
// The fixes: lock what you read (FOR UPDATE), or let the UPDATE do the arithmetic.
func TestIso_RR_LostUpdateAndFixes(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS ctr", "CREATE TABLE ctr (id int PRIMARY KEY, n int)", "INSERT INTO ctr VALUES (1, 0)")
	t1, t2 := L.Session("T1"), L.Session("T2")

	L.Note("1) read-modify-write in the client: lost update")
	t1.Do("START TRANSACTION")
	t2.Do("START TRANSACTION")
	n1 := t1.Do("SELECT n FROM ctr WHERE id = 1").Int()
	n2 := t2.Do("SELECT n FROM ctr WHERE id = 1").Int()
	t1.Do("UPDATE ctr SET n = ? WHERE id = 1", n1+1)
	w := t2.Do("UPDATE ctr SET n = ? WHERE id = 1", n2+1).MustBlock()
	t1.Do("COMMIT")
	w.MustOK()
	t2.Do("COMMIT")
	t2.Do("SELECT n FROM ctr").MustRows("(1)")

	L.Note("2) SELECT ... FOR UPDATE: the second reader waits and then reads the committed value")
	t1.Do("START TRANSACTION")
	t2.Do("START TRANSACTION")
	n1 = t1.Do("SELECT n FROM ctr WHERE id = 1 FOR UPDATE").Int()
	r2 := t2.Do("SELECT n FROM ctr WHERE id = 1 FOR UPDATE").MustBlock()
	t1.Do("UPDATE ctr SET n = ? WHERE id = 1", n1+1)
	t1.Do("COMMIT")
	t2.Do("UPDATE ctr SET n = ? WHERE id = 1", r2.Int()+1)
	t2.Do("COMMIT")
	t2.Do("SELECT n FROM ctr").MustRows("(3)")

	L.Note("3) atomic UPDATE: the UPDATE is a current read, so n = n + 1 is always correct")
	t1.Do("START TRANSACTION")
	t2.Do("START TRANSACTION")
	t1.Do("UPDATE ctr SET n = n + 1 WHERE id = 1")
	w = t2.Do("UPDATE ctr SET n = n + 1 WHERE id = 1").MustBlock()
	t1.Do("COMMIT")
	w.MustOK()
	t2.Do("COMMIT")
	t2.Do("SELECT n FROM ctr").MustRows("(5)")
}

// Write skew (on-call doctors) in REPEATABLE READ, and the locking fix: read the rows the
// invariant depends on with FOR UPDATE, which turns the race into a queue.
func TestIso_RR_WriteSkewAndForUpdateFix(t *testing.T) {
	for _, lockReads := range []bool{false, true} {
		name := "plain-select"
		if lockReads {
			name = "for-update"
		}
		t.Run(name, func(t *testing.T) {
			L := newLab(t)
			L.Exec("DROP TABLE IF EXISTS doctors", "CREATE TABLE doctors (name varchar(10) PRIMARY KEY, on_call bool)",
				"INSERT INTO doctors VALUES ('alice', true), ('bob', true)")
			a, b := L.Session("A"), L.Session("B")
			check := "SELECT count(*) FROM doctors WHERE on_call"
			if lockReads {
				check += " FOR UPDATE"
			}
			a.Do("START TRANSACTION")
			b.Do("START TRANSACTION")
			if a.Do(check).Int() >= 2 {
				a.Do("UPDATE doctors SET on_call = false WHERE name = 'alice'")
			}
			cb := b.Do(check)
			a.Do("COMMIT")
			if cb.Int() >= 2 {
				b.Do("UPDATE doctors SET on_call = false WHERE name = 'bob'")
			} else {
				L.Note("B sees only %d doctor on call, so it must stay", cb.Int())
			}
			b.Do("COMMIT")
			onCall := L.QueryString("SELECT count(*) FROM doctors WHERE on_call")
			L.Note("doctors on call afterwards: %s", onCall)
			want := "0"
			if lockReads {
				want = "1"
				if !cb.Blocked {
					t.Fatalf("B's FOR UPDATE read should have waited for A")
				}
			}
			if onCall != want {
				t.Fatalf("on call = %s, want %s", onCall, want)
			}
		})
	}
}

// SERIALIZABLE only turns plain SELECTs into locking reads inside explicit transactions.
// With autocommit=1 each SELECT is its own transaction and stays a non-locking snapshot read.
func TestIso_SER_AutocommitSelectStaysNonLocking(t *testing.T) {
	L := newLab(t)
	setupT(L)
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("START TRANSACTION")
	t1.Do("UPDATE t SET v = 99 WHERE id = 10")
	t2.Do("SET SESSION TRANSACTION ISOLATION LEVEL SERIALIZABLE")
	t2.Do("SELECT v FROM t WHERE id = 10").MustNotBlock().MustRows("(0)")
	t2.Do("START TRANSACTION")
	sel := t2.Do("SELECT v FROM t WHERE id = 10").MustBlock()
	t1.Do("COMMIT")
	sel.MustRows("(99)")
	t2.Do("COMMIT")
}

// READ UNCOMMITTED reads the newest version in the clustered index, uncommitted or not.
func TestIso_RU_DirtyRead(t *testing.T) {
	L := newLab(t)
	setupT(L)
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("START TRANSACTION")
	t1.Do("UPDATE t SET v = 42 WHERE id = 10")
	t2.Begin(lab.ReadUncommitted)
	t2.Do("SELECT v FROM t WHERE id = 10").MustRows("(42)")
	t1.Do("ROLLBACK")
	t2.Do("SELECT v FROM t WHERE id = 10").MustRows("(0)")
	t2.Do("COMMIT")
}
