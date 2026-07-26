package describe

import (
	"strings"
	"testing"

	sdk "github.com/cameraui/sdk/go"
)

// TestEventLabels_AcrossSegments_ReturnsDistinctNonEmptyLabels pins the input to
// the allow-list gate. Duplicates across segments are the norm — an event that
// runs for ten seconds produces the same "person" label in every segment — so
// de-duplication is what keeps Config.AllowsLabels' inner loop bounded by the
// number of distinct object classes rather than the event's duration.
func TestEventLabels_AcrossSegments_ReturnsDistinctNonEmptyLabels(t *testing.T) {
	ev := sdk.DetectionEvent{
		Segments: []sdk.EventSegment{
			{Detections: []sdk.EventDetection{{Label: "person", Score: 0.9}, {Label: "vehicle", Score: 0.5}}},
			{Detections: []sdk.EventDetection{{Label: "person", Score: 0.95}, {Label: "", Score: 0.2}}},
		},
	}

	got := EventLabels(ev)
	if len(got) != 2 {
		t.Fatalf("EventLabels = %v, want 2 distinct labels", got)
	}
	joined := strings.Join(got, ",")
	if !strings.Contains(joined, "person") || !strings.Contains(joined, "vehicle") {
		t.Errorf("EventLabels = %v, want person and vehicle", got)
	}
}

// TestEventLabels_PreservesFirstAppearanceOrder locks the ordering contract the
// doc comment promises. Nothing functional depends on it today, but a map-order
// implementation would make every test that joins the result flaky.
func TestEventLabels_PreservesFirstAppearanceOrder(t *testing.T) {
	ev := sdk.DetectionEvent{
		Segments: []sdk.EventSegment{
			{Detections: []sdk.EventDetection{{Label: "vehicle"}, {Label: "person"}}},
			{Detections: []sdk.EventDetection{{Label: "person"}, {Label: "dog"}}},
		},
	}

	want := []string{"vehicle", "person", "dog"}
	got := EventLabels(ev)
	if len(got) != len(want) {
		t.Fatalf("EventLabels = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("EventLabels[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestEventContext_FullEvent_IncludesCameraDetectionsAndAttributes asserts that
// everything the detection pipeline already established reaches the model. This
// is the substance of the prompt: without it the model re-guesses facts the
// system knows properly ("a man" instead of "Caleb"), which is both worse and
// unnecessary.
func TestEventContext_FullEvent_IncludesCameraDetectionsAndAttributes(t *testing.T) {
	ev := sdk.DetectionEvent{
		StartTime: 1_700_000_000_000,
		EndTime:   1_700_000_012_000,
		Segments: []sdk.EventSegment{{
			Detections: []sdk.EventDetection{{Label: "person", Score: 0.94}},
			Attributes: []sdk.EventAttribute{{Type: "face", Label: "Caleb", Confidence: 0.8}},
		}},
	}

	got := EventContext("Sideyard", ev)

	for _, want := range []string{"Sideyard", "person", "94", "Caleb", "12"} {
		if !strings.Contains(got, want) {
			t.Errorf("EventContext missing %q:\n%s", want, got)
		}
	}
}

// TestEventContext_IncludesZones covers the one EventSegment field that is
// populated by the zone-overlap pass rather than the detector itself. Zone names
// are user-chosen ("Driveway", "Porch"), so they carry intent the model cannot
// infer from pixels.
func TestEventContext_IncludesZones(t *testing.T) {
	ev := sdk.DetectionEvent{
		StartTime: 1_700_000_000_000,
		Segments: []sdk.EventSegment{
			{Zones: []string{"Driveway", "Porch"}},
			{Zones: []string{"Driveway", ""}},
		},
	}

	got := EventContext("Front", ev)
	for _, want := range []string{"Driveway", "Porch"} {
		if !strings.Contains(got, want) {
			t.Errorf("EventContext missing zone %q:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "Driveway"); n != 1 {
		t.Errorf("Driveway appears %d times, want 1 (zones must be de-duplicated)", n)
	}
}

// TestEventContext_NoDetections_StillNamesTheCamera guards the degenerate event
// (a motion-only trigger, or an event whose segments never arrived). Every
// section of the context is optional except the camera, so the result must never
// be an empty user message — an empty text part is what makes some
// OpenAI-compatible servers reject the request outright.
func TestEventContext_NoDetections_StillNamesTheCamera(t *testing.T) {
	got := EventContext("Sideyard", sdk.DetectionEvent{StartTime: 1_700_000_000_000})
	if got == "" {
		t.Error("EventContext returned empty string; it must always describe the camera at least")
	}
	if !strings.Contains(got, "Sideyard") {
		t.Errorf("EventContext = %q, want it to name the camera", got)
	}
}

// TestEventContext_BlankCameraName_UsesAPlaceholder covers the camera whose name
// lookup failed (a camera removed between ingestion and generation). A prompt
// reading "Camera: " with nothing after it invites the model to invent a
// location.
func TestEventContext_BlankCameraName_UsesAPlaceholder(t *testing.T) {
	got := EventContext("", sdk.DetectionEvent{StartTime: 1_700_000_000_000})
	if !strings.Contains(got, "unknown camera") {
		t.Errorf("EventContext = %q, want a placeholder camera name", got)
	}
}
