package mysql

import (
	"testing"

	"github.com/jyny/db-iso-lv-go/internal/lab"
)

// InnoDB lock vocabulary as shown in performance_schema.data_locks.LOCK_MODE:
//
//	IX / IS               intention locks on the table ("I will lock some rows")
//	X,REC_NOT_GAP         record lock: just the index record
//	X,GAP                 gap lock: the open interval BEFORE the record, not the record
//	X                     next-key lock: record + the gap before it, i.e. (prev, rec]
//	X,INSERT_INTENTION    an insert waiting for a gap lock held by someone else
//	"supremum pseudo-record"  the +∞ record of a page; X on it locks the gap to +∞
//
// Gap locks never conflict with each other; they only block inserts (insert intention).

// Equality on a unique index that finds a row: only a record lock.
func TestLock_RecordLockOnPK(t *testing.T) {
	L := newLab(t)
	setupT(L)
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("BEGIN")
	t1.Do("SELECT * FROM t WHERE id = 20 FOR UPDATE")
	locks := L.ShowLocks()
	mustHaveLock(t, locks, "T1", "IX", "")
	mustHaveLock(t, locks, "T1", "X,REC_NOT_GAP", "20")

	t2.Do("BEGIN")
	t2.Do("INSERT INTO t VALUES (15, 15, 0)").MustNotBlock().MustOK()
	t2.Do("INSERT INTO t VALUES (25, 25, 0)").MustNotBlock().MustOK()
	t2.Do("UPDATE t SET v = 1 WHERE id = 20").MustBlock()
	t1.Do("ROLLBACK")
	L.WaitAll()
	t2.Do("ROLLBACK")
}

// Equality on a unique index that finds NOTHING: a gap lock on the next record.
// Gap locks are compatible with each other, so T2 can lock the same gap; only inserts block.
func TestLock_GapLockOnMissingKey(t *testing.T) {
	L := newLab(t)
	setupT(L)
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("BEGIN")
	t1.Do("SELECT * FROM t WHERE id = 15 FOR UPDATE").MustRows("(no rows)")
	mustHaveLock(t, L.ShowLocks(), "T1", "X,GAP", "20")
	L.Note("T1 locked the gap (10,20). 'FOR UPDATE' on a gap is still compatible with other gap locks:")

	t2.Do("BEGIN")
	t2.Do("SELECT * FROM t WHERE id = 16 FOR UPDATE").MustNotBlock()
	ins := t2.Do("INSERT INTO t VALUES (12, 12, 0)").MustBlock()
	mustHaveLock(t, L.ShowLocks(), "T2", "X,GAP,INSERT_INTENTION", "20")
	t1.Do("ROLLBACK")
	ins.MustOK()
	t2.Do("INSERT INTO t VALUES (35, 35, 0)").MustNotBlock()
	t2.Do("ROLLBACK")
}

// Range scan on the PK: next-key locks on matching records plus a gap lock on the record
// that ends the scan. Inserts anywhere in [15, 30) block; beyond 30 they don't.
func TestLock_NextKeyRangeOnPK(t *testing.T) {
	L := newLab(t)
	setupT(L)
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("BEGIN")
	t1.Do("SELECT id FROM t WHERE id >= 15 AND id <= 25 FOR UPDATE").MustRows("(20)")
	locks := L.ShowLocks()
	mustHaveLock(t, locks, "T1", "X", "20")
	mustHaveLock(t, locks, "T1", "X,GAP", "30")

	t2.Do("BEGIN")
	a := t2.Do("INSERT INTO t VALUES (12, 0, 0)").MustBlock()
	t1.Do("ROLLBACK")
	a.MustOK()
	t2.Do("ROLLBACK")

	t1.Do("BEGIN")
	t1.Do("SELECT id FROM t WHERE id >= 15 AND id <= 25 FOR UPDATE")
	t2.Do("BEGIN")
	b := t2.Do("INSERT INTO t VALUES (28, 0, 0)").MustBlock()
	t1.Do("ROLLBACK")
	b.MustOK()
	t2.Do("ROLLBACK")

	t1.Do("BEGIN")
	t1.Do("SELECT id FROM t WHERE id >= 15 AND id <= 25 FOR UPDATE")
	t2.Do("BEGIN")
	t2.Do("INSERT INTO t VALUES (35, 0, 0)").MustNotBlock().MustOK()
	t2.Do("UPDATE t SET v = 1 WHERE id = 30").MustNotBlock().MustOK()
	t1.Do("ROLLBACK")
	t2.Do("ROLLBACK")
}

