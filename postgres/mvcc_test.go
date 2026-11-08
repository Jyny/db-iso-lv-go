package postgres

import (
	"strconv"
	"strings"
	"testing"
)

// An UPDATE in PostgreSQL never overwrites a row in place: it writes a new tuple version and
// stamps the old one's xmax. Both versions stay on the page until VACUUM.
func TestMVCC_UpdateCreatesNewTupleVersion(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS mv", "CREATE TABLE mv (id int primary key, v int)")
	t1 := L.Session("T1")

	t1.Do("BEGIN")
	xid1 := t1.Do("SELECT pg_current_xact_id()").Str()
	t1.Do("INSERT INTO mv VALUES (1, 100)")
	t1.Do("COMMIT")
	t1.Do("SELECT xmin, xmax, ctid, * FROM mv").MustRows("(" + xid1 + ",0,(0,1),1,100)")

	t1.Do("BEGIN")
	xid2 := t1.Do("SELECT pg_current_xact_id()").Str()
	t1.Do("UPDATE mv SET v = 200 WHERE id = 1")
	t1.Do("COMMIT")
	L.Note("the visible row now lives at ctid (0,2) and was created by xid %s", xid2)
	t1.Do("SELECT xmin, xmax, ctid, * FROM mv").MustRows("(" + xid2 + ",0,(0,2),1,200)")

	L.Note("pageinspect shows BOTH versions: (0,1) is dead (xmax=%s) and its t_ctid points to (0,2)", xid2)
	items := pageItems(L, "mv")
	if len(items) != 2 {
		t.Fatalf("want 2 tuple versions on the page, got %d", len(items))
	}
	if items[0][3] != xid2 || items[0][4] != "(0,2)" || items[1][2] != xid2 {
		t.Fatalf("unexpected tuple chain: %v", items)
	}
	if items[0][9] != "true" {
		t.Fatalf("expected a HOT update (v is not indexed): %v", items)
	}

	L.Note("VACUUM removes the dead version; lp 1 becomes a redirect to lp 2 (HOT chain)")
	L.Exec("VACUUM mv")
	items = pageItems(L, "mv")
	if items[0][1] != "redirect" {
		t.Fatalf("expected lp 1 to become a redirect after VACUUM: %v", items)
	}
}

// A rolled-back INSERT still leaves its tuple on disk. Visibility is decided by looking
// the xmin up in the commit log (pg_xact); readers then set hint bits so later readers don't have to.
func TestMVCC_RollbackLeavesTupleOnDisk(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS mv", "CREATE TABLE mv (id int, v int)")
	t1 := L.Session("T1")

	t1.Do("BEGIN")
	t1.Do("INSERT INTO mv VALUES (1, 1)")
	t1.Do("ROLLBACK")
	t1.Do("SELECT count(*) FROM mv").MustRows("(0)")

	L.Note("the aborted tuple is still physically there; the SELECT above set its xmin_a (aborted) hint bit")
	items := pageItems(L, "mv")
	if len(items) != 1 || items[0][6] != "true" {
		t.Fatalf("expected one tuple with the xmin-aborted hint bit: %v", items)
	}
}

// For REPEATABLE READ the snapshot is taken at the first statement, not at BEGIN.
// A snapshot is just (xmin, xmax, in-progress xids): see pg_current_snapshot().
func TestMVCC_SnapshotTakenAtFirstStatement(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS mv", "CREATE TABLE mv (id int primary key, v int)", "INSERT INTO mv VALUES (1, 10)")
	t1, t2, t3 := L.Session("T1"), L.Session("T2"), L.Session("T3")

	t3.Do("BEGIN")
	xid3 := t3.Do("SELECT pg_current_xact_id()").Str()
	L.Note("T3 stays open with xid %s, so it will show up as in-progress in T1's snapshot", xid3)

	t1.Do("BEGIN ISOLATION LEVEL REPEATABLE READ")
	t2.Do("UPDATE mv SET v = 11 WHERE id = 1")
	L.Note("T2 committed (autocommit) AFTER T1's BEGIN but BEFORE T1's first query")
	t1.Do("SELECT v FROM mv").MustRows("(11)")
	snap := t1.Do("SELECT pg_current_snapshot()").Str()
	L.Note("T1's snapshot xmin:xmax:xip = %s; every xid listed is invisible to T1", snap)
	if xip := strings.Split(snap, ":")[2]; !strings.Contains(","+xip+",", ","+xid3+",") {
		t.Fatalf("expected T3's xid %s in the snapshot's in-progress list: %s", xid3, snap)
	}

	t2.Do("UPDATE mv SET v = 12 WHERE id = 1")
	t1.Do("SELECT v FROM mv").MustRows("(11)")
	t1.Do("COMMIT")
	t3.Do("COMMIT")
}

