// Per-detection-type notification filtering: which kinds of detection are
// worth a notification, as a set of toggles on the settings page's Detections
// tab.
//
// This exists because notifications were previously all-or-nothing. The
// frontend's own notification settings (DBNotificationSettings, camera.ui
// server) offer a master switch, a per-plugin source switch, per-system-type
// switches, and quiet hours — but nothing about detection labels — so a user
// who wanted "tell me about people, not passing cars" had no option short of
// silencing this plugin entirely.
//
// Deliberately PLUGIN-WIDE rather than per-camera: the SDK exposes only a
// plugin-level StorageSchema (there is no per-camera schema hook), so a
// per-camera version would mean inventing a bespoke storage shape and an editor
// for it. camera.ui's automation system already does per-camera label filtering
// properly (a detection trigger filters on cameraId plus detectionLabels, feeding
// a notification action), so anyone needing that granularity has a supported
// route that this does not attempt to duplicate.
//
// This filter affects NOTIFICATIONS ONLY. Ingestion, recording, retention,
// thumbnails, and AI descriptions all continue regardless — a detection the user
// does not want to be pinged about is still footage they will want to review.
package main

import (
	"strings"
	"sync"

	sdk "github.com/cameraui/sdk/go"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/recorder"
	"github.com/calebcall/plugins/camera-ui-nvr-local/src/store"
)

// Storage keys for the notification toggles, all boolean and all defaulting to
// true so that enabling this feature changes nothing until a user turns
// something off.
//
// There is deliberately no motion or audio key. store.EventHasDetections
// already excludes motion-only and audio-only events from notifying at all
// (notify consults it before this filter is ever reached), so such a toggle
// would be a control that does nothing — or, if it were wired to start
// notifying for audio, a behavior change smuggled in under a filtering feature.
const (
	notifyPersonKey  = "notifyPerson"
	notifyVehicleKey = "notifyVehicle"
	notifyAnimalKey  = "notifyAnimal"
	notifyPackageKey = "notifyPackage"

	// notifyOtherKey governs classifier-produced labels outside the standard
	// set — what a bird, weather, or scene classifier plugin emits. Without it
	// those labels would be the one category of detection a user could not
	// filter. sdk.KnownEventTypes exists precisely to identify them.
	notifyOtherKey = "notifyOther"

	// notifyOverrideKey lives on a CAMERA's storage, not the plugin's. While it
	// is false (the default) that camera follows the plugin-wide toggles; while
	// it is true the camera's own copies of the five keys above are used
	// instead, and the global values are ignored entirely for it.
	//
	// An explicit override flag rather than a tri-state per toggle: JsonSchema
	// booleans are two-state, so "use global / force on / force off" per type
	// would mean five enums, and the form would stop reading like a set of
	// switches. This also keeps every existing install untouched — no camera
	// overrides anything until someone deliberately turns this on.
	notifyOverrideKey = "notifyOverride"
)

// notifyObjectLabelKeys maps each standard object-detection label to its
// toggle. Keyed by the lowercase label as it appears in sdk.DetectionLabels;
// motion and audio are absent by design (see the key block above).
var notifyObjectLabelKeys = map[string]string{
	"person":  notifyPersonKey,
	"vehicle": notifyVehicleKey,
	"animal":  notifyAnimalKey,
	"package": notifyPackageKey,
}

// notifySettingsStore is the subset of *sdk.DeviceStorage this filter needs.
// Variadic to match the real method, exactly as describe.ConfigGetter is.
type notifySettingsStore interface {
	GetValue(key string, fallback ...any) any
}

// cameraNotifySettings resolves a camera ID to that camera's own settings
// store, if this plugin currently has one for it. *cameraNotifyRegistry
// (plugin.go) satisfies it; ok is false for a camera that was never attached or
// has since been released, which falls back to the plugin-wide settings.
type cameraNotifySettings interface {
	CameraNotifySettings(cameraID string) (notifySettingsStore, bool)
}

// notifyLabelFilter answers "should this event produce a notification?" from
// the current toggle values.
//
// Values are read per event rather than cached, the same choice describe.Load
// and NVRPlugin.nvrQuotaGB make: turning a detection type off in the UI must
// take effect on the next event without restarting the plugin, which is exactly
// how someone will use this — noticing they are being pinged about passing cars
// and wanting it to stop now.
type notifyLabelFilter struct {
	store notifySettingsStore

	// cameras supplies per-camera overrides. nil means plugin-wide settings
	// govern every camera, which is exactly the behavior before overrides
	// existed.
	cameras cameraNotifySettings
}

