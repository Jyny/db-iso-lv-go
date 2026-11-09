# db-iso-lv-go — PostgreSQL 18 / MySQL 9.7 isolation level, MVCC, lock and deadlock lab

Go tests that turn "textbook interleavings" into experiments that are **re-runnable and checked automatically**.
Every conclusion is backed by a test; run it with `-v` to see the full timeline and the DB's internal state (`pg_locks`, `pageinspect`, `performance_schema.data_locks`, …).

| Component | Version (latest stable, 2026-11) |
|---|---|
| PostgreSQL | **18.6** (19 is still an rc) |
| MySQL | **9.7.4 LTS** (26.x is an innovation release) |
| Go | 1.27, drivers: pgx v5, go-sql-driver/mysql |

```bash
make up          # docker-compose starts PG (55432) + MySQL (53306)
make test        # all experiments (about 50 seconds)
make hermitage   # isolation-level anomaly matrix
go test -p 1 -count=1 -v ./mysql -run 'TestLock_GapLockOnMissingKey'   # timeline of one experiment
```

---

## 0. How the harness works (`internal/lab`)

- Each `Session` = one dedicated connection + one worker goroutine.
- `s.Do(sql)`: returns the result when the statement finishes. If it gets stuck on a lock, the harness **asks the DB to confirm it is really waiting for a lock** and returns immediately (`op.Blocked = true`), so the test can keep driving other sessions. When the statement is later unblocked, `↳ #N done` is printed.
- After every step the harness waits until every session is quiescent (idle, or confirmed waiting for a lock), so interleavings are **deterministic** without guessing with `sleep`.
- Lock-wait detection: PG uses `pg_stat_activity.wait_event_type = 'Lock'`; MySQL uses `performance_schema.data_lock_waits` plus the MDL wait states in the processlist.
- Transaction tracking: after a deadlock (and, on PG, after any error inside a transaction), later statements are skipped until the transaction ends, so nothing silently runs in autocommit mode. `ROLLBACK TO SAVEPOINT` runs normally and does not end the transaction.

Example timeline (`go test ./postgres -v -run DDLLockQueue`):

```
  1  TX │ BEGIN
  2  TX │ SELECT * FROM q   → (1)
  3          DDL │ ALTER TABLE q ADD COLUMN c int   ⏳ BLOCKED (waiting for lock)
  4                  RD │ SELECT * FROM q   ⏳ BLOCKED (waiting for lock)
     ┌─ pg_locks
     │ TX  relation      q          AccessShareLock          GRANTED
     │ DDL relation      q          AccessExclusiveLock      WAITING
     │ RD  relation      q          AccessShareLock          WAITING   ← queued behind the DDL!
     └─
  5  TX │ COMMIT
         DDL │ ↳ #3 done (waited 422ms)
                 RD │ ↳ #4 done (waited 411ms)   → (1,NULL)
```

---

## 1. MVCC under the hood

Both are MVCC, but they keep old versions in opposite places, and that explains almost every behavioral difference that follows.

| | PostgreSQL | MySQL InnoDB |
|---|---|---|
| Where old versions live | **In the heap itself**: UPDATE = write a new tuple + set `xmax` on the old one | **In the undo log**: the clustered index holds only the latest version; older ones are chained via `DB_ROLL_PTR` |
| Per-row version info | `xmin` (creator), `xmax` (deleter / locker), `ctid`, infomask hint bits | Hidden columns `DB_TRX_ID` (last writer), `DB_ROLL_PTR` |
| Snapshot | `pg_current_snapshot()` = `xmin:xmax:xip[]` | Read View = `m_low_limit_id / m_up_limit_id / m_ids[]` |
| When the snapshot is taken (RR) | First statement | First consistent read (or `WITH CONSISTENT SNAPSHOT`) |
| Reclaiming old versions | `VACUUM` (held back by long transactions → table bloat) | Purge thread (held back by long read views → history list grows) |
| Where row locks live | **In the tuple header** (`xmax` + infomask; a MultiXact when shared) | In-memory lock system (`data_locks`); freshly inserted rows have "implicit locks" |
| Which version UPDATE / DELETE read | **The snapshot** (RC re-checks the latest version after a lock wait: EvalPlanQual) | **Current read**: always the latest committed version, locked |

### PostgreSQL (`postgres/mvcc_test.go`)

