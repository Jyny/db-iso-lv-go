package mysql

import (
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/jyny/db-iso-lv-go/internal/lab"
)

// InnoDB keeps only the newest row version in the clustered index. Each record has hidden
// DB_TRX_ID (last writer) and DB_ROLL_PTR (pointer to the undo record holding the previous
// version). A consistent read walks the undo chain back to the version its read view can see.
//
// In REPEATABLE READ the read view is created at the FIRST consistent read, not at BEGIN.
// START TRANSACTION WITH CONSISTENT SNAPSHOT creates it immediately.
func TestMVCC_ReadViewCreatedAtFirstRead(t *testing.T) {
	L := newLab(t)
	setupT(L)
	t1, t2 := L.Session("T1"), L.Session("T2")

	t1.Do("START TRANSACTION")
	t2.Do("UPDATE t SET v = 1 WHERE id = 10")
	L.Note("T2 committed after T1's START TRANSACTION, but T1 has not read anything yet")
	t1.Do("SELECT v FROM t WHERE id = 10").MustRows("(1)")
	t2.Do("UPDATE t SET v = 2 WHERE id = 10")
	t1.Do("SELECT v FROM t WHERE id = 10").MustRows("(1)")
	t1.Do("COMMIT")

	t1.Do("START TRANSACTION WITH CONSISTENT SNAPSHOT")
	t2.Do("UPDATE t SET v = 3 WHERE id = 10")
	L.Note("WITH CONSISTENT SNAPSHOT: the read view already exists, so T2's change is invisible")
	t1.Do("SELECT v FROM t WHERE id = 10").MustRows("(2)")
	t1.Do("COMMIT")
}

// One transaction, two kinds of read:
//   - consistent (snapshot) read: plain SELECT, uses the read view
//   - current read: SELECT ... FOR SHARE/UPDATE, UPDATE, DELETE, INSERT checks; always the
//     latest committed version, and takes locks
//
// So in REPEATABLE READ the same transaction can see two different values of one row.
func TestMVCC_SnapshotReadVsCurrentRead(t *testing.T) {
	L := newLab(t)
	setupT(L)
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("START TRANSACTION")
	t1.Do("SELECT v FROM t WHERE id = 10").MustRows("(0)")
	t2.Do("UPDATE t SET v = 5 WHERE id = 10")
	t1.Do("SELECT v FROM t WHERE id = 10").MustRows("(0)")
	t1.Do("SELECT v FROM t WHERE id = 10 FOR SHARE").MustRows("(5)")
	t1.Do("SELECT v FROM t WHERE id = 10").MustRows("(0)")
	L.Note("and an UPDATE computes from the CURRENT version, not the snapshot")
	t1.Do("UPDATE t SET v = v + 1 WHERE id = 10")
	t1.Do("SELECT v FROM t WHERE id = 10").MustRows("(6)")
	t1.Do("COMMIT")
}

// The famous MySQL REPEATABLE READ "phantom": a row invisible to the snapshot can still be
// matched by an UPDATE (current read). Once T1 has modified it, it carries T1's trx id and
// becomes visible to T1's subsequent snapshot reads.
func TestMVCC_RR_PhantomAppearsAfterUpdate(t *testing.T) {
	L := newLab(t)
	setupT(L)
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("START TRANSACTION")
	t1.Do("SELECT id FROM t WHERE k >= 40").MustRows("(no rows)")
	t2.Do("INSERT INTO t VALUES (40, 40, 0)")
	t1.Do("SELECT id FROM t WHERE k >= 40").MustRows("(no rows)")
	t1.Do("UPDATE t SET v = 7 WHERE k >= 40").MustAffect(1)
	t1.Do("SELECT id, v FROM t WHERE k >= 40").MustRows("(40,7)")
	t1.Do("COMMIT")
	L.Note("PostgreSQL RR would update 0 rows here (the UPDATE also uses the snapshot)")
}

// A read-only transaction never gets a real transaction id: innodb_trx shows a large
// synthetic id. The real id is assigned on the first write.
func TestMVCC_TrxIdAssignedOnFirstWrite(t *testing.T) {
	L := newLab(t)
	setupT(L)
	t1 := L.Session("T1")
	trxID := func() int64 {
		lab.FreshInnoDBTrx()
		v := L.QueryString("SELECT trx_id FROM information_schema.innodb_trx WHERE trx_mysql_thread_id = ?", t1.ID)
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	t1.Do("START TRANSACTION")
	t1.Do("SELECT * FROM t WHERE id = 10")
	ro := trxID()
	t1.Do("UPDATE t SET v = 1 WHERE id = 10")
	rw := trxID()
	L.Note("trx_id while read-only: %d; after the first write: %d", ro, rw)
	if ro < 1<<48 || rw >= 1<<48 {
		t.Fatalf("expected a synthetic id (>= 2^48) before the write and a real one after: %d, %d", ro, rw)
	}
	t1.Do("ROLLBACK")
}

var hllRe = regexp.MustCompile(`History list length (\d+)`)

func historyListLength(L *lab.Lab) int {
	L.T.Helper()
	var typ, name, status string
	if err := L.Admin.QueryRow("SHOW ENGINE INNODB STATUS").Scan(&typ, &name, &status); err != nil {
		L.T.Fatal(err)
	}
	m := hllRe.FindStringSubmatch(status)
	n, _ := strconv.Atoi(m[1])
	return n
}

// Old row versions live in undo logs. Purge can only discard undo that no open read view
// still needs, so one long-running transaction makes the "history list" grow without bound.
func TestMVCC_LongReadViewBlocksPurge(t *testing.T) {
	L := newLab(t)
	setupT(L)
	rd, wr := L.Session("RD"), L.Session("WR")
	rd.Do("START TRANSACTION WITH CONSISTENT SNAPSHOT")
	before := historyListLength(L)
	L.Quiet()
	for i := 0; i < 200; i++ {
		wr.Do("UPDATE t SET v = v + 1 WHERE id = 10")
	}
	L.Loud()
	peak := historyListLength(L)
	L.Note("history list length: %d → %d after 200 commits while RD's read view is open", before, peak)
	// Purge may still discard undo OLDER than RD's read view (e.g. a backlog left by earlier
	// workloads such as ../jepsen), so "before" says little. What it must keep is every
	// version newer than the view: all 200 of ours.
	if peak < 200 {
		t.Fatalf("history list is %d; the 200 versions newer than RD's read view must be retained", peak)
	}
	rd.Do("SELECT v FROM t WHERE id = 10").MustRows("(0)")
	rd.Do("COMMIT")

	deadline := time.Now().Add(20 * time.Second)
	for {
		now := historyListLength(L)
		if now < peak-150 {
			L.Note("after RD commits, purge catches up: %d", now)
			return
		}
		if time.Now().After(deadline) {
			lab.FreshInnoDBTrx()
			L.Show("SELECT trx_id, trx_state, trx_started, trx_mysql_thread_id, trx_query FROM information_schema.innodb_trx")
			L.Show("SELECT ID, USER, DB, COMMAND, TIME, STATE, INFO FROM performance_schema.processlist WHERE COMMAND <> 'Daemon'")
			t.Fatalf("history list did not shrink after the read view closed: %d", now)
		}
		// The purge coordinator sleeps when it sees no new commits, so a small leftover history
		// can sit untouched on an idle server. One tiny commit wakes it up.
		L.Exec("UPDATE t SET v = v + 1 WHERE id = 30")
		time.Sleep(200 * time.Millisecond)
	}
}
