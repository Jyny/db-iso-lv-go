package postgres

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jyny/db-iso-lv-go/internal/lab"
)

// The four row-level lock modes and which pairs conflict.
// https://www.postgresql.org/docs/18/explicit-locking.html#LOCKING-ROWS
func TestLock_RowLockConflictMatrix(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS rl", "CREATE TABLE rl (id int primary key, v int)", "INSERT INTO rl VALUES (1, 1)")
	modes := []string{"KEY SHARE", "SHARE", "NO KEY UPDATE", "UPDATE"}
	want := [][]bool{
		{false, false, false, true},
		{false, false, true, true},
		{false, true, true, true},
		{true, true, true, true},
	}
	L.Quiet()
	t1, t2 := L.Session("T1"), L.Session("T2")
	var b strings.Builder
	fmt.Fprintf(&b, "\n%-24s %s\n", "held \\ requested", strings.Join(modes, " | "))
	for i, held := range modes {
		fmt.Fprintf(&b, "FOR %-20s", held)
		for j, req := range modes {
			t1.Do("BEGIN")
			t1.Do("SELECT * FROM rl WHERE id = 1 FOR " + held)
			t2.Do("BEGIN")
			op := t2.Do("SELECT * FROM rl WHERE id = 1 FOR " + req)
			blocked := op.Blocked
			t1.Do("ROLLBACK")
			op.Wait()
			t2.Do("ROLLBACK")
			cell := "ok"
			if blocked {
				cell = "BLOCK"
			}
			fmt.Fprintf(&b, " %-*s |", len(req), cell)
			if blocked != want[i][j] {
				t.Errorf("FOR %s vs FOR %s: blocked=%v, want %v", held, req, blocked, want[i][j])
			}
		}
		b.WriteString("\n")
	}
	t.Log(b.String())
}

// A plain UPDATE that does not touch key columns takes FOR NO KEY UPDATE, and an FK check on
// the child side takes FOR KEY SHARE on the parent, so they don't conflict. Updating the
// parent's key to a different value takes FOR UPDATE and blocks child inserts.
func TestLock_ForeignKeyAndNoKeyUpdate(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS child", "DROP TABLE IF EXISTS parent",
		"CREATE TABLE parent (id int primary key, name text)",
		"CREATE TABLE child (id int primary key, parent_id int REFERENCES parent(id))",
		"INSERT INTO parent VALUES (1, 'p1')")
	t1, t2 := L.Session("T1"), L.Session("T2")

	t1.Do("BEGIN")
	t1.Do("UPDATE parent SET name = 'renamed' WHERE id = 1")
	t2.Do("BEGIN")
	L.Note("the FK check takes FOR KEY SHARE on parent row 1: compatible with FOR NO KEY UPDATE")
	t2.Do("INSERT INTO child VALUES (1, 1)").MustNotBlock().MustOK()
	t2.Do("ROLLBACK")
	t1.Do("ROLLBACK")

	t1.Do("BEGIN")
	t1.Do("UPDATE parent SET id = 1 WHERE id = 1")
	L.Note("assigning the key column its SAME value: PG compares old/new key values, still FOR NO KEY UPDATE")
	t2.Do("BEGIN")
	t2.Do("INSERT INTO child VALUES (2, 1)").MustNotBlock().MustOK()
	t2.Do("ROLLBACK")
	t1.Do("ROLLBACK")

	t1.Do("BEGIN")
	t1.Do("UPDATE parent SET id = 100 WHERE id = 1")
	L.Note("actually changing the key takes FOR UPDATE, which conflicts with the FK's FOR KEY SHARE")
	t2.Do("BEGIN")
	ins := t2.Do("INSERT INTO child VALUES (3, 1)").MustBlock()
	L.ShowLocks()
	t1.Do("COMMIT")
	L.Note("parent 1 no longer exists once T1 commits → FK violation")
	ins.MustFail(func(err error) bool { return lab.Code(err) == "23503" })
	t2.Do("ROLLBACK")
}

