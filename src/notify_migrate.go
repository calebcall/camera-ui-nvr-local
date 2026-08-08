// One-time import of the closed @camera.ui/camera-ui-nvr plugin's per-camera
// object filter (notifyObjects) into this fork's own notification settings
// (notifyTypes/notifyOverride, notify_filter.go).
//
// This fork ships under the SAME package name as the closed plugin it replaces
// — deliberately, so camera.ui's auto-updater never swaps this local build for
// the license-gated original (see CHANGELOG.md's versioning note). Same package
// name means the same storage scope, so every value the closed plugin ever
// wrote is still sitting in the store this plugin reads. notifyObjects is the
// interesting one: its per-camera "notify me about these object types" list.
//
// This fork was written as a reimplementation rather than a port, so it invented
// its own key vocabulary instead of adopting that one. The failure mode was
// silent and genuinely hard to spot: someone who had set a camera to "person
// only" under the closed plugin still had that choice stored, still saw a
// plausible-looking notification section in the camera drawer, and simply was
// not filtered by any of it — the plugin read different keys.
//
// Deliberately a WRITE rather than a read-time fallback. A fallback would leave
// the camera behaving one way while the "Notify for" control in its drawer said
// another, which is the same class of invisible mismatch that caused the
// original complaint. Migrating into the real keys keeps what the UI shows and
// what the filter uses the same thing, and leaves the result editable — and
// undoable — like any other setting.
package main

import (
	"strings"

	sdk "github.com/cameraui/sdk/go"
)

const (
	// notifyObjectsKey is the closed plugin's per-camera object filter: a list
	// of object labels ("person", "vehicle", ...) meaning "notify for these".
	// Read-only here, and deliberately absent from every schema — this plugin
	// imports it once and then never writes to it, so the original values stay
	// intact for inspection (and for a second run if a future migration needs
	// them).
	notifyObjectsKey = "notifyObjects"

	// notifyObjectsMigratedKey records that a camera has been through this
	// migration, so it runs exactly once per camera rather than once per attach.
	//
	// A marker is load-bearing rather than tidiness. Without it the migration
	// re-fires on the next restart of any camera whose settings are back in the
	// inert state — which is exactly what someone does when they turn
	// "Override notifications" off again because they decided they want the
	// camera to follow the plugin-wide toggles after all. Re-applying the
	// import there would silently undo a deliberate choice, every restart,
	// forever.
	//
	// Hidden, because it is bookkeeping and not a setting: without that flag
	// the camera drawer would render a stray toggle nobody can interpret. It
	// also carries no DefaultValue on purpose — AddSchema writes a non-nil
	// DefaultValue straight into storage (sdk storage.go), and a marker that
	// materializes itself the moment it is declared would mark every camera as
	// migrated before the migration ever looked at one.
	notifyObjectsMigratedKey = "notifyObjectsMigrated"

	// notifyBooleansMigratedKey is notifyObjectsMigratedKey's counterpart for
	// the pre-5.7 booleans, on both the plugin and per-camera scopes. Hidden
	// and DefaultValue-free for the same reasons.
	notifyBooleansMigratedKey = "notifyBooleansMigrated"
)

// notifyMigrationStore is the storage surface this migration needs: everything
// notifySettingsStore reads, plus the writes.
//
// Separate from recorder.CameraStorage (which has no SetValue) rather than
// widening that interface: CameraStorage is the recording manager's contract
// and is implemented by several test fakes there, none of which have any
// business growing a writer for this. cameraNotifyRegistry.add type-asserts to
// this instead — a real camera's storage is always an *sdk.DeviceStorage, which
// satisfies it.
type notifyMigrationStore interface {
	notifySettingsStore
	SetValue(key string, value any) error
}

