package main

import (
	"testing"

	sdk "github.com/cameraui/sdk/go"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/store"
)

// notifyStore is a notifySettingsStore over a plain map, honoring the caller's
// fallback for absent keys exactly as sdk.DeviceStorage.GetValue does. Absent
// therefore means "on", which is the whole default-to-true contract.
type notifyStore map[string]any

func (n notifyStore) GetValue(key string, fallback ...any) any {
	if v, ok := n[key]; ok {
		return v
	}
	if len(fallback) > 0 {
		return fallback[0]
	}
	return nil
}

// labelEvent builds an event carrying labels in BOTH Types and a segment
// detection, which is the shape a merged event has by the time notify sees it.
// Tests that care about only one of the two sources construct their own.
func labelEvent(labels ...string) store.DetectionEvent {
	dets := make([]sdk.EventDetection, 0, len(labels))
	for _, l := range labels {
		dets = append(dets, sdk.EventDetection{Label: l, Score: 0.9})
	}
	return store.DetectionEvent{
		ID:        "ev-1",
		CameraID:  "cam-1",
		State:     sdk.DetectionEventStateEnded,
		StartTime: 1000,
		EndTime:   2000,
		Types:     labels,
		Segments:  []sdk.EventSegment{{Detections: dets}},
	}
}

func TestNotifyLabelFilter_NothingStored_AllowsEveryLabel(t *testing.T) {
	f := newNotifyLabelFilter(notifyStore{})

	for _, label := range []string{"person", "vehicle", "animal", "package", "bird"} {
		t.Run(label, func(t *testing.T) {
			if !f.NotifyAllowed(labelEvent(label)) {
				t.Errorf("%q was suppressed with nothing configured; every toggle defaults to on", label)
			}
		})
	}
}

func TestNotifyLabelFilter_DisabledLabel_IsSuppressed(t *testing.T) {
	for _, tc := range []struct {
		label string
		key   string
	}{
		{"person", notifyPersonKey},
		{"vehicle", notifyVehicleKey},
		{"animal", notifyAnimalKey},
		{"package", notifyPackageKey},
	} {
		t.Run(tc.label, func(t *testing.T) {
			f := newNotifyLabelFilter(notifyStore{tc.key: false})

			if f.NotifyAllowed(labelEvent(tc.label)) {
				t.Errorf("%q notified with %s=false", tc.label, tc.key)
			}

			// Disabling one type must not disable the others: every OTHER
			// label still notifies on its own.
			for otherLabel, otherKey := range notifyObjectLabelKeys {
				if otherKey == tc.key {
					continue
				}
				if !f.NotifyAllowed(labelEvent(otherLabel)) {
					t.Errorf("%q was suppressed by %s=false", otherLabel, tc.key)
				}
			}
		})
	}
}

// TestNotifyLabelFilter_AnyEnabledLabel_Allows is the rule that keeps this from
// being infuriating in practice. A person arriving in a car produces one event
// carrying both labels; someone who turned Vehicle off did so to stop being
// pinged about passing traffic, not to stop being told a person showed up.
func TestNotifyLabelFilter_AnyEnabledLabel_Allows(t *testing.T) {
	f := newNotifyLabelFilter(notifyStore{notifyVehicleKey: false})

	if !f.NotifyAllowed(labelEvent("person", "vehicle")) {
		t.Error("a person+vehicle event was suppressed because vehicle is off; any enabled label should allow it")
	}
	if f.NotifyAllowed(labelEvent("vehicle")) {
		t.Error("a vehicle-only event notified with vehicle off")
	}
}

func TestNotifyLabelFilter_EveryLabelDisabled_SuppressesEverything(t *testing.T) {
	f := newNotifyLabelFilter(notifyStore{
		notifyPersonKey:  false,
		notifyVehicleKey: false,
		notifyAnimalKey:  false,
		notifyPackageKey: false,
		notifyOtherKey:   false,
	})

	for _, labels := range [][]string{
		{"person"}, {"vehicle"}, {"animal"}, {"package"}, {"bird"},
		{"person", "vehicle", "animal", "package", "bird"},
	} {
		if f.NotifyAllowed(labelEvent(labels...)) {
			t.Errorf("%v notified with every toggle off", labels)
		}
	}
}

// TestNotifyLabelFilter_ClassifierLabel_IsGovernedByOther covers the catch-all.
// A classifier plugin can emit anything, and without notifyOther those labels
// would be the one category a user could not filter.
func TestNotifyLabelFilter_ClassifierLabel_IsGovernedByOther(t *testing.T) {
	for _, label := range []string{"bird", "raccoon", "rain", "delivery_van"} {
		t.Run(label, func(t *testing.T) {
			if newNotifyLabelFilter(notifyStore{notifyOtherKey: false}).NotifyAllowed(labelEvent(label)) {
				t.Errorf("%q notified with %s=false", label, notifyOtherKey)
			}
			if !newNotifyLabelFilter(notifyStore{notifyOtherKey: true}).NotifyAllowed(labelEvent(label)) {
				t.Errorf("%q was suppressed with %s=true", label, notifyOtherKey)
			}
		})
	}
}

