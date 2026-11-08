// Package postgres contains PostgreSQL experiments: MVCC internals, locks, deadlocks and
// isolation-level behavior. Run with -v to see the timeline of each experiment:
//
//	go test ./postgres -v -run TestMVCC
package postgres

import (
	"testing"

	"github.com/jyny/db-iso-lv-go/internal/lab"
)

const dbname = "lab_pg"

func newLab(t *testing.T) *lab.Lab {
	L := lab.New(t, lab.Postgres, dbname)
	L.Exec("CREATE EXTENSION IF NOT EXISTS pageinspect", "CREATE EXTENSION IF NOT EXISTS pgrowlocks")
	return L
}

// pageItems shows the raw tuples on page 0 of a table, including dead and aborted ones,
// which a normal SELECT never shows.
//
//	t_xmin       xid that created the tuple version
//	t_xmax       xid that deleted/updated it (or locked it; 0 = live)
//	t_ctid       points to itself, or to the newer version after an UPDATE
//	xmin_c/xmin_a  hint bits: creator known committed / aborted
//	xmax_c/xmax_a  hint bits: deleter known committed / aborted (invalid)
//	hot          HEAP_HOT_UPDATED: the newer version is on the same page, no new index entry
func pageItems(L *lab.Lab, table string) [][]string {
	L.T.Helper()
	return L.ShowAs("heap_page_items(get_raw_page('"+table+"', 0))", `
SELECT lp, CASE lp_flags WHEN 0 THEN 'unused' WHEN 1 THEN 'normal' WHEN 2 THEN 'redirect' ELSE 'dead' END AS lp_flags,
       t_xmin, t_xmax, t_ctid,
       (t_infomask & 256) > 0  AS xmin_c, (t_infomask & 512) > 0  AS xmin_a,
       (t_infomask & 1024) > 0 AS xmax_c, (t_infomask & 2048) > 0 AS xmax_a,
       (t_infomask2 & 16384) > 0 AS hot
FROM heap_page_items(get_raw_page('`+table+`', 0))`)
}
