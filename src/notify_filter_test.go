package main

import (
	"errors"
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
	f := newNotifyLabelFilter(notifyStore{}, nil)

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
			f := newNotifyLabelFilter(notifyStore{tc.key: false}, nil)

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
	f := newNotifyLabelFilter(notifyStore{notifyVehicleKey: false}, nil)

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
	}, nil)

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
			if newNotifyLabelFilter(notifyStore{notifyOtherKey: false}, nil).NotifyAllowed(labelEvent(label)) {
				t.Errorf("%q notified with %s=false", label, notifyOtherKey)
			}
			if !newNotifyLabelFilter(notifyStore{notifyOtherKey: true}, nil).NotifyAllowed(labelEvent(label)) {
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
	f := newNotifyLabelFilter(notifyStore{notifyOtherKey: false}, nil)

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
	f := newNotifyLabelFilter(notifyStore{notifyPersonKey: false}, nil)

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
	}, nil)
	nonSubject := store.DetectionEvent{ID: "ev-2", Types: []string{"motion", "audio", "doorbell", "face"}}
	if !allOff.NotifyAllowed(nonSubject) {
		t.Error("an event with no classifiable label was suppressed; unanticipated shapes must fail open")
	}
}

// TestNotifyLabelFilter_NoLabelsAtAll_Allows pins the fail-open default. An
// extra notification for a shape we did not anticipate is recoverable; silent,
// undiagnosable suppression is not.
func TestNotifyLabelFilter_NoLabelsAtAll_Allows(t *testing.T) {
	f := newNotifyLabelFilter(notifyStore{notifyPersonKey: false, notifyOtherKey: false}, nil)

	if !f.NotifyAllowed(store.DetectionEvent{ID: "ev-1"}) {
		t.Error("an event with no types and no segments was suppressed; it must fail open")
	}
}

