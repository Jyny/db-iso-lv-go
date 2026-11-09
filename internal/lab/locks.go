package lab

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

var (
	trxReadMu   sync.Mutex
	lastTrxRead time.Time
)

// FreshInnoDBTrx makes the next information_schema.innodb_trx read return fresh data.
// That table is served from a server-wide cache that is refreshed only when it has not
// been read for 100ms, so back-to-back reads would see stale transactions.
func FreshInnoDBTrx() {
	trxReadMu.Lock()
	defer trxReadMu.Unlock()
	if d := 120*time.Millisecond - time.Since(lastTrxRead); d > 0 {
		time.Sleep(d)
	}
	lastTrxRead = time.Now()
}

// Lock is one lock observed in the DB.
//
//	PG: from pg_locks. Type = locktype (relation / tuple / transactionid / advisory ...),
//	    Object = table name or xid, Mode = AccessShareLock / RowExclusiveLock / ShareLock / ExclusiveLock ...
//	MySQL: from performance_schema.data_locks. Type = TABLE / RECORD, Object = table.index,
//	    Mode = IX / X / S / X,GAP / X,REC_NOT_GAP / X,INSERT_INTENTION ..., Data = the locked index key
type Lock struct {
	Session string
	Type    string
	Object  string
	Mode    string
	Granted bool
	Data    string
}

func (k Lock) String() string {
	st := "GRANTED"
	if !k.Granted {
		st = "WAITING"
	}
	return fmt.Sprintf("%-3s %-13s %-22s %-24s %-8s %s", k.Session, k.Type, k.Object, k.Mode, st, k.Data)
}

// Locks lists the locks currently held or awaited by this experiment's sessions.
func (l *Lab) Locks() []Lock {
	l.T.Helper()
	if len(l.sessions) == 0 {
		return nil
	}
	names := map[string]string{}
	var ids []string
	for _, s := range l.sessions {
		id := fmt.Sprint(s.ID)
		names[id] = s.Name
		ids = append(ids, id)
	}
	in := strings.Join(ids, ",")
	var rows [][]string
	switch l.Kind {
	case Postgres:
		rows = l.Query(`
SELECT l.pid::text, l.locktype,
       CASE
         WHEN l.locktype = 'transactionid' THEN 'xid ' || l.transactionid::text
         WHEN l.locktype = 'advisory' THEN 'key ' || l.objid::text
         WHEN l.relation IS NOT NULL THEN l.relation::regclass::text
         ELSE '' END,
       l.mode, l.granted::text,
       CASE WHEN l.locktype = 'tuple' THEN 'ctid (' || l.page || ',' || l.tuple || ')' ELSE '' END
FROM pg_locks l
WHERE l.pid IN (` + in + `)
  AND l.locktype NOT IN ('virtualxid')
  AND (l.relation IS NULL OR l.relation::regclass::text NOT LIKE 'pg\_%')
ORDER BY l.pid, l.granted DESC, l.locktype, 3`)
	case MySQL:
		FreshInnoDBTrx()
		// Attribute each lock to the session that owns its TRANSACTION. data_locks.THREAD_ID is
		// not reliable for that: when T2 hits T1's implicit lock (e.g. on a freshly inserted row),
		// T2's thread converts it into an explicit lock owned by T1's transaction, and
		// data_locks reports it under T2's THREAD_ID.
		rows = l.Query(`
SELECT CAST(COALESCE(x.trx_mysql_thread_id, t.PROCESSLIST_ID) AS CHAR), d.LOCK_TYPE,
       CONCAT(d.OBJECT_NAME, IFNULL(CONCAT('.', d.INDEX_NAME), '')),
       d.LOCK_MODE, IF(d.LOCK_STATUS = 'GRANTED', 'true', 'false'), IFNULL(d.LOCK_DATA, '')
FROM performance_schema.data_locks d
JOIN performance_schema.threads t ON t.THREAD_ID = d.THREAD_ID
LEFT JOIN information_schema.innodb_trx x ON x.trx_id = d.ENGINE_TRANSACTION_ID
WHERE COALESCE(x.trx_mysql_thread_id, t.PROCESSLIST_ID) IN (` + in + `)
ORDER BY 1, d.LOCK_STATUS, d.LOCK_TYPE DESC, d.INDEX_NAME, d.LOCK_DATA`)
	}
	out := make([]Lock, 0, len(rows))
	for _, r := range rows {
		out = append(out, Lock{Session: names[r[0]], Type: r[1], Object: r[2], Mode: r[3], Granted: r[4] == "true", Data: r[5]})
	}
	return out
}

// ShowLocks prints the current locks and returns them.
func (l *Lab) ShowLocks() []Lock {
	l.T.Helper()
	locks := l.Locks()
	src := "pg_locks"
	if l.Kind == MySQL {
		src = "performance_schema.data_locks"
	}
	l.printf("     ┌─ %s", src)
	for _, k := range locks {
		l.printf("     │ %s", k)
	}
	l.printf("     └─")
	return locks
}

// ShowMetadataLocks prints MySQL metadata locks (MDL), which live in
// performance_schema.metadata_locks rather than data_locks.
func (l *Lab) ShowMetadataLocks() [][]string {
	l.T.Helper()
	if len(l.sessions) == 0 {
		return nil
	}
	names := map[string]string{}
	var ids []string
	for _, s := range l.sessions {
		names[fmt.Sprint(s.ID)] = s.Name
		ids = append(ids, fmt.Sprint(s.ID))
	}
	rows := l.Query(`
SELECT CAST(t.PROCESSLIST_ID AS CHAR), m.OBJECT_NAME, m.LOCK_TYPE, m.LOCK_DURATION, m.LOCK_STATUS
FROM performance_schema.metadata_locks m
JOIN performance_schema.threads t ON t.THREAD_ID = m.OWNER_THREAD_ID
WHERE t.PROCESSLIST_ID IN (` + strings.Join(ids, ",") + `) AND m.OBJECT_TYPE = 'TABLE'
ORDER BY t.PROCESSLIST_ID, m.LOCK_STATUS`)
	l.printf("     ┌─ performance_schema.metadata_locks")
	for _, r := range rows {
		r[0] = names[r[0]]
		l.printf("     │ %-3s %-12s %-20s %-16s %s", r[0], r[1], r[2], r[3], r[4])
	}
	l.printf("     └─")
	return rows
}

// FindLock finds a lock matching the filters (an empty string matches anything).
func FindLock(locks []Lock, session, mode, data string) (Lock, bool) {
	for _, k := range locks {
		if (session == "" || k.Session == session) && (mode == "" || k.Mode == mode) && (data == "" || k.Data == data) {
			return k, true
		}
	}
	return Lock{}, false
}
