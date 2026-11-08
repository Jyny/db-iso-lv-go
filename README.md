# db-iso-lv-go — PostgreSQL 18 / MySQL 9.7 隔離等級、MVCC、鎖與死鎖實驗室

用 Go 測試把「教科書上的交錯執行」變成**可重跑、可自動判定**的實驗。
每個結論都對應一個測試，用 `-v` 跑就能看到完整時間軸與 DB 內部狀態（`pg_locks`、`pageinspect`、`performance_schema.data_locks`…）。

| 元件 | 版本（2026-11 最新穩定版） |
|---|---|
| PostgreSQL | **18.6**（19 仍是 rc） |
| MySQL | **9.7.4 LTS**（26.x 是 innovation release） |
| Go | 1.27，驅動：pgx v5、go-sql-driver/mysql |

```bash
make up          # docker-compose 起 PG(55432) + MySQL(53306)
make test        # 全部實驗（約 20 秒）
make hermitage   # 隔離等級異常矩陣
go test -p 1 -count=1 -v ./mysql -run 'TestLock_GapLockOnMissingKey'   # 看單一實驗的時間軸
```

---

## 0. 實驗框架怎麼運作（`internal/lab`）

- 每個 `Session` = 一條專屬連線 + 一個 worker goroutine。
- `s.Do(sql)`：指令完成就回傳結果；若被鎖擋住，框架**向 DB 確認它真的在等鎖**後立即回傳（`op.Blocked = true`），測試可繼續操作其它 session。之後解除阻塞會印出 `↳ #N done`。
- 每一步之後等待所有 session「靜止」（閒置或確定在等鎖），所以交錯順序是**決定性的**，不靠 `sleep` 猜時間。
- 等鎖偵測：PG 看 `pg_stat_activity.wait_event_type = 'Lock'`；MySQL 看 `performance_schema.data_lock_waits` + processlist 的 MDL 等待狀態。

時間軸範例（`go test ./postgres -v -run DDLLockQueue`）：

```
  1  TX │ BEGIN
  2  TX │ SELECT * FROM q   → (1)
  3          DDL │ ALTER TABLE q ADD COLUMN c int   ⏳ BLOCKED (waiting for lock)
  4                  RD │ SELECT * FROM q   ⏳ BLOCKED (waiting for lock)
     ┌─ pg_locks
     │ TX  relation      q          AccessShareLock          GRANTED
     │ DDL relation      q          AccessExclusiveLock      WAITING
     │ RD  relation      q          AccessShareLock          WAITING   ← 排在 DDL 後面！
     └─
  5  TX │ COMMIT
         DDL │ ↳ #3 done (waited 422ms)
                 RD │ ↳ #4 done (waited 411ms)   → (1,NULL)
```

---

## 1. MVCC under the hood

兩者都是 MVCC，但「舊版本放哪裡」完全相反，這決定了後面幾乎所有行為差異。

| | PostgreSQL | MySQL InnoDB |
|---|---|---|
| 舊版本存放 | **在 heap 本身**：UPDATE = 寫一個新 tuple + 在舊 tuple 填 `xmax` | **在 undo log**：聚簇索引只放最新版，舊版靠 `DB_ROLL_PTR` 串成鏈 |
| 每列的版本資訊 | `xmin`（建立者）、`xmax`（刪除/鎖定者）、`ctid`、infomask hint bits | 隱藏欄位 `DB_TRX_ID`（最後寫入者）、`DB_ROLL_PTR` |
| 快照 | `pg_current_snapshot()` = `xmin:xmax:xip[]` | Read View = `m_low_limit_id / m_up_limit_id / m_ids[]` |
| 快照建立時機（RR） | 第一個語句 | 第一個 consistent read（或 `WITH CONSISTENT SNAPSHOT`） |
| 回收舊版本 | `VACUUM`（會被長交易擋住 → 表膨脹） | purge thread（會被長 read view 擋住 → history list 變長） |
| 列鎖存放 | **tuple header 本身**（`xmax` + infomask；多人共享時變 MultiXact） | 記憶體中的 lock system（`data_locks`），新插入的列是「隱式鎖」 |
| UPDATE / DELETE 讀哪個版本 | **快照**（RC 會在等鎖後 re-check 最新版：EvalPlanQual） | **current read**：永遠讀最新 committed 版本並上鎖 |

### PostgreSQL（`postgres/mvcc_test.go`）