// Equality on a NON-unique secondary index: next-key lock on the match, gap lock on the
// next key (another k=20 could otherwise be inserted on either side), and a record lock
// on the clustered (PK) record.
func TestLock_NonUniqueSecondaryIndex(t *testing.T) {
	L := newLab(t)
	setupT(L)
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("BEGIN")
	t1.Do("SELECT id FROM t WHERE k = 20 FOR UPDATE").MustRows("(20)")
	locks := L.ShowLocks()
	mustHaveLock(t, locks, "T1", "X", "20, 20")
	mustHaveLock(t, locks, "T1", "X,GAP", "30, 30")
	mustHaveLock(t, locks, "T1", "X,REC_NOT_GAP", "20")

	t2.Do("BEGIN")
	L.Note("both gaps around k=20 are locked: k=15 and k=25 block, k=35 does not")
	a := t2.Do("INSERT INTO t VALUES (15, 15, 0)").MustBlock()
	t1.Do("ROLLBACK")
	a.MustOK()
	t2.Do("ROLLBACK")

	t1.Do("BEGIN")
	t1.Do("SELECT id FROM t WHERE k = 20 FOR UPDATE")
	t2.Do("BEGIN")
	b := t2.Do("INSERT INTO t VALUES (25, 25, 0)").MustBlock()
	t1.Do("ROLLBACK")
	b.MustOK()
	t2.Do("ROLLBACK")

	t1.Do("BEGIN")
	t1.Do("SELECT id FROM t WHERE k = 20 FOR UPDATE")
	t2.Do("BEGIN")
	t2.Do("INSERT INTO t VALUES (35, 35, 0)").MustNotBlock().MustOK()
	t1.Do("ROLLBACK")
	t2.Do("ROLLBACK")
}

// Without a usable index, a locking statement scans the clustered index and, in
// REPEATABLE READ, locks every row and every gap: effectively a table lock.
// In READ COMMITTED, InnoDB releases locks on rows that don't match ("semi-consistent read").
func TestLock_NoIndexLocksEverything(t *testing.T) {
	for _, level := range []lab.Level{lab.RepeatableRead, lab.ReadCommitted} {
		t.Run(level.Short(), func(t *testing.T) {
			L := newLab(t)
			setupT(L)
			L.Exec("UPDATE t SET v = id")
			t1, t2 := L.Session("T1"), L.Session("T2")
			t1.Begin(level)
			t1.Do("UPDATE t SET v = -1 WHERE v = 20").MustAffect(1)
			locks := L.ShowLocks()
			t2.Begin(level)
			other := t2.Do("UPDATE t SET v = -2 WHERE id = 30")
			ins := t2.Do("INSERT INTO t VALUES (40, 40, 40)")
			if level == lab.RepeatableRead {
				mustHaveLock(t, locks, "T1", "X", "supremum pseudo-record")
				other.MustBlock()
				t1.Do("ROLLBACK")
				L.WaitAll()
				other.MustOK()
				ins.MustOK()
			} else {
				if len(locks) != 2 {
					t.Fatalf("RC should keep only IX + the matching record lock, got %v", locks)
				}
				other.MustNotBlock().MustOK()
				ins.MustNotBlock().MustOK()
				t1.Do("ROLLBACK")
			}
			t2.Do("ROLLBACK")
		})
	}
}

// READ COMMITTED takes no gap locks for searches, so a phantom can be inserted
// into a range another transaction has "locked".
func TestLock_RC_NoGapLocks(t *testing.T) {
	L := newLab(t)
	setupT(L)
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Begin(lab.ReadCommitted)
	t1.Do("SELECT id FROM t WHERE id BETWEEN 10 AND 30 FOR UPDATE").MustRows("(10) (20) (30)")
	L.ShowLocks()
	t2.Do("INSERT INTO t VALUES (15, 15, 0)").MustNotBlock().MustOK()
	t1.Do("SELECT id FROM t WHERE id BETWEEN 10 AND 30 FOR UPDATE").MustRows("(10) (15) (20) (30)")
	t1.Do("COMMIT")
}