// TestNotifyLabelFilter_ReadsEitherLabelSource matters because a terminal 'end'
// message is routinely sparse — that is why the merge accumulator exists — so
// either Types or the segment detections can be empty for an event that really
// does have detections.
func TestNotifyLabelFilter_ReadsEitherLabelSource(t *testing.T) {
	f := newNotifyLabelFilter(notifyStore{notifyVehicleKey: false}, nil)

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
	f := newNotifyLabelFilter(notifyStore{notifyPersonKey: false}, nil)

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
		if !newNotifyLabelFilter(notifyStore{notifyPersonKey: stored}, nil).NotifyAllowed(labelEvent("person")) {
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
	if !newNotifyLabelFilter(nil, nil).NotifyAllowed(labelEvent("person")) {
		t.Error("a filter with a nil store suppressed a notification")
	}
}

// fakeCameraNotifySettings resolves camera IDs to per-camera stores.
type fakeCameraNotifySettings map[string]notifyStore

func (f fakeCameraNotifySettings) CameraNotifySettings(cameraID string) (notifySettingsStore, bool) {
	s, ok := f[cameraID]
	if !ok {
		return nil, false
	}
	return s, true
}

func camEvent(cameraID string, labels ...string) store.DetectionEvent {
	ev := labelEvent(labels...)
	ev.CameraID = cameraID
	return ev
}

// TestNotifyLabelFilter_OverrideOff_FollowsGlobalSettings is the inheritance
// default: a camera with settings storage but no override in effect must behave
// exactly as it did before per-camera settings existed.
func TestNotifyLabelFilter_OverrideOff_FollowsGlobalSettings(t *testing.T) {
	global := notifyStore{notifyVehicleKey: false}
	cameras := fakeCameraNotifySettings{
		// The camera has its OWN vehicle=true, which must be ignored while the
		// override flag is off.
		"cam-1": notifyStore{notifyOverrideKey: false, notifyVehicleKey: true},
	}
	f := newNotifyLabelFilter(global, cameras)

	if f.NotifyAllowed(camEvent("cam-1", "vehicle")) {
		t.Error("camera used its own vehicle=true while override was off; global vehicle=false must win")
	}
	if !f.NotifyAllowed(camEvent("cam-1", "person")) {
		t.Error("person was suppressed; global person is enabled")
	}
}

// TestNotifyLabelFilter_OverrideOn_UsesCameraSettings is the point of the
// feature, in both directions: a camera can be stricter than global AND more
// permissive than it.
func TestNotifyLabelFilter_OverrideOn_UsesCameraSettings(t *testing.T) {
	t.Run("stricter than global", func(t *testing.T) {
		f := newNotifyLabelFilter(
			notifyStore{}, // everything on globally
			fakeCameraNotifySettings{"cam-1": notifyStore{notifyOverrideKey: true, notifyPersonKey: false}},
		)
		if f.NotifyAllowed(camEvent("cam-1", "person")) {
			t.Error("camera override person=false was ignored")
		}
	})

	t.Run("more permissive than global", func(t *testing.T) {
		f := newNotifyLabelFilter(
			notifyStore{notifyVehicleKey: false}, // vehicles off globally
			fakeCameraNotifySettings{"cam-1": notifyStore{notifyOverrideKey: true, notifyVehicleKey: true}},
		)
		if !f.NotifyAllowed(camEvent("cam-1", "vehicle")) {
			t.Error("camera could not re-enable a type disabled globally; that is the reason overrides are not ANDed with global")
		}
	})
}

// TestNotifyLabelFilter_OverrideIsPerCamera checks that one camera's override
// does not leak to another — the failure mode that would make this worse than
// no feature at all.
func TestNotifyLabelFilter_OverrideIsPerCamera(t *testing.T) {
	f := newNotifyLabelFilter(
		notifyStore{},
		fakeCameraNotifySettings{
			"cam-1": notifyStore{notifyOverrideKey: true, notifyPersonKey: false},
			"cam-2": notifyStore{notifyOverrideKey: false},
		},
	)

	if f.NotifyAllowed(camEvent("cam-1", "person")) {
		t.Error("cam-1 override did not apply")
	}
	if !f.NotifyAllowed(camEvent("cam-2", "person")) {
		t.Error("cam-1's override leaked to cam-2")
	}
	if !f.NotifyAllowed(camEvent("cam-unknown", "person")) {
		t.Error("an unregistered camera did not fall back to global")
	}
}

// TestNotifyLabelFilter_CameraResolutionFailures_FallBackToGlobal covers every
// way resolution can come up empty. All of them must land on the global
// settings rather than on "allow" or "deny" — this step only chooses WHICH
// toggles apply.
func TestNotifyLabelFilter_CameraResolutionFailures_FallBackToGlobal(t *testing.T) {
	global := notifyStore{notifyVehicleKey: false}

	for _, tc := range []struct {
		name    string
		cameras cameraNotifySettings
	}{
		{"nil lookup", nil},
		{"camera not registered", fakeCameraNotifySettings{}},
		{"override absent", fakeCameraNotifySettings{"cam-1": notifyStore{}}},
		{"override non-bool", fakeCameraNotifySettings{"cam-1": notifyStore{notifyOverrideKey: "yes", notifyVehicleKey: true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNotifyLabelFilter(global, tc.cameras)
			if f.NotifyAllowed(camEvent("cam-1", "vehicle")) {
				t.Error("resolution failure did not fall back to the global settings")
			}
		})
	}
}

// TestNotifyLabelFilter_OverrideOn_UnsetCameraKeysDefaultToOn keeps an override
// from being a silent mute. Turning the flag on with nothing else configured
// must notify for everything, not nothing.
func TestNotifyLabelFilter_OverrideOn_UnsetCameraKeysDefaultToOn(t *testing.T) {
	f := newNotifyLabelFilter(
		notifyStore{notifyPersonKey: false, notifyVehicleKey: false},
		fakeCameraNotifySettings{"cam-1": notifyStore{notifyOverrideKey: true}},
	)

	for _, label := range []string{"person", "vehicle", "animal", "package", "bird"} {
		if !f.NotifyAllowed(camEvent("cam-1", label)) {
			t.Errorf("%q was suppressed on a camera whose override is on but has no toggles set", label)
		}
	}
}

// TestCameraNotifyRegistry_AddDeclaresSchemaAndResolves covers the registry
// itself, including that a released camera stops resolving.
func TestCameraNotifyRegistry_AddDeclaresSchemaAndResolves(t *testing.T) {
	var r cameraNotifyRegistry
	storage := newFakeCameraSettings()

	r.add("cam-1", storage)

	for _, key := range []string{notifyOverrideKey, notifyTypesKey} {
		if !storage.HasSchema(key) {
			t.Errorf("schema %q was not declared on the camera's storage", key)
		}
	}

	if _, ok := r.CameraNotifySettings("cam-1"); !ok {
		t.Error("cam-1 did not resolve after add")
	}

	r.remove("cam-1")
	if _, ok := r.CameraNotifySettings("cam-1"); ok {
		t.Error("cam-1 still resolves after remove; released cameras must not leak")
	}
}

// TestCameraNotifyRegistry_AddIsIdempotent matters because OnCameraAdded can
// fire more than once for the same camera, and AddSchema errors on a duplicate
// key.
func TestCameraNotifyRegistry_AddIsIdempotent(t *testing.T) {
	var r cameraNotifyRegistry
	storage := newFakeCameraSettings()

	r.add("cam-1", storage)
	declared := len(storage.declared)
	r.add("cam-1", storage)

	if got := len(storage.declared); got != declared {
		t.Errorf("declared key count went %d -> %d on re-add", declared, got)
	}
	if _, ok := r.CameraNotifySettings("cam-1"); !ok {
		t.Error("cam-1 stopped resolving after a second add")
	}
}

func TestCameraNotifyRegistry_NilStorage_IsIgnored(t *testing.T) {
	var r cameraNotifyRegistry
	r.add("cam-1", nil) // must not panic

	if _, ok := r.CameraNotifySettings("cam-1"); ok {
		t.Error("a nil storage was registered; it must be ignored")
	}
}

// fakeCameraSettings is an in-memory recorder.CameraStorage for registry tests.
type fakeCameraSettings struct {
	values   map[string]any
	declared map[string]bool
}

func newFakeCameraSettings() *fakeCameraSettings {
	return &fakeCameraSettings{values: map[string]any{}, declared: map[string]bool{}}
}

func (f *fakeCameraSettings) GetValue(key string, fallback ...any) any {
	if v, ok := f.values[key]; ok {
		return v
	}
	if len(fallback) > 0 {
		return fallback[0]
	}
	return nil
}

func (f *fakeCameraSettings) HasSchema(key string) bool { return f.declared[key] }

func (f *fakeCameraSettings) AddSchema(schema *sdk.JsonSchema) error {
	if f.declared[schema.Key] {
		return errDuplicateSchema
	}
	f.declared[schema.Key] = true
	if _, exists := f.values[schema.Key]; !exists && schema.DefaultValue != nil {
		f.values[schema.Key] = schema.DefaultValue
	}
	return nil
}

var errDuplicateSchema = errors.New("schema already exists")

// TestNotifyLabelFilter_NotifyTypes_GovernsWhichLabelsNotify covers the new
// list-shaped setting directly.
func TestNotifyLabelFilter_NotifyTypes_GovernsWhichLabelsNotify(t *testing.T) {
	f := newNotifyLabelFilter(notifyStore{notifyTypesKey: []string{"person", "package"}}, nil)

	for _, tc := range []struct {
		label string
		want  bool
	}{
		{"person", true},
		{"package", true},
		{"vehicle", false},
		{"animal", false},
		{"bird", false}, // "other" not selected
	} {
		t.Run(tc.label, func(t *testing.T) {
			if got := f.NotifyAllowed(labelEvent(tc.label)); got != tc.want {
				t.Errorf("NotifyAllowed(%q) = %v, want %v", tc.label, got, tc.want)
			}
		})
	}
}

// TestNotifyLabelFilter_EmptyNotifyTypes_SuppressesEverything is the case a
// plain default value cannot express, and the reason enabledNotifyTypes uses a
// nil sentinel rather than a default: an empty selection is a deliberate choice
// made in the UI, and treating it as "unset" would silently re-enable
// everything the user just turned off.
func TestNotifyLabelFilter_EmptyNotifyTypes_SuppressesEverything(t *testing.T) {
	f := newNotifyLabelFilter(notifyStore{notifyTypesKey: []string{}}, nil)

	for _, label := range []string{"person", "vehicle", "animal", "package", "bird"} {
		if f.NotifyAllowed(labelEvent(label)) {
			t.Errorf("%q notified with an explicitly empty selection", label)
		}
	}
}

// TestNotifyLabelFilter_NotifyTypes_AcceptsMsgpackShapes covers the list coming
// back as []any rather than []string, which is what a round trip through
// storage produces.
func TestNotifyLabelFilter_NotifyTypes_AcceptsMsgpackShapes(t *testing.T) {
	f := newNotifyLabelFilter(notifyStore{notifyTypesKey: []any{"person", " VEHICLE ", 42}}, nil)

	if !f.NotifyAllowed(labelEvent("person")) {
		t.Error("person was suppressed with a []any selection")
	}
	if !f.NotifyAllowed(labelEvent("vehicle")) {
		t.Error("vehicle was suppressed; entries must be trimmed and lowercased")
	}
	if f.NotifyAllowed(labelEvent("animal")) {
		t.Error("animal notified though it was not selected")
	}
}

// TestNotifyLabelFilter_LegacyBooleans_StillHonored is the 5.5.0/5.6.0 upgrade
// path. Those five keys may already be configured, and losing them on upgrade
// would silently restore notifications the user had turned off.
func TestNotifyLabelFilter_LegacyBooleans_StillHonored(t *testing.T) {
	// No notifyTypes key at all — only the legacy booleans.
	f := newNotifyLabelFilter(notifyStore{notifyVehicleKey: false, notifyOtherKey: false}, nil)

	if f.NotifyAllowed(labelEvent("vehicle")) {
		t.Error("legacy notifyVehicle=false was ignored")
	}
	if f.NotifyAllowed(labelEvent("bird")) {
		t.Error("legacy notifyOther=false was ignored")
	}
	if !f.NotifyAllowed(labelEvent("person")) {
		t.Error("person was suppressed; unset legacy keys default to true")
	}
}

// TestNotifyLabelFilter_NotifyTypes_WinsOverLegacyBooleans pins the precedence.
// Once the new key exists it is authoritative, including when it contradicts a
// stale legacy boolean left behind on disk.
func TestNotifyLabelFilter_NotifyTypes_WinsOverLegacyBooleans(t *testing.T) {
	f := newNotifyLabelFilter(notifyStore{
		notifyTypesKey:   []string{"vehicle"},
		notifyVehicleKey: false, // stale, must be ignored
		notifyPersonKey:  true,  // stale, must be ignored
	}, nil)

	if !f.NotifyAllowed(labelEvent("vehicle")) {
		t.Error("notifyTypes did not win over a stale legacy notifyVehicle=false")
	}
	if f.NotifyAllowed(labelEvent("person")) {
		t.Error("a stale legacy notifyPerson=true re-enabled a type not in notifyTypes")
	}
}

// TestNotifyLabelFilter_PerCameraNotifyTypes_Overrides checks the list-shaped
// setting works through the per-camera override too, in both directions.
func TestNotifyLabelFilter_PerCameraNotifyTypes_Overrides(t *testing.T) {
	f := newNotifyLabelFilter(
		notifyStore{notifyTypesKey: []string{"person"}},
		fakeCameraNotifySettings{
			"cam-1": notifyStore{notifyOverrideKey: true, notifyTypesKey: []string{"vehicle"}},
			"cam-2": notifyStore{notifyOverrideKey: false, notifyTypesKey: []string{"vehicle"}},
		},
	)

	if !f.NotifyAllowed(camEvent("cam-1", "vehicle")) {
		t.Error("cam-1 override did not enable vehicle")
	}
	if f.NotifyAllowed(camEvent("cam-1", "person")) {
		t.Error("cam-1 override did not exclude person")
	}
	if !f.NotifyAllowed(camEvent("cam-2", "person")) {
		t.Error("cam-2 should follow global (person) while its override is off")
	}
	if f.NotifyAllowed(camEvent("cam-2", "vehicle")) {
		t.Error("cam-2 used its own list while its override was off")
	}
}
