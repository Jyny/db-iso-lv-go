package lab

import (
	"fmt"
	"strings"
	"testing"
)

const testDB = "lab_harness"

// A DSN override may or may not already carry query parameters.
func TestDSNOverride(t *testing.T) {
	cases := []struct {
		kind Kind
		env  string
		want string
	}{
		{Postgres, "postgres://u:p@h:1/%s?sslmode=disable", "postgres://u:p@h:1/x?sslmode=disable&default_query_exec_mode=simple_protocol"},
		{Postgres, "postgres://u:p@h:1/%s", "postgres://u:p@h:1/x?default_query_exec_mode=simple_protocol"},
		{MySQL, "u:p@tcp(h:1)/%s", "u:p@tcp(h:1)/x?interpolateParams=true&parseTime=true"},
		{MySQL, "u:p@tcp(h:1)/%s?tls=skip-verify", "u:p@tcp(h:1)/x?tls=skip-verify&interpolateParams=true&parseTime=true"},
	}
	for _, c := range cases {
		key := "LAB_PG_DSN"
		if c.kind == MySQL {
			key = "LAB_MYSQL_DSN"
		}
		t.Setenv(key, c.env)
		if got := dsn(c.kind, "x"); got != c.want {
			t.Errorf("%s %q:\n got  %s\n want %s", c.kind, c.env, got, c.want)
		}
	}
}

// After op.Wait() returns, the session must already be idle: the next Do must run normally
// instead of being reported as queued behind a blocked statement.
func TestWaitThenDoIsNotQueued(t *testing.T) {
	for _, kind := range []Kind{Postgres, MySQL} {
		t.Run(kind.String(), func(t *testing.T) {
			L := New(t, kind, testDB)
			L.Exec("DROP TABLE IF EXISTS w", "CREATE TABLE w (id int primary key, v int)", "INSERT INTO w VALUES (1, 0)")
			for i := range 200 {
				L.Quiet()
				t1, t2 := L.Session("T1"), L.Session("T2")
				t1.Do("BEGIN")
				t1.Do("UPDATE w SET v = v + 1 WHERE id = 1")
				p := t2.Do("UPDATE w SET v = v + 1 WHERE id = 1").MustBlock()
				// Release T2 from outside the timeline, so its completion is not observed by settle().
				terminate(L, t1)
				p.Wait()
				if op := t2.Do("SELECT 1"); !op.Done() {
					t.Fatalf("iteration %d: Do right after Wait returned before the statement ran", i)
				}
				L.sessions = nil
				t1.close()
				t2.close()
			}
		})
	}
}

func terminate(L *Lab, s *Session) {
	if L.Kind == Postgres {
		L.Exec(fmt.Sprintf("SELECT pg_terminate_backend(%d)", s.ID))
	} else {
		L.Exec(fmt.Sprintf("KILL %d", s.ID))
	}
}

// MySQL rolls back a deadlock victim's whole transaction immediately, so a new
// transaction started on that session must run, not be skipped.
func TestMySQLNewTxAfterDeadlockRuns(t *testing.T) {
	L := New(t, MySQL, testDB)
	L.Exec("DROP TABLE IF EXISTS d", "CREATE TABLE d (id int primary key, v int)", "INSERT INTO d VALUES (1, 0), (2, 0)")
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("START TRANSACTION")
	t2.Do("START TRANSACTION")
	t1.Do("UPDATE d SET v = 1 WHERE id = 1")
	t2.Do("UPDATE d SET v = 2 WHERE id = 2")
	a := t1.Do("UPDATE d SET v = 1 WHERE id = 2")
	b := t2.Do("UPDATE d SET v = 2 WHERE id = 1")
	victim := ExpectOneDeadlockVictim(t, a, b)
	other := t1
	if victim.S == t1 {
		other = t2
	}
	other.Do("COMMIT").MustOK()

	// No explicit ROLLBACK: the app just starts its retry.
	s := victim.S
	s.Begin(Serializable)
	s.Do("UPDATE d SET v = 9 WHERE id = 1").MustAffect(1)
	FreshInnoDBTrx()
	if lvl := L.QueryString("SELECT trx_isolation_level FROM information_schema.innodb_trx WHERE trx_mysql_thread_id = ?", s.ID); lvl != "SERIALIZABLE" {
		t.Fatalf("retry transaction runs at %q, want SERIALIZABLE", lvl)
	}
	s.Do("COMMIT").MustOK()
	if v := L.QueryString("SELECT v FROM d WHERE id = 1"); v != "9" {
		t.Fatalf("retry was not committed: v = %s", v)
	}
}

// A skipped op carrying the victim's deadlock error is not an "unexpected error".
func TestExpectOneDeadlockVictimIgnoresSkipped(t *testing.T) {
	L := New(t, MySQL, testDB)
	L.Exec("DROP TABLE IF EXISTS d", "CREATE TABLE d (id int primary key, v int)", "INSERT INTO d VALUES (1, 0), (2, 0)")
	t1, t2 := L.Session("T1"), L.Session("T2")
	t1.Do("START TRANSACTION")
	t2.Do("START TRANSACTION")
	t1.Do("UPDATE d SET v = 1 WHERE id = 1")
	t2.Do("UPDATE d SET v = 2 WHERE id = 2")
	a := t1.Do("UPDATE d SET v = 1 WHERE id = 2")
	b := t2.Do("UPDATE d SET v = 2 WHERE id = 1")
	// Ops queued on both sessions; the victim's one will be skipped with the deadlock error.
	a2 := t1.Do("SELECT 1")
	b2 := t2.Do("SELECT 1")
	if v := ExpectOneDeadlockVictim(t, a, b, a2, b2); !strings.HasPrefix(v.SQL, "UPDATE") {
		t.Fatalf("victim should be the UPDATE, got %q", v.SQL)
	}
}
