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
	// notifyTypesKey holds the enabled detection types as a list, rendered as a
	// single multi-select. It replaced five separate booleans, which the
	// frontend drew as five bordered toggle rows — one control conveys the same
	// thing in a fraction of the height.
	notifyTypesKey = "notifyTypes"

	// The five legacy boolean keys, shipped in 5.5.0/5.6.0. Retained READ-ONLY
	// as a migration fallback for configs written before notifyTypesKey existed,
	// and deliberately absent from every schema so nothing writes to them again.
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

// notifyTypeOther is the multi-select entry covering classifier-produced labels
// outside the standard set, mirroring what notifyOtherKey governed.
const notifyTypeOther = "other"

// notifyTypeOptions is the multi-select's option list, and — as the schema's
// DefaultValue — the set a fresh install notifies for. Order is presentation.
var notifyTypeOptions = []string{"person", "vehicle", "animal", "package", notifyTypeOther}

// enabledNotifyTypes resolves which detection types a store permits.
//
// Resolution order matters and is the whole migration story:
//
//  1. notifyTypesKey present — use it verbatim, INCLUDING an empty list. Empty
//     means "notify for nothing", a deliberate choice someone made in the UI, and
//     treating it as "unset" would silently re-enable everything they turned off.
//     Distinguishing absent from empty is the entire point; this repo has already
//     been bitten once by conflating them (see keyRoles in recorder/manager.go,
//     where every camera stored [] and recorded nothing).
//  2. Absent — derive from the five legacy booleans, each defaulting to true, so
//     a config written by 5.5.0 or 5.6.0 keeps behaving exactly as it did.
//
// The second value reports whether any resolution succeeded; false means the
// store had nothing to say and the caller should fall back (a camera without an
// override, for instance).
func enabledNotifyTypes(s notifySettingsStore) (map[string]bool, bool) {
	if s == nil {
		return nil, false
	}

	// A sentinel distinguishes "key absent" from "key present but empty", which
	// a plain default value cannot.
	if raw := s.GetValue(notifyTypesKey, nil); raw != nil {
		enabled := make(map[string]bool, len(notifyTypeOptions))
		for _, t := range stringSliceValue(raw) {
			if norm := strings.ToLower(strings.TrimSpace(t)); norm != "" {
				enabled[norm] = true
			}
		}
		return enabled, true
	}

	enabled := make(map[string]bool, len(notifyTypeOptions))
	for label, key := range notifyObjectLabelKeys {
		enabled[label] = boolValue(s.GetValue(key, true), true)
	}
	enabled[notifyTypeOther] = boolValue(s.GetValue(notifyOtherKey, true), true)
	return enabled, true
}

// boolValue coerces a stored value, falling back for anything that is not a
// bool. The bare `v, _ := x.(bool)` idiom yields false on a failed assertion,
// which would silently silence a detection type on the strength of a malformed
// value.
func boolValue(v any, fallback bool) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return fallback
}

// stringSliceValue coerces whatever shape a stored list comes back as — []string
// from a schema DefaultValue, []any once it has been through msgpack.
func stringSliceValue(v any) []string {
	switch t := v.(type) {
	case []string:
		return append([]string(nil), t...)
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if str, ok := item.(string); ok {
				out = append(out, str)
			}
		}
		return out
	default:
		return nil
	}
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

	enabled, ok := enabledNotifyTypes(f.settingsFor(event.CameraID))
	if !ok {
		return true
	}

	var sawClassifiable bool
	for _, label := range notifiableLabels(event) {
		typ, isObjectLabel := label, true
		if _, known := notifyObjectLabelKeys[label]; !known {
			isObjectLabel = false
		}
		if !isObjectLabel {
			// A known non-subject type — motion, audio, an attribute such as
			// face/license_plate, or a trigger type such as doorbell. These
			// never justify a notification on their own and must not keep an
			// event alive after its actual object label was turned off, so they
			// are skipped rather than treated as "other".
			if _, known := sdk.KnownEventTypes[label]; known {
				continue
			}
			typ = notifyTypeOther
		}

		sawClassifiable = true
		if enabled[typ] {
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

	return []sdk.JsonSchema{
		{
			Type:         sdk.JsonSchemaTypeBoolean,
			Key:          notifyOverrideKey,
			Title:        "Override notifications",
			DefaultValue: false,
			Store:        &storeTrue,
		},
		{
			Type:         sdk.JsonSchemaTypeString,
			Key:          notifyTypesKey,
			Title:        "Notify for",
			Enum:         notifyTypeOptions,
			Multiple:     true,
			DefaultValue: notifyTypeOptions,
			Store:        &storeTrue,
			Condition: []sdk.SchemaCondition{{
				Key:      notifyOverrideKey,
				Operator: sdk.SchemaConditionEq,
				Value:    true,
			}},
		},
	}
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
