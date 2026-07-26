package main

import (
	"errors"
	"sync"
	"testing"

	sdk "github.com/cameraui/sdk/go"
)

// fakeNotifier is an eventNotifier test double recording every Publish call
// it receives, so notify (events_ingest.go) can be tested without a real
// *sdk.NotificationManager/host connection. Guarded by mu for the same
// no-single-goroutine-guarantee reason every other ingester dependency's
// fake in this package is (handle has no documented single-goroutine
// contract).
type fakeNotifier struct {
	mu           sync.Mutex
	published    []*sdk.Notification
	publishErr   error
	publishCalls int
}

func (f *fakeNotifier) Publish(n *sdk.Notification) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.publishCalls++
	if f.publishErr != nil {
		return f.publishErr
	}
	f.published = append(f.published, n)
	return nil
}

// fakeCameraNames is a cameraNamer test double: names maps a camera ID to
// its display name; a camera ID absent from the map reports ok=false, the
// same "no entry for this camera" contract RecorderManager.CameraName has.
type fakeCameraNames struct {
	names map[string]string
}

func (f *fakeCameraNames) CameraName(cameraID string) (string, bool) {
	name, ok := f.names[cameraID]
	return name, ok
}

// TestDetectionEventIngester_Notify_ObjectEventTerminalMessagePublishesOnce
// is the FIX C core proof: a person-detection event's terminal ('end')
// message publishes exactly one notification, titled with the resolved
// camera name and the primary (object) label, and carrying cameraId/eventId
// in Data.
func TestDetectionEventIngester_Notify_ObjectEventTerminalMessagePublishesOnce(t *testing.T) {
	notifier := &fakeNotifier{}
	names := &fakeCameraNames{names: map[string]string{"cam1": "Sideyard"}}
	ingester := newDetectionEventIngester(&fakeEventStore{}, nil, nil, nil, notifier, names, nil, nil, nil)

	ingester.handle(sdk.DetectionEventEnd, sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", State: sdk.DetectionEventStateEnded,
		StartTime: 1000, EndTime: 5000,
		Types: []string{"motion", "person"},
		Segments: []sdk.EventSegment{
			{Detections: []sdk.EventDetection{{Label: "person", Score: 0.8}}},
		},
	})

	if notifier.publishCalls != 1 {
		t.Fatalf("expected exactly 1 Publish call, got %d", notifier.publishCalls)
	}
	n := notifier.published[0]
	if n.Title != "Sideyard — Person" {
		t.Errorf("expected title %q, got %q", "Sideyard — Person", n.Title)
	}
	if n.Severity != sdk.SeverityInfo {
		t.Errorf("expected severity info, got %q", n.Severity)
	}
	if n.Data["cameraId"] != "cam1" || n.Data["eventId"] != "evt-1" {
		t.Errorf("expected Data to carry cameraId=cam1 eventId=evt-1, got %+v", n.Data)
	}
	// DeepLink uses the camera NAME (the /cameras/:cameraname route resolves
	// by name, not id), URL-escaped — "cam1" resolves to "Sideyard" here.
	if want := "/cameras/Sideyard?startTs=1000"; n.DeepLink != want {
		t.Errorf("expected DeepLink %q, got %q", want, n.DeepLink)
	}
}

// TestDetectionEventIngester_Notify_MotionOnlyEventNeverPublishes proves a
// motion-only event's terminal message publishes NO notification at all —
// the anti-spam gate FIX C's brief requires (only object-detection events
// notify).
func TestDetectionEventIngester_Notify_MotionOnlyEventNeverPublishes(t *testing.T) {
	notifier := &fakeNotifier{}
	ingester := newDetectionEventIngester(&fakeEventStore{}, nil, nil, nil, notifier, nil, nil, nil, nil)

	ingester.handle(sdk.DetectionEventEnd, sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", State: sdk.DetectionEventStateEnded,
		StartTime: 1000, EndTime: 5000, Types: []string{"motion"},
	})

	if notifier.publishCalls != 0 {
		t.Fatalf("expected motion-only event to never publish, got %d calls", notifier.publishCalls)
	}
}

// TestDetectionEventIngester_Notify_AudioOnlyEventNeverPublishes proves an
// audio-only event's terminal message also publishes nothing — the same
// non-object-detection gate as motion-only.
func TestDetectionEventIngester_Notify_AudioOnlyEventNeverPublishes(t *testing.T) {
	notifier := &fakeNotifier{}
	ingester := newDetectionEventIngester(&fakeEventStore{}, nil, nil, nil, notifier, nil, nil, nil, nil)

	ingester.handle(sdk.DetectionEventEnd, sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", State: sdk.DetectionEventStateEnded,
		StartTime: 1000, EndTime: 5000, Types: []string{"audio"},
	})

	if notifier.publishCalls != 0 {
		t.Fatalf("expected audio-only event to never publish, got %d calls", notifier.publishCalls)
	}
}

