package hermitage

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jyny/db-iso-lv-go/internal/lab"
)

// TestHermitageDocs replays every code block of Hermitage's postgres.md and mysql.md
// statement by statement, in the documented order, and checks what the document says about
// each statement: the rows it shows, that it BLOCKS, the error it prints.
//
//	https://github.com/ept/hermitage/blob/master/postgres.md  (written for PostgreSQL 9.3.5)
//	https://github.com/ept/hermitage/blob/master/mysql.md     (written for MySQL 5.6.21)
//
// TestHermitage runs one script per anomaly on every level; this test is the documents'
// own scripts, which differ per level where a lock would otherwise get in the way.
//
// Run: go test ./hermitage -v -run 'TestHermitageDocs/mysql/G2-item_serializable'

// docStep is one line of a code block. want is what the document says about it:
//
//	""         runs without waiting and without error
//	"=…"       returns these rows (in any order); "=none" for an empty result
//	"#n"       affects n rows
//	"!text"    fails with an error containing text
//	"block"    BLOCKS; then says how it ends once unblocked ("" = succeeds, or "=…" / "!text")
//	"-"        not checked (COMMIT / ROLLBACK of a transaction the DB may already have aborted)
type docStep struct {
	s          int
	sql        string
	want, then string
}

type docBlock struct {
	name  string
	level string
	// sessions that begin, in order, before the first step. Blocks that begin a session
	// later (Fekete's example) use a "begin" step instead.
	begin []int
	steps []docStep
}

const (
	docDeadlock  = "!Deadlock found when trying to get lock; try restarting transaction"
	docPGUpdate  = "!could not serialize access due to concurrent update"
	docPGDepends = "!could not serialize access due to read/write dependencies among transactions"
)

var both, all3 = []int{1, 2}, []int{1, 2, 3}

