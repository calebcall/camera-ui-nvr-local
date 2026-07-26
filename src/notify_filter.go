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

	sdk "github.com/cameraui/sdk/go"

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
}

// newNotifyLabelFilter returns a filter reading from store. A nil store (or a
// nil filter) allows everything, matching the optional-dependency convention
// the rest of the ingestion path uses.
func newNotifyLabelFilter(store notifySettingsStore) *notifyLabelFilter {
	return &notifyLabelFilter{store: store}
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
		enabled, ok := f.store.GetValue(key, true).(bool)
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