// TestDetectionEventIngester_Notify_NonTerminalMessagesNeverPublish proves
// an object-detection event's start/update messages (not yet terminal) do
// NOT publish — only the terminal message does.
func TestDetectionEventIngester_Notify_NonTerminalMessagesNeverPublish(t *testing.T) {
	notifier := &fakeNotifier{}
	ingester := newDetectionEventIngester(&fakeEventStore{}, nil, nil, nil, notifier, nil, nil, nil, nil)

	ingester.handle(sdk.DetectionEventStart, sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", State: sdk.DetectionEventStateActive,
		StartTime: 1000, Types: []string{"person"},
	})
	ingester.handle(sdk.DetectionEventUpdate, sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", State: sdk.DetectionEventStateActive,
		StartTime: 1000, Types: []string{"person"},
	})

	if notifier.publishCalls != 0 {
		t.Fatalf("expected non-terminal messages to never publish, got %d calls", notifier.publishCalls)
	}
}

// TestDetectionEventIngester_Notify_DuplicateTerminalMessageDoesNotDoublePublish
// proves a second terminal message arriving for an event id already
// notified (e.g. a duplicate/retried 'end' delivery) does not publish a
// second time.
func TestDetectionEventIngester_Notify_DuplicateTerminalMessageDoesNotDoublePublish(t *testing.T) {
	notifier := &fakeNotifier{}
	ingester := newDetectionEventIngester(&fakeEventStore{}, nil, nil, nil, notifier, nil, nil, nil, nil)

	end := sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", State: sdk.DetectionEventStateEnded,
		StartTime: 1000, EndTime: 5000, Types: []string{"person"},
	}
	ingester.handle(sdk.DetectionEventEnd, end)
	ingester.handle(sdk.DetectionEventEnd, end)

	if notifier.publishCalls != 1 {
		t.Fatalf("expected exactly 1 Publish call across two terminal messages for the same event id, got %d", notifier.publishCalls)
	}
}

// TestDetectionEventIngester_Notify_NilNotifierNeverPublishesOrPanics proves
// a nil notifier (the default for tests, or a host build that never wired
// api.NotificationManager) is a safe no-op rather than a nil-pointer panic.
func TestDetectionEventIngester_Notify_NilNotifierNeverPublishesOrPanics(t *testing.T) {
	ingester := newDetectionEventIngester(&fakeEventStore{}, nil, nil, nil, nil, nil, nil, nil, nil)

	ingester.handle(sdk.DetectionEventEnd, sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", State: sdk.DetectionEventStateEnded,
		StartTime: 1000, EndTime: 5000, Types: []string{"person"},
	})
	// No assertion beyond "did not panic" — there is no notifier to have
	// been called.
}

// TestDetectionEventIngester_Notify_UnresolvedCameraNameFallsBackToID proves
// a nil cameraNamer, or one with no entry for the event's camera, falls back
// to the bare camera ID in the notification title rather than failing to
// notify.
func TestDetectionEventIngester_Notify_UnresolvedCameraNameFallsBackToID(t *testing.T) {
	notifier := &fakeNotifier{}
	ingester := newDetectionEventIngester(&fakeEventStore{}, nil, nil, nil, notifier, nil, nil, nil, nil)

	ingester.handle(sdk.DetectionEventEnd, sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", State: sdk.DetectionEventStateEnded,
		StartTime: 1000, EndTime: 5000, Types: []string{"person"},
	})

	if notifier.publishCalls != 1 {
		t.Fatalf("expected exactly 1 Publish call, got %d", notifier.publishCalls)
	}
	if notifier.published[0].Title != "cam1 — Person" {
		t.Errorf("expected title %q, got %q", "cam1 — Person", notifier.published[0].Title)
	}
}

// TestDetectionEventIngester_Notify_PublishErrorIsSwallowed proves a failing
// Publish call is logged (exercised here with a nil logger, to prove it
// doesn't panic when there's nowhere to log to either) rather than
// propagated — OnDetectionEvent's callback has no error return for it to go
// through.
func TestDetectionEventIngester_Notify_PublishErrorIsSwallowed(t *testing.T) {
	notifier := &fakeNotifier{publishErr: errors.New("boom")}
	ingester := newDetectionEventIngester(&fakeEventStore{}, nil, nil, nil, notifier, nil, nil, nil, nil)

	ingester.handle(sdk.DetectionEventEnd, sdk.DetectionEvent{
		ID: "evt-1", CameraID: "cam1", State: sdk.DetectionEventStateEnded,
		StartTime: 1000, EndTime: 5000, Types: []string{"person"},
	})
	// No panic, and the earlier "swallowed, not surfaced" contract is
	// implicitly proven by this test simply completing.
}