var postgresDocs = []docBlock{
	{"G0", "read committed", both, []docStep{
		{1, "update test set value = 11 where id = 1", "", ""},
		{2, "update test set value = 12 where id = 1", "block", ""},
		{1, "update test set value = 21 where id = 2", "", ""},
		{1, "commit", "", ""},
		{1, "select * from test", "=(1,11) (2,21)", ""},
		{2, "update test set value = 22 where id = 2", "", ""},
		{2, "commit", "", ""},
		{1, "select * from test", "=(1,12) (2,22)", ""},
	}},
	{"G1a", "read committed", both, []docStep{
		{1, "update test set value = 101 where id = 1", "", ""},
		{2, "select * from test", "=(1,10) (2,20)", ""},
		{1, "abort", "", ""},
		{2, "select * from test", "=(1,10) (2,20)", ""},
		{2, "commit", "", ""},
	}},
	{"G1b", "read committed", both, []docStep{
		{1, "update test set value = 101 where id = 1", "", ""},
		{2, "select * from test", "=(1,10) (2,20)", ""},
		{1, "update test set value = 11 where id = 1", "", ""},
		{1, "commit", "", ""},
		{2, "select * from test", "=(1,11) (2,20)", ""},
		{2, "commit", "", ""},
	}},
	{"G1c", "read committed", both, []docStep{
		{1, "update test set value = 11 where id = 1", "", ""},
		{2, "update test set value = 22 where id = 2", "", ""},
		{1, "select * from test where id = 2", "=(2,20)", ""},
		{2, "select * from test where id = 1", "=(1,10)", ""},
		{1, "commit", "", ""},
		{2, "commit", "", ""},
	}},
	{"OTV", "read committed", all3, []docStep{
		{1, "update test set value = 11 where id = 1", "", ""},
		{1, "update test set value = 19 where id = 2", "", ""},
		{2, "update test set value = 12 where id = 1", "block", ""},
		{1, "commit", "", ""},
		{3, "select * from test where id = 1", "=(1,11)", ""},
		{2, "update test set value = 18 where id = 2", "", ""},
		{3, "select * from test where id = 2", "=(2,19)", ""},
		{2, "commit", "", ""},
		{3, "select * from test where id = 2", "=(2,18)", ""},
		{3, "select * from test where id = 1", "=(1,12)", ""},
		{3, "commit", "", ""},
	}},
	{"PMP", "read committed", both, []docStep{
		{1, "select * from test where value = 30", "=none", ""},
		{2, "insert into test (id, value) values(3, 30)", "", ""},
		{2, "commit", "", ""},
		{1, "select * from test where value % 3 = 0", "=(3,30)", ""},
		{1, "commit", "", ""},
	}},
	{"PMP", "repeatable read", both, []docStep{
		{1, "select * from test where value = 30", "=none", ""},
		{2, "insert into test (id, value) values(3, 30)", "", ""},
		{2, "commit", "", ""},
		{1, "select * from test where value % 3 = 0", "=none", ""},
		{1, "commit", "", ""},
	}},
	{"PMP-write", "read committed", both, []docStep{
		{1, "update test set value = value + 10", "", ""},
		{2, "delete from test where value = 20", "block", ""},
		{1, "commit", "", ""},
		{2, "select * from test where value = 20", "=(1,20)", ""},
		{2, "commit", "", ""},
	}},
	{"PMP-write", "repeatable read", both, []docStep{
		{1, "update test set value = value + 10", "", ""},
		{2, "delete from test where value = 20", "block", docPGUpdate},
		{1, "commit", "", ""},
		{2, "abort", "-", ""},
	}},
	{"P4", "read committed", both, []docStep{
		{1, "select * from test where id = 1", "=(1,10)", ""},
		{2, "select * from test where id = 1", "=(1,10)", ""},
		{1, "update test set value = 11 where id = 1", "", ""},
		{2, "update test set value = 11 where id = 1", "block", ""},
		{1, "commit", "", ""},
		{2, "commit", "", ""},
	}},
	{"P4", "repeatable read", both, []docStep{
		{1, "select * from test where id = 1", "=(1,10)", ""},
		{2, "select * from test where id = 1", "=(1,10)", ""},
		{1, "update test set value = 11 where id = 1", "", ""},
		{2, "update test set value = 11 where id = 1", "block", docPGUpdate},
		{1, "commit", "", ""},
		{2, "abort", "-", ""},
	}},
	{"G-single", "read committed", both, []docStep{
		{1, "select * from test where id = 1", "=(1,10)", ""},
		{2, "select * from test where id = 1", "", ""},
		{2, "select * from test where id = 2", "", ""},
		{2, "update test set value = 12 where id = 1", "", ""},
		{2, "update test set value = 18 where id = 2", "", ""},
		{2, "commit", "", ""},
		{1, "select * from test where id = 2", "=(2,18)", ""},
		{1, "commit", "", ""},
	}},
	{"G-single", "repeatable read", both, []docStep{
		{1, "select * from test where id = 1", "=(1,10)", ""},
		{2, "select * from test where id = 1", "", ""},
		{2, "select * from test where id = 2", "", ""},
		{2, "update test set value = 12 where id = 1", "", ""},
		{2, "update test set value = 18 where id = 2", "", ""},
		{2, "commit", "", ""},
		{1, "select * from test where id = 2", "=(2,20)", ""},
		{1, "commit", "", ""},
	}},
	{"G-single-pred", "repeatable read", both, []docStep{
		{1, "select * from test where value % 5 = 0", "", ""},
		{2, "update test set value = 12 where value = 10", "", ""},
		{2, "commit", "", ""},
		{1, "select * from test where value % 3 = 0", "=none", ""},
		{1, "commit", "", ""},
	}},
	{"G-single-write", "repeatable read", both, []docStep{
		{1, "select * from test where id = 1", "=(1,10)", ""},
		{2, "select * from test", "", ""},
		{2, "update test set value = 12 where id = 1", "", ""},
		{2, "update test set value = 18 where id = 2", "", ""},
		{2, "commit", "", ""},
		{1, "delete from test where value = 20", docPGUpdate, ""},
		{1, "abort", "-", ""},
	}},
	{"G2-item", "repeatable read", both, []docStep{
		{1, "select * from test where id in (1,2)", "", ""},
		{2, "select * from test where id in (1,2)", "", ""},
		{1, "update test set value = 11 where id = 1", "", ""},
		{2, "update test set value = 21 where id = 2", "", ""},
		{1, "commit", "", ""},
		{2, "commit", "", ""},
	}},
	{"G2-item", "serializable", both, []docStep{
		{1, "select * from test where id in (1,2)", "", ""},
		{2, "select * from test where id in (1,2)", "", ""},
		{1, "update test set value = 11 where id = 1", "", ""},
		{2, "update test set value = 21 where id = 2", "", ""},
		{1, "commit", "", ""},
		{2, "commit", docPGDepends, ""},
	}},
	{"G2", "repeatable read", both, []docStep{
		{1, "select * from test where value % 3 = 0", "", ""},
		{2, "select * from test where value % 3 = 0", "", ""},
		{1, "insert into test (id, value) values(3, 30)", "", ""},
		{2, "insert into test (id, value) values(4, 42)", "", ""},
		{1, "commit", "", ""},
		{2, "commit", "", ""},
		{1, "select * from test where value % 3 = 0", "=(3,30) (4,42)", ""},
	}},
	{"G2", "serializable", both, []docStep{
		{1, "select * from test where value % 3 = 0", "", ""},
		{2, "select * from test where value % 3 = 0", "", ""},
		{1, "insert into test (id, value) values(3, 30)", "", ""},
		{2, "insert into test (id, value) values(4, 42)", "", ""},
		{1, "commit", "", ""},
		{2, "commit", docPGDepends, ""},
	}},
	{"G2-two-edges", "serializable", nil, []docStep{
		{1, "begin", "", ""},
		{1, "select * from test", "=(1,10) (2,20)", ""},
		{2, "begin", "", ""},
		{2, "update test set value = value + 5 where id = 2", "", ""},
		{2, "commit", "", ""},
		{3, "begin", "", ""},
		{3, "select * from test", "=(1,10) (2,25)", ""},
		{3, "commit", "", ""},
		{1, "update test set value = 0 where id = 1", docPGDepends, ""},
		{1, "abort", "-", ""},
	}},
}

