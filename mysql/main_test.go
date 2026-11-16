// Package mysql contains MySQL / InnoDB experiments: MVCC internals, record / gap /
// next-key locks, metadata locks, deadlocks and isolation-level behavior.
// InnoDB's default isolation level is REPEATABLE READ. Run with -v to see each timeline:
//
//	go test ./mysql -v -run TestLock_Gap
package mysql

import (
	"testing"

	"github.com/jyny/db-iso-lv-go/internal/lab"
)

const dbname = "lab_mysql"

func newLab(t *testing.T) *lab.Lab {
	return lab.New(t, lab.MySQL, dbname)
}

// setupT creates the table most lock experiments use:
//
//	id (PK) : 10, 20, 30
//	k (non-unique secondary index idx_k) : 10, 20, 30
//	v (no index)
func setupT(L *lab.Lab) {
	// fkc (TestLock_LocksSetByStatement) references t; drop it first or DROP TABLE t fails.
	L.Exec("DROP TABLE IF EXISTS fkc", "DROP TABLE IF EXISTS t",
		"CREATE TABLE t (id int PRIMARY KEY, k int, v int, KEY idx_k (k))",
		"INSERT INTO t VALUES (10, 10, 0), (20, 20, 0), (30, 30, 0)")
}

// mustHaveLock asserts a lock (session, mode, lock data) appears in performance_schema.data_locks.
func mustHaveLock(t *testing.T, locks []lab.Lock, session, mode, data string) {
	t.Helper()
	if _, ok := lab.FindLock(locks, session, mode, data); !ok {
		t.Fatalf("expected %s to hold/wait %s on %q; locks: %v", session, mode, data, locks)
	}
}