// FOR SHARE vs FOR UPDATE, NOWAIT and SKIP LOCKED.
func TestLock_SharedExclusiveNowaitSkipLocked(t *testing.T) {
	L := newLab(t)
	setupT(L)
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("BEGIN")
	t1.Do("SELECT * FROM t WHERE id = 10 FOR SHARE")
	t2.Do("BEGIN")
	t2.Do("SELECT * FROM t WHERE id = 10 FOR SHARE").MustNotBlock().MustOK()
	t2.Do("SELECT * FROM t WHERE id = 10 FOR UPDATE NOWAIT").MustFail(lab.IsLockNotAvailable)
	t2.Do("SELECT id FROM t ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED").MustRows("(20)")
	t1.Do("COMMIT")
	t2.Do("COMMIT")
}

// Plain SELECT is a non-locking consistent read: it reads the undo version and never waits
// for row locks. In SERIALIZABLE (with autocommit off) InnoDB turns it into SELECT ... FOR SHARE.
func TestLock_PlainSelectIsNonLocking(t *testing.T) {
	for _, level := range []lab.Level{lab.RepeatableRead, lab.Serializable} {
		t.Run(level.Short(), func(t *testing.T) {
			L := newLab(t)
			setupT(L)
			t1, t2 := L.Session("T1"), L.Session("T2")
			t1.Do("BEGIN")
			t1.Do("UPDATE t SET v = 99 WHERE id = 10")
			t2.Begin(level)
			sel := t2.Do("SELECT v FROM t WHERE id = 10")
			if level == lab.RepeatableRead {
				sel.MustNotBlock().MustRows("(0)")
			} else {
				sel.MustBlock()
				mustHaveLock(t, L.ShowLocks(), "T2", "S,REC_NOT_GAP", "10")
				t1.Do("COMMIT")
				sel.MustRows("(99)")
			}
			t1.Do("ROLLBACK")
			t2.Do("COMMIT")
		})
	}
}

// A duplicate-key check takes a shared lock on the conflicting record, so a second insert
// of an uncommitted key waits: rollback → success, commit → ERROR 1062.
func TestLock_DuplicateKeyCheckWaits(t *testing.T) {
	L := newLab(t)
	setupT(L)
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("BEGIN")
	t1.Do("INSERT INTO t VALUES (40, 40, 0)")
	L.Note("a fresh insert has only an IMPLICIT lock (the row's DB_TRX_ID): nothing in data_locks yet")
	L.ShowLocks()
	t2.Do("BEGIN")
	ins := t2.Do("INSERT INTO t VALUES (40, 40, 0)").MustBlock()
	L.Note("T2's conflict converted T1's implicit lock into an explicit X,REC_NOT_GAP; T2 waits with S.\n" +
		"quirk: data_locks.THREAD_ID shows T2's thread for T1's converted lock (ENGINE_TRANSACTION_ID is T1's)")
	locks := L.ShowLocks()
	mustHaveLock(t, locks, "T1", "X,REC_NOT_GAP", "40")
	mustHaveLock(t, locks, "T2", "S,REC_NOT_GAP", "40")
	t1.Do("COMMIT")
	ins.MustFail(lab.IsUniqueViolation)
	t2.Do("ROLLBACK")
}

// Metadata locks (MDL) protect table definitions. Any open transaction that touched a table
// holds a shared MDL until it ends; ALTER TABLE needs an exclusive MDL and waits, and every
// later query on the table queues behind the ALTER.
func TestLock_MetadataLockQueue(t *testing.T) {
	L := newLab(t)
	setupT(L)
	tx, ddl, rd := L.Session("TX"), L.Session("DDL"), L.Session("RD")
	tx.Do("BEGIN")
	tx.Do("SELECT count(*) FROM t")
	alter := ddl.Do("ALTER TABLE t ADD COLUMN c int, ALGORITHM=INSTANT").MustBlock()
	sel := rd.Do("SELECT count(*) FROM t").MustBlock()
	L.Note("even an INSTANT alter needs an exclusive MDL for a moment; RD queues behind it")
	L.ShowMetadataLocks()
	tx.Do("COMMIT")
	alter.MustOK()
	sel.MustOK()

	L.Note("the safe way: a short lock_wait_timeout (MDL timeout) for DDL sessions")
	tx.Do("BEGIN")
	tx.Do("SELECT count(*) FROM t")
	ddl.Do("SET SESSION lock_wait_timeout = 1")
	ddl.Do("ALTER TABLE t ADD COLUMN d int, ALGORITHM=INSTANT").MustFail(lab.IsLockNotAvailable)
	rd.Do("SELECT count(*) FROM t").MustNotBlock()
	tx.Do("COMMIT")
}