| 實驗 | 你會看到 |
|---|---|
| `TestMVCC_UpdateCreatesNewTupleVersion` | `pageinspect` 顯示同一頁上兩個版本：舊的 `t_xmax = 更新者 xid`、`t_ctid → (0,2)`、`hot = true`。VACUUM 後 lp 1 變成 `redirect`（HOT chain）。 |
| `TestMVCC_RollbackLeavesTupleOnDisk` | ROLLBACK 的 INSERT **仍在磁碟上**，第一次被讀到時設 `xmin_a`（aborted）hint bit。 |
| `TestMVCC_SnapshotTakenAtFirstStatement` | `BEGIN` 之後、第一個 SELECT 之前提交的資料**看得到**；快照 `4238:4240:4238` 的 xip 就是仍在進行的 T3。 |
| `TestMVCC_OldSnapshotBlocksVacuum` | 開著一個 RR 交易時更新 5 次 → VACUUM 後 6 個版本**全留著**；交易結束後 VACUUM 才清掉。 |
| `TestMVCC_RowLocksLiveInTupleHeader` | `FOR SHARE` 後 `t_xmax = 鎖定者 xid`（沒刪任何東西）；兩人共享鎖 → `pgrowlocks` 顯示 `multi = true, xids = {a,b}`。 |

### MySQL（`mysql/mvcc_test.go`）

| 實驗 | 你會看到 |
|---|---|
| `TestMVCC_ReadViewCreatedAtFirstRead` | `START TRANSACTION` 後別人提交的資料，第一次 SELECT **看得到**；`WITH CONSISTENT SNAPSHOT` 則看不到。 |
| `TestMVCC_SnapshotReadVsCurrentRead` | 同一交易內：`SELECT` → 0、`SELECT … FOR SHARE` → 5、再 `SELECT` → 0。UPDATE 以 current 版本計算。 |
| `TestMVCC_RR_PhantomAppearsAfterUpdate` | **MySQL RR 的著名幻讀**：快照看不到的列，`UPDATE … WHERE` 卻改到了（1 row），之後 SELECT 就「冒出來」了。PG 的對照組 `TestIso_RR_UpdateUsesSnapshotNoPhantom` 會更新 0 列。 |
| `TestMVCC_TrxIdAssignedOnFirstWrite` | 唯讀交易的 `trx_id` 是 ≥ 2⁴⁸ 的假 id（`562947782929624`），第一次寫入才拿到真的（`23423`）。 |
| `TestMVCC_LongReadViewBlocksPurge` | 開著 read view 做 200 次提交 → History list length `42 → 242`；read view 關掉後 purge 清到 `0`。 |

---

## 2. 鎖

### PostgreSQL：四種列鎖（`TestLock_RowLockConflictMatrix`，實測）

| 持有 \ 請求 | KEY SHARE | SHARE | NO KEY UPDATE | UPDATE |
|---|---|---|---|---|
| **FOR KEY SHARE** | ok | ok | ok | BLOCK |
| **FOR SHARE** | ok | ok | BLOCK | BLOCK |
| **FOR NO KEY UPDATE** | ok | BLOCK | BLOCK | BLOCK |
| **FOR UPDATE** | BLOCK | BLOCK | BLOCK | BLOCK |

- 一般 `UPDATE`（沒改 key 欄位）只拿 `FOR NO KEY UPDATE`；FK 檢查只拿 `FOR KEY SHARE`，所以**更新父表的非 key 欄位不會擋住子表 INSERT**（`TestLock_ForeignKeyAndNoKeyUpdate`）。
- 🔎 **實測發現**：`UPDATE parent SET id = 1 WHERE id = 1`（key 設成同一個值）**仍是 NO KEY UPDATE**。PG 比較新舊 key 值，只有真的改變才升級成 `FOR UPDATE`。
- PG 的列鎖等待在 `pg_locks` 裡長這樣：等待者對**持有者的 transactionid** 要 `ShareLock`（WAITING），外加一個 `tuple` 鎖排隊。列鎖本身不在 `pg_locks`，而在 tuple header。
- **DDL lock queue**（`TestLock_DDLLockQueue`）：等待中的 `ACCESS EXCLUSIVE` 會讓後面所有 SELECT 一起排隊，這是 production 最常見的「一個 ALTER 把整站打掛」。解法：`SET lock_timeout = '…'` 後重試。
- `NOWAIT` / `SKIP LOCKED`（job queue）、unique insert 等待對方結果、advisory lock 各有一個實驗。

### MySQL InnoDB：record / gap / next-key（`mysql/locks_test.go`，表 `t(id PK, k 非唯一索引)` 值 10/20/30）

`data_locks.LOCK_MODE` 的語彙：

