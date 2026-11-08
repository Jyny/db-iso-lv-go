package mysql

import (
	"fmt"
	"testing"

	"github.com/jyny/db-iso-lv-go/internal/lab"
)

// InnoDB detects deadlocks immediately (innodb_deadlock_detect=ON) when a lock wait would
// close a cycle, and rolls back the transaction with the smaller "weight" (fewer rows
// changed/locked). The victim's WHOLE transaction is rolled back (ERROR 1213).
//
// Cases marked "aneasystone #N" are ported from https://github.com/alleycat1/mysql-deadlocks-case
// (originally reproduced on MySQL 5.7). The `deadlock` field records what MySQL 9.7 does.

type step struct {
	s   int // session index: 1 = T1, 2 = T2, ...
	sql string
}

type deadlockCase struct {
	name     string
	ref      string
	why      string
	level    lab.Level // default REPEATABLE READ
	setup    []string
	steps    []step
	deadlock bool
	// was57 explains cases that deadlocked in the original MySQL 5.7 write-up but not on 9.7.
	was57 string
}

var deadlockCases = []deadlockCase{
	{
		name: "OppositeOrder", ref: "aneasystone #8",
		why: "T1 locks 10 then 20, T2 locks 20 then 10",
		steps: []step{
			{1, "DELETE FROM t WHERE id = 10"},
			{2, "DELETE FROM t WHERE id = 20"},
			{1, "DELETE FROM t WHERE id = 20"},
			{2, "DELETE FROM t WHERE id = 10"},
		},
		deadlock: true,
	},
	{
		name: "CheckThenInsert_GapLocks", ref: "aneasystone #1 (pattern)",
		why: "both lock the same gap with FOR UPDATE on a missing key (gap locks are compatible),\n" +
			"then each INSERT needs an insert-intention lock that conflicts with the other's gap lock",
		steps: []step{
			{1, "SELECT * FROM t WHERE id = 15 FOR UPDATE"},
			{2, "SELECT * FROM t WHERE id = 16 FOR UPDATE"},
			{1, "INSERT INTO t VALUES (15, 15, 0)"},
			{2, "INSERT INTO t VALUES (16, 16, 0)"},
		},
		deadlock: true,
	},
	{
		name: "DuplicateInsertThenRollback", ref: "aneasystone #2 / MySQL manual",
		why: "T2 and T3 both wait with S locks on T1's uncommitted duplicate; when T1 rolls back\n" +
			"both get S, and both then need X to insert → each waits for the other's S",
		setup: []string{
			"DROP TABLE IF EXISTS lingluo",
			"CREATE TABLE lingluo (a int NOT NULL PRIMARY KEY, b int, c int, d int, UNIQUE KEY uk_bc (b, c))",
		},
		steps: []step{
			{1, "INSERT INTO lingluo VALUES (100213, 215, 215, 312)"},
			{2, "INSERT INTO lingluo VALUES (100214, 215, 215, 312)"},
			{3, "INSERT INTO lingluo VALUES (100215, 215, 215, 312)"},
			{1, "ROLLBACK"},
		},
		deadlock: true,
	},
	{
		name: "DeleteThenReinsertSameKey", ref: "aneasystone #18",
		why: "T1 deletes id=4 (X record lock); T2's delete waits; T1 re-inserts 4 and its duplicate\n" +
			"check on the delete-marked record queues behind T2's waiting X request",
		setup: []string{
			"DROP TABLE IF EXISTS t18",
			"CREATE TABLE t18 (id int unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY)",
			"INSERT INTO t18 VALUES (1),(2),(3),(4),(5),(6),(7),(8)",
		},
		steps: []step{
			{1, "DELETE FROM t18 WHERE id = 4"},
			{2, "DELETE FROM t18 WHERE id = 4"},
			{1, "INSERT INTO t18 VALUES (4)"},
		},
		deadlock: false,
		was57: "deadlocked on 5.7. On 9.7 T1 already holds X,REC_NOT_GAP on its own delete-marked\n" +
			"record, so the re-insert's duplicate check does not queue behind T2's waiting request",
	},
	{
		name: "UniqueSecondary_DeleteInsert", ref: "aneasystone #4",
		why: "T2 deletes a=2; T1's delete of a=2 waits; T2 re-inserts a=2, whose duplicate check\n" +
			"needs S on the delete-marked a=2, conflicting with T1's waiting X",
		setup: []string{
			"DROP TABLE IF EXISTS test4",
			"CREATE TABLE test4 (id int unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, a int unsigned, UNIQUE KEY a (a))",
			"INSERT INTO test4 (id, a) VALUES (1,1),(2,2),(3,3),(4,4),(5,5),(6,6),(7,7),(8,8)",
		},
		steps: []step{
			{2, "DELETE FROM test4 WHERE a = 2"},
			{1, "DELETE FROM test4 WHERE a = 2"},
			{2, "INSERT INTO test4 (id, a) VALUES (10, 2)"},
		},
		deadlock: false,
		was57: "deadlocked on 5.7. On 9.7 T2's re-insert only takes S,GAP locks around its own\n" +
			"delete-marked a=2 (it already holds X on the record), which never wait for T1's X request",
	},
	{
		name: "NonUniqueSecondary_DeleteInsertGap", ref: "aneasystone #12",
		why: "T1 deletes a=5 (next-key lock (2,5] + gap (5,6)); T2's delete of a=5 waits;\n" +
			"T1 inserts a=2 into the gap before 5, which T2's WAITING next-key request also covers",
		setup: []string{
			"DROP TABLE IF EXISTS ty",
			"CREATE TABLE ty (id int NOT NULL AUTO_INCREMENT PRIMARY KEY, a int, b int, KEY idxa (a))",
			"INSERT INTO ty (a, b) VALUES (2,3),(5,4),(6,7)",
		},
		steps: []step{
			{1, "DELETE FROM ty WHERE a = 5"},
			{2, "DELETE FROM ty WHERE a = 5"},
			{1, "INSERT INTO ty (a, b) VALUES (2, 10)"},
		},
		deadlock: true,
	},
	{
		name: "UniqueSecondary_InsertInsert", ref: "aneasystone #15",
		why: "T1's insert of a=10 waits with a next-key S lock on T2's uncommitted a=10;\n" +
			"T2's next insert (a=9) lands in the gap that T1's waiting S lock covers",
		setup: []string{
			"DROP TABLE IF EXISTS t7",
			"CREATE TABLE t7 (id int NOT NULL AUTO_INCREMENT PRIMARY KEY, a int NOT NULL, UNIQUE KEY ua (a))",
			"INSERT INTO t7 (id, a) VALUES (1,1),(5,4),(20,20),(25,12)",
		},
		steps: []step{
			{2, "INSERT INTO t7 (id, a) VALUES (26, 10)"},
			{1, "INSERT INTO t7 (id, a) VALUES (30, 10)"},
			{2, "INSERT INTO t7 (id, a) VALUES (40, 9)"},
		},
		deadlock: true,
	},
	{
		name: "UpdateUniqueKey_ThreeSessions", ref: "aneasystone #11",
		why: "three updates of the same unique-key row wait in line; after T1 commits, T2 and T3\n" +
			"both get S locks on the new index entry and then both need X",
		setup: []string{
			"DROP TABLE IF EXISTS tt",
			"CREATE TABLE tt (id int NOT NULL DEFAULT 0 PRIMARY KEY, fileid int, UNIQUE KEY fileid (fileid))",
			"INSERT INTO tt VALUES (1, 1)",
		},
		steps: []step{
			{1, "UPDATE tt SET id = 2 WHERE fileid = 1"},
			{2, "UPDATE tt SET id = 3 WHERE fileid = 1"},
			{3, "UPDATE tt SET id = 4 WHERE fileid = 1"},
			{1, "COMMIT"},
		},
		deadlock: false,
		was57: "deadlocked on 5.7. On 9.7 T2 and T3 take X,GAP locks (compatible) instead of\n" +
			"S locks that later need upgrading, so they simply queue",
	},
	{
		name: "Serializable_ReadThenUpdate", level: lab.Serializable,
		why: "in SERIALIZABLE a plain SELECT takes S locks; both read, then both try to write\n" +
			"→ S→X upgrade deadlock (the price of SERIALIZABLE-by-locking)",
		steps: []step{
			{1, "SELECT v FROM t WHERE id = 10"},
			{2, "SELECT v FROM t WHERE id = 10"},
			{1, "UPDATE t SET v = 1 WHERE id = 10"},
			{2, "UPDATE t SET v = 2 WHERE id = 10"},
		},
		deadlock: true,
	},
}

