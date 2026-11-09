// Package jepsen runs a Jepsen-style randomized workload: instead of one hand-picked
// interleaving (see ../hermitage), many concurrent clients run random transactions and the
// resulting history is checked against invariants.
//
// The workload is Jepsen's classic "bank" test (https://github.com/jepsen-io/jepsen):
//
//   - transfer: read two balances, compute in the client, write both back
//     (client-side read-modify-write, so lost updates are possible)
//   - read: sum every account with separate SELECTs, twice in the same transaction
//
// Checks:
//   - unstable reads: the two passes of one read transaction differ → no stable snapshot
//     (non-repeatable / skewed reads)
//   - wrong totals: a read summed to something other than the initial total → money was
//     created or destroyed by lost updates (or a skewed read saw a half-done transfer)
//   - final total ≠ initial total → lost updates really happened
//
// Transactions that fail with a deadlock or serialization failure are retried, as a real
// application must.
//
// Run: go test ./jepsen -v -count=1
package jepsen

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jyny/db-iso-lv-go/internal/lab"
)

const (
	dbname     = "lab_jepsen"
	accounts   = 5
	initial    = 100 // per account
	writers    = 8
	readers    = 2
	runFor     = 1500 * time.Millisecond
	maxRetries = 50
)

var isoLevels = map[lab.Level]sql.IsolationLevel{
	lab.ReadUncommitted: sql.LevelReadUncommitted,
	lab.ReadCommitted:   sql.LevelReadCommitted,
	lab.RepeatableRead:  sql.LevelRepeatableRead,
	lab.Serializable:    sql.LevelSerializable,
}

type stats struct {
	commits, retries, gaveUp  atomic.Int64
	reads, unstable, wrong    atomic.Int64
	minSeen, maxSeen          atomic.Int64
	finalTotal, expectedTotal int64
}

func (s *stats) observe(total int64) {
	for {
		cur := s.minSeen.Load()
		if total >= cur || s.minSeen.CompareAndSwap(cur, total) {
			break
		}
	}
	for {
		cur := s.maxSeen.Load()
		if total <= cur || s.maxSeen.CompareAndSwap(cur, total) {
			break
		}
	}
}

// ph renders the n-th placeholder for the DB's driver.
func ph(kind lab.Kind, n int) string {
	if kind == lab.Postgres {
		return fmt.Sprintf("$%d", n)
	}
	return "?"
}

// retry runs fn in a transaction at level, retrying on deadlock / serialization failure.
func retry(ctx context.Context, db *sql.DB, level sql.IsolationLevel, st *stats, fn func(*sql.Tx) error) bool {
	for range maxRetries {
		tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: level})
		if err != nil {
			return false
		}
		err = fn(tx)
		if err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		switch {
		case err == nil:
			st.commits.Add(1)
			return true
		case lab.IsTxConflict(err):
			st.retries.Add(1)
			continue
		default:
			return false
		}
	}
	st.gaveUp.Add(1)
	return false
}