| LOCK_MODE | 意義 |
|---|---|
| `X,REC_NOT_GAP` | record lock：只鎖這筆索引記錄 |
| `X,GAP` | gap lock：鎖「這筆記錄**之前**」的開區間，不含記錄本身 |
| `X` | next-key lock = record + 前面的 gap，即 `(prev, rec]` |
| `X,GAP,INSERT_INTENTION` | 想插入某個 gap、但被別人的 gap lock 擋住 |
| `supremum pseudo-record` | 頁面上的 +∞，對它上 X = 鎖到無限大 |

| 實驗 | 語句 | 實測拿到的鎖 | 效果 |
|---|---|---|---|
| `RecordLockOnPK` | `id = 20 FOR UPDATE`（存在） | `X,REC_NOT_GAP 20` | 只鎖該列，旁邊可插入 |
| `GapLockOnMissingKey` | `id = 15 FOR UPDATE`（不存在） | `X,GAP 20` → 鎖 (10,20) | **另一個交易也能拿同一個 gap lock**；只有 INSERT 會被擋 |
| `NextKeyRangeOnPK` | `id BETWEEN 15 AND 25 FOR UPDATE` | `X 20` + `X,GAP 30` | 擋 (10,30) 內的插入；30 本身與 >30 不擋（8.0.18+ 修正） |
| `NonUniqueSecondaryIndex` | `k = 20 FOR UPDATE` | `X (20,20)` + `X,GAP (30,30)` + PK `X,REC_NOT_GAP 20` | k=15、k=25 都被擋：非唯一索引必須鎖兩側 gap 才能防幻讀 |
| `NoIndexLocksEverything/RR` | `UPDATE … WHERE v = 20`（v 無索引） | 每列 `X` + `supremum` | 等於鎖全表，連 INSERT 都擋 |
| `NoIndexLocksEverything/RC` | 同上 | 只剩符合的那一列 | RC 會釋放不符合的列（semi-consistent read） |
| `RC_NoGapLocks` | RC 下 `BETWEEN 10 AND 30 FOR UPDATE` | 只有 record lock | 別人可以插 15 → 第二次查詢出現幻影 |
| `PlainSelectIsNonLocking` | 別人持有 X 時的普通 SELECT | RR：不等、讀 undo；**SER：變成 `S,REC_NOT_GAP` 並等待** | |
| `DuplicateKeyCheckWaits` | 插入別人未提交的相同 PK | 新插入的列是**隱式鎖**（data_locks 看不到）；衝突時被轉成顯式 `X,REC_NOT_GAP`，等待者拿 `S` | |
| `MetadataLockQueue` | 開著的交易 + `ALTER … ALGORITHM=INSTANT` | MDL `SHARED_READ` vs `EXCLUSIVE` | 跟 PG 一樣的 lock queue 問題；`lock_wait_timeout` 是 MDL 的逾時 |

🔎 **實測發現**：上表的隱式→顯式轉換，`performance_schema.data_locks.THREAD_ID` 會顯示**觸發轉換的那個 thread（T2）**，但 `ENGINE_TRANSACTION_ID` 是 T1 的。看鎖要用 transaction id 對應，框架已處理。

---

## 3. 死鎖

### PostgreSQL（`postgres/deadlock_test.go`）

PG 不會立刻檢查死鎖：等鎖超過 `deadlock_timeout`（預設 1s，實驗設 100ms）才跑偵測，**跑偵測並發現環的那個 process 自己當犧牲者**。錯誤 `40P01`，DETAIL 會列出環：

```
Process 3121 waits for ShareLock on transaction 4370; blocked by process 3119.
Process 3119 waits for ShareLock on transaction 4371; blocked by process 3120.
Process 3120 waits for ShareLock on transaction 4372; blocked by process 3121.
```

| 案例 | 成因 |
|---|---|
| `OppositeOrder` | 轉帳 A→B 與 B→A 以相反順序鎖列 |
| `AvoidedByLockOrdering` | 解法：`ORDER BY id FOR UPDATE` 一次依序鎖好 → 只會排隊不會死鎖 |
| `ShareLockUpgrade` | 兩人都 `FOR SHARE`，再都 UPDATE（S→X 升級） |
| `UniqueInsertCrossed` | 交叉插入相同 unique key，互等對方交易結果 |
| `TableLockUpgrade` | 兩人 `LOCK TABLE IN SHARE MODE` 後都 INSERT（需 ROW EXCLUSIVE） |
| `ThreeWayCycle` | T1→T2→T3→T1 三方環 |

### MySQL（`mysql/deadlock_test.go`，案例取自 [mysql-deadlocks-case](https://github.com/alleycat1/mysql-deadlocks-case)，原作在 5.7 上重現）