var mysqlDocs = []docBlock{
	{"G0", "read uncommitted", both, []docStep{
		{1, "update test set value = 11 where id = 1", "", ""},
		{2, "update test set value = 12 where id = 1", "block", ""},
		{1, "update test set value = 21 where id = 2", "", ""},
		{1, "commit", "", ""},
		{1, "select * from test", "=(1,12) (2,21)", ""},
		{2, "update test set value = 22 where id = 2", "", ""},
		{2, "commit", "", ""},
		{1, "select * from test", "=(1,12) (2,22)", ""},
	}},
	{"G1a", "read uncommitted", both, []docStep{
		{1, "update test set value = 101 where id = 1", "", ""},
		{2, "select * from test", "=(1,101) (2,20)", ""},
		{1, "rollback", "", ""},
		{2, "select * from test", "=(1,10) (2,20)", ""},
		{2, "commit", "", ""},
	}},
	{"G1a", "read committed", both, []docStep{
		{1, "update test set value = 101 where id = 1", "", ""},
		{2, "select * from test", "=(1,10) (2,20)", ""},
		{1, "rollback", "", ""},
		{2, "select * from test", "=(1,10) (2,20)", ""},
		{2, "commit", "", ""},
	}},
	{"G1b", "read uncommitted", both, []docStep{
		{1, "update test set value = 101 where id = 1", "", ""},
		{2, "select * from test", "=(1,101) (2,20)", ""},
		{1, "update test set value = 11 where id = 1", "", ""},
		{1, "commit", "", ""},
		{2, "select * from test", "=(1,11) (2,20)", ""},
		{2, "commit", "", ""},
	}},
	{"G1b", "read committed", both, []docStep{
		{1, "update test set value = 101 where id = 1", "", ""},
		{2, "select * from test", "=(1,10) (2,20)", ""},
		{1, "update test set value = 11 where id = 1", "", ""},
		{1, "commit", "", ""},
		{2, "select * from test", "=(1,11) (2,20)", ""},
		{2, "commit", "", ""},
	}},
	{"G1c", "read uncommitted", both, []docStep{
		{1, "update test set value = 11 where id = 1", "", ""},
		{2, "update test set value = 22 where id = 2", "", ""},
		{1, "select * from test where id = 2", "=(2,22)", ""},
		{2, "select * from test where id = 1", "=(1,11)", ""},
		{1, "commit", "", ""},
		{2, "commit", "", ""},
	}},
	{"G1c", "read committed", both, []docStep{
		{1, "update test set value = 11 where id = 1", "", ""},
		{2, "update test set value = 22 where id = 2", "", ""},
		{1, "select * from test where id = 2", "=(2,20)", ""},
		{2, "select * from test where id = 1", "=(1,10)", ""},
		{1, "commit", "", ""},
		{2, "commit", "", ""},
	}},
	{"OTV", "read uncommitted", all3, []docStep{
		{1, "update test set value = 11 where id = 1", "", ""},
		{1, "update test set value = 19 where id = 2", "", ""},
		{2, "update test set value = 12 where id = 1", "block", ""},
		{1, "commit", "", ""},
		{3, "select * from test", "=(1,12) (2,19)", ""},
		{2, "update test set value = 18 where id = 2", "", ""},
		{3, "select * from test", "=(1,12) (2,18)", ""},
		{2, "commit", "", ""},
		{3, "commit", "", ""},
	}},
	{"OTV", "read committed", all3, []docStep{
		{1, "update test set value = 11 where id = 1", "", ""},
		{1, "update test set value = 19 where id = 2", "", ""},
		{2, "update test set value = 12 where id = 1", "block", ""},
		{1, "commit", "", ""},
		{3, "select * from test", "=(1,11) (2,19)", ""},
		{2, "update test set value = 18 where id = 2", "", ""},
		{3, "select * from test", "=(1,11) (2,19)", ""},
		{2, "commit", "", ""},
		{3, "select * from test", "=(1,12) (2,18)", ""},
		{3, "commit", "", ""},
	}},
	{"PMP", "read committed", both, []docStep{
		{1, "select * from test where value = 30", "=none", ""},
		{2, "insert into test (id, value) values(3, 30)", "", ""},
		{2, "commit", "", ""},
		{1, "select * from test where value % 3 = 0", "=(3,30)", ""},
		{1, "commit", "", ""},
	}},
	{"PMP", "repeatable read", both, []docStep{
		{1, "select * from test where value = 30", "=none", ""},
		{2, "insert into test (id, value) values(3, 30)", "", ""},
		{2, "commit", "", ""},
		{1, "select * from test where value % 3 = 0", "=none", ""},
		{1, "commit", "", ""},
	}},
	{"PMP-write", "read committed", both, []docStep{
		{1, "update test set value = value + 10", "", ""},
		{2, "select * from test", "=(1,10) (2,20)", ""},
		{2, "delete from test where value = 20", "block", ""},
		{1, "commit", "", ""},
		{2, "select * from test", "=(2,30)", ""},
		{2, "commit", "", ""},
	}},
	{"PMP-write", "repeatable read", both, []docStep{
		{1, "update test set value = value + 10", "", ""},
		{2, "select * from test where value = 20", "=(2,20)", ""},
		{2, "delete from test where value = 20", "block", ""},
		{1, "commit", "", ""},
		{2, "select * from test", "=(2,20)", ""},
		{2, "commit", "", ""},
	}},
	{"PMP-write", "serializable", both, []docStep{
		{2, "select * from test where value = 20", "=(2,20)", ""},
		{1, "update test set value = value + 10", "block", docDeadlock},
		{2, "delete from test where value = 20", "", ""},
		{1, "rollback", "-", ""},
		{2, "commit", "", ""},
	}},
	{"P4", "repeatable read", both, []docStep{
		{1, "select * from test where id = 1", "=(1,10)", ""},
		{2, "select * from test where id = 1", "=(1,10)", ""},
		{1, "update test set value = 11 where id = 1", "", ""},
		{2, "update test set value = 11 where id = 1", "block", ""},
		{1, "commit", "", ""},
		{2, "commit", "", ""},
	}},
	{"P4", "serializable", both, []docStep{
		{1, "select * from test where id = 1", "=(1,10)", ""},
		{2, "select * from test where id = 1", "=(1,10)", ""},
		{1, "update test set value = 11 where id = 1", "block", ""},
		{2, "update test set value = 11 where id = 1", docDeadlock, ""},
		{1, "commit", "", ""},
		{2, "rollback", "-", ""},
	}},
	{"G-single", "read committed", both, []docStep{
		{1, "select * from test where id = 1", "=(1,10)", ""},
		{2, "select * from test where id = 1", "", ""},
		{2, "select * from test where id = 2", "", ""},
		{2, "update test set value = 12 where id = 1", "", ""},
		{2, "update test set value = 18 where id = 2", "", ""},
		{2, "commit", "", ""},
		{1, "select * from test where id = 2", "=(2,18)", ""},
		{1, "commit", "", ""},
	}},
	{"G-single", "repeatable read", both, []docStep{
		{1, "select * from test where id = 1", "=(1,10)", ""},
		{2, "select * from test where id = 1", "", ""},
		{2, "select * from test where id = 2", "", ""},
		{2, "update test set value = 12 where id = 1", "", ""},
		{2, "update test set value = 18 where id = 2", "", ""},
		{2, "commit", "", ""},
		{1, "select * from test where id = 2", "=(2,20)", ""},
		{1, "commit", "", ""},
	}},
	{"G-single-pred", "repeatable read", both, []docStep{
		{1, "select * from test where value % 5 = 0", "", ""},
		{2, "update test set value = 12 where value = 10", "", ""},
		{2, "commit", "", ""},
		{1, "select * from test where value % 3 = 0", "=none", ""},
		{1, "commit", "", ""},
	}},
	{"G-single-write", "repeatable read", both, []docStep{
		{1, "select * from test where id = 1", "=(1,10)", ""},
		{2, "select * from test", "", ""},
		{2, "update test set value = 12 where id = 1", "", ""},
		{2, "update test set value = 18 where id = 2", "", ""},
		{2, "commit", "", ""},
		{1, "delete from test where value = 20", "#0", ""},
		{1, "select * from test where id = 2", "=(2,20)", ""},
		{1, "commit", "", ""},
	}},
	{"G-single-write", "serializable", both, []docStep{
		{1, "select * from test where id = 1", "=(1,10)", ""},
		{2, "select * from test", "", ""},
		{2, "update test set value = 12 where id = 1", "block", ""},
		{1, "delete from test where value = 20", docDeadlock, ""},
		{2, "update test set value = 18 where id = 2", "", ""},
		{1, "rollback", "-", ""},
		{2, "commit", "", ""},
	}},
	{"G2-item", "repeatable read", both, []docStep{
		{1, "select * from test where id in (1,2)", "", ""},
		{2, "select * from test where id in (1,2)", "", ""},
		{1, "update test set value = 11 where id = 1", "", ""},
		{2, "update test set value = 21 where id = 2", "", ""},
		{1, "commit", "", ""},
		{2, "commit", "", ""},
	}},
	{"G2-item", "serializable", both, []docStep{
		{1, "select * from test where id in (1,2)", "", ""},
		{2, "select * from test where id in (1,2)", "", ""},
		{1, "update test set value = 11 where id = 1", "block", ""},
		{2, "update test set value = 21 where id = 2", docDeadlock, ""},
		{1, "commit", "", ""},
		{2, "rollback", "-", ""},
	}},
	{"G2", "repeatable read", both, []docStep{
		{1, "select * from test where value % 3 = 0", "", ""},
		{2, "select * from test where value % 3 = 0", "", ""},
		{1, "insert into test (id, value) values(3, 30)", "", ""},
		{2, "insert into test (id, value) values(4, 42)", "", ""},
		{1, "commit", "", ""},
		{2, "commit", "", ""},
		{1, "select * from test where value % 3 = 0", "=(3,30) (4,42)", ""},
	}},
	{"G2", "serializable", both, []docStep{
		{1, "select * from test where value % 3 = 0", "", ""},
		{2, "select * from test where value % 3 = 0", "", ""},
		{1, "insert into test (id, value) values(3, 30)", "block", ""},
		{2, "insert into test (id, value) values(4, 42)", docDeadlock, ""},
		{1, "commit", "", ""},
		{2, "rollback", "-", ""},
	}},
	{"G2-two-edges", "serializable", nil, []docStep{
		{1, "begin", "", ""},
		{1, "select * from test", "=(1,10) (2,20)", ""},
		{2, "begin", "", ""},
		{2, "update test set value = value + 5 where id = 2", "block", docDeadlock},
		{3, "begin", "", ""},
		{3, "select * from test", "block", "=(1,10) (2,20)"},
		{1, "update test set value = 0 where id = 1", "block", ""},
		{3, "commit", "-", ""},
		{1, "commit", "", ""},
		{2, "rollback", "-", ""},
	}},
}

