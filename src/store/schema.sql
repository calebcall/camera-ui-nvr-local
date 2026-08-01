-- Schema for the camera-ui-nvr-local plugin's embedded SQLite database.
--
-- Applied once by store.migrate and tracked via PRAGMA user_version (see
-- db.go). Every statement is written defensively with IF NOT EXISTS so that
-- re-running this file against an already-migrated database is a no-op,
-- even if the user_version bookkeeping were ever bypassed.
--
-- Vector search (face_embeddings, clip_embeddings) is NOT backed by the
-- sqlite-vec virtual table extension here — see vector.go for why (the
-- currently maintained ncruces/go-sqlite3 release is API-incompatible with
-- the last published sqlite-vec WASM bindings). Both tables are plain
-- tables storing the embedding as a BLOB of little-endian float32s; nearest
-- neighbor search is done in Go by bruteForceVectorBackend.

CREATE TABLE IF NOT EXISTS cameras (
  id TEXT PRIMARY KEY,
  config JSON,
  updated_ms INTEGER
);

CREATE TABLE IF NOT EXISTS segments (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  camera_id TEXT,
  role TEXT,
  path TEXT,
  start_ms INTEGER,
  end_ms INTEGER,
  has_video INTEGER,
  has_audio INTEGER,
  codec TEXT,
  -- referenced (Task 8): whether this segment is permanently retained.
  -- Continuous-mode segments are always inserted referenced=1. Events-mode
  -- segments are inserted referenced=0 ("spool") and only flipped to 1 by
  -- Recorder.MarkEvent when a detection event's [start-preRoll,
  -- end+postRoll] window covers them; unreferenced spool segments older
  -- than the camera's preRoll are removed by the events-mode janitor (see
  -- recorder/event_mode.go). Added retroactively via an ALTER TABLE
  -- migration for pre-existing v1 databases (see db.go's migrateToV2) —
  -- DEFAULT 1 there so already-recorded footage is never swept.
  referenced INTEGER NOT NULL DEFAULT 1
);

CREATE INDEX IF NOT EXISTS idx_segments_camera_role_start
  ON segments (camera_id, role, start_ms);

CREATE TABLE IF NOT EXISTS events (
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
  raw JSON,
  -- description: the AI-generated sdk.EventDescription for this event as
  -- JSON, or NULL when none was generated (which is every event, until the
  -- feature is switched on). Deliberately its own column rather than
  -- embedded in raw: EventStore.Upsert rewrites raw wholesale on every
  -- lifecycle message, so a duplicate or late terminal message arriving
  -- after generation completed would silently erase a description stored
  -- there. Merged onto Segments[0].Description when Query decodes a row —
  -- see attachDescription (events.go). Added retroactively via an ALTER
  -- TABLE migration for pre-existing v2 databases (see db.go's
  -- migrateToV3); nullable with no default, since NULL is exactly the state
  -- every pre-existing event is genuinely in.
  description TEXT,
  -- has_detections: EventHasDetections(ev) precomputed at write time, so the
  -- getEvents `hasDetections` filter can be answered in SQL instead of by
  -- decoding every row's raw JSON in Go. That filter is on the hot path — the
  -- frontend sends {"hasDetections":true,"limit":16} on every event-list load
  -- — and routing it through the Go post-filter forced buildEventsQuery to
  -- drop the SQL LIMIT and read the whole table to return a page of 16 (652 MB
  -- and ~9s on a real 12k-event install). Kept in lockstep with the Go
  -- predicate by upsertOneEvent, its only writer. Added retroactively via an
  -- ALTER TABLE migration for pre-existing v3 databases (see db.go's
  -- migrateToV4), which backfills from the existing `types` column.
  has_detections INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_events_camera_ts
  ON events (camera_id, ts_ms);

-- Serves the hot event-list query: a set of camera ids, filtered to events
-- with detections, newest first. Leading camera_id matches the
-- `camera_id IN (...)` clause, has_detections then narrows, and ts_ms orders
-- within each group.
CREATE INDEX IF NOT EXISTS idx_events_camera_hasdet_ts
  ON events (camera_id, has_detections, ts_ms);

-- Serves the unfiltered "recent events across every camera" query
-- (getEvents with no camera ids), which orders by ts_ms alone. Neither index
-- above can: both lead with camera_id, which that query leaves unconstrained,
-- so SQLite fell back to scanning every row and sorting it through a temp
-- B-tree before LIMIT could apply — carrying the full raw payload through the
-- sorter. On a 12,673-event / 652 MB install that was ~1.8s per call versus
-- 6ms with this index, which itself costs 31ms to build and no measurable
-- file size. DESC matches the query's ORDER BY direction.
CREATE INDEX IF NOT EXISTS idx_events_ts
  ON events (ts_ms DESC);

-- event_thumbnails holds the JPEG bytes an sdk.DetectionEvent carries inline,
-- lifted out of events.raw so the event-list queries never pay for them.
--
-- The SDK inlines thumbnails at four levels (event, segment, detection,
-- attribute) and encoding/json renders []byte as base64, so storing the event
-- verbatim put all of it in the one column every list query reads. On a real
-- install that was 99.1% of a 652 MB events table — the actual event data was
-- ~6 MB. Splitting them out leaves raw small and keeps the bytes available to
-- GetEventThumbnails, which is the only reader that actually wants them.
--
-- payload is the thumbnails positionally mirroring the event's structure (see
-- eventThumbnails in events.go), not the wire's EventThumbnails shape: it has
-- to survive a round trip back onto a decoded event, and the wire shape is
-- lossy for that (it keys detections by label, which is not unique within a
-- segment).
--
-- ON DELETE CASCADE is what keeps retention honest: DeleteOlderThan only
-- deletes from events, and PRAGMA foreign_keys=ON (set in Open) then drops
-- the matching thumbnails rather than orphaning them forever.
CREATE TABLE IF NOT EXISTS event_thumbnails (
  event_id TEXT PRIMARY KEY REFERENCES events(id) ON DELETE CASCADE,
  payload TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS system_events (
  id TEXT PRIMARY KEY,
  camera_id TEXT,
  ts_ms INTEGER,
  type TEXT,
  severity TEXT,
  message TEXT,
  duration_ms INTEGER
);

CREATE TABLE IF NOT EXISTS faces (
  name TEXT PRIMARY KEY,
  created_ms INTEGER,
  updated_ms INTEGER,
  thumbnail BLOB
);

CREATE TABLE IF NOT EXISTS face_images (
  id TEXT PRIMARY KEY,
  name TEXT,
  jpeg BLOB,
  confidence REAL
);

CREATE TABLE IF NOT EXISTS unknown_faces (
  id TEXT PRIMARY KEY,
  camera_id TEXT,
  event_id TEXT,
  ts_ms INTEGER,
  jpeg BLOB,
  cluster_id TEXT
);

-- Vector tables (brute-force fallback backend; see vector.go).
-- Keyed to face_images.id and events.id respectively.

CREATE TABLE IF NOT EXISTS face_embeddings (
  id TEXT PRIMARY KEY,
  embedding BLOB NOT NULL,
  dim INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS clip_embeddings (
  id TEXT PRIMARY KEY,
  embedding BLOB NOT NULL,
  dim INTEGER NOT NULL
);