| Experiment | What you see |
|---|---|
| `TestMVCC_UpdateCreatesNewTupleVersion` | `pageinspect` shows two versions on the same page: the old one has `t_xmax = updater's xid`, `t_ctid → (0,2)`, `hot = true`. After VACUUM, lp 1 becomes a `redirect` (HOT chain). |
| `TestMVCC_RollbackLeavesTupleOnDisk` | A rolled-back INSERT **is still on disk**; the first read sets the `xmin_a` (aborted) hint bit. |
| `TestMVCC_SnapshotTakenAtFirstStatement` | Data committed after `BEGIN` but before the first SELECT **is visible**; in the snapshot `4238:4240:4238`, the xip is T3, which is still running. |
| `TestMVCC_OldSnapshotBlocksVacuum` | 5 updates while an RR transaction is open → after VACUUM **all 6 versions remain**; VACUUM removes them only after the transaction ends. |
| `TestMVCC_RowLocksLiveInTupleHeader` | After `FOR SHARE`, `t_xmax = locker's xid` (nothing was deleted); two shared lockers → `pgrowlocks` shows `multi = true, xids = {a,b}`. |

### MySQL (`mysql/mvcc_test.go`)

| Experiment | What you see |
|---|---|
| `TestMVCC_ReadViewCreatedAtFirstRead` | Data another session commits after `START TRANSACTION` **is visible** to the first SELECT; with `WITH CONSISTENT SNAPSHOT` it is not. |
| `TestMVCC_SnapshotReadVsCurrentRead` | In one transaction: `SELECT` → 0, `SELECT … FOR SHARE` → 5, `SELECT` again → 0. UPDATE computes from the current version. |
| `TestMVCC_RR_PhantomAppearsAfterUpdate` | **MySQL RR's famous phantom**: a row invisible to the snapshot is still changed by `UPDATE … WHERE` (1 row), and then "appears" in later SELECTs. The PG counterpart `TestIso_RR_UpdateUsesSnapshotNoPhantom` updates 0 rows. |
| `TestMVCC_TrxIdAssignedOnFirstWrite` | A read-only transaction's `trx_id` is a fake id ≥ 2⁴⁸ (`562947782929624`); it gets a real one (`23423`) on its first write. |
| `TestMVCC_LongReadViewBlocksPurge` | 200 commits while a read view is open → history list length `42 → 242`; after the read view closes, purge brings it to `0`. |

---

## 2. Locks

### PostgreSQL: the four row lock modes (`TestLock_RowLockConflictMatrix`, measured)

| Held \ Requested | KEY SHARE | SHARE | NO KEY UPDATE | UPDATE |
|---|---|---|---|---|
| **FOR KEY SHARE** | ok | ok | ok | BLOCK |
| **FOR SHARE** | ok | ok | BLOCK | BLOCK |
| **FOR NO KEY UPDATE** | ok | BLOCK | BLOCK | BLOCK |
| **FOR UPDATE** | BLOCK | BLOCK | BLOCK | BLOCK |

- A plain `UPDATE` that does not change key columns takes only `FOR NO KEY UPDATE`; an FK check takes only `FOR KEY SHARE`, so **updating a non-key column of the parent does not block child INSERTs** (`TestLock_ForeignKeyAndNoKeyUpdate`).
- 🔎 **Found by measurement**: `UPDATE parent SET id = 1 WHERE id = 1` (key set to the same value) **is still NO KEY UPDATE**. PG compares old and new key values and upgrades to `FOR UPDATE` only if they actually change.
- In `pg_locks`, a PG row-lock wait looks like this: the waiter requests `ShareLock` on the **holder's transactionid** (WAITING), plus a queued `tuple` lock. The row lock itself is not in `pg_locks`; it lives in the tuple header.
- **DDL lock queue** (`TestLock_DDLLockQueue`): a waiting `ACCESS EXCLUSIVE` makes every later SELECT queue behind it. This is the most common production "one ALTER took the whole site down". Fix: `SET lock_timeout = '…'` and retry.
- `NOWAIT` / `SKIP LOCKED` (job queues), waiting on another transaction's unique insert, and advisory locks each have an experiment.

### MySQL InnoDB: record / gap / next-key (`mysql/locks_test.go`, table `t(id PK, k non-unique index)` with values 10/20/30)

`data_locks.LOCK_MODE` vocabulary:

