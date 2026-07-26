package store

import (
	"path/filepath"
	"testing"

	"github.com/ncruces/go-sqlite3"
)

// TestOpen_CreatesSchema verifies that Open bootstraps a fresh database
// with every table the later store layers (segments, events, faces,
// vector search) depend on.
func TestOpen_CreatesSchema(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, tbl := range []string{
		"cameras",
		"segments",
		"events",
		"system_events",
		"faces",
		"face_images",
		"unknown_faces",
		"face_embeddings",
		"clip_embeddings",
	} {
		if !db.hasTable(tbl) {
			t.Errorf("missing table %s", tbl)
		}
	}

	if db.hasTable("not_a_real_table") {
		t.Errorf("hasTable reported a table that does not exist")
	}
}

// TestOpen_IsIdempotent verifies that re-opening an already-migrated
// database file does not error, does not attempt to re-run the schema
// (which would fail on CREATE TABLE without IF NOT EXISTS/guards), and
// leaves PRAGMA user_version at schemaVersion both times — i.e. it actually
// exercises the `current >= schemaVersion` gate in migrate, not just the
// IF NOT EXISTS DDL guards (which alone would make this test pass even if
// that gate were deleted).
func TestOpen_IsIdempotent(t *testing.T) {
	dir := t.TempDir()

	db1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	v1, err := userVersion(db1.Conn())
	if err != nil {
		t.Fatal(err)
	}
	if v1 != schemaVersion {
		t.Errorf("user_version after first Open = %d, want %d", v1, schemaVersion)
	}
	if err := db1.Close(); err != nil {
		t.Fatal(err)
	}

	db2, err := Open(dir)
	if err != nil {
		t.Fatalf("re-opening a migrated database failed: %v", err)
	}
	defer db2.Close()

	if !db2.hasTable("cameras") {
		t.Errorf("missing table cameras after re-open")
	}

	v2, err := userVersion(db2.Conn())
	if err != nil {
		t.Fatal(err)
	}
	if v2 != schemaVersion {
		t.Errorf("user_version after second Open = %d, want %d (unchanged)", v2, schemaVersion)
	}
}

