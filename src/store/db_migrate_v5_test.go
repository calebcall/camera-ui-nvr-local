package store

import (
	"strings"
	"testing"
)

// queryPlan returns EXPLAIN QUERY PLAN's detail lines for sql.
func queryPlan(t *testing.T, db *DB, sql string) []string {
	t.Helper()
	db.Lock()
	defer db.Unlock()
	stmt, _, err := db.Conn().Prepare("EXPLAIN QUERY PLAN " + sql)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	var out []string
	for stmt.Step() {
		out = append(out, stmt.ColumnText(3))
	}
	if err := stmt.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestUnfilteredRecentEventsUsesTsIndex asserts the plan, not a duration, so a
// future schema change cannot silently reintroduce the full scan.
//
// Before idx_events_ts the only index was (camera_id, ts_ms), whose leading
// column is unconstrained here, so SQLite scanned every row and sorted it
// through a temp B-tree before LIMIT could apply — dragging the whole raw
// payload through the sorter. On a real 12,673-event / 652 MB install that was
// ~1.8s per call; with the index, 6ms.
func TestUnfilteredRecentEventsUsesTsIndex(t *testing.T) {
	_, db := openTestEventStoreWithDB(t)

	query, _, _ := buildEventsQuery(nil, GetEventsOptions{})
	plan := strings.Join(queryPlan(t, db, query), " | ")

	if !strings.Contains(plan, "idx_events_ts") {
		t.Errorf("expected the unfiltered recent-events query to use idx_events_ts, plan was: %s", plan)
	}
	if strings.Contains(strings.ToUpper(plan), "TEMP B-TREE") {
		t.Errorf("expected no temp B-tree sort, plan was: %s", plan)
	}
}

// TestMigrateToV5_AddsTsIndexToExistingDatabase covers the upgrade path: a
// database already at the previous version must gain the index on open.
func TestMigrateToV5_AddsTsIndexToExistingDatabase(t *testing.T) {
	dir := t.TempDir()

	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Drop it to simulate a database created before this version, then reopen.
	db.Lock()
	if err := db.Conn().Exec("DROP INDEX IF EXISTS idx_events_ts"); err != nil {
		db.Unlock()
		t.Fatal(err)
	}
	if err := db.Conn().Exec("PRAGMA user_version = 4"); err != nil {
		db.Unlock()
		t.Fatal(err)
	}
	db.Unlock()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("Open (migrate v4 -> v5): %v", err)
	}
	defer reopened.Close()

	v, err := userVersion(reopened.Conn())
	if err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}

	query, _, _ := buildEventsQuery(nil, GetEventsOptions{})
	plan := strings.Join(queryPlan(t, reopened, query), " | ")
	if !strings.Contains(plan, "idx_events_ts") {
		t.Errorf("index missing after migration, plan was: %s", plan)
	}
}
