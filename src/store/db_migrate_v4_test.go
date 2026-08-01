package store

import (
	"path/filepath"
	"testing"

	"github.com/ncruces/go-sqlite3"
)

// TestMigrateToV4_BackfillsHasDetectionsOnExistingDatabase is the regression
// guard for the failure mode that makes this migration different from
// migrateToV3's nullable column: getEvents' `hasDetections` filter now trusts
// has_detections, so leaving pre-existing rows at the DEFAULT 0 would make
// every event already in a user's database disappear from the event list the
// moment they upgrade.
//
// The fixture is a v3 database (description present, has_detections absent)
// holding one event of each shape the predicate distinguishes.
func TestMigrateToV4_BackfillsHasDetectionsOnExistingDatabase(t *testing.T) {
	dir := t.TempDir()

	conn, err := sqlite3.Open(filepath.Join(dir, dbFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Exec(`CREATE TABLE segments (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		camera_id TEXT, role TEXT, path TEXT,
		start_ms INTEGER, end_ms INTEGER,
		has_video INTEGER, has_audio INTEGER, codec TEXT,
		referenced INTEGER NOT NULL DEFAULT 1)`); err != nil {
		t.Fatal(err)
	}
	if err := conn.Exec(`CREATE TABLE events (
		id TEXT PRIMARY KEY,
		camera_id TEXT, ts_ms INTEGER, end_ms INTEGER,
		types JSON, label TEXT, confidence REAL, box JSON,
		thumb_ref TEXT, has_recording INTEGER, raw JSON,
		description TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := conn.Exec("PRAGMA user_version = 3"); err != nil {
		t.Fatal(err)
	}

	fixtures := []struct {
		id    string
		types string
		want  bool
	}{
		{"person", `["motion","person"]`, true},
		{"vehicle", `["vehicle"]`, true},
		{"motion-only", `["motion"]`, false},
		{"audio-only", `["audio"]`, false},
		{"motion-and-audio", `["motion","audio"]`, false},
		{"empty-types", `[]`, false},
		{"null-types", `null`, false},
	}
	for i, f := range fixtures {
		if err := conn.Exec(`INSERT INTO events (id, camera_id, ts_ms, types, raw)
			VALUES ('` + f.id + `', 'cam1', ` + itoa(1000+i) + `, '` + f.types + `', '{}')`); err != nil {
			t.Fatal(err)
		}
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(dir)
	if err != nil {
		t.Fatalf("Open (migrate v3 -> v4): %v", err)
	}
	defer db.Close()

	v, err := userVersion(db.Conn())
	if err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Errorf("user_version after migration = %d, want %d", v, schemaVersion)
	}

	stmt, _, err := db.Conn().Prepare("SELECT id, has_detections FROM events")
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	got := map[string]bool{}
	for stmt.Step() {
		got[stmt.ColumnText(0)] = stmt.ColumnBool(1)
	}
	if err := stmt.Err(); err != nil {
		t.Fatal(err)
	}
	for _, f := range fixtures {
		if got[f.id] != f.want {
			t.Errorf("%s (types=%s): has_detections=%v, want %v", f.id, f.types, got[f.id], f.want)
		}
	}
}

// TestMigrateToV4_IsIdempotent re-opens an already-migrated database, which
// must be a no-op rather than an error (the ALTER TABLE would fail on a second
// run if it were not guarded).
func TestMigrateToV4_IsIdempotent(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		db, err := Open(dir)
		if err != nil {
			t.Fatalf("Open #%d: %v", i+1, err)
		}
		v, err := userVersion(db.Conn())
		if err != nil {
			t.Fatal(err)
		}
		if v != schemaVersion {
			t.Errorf("Open #%d: user_version = %d, want %d", i+1, v, schemaVersion)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