// TestDetectionSummary verifies the notification Body summarizes detected
// objects by best confidence (highest first) plus recognized attributes.
func TestDetectionSummary(t *testing.T) {
	event := sdk.DetectionEvent{
		Segments: []sdk.EventSegment{
			{
				Detections: []sdk.EventDetection{
					{Label: "person", Score: 0.94},
					{Label: "vehicle", Score: 0.81},
				},
				Attributes: []sdk.EventAttribute{{Type: "face", Label: "Caleb"}},
			},
		},
	}
	if got, want := detectionSummary(event), "Person 94%, Vehicle 81% · Caleb"; got != want {
		t.Fatalf("detectionSummary = %q, want %q", got, want)
	}
	if got := detectionSummary(sdk.DetectionEvent{}); got != "" {
		t.Fatalf("empty event summary = %q, want empty", got)
	}
}

// TestNotify_FilterSuppressesDisabledDetectionType wires the REAL
// notifyLabelFilter into the ingester, so this covers the seam between the
// filter's own unit tests and notify's gate ordering rather than re-testing the
// filter in isolation.
func TestNotify_FilterSuppressesDisabledDetectionType(t *testing.T) {
	for _, tc := range []struct {
		name        string
		settings    notifyStore
		labels      []string
		wantPublish int
	}{
		{"nothing configured notifies", notifyStore{}, []string{"vehicle"}, 1},
		{"disabled type is suppressed", notifyStore{notifyVehicleKey: false}, []string{"vehicle"}, 0},
		{"another enabled label still notifies", notifyStore{notifyVehicleKey: false}, []string{"person", "vehicle"}, 1},
		{"a different type is unaffected", notifyStore{notifyVehicleKey: false}, []string{"person"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			notifier := &fakeNotifier{}
			i := newDetectionEventIngester(
				&fakeEventStore{}, nil, nil, nil, notifier, nil, nil,
				newNotifyLabelFilter(tc.settings, nil), nil,
			)

			ended := labelEvent(tc.labels...)
			i.handle(sdk.DetectionEventEnd, ended)

			if notifier.publishCalls != tc.wantPublish {
				t.Errorf("publish calls = %d, want %d", notifier.publishCalls, tc.wantPublish)
			}
		})
	}
}

// TestNotify_FilterRunsBeforeDedup is the ordering test, and the reason it
// matters is not obvious from reading notify top to bottom.
//
// markFirst is a one-shot latch per event ID. If filtering happened after it, a
// suppressed event would still consume its own notification slot — so a user who
// re-enabled that detection type while the event was still arriving would get
// nothing, and nothing in the UI would explain why. Filtering first leaves the
// latch untouched for a suppressed event.
func TestNotify_FilterRunsBeforeDedup(t *testing.T) {
	notifier := &fakeNotifier{}
	settings := notifyStore{notifyVehicleKey: false}
	i := newDetectionEventIngester(
		&fakeEventStore{}, nil, nil, nil, notifier, nil, nil,
		newNotifyLabelFilter(settings, nil), nil,
	)

	ended := labelEvent("vehicle")

	// Suppressed: must not publish, and must not burn the dedup slot.
	i.handle(sdk.DetectionEventEnd, ended)
	if notifier.publishCalls != 0 {
		t.Fatalf("publish calls = %d, want 0 while vehicle is off", notifier.publishCalls)
	}

	// The user turns vehicle notifications back on; the same event arriving
	// again must now be publishable, which is only true if the earlier
	// suppressed pass left markFirst alone.
	settings[notifyVehicleKey] = true
	i.handle(sdk.DetectionEventEnd, ended)

	if notifier.publishCalls != 1 {
		t.Errorf("publish calls = %d, want 1 after re-enabling; the suppressed pass consumed the dedup slot", notifier.publishCalls)
	}
}

// TestNotify_NilFilterNotifiesEverything keeps the optional-dependency
// convention verified at the ingester level too.
func TestNotify_NilFilterNotifiesEverything(t *testing.T) {
	notifier := &fakeNotifier{}
	i := newDetectionEventIngester(&fakeEventStore{}, nil, nil, nil, notifier, nil, nil, nil, nil)

	i.handle(sdk.DetectionEventEnd, labelEvent("vehicle"))

	if notifier.publishCalls != 1 {
		t.Errorf("publish calls = %d, want 1 with no filter wired", notifier.publishCalls)
	}
}