// A long-running snapshot holds back VACUUM: dead tuples it might still need cannot be removed.
// This is the classic cause of table bloat with idle-in-transaction sessions.
func TestMVCC_OldSnapshotBlocksVacuum(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS mv", "CREATE TABLE mv (id int primary key, v int) WITH (autovacuum_enabled = off)",
		"INSERT INTO mv VALUES (1, 0)")
	reader, writer := L.Session("RD"), L.Session("WR")

	reader.Do("BEGIN ISOLATION LEVEL REPEATABLE READ")
	reader.Do("SELECT v FROM mv").MustRows("(0)")
	for i := 1; i <= 5; i++ {
		writer.Do("UPDATE mv SET v = " + strconv.Itoa(i) + " WHERE id = 1")
	}
	L.Exec("VACUUM mv")
	live := countNormal(pageItems(L, "mv"))
	L.Note("after VACUUM, %d tuple versions remain: the reader's snapshot may still need them", live)
	if live < 6 {
		t.Fatalf("expected VACUUM to keep all 6 versions while the snapshot is open, got %d", live)
	}
	reader.Do("SELECT v FROM mv").MustRows("(0)")
	reader.Do("COMMIT")

	L.Exec("VACUUM mv")
	live = countNormal(pageItems(L, "mv"))
	L.Note("after the reader commits, VACUUM can reclaim them: %d left", live)
	if live != 1 {
		t.Fatalf("expected 1 live version after VACUUM, got %d", live)
	}
}

func countNormal(items [][]string) int {
	n := 0
	for _, it := range items {
		if it[1] == "normal" {
			n++
		}
	}
	return n
}

// Row locks are stored in the tuple header itself (xmax + infomask bits), not in a lock table.
// When two transactions share-lock the same row, xmax becomes a MultiXactId.
func TestMVCC_RowLocksLiveInTupleHeader(t *testing.T) {
	L := newLab(t)
	L.Exec("DROP TABLE IF EXISTS mv", "CREATE TABLE mv (id int primary key, v int)", "INSERT INTO mv VALUES (1, 10)")
	t1, t2 := L.Session("T1"), L.Session("T2")

	t1.Do("BEGIN")
	xid1 := t1.Do("SELECT pg_current_xact_id()").Str()
	t1.Do("SELECT * FROM mv WHERE id = 1 FOR SHARE")
	L.Note("one locker: xmax holds T1's xid %s even though nothing was deleted", xid1)
	items := pageItems(L, "mv")
	if items[0][3] != xid1 {
		t.Fatalf("expected xmax = locker xid %s: %v", xid1, items)
	}

	t2.Do("BEGIN")
	t2.Do("SELECT * FROM mv WHERE id = 1 FOR SHARE").MustNotBlock()
	L.Note("two lockers: xmax is replaced by a MultiXactId listing both")
	rl := L.Show("SELECT locked_row, multi, xids, modes FROM pgrowlocks('mv')")
	if len(rl) != 1 || rl[0][1] != "true" {
		t.Fatalf("expected a multixact row lock: %v", rl)
	}
	t1.Do("COMMIT")
	t2.Do("COMMIT")
}