// The lock queue problem: an ALTER TABLE waiting for ACCESS EXCLUSIVE blocks every later
// query on the table, even plain SELECTs, because they queue behind it.
// The fix is SET lock_timeout before DDL, and retry.
func TestLock_DDLLockQueue(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS q", "CREATE TABLE q (id int primary key)", "INSERT INTO q VALUES (1)")
	long, ddl, reader := L.Session("TX"), L.Session("DDL"), L.Session("RD")

	long.Do("BEGIN")
	long.Do("SELECT * FROM q")
	L.Note("TX holds ACCESS SHARE on q until it ends")
	alter := ddl.Do("ALTER TABLE q ADD COLUMN c int").MustBlock()
	sel := reader.Do("SELECT * FROM q").MustBlock()
	L.Note("RD's ACCESS SHARE is compatible with TX's, but it queues behind DDL's ACCESS EXCLUSIVE request")
	L.ShowLocks()
	long.Do("COMMIT")
	alter.MustOK()
	sel.MustOK()

	L.Note("the safe way: give DDL a lock_timeout so it gives up instead of stalling everyone")
	long.Do("BEGIN")
	long.Do("SELECT * FROM q")
	ddl.Do("SET lock_timeout = '200ms'")
	ddl.Do("ALTER TABLE q ADD COLUMN d int").MustFail(lab.IsLockNotAvailable)
	reader.Do("SELECT count(*) FROM q").MustNotBlock()
	long.Do("COMMIT")
}

// NOWAIT fails immediately; SKIP LOCKED silently skips locked rows (a job-queue pattern).
func TestLock_NowaitAndSkipLocked(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS jobs", "CREATE TABLE jobs (id int primary key, state text)",
		"INSERT INTO jobs SELECT g, 'new' FROM generate_series(1, 3) g")
	w1, w2 := L.Session("W1"), L.Session("W2")

	w1.Do("BEGIN")
	w1.Do("SELECT id FROM jobs WHERE state = 'new' ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED").MustRows("(1)")
	w2.Do("BEGIN")
	w2.Do("SELECT id FROM jobs WHERE state = 'new' ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED").MustRows("(2)")
	w2.Do("SELECT id FROM jobs WHERE id = 1 FOR UPDATE NOWAIT").MustFail(lab.IsLockNotAvailable)
	w1.Do("COMMIT")
	w2.Do("ROLLBACK")
}

// A unique-index insert that collides with an uncommitted insert waits for that
// transaction's outcome: rollback → success, commit → duplicate key error.
func TestLock_UniqueInsertWaitsForOutcome(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS u", "CREATE TABLE u (id int primary key)")
	t1, t2 := L.Session("T1"), L.Session("T2")

	t1.Do("BEGIN")
	t1.Do("INSERT INTO u VALUES (5)")
	t2.Do("BEGIN")
	ins := t2.Do("INSERT INTO u VALUES (5)").MustBlock()
	L.Note("T2 waits on T1's transactionid: it cannot know yet whether 5 is a duplicate")
	L.ShowLocks()
	t1.Do("ROLLBACK")
	ins.MustOK()
	t2.Do("COMMIT")

	t1.Do("BEGIN")
	t1.Do("INSERT INTO u VALUES (6)")
	ins = t2.Do("INSERT INTO u VALUES (6)").MustBlock()
	t1.Do("COMMIT")
	ins.MustFail(lab.IsUniqueViolation)
}

// Advisory locks: application-defined locks on arbitrary numbers, released at transaction end
// (pg_advisory_xact_lock) or explicitly (pg_advisory_lock / pg_advisory_unlock).
func TestLock_Advisory(t *testing.T) {
	L := newLab(t)
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("BEGIN")
	t1.Do("SELECT pg_advisory_xact_lock(42)")
	t2.Do("SELECT pg_try_advisory_lock(42)").MustRows("(false)")
	lock := t2.Do("SELECT pg_advisory_lock(42)").MustBlock()
	L.ShowLocks()
	t1.Do("COMMIT")
	lock.MustOK()
	t2.Do("SELECT pg_advisory_unlock(42)").MustRows("(true)")
}