// migrateNotifyObjects imports one camera's notifyObjects into notifyTypes +
// notifyOverride, reporting whether it wrote anything.
//
// Must be called AFTER cameraNotifySchema has been declared on s: sdk's
// SetValue silently no-ops for a key with no registered schema, so migrating
// first would mark the camera done without having changed a thing.
func migrateNotifyObjects(cameraID string, s notifyMigrationStore, logger *sdk.Logger) bool {
	if s == nil {
		return false
	}
	if done, ok := s.GetValue(notifyObjectsMigratedKey, false).(bool); ok && done {
		return false
	}

	legacy, ok := legacyNotifyObjects(s)
	if !ok {
		// Nothing usable to import. Deliberately NOT marked as migrated: a
		// camera the closed plugin never configured may still be attached to
		// an install that gets those values restored from a backup later, and
		// there is no cost to looking again next start.
		return false
	}

	// Someone has already made a deliberate choice in the new vocabulary for
	// this camera. Their choice wins over a value from the plugin this one
	// replaced — but mark it done, so turning the override off later does not
	// hand the camera back to a migration they already superseded.
	if !cameraNotifySettingsAreInert(s) {
		markNotifyMigrated(notifyObjectsMigratedKey, cameraID, s, logger)
		if logger != nil {
			logger.Debug("nvr-local: notification migration: camera", cameraID, "already configured in the new settings, leaving it alone")
		}
		return false
	}

	// notifyTypes BEFORE notifyOverride, and the order is the crash-safety
	// story: interrupted after the first write, the camera holds an imported
	// list with the override still off — inert, i.e. exactly the behavior it
	// had before. Interrupted the other way round it would be notifying for
	// the full default set, which is the outcome this whole migration exists
	// to stop.
	if err := s.SetValue(notifyTypesKey, legacy); err != nil {
		if logger != nil {
			logger.Error("nvr-local: notification migration: camera", cameraID, "write notifyTypes failed:", err)
		}
		return false
	}
	if err := s.SetValue(notifyOverrideKey, true); err != nil {
		if logger != nil {
			logger.Error("nvr-local: notification migration: camera", cameraID, "write notifyOverride failed:", err)
		}
		// notifyTypes landed but the override did not, so the camera is still
		// inert and still unmarked — the next start retries the pair.
		return false
	}

	markNotifyMigrated(notifyObjectsMigratedKey, cameraID, s, logger)

	if logger != nil {
		logger.Log(
			"nvr-local: notification migration: camera", cameraID,
			"imported notifyObjects=["+strings.Join(legacy, ",")+"] from the previous NVR plugin;",
			"this camera now overrides the plugin-wide notification types instead of following them",
		)
	}
	return true
}

// legacyNotifyObjects reads and validates notifyObjects, returning the object
// labels to migrate in notifyTypeOptions order (stable output regardless of how
// the closed plugin happened to store them).
//
// An EMPTY result reports "nothing to migrate" rather than "notify for
// nothing", even though an empty notifyTypes means precisely the latter in this
// plugin's own vocabulary (see enabledNotifyTypes). The closed plugin's empty
// list is ambiguous — "the user switched everything off" and "the key was
// written but never configured" are indistinguishable from here — and the two
// readings fail in very different directions. Guessing "off" silences a camera
// completely, with no notification to notice and no obvious cause to find;
// guessing "unset" leaves it exactly as it is today. Only one of those is
// recoverable by a user who has not read this comment.
//
// Labels outside the four object types are dropped rather than mapped to
// "other": the closed plugin's list only ever held object labels, so anything
// else is a value this migration does not understand, and inventing an
// interpretation for it is how a migration turns into a bug.
func legacyNotifyObjects(s notifySettingsStore) ([]string, bool) {
	raw := s.GetValue(notifyObjectsKey, nil)
	if raw == nil {
		return nil, false
	}

	want := make(map[string]struct{})
	for _, v := range stringSliceValue(raw) {
		if norm := strings.ToLower(strings.TrimSpace(v)); norm != "" {
			want[norm] = struct{}{}
		}
	}

	migrated := make([]string, 0, len(notifyTypeOptions))
	for _, opt := range notifyTypeOptions {
		if opt == notifyTypeOther {
			continue // no counterpart in the closed plugin's vocabulary
		}
		if _, ok := want[opt]; ok {
			migrated = append(migrated, opt)
		}
	}

	if len(migrated) == 0 {
		return nil, false
	}
	return migrated, true
}

// cameraNotifySettingsAreInert reports whether a camera's notification settings
// are still in the state that means "nobody has touched these", so importing
// over them cannot destroy a choice.
//
// Two conditions, and the second is not redundant. The override being off is
// already enough to make notifyTypes have no effect (settingsFor ignores it),
// but a user can turn the override on, pick types, and turn it off again —
// leaving a real choice sitting behind an inactive switch. Requiring the list
// to also be the untouched default protects that.
//
// "Untouched" has to be tested by VALUE rather than by absence, which is the
// non-obvious part: sdk's AddSchema writes a schema's DefaultValue straight
// into storage the moment it is declared (storage.go), and GetValue resolves
// stored -> schema default -> caller's fallback. Between them there is no
// observable difference between "never set" and "set to the default", for this
// key or any other with a DefaultValue. Comparing against notifyTypeOptions as
// a SET (order-independent) is the closest available proxy.
func cameraNotifySettingsAreInert(s notifySettingsStore) bool {
	if override, ok := s.GetValue(notifyOverrideKey, false).(bool); ok && override {
		return false
	}
	return notifyTypesIsDefault(s)
}