| LOCK_MODE | Meaning |
|---|---|
| `X,REC_NOT_GAP` | Record lock: locks only this index record |
| `X,GAP` | Gap lock: locks the open interval **before** this record, not the record itself |
| `X` | Next-key lock = record + the gap before it, i.e. `(prev, rec]` |
| `X,GAP,INSERT_INTENTION` | Wants to insert into a gap but is blocked by someone else's gap lock |
| `supremum pseudo-record` | The +∞ of a page; X on it = locked up to infinity |

| Experiment | Statement | Locks measured | Effect |
|---|---|---|---|
| `RecordLockOnPK` | `id = 20 FOR UPDATE` (exists) | `X,REC_NOT_GAP 20` | Locks only that row; neighbors can be inserted |
| `GapLockOnMissingKey` | `id = 15 FOR UPDATE` (missing) | `X,GAP 20` → locks (10,20) | **Another transaction can take the same gap lock**; only INSERT is blocked |
| `NextKeyRangeOnPK` | `id BETWEEN 15 AND 25 FOR UPDATE` | `X 20` + `X,GAP 30` | Blocks inserts in (10,30); 30 itself and >30 are not blocked (fixed in 8.0.18+) |
| `NonUniqueSecondaryIndex` | `k = 20 FOR UPDATE` | `X (20,20)` + `X,GAP (30,30)` + PK `X,REC_NOT_GAP 20` | Both k=15 and k=25 blocked: a non-unique index must lock the gaps on both sides to prevent phantoms |
| `NoIndexLocksEverything/RR` | `UPDATE … WHERE v = 20` (v unindexed) | `X` on every row + `supremum` | Effectively a table lock; even INSERT is blocked |
| `NoIndexLocksEverything/RC` | Same | Only the matching row remains | RC releases non-matching rows (semi-consistent read) |
| `RC_NoGapLocks` | `BETWEEN 10 AND 30 FOR UPDATE` under RC | Record locks only | Others can insert 15 → a phantom appears on the second query |
| `PlainSelectIsNonLocking` | Plain SELECT while another session holds X | RR: no wait, reads undo; **SER: becomes `S,REC_NOT_GAP` and waits** | |
| `DuplicateKeyCheckWaits` | Inserting the same PK as another uncommitted insert | A freshly inserted row has an **implicit lock** (not in data_locks); on conflict it becomes an explicit `X,REC_NOT_GAP` and the waiter takes `S` | |
| `MetadataLockQueue` | Open transaction + `ALTER … ALGORITHM=INSTANT` | MDL `SHARED_READ` vs `EXCLUSIVE` | Same lock-queue problem as PG; `lock_wait_timeout` is the MDL timeout |

🔎 **Found by measurement**: for the implicit→explicit conversion above, `performance_schema.data_locks.THREAD_ID` shows **the thread that triggered the conversion (T2)**, while `ENGINE_TRANSACTION_ID` is T1's. Attribute locks by transaction id; the harness does this.

---

## 3. Deadlocks

### PostgreSQL (`postgres/deadlock_test.go`)

PG does not check for deadlocks immediately: detection runs only after a lock wait exceeds `deadlock_timeout` (default 1s, 100ms in these experiments), and **the process that runs detection and finds the cycle becomes the victim itself**. The error is `40P01`, and DETAIL lists the cycle:

```
Process 3121 waits for ShareLock on transaction 4370; blocked by process 3119.
Process 3119 waits for ShareLock on transaction 4371; blocked by process 3120.
Process 3120 waits for ShareLock on transaction 4372; blocked by process 3121.
```

| Case | Cause |
|---|---|
| `OppositeOrder` | Transfers A→B and B→A lock rows in opposite order |
| `AvoidedByLockOrdering` | Fix: lock everything up front in order with `ORDER BY id FOR UPDATE` → transactions queue instead of deadlocking |
| `ShareLockUpgrade` | Both take `FOR SHARE`, then both UPDATE (S→X upgrade) |
| `UniqueInsertCrossed` | Cross-inserting the same unique keys; each waits for the other's outcome |
| `TableLockUpgrade` | Both `LOCK TABLE IN SHARE MODE`, then both INSERT (needs ROW EXCLUSIVE) |
| `ThreeWayCycle` | Three-way cycle T1→T2→T3→T1 |

### MySQL (`mysql/deadlock_test.go`, cases from [mysql-deadlocks-case](https://github.com/alleycat1/mysql-deadlocks-case), originally reproduced on 5.7)