// docNow lists the steps where the current server (MySQL 9.7) no longer does what the
// document (MySQL 5.6) says: block → step index → {outcome now, why}. For a step that
// BLOCKS, the outcome is how it ends.
var docNow = map[string]map[int][2]string{
	"mysql/PMP-write_serializable": {1: {"#1",
		"no deadlock: T2 already holds S locks on the rows, and its DELETE upgrades them to X without\n" +
			"queueing behind T1's waiting request. T1 waits, then updates the one row T2 left (serial order T2 → T1)"}},
}

// sameRows compares a FormatRows result with a documented one, ignoring row order
// (the documents' "select * from test" has no ORDER BY).
func sameRows(got, want string) bool {
	if want == "none" {
		want = "(no rows)"
	}
	g, w := strings.Fields(got), strings.Fields(want)
	slices.Sort(g)
	slices.Sort(w)
	return slices.Equal(g, w)
}

// outcome checks a finished op against "" / "=rows" / "#n" / "!error".
func outcome(t *testing.T, op *lab.Op, want string) {
	t.Helper()
	switch {
	case want == "":
		if !op.OK() {
			t.Errorf("#%d %s: %q failed: %s", op.Seq, op.S.Name, op.SQL, lab.Describe(op.Err))
		}
	case want[0] == '=':
		if !op.OK() || !sameRows(op.Text(), want[1:]) {
			t.Errorf("#%d %s: %q returned %s (err %v), the document says %s", op.Seq, op.S.Name, op.SQL, op.Text(), op.Err, want[1:])
		}
	case want[0] == '#':
		if !op.OK() || want[1:] != strconv.FormatInt(op.Affected, 10) {
			t.Errorf("#%d %s: %q affected %d row(s) (err %v), the document says %s", op.Seq, op.S.Name, op.SQL, op.Affected, op.Err, want[1:])
		}
	case want[0] == '!':
		if op.Err == nil || op.Skipped {
			t.Errorf("#%d %s: %q succeeded, the document says it fails with %q", op.Seq, op.S.Name, op.SQL, want[1:])
		} else if !strings.Contains(lab.Describe(op.Err), want[1:]) {
			t.Errorf("#%d %s: %q failed with %s, the document says %q", op.Seq, op.S.Name, op.SQL, lab.Describe(op.Err), want[1:])
		}
	}
}

