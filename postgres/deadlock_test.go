package postgres

import (
	"testing"

	"github.com/jyny/db-iso-lv-go/internal/lab"
)

func setupAccounts(L *lab.Lab) {
	L.Exec("DROP TABLE IF EXISTS acct", "CREATE TABLE acct (id int primary key, bal int)",
		"INSERT INTO acct VALUES (1, 100), (2, 100), (3, 100)")
}

// Classic: two transfers lock the same rows in opposite order.
// PG runs the deadlock detector only after waiting deadlock_timeout (here 100ms); the
// process that runs it and finds the cycle aborts itself.
func TestDeadlock_OppositeOrder(t *testing.T) {
	L := newLab(t)
	setupAccounts(L)
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("BEGIN")
	t2.Do("BEGIN")
	t1.Do("UPDATE acct SET bal = bal - 10 WHERE id = 1")
	t2.Do("UPDATE acct SET bal = bal - 10 WHERE id = 2")
	a := t1.Do("UPDATE acct SET bal = bal + 10 WHERE id = 2")
	b := t2.Do("UPDATE acct SET bal = bal + 10 WHERE id = 1")
	L.ShowDeadlock(lab.ExpectOneDeadlockVictim(t, a, b))
	t1.Do("ROLLBACK")
	t2.Do("ROLLBACK")
}

// Fix: lock rows in a consistent order (e.g. ORDER BY id FOR UPDATE) → the second
// transaction just waits; no cycle is possible.
func TestDeadlock_AvoidedByLockOrdering(t *testing.T) {
	L := newLab(t)
	setupAccounts(L)
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("BEGIN")
	t2.Do("BEGIN")
	t1.Do("SELECT id FROM acct WHERE id IN (1, 2) ORDER BY id FOR UPDATE")
	w := t2.Do("SELECT id FROM acct WHERE id IN (2, 1) ORDER BY id FOR UPDATE").MustBlock()
	t1.Do("UPDATE acct SET bal = bal - 10 WHERE id = 1")
	t1.Do("UPDATE acct SET bal = bal + 10 WHERE id = 2")
	t1.Do("COMMIT")
	w.MustOK()
	t2.Do("COMMIT")
}

// Lock upgrade: both take FOR SHARE (compatible), then both try to UPDATE the same row.
// Each UPDATE needs the other's share lock gone → deadlock.
func TestDeadlock_ShareLockUpgrade(t *testing.T) {
	L := newLab(t)
	setupAccounts(L)
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("BEGIN")
	t2.Do("BEGIN")
	t1.Do("SELECT * FROM acct WHERE id = 1 FOR SHARE")
	t2.Do("SELECT * FROM acct WHERE id = 1 FOR SHARE")
	a := t1.Do("UPDATE acct SET bal = 0 WHERE id = 1")
	b := t2.Do("UPDATE acct SET bal = 0 WHERE id = 1")
	L.ShowDeadlock(lab.ExpectOneDeadlockVictim(t, a, b))
}

// Two transactions insert the same unique keys in opposite order. Each waits for the
// other's transaction to finish to know whether its key is a duplicate.
func TestDeadlock_UniqueInsertCrossed(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS u", "CREATE TABLE u (id int primary key)")
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("BEGIN")
	t2.Do("BEGIN")
	t1.Do("INSERT INTO u VALUES (1)")
	t2.Do("INSERT INTO u VALUES (2)")
	a := t1.Do("INSERT INTO u VALUES (2)")
	b := t2.Do("INSERT INTO u VALUES (1)")
	L.ShowDeadlock(lab.ExpectOneDeadlockVictim(t, a, b))
}

// Table-lock upgrade: LOCK TABLE ... IN SHARE MODE allows other SHARE locks, but an INSERT
// needs ROW EXCLUSIVE, which conflicts with SHARE held by the other transaction.
func TestDeadlock_TableLockUpgrade(t *testing.T) {
	L := newLab(t)
	setupAccounts(L)
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("BEGIN")
	t2.Do("BEGIN")
	t1.Do("LOCK TABLE acct IN SHARE MODE")
	t2.Do("LOCK TABLE acct IN SHARE MODE")
	a := t1.Do("INSERT INTO acct VALUES (10, 0)")
	b := t2.Do("INSERT INTO acct VALUES (11, 0)")
	L.ShowDeadlock(lab.ExpectOneDeadlockVictim(t, a, b))
}

// A cycle can involve more than two transactions: T1 → T2 → T3 → T1.
func TestDeadlock_ThreeWayCycle(t *testing.T) {
	L := newLab(t)
	setupAccounts(L)
	t1, t2, t3 := L.Session("T1"), L.Session("T2"), L.Session("T3")
	for _, s := range []*lab.Session{t1, t2, t3} {
		s.Do("BEGIN")
	}
	t1.Do("UPDATE acct SET bal = 1 WHERE id = 1")
	t2.Do("UPDATE acct SET bal = 2 WHERE id = 2")
	t3.Do("UPDATE acct SET bal = 3 WHERE id = 3")
	a := t1.Do("UPDATE acct SET bal = 1 WHERE id = 2")
	b := t2.Do("UPDATE acct SET bal = 2 WHERE id = 3")
	c := t3.Do("UPDATE acct SET bal = 3 WHERE id = 1")
	victim := lab.WaitDeadlockVictim(t, a, b, c)
	L.ShowDeadlock(victim)
	// Once the victim is gone the remaining two form a plain wait chain; finish them in order.
	for _, s := range []*lab.Session{t1, t2, t3} {
		if s != victim.S {
			s.Do("COMMIT")
		}
	}
	L.WaitAll()
}
