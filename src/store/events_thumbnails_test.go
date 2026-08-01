package store

import (
	"bytes"
	"strings"
	"testing"

	sdk "github.com/cameraui/sdk/go"
)

func thumbEvent(id string) DetectionEvent {
	ev := newTestEvent(id, "cam-1", 1000, 0.9, "ended", "person")
	ev.Thumbnail = []byte("EVENT-THUMB-BYTES")
	ev.Segments = []sdk.EventSegment{
		{
			Thumbnail: []byte("SCENE-0"),
			Detections: []sdk.EventDetection{
				{Label: "person", Score: 0.9, Thumbnail: []byte("DET-0-0")},
				{Label: "dog", Score: 0.5, Thumbnail: []byte("DET-0-1")},
			},
			Attributes: []sdk.EventAttribute{
				{Type: "face", Label: "alice", Thumbnail: []byte("ATTR-0-0")},
			},
		},
		{
			Thumbnail: []byte("SCENE-1"),
			Detections: []sdk.EventDetection{
				{Label: "car", Score: 0.8, Thumbnail: []byte("DET-1-0")},
			},
		},
	}
	return ev
}

func assertThumbnailsIntact(t *testing.T, ev DetectionEvent) {
	t.Helper()
	if !bytes.Equal(ev.Thumbnail, []byte("EVENT-THUMB-BYTES")) {
		t.Errorf("event thumbnail = %q", ev.Thumbnail)
	}
	if len(ev.Segments) != 2 {
		t.Fatalf("segments = %d, want 2", len(ev.Segments))
	}
	if !bytes.Equal(ev.Segments[0].Thumbnail, []byte("SCENE-0")) {
		t.Errorf("segment 0 thumbnail = %q", ev.Segments[0].Thumbnail)
	}
	if !bytes.Equal(ev.Segments[1].Thumbnail, []byte("SCENE-1")) {
		t.Errorf("segment 1 thumbnail = %q", ev.Segments[1].Thumbnail)
	}
	if !bytes.Equal(ev.Segments[0].Detections[0].Thumbnail, []byte("DET-0-0")) {
		t.Errorf("det 0:0 = %q", ev.Segments[0].Detections[0].Thumbnail)
	}
	if !bytes.Equal(ev.Segments[0].Detections[1].Thumbnail, []byte("DET-0-1")) {
		t.Errorf("det 0:1 = %q", ev.Segments[0].Detections[1].Thumbnail)
	}
	if !bytes.Equal(ev.Segments[1].Detections[0].Thumbnail, []byte("DET-1-0")) {
		t.Errorf("det 1:0 = %q", ev.Segments[1].Detections[0].Thumbnail)
	}
	if !bytes.Equal(ev.Segments[0].Attributes[0].Thumbnail, []byte("ATTR-0-0")) {
		t.Errorf("attr 0:0 = %q", ev.Segments[0].Attributes[0].Thumbnail)
	}
}

// TestUpsert_StripsThumbnailsFromRaw is the whole point: raw is the column
// every list query reads, and on a real install 99.1% of it was base64
// thumbnail bytes.
func TestUpsert_StripsThumbnailsFromRaw(t *testing.T) {
	events, db := openTestEventStoreWithDB(t)
	if err := events.Upsert([]DetectionEvent{thumbEvent("ev-1")}); err != nil {
		t.Fatal(err)
	}

	db.Lock()
	defer db.Unlock()
	stmt, _, err := db.Conn().Prepare("SELECT raw FROM events WHERE id = 'ev-1'")
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	if !stmt.Step() {
		t.Fatal("no row")
	}
	raw := stmt.ColumnText(0)

	for _, marker := range []string{"EVENT-THUMB-BYTES", "SCENE-0", "DET-0-0", "ATTR-0-0"} {
		// json.Marshal base64-encodes []byte, so check for the encoded form.
		if strings.Contains(raw, b64(marker)) {
			t.Errorf("raw still contains thumbnail %q", marker)
		}
	}
	if !strings.Contains(raw, "person") {
		t.Error("raw lost non-thumbnail event data")
	}
}