func TestHermitageDocs(t *testing.T) {
	for _, db := range []struct {
		kind   lab.Kind
		blocks []docBlock
	}{{lab.Postgres, postgresDocs}, {lab.MySQL, mysqlDocs}} {
		t.Run(db.kind.String(), func(t *testing.T) {
			for _, b := range db.blocks {
				t.Run(b.name+"_"+strings.ReplaceAll(b.level, " ", "_"), func(t *testing.T) {
					L := lab.New(t, db.kind, dbname)
					L.Exec("DROP TABLE IF EXISTS test",
						"CREATE TABLE test (id int primary key, value int)",
						"INSERT INTO test (id, value) VALUES (1, 10), (2, 20)")
					sess := []*lab.Session{nil, L.Session("T1"), L.Session("T2"), L.Session("T3")}
					// The documents' own preamble for each session.
					begin := func(i int) {
						if db.kind == lab.MySQL {
							sess[i].Do("set session transaction isolation level " + b.level)
							sess[i].Do("begin")
						} else {
							sess[i].Do("begin")
							sess[i].Do("set transaction isolation level " + b.level)
						}
					}
					for _, i := range b.begin {
						begin(i)
					}
					type pending struct {
						op   *lab.Op
						then string
					}
					var blocked []pending
					now := docNow[db.kind.String()+"/"+t.Name()[strings.LastIndex(t.Name(), "/")+1:]]
					for i, st := range b.steps {
						if o, ok := now[i]; ok {
							doc := st.want
							if st.want == "block" {
								doc, st.then = st.then, o[0]
							} else {
								st.want = o[0]
							}
							L.Note("the document says %q for the next statement; now: %s", strings.TrimLeft(doc, "!=#"), o[1])
						}
						if st.sql == "begin" {
							begin(st.s)
							continue
						}
						op := sess[st.s].Do(st.sql)
						switch st.want {
						case "-":
						case "block":
							if !op.Blocked {
								t.Errorf("#%d %s: %q did not block, the document says it BLOCKS", op.Seq, op.S.Name, op.SQL)
							}
							blocked = append(blocked, pending{op, st.then})
						default:
							if op.Blocked {
								t.Errorf("#%d %s: %q blocked, the document does not say so", op.Seq, op.S.Name, op.SQL)
								blocked = append(blocked, pending{op, st.want})
								continue
							}
							outcome(t, op, st.want)
						}
					}
					L.WaitAll()
					for _, p := range blocked {
						outcome(t, p.op, p.then)
					}
				})
			}
		})
	}
}