InnoDB 在「等待會形成環」的當下**立即**偵測，回滾 weight 較小（改動/鎖定列較少）的交易，**整個交易**都被 rollback（`1213`）。測試會印出 `SHOW ENGINE INNODB STATUS` 的 LATEST DETECTED DEADLOCK 節錄。

| 案例 | 來源 | 9.7 實測 | 成因 |
|---|---|---|---|
| `OppositeOrder` | #8 | 💥 死鎖 | 相反順序 |
| `CheckThenInsert_GapLocks` | #1 | 💥 死鎖 | **最常見**：「先 `SELECT … FOR UPDATE` 確認不存在再 INSERT」，兩人都拿到同一 gap lock（相容），INSERT 的 insert-intention 互相被擋 |
| `DuplicateInsertThenRollback` | #2 / 官方手冊 | 💥 死鎖 | 三人插入同一 unique key，T1 rollback 後 T2、T3 都持 S 又都要 X |
| `NonUniqueSecondary_DeleteInsertGap` | #12 | 💥 死鎖 | delete 的 next-key 範圍與自己後續 insert 的 gap 被對方的**等待中**請求覆蓋 |
| `UniqueSecondary_InsertInsert` | #15 | 💥 死鎖 | 唯一鍵衝突等待的 S next-key 蓋住對方下一筆 insert 的 gap |
| `Serializable_ReadThenUpdate` | — | 💥 死鎖 | SERIALIZABLE 的 SELECT 隱含 S 鎖 → S→X 升級 |
| `DeleteThenReinsertSameKey` | #18 | ✅ **不再死鎖** | 5.7 會死鎖。9.7 中 T1 已持有自己 delete-marked 記錄的 X，重新插入的 duplicate check 不會排到 T2 後面 |
| `UniqueSecondary_DeleteInsert` | #4 | ✅ **不再死鎖** | 同上，重新插入只拿自己記錄周邊的 `S,GAP` |
| `UpdateUniqueKey_ThreeSessions` | #11 | ✅ **不再死鎖** | 9.7 中 T2/T3 拿的是相容的 `X,GAP` 而不是之後要升級的 S |

👉 **網路上的 MySQL 死鎖案例多半是 5.7 時代的，不要假設在 8.x/9.x 仍成立，拿這個框架重跑一次最準。**

---

## 4. 隔離等級：Hermitage 矩陣（`hermitage/`，實測）

