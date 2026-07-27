package recorder

import (
	sdk "github.com/cameraui/sdk/go"
)

// camera.ui core owns recording settings as of its `recording settings move
// from NVR plugin storage to the camera record` migration: the camera record
// carries `recordingSettings` (enabled / mode / preBuffer / sources) and the
// UI edits them there. This plugin's own per-camera keys predate that move,
// so the two drifted — different names, different spellings, and opposite
// defaults for which stream tiers get recorded. The result was a settings
// panel that looked authoritative while changing nothing.
//
// applyCoreRecordingSettings folds core's values onto the locally-stored
// config, which stays the source of truth for the fields core has no home
// for (postRoll, retention) and the fallback for a core too old to send any.

// coreSourceRoles maps core's short stream tiers onto the store role strings
// segments are actually indexed under.
var coreSourceRoles = map[sdk.RecordingSource]string{
	sdk.RecordingSourceHigh: string(sdk.CameraRoleHighRes),
	sdk.RecordingSourceMid:  string(sdk.CameraRoleMidRes),
	sdk.RecordingSourceLow:  string(sdk.CameraRoleLowRes),
}

// coreDefaultRecordingSettings is what core stores for a camera nobody has
// configured: its schema's own defaults (`cameras.schema.ts`'s
// DEFAULT_RECORDING_SETTINGS).
//
// This matters because core's migration off this plugin's storage reads the
// closed upstream plugin's key names (`recordedSources`, `recordingEnabled`,
// `preBuffer`) — none of which this fork ever wrote — so it migrated every
// camera to exactly these defaults. Worse, the one key whose name does match,
// `recordingMode`, has values core doesn't recognise: this plugin's "off" and
// "events" are both outside core's {continuous, event, adhoc}, so both
// collapsed to "continuous".
//
// Adopting that as user intent would start continuous recording on every
// camera deliberately switched off. So a payload identical to these defaults
// is read as "never configured" rather than as a choice.
func coreDefaultRecordingSettings() sdk.CameraRecordingSettings {
	return sdk.CameraRecordingSettings{
		Enabled:   true,
		Mode:      sdk.RecordingModeContinuous,
		PreBuffer: 10,
		Sources: []sdk.RecordingSource{
			sdk.RecordingSourceHigh,
			sdk.RecordingSourceMid,
			sdk.RecordingSourceLow,
		},
	}
}

// isCoreDefault reports whether core's settings are byte-identical to the
// untouched default. The comparison is whole-struct on purpose: any edit in
// core's UI — mode, tiers, pre-buffer, the enable toggle — hands core
// authority over all of it, which is easier to explain than a per-field
// tug-of-war between two config stores.
func isCoreDefault(core sdk.CameraRecordingSettings) bool {
	def := coreDefaultRecordingSettings()
	if core.Enabled != def.Enabled || core.Mode != def.Mode || core.PreBuffer != def.PreBuffer {
		return false
	}
	if len(core.Sources) != len(def.Sources) {
		return false
	}
	for i := range core.Sources {
		if core.Sources[i] != def.Sources[i] {
			return false
		}
	}
	return true
}

// applyCoreRecordingSettings returns local with core's recording settings
// applied, or local unchanged when core sent none — or sent only its own
// untouched defaults (see coreDefaultRecordingSettings).
//
// Absence is detected on Mode: core's schema defaults it to "continuous"
// whenever it has settings for a camera, so an empty Mode means the payload
// predates core owning these (or the field never reached this plugin). That
// distinction matters — a zero-value CameraRecordingSettings has
// Enabled=false, and treating that as "recording off" would silently stop
// every recorder the moment this plugin ran against an older core.
func applyCoreRecordingSettings(local RecordingConfig, core sdk.CameraRecordingSettings) RecordingConfig {
	if core.Mode == "" || isCoreDefault(core) {
		return local
	}

	merged := local
	merged.Mode = coreRecordingMode(core)
	merged.PreRollS = int(core.PreBuffer)

	// A populated Mode with no sources means "record no tiers" — a real
	// choice, not a missing value, so it is not backfilled from local.
	roles := make([]string, 0, len(core.Sources))
	for _, src := range core.Sources {
		if role, ok := coreSourceRoles[src]; ok {
			roles = append(roles, role)
		}
	}
	merged.Roles = roles

	return merged
}

// coreRecordingMode maps core's mode onto this plugin's. Core's "event" is
// this plugin's "events", and core's "adhoc" (record only when started
// manually) has no automatic-recording equivalent here — it resolves to off
// rather than being mistaken for continuous, so an adhoc camera records
// nothing on its own instead of everything.
func coreRecordingMode(core sdk.CameraRecordingSettings) RecordingMode {
	if !core.Enabled {
		return RecordingModeOff
	}

	switch core.Mode {
	case sdk.RecordingModeContinuous:
		return RecordingModeContinuous
	case sdk.RecordingModeEvent:
		return RecordingModeEvents
	case sdk.RecordingModeAdhoc:
		return RecordingModeOff
	default:
		return RecordingModeOff
	}
}