// TestUpsert_DoesNotMutateCallersEvent guards a subtle aliasing bug: the
// ingest path reuses the same event for push notifications immediately after
// Upsert (events_ingest.go), so stripping in place would silently strip the
// notification thumbnail too.
func TestUpsert_DoesNotMutateCallersEvent(t *testing.T) {
	events := openTestEventStore(t)
	ev := thumbEvent("ev-1")
	if err := events.Upsert([]DetectionEvent{ev}); err != nil {
		t.Fatal(err)
	}
	assertThumbnailsIntact(t, ev)
}

// TestAttachThumbnails_RestoresEveryLevel round-trips through storage.
func TestAttachThumbnails_RestoresEveryLevel(t *testing.T) {
	events := openTestEventStore(t)
	if err := events.Upsert([]DetectionEvent{thumbEvent("ev-1")}); err != nil {
		t.Fatal(err)
	}

	got, err := events.Query(nil, GetEventsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Events) != 1 {
		t.Fatalf("events = %d", len(got.Events))
	}

	// Query must NOT pay for thumbnails — that is the performance fix.
	if len(got.Events[0].Thumbnail) != 0 {
		t.Error("Query returned an event carrying thumbnail bytes")
	}

	ev := got.Events[0]
	if err := events.AttachThumbnails(&ev); err != nil {
		t.Fatal(err)
	}
	assertThumbnailsIntact(t, ev)
}

// TestAttachThumbnails_LegacyInlineRowsStillWork covers databases written by
// an older version, where thumbnails are still inline in raw and there is no
// event_thumbnails row. Those must keep serving thumbnails rather than going
// blank — which is what lets this ship without a backfill.
func TestAttachThumbnails_LegacyInlineRowsStillWork(t *testing.T) {
	events, db := openTestEventStoreWithDB(t)
	if err := events.Upsert([]DetectionEvent{thumbEvent("ev-1")}); err != nil {
		t.Fatal(err)
	}

	// Rewrite the row the way the previous version stored it: thumbnails
	// inline in raw, no event_thumbnails entry.
	legacy := thumbEvent("ev-1")
	legacyRaw, err := marshalEventForTest(legacy)
	if err != nil {
		t.Fatal(err)
	}
	db.Lock()
	if err := db.Conn().Exec("DELETE FROM event_thumbnails WHERE event_id = 'ev-1'"); err != nil {
		db.Unlock()
		t.Fatal(err)
	}
	st, _, err := db.Conn().Prepare("UPDATE events SET raw = ? WHERE id = 'ev-1'")
	if err != nil {
		db.Unlock()
		t.Fatal(err)
	}
	if err := st.BindText(1, legacyRaw); err != nil {
		db.Unlock()
		t.Fatal(err)
	}
	if err := st.Exec(); err != nil {
		db.Unlock()
		t.Fatal(err)
	}
	st.Close()
	db.Unlock()

	got, err := events.Query(nil, GetEventsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ev := got.Events[0]
	if err := events.AttachThumbnails(&ev); err != nil {
		t.Fatal(err)
	}
	assertThumbnailsIntact(t, ev)
}

// TestDeleteOlderThan_RemovesThumbnails stops retention leaving the
// thumbnails behind forever, which would defeat the point of moving them.
func TestDeleteOlderThan_RemovesThumbnails(t *testing.T) {
	events, db := openTestEventStoreWithDB(t)
	ev := thumbEvent("ev-old")
	ev.StartTime = 1000
	ev.EndTime = 2000
	if err := events.Upsert([]DetectionEvent{ev}); err != nil {
		t.Fatal(err)
	}
	if _, err := events.DeleteOlderThan("cam-1", 5000); err != nil {
		t.Fatal(err)
	}

	db.Lock()
	defer db.Unlock()
	stmt, _, err := db.Conn().Prepare("SELECT COUNT(*) FROM event_thumbnails")
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	if !stmt.Step() {
		t.Fatal("no count row")
	}
	if n := stmt.ColumnInt(0); n != 0 {
		t.Errorf("orphaned thumbnail rows: %d", n)
	}
}
