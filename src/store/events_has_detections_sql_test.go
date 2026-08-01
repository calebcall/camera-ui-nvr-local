package store

import (
	"strings"
	"testing"
)

// TestNeedsPostFilter_HasDetectionsIsSQLExpressible locks in that
// HasDetections is answered by the indexed has_detections column rather than
// by decoding every row's raw JSON in Go. It is the difference between
// reading 16 rows and reading the whole table (see the perf investigation on
// the epic): the frontend sends {"hasDetections":true,"limit":16} on every
// event-list load.
func TestNeedsPostFilter_HasDetectionsIsSQLExpressible(t *testing.T) {
	yes := true
	if needsPostFilter(GetEventsOptions{HasDetections: &yes}) {
		t.Fatal("HasDetections must not force the Go post-filter path")
	}
	no := false
	if needsPostFilter(GetEventsOptions{HasDetections: &no}) {
		t.Fatal("HasDetections=false must not force the Go post-filter path")
	}
}

// TestBuildEventsQuery_HasDetectionsKeepsLimit is the actual bug: because
// needsPostFilter returned true, buildEventsQuery dropped the SQL LIMIT and
// the store decoded every matching row to return a page of 16.
func TestBuildEventsQuery_HasDetectionsKeepsLimit(t *testing.T) {
	yes := true
	limit := int64(16)
	query, args, gotLimit := buildEventsQuery(nil, GetEventsOptions{HasDetections: &yes, Limit: &limit})

	if !strings.Contains(query, "LIMIT ?") {
		t.Fatalf("expected a SQL LIMIT, got: %s", query)
	}
	if !strings.Contains(query, "has_detections") {
		t.Fatalf("expected has_detections pushed into SQL, got: %s", query)
	}
	if gotLimit != 16 {
		t.Fatalf("expected effective limit 16, got %d", gotLimit)
	}
	// limit+1 is the last bound arg, per buildEventsQuery's pagination contract.
	if len(args) == 0 || args[len(args)-1] != 17 {
		t.Fatalf("expected trailing limit+1 arg of 17, got %v", args)
	}
}

// TestBuildEventsQuery_HasDetectionsFalseIsNotAFilter preserves the existing
// semantic that hasDetections=false means "no filter", not "only events
// without detections" — matchesFilters only ever filtered when the value was
// true. Changing that would silently hide events.
func TestBuildEventsQuery_HasDetectionsFalseIsNotAFilter(t *testing.T) {
	no := false
	query, _, _ := buildEventsQuery(nil, GetEventsOptions{HasDetections: &no})
	if strings.Contains(query, "has_detections") {
		t.Fatalf("hasDetections=false must not constrain the query, got: %s", query)
	}
}

// TestEventStore_HasDetectionsColumnMatchesPredicate keeps the stored column
// and the Go predicate from drifting: the column is what queries now trust.
func TestEventStore_HasDetectionsColumnMatchesPredicate(t *testing.T) {
	events, db := openTestEventStoreWithDB(t)

	cases := []struct {
		id    string
		types []string
	}{
		{"motion-only", []string{"motion"}},
		{"audio-only", []string{"audio"}},
		{"motion-and-audio", []string{"motion", "audio"}},
		{"person", []string{"motion", "person"}},
		{"vehicle", []string{"vehicle"}},
		{"no-types", nil},
	}
	for _, c := range cases {
		ev := newTestEvent(c.id, "cam-1", 1000, 0.9, "ended", c.types...)
		if err := events.Upsert([]DetectionEvent{ev}); err != nil {
			t.Fatal(err)
		}
	}

	db.Lock()
	defer db.Unlock()
	stmt, _, err := db.Conn().Prepare("SELECT id, has_detections FROM events")
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()

	seen := 0
	for stmt.Step() {
		id, stored := stmt.ColumnText(0), stmt.ColumnBool(1)
		var types []string
		for _, c := range cases {
			if c.id == id {
				types = c.types
			}
		}
		want := EventHasDetections(newTestEvent(id, "cam-1", 1000, 0.9, "ended", types...))
		if stored != want {
			t.Errorf("%s: has_detections=%v, EventHasDetections=%v", id, stored, want)
		}
		seen++
	}
	if seen != len(cases) {
		t.Fatalf("expected %d rows, saw %d", len(cases), seen)
	}
}