// newNotifyLabelFilter returns a filter reading from store. A nil store (or a
// nil filter) allows everything, matching the optional-dependency convention
// the rest of the ingestion path uses.
func newNotifyLabelFilter(store notifySettingsStore, cameras cameraNotifySettings) *notifyLabelFilter {
	return &notifyLabelFilter{store: store, cameras: cameras}
}

// settingsFor picks which store governs cameraID: the camera's own when it has
// one AND that camera has notifyOverride turned on, otherwise the plugin-wide
// store.
//
// Read per event rather than resolved once, for the same reason every other
// setting here is: flipping the override on a camera must take effect on its
// next event without a restart.
//
// Every failure path falls back to the global store rather than to "allow" —
// this is only choosing WHICH toggles apply, and the fail-open decisions belong
// in NotifyAllowed where they can be made once.
func (f *notifyLabelFilter) settingsFor(cameraID string) notifySettingsStore {
	if f.cameras == nil {
		return f.store
	}
	cameraStore, ok := f.cameras.CameraNotifySettings(cameraID)
	if !ok || cameraStore == nil {
		return f.store
	}
	if override, isBool := cameraStore.GetValue(notifyOverrideKey, false).(bool); isBool && override {
		return cameraStore
	}
	return f.store
}

// NotifyAllowed reports whether event's detection types permit a notification.
//
// An event is allowed when ANY of its classifiable labels is enabled, not when
// all are. A single event routinely carries several labels (a person arriving in
// a vehicle produces both), and a user who disabled Vehicle wants to be told
// about the person rather than to have the whole event suppressed because a car
// was also in frame.
//
// An event with no classifiable labels at all is ALLOWED. Every label is either
// one of the four toggles, an ignorable non-subject (motion/audio, an attribute
// like face, a trigger type like doorbell), or "other" — so this covers only the
// case of an event carrying nothing but ignorable types, which
// store.EventHasDetections should already have rejected. Allowing it means a
// detection shape nobody anticipated produces an extra notification rather than
// silent, undiagnosable suppression; that is the right way for a filter to fail.
func (f *notifyLabelFilter) NotifyAllowed(event store.DetectionEvent) bool {
	if f == nil || f.store == nil {
		return true
	}

	settings := f.settingsFor(event.CameraID)

	var sawClassifiable bool
	for _, label := range notifiableLabels(event) {
		key, isObjectLabel := notifyObjectLabelKeys[label]
		if !isObjectLabel {
			// A known non-subject type — motion, audio, an attribute such as
			// face/license_plate, or a trigger type such as doorbell. These
			// never justify a notification on their own and must not keep an
			// event alive after its actual object label was turned off, so they
			// are skipped rather than treated as "other".
			if _, known := sdk.KnownEventTypes[label]; known {
				continue
			}
			key = notifyOtherKey
		}

		sawClassifiable = true

		// A value stored as something other than a bool (a hand-edited config,
		// or a future storage encoding) must read as ENABLED, not disabled. The
		// bare `v, _ := x.(bool)` idiom yields false on a failed assertion,
		// which would silently silence a whole detection type on the strength of
		// a malformed value — exactly the failure this feature must not have.
		enabled, ok := settings.GetValue(key, true).(bool)
		if !ok {
			enabled = true
		}
		if enabled {
			return true
		}
	}

	return !sawClassifiable
}

// notifiableLabels returns event's distinct candidate labels, normalized to
// lowercase.
//
// The union of event.Types and every segment detection's Label is deliberate,
// not belt-and-braces: a terminal 'end' message is routinely sparse (that is
// what the merge accumulator in events_ingest_merge.go exists to compensate
// for), so either source alone can be empty for an event that genuinely has
// detections. store.EventHasDetections reads Types; the AI-description label
// gate reads segment detections; this needs to agree with both.
func notifiableLabels(event store.DetectionEvent) []string {
	seen := make(map[string]struct{})
	var labels []string

	add := func(raw string) {
		norm := strings.ToLower(strings.TrimSpace(raw))
		if norm == "" {
			return
		}
		if _, ok := seen[norm]; ok {
			return
		}
		seen[norm] = struct{}{}
		labels = append(labels, norm)
	}

	for _, t := range event.Types {
		add(t)
	}
	for _, seg := range event.Segments {
		for _, d := range seg.Detections {
			add(d.Label)
		}
	}
	return labels
}