[Hermitage](https://github.com/ept/hermitage) 的每個異常都在每個 DB × 每個隔離等級自動跑一次。✗ = 異常發生；✓ = 被防止，後面註明**靠什麼防止**：

- `mvcc`：快照讓交易根本看不到別人的寫入
- `lock`：被鎖擋住、被迫排隊（等於序列執行）
- `abort`：DB 偵測到衝突，回傳 serialization failure（`40001`）
- `deadlock`：鎖造成死鎖，DB 殺掉一個交易

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

\* MySQL RU 的 PMP-write 是**碰巧**正確：T2 髒讀到 T1 未提交的值，結果剛好等於序列 T1→T2。RU 不安全，看 G1a。

| 異常 | 白話 |
|---|---|
| G0 dirty write | 覆寫別人未提交的寫入 |
| G1a/b/c dirty read | 讀到被 rollback 的值 / 中間值 / 互相讀到對方未提交的值 |
| OTV | 看到 T2 的寫入後，又看到 T2 已覆蓋的舊值 |
| PMP phantom | 同一條件查兩次，多出別人新插入的列 |
| PMP-write | DELETE/UPDATE 的條件判斷混用新舊版本（PG 文件的 `website.hits` 範例） |
| P4 lost update | 讀-改-寫，其中一個更新消失 |
| G-single read skew | 讀到某交易提交「前」的 x 和提交「後」的 y |
| G2-item write skew | 各讀兩筆、各改不同一筆，破壞跨列不變式（值班醫生） |
| G2 | 同上但是用條件查詢 + INSERT |
| RO-anomaly | Fekete 2004：連**唯讀**交易都能看到不可能的狀態，只有真正的 serializable 能擋 |

### 讀矩陣的重點

1. **名字一樣，保證不同**。PG 的 RR 是 Snapshot Isolation（擋 P4、G-single），MySQL 的 RR 連 **lost update 都不擋**。Hermitage 作者認為 MySQL RR 更接近 "Monotonic Atomic View"。
2. **PG 靠「失敗」，MySQL 靠「等待」**。PG RR/SER 幾乎全是 `abort`，應用程式**必須寫重試**；MySQL SER 幾乎全是 `lock`/`deadlock`，吞吐量差但語意直觀。
3. **PG 的 RU = RC**，PG 不存在 dirty read。
4. **MySQL RR 的 PMP 是 ✓ mvcc，但 PMP-write 是 ✗**：純讀有快照保護，一旦寫入就變 current read，看到快照外的世界（見 §1 的 phantom 實驗）。
5. 想在 MySQL 防 write skew / lost update：讀的時候就上鎖（`FOR UPDATE`），見 `mysql/TestIso_RR_WriteSkewAndForUpdateFix`、`TestIso_RR_LostUpdateAndFixes`。

### 其他隔離等級實驗

| 測試 | 重點 |
|---|---|
| `postgres/TestIso_RC_EvalPlanQualDocsExample` | 官方文件範例：等鎖後只 re-check「自己快照找到的列」，所以 `DELETE WHERE hits = 10` 刪了 0 列，即使此時有一列 hits=10 |
| `postgres/TestIso_RC_InPlaceIncrementIsSafe` | `SET n = n + 1` 在 RC 是安全的；在 client 端算好再寫回就會 lost update |
| `postgres/TestIso_RR_FirstUpdaterWinsAndRetry` | RR 更新被別人改過的列 → `40001`，整個交易重試；先更新者若 rollback，等待者正常繼續 |
| `postgres/TestIso_WriteSkew_OnCallDoctors` | RR 兩位醫生都下班（0 人值班）；SER 第二個 COMMIT 失敗 |
| `postgres/TestIso_SER_FalsePositiveFromSeqScan` | **SSI 的 false positive**：seq scan 拿 relation 級 SIRead 鎖，兩個交易操作完全不相干的 group 仍會 `40001`；改用 index scan 就沒事 |
| `mysql/TestIso_SER_AutocommitSelectStaysNonLocking` | MySQL SERIALIZABLE 只把**顯式交易中**的 SELECT 變成鎖定讀；autocommit 的 SELECT 仍是快照讀 |

---

## 5. 做實驗時踩到的坑（也是知識點）

1. **`information_schema.innodb_trx` 是快取**：只有「距上次讀取超過 100ms」才會刷新。高頻輪詢會讓它永遠停在舊狀態（框架一開始用它偵測等鎖，結果偵測不到、測試多跑了 2 分鐘）。要用 `performance_schema.data_lock_waits`。
2. **processlist 的 STATE 比對要區分大小寫**：`waiting for handler commit` 是正常提交，不是等鎖。
3. **MySQL 死鎖犧牲者的整個交易已被 rollback**，之後的語句會以 autocommit 執行。應用程式（和測試框架）若沒注意，會在「以為還在交易中」的狀態下寫入資料。
4. **History list length、SSI 的 predicate lock 都是 server 全域的**，跟其他 workload 互相影響。`make test` 用 `-p 1` 讓 package 依序執行。

---

## 6. 目錄

```
internal/lab/        多 session 交錯執行框架（Session、Op、等鎖偵測、鎖觀察、錯誤分類）
hermitage/           Hermitage 異常矩陣（PG + MySQL × 4 levels × 13 anomalies）
postgres/            mvcc_test / locks_test / deadlock_test / isolation_test
mysql/               mvcc_test / locks_test / deadlock_test / isolation_test
docker-compose.yml   postgres:18.6、mysql:9.7.4
```

連線字串可用 `LAB_PG_DSN` / `LAB_MYSQL_DSN` 覆寫（`%s` 是資料庫名）。

## 參考

- PostgreSQL 18 — [Transaction Isolation](https://www.postgresql.org/docs/18/transaction-iso.html)、[Explicit Locking](https://www.postgresql.org/docs/18/explicit-locking.html)
- MySQL 8.4 — [InnoDB Transaction Isolation Levels](https://dev.mysql.com/doc/refman/8.4/en/innodb-transaction-isolation-levels.html)、[InnoDB Locking](https://dev.mysql.com/doc/refman/8.4/en/innodb-locking.html)
- Martin Kleppmann — [Hermitage: Testing the "I" in ACID](https://martin.kleppmann.com/2014/11/25/hermitage-testing-the-i-in-acid.html)、[ept/hermitage](https://github.com/ept/hermitage)
- Cockroach Labs — [SQL isolation levels explained](https://www.cockroachlabs.com/blog/sql-isolation-levels-explained/)
- [jepsen-io/jepsen](https://github.com/jepsen-io/jepsen)、[alleycat1/mysql-deadlocks-case](https://github.com/alleycat1/mysql-deadlocks-case)
- Fekete, O'Neil, O'Neil — *A Read-Only Transaction Anomaly Under Snapshot Isolation* (2004)
- Ports & Grittner — *Serializable Snapshot Isolation in PostgreSQL* (VLDB 2012)