// TestNotifyLabelFilter_OtherToggle_DoesNotGovernKnownLabels keeps the catch-all
// from swallowing the standard types. If it did, turning off "Other detections"
// would silence people too — which the label on the switch does not remotely
// suggest.
func TestNotifyLabelFilter_OtherToggle_DoesNotGovernKnownLabels(t *testing.T) {
	f := newNotifyLabelFilter(notifyStore{notifyOtherKey: false})

	for _, label := range []string{"person", "vehicle", "animal", "package"} {
		if !f.NotifyAllowed(labelEvent(label)) {
			t.Errorf("%q was suppressed by %s, which must only govern non-standard labels", label, notifyOtherKey)
		}
	}
}

// TestNotifyLabelFilter_NonSubjectTypes_AreIgnored is the subtle one. An event's
// Types carries more than object labels: motion and audio trigger types, and
// attributes like face and license_plate. Those must not be treated as "other"
// (which would let them keep an event alive after its real label was turned
// off) and must not be treated as classifiable on their own.
func TestNotifyLabelFilter_NonSubjectTypes_AreIgnored(t *testing.T) {
	// Person is off; the event also carries motion and a recognized face. It
	// must stay suppressed — a face is an attribute OF the person, not an
	// independent reason to notify.
	f := newNotifyLabelFilter(notifyStore{notifyPersonKey: false})

	ev := labelEvent("person")
	ev.Types = []string{"person", "motion", "face"}

	if f.NotifyAllowed(ev) {
		t.Error("event notified despite person being off; motion/face must not act as independent subjects")
	}

	// With every object toggle off, an event carrying ONLY non-subject types has
	// nothing classifiable, so it falls through to the allow-by-default branch
	// rather than being silently dropped.
	allOff := newNotifyLabelFilter(notifyStore{
		notifyPersonKey:  false,
		notifyVehicleKey: false,
		notifyAnimalKey:  false,
		notifyPackageKey: false,
		notifyOtherKey:   false,
	})
	nonSubject := store.DetectionEvent{ID: "ev-2", Types: []string{"motion", "audio", "doorbell", "face"}}
	if !allOff.NotifyAllowed(nonSubject) {
		t.Error("an event with no classifiable label was suppressed; unanticipated shapes must fail open")
	}
}

// TestNotifyLabelFilter_NoLabelsAtAll_Allows pins the fail-open default. An
// extra notification for a shape we did not anticipate is recoverable; silent,
// undiagnosable suppression is not.
func TestNotifyLabelFilter_NoLabelsAtAll_Allows(t *testing.T) {
	f := newNotifyLabelFilter(notifyStore{notifyPersonKey: false, notifyOtherKey: false})

	if !f.NotifyAllowed(store.DetectionEvent{ID: "ev-1"}) {
		t.Error("an event with no types and no segments was suppressed; it must fail open")
	}
}

// TestNotifyLabelFilter_ReadsEitherLabelSource matters because a terminal 'end'
// message is routinely sparse — that is why the merge accumulator exists — so
// either Types or the segment detections can be empty for an event that really
// does have detections.
func TestNotifyLabelFilter_ReadsEitherLabelSource(t *testing.T) {
	f := newNotifyLabelFilter(notifyStore{notifyVehicleKey: false})

	typesOnly := store.DetectionEvent{ID: "ev-1", Types: []string{"vehicle"}}
	if f.NotifyAllowed(typesOnly) {
		t.Error("a vehicle label in Types alone was not filtered")
	}

	segmentsOnly := store.DetectionEvent{
		ID:       "ev-2",
		Segments: []sdk.EventSegment{{Detections: []sdk.EventDetection{{Label: "vehicle", Score: 0.9}}}},
	}
	if f.NotifyAllowed(segmentsOnly) {
		t.Error("a vehicle label in segment detections alone was not filtered")
	}
}

func TestNotifyLabelFilter_LabelsAreNormalized(t *testing.T) {
	f := newNotifyLabelFilter(notifyStore{notifyPersonKey: false})

	for _, raw := range []string{"Person", "PERSON", "  person  ", "PeRsOn"} {
		t.Run(raw, func(t *testing.T) {
			if f.NotifyAllowed(labelEvent(raw)) {
				t.Errorf("%q was not matched against the person toggle", raw)
			}
		})
	}
}

// TestNotifyLabelFilter_NonBooleanStoredValue_FallsBackToAllowing covers a value
// written as something other than a bool (a hand-edited config, or a future
// storage encoding). Allowing is the safe direction, consistent with every other
// fail-open decision here.
func TestNotifyLabelFilter_NonBooleanStoredValue_FallsBackToAllowing(t *testing.T) {
	for _, stored := range []any{"false", 0, nil, float64(0)} {
		if !newNotifyLabelFilter(notifyStore{notifyPersonKey: stored}).NotifyAllowed(labelEvent("person")) {
			t.Errorf("stored value %#v suppressed the notification; a non-bool must fail open", stored)
		}
	}
}

// TestNotifyLabelFilter_NilReceiverAndNilStore_Allow keeps the
// optional-dependency convention honest: a filter that was never wired must
// behave exactly as no filter at all.
func TestNotifyLabelFilter_NilReceiverAndNilStore_Allow(t *testing.T) {
	var nilFilter *notifyLabelFilter
	if !nilFilter.NotifyAllowed(labelEvent("person")) {
		t.Error("a nil *notifyLabelFilter suppressed a notification")
	}
	if !newNotifyLabelFilter(nil).NotifyAllowed(labelEvent("person")) {
		t.Error("a filter with a nil store suppressed a notification")
	}
}