// cameraNotifySchema declares the per-camera notification override fields on a
// camera's own storage scope.
//
// Rendered by the camera drawer's Plugins tab, hub section: that tab calls
// useCameraStorage(camera, plugin) for every plugin whose contract is a Hub
// role, which this plugin's is — the same surface the per-camera recording
// settings already appear on. Nothing in the frontend needs changing.
//
// Declared additively via recorder.CameraStorage's HasSchema/AddSchema rather
// than DefineSchemas, because a camera's storage scope is shared with the
// recording config (recorder/manager.go) and DefineSchemas replaces the entire
// schema list for the scope — see declareCameraSchemas there.
func cameraNotifySchema() []sdk.JsonSchema {
	storeTrue := true

	overrideCondition := func() []sdk.SchemaCondition {
		return []sdk.SchemaCondition{{
			Key:      notifyOverrideKey,
			Operator: sdk.SchemaConditionEq,
			Value:    true,
		}}
	}

	schemas := []sdk.JsonSchema{{
		Type:         sdk.JsonSchemaTypeBoolean,
		Key:          notifyOverrideKey,
		Title:        "Override notification settings",
		Description:  "By default this camera follows the global Detections settings on Settings → Recordings. Turn this on to give it its own.",
		DefaultValue: false,
		Store:        &storeTrue,
	}}

	// Same keys as the plugin-wide toggles, deliberately: the filter reads
	// whichever store is in effect using one set of key names, so there is no
	// second naming scheme to keep in step.
	for _, f := range []struct{ key, title, description string }{
		{notifyPersonKey, "Notify: Person", "Send a notification when a person is detected on this camera."},
		{notifyVehicleKey, "Notify: Vehicle", "Send a notification when a vehicle is detected on this camera."},
		{notifyAnimalKey, "Notify: Animal", "Send a notification when an animal is detected on this camera."},
		{notifyPackageKey, "Notify: Package", "Send a notification when a package is detected on this camera."},
		{notifyOtherKey, "Notify: Other detections", "Send a notification for detection types outside the standard set on this camera."},
	} {
		schemas = append(schemas, sdk.JsonSchema{
			Type:         sdk.JsonSchemaTypeBoolean,
			Key:          f.key,
			Title:        f.title,
			Description:  f.description + " Affects notifications only — recording, the timeline, and AI descriptions are unaffected.",
			DefaultValue: true,
			Store:        &storeTrue,
			Condition:    overrideCondition(),
		})
	}
	return schemas
}

// cameraNotifyRegistry tracks each attached camera's own settings storage, so
// the shared notifyLabelFilter can resolve per-camera overrides from an event's
// CameraID alone.
//
// A registry rather than per-camera ingesters because detectionEventIngester is
// deliberately one shared instance across every camera (it holds no per-camera
// state — the event identifies its own camera), and giving each camera its own
// would duplicate the accumulator and the notification dedup set along with it.
//
// Guarded by a mutex for the same reason detectionSubscriptions is: the camera
// lifecycle callbacks that populate it are host-driven with no documented
// single-goroutine guarantee, and notification filtering reads it from the
// detection-event callback.
type cameraNotifyRegistry struct {
	mu     sync.RWMutex
	stores map[string]recorder.CameraStorage
}

// add registers cameraID's storage and declares the override schema on it.
// Declaration happens here, once per attach, rather than on every read: unlike
// the recording config there is no equivalent of readRecordingConfig running
// periodically to re-assert it.
func (r *cameraNotifyRegistry) add(cameraID string, storage recorder.CameraStorage) {
	if storage == nil {
		return
	}

	for _, schema := range cameraNotifySchema() {
		if storage.HasSchema(schema.Key) {
			continue
		}
		_ = storage.AddSchema(&schema)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stores == nil {
		r.stores = make(map[string]recorder.CameraStorage)
	}
	r.stores[cameraID] = storage
}

// remove drops a released camera, so a hub detach doesn't leak its storage
// handle for the lifetime of the process.
func (r *cameraNotifyRegistry) remove(cameraID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.stores, cameraID)
}

// CameraNotifySettings satisfies cameraNotifySettings. ok is false for a camera
// this plugin has never attached (or has released), which the filter treats as
// "use the plugin-wide settings".
func (r *cameraNotifyRegistry) CameraNotifySettings(cameraID string) (notifySettingsStore, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	storage, ok := r.stores[cameraID]
	if !ok || storage == nil {
		return nil, false
	}
	return storage, true
}