// TestMigrateToV2_AddsColumnToExistingV1Database proves the Task 8
// incremental migration path (migrate's `current < 2` step, migrateToV2):
// given a database file left at PRAGMA user_version 1 by an earlier build
// (its segments table created without the referenced column, exactly as
// schema.sql looked before this task), re-opening it via Open brings it to
// schemaVersion, adds the column via ALTER TABLE, and — critically —
// defaults every pre-existing row to referenced=true so footage recorded
// before this feature existed is never swept by the new events-mode
// janitor. This directly exercises the incremental-migration code path that
// TestOpen_CreatesSchema/TestOpen_IsIdempotent (a fresh database going
// straight to schemaVersion via schemaSQL) cannot: those never take the
// `current < 2` / migrateToV2 branch at all.
//
// Since schemaVersion moved past 2, this also covers the full multi-step walk:
// one Open takes this file through migrateToV2 AND migrateToV3, which is what
// a genuinely old install upgrading across several releases at once does.
func TestMigrateToV2_AddsColumnToExistingV1Database(t *testing.T) {
	dir := t.TempDir()

	// Hand-build a v1 database: schemaSQL as it existed before this task
	// (segments table with no referenced column), user_version pinned to 1,
	// and one pre-existing segment row — standing in for footage a real
	// install already recorded under continuous mode before upgrading.
	//
	// The events table is here because a real v1 database has one: reaching
	// user_version 1 means schemaSQL ran to completion inside migrate's
	// transaction, creating every table it declares. Omitting it made this
	// fixture only look like a v1 database, which held right up until a
	// migration step touched a table other than segments — migrateToV3's
	// ALTER TABLE events then failed against it.
	conn, err := sqlite3.Open(filepath.Join(dir, dbFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Exec(`CREATE TABLE segments (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		camera_id TEXT, role TEXT, path TEXT,
		start_ms INTEGER, end_ms INTEGER,
		has_video INTEGER, has_audio INTEGER, codec TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := conn.Exec(`CREATE TABLE events (
		id TEXT PRIMARY KEY,
		camera_id TEXT, ts_ms INTEGER, end_ms INTEGER,
		types JSON, label TEXT, confidence REAL, box JSON,
		thumb_ref TEXT, has_recording INTEGER, raw JSON)`); err != nil {
		t.Fatal(err)
	}
	if err := conn.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatal(err)
	}
	if err := conn.Exec(`INSERT INTO segments (camera_id, role, path, start_ms, end_ms)
		VALUES ('cam1', 'high', '/rec/pre-existing.mp4', 0, 1000)`); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(dir)
	if err != nil {
		t.Fatalf("Open (migrate v1 -> v2): %v", err)
	}
	defer db.Close()

	v, err := userVersion(db.Conn())
	if err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Errorf("user_version after migration = %d, want %d", v, schemaVersion)
	}

	got, err := NewSegmentStore(db).InRange("cam1", "high", 0, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected the pre-existing segment to survive migration, got %d rows", len(got))
	}
	if !got[0].Referenced {
		t.Errorf("expected the pre-existing row to default to referenced=true after migration, got %+v", got[0])
	}
}

// TestMigrateToV3_AddsColumnToExistingV2Database proves the AI-description
// incremental migration path (migrate's `current < 3` step, migrateToV3):
// given a database file left at PRAGMA user_version 2 by an earlier build (its
// events table created without the description column, exactly as schema.sql
// looked before this feature), re-opening it via Open brings it to
// schemaVersion, adds the column via ALTER TABLE, and leaves every
// pre-existing event readable with no description — which is precisely the
// state they are all genuinely in, since nothing was ever generated for them.
//
// Hand-building the v2 database (rather than opening a fresh one and dropping
// the column back off) is deliberate, and mirrors
// TestMigrateToV2_AddsColumnToExistingV1Database: it reproduces the real
// upgrade scenario — a table whose CREATE TABLE never mentioned the column —
// instead of relying on ALTER TABLE ... DROP COLUMN, which this SQLite build
// need not support and which would in any case only simulate the shape, not
// the history.
func TestMigrateToV3_AddsColumnToExistingV2Database(t *testing.T) {
	dir := t.TempDir()

	// events as schema.sql declared it at version 2: no description column.
	// One pre-existing row stands in for an event a real install already
	// recorded before upgrading.
	conn, err := sqlite3.Open(filepath.Join(dir, dbFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Exec(`CREATE TABLE events (
		id TEXT PRIMARY KEY,
		camera_id TEXT,
		ts_ms INTEGER,
		end_ms INTEGER,
		types JSON,
		label TEXT,
		confidence REAL,
		box JSON,
		thumb_ref TEXT,
		has_recording INTEGER,
		raw JSON)`); err != nil {
		t.Fatal(err)
	}
	if err := conn.Exec("PRAGMA user_version = 2"); err != nil {
		t.Fatal(err)
	}
	if err := conn.Exec(`INSERT INTO events (id, camera_id, ts_ms, end_ms, raw)
		VALUES ('pre-existing', 'cam1', 1000, 2000,
			'{"id":"pre-existing","cameraId":"cam1","startTime":1000,"endTime":2000}')`); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(dir)
	if err != nil {
		t.Fatalf("Open (migrate v2 -> v3): %v", err)
	}
	defer db.Close()

	v, err := userVersion(db.Conn())
	if err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Errorf("user_version after migration = %d, want %d", v, schemaVersion)
	}

	db.Lock()
	has, herr := hasColumn(db.Conn(), "events", "description")
	db.Unlock()
	if herr != nil {
		t.Fatal(herr)
	}
	if !has {
		t.Fatal("events.description missing after migrating a v2 database")
	}

	// The pre-existing row must survive the ALTER TABLE and read back with no
	// description (a NULL column), not a decode error.
	got, err := NewEventStore(db).Query([]string{"cam1"}, GetEventsOptions{})
	if err != nil {
		t.Fatalf("Query after migration: %v", err)
	}
	if len(got.Events) != 1 || got.Events[0].ID != "pre-existing" {
		t.Fatalf("expected the pre-existing event to survive migration, got %+v", got.Events)
	}
}

// TestConn_ReturnsUnderlyingConnection verifies the Conn accessor exposes a
// usable *sqlite3.Conn for lower-level access by later tasks.
func TestConn_ReturnsUnderlyingConnection(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if db.Conn() == nil {
		t.Fatal("Conn() returned nil")
	}
	if err := db.Conn().Exec("SELECT 1"); err != nil {
		t.Fatalf("Conn() returned an unusable connection: %v", err)
	}
}