func runBank(t *testing.T, kind lab.Kind, level lab.Level) *stats {
	L := lab.New(t, kind, dbname)
	L.Exec("DROP TABLE IF EXISTS accounts",
		"CREATE TABLE accounts (id int PRIMARY KEY, balance int NOT NULL)")
	var vals []string
	for i := range accounts {
		vals = append(vals, fmt.Sprintf("(%d, %d)", i, initial))
	}
	L.Exec("INSERT INTO accounts VALUES " + strings.Join(vals, ","))
	if kind == lab.Postgres {
		// Detect deadlocks after 100ms instead of the default 1s (applies to new connections).
		L.Exec("ALTER DATABASE " + dbname + " SET deadlock_timeout = '100ms'")
	}

	db := L.Admin
	// ALTER DATABASE only affects new connections; drop the idle ones from setup.
	db.SetMaxIdleConns(0)
	db.SetMaxIdleConns(writers + readers + 2)
	db.SetMaxOpenConns(writers + readers + 2)
	if kind == lab.Postgres {
		if got := L.QueryString("SHOW deadlock_timeout"); got != "100ms" {
			t.Fatalf("pooled connection has deadlock_timeout = %s, want 100ms", got)
		}
	}
	iso := isoLevels[level]
	st := &stats{expectedTotal: accounts * initial}
	st.minSeen.Store(1 << 62)
	ctx, cancel := context.WithTimeout(context.Background(), runFor)
	defer cancel()
	sel := "SELECT balance FROM accounts WHERE id = " + ph(kind, 1)
	upd := "UPDATE accounts SET balance = " + ph(kind, 1) + " WHERE id = " + ph(kind, 2)

	var wg sync.WaitGroup
	for range writers {
		wg.Go(func() {
			for ctx.Err() == nil {
				from, to := rand.IntN(accounts), rand.IntN(accounts-1)
				if to >= from {
					to++
				}
				retry(ctx, db, iso, st, func(tx *sql.Tx) error {
					var bf, bt int64
					if err := tx.QueryRowContext(ctx, sel, from).Scan(&bf); err != nil {
						return err
					}
					if err := tx.QueryRowContext(ctx, sel, to).Scan(&bt); err != nil {
						return err
					}
					if bf <= 0 {
						return nil
					}
					amt := rand.Int64N(bf) + 1
					if _, err := tx.ExecContext(ctx, upd, bf-amt, from); err != nil {
						return err
					}
					_, err := tx.ExecContext(ctx, upd, bt+amt, to)
					return err
				})
			}
		})
	}
	for range readers {
		wg.Go(func() {
			for ctx.Err() == nil {
				var pass [2]int64
				ok := retry(ctx, db, iso, st, func(tx *sql.Tx) error {
					for p := range pass {
						pass[p] = 0
						for i := range accounts {
							var b int64
							if err := tx.QueryRowContext(ctx, sel, i).Scan(&b); err != nil {
								return err
							}
							pass[p] += b
						}
					}
					return nil
				})
				if ok {
					st.reads.Add(1)
					st.observe(pass[0])
					if pass[0] != pass[1] {
						st.unstable.Add(1)
					}
					if pass[0] != st.expectedTotal {
						st.wrong.Add(1)
					}
				}
			}
		})
	}
	wg.Wait()
	fmt.Sscan(L.QueryString("SELECT sum(balance) FROM accounts"), &st.finalTotal)
	return st
}

func TestBank(t *testing.T) {
	type row struct {
		kind  lab.Kind
		level lab.Level
		st    *stats
	}
	var rows []row
	for _, kind := range []lab.Kind{lab.Postgres, lab.MySQL} {
		for _, level := range lab.AllLevels {
			t.Run(kind.String()+"/"+level.Short(), func(t *testing.T) {
				st := runBank(t, kind, level)
				rows = append(rows, row{kind, level, st})
				// Safety must hold at the levels that promise it. Weaker levels are expected to
				// show anomalies, but that is probabilistic, so they are only reported.
				strong := level == lab.Serializable || (kind == lab.Postgres && level == lab.RepeatableRead)
				if strong && st.finalTotal != st.expectedTotal {
					t.Errorf("final total %d, want %d", st.finalTotal, st.expectedTotal)
				}
				if strong && st.wrong.Load() > 0 {
					t.Errorf("%d reads saw a total other than %d", st.wrong.Load(), st.expectedTotal)
				}
				// Every snapshot-based level must give both passes the same view.
				snapshot := level == lab.RepeatableRead || (kind == lab.Postgres && level == lab.Serializable)
				if (snapshot || strong) && st.unstable.Load() > 0 {
					t.Errorf("%d read transactions saw different totals in their two passes", st.unstable.Load())
				}
				if st.commits.Load() == 0 {
					t.Errorf("no transaction committed")
				}
			})
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\nbank: %d accounts × %d, %d writers + %d readers, %v per cell\n\n", accounts, initial, writers, readers, runFor)
	b.WriteString("| DB | Level | commits | retries (40001/40P01/1213) | unstable reads | reads with total ≠ 500 | totals seen | final total |\n|---|---|---|---|---|---|---|---|\n")
	for _, r := range rows {
		st := r.st
		seen := fmt.Sprintf("%d…%d", st.minSeen.Load(), st.maxSeen.Load())
		if st.reads.Load() == 0 {
			seen = "-"
		}
		final := fmt.Sprint(st.finalTotal)
		if st.finalTotal != st.expectedTotal {
			final += " ✗"
		}
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %d / %d | %d / %d | %s | %s |\n", r.kind, r.level.Short(),
			st.commits.Load(), st.retries.Load(), st.unstable.Load(), st.reads.Load(),
			st.wrong.Load(), st.reads.Load(), seen, final)
	}
	t.Log(b.String())
}
