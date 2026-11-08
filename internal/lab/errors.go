package lab

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
)

// Code returns the PG SQLSTATE or the MySQL error number.
func Code(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return fmt.Sprint(me.Number)
	}
	return ""
}

// IsDeadlock: PG 40P01 deadlock_detected / MySQL 1213 ER_LOCK_DEADLOCK
func IsDeadlock(err error) bool {
	c := Code(err)
	return c == "40P01" || c == "1213"
}

// IsSerialization: PG 40001 serialization_failure (RR first-updater-wins, SSI dangerous structure).
func IsSerialization(err error) bool { return Code(err) == "40001" }

// IsTxConflict: deadlock or serialization failure, i.e. errors where the application should retry the whole transaction.
func IsTxConflict(err error) bool { return IsDeadlock(err) || IsSerialization(err) }

// IsLockNotAvailable: NOWAIT / lock_timeout / innodb_lock_wait_timeout.
//
//	PG 55P03 lock_not_available
//	MySQL 1205 ER_LOCK_WAIT_TIMEOUT, 3572 ER_LOCK_NOWAIT
func IsLockNotAvailable(err error) bool {
	c := Code(err)
	return c == "55P03" || c == "1205" || c == "3572"
}

// IsUniqueViolation: PG 23505 / MySQL 1062 ER_DUP_ENTRY
func IsUniqueViolation(err error) bool {
	c := Code(err)
	return c == "23505" || c == "1062"
}

// Describe renders an error as one human-readable line.
func Describe(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return fmt.Sprintf("ERROR %s: %s", pe.Code, pe.Message)
	}
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return fmt.Sprintf("ERROR %d (%s): %s", me.Number, string(me.SQLState[:]), me.Message)
	}
	return err.Error()
}

// Detail returns a PG error's DETAIL (for deadlocks it lists who waits for whom).
func Detail(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Detail
	}
	return ""
}

// ExpectOneDeadlockVictim waits for all ops and asserts exactly one failed with a deadlock
// error and the rest succeeded. It returns the victim.
func ExpectOneDeadlockVictim(t testing.TB, ops ...*Op) (victim *Op) {
	t.Helper()
	var victims []string
	for _, op := range ops {
		op.Wait()
		switch {
		case op.Skipped:
			// Not executed: it only carries the victim's error forward.
		case op.Err != nil && IsDeadlock(op.Err):
			victim = op
			victims = append(victims, op.S.Name)
		case op.Err != nil:
			t.Fatalf("#%d %s unexpected error: %s", op.Seq, op.S.Name, Describe(op.Err))
		}
	}
	if len(victims) != 1 {
		t.Fatalf("expected exactly one deadlock victim, got %v", victims)
	}
	return victim
}

// ShowDeadlock prints deadlock details: the error DETAIL on PG, and the
// LATEST DETECTED DEADLOCK section of SHOW ENGINE INNODB STATUS on MySQL.
func (l *Lab) ShowDeadlock(victim *Op) {
	if victim == nil {
		return
	}
	l.Note("deadlock victim: %s (#%d %s)", victim.S.Name, victim.Seq, victim.SQL)
	switch l.Kind {
	case Postgres:
		if d := Detail(victim.Err); d != "" {
			l.Note("%s", d)
		}
	case MySQL:
		status := ""
		var typ, name string
		_ = l.Admin.QueryRow("SHOW ENGINE INNODB STATUS").Scan(&typ, &name, &status)
		if i := strings.Index(status, "LATEST DETECTED DEADLOCK"); i >= 0 {
			sec := status[i:]
			if j := strings.Index(sec, "\n------------\nTRANSACTIONS"); j >= 0 {
				sec = sec[:j]
			}
			l.Note("%s", condenseInnoDB(sec))
		}
	}
}

// condenseInnoDB keeps only the lines of the deadlock report that help understanding.
func condenseInnoDB(sec string) string {
	var keep []string
	for _, line := range strings.Split(sec, "\n") {
		s := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(s, "*** ("),
			strings.HasPrefix(s, "RECORD LOCKS"),
			strings.HasPrefix(s, "TABLE LOCK"),
			strings.HasPrefix(s, "*** WE ROLL BACK"),
			strings.HasPrefix(s, "UPDATE"), strings.HasPrefix(s, "update"),
			strings.HasPrefix(s, "DELETE"), strings.HasPrefix(s, "delete"),
			strings.HasPrefix(s, "INSERT"), strings.HasPrefix(s, "insert"),
			strings.HasPrefix(s, "SELECT"), strings.HasPrefix(s, "select"):
			keep = append(keep, "  "+s)
		}
	}
	return "InnoDB LATEST DETECTED DEADLOCK (excerpt):\n" + strings.Join(keep, "\n")
}

// WaitDeadlockVictim waits until one of ops fails with a deadlock error and returns it,
// without waiting for the survivors (which may still be blocked by each other).
func WaitDeadlockVictim(t testing.TB, ops ...*Op) *Op {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		for _, op := range ops {
			if op.Done() && op.Err != nil && IsDeadlock(op.Err) && !op.Skipped {
				return op
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no deadlock victim among %d ops after %v", len(ops), waitTimeout)
	return nil
}
