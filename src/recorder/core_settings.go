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

// applyCoreRecordingSettings returns local with core's recording settings
// applied, or local unchanged when core sent none.
//
// Core is the contract. Whenever it sends a mode, that is the answer — this
// plugin does not second-guess it, and cannot write back to it either
// (CameraDevice.RecordingSettings is read-only, so there is no "sync" to
// perform in the other direction). The plugin's own keys survive for exactly
// one case: a core too old to send anything.
//
// Absence is detected on Mode: core's schema defaults it to "continuous"
// whenever it has settings for a camera, so an empty Mode means the payload
// predates core owning these (or the field never reached this plugin). That
// distinction matters — a zero-value CameraRecordingSettings has
// Enabled=false, and treating that as "recording off" would silently stop
// every recorder the moment this plugin ran against an older core.
//
// UNTIL 5.12.0 there was a second escape hatch here: a core payload
// byte-identical to core's own schema default was read as "never configured"
// and discarded. The reasoning was sound at the time — core's migration off
// this plugin's storage read the CLOSED upstream plugin's key names
// (`recordedSources`, `recordingEnabled`, `preBuffer`), none of which this
// fork ever wrote, so it migrated every camera to exactly those defaults;
// and the one key whose name did match, `recordingMode`, carried this
// plugin's "off"/"events", which are outside core's {continuous, event,
// adhoc} and so collapsed to "continuous". Adopting that wholesale would
// have started recording on cameras deliberately switched off.
//
// It was still wrong, and permanently so: it made "continuous with all three
// tiers" — core's default, and by far the most common configuration there is
// — the one choice a user could not express. Selecting it in the UI produced
// a payload indistinguishable from the migration's, so it lost to whatever
// this plugin had stored. Worse on a fresh install, where the plugin's own
// recordingMode defaults to "off": a brand-new camera reported "continuous"
// in the UI and recorded nothing at all.
//
// The general lesson, which this repo has now hit four times (see
// enabledNotifyTypes, migrateLegacyNotifyBooleans, and keyRoles's own
// history): "the value equals the default" is not a usable proxy for "the
// user never chose it". sdk.DeviceStorage.AddSchema writes a schema's
// DefaultValue straight into the value map, so neither GetValue nor HasValue
// can tell the two apart. The only reliable fix is to stop needing the
// distinction — here, by letting core simply win.
func applyCoreRecordingSettings(local RecordingConfig, core sdk.CameraRecordingSettings) RecordingConfig {
	if core.Mode == "" {
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