func TestDeadlock(t *testing.T) {
	for _, c := range deadlockCases {
		t.Run(c.name, func(t *testing.T) {
			L := newLab(t)
			setupT(L)
			L.Exec(c.setup...)
			if c.ref != "" {
				L.Note("%s", c.ref)
			}
			L.Note("%s", c.why)
			if c.was57 != "" {
				L.Note("MySQL 9.7: %s", c.was57)
			}

			n := 0
			for _, st := range c.steps {
				n = max(n, st.s)
			}
			level := c.level
			if level == "" {
				level = lab.RepeatableRead
			}
			sessions := make([]*lab.Session, n)
			for i := range sessions {
				sessions[i] = L.Session(fmt.Sprintf("T%d", i+1))
				sessions[i].Begin(level)
			}
			var ops []*lab.Op
			for i, st := range c.steps {
				if i == len(c.steps)-1 {
					L.ShowLocks()
				}
				ops = append(ops, sessions[st.s-1].Do(st.sql))
			}
			L.Settle()

			var victim *lab.Op
			for _, op := range ops {
				if op.Done() && op.Err != nil && lab.IsDeadlock(op.Err) && !op.Skipped {
					victim = op
				}
			}
			if victim != nil {
				L.ShowDeadlock(victim)
			} else {
				L.ShowLocks()
			}
			if got := victim != nil; got != c.deadlock {
				t.Errorf("deadlock = %v, want %v", got, c.deadlock)
			}
		})
	}
}