// notifyTypesIsDefault reports whether notifyTypes still holds the full shipped
// option set, compared as a SET so a differently-ordered but equivalent list
// still counts. See cameraNotifySettingsAreInert for why this has to be a value
// comparison rather than a check for absence.
func notifyTypesIsDefault(s notifySettingsStore) bool {
	current := stringSliceValue(s.GetValue(notifyTypesKey, nil))
	if len(current) != len(notifyTypeOptions) {
		return false
	}

	seen := make(map[string]struct{}, len(current))
	for _, c := range current {
		seen[strings.ToLower(strings.TrimSpace(c))] = struct{}{}
	}
	for _, opt := range notifyTypeOptions {
		if _, ok := seen[opt]; !ok {
			return false
		}
	}
	return true
}

// migrateLegacyNotifyBooleans folds the pre-5.7 per-label booleans
// (notifyPerson/notifyVehicle/notifyAnimal/notifyPackage/notifyOther) into
// notifyTypes, reporting whether it wrote anything. scope is "plugin" or a
// camera id, and is only used for logging.
//
// This exists because the fallback in enabledNotifyTypes that was SUPPOSED to
// handle these could never run. It keys off GetValue returning nil for an unset
// notifyTypes, but GetValue resolves stored -> schema default -> caller's
// fallback, and notifyTypes declares a DefaultValue — so it returns the full
// option set and never the nil the fallback tests for. The documented 5.5.0/
// 5.6.0 migration has therefore been dead code since the day it shipped:
// anyone who had switched a label off had it silently switched back on.
//
// Converting it into a real one-time write rather than repairing the fallback
// in place, for the same reason the notifyObjects import is a write: a
// read-time fallback leaves the stored value and the UI disagreeing with the
// behavior, which is precisely what made this invisible for so long. The
// fallback stays where it is — harmless, and still correct for a store whose
// notifyTypes genuinely resolves to nil.
func migrateLegacyNotifyBooleans(scope string, s notifyMigrationStore, logger *sdk.Logger) bool {
	if s == nil {
		return false
	}
	if done, ok := s.GetValue(notifyBooleansMigratedKey, false).(bool); ok && done {
		return false
	}

	// These keys are deliberately never declared as schemas, so unlike
	// notifyTypes their absence really is observable as nil.
	anyStored := false
	for _, key := range append(legacyNotifyBooleanKeys(), notifyOtherKey) {
		if s.GetValue(key, nil) != nil {
			anyStored = true
			break
		}
	}
	if !anyStored {
		return false
	}

	// Someone has since made a choice in the list-shaped setting. It supersedes
	// the booleans by definition — it is the control that replaced them.
	if !notifyTypesIsDefault(s) {
		markNotifyMigrated(notifyBooleansMigratedKey, scope, s, logger)
		return false
	}

	enabled := make([]string, 0, len(notifyTypeOptions))
	for _, opt := range notifyTypeOptions {
		key := notifyOtherKey
		if opt != notifyTypeOther {
			key = notifyObjectLabelKeys[opt]
		}
		if boolValue(s.GetValue(key, true), true) {
			enabled = append(enabled, opt)
		}
	}

	// Every label on is exactly what notifyTypes already says. Writing it would
	// churn storage and, worse, take the key off its default and thereby block
	// the notifyObjects import from ever recognising this scope as untouched.
	if len(enabled) == len(notifyTypeOptions) {
		markNotifyMigrated(notifyBooleansMigratedKey, scope, s, logger)
		return false
	}

	if err := s.SetValue(notifyTypesKey, enabled); err != nil {
		if logger != nil {
			logger.Error("nvr-local: notification migration:", scope, "write notifyTypes failed:", err)
		}
		return false
	}
	markNotifyMigrated(notifyBooleansMigratedKey, scope, s, logger)

	if logger != nil {
		logger.Log(
			"nvr-local: notification migration:", scope,
			"imported the pre-5.7 per-label switches into notifyTypes=["+strings.Join(enabled, ",")+"];",
			"these had been silently ignored since 5.7",
		)
	}
	return true
}

// legacyNotifyBooleanKeys lists the four object-label booleans in
// notifyTypeOptions order. notifyOtherKey is deliberately not included — it is
// not an object label, and every caller appends it explicitly.
func legacyNotifyBooleanKeys() []string {
	keys := make([]string, 0, len(notifyObjectLabelKeys))
	for _, opt := range notifyTypeOptions {
		if opt == notifyTypeOther {
			continue
		}
		keys = append(keys, notifyObjectLabelKeys[opt])
	}
	return keys
}

// markNotifyMigrated records that scope has been through the migration named by
// markerKey. A failure here is logged and otherwise tolerated: the cost is that
// the migration reconsiders this scope on the next start, and by then its
// settings are no longer at their defaults, so it takes the "already
// configured" path and still writes nothing.
func markNotifyMigrated(markerKey, scope string, s notifyMigrationStore, logger *sdk.Logger) {
	if err := s.SetValue(markerKey, true); err != nil && logger != nil {
		logger.Warn("nvr-local: notification migration:", scope, "could not be marked as migrated:", err)
	}
}