InnoDB detects a deadlock **immediately**, the moment a wait would close a cycle, and rolls back the transaction with the smaller weight (fewer rows changed/locked). The **whole transaction** is rolled back (`1213`). The tests print the LATEST DETECTED DEADLOCK excerpt from `SHOW ENGINE INNODB STATUS`.

| Case | Source | Measured on 9.7 | Cause |
|---|---|---|---|
| `OppositeOrder` | #8 | 💥 deadlock | Opposite lock order |
| `CheckThenInsert_GapLocks` | #1 | 💥 deadlock | **The most common one**: "`SELECT … FOR UPDATE` to check it doesn't exist, then INSERT". Both get the same gap lock (compatible); their insert-intention locks block each other |
| `DuplicateInsertThenRollback` | #2 / reference manual | 💥 deadlock | Three sessions insert the same unique key; after T1 rolls back, T2 and T3 both hold S and both want X |
| `NonUniqueSecondary_DeleteInsertGap` | #12 | 💥 deadlock | The delete's next-key range and the gap needed by its own later insert are covered by the other side's **waiting** request |
| `UniqueSecondary_InsertInsert` | #15 | 💥 deadlock | The S next-key taken while waiting on a unique-key conflict covers the gap of the other side's next insert |
| `Serializable_ReadThenUpdate` | — | 💥 deadlock | SERIALIZABLE SELECT takes an implicit S lock → S→X upgrade |
| `DeleteThenReinsertSameKey` | #18 | ✅ **no longer deadlocks** | Deadlocks on 5.7. On 9.7 T1 already holds X on its own delete-marked record, so the re-insert's duplicate check does not queue behind T2 |
| `UniqueSecondary_DeleteInsert` | #4 | ✅ **no longer deadlocks** | Same as above; the re-insert only takes `S,GAP` around its own record |
| `UpdateUniqueKey_ThreeSessions` | #11 | ✅ **no longer deadlocks** | On 9.7 T2/T3 take a compatible `X,GAP` instead of an S that later has to be upgraded |

👉 **Most MySQL deadlock cases online date from 5.7. Don't assume they still hold on 8.x/9.x; re-running them with this harness is the reliable check.**

---

## 4. Isolation levels: the Hermitage matrix (`hermitage/`, measured)

Every [Hermitage](https://github.com/ept/hermitage) anomaly runs automatically on each DB × each isolation level. ✗ = the anomaly occurs; ✓ = prevented, followed by **what prevents it** (every cell, mechanism included, is asserted by the test, and the result is deterministic):

- `mvcc`: the snapshot means the transaction never sees the other's writes
- `lock`: blocked by a lock and forced to queue (effectively serial execution)
- `abort`: the DB detects the conflict and returns a serialization failure (`40001`)
- `deadlock`: locks form a deadlock and the DB kills one transaction

| DB | Level | G0 | G1a | G1b | G1c | OTV | PMP | PMP-write | P4 | G-single | G-single-write | G2-item | G2 | RO-anomaly |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| postgres | RU | ✓ lock | ✓ mvcc | ✓ mvcc | ✓ mvcc | ✓ lock | ✗ | ✗ | ✗ | ✗ | ✗ | ✗ | ✗ | ✗ |
| postgres | RC | ✓ lock | ✓ mvcc | ✓ mvcc | ✓ mvcc | ✓ lock | ✗ | ✗ | ✗ | ✗ | ✗ | ✗ | ✗ | ✗ |
| postgres | RR | ✓ abort | ✓ mvcc | ✓ mvcc | ✓ mvcc | ✓ abort | ✓ mvcc | ✓ abort | ✓ abort | ✓ mvcc | ✓ abort | ✗ | ✗ | ✗ |
| postgres | SER | ✓ abort | ✓ mvcc | ✓ mvcc | ✓ abort | ✓ abort | ✓ mvcc | ✓ abort | ✓ abort | ✓ mvcc | ✓ abort | ✓ abort | ✓ abort | ✓ abort |
| mysql | RU | ✓ lock | ✗ | ✗ | ✗ | ✗ | ✗ | ✓ lock* | ✗ | ✗ | ✗ | ✗ | ✗ | ✗ |
| mysql | RC | ✓ lock | ✓ mvcc | ✓ mvcc | ✓ mvcc | ✓ lock | ✗ | ✗ | ✗ | ✗ | ✗ | ✗ | ✗ | ✗ |
| mysql | RR | ✓ lock | ✓ mvcc | ✓ mvcc | ✓ mvcc | ✓ lock | ✓ mvcc | ✗ | ✗ | ✓ mvcc | ✗ | ✗ | ✗ | ✗ |
| mysql | SER | ✓ lock | ✓ lock | ✓ lock | ✓ deadlock | ✓ lock | ✓ lock | ✓ lock | ✓ deadlock | ✓ lock | ✓ deadlock | ✓ deadlock | ✓ deadlock | ✓ deadlock |

\* MySQL RU's PMP-write is correct **by accident**: T2 dirty-reads T1's uncommitted value, and the result happens to equal the serial order T1→T2. RU is not safe; see G1a.

| Anomaly | In plain words |
|---|---|
| G0 dirty write | Overwriting someone else's uncommitted write |
| G1a/b/c dirty read | Reading a value that was rolled back / an intermediate value / each reading the other's uncommitted value |
| OTV | After seeing T2's write, seeing an old value T2 already overwrote |
| PMP phantom | The same predicate queried twice returns an extra row someone else inserted |
| PMP-write | A DELETE/UPDATE predicate evaluated on a mix of old and new versions (the `website.hits` example from the PG docs) |
| P4 lost update | Read-modify-write where one of the updates disappears |
| G-single read skew | Reading x from before a transaction commits and y from after |
| G2-item write skew | Each reads two rows and updates a different one, breaking a cross-row invariant (on-call doctors) |
| G2 | Same, but with a predicate query + INSERT |
| RO-anomaly | Fekete 2004: even a **read-only** transaction can observe an impossible state; only true serializability prevents it |

### Key takeaways from the matrix

1. **Same name, different guarantees.** PG's RR is Snapshot Isolation (prevents P4 and G-single); MySQL's RR **does not even prevent lost updates**. The Hermitage author considers MySQL RR closer to "Monotonic Atomic View".
2. **PG relies on failing, MySQL on waiting.** PG RR/SER are almost all `abort`, so applications **must retry**; MySQL SER is almost all `lock`/`deadlock`: lower throughput, but intuitive semantics.
3. **PG's RU = RC**; PG has no dirty reads.
4. **MySQL RR's PMP is ✓ mvcc, but PMP-write is ✗**: pure reads are protected by the snapshot, but a write switches to a current read and sees outside the snapshot (see the phantom experiment in §1).
5. To prevent write skew / lost updates on MySQL, lock when reading (`FOR UPDATE`); see `mysql/TestIso_RR_WriteSkewAndForUpdateFix` and `TestIso_RR_LostUpdateAndFixes`.

### Other isolation experiments

| Test | Point |
|---|---|
| `postgres/TestIso_RC_EvalPlanQualDocsExample` | The docs example: after a lock wait, PG re-checks only "the rows its own snapshot found", so `DELETE WHERE hits = 10` deletes 0 rows even though a row with hits=10 exists at that point |
| `postgres/TestIso_RC_InPlaceIncrementIsSafe` | `SET n = n + 1` is safe under RC; computing in the client and writing back loses updates |
| `postgres/TestIso_RR_FirstUpdaterWinsAndRetry` | Under RR, updating a row someone else changed → `40001`, retry the whole transaction; if the first updater rolls back, the waiter continues normally |
| `postgres/TestIso_WriteSkew_OnCallDoctors` | RR: both doctors go off call (0 on call); SER: the second COMMIT fails |
| `postgres/TestIso_SER_FalsePositiveFromSeqScan` | **SSI false positive**: a seq scan takes a relation-level SIRead lock, so two transactions working on unrelated groups still get `40001`; an index scan avoids it |
| `mysql/TestIso_SER_AutocommitSelectStaysNonLocking` | MySQL SERIALIZABLE turns SELECTs into locking reads only **inside explicit transactions**; an autocommit SELECT stays a snapshot read |

### Jepsen-style randomized test (`jepsen/`, measured)

Hermitage is "one hand-picked interleaving"; [Jepsen](https://github.com/jepsen-io/jepsen) instead runs **many concurrent random transactions and checks invariants over the whole history**. This implements Jepsen's classic **bank test**:

- 8 writers make random transfers: read two balances → compute in Go → write back (client-side read-modify-write)
- 2 readers SELECT each of the 5 accounts and sum them **twice in the same transaction**
- Deadlocks / serialization failures retry the whole transaction (as a correct application must)

| DB | Level | commits | retries | unstable reads (two sums differ) | reads with total ≠ 500 | final total |
|---|---|---|---|---|---|---|
| postgres | RU | 1575 | 21 | 39 / 1511 | 1409 / 1511 | 741 ✗ |
| postgres | RC | 1609 | 25 | 47 / 1520 | 1519 / 1520 | 804 ✗ |
| postgres | RR | 1455 | 457 | **0** | **0** | **500** |
| postgres | SER | 1546 | 566 | **0** | **0** | **500** |
| mysql | RU | 2441 | 80 | 607 / 845 | 845 / 845 | 4 ✗ |
| mysql | RC | 1934 | 107 | 813 / 937 | 934 / 937 | 290000 ✗ |
| mysql | RR | 1964 | 91 | **0** | 918 / 925 | **2739 ✗** |
| mysql | SER | 615 | 1665 | **0** | **0** | **500** |

(Each cell runs for 1.5 seconds. The numbers change every run, but the ✗ pattern is stable: across 4 consecutive clean runs, PG RR/SER and MySQL SER were always 0 / 0 / 500, and MySQL RR always had 0 unstable reads with a final total away from 500.)

- **MySQL RR is the one to look at**: 0 unstable reads, a perfectly consistent snapshot, yet the total drifted from 500 to 2739, because RR does not prevent lost updates (Hermitage P4). A consistent snapshot is not the same as correct data.
- **PG RR stays correct by aborting**: 457 retries buy an unchanged total (first-updater-wins).
- **MySQL SER stays correct by locking**, but with 1665 deadlock retries and about 1/3 the commits of the other levels. That is the cost of serializability by locking.
- Anomalies at weak levels are probabilistic, so the test asserts only the levels that guarantee correctness (PG RR/SER, MySQL SER); weak levels are only reported.

---

## 5. Pitfalls hit while building this (also worth knowing)

1. **`information_schema.innodb_trx` is a cache**: it refreshes only if it has not been read for more than 100ms. High-frequency polling keeps it stale forever (the harness first used it for lock-wait detection, missed every wait, and the tests ran 2 minutes longer). Use `performance_schema.data_lock_waits`.
2. **Match the processlist STATE case-sensitively**: `waiting for handler commit` is a normal commit, not a lock wait.
3. **A MySQL deadlock victim's whole transaction is already rolled back**, and later statements run in autocommit mode. An application (or test harness) that misses this writes data while believing it is still inside a transaction.
4. **History list length and SSI predicate locks are server-wide**, so they interfere with other workloads. `make test` uses `-p 1` to run packages one at a time.

---

## 6. Layout

```
internal/lab/        multi-session interleaving harness (Session, Op, lock-wait detection, lock inspection, error classification)
hermitage/           Hermitage anomaly matrix (PG + MySQL × 4 levels × 13 anomalies)
jepsen/              Jepsen-style bank test: concurrent random transfers + invariant checks
postgres/            mvcc_test / locks_test / deadlock_test / isolation_test
mysql/               mvcc_test / locks_test / deadlock_test / isolation_test
docker-compose.yml   postgres:18.6, mysql:9.7.4
```

Connection strings can be overridden with `LAB_PG_DSN` / `LAB_MYSQL_DSN` (`%s` is the database name).

## References

- PostgreSQL 18 — [Transaction Isolation](https://www.postgresql.org/docs/18/transaction-iso.html), [Explicit Locking](https://www.postgresql.org/docs/18/explicit-locking.html)
- MySQL 8.4 — [InnoDB Transaction Isolation Levels](https://dev.mysql.com/doc/refman/8.4/en/innodb-transaction-isolation-levels.html), [InnoDB Locking](https://dev.mysql.com/doc/refman/8.4/en/innodb-locking.html)
- Martin Kleppmann — [Hermitage: Testing the "I" in ACID](https://martin.kleppmann.com/2014/11/25/hermitage-testing-the-i-in-acid.html), [ept/hermitage](https://github.com/ept/hermitage)
- Cockroach Labs — [SQL isolation levels explained](https://www.cockroachlabs.com/blog/sql-isolation-levels-explained/)
- [jepsen-io/jepsen](https://github.com/jepsen-io/jepsen), [alleycat1/mysql-deadlocks-case](https://github.com/alleycat1/mysql-deadlocks-case)
- Fekete, O'Neil, O'Neil — *A Read-Only Transaction Anomaly Under Snapshot Isolation* (2004)
- Ports & Grittner — *Serializable Snapshot Isolation in PostgreSQL* (VLDB 2012)
