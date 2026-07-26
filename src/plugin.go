// RPC dispatch mechanism (findings, task 2)
//
// Source read: github.com/cameraui/rpc/go@v1.0.6/handler.go,
// github.com/cameraui/sdk/go@v1.1.11/run.go, .../storage.go.
//
//  1. Method-name casing is automatic and unconditional. sdk.Run registers the
//     plugin struct itself under the child-RPC namespace:
//
//       cleanupRPC, err = client.RegisterHandler(namespaces.PluginChildRPC, plugin)
//
//     RegisterHandler calls rpc.ExtractMethods(handler), which walks the
//     method set of the handler's type and, for every exported Go method,
//     lowercases just the first rune to produce the wire name:
//
//       wireName := toCamelCase(m.Name) // GetManagedCameraIds -> getManagedCameraIds
//
//     (toCamelCase: "runes[0] = unicode.ToLower(runes[0]); return string(runes)").
//     Each wire name is subscribed as its own NATS subject
//     "rpc.<namespace>.<wireName>", so `GetManagedCameraIds` on this struct is
//     exactly what answers "plugin.<id>.child.rpc.getManagedCameraIds". There
//     is no explicit map and no struct tag involved in this step — the
//     mapping is purely mechanical, off the Go method name.
//
//  2. RPCMethods() is a real, load-bearing allow-list, not just documentation.
//     ExtractMethods checks whether the handler implements:
//
//       type RPCMethodAllowlist interface { RPCMethods() []string }
//
//     If it does, only the *wire names* returned by RPCMethods() are
//     registered as subjects — every other exported method on the struct
//     stays callable in-process (e.g. from other Go code in this plugin) but
//     is never subscribed on the wire, so the frontend/host cannot reach it
//     over RPC. Comment straight from rpc/go's handler.go:
//
//       "RPCMethodAllowlist lets a struct handler restrict which of its
//       exported methods are reachable over RPC. When a handler implements
//       it, only the returned wire names (camelCase, e.g. "getValue") are
//       registered; every other exported method — including RPCMethods
//       itself — stays callable in-process but is not exposed on the wire.
//       Handlers that do not implement it expose all exported methods (the
//       default)."
//
//     sdk.DeviceStorage uses exactly this pattern (storage.go:74-83) to
//     narrow its RPC surface to the config API while keeping lifecycle
//     methods like Save/DefineSchemas Go-callable but wire-invisible. This
//     plugin follows the same convention below: RPCMethods() lists the wire
//     names ("getManagedCameraIds", "getInstanceId", ...), extended as later
//     tasks add RPC-visible methods. Names in the allow-list are the
//     lowercase-first-letter wire names, not the Go method names.
//
// Instance ID (findings, task 2 — revised after review)
//
// There is no public SDK accessor for a plugin's own instance/runtime ID —
// neither on CoreManager (GetFFmpegPath, GetServerAddresses,
// GetCloudServerID, GetPluginsByInterface, ConnectToPlugin — none return the
// caller's own ID) nor on PluginAPI/BasePlugin/Logger (Logger stores an
// unexported pluginID field with no getter) — nor is there any SDK/core
// accessor for the core's own settings.instanceId. This was initially taken
// to mean GetInstanceId should mirror sdk.Run's os.Getenv("PLUGIN_ID"), but
// review of the compiled @camera.ui/nvr frontend showed that's the wrong
// contract: getInstanceId() is polled by the frontend purely as a
// cache-invalidation change-token — when the returned value CHANGES from
// what it last saw, the client flushes its NVR event cache. It is never
// compared to the core's instanceId. PLUGIN_ID is the plugin's constant
// package id (e.g. "@calebcall/camera-ui-nvr-local") and never changes
// across restarts, so it could never drive that flush — it was simply the
// wrong value for what this method is actually for.
//
// GetInstanceId (rpc_recording.go) instead returns a UUID generated once and
// persisted in this plugin's own DeviceStorage (p.store, backed by
// p.Storage in production) under instanceIDStorageKey. That's stable across
// restarts and unique per install, changing only if the plugin's storage is
// wiped — exactly the change-token semantics the frontend's cache consumer
// needs, achieved entirely from this plugin's own state with no core/SDK
// change required.
//
// Correction (second review pass): the first cut of this fix persisted via
// p.store.SetValue(instanceIDStorageKey, id) without ever declaring a schema
// for that key. sdk.DeviceStorage.SetValue (storage.go) silently no-ops when
// no schema exists for the key —
//
//	schema := ds.findSchemaByKey(key)
//	if schema == nil {
//	    ds.mu.Unlock()
//	    return nil
//	}
//
// — so the "persisted" UUID was never actually written, and every call
// regenerated a fresh one (worse than the original PLUGIN_ID bug: the
// frontend's change-token would flip on every poll instead of never).
// NVRPlugin now implements sdk.StorageSchemaProvider (StorageSchema, below)
// to declare a schema for instanceIDStorageKey. Ordering is confirmed safe
// from run.go: the host constructs the plugin, then — *before* registering
// any RPC handler — calls StorageSchema() and DefineSchemas() on the result:
//
//	plugin = constructor(logger, api, pluginStorage)
//	if schemaProvider, ok := plugin.(StorageSchemaProvider); ok {
//	    schemas := schemaProvider.StorageSchema()
//	    if len(schemas) > 0 {
//	        pluginStorage.DefineSchemas(schemas)
//	    }
//	}
//	cleanupRPC, err = client.RegisterHandler(namespaces.PluginChildRPC, plugin)
//
// so the schema is always registered before the first getInstanceId RPC
// call could possibly arrive.
package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"strings"
	"sync"

	sdk "github.com/cameraui/sdk/go"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/describe"
	"github.com/calebcall/plugins/camera-ui-nvr-local/src/media"
	"github.com/calebcall/plugins/camera-ui-nvr-local/src/recorder"
	"github.com/calebcall/plugins/camera-ui-nvr-local/src/store"
)

// NVRPlugin is the minimal boot skeleton for the local NVR hub plugin.
// Recording and playback are implemented in later tasks.
type NVRPlugin struct {
	sdk.BasePlugin

	// recorder tracks which cameras assigned to this Hub-role plugin are
	// actually being recorded, and each one's per-camera recording config.
	// Replaces the Task-2 noRecorders/managedCameraSource stub —
	// GetManagedCameraIds (rpc_recording.go) now delegates to
	// recorder.ManagedCameraIDs(). Populated from the Hub camera lifecycle
	// (ConfigureCameras/OnCameraAdded/OnCameraReleased, below) via the
	// sdkManagedCamera adapter. No ffmpeg/recording process state lives here
	// yet — that's Task 7.
	recorder *recorder.RecorderManager

	// store backs GetInstanceId's persistent UUID. Set to the plugin's real
	// sdk.DeviceStorage (the same value as BasePlugin.Storage) in NewPlugin;
	// tests substitute an in-memory fake.
	store instanceIDStore

	// db is this plugin's embedded SQLite database (store.Open), holding
	// events, segments, faces, and vector tables. Opened against
	// api.StoragePath in NewPlugin; nil in unit tests that construct
	// NVRPlugin directly rather than going through NewPlugin, and left nil
	// in production too if store.Open fails (logged, not fatal — see
	// NewPlugin) since sdk's pluginConstructor signature has no error return
	// for a failure here to propagate through.
	db *store.DB

	// events is the EventStore backing DetectionEvent ingestion
	// (attachDetectionIngestion, events_ingest.go) and, in a later task, the
	// getEvents/getCameraEvents RPC handlers. nil whenever db is nil.
	events *store.EventStore

	// segments is the SegmentStore backing retention garbage collection
	// (Task 9: recorder.RecorderManager.RunRetentionOnce, wired via
	// p.recorder.ConfigureRetention below). nil whenever db is nil.
	segments *store.SegmentStore

	// systemEvents is the SystemEventStore backing the getSystemEvents RPC
	// handler (rpc_events.go). nil whenever db is nil. Nothing in this
	// plugin currently calls SystemEventStore.Insert — see that type's doc
	// comment (src/store/system_events.go) for the producer-side gap this
	// leaves getSystemEvents with today (it can only ever return an empty
	// result until a later task wires a producer).
	systemEvents *store.SystemEventStore

	// thumbs generates and persists each detection event's primary JPEG
	// thumbnail (Task 11, src/media/thumbs.go) — dispatched from
	// attachDetectionIngestion via detectionEventIngester.generateThumbnail
	// on every DetectionEvent lifecycle message. nil whenever db is nil
	// (same guard as events/segments above), and also nil in unit tests
	// that construct NVRPlugin directly rather than through NewPlugin.
	thumbs *media.Generator

	// describer generates AI descriptions of detection events (Feature: AI
	// Descriptions, src/describe) via a user-configured OpenAI-compatible
	// vision endpoint, and persists them through p.events.SetDescription —
	// dispatched from attachDetectionIngestion via
	// detectionEventIngester.describe on every DetectionEvent lifecycle
	// message.
	//
	// Constructed alongside p.thumbs whenever the store opened, and always
	// constructed there even when the feature is switched OFF: the enabled
	// check lives inside DescribeAsync (read per event) so the setting
	// applies without a plugin restart, and an idle Describer costs nothing
	// but an empty channel and one parked goroutine. nil whenever db is nil
	// (same guard as events/segments/thumbs above), and also nil in unit
	// tests that construct NVRPlugin directly rather than through NewPlugin.
	describer *describe.Describer

	// detectionSubs tracks the per-camera sdk.Disposable returned by
	// CameraDevice.OnDetectionEvent so OnCameraReleased can unsubscribe
	// exactly the released camera (see events_ingest.go).
	detectionSubs detectionSubscriptions

	// recorders backs detectionEventIngester's eventRecorderLookup
	// (events_ingest.go), letting DetectionEvent ingestion call MarkEvent on
	// the camera's live recorder when one is registered. Zero value is
	// ready to use (an always-empty registry) — see recorderRegistry's doc
	// comment for why nothing populates it yet.
	recorders recorderRegistry

	// recordingsDir is the resolved base directory NEW recordings
	// (RecorderConfig.DataDir) and the thumbnail Generator are rooted
	// under — resolveRecordingsBaseDir(api.StoragePath, the recordingPath
	// storage value, ...) in NewPlugin (see recording_path.go): the
	// configured recordingPath when non-empty and usable, else
	// api.StoragePath unchanged (the pre-existing default). Also fed to
	// GetStorageStats' diskStats call (rpc_recording.go), since once
	// recordings move here, THIS is the disk whose free/used space
	// actually matters to report — not necessarily api.StoragePath's own
	// filesystem. Empty in unit tests that construct NVRPlugin directly
	// rather than through NewPlugin.
	recordingsDir string

	// scrubber backs NvrScrub/NvrPreviewFrames (rpc_playback.go, Task
	// SCRUB): extracts single Annex-B H.264 keyframes and evenly-spaced
	// filmstrips of them from recorded segments, via a *media.Scrubber
	// built from p.segments and the same resolved ffmpeg every recorder/
	// the thumbnail Generator already exec (see NewPlugin). nil whenever
	// db is nil (same guard as events/segments/thumbs above) and in unit
	// tests that construct NVRPlugin directly rather than through
	// NewPlugin — both handlers treat a nil scrubber as "no data" rather
	// than panicking or erroring.
	scrubber scrubber

	// player backs NvrPlayback (rpc_playback.go, Task PLAYBACK): streams
	// Annex-B H.264 access units from an arbitrary timestamp forward,
	// rolling segment to segment, via a *media.Player built from the same
	// p.segments and resolved ffmpeg as p.scrubber above — streaming
	// playback is scrub's own "look up the covering segment, exec ffmpeg
	// against it" shape, just draining a segment's entire remainder
	// instead of a single keyframe. nil whenever db is nil (same guard as
	// scrubber/events/segments/thumbs above) and in unit tests that
	// construct NVRPlugin directly rather than through NewPlugin —
	// NvrPlayback treats a nil player as "no data" (onNoData) rather than
	// panicking, the same contract p.scrubber == nil gets from NvrScrub.
	player playbackSource

	// playbackSessions is the sessionID -> *playbackSession registry
	// nvrPlaybackCmd (rpc_playback.go) looks up to pause/resume/adjust the
	// speed of a live NvrPlayback session. Zero value is immediately
	// usable — see playback_sessions.go.
	playbackSessions playbackSessionRegistry

	// recordingStateSubs and systemEventSubs back OnRecordingState/
	// OnSystemEvent (rpc_subscriptions.go, Task SUBS): the two callback
	// subscription RPC methods the closed frontend's CameraTimeline calls
	// but this plugin never implemented (hence the browser-side
	// `... is not a function` TypeErrors this task fixes). Both zero
	// values are immediately usable — see subscriptions.go — so no
	// construction is needed here or in NewPlugin. onRecorderStateChange
	// (rpc_subscriptions.go), wired below via
	// p.recorder.SetStateNotifier, is what actually calls emit on these.
	recordingStateSubs recordingStateSubscribers
	systemEventSubs    systemEventSubscribers
}

// Compile-time assertions that NVRPlugin implements the optional SDK
// interfaces it relies on.
var _ sdk.StorageSchemaProvider = (*NVRPlugin)(nil)

// RPCMethods restricts this plugin's RPC surface to the wire names listed
// here (see the casing/allow-list findings above). Extend this list as later
// tasks add RPC-visible methods; every entry must be the camelCase wire name,
// not the Go method name.
func (p *NVRPlugin) RPCMethods() []string {
	return []string{
		"getManagedCameraIds",
		"getInstanceId",
		// Read-path methods (this task): every one of these is backed by an
		// EventStore/SegmentStore/SystemEventStore query or a disk-stats
		// read — no writes, no subscriptions. See rpc_events.go and
		// rpc_recording.go for each handler, and logRPC (below) for the
		// per-call debug logging every one of them starts with.
		"getEvents",
		"getCameraEvents",
		"getRecordingDays",
		"getRecordingSegments",
		"getSystemEvents",
		"getStorageStats",
		"getEventThumbnails",
		"getDetectionHeatmap",
		// Playback frame path, phase 1 (Task SCRUB): a single scrub
		// keyframe and a filmstrip preview — see rpc_playback.go.
		"nvrScrub",
		"nvrPreviewFrames",
		// Streaming playback (Task PLAYBACK): nvrPlayback is a
		// pull-iterator-with-callbacks method — rpc.ExtractMethods
		// detects that shape itself (a *rpc.CallbackInvoker last
		// parameter), same as the func(T)-parameter detection
		// onRecordingState/onSystemEvent rely on below — but, like them,
		// without an entry here it is never registered on the wire at
		// all (RPCMethodAllowlist). nvrPlaybackCmd is an ordinary
		// request/response method with no such auto-detection, so it
		// needs the entry regardless. See rpc_playback.go.
		"nvrPlayback",
		"nvrPlaybackCmd",
		// Callback subscriptions (this task): rpc/go's ExtractMethods
		// detects these as subscriptions by their func(T) parameter, not
		// by anything in this list — but without an entry here they are
		// never registered on the wire at all (RPCMethodAllowlist), which
		// is exactly why the frontend previously saw them as undefined.
		// See rpc_subscriptions.go.
		"onRecordingState",
		"onSystemEvent",
		// sdk.OAuthCapable (Feature #2, oauth.go): rpc/go's ExtractMethods
		// has no OAuthCapable-specific auto-registration at all — see
		// oauth.go's package doc comment for the confirmed evidence from
		// rpc/go@v1.0.6's handler.go — so, like every method above, these
		// three are simply never subscribed on the wire without an entry
		// here, regardless of contract.ts declaring
		// PluginInterface.OAuthCapable.
		"getOAuthMetadata",
		"getOAuthState",
		"disconnect",
	}
}

// logRPC logs one incoming child RPC call at debug level: the wire method
// name plus a short, non-sensitive arg summary (camera ids, event ids, ...),
// NEVER full payloads (thumbnail bytes, detection segments, event lists) —
// so an operator can `tail -f camera.ui.log | grep nvr-local` and see which
// RPC methods the frontend is actually calling, and with what scope,
// without leaking payload contents into the log stream.
//
// This plugin's RPC methods are plain exported Go methods dispatched by
// github.com/cameraui/rpc/go's ExtractMethods/RPCMethodAllowlist (see the
// casing findings at the top of this file) — that package has no
// middleware/interceptor hook a caller can install around every dispatched
// call (confirmed by reading handler.go/service.go/client.go in
// rpc/go@v1.0.6: dispatch goes straight from the NATS subject to
// reflect.Value.Call with no hook point in between), so per-call logging is
// added here instead, as a one-line call at the top of every read-path
// handler (rpc_events.go, rpc_recording.go) — the "small helper each handler
// calls at entry" alternative the task brief anticipated for exactly this
// case.
//
// p.Logger is nil in unit tests that construct NVRPlugin directly (see
// newTestPlugin in plugin_rpc_test.go) rather than through NewPlugin/
// sdk.Run; this guards against that the same way every other p.Logger call
// site in this package already does, because sdk.Logger.Debug would
// nil-pointer-panic on a nil *Logger (it reads l.debugEnabled before
// deciding whether to write — see logger.go).
func (p *NVRPlugin) logRPC(method string, args ...string) {
	if p.Logger == nil {
		return
	}
	if len(args) == 0 {
		p.Logger.Debug("nvr-local: rpc", method)
		return
	}
	p.Logger.Debug("nvr-local: rpc", method, strings.Join(args, " "))
}

// nvrQuotaGBStorageKey is the plugin (instance-level, not per-camera)
// storage key holding the optional whole-NVR disk cap, in gigabytes, that
// retention's disk-cap GC (recorder/retention.go, RunRetentionOnce) enforces
// across every managed camera. Deliberately plugin-level rather than a
// per-camera DeviceStorage key: the frontend contract
// (docs/superpowers/specs/2026-07-19-nvr-frontend-contract.d.ts,
// StorageStats) has exactly one top-level nvrQuotaGB for the whole NVR, not
// one per camera — see retention.go's package doc comment for the full
// reasoning (an earlier version of this plugin stored this per-camera, which
// a review caught as wrong: N cameras could then each consume up to the
// "cap", for N times the intended total).
const nvrQuotaGBStorageKey = "nvrQuotaGB"

// Settings groups. The frontend's CuiSchema renders one TAB per distinct
// non-empty group, in the order the fields are declared here, and renders any
// UNGROUPED field loose underneath the whole tab strip
// (CuiSchema.vue, groupTabs/ungroupedSchemas). Two consequences drive how these
// are used below:
//
//   - Every visible field must carry a group. A single ungrouped field appears
//     stranded below the tabs, which is exactly how the storage settings
//     looked before this existed.
//   - Declaration order is tab order, so storageGroup's fields are emitted
//     first to make Storage the tab that opens by default.
//
// Hidden fields (instanceIDStorageKey) are excluded from the tab strip by
// CuiSchema itself, so they need no group and create no empty tab.
//
// The group titles are declared here, next to the other plugin-level storage
// keys, but the individual AI field keys deliberately are NOT: they live in
// src/describe as describe.Key* because that package both consumes them (to
// read the values back) and owns their defaults. Keeping the key strings and
// the code that reads them in the same package is what stops the form and the
// reader from silently drifting apart — a renamed key here with no
// corresponding change there would compile fine and just never read anything.
const (
	storageGroup = "Storage"

	// detectionsGroup holds everything about which detections this plugin acts
	// on. Today that is the per-type notification toggles (notify_filter.go);
	// face-recognition settings belong here too when they exist, rather than in
	// a tab of their own — faces are a detection concern, and splitting
	// detection settings across two tabs would make neither complete.
	detectionsGroup = "Detections"

	genAIGroup = "GenAI"
)

// aiEnabledCondition hides a field until the AI-descriptions master toggle is
// on, so an unconfigured install sees a single checkbox rather than a dozen
// fields and a button it has no use for.
//
// Returned fresh on each call rather than shared as a package-level variable,
// because JsonSchema.Condition is a slice: a single shared value would give
// every field the same backing array, so anything that mutated one field's
// condition — the frontend's schema round-trip, a future field that appends a
// second condition — would silently rewrite the visibility rule of all of them.
func aiEnabledCondition() []sdk.SchemaCondition {
	return []sdk.SchemaCondition{{
		Key:      describe.KeyEnabled,
		Operator: sdk.SchemaConditionEq,
		Value:    true,
	}}
}

// aiProviderCondition shows a field only when the master toggle is on AND the
// selected provider is the given one. Multiple conditions are ANDed by the
// frontend (see JsonSchema.Condition), and CuiSchema re-evaluates them against
// live form values, so switching the provider dropdown shows and hides these
// immediately with no save or round trip.
//
// This is what makes the base URL field Ollama-only, and what gives each
// provider its own model field carrying its own correct default.
func aiProviderCondition(provider string) []sdk.SchemaCondition {
	return []sdk.SchemaCondition{
		{
			Key:      describe.KeyEnabled,
			Operator: sdk.SchemaConditionEq,
			Value:    true,
		},
		{
			Key:      describe.KeyProvider,
			Operator: sdk.SchemaConditionEq,
			Value:    provider,
		},
	}
}

// StorageSchema declares the plugin-level storage schema. sdk.Run calls this
// (via sdk.StorageSchemaProvider) right after construction and feeds the
// result into DeviceStorage.DefineSchemas — before RPC handlers are
// registered — so every key here is writable via SetValue from the first RPC
// call onward (see the "Correction" note above for why this matters).
//
// instanceIDStorageKey is hidden (internal bookkeeping, not a user-facing
// setting) and stored (Store: true) so GetInstanceId's generated UUID
// actually persists across restarts. nvrQuotaGBStorageKey is user-facing (a
// real setting, not hidden) and stored so it survives restarts and can be
// edited like any other plugin setting; 0 (the default) means uncapped —
// retention only prunes by each camera's own retentionDays.
//
// recordingPathStorageKey (Feature #1: configurable recording storage path)
// is also user-facing and stored: the settings page
// (/settings/recordings, SettingsRecordings.vue) renders this plugin's
// whole StorageSchema as a config form via usePluginStorage, so this is
// literally what makes the field appear there at all. Its DefaultValue is
// deliberately omitted (an empty string default already falls out of
// DeviceStorage's own zero value for an unset string key) — empty is a
// meaningful value here ("use this plugin's default storage location", see
// resolveRecordingsBaseDir in recording_path.go), not a placeholder for
// something else.
//
// Both storage fields carry storageGroup and the AI fields carry genAIGroup,
// which is what makes the frontend render two tabs instead of one tab plus two
// stranded fields — see the groups' own doc comment for why every visible field
// needs one, and why declaration order is tab order.
//
// The remaining fields plus the "Test Connection" button are the AI Descriptions
// settings, all conditional on the master toggle (see aiEnabledCondition), with
// the base URL and the three model fields further conditional on the selected
// provider (aiProviderCondition). Their keys and defaults come from src/describe
// rather than being spelled out here, so this form and the code that reads it
// can never disagree about either. Four things about them are deliberate and
// worth stating, because each is the kind of choice a later edit would casually
// undo:
//
//   - The toggle defaults to OFF. Turning it on with the default provider
//     sends recorded frames of your property to a third party and bills you
//     per event; that is not a decision a plugin gets to make on a user's
//     behalf by shipping it pre-enabled.
//   - There is one model field PER PROVIDER, not one shared field. A JsonSchema
//     field carries a single DefaultValue, so a shared field would keep showing
//     the previous provider's model after a switch — sending an OpenAI model
//     name to Gemini, which fails on every event until someone notices.
//   - Every Description here explains the COST consequence, not just the
//     mechanic. "Frames Per Event" is not self-evidently the difference
//     between $0.003 and $0.012 an event, and a user who discovers that from
//     an invoice rather than from the help text was failed by this form.
//   - The API key uses StringFormatPassword so it renders masked. It is stored
//     in the same plaintext DeviceStorage as everything else — the format only
//     stops it being shoulder-surfed off a settings page, and claiming more
//     than that would be a lie.
func (p *NVRPlugin) StorageSchema() []sdk.JsonSchema {
	storeTrue := true
	return []sdk.JsonSchema{
		{
			Type:   sdk.JsonSchemaTypeString,
			Key:    instanceIDStorageKey,
			Title:  "Instance ID",
			Hidden: true,
			Store:  &storeTrue,
		},
		{
			Type:         sdk.JsonSchemaTypeNumber,
			Key:          nvrQuotaGBStorageKey,
			Title:        "Disk Quota (GB)",
			Description:  "Optional cap on this NVR instance's total recorded storage, across every camera. 0 disables the cap (age-based retention only); once exceeded, the oldest segments across every camera are deleted first.",
			DefaultValue: float64(0),
			Minimum:      sdk.Float64(0),
			Group:        storageGroup,
			Store:        &storeTrue,
		},
		{
			Type:        sdk.JsonSchemaTypeString,
			Key:         recordingPathStorageKey,
			Title:       "Recording Storage Path",
			Description: "Optional custom directory where new recordings (and their thumbnails) are written, e.g. an external drive or network share mounted on this host. Leave empty to use this plugin's default storage location. Changing this only affects NEW recordings — existing recordings stay where they are and remain playable.",
			Group:       storageGroup,
			Store:       &storeTrue,
		},
		// Detections tab: which detection types are worth a notification. All
		// default to ON so that upgrading changes nothing — a user who never
		// opens this tab keeps getting exactly the notifications they got
		// before it existed.
		//
		// These affect NOTIFICATIONS ONLY. Recording, retention, timeline
		// markers, thumbnails, and AI descriptions are unaffected: a detection
		// you don't want to be pinged about is still footage you'll want to
		// review. Each Description says so, because "Animal: off" could
		// otherwise reasonably be read as "stop recording animals".
		//
		// There is deliberately no Motion or Audio toggle — see the storage-key
		// block in notify_filter.go for why one would be a control that does
		// nothing.
		{
			Type:         sdk.JsonSchemaTypeBoolean,
			Key:          notifyPersonKey,
			Title:        "Notify: Person",
			Description:  "Send a notification when a person is detected. Turning this off does not stop recording or detection — the event is still saved, still on the timeline, and still described.",
			DefaultValue: true,
			Group:        detectionsGroup,
			Store:        &storeTrue,
		},
		{
			Type:         sdk.JsonSchemaTypeBoolean,
			Key:          notifyVehicleKey,
			Title:        "Notify: Vehicle",
			Description:  "Send a notification when a vehicle is detected. Often the first one to turn off for a camera facing a road or shared driveway.",
			DefaultValue: true,
			Group:        detectionsGroup,
			Store:        &storeTrue,
		},
		{
			Type:         sdk.JsonSchemaTypeBoolean,
			Key:          notifyAnimalKey,
			Title:        "Notify: Animal",
			Description:  "Send a notification when an animal is detected — pets, wildlife, next door's cat.",
			DefaultValue: true,
			Group:        detectionsGroup,
			Store:        &storeTrue,
		},
		{
			Type:         sdk.JsonSchemaTypeBoolean,
			Key:          notifyPackageKey,
			Title:        "Notify: Package",
			Description:  "Send a notification when a package is detected.",
			DefaultValue: true,
			Group:        detectionsGroup,
			Store:        &storeTrue,
		},
		{
			Type:         sdk.JsonSchemaTypeBoolean,
			Key:          notifyOtherKey,
			Title:        "Notify: Other detections",
			Description:  "Send a notification for detection types outside the standard set — labels produced by a classifier plugin, such as a specific bird or a weather condition. Without this they would be the one kind of detection you could not filter.",
			DefaultValue: true,
			Group:        detectionsGroup,
			Store:        &storeTrue,
		},
		{
			Type:         sdk.JsonSchemaTypeBoolean,
			Key:          describe.KeyEnabled,
			Title:        "Enable AI Descriptions",
			Description:  "Generate a written description of each detection event using a vision model, shown on event cards, in the recordings list, and over the player. OFF by default: with the default provider below this sends recorded frames to OpenAI's paid API and costs roughly $0.003-0.006 per event. Choose the Ollama provider instead to keep everything on your own hardware for free.",
			DefaultValue: false,
			Group:        genAIGroup,
			Store:        &storeTrue,
		},
		{
			Type:         sdk.JsonSchemaTypeString,
			Key:          describe.KeyProvider,
			Title:        "Provider",
			Description:  "Where descriptions are generated. OpenAI and Gemini are hosted, cost money per event, and receive frames of whatever your cameras see. Ollama runs on your own hardware for free and sends nothing off your network. All three speak the same API, so switching is just this dropdown.",
			Enum:         []string{describe.ProviderOpenAI, describe.ProviderOllama, describe.ProviderGemini},
			DefaultValue: describe.DefaultProvider,
			Required:     true,
			Group:        genAIGroup,
			Store:        &storeTrue,
			Condition:    aiEnabledCondition(),
		},
		{
			// Ollama only. The hosted providers each have exactly one correct
			// base URL, which describe.resolveBaseURL supplies as a constant —
			// showing a field for it would only create a way to get it wrong,
			// and a stale value left in it would follow the user to the next
			// provider they picked.
			Type:         sdk.JsonSchemaTypeString,
			Key:          describe.KeyBaseURL,
			Title:        "Ollama Base URL",
			Description:  "Where your Ollama server is reachable, including the /v1 suffix. Any other OpenAI-compatible runtime works here too — LM Studio, vLLM, llama.cpp — so use this provider for those as well.",
			Placeholder:  describe.DefaultOllamaBaseURL,
			DefaultValue: describe.DefaultOllamaBaseURL,
			Required:     true,
			Group:        genAIGroup,
			Store:        &storeTrue,
			Condition:    aiProviderCondition(describe.ProviderOllama),
		},
		{
			Type:        sdk.JsonSchemaTypeString,
			Key:         describe.KeyAPIKey,
			Title:       "API Key",
			Description: "Required for OpenAI and Gemini. Leave empty for a local Ollama, which does not authenticate by default.",
			Format:      sdk.StringFormatPassword,
			Group:       genAIGroup,
			Store:       &storeTrue,
			Condition:   aiEnabledCondition(),
		},
		{
			// One model field per provider, rather than one shared field, so
			// each can carry its own correct DefaultValue. A single field would
			// leave the previous provider's model behind after a switch —
			// sending "gpt-5.6-luna" to Gemini, which 404s on every event.
			Type:         sdk.JsonSchemaTypeString,
			Key:          describe.KeyModelOpenAI,
			Title:        "Model",
			Description:  "A vision-capable OpenAI model. " + describe.DefaultModelOpenAI + " is the cost-efficient high-volume tier; move up to gpt-5.6-terra if descriptions read too generic.",
			Placeholder:  describe.DefaultModelOpenAI,
			DefaultValue: describe.DefaultModelOpenAI,
			Required:     true,
			Group:        genAIGroup,
			Store:        &storeTrue,
			Condition:    aiProviderCondition(describe.ProviderOpenAI),
		},
		{
			Type:         sdk.JsonSchemaTypeString,
			Key:          describe.KeyModelGemini,
			Title:        "Model",
			Description:  "A vision-capable Gemini model. " + describe.DefaultModelGemini + " is the cheapest current tier; gemini-3.5-flash reasons noticeably better on ambiguous scenes for more per event.",
			Placeholder:  describe.DefaultModelGemini,
			DefaultValue: describe.DefaultModelGemini,
			Required:     true,
			Group:        genAIGroup,
			Store:        &storeTrue,
			Condition:    aiProviderCondition(describe.ProviderGemini),
		},
		{
			Type:         sdk.JsonSchemaTypeString,
			Key:          describe.KeyModelOllama,
			Title:        "Model",
			Description:  "A vision-capable model you have already pulled on that server. " + describe.DefaultModelOllama + " is a reasonable starting point. A model you have not pulled returns a clear error from Ollama — use Test Connection to check before waiting on a real event.",
			Placeholder:  describe.DefaultModelOllama,
			DefaultValue: describe.DefaultModelOllama,
			Required:     true,
			Group:        genAIGroup,
			Store:        &storeTrue,
			Condition:    aiProviderCondition(describe.ProviderOllama),
		},
		{
			Type:         sdk.JsonSchemaTypeNumber,
			Key:          describe.KeyFrameCount,
			Title:        "Frames Per Event",
			Description:  "How many frames across the event are sent to the model. More frames describe motion better but cost proportionally more — this is the single biggest cost lever here.",
			DefaultValue: float64(describe.DefaultFrameCount),
			Minimum:      sdk.Float64(1),
			Maximum:      sdk.Float64(8),
			Step:         sdk.Float64(1),
			Group:        genAIGroup,
			Store:        &storeTrue,
			Condition:    aiEnabledCondition(),
		},
		{
			Type:        sdk.JsonSchemaTypeString,
			Key:         describe.KeyLabels,
			Title:       "Only Describe These Labels",
			Description: "Comma-separated detection labels, e.g. \"person,vehicle\". Leave empty to describe every detection event. A real cost control, not a nicety: a camera facing a public road can otherwise generate thousands of paid requests a day.",
			Placeholder: "person,vehicle",
			Group:       genAIGroup,
			Store:       &storeTrue,
			Condition:   aiEnabledCondition(),
		},
		{
			Type:         sdk.JsonSchemaTypeNumber,
			Key:          describe.KeyMinConfidence,
			Title:        "Minimum Confidence",
			Description:  "Skip events whose best detection scores below this. 0 describes everything that passes the label filter.",
			DefaultValue: float64(0),
			Minimum:      sdk.Float64(0),
			Maximum:      sdk.Float64(1),
			Step:         sdk.Float64(0.05),
			Group:        genAIGroup,
			Store:        &storeTrue,
			Condition:    aiEnabledCondition(),
		},
		{
			Type:         sdk.JsonSchemaTypeNumber,
			Key:          describe.KeyTimeoutSeconds,
			Title:        "Timeout (seconds)",
			Description:  "How long one description may take, covering both frame extraction and the model call. The default is deliberately generous because a local model cold-loading into VRAM on its first request is slow.",
			DefaultValue: float64(describe.DefaultTimeoutSeconds),
			Minimum:      sdk.Float64(10),
			Maximum:      sdk.Float64(600),
			Group:        genAIGroup,
			Store:        &storeTrue,
			Condition:    aiEnabledCondition(),
		},
		{
			Type:         sdk.JsonSchemaTypeNumber,
			Key:          describe.KeyQueueDepth,
			Title:        "Queue Depth",
			Description:  "How many pending events wait for description before the oldest is dropped. Descriptions are generated one at a time. Changing this takes effect after a plugin restart, unlike every other setting here.",
			DefaultValue: float64(describe.DefaultQueueDepth),
			Minimum:      sdk.Float64(1),
			Maximum:      sdk.Float64(64),
			Step:         sdk.Float64(1),
			Group:        genAIGroup,
			Store:        &storeTrue,
			Condition:    aiEnabledCondition(),
		},
		{
			Type:        sdk.JsonSchemaTypeSubmit,
			Key:         aiTestConnectionKey,
			Title:       "Test Connection",
			Description: "Sends one tiny image to the endpoint above and reports what came back. Use this to tell a wrong URL, a bad key, a missing model, and a text-only model apart.",
			Color:       sdk.ButtonColorInfo,
			Group:       genAIGroup,
			Condition:   aiEnabledCondition(),
			OnClick:     p.testAIConnection,
		},
	}
}

// aiTestConnectionKey identifies the "Test Connection" submit field. Unlike
// every other key in this group it names an action rather than a setting, so it
// stays here instead of in src/describe (which never reads it) and carries no
// Store: there is no value to persist — pressing the button runs
// testAIConnection and the response is a transient toast.
const aiTestConnectionKey = "aiTestConnection"

// testAIConnection is the OnClick handler behind the "Test Connection" button
// in the AI Descriptions settings group. It sends one tiny generated JPEG
// through exactly the same config load and HTTP client a real event uses, then
// reports the outcome as a toast.
//
// This exists because a bring-your-own-endpoint feature has four completely
// different failure modes that all present to the user as the same symptom —
// "descriptions never appear": a wrong base URL, a rejected API key, a model
// name that doesn't exist on that server, and a model with no vision
// capability. Without this button, diagnosing which one you have means
// correlating plugin logs against events that may take minutes to arrive.
//
// Sending a real image rather than a text-only probe is the load-bearing
// detail: a text-only model answers a plain "hello" request perfectly happily
// and would pass any connectivity check, then fail on every actual event. The
// only way to detect that from a settings page is to make the test request the
// same *shape* as a real one.
//
// The submitted form value is ignored — the button carries no value of its
// own, and the fields it tests were already persisted by the form before
// OnClick fires, so reading them back out of storage (rather than trusting a
// payload) is both simpler and closer to what the real path does.
//
// Errors are returned as toasts, never as Go errors: this is a diagnostic, and
// "the endpoint rejected your key" is a successful diagnosis, not a failure of
// the handler.
func (p *NVRPlugin) testAIConnection(_ any) *sdk.FormSubmitResponse {
	cfg := describe.Load(p.store)
	if err := cfg.Validate(); err != nil {
		return aiToast(sdk.ToastError, err.Error())
	}

	frame, err := probeFrame()
	if err != nil {
		return aiToast(sdk.ToastError, fmt.Sprintf("could not build a test image: %v", err))
	}

	// Bounded by the user's own configured timeout rather than a shorter
	// test-only one, so a slow endpoint that nonetheless works (an Ollama
	// cold-loading a model into VRAM) reports success here exactly as it
	// would in production, instead of failing a test the real path passes.
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	desc, err := describe.NewClient().Complete(ctx, cfg, "This is a connection test. Describe this image in one short sentence.", [][]byte{frame})
	if err != nil {
		// Naming the provider AND the resolved base URL matters here: with the
		// base URL now derived rather than typed, an error mentioning only the
		// model gives no clue whether the request even went where the user
		// thinks it did.
		return aiToast(sdk.ToastError, fmt.Sprintf("%s (%s at %s) failed: %v", cfg.Model, cfg.Provider, cfg.BaseURL, err))
	}

	// Echoing the model's own Title back proves the whole round trip: the
	// server was reachable, the key was accepted, the model exists, it could
	// see the image, and it returned JSON in the shape this plugin parses.
	return aiToast(sdk.ToastSuccess, fmt.Sprintf("%s (%s) responded: %s", cfg.Model, cfg.Provider, desc.Title))
}

// aiToast wraps one message in the FormSubmitResponse shape the settings form
// expects. A trivial helper, but testAIConnection has four return points and
// spelling out three levels of nested struct literal at each of them buried
// the actual messages.
func aiToast(kind sdk.ToastType, message string) *sdk.FormSubmitResponse {
	return &sdk.FormSubmitResponse{Toast: &sdk.ToastMessage{
		Type:    kind,
		Message: message,
	}}
}

// probeFrame builds a small valid JPEG for testAIConnection to send.
//
// Encoded at call time rather than embedded as a base64 constant so there is
// no opaque blob in the source that no reader can verify is really an image
// (or really harmless). Grayscale and 64x64 because the content is irrelevant
// — the test asks whether the endpoint can accept and decode an image at all,
// not what is in it — and a few hundred bytes keeps the probe far cheaper than
// any real event.
func probeFrame() ([]byte, error) {
	img := image.NewGray(image.Rect(0, 0, 64, 64))
	// A gradient rather than a flat fill: a uniform image compresses to almost
	// nothing, and an implausibly tiny payload is exactly the kind of thing a
	// strict endpoint might reject as malformed.
	for i := range img.Pix {
		img.Pix[i] = uint8(i % 256)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// nvrQuotaGB reads the current instance-wide disk cap (see
// nvrQuotaGBStorageKey) from this plugin's own storage, coercing whatever
// numeric type GetValue hands back (float64 from a JSON/schema default,
// or one of the narrower types msgpack may decode onto the wire) into a
// float64. Passed to recorder.RecorderManager.ConfigureRetention as a
// getter (not read once) so an in-place config edit takes effect on the
// next retention pass without a restart.
func (p *NVRPlugin) nvrQuotaGB() float64 {
	switch v := p.store.GetValue(nvrQuotaGBStorageKey, float64(0)).(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int32:
		return float64(v)
	case int64:
		return float64(v)
	default:
		return 0
	}
}

func NewPlugin(logger *sdk.Logger, api *sdk.PluginAPI, storage *sdk.DeviceStorage) sdk.Plugin {
	p := &NVRPlugin{BasePlugin: sdk.NewBasePlugin(logger, api, storage), recorder: recorder.NewRecorderManager(), store: storage}

	// recordingsDir (Feature #1) resolves once, here, before anything below
	// that needs a base directory for recordings/thumbnails is constructed.
	// storage.GetValue works correctly even though sdk.Run defines this
	// plugin's storage schema (DefineSchemas) only AFTER this constructor
	// returns (see the "Correction" note atop this file): DeviceStorage's
	// Values map is already populated from whatever was persisted to disk
	// at newDeviceStorage time, well before schemas ever enter into it —
	// GetValue's schema lookup only matters for its OnGet/DefaultValue
	// fallback, neither of which this read depends on for an
	// already-persisted value. A first-ever run (nothing persisted yet)
	// correctly reads back "" here, which resolveRecordingsBaseDir treats
	// as "use the default", exactly the desired unconfigured behavior.
	configuredRecordingPath, _ := storage.GetValue(recordingPathStorageKey, "").(string)
	p.recordingsDir = resolveRecordingsBaseDir(api.StoragePath, configuredRecordingPath, logger)

	// ff resolves the ffmpeg binary every recorder and the thumbnail
	// Generator below exec: primarily via the SDK's CoreManager.GetFFmpegPath
	// RPC (recorder.ResolveFFmpegSDK — see that function's doc comment for
	// the confirmed production bug this fixes: the core's master-mode
	// runtime hands plugins an explicit env allow-list with neither PATH nor
	// CAMERAUI_FFMPEG_PATH set, so the plain env/PATH resolution this used to
	// call directly never found ffmpeg there at all), falling back to
	// recorder.ResolveFFmpeg's env/PATH-only resolution when api.CoreManager
	// itself is unavailable (nil — never happens via sdk.Run, only a
	// defensive guard for e.g. a future test harness) since
	// ResolveFFmpegSDK's own fallback already covers every other failure
	// (RPC error, empty result).
	var ff *recorder.FFmpeg
	if api.CoreManager != nil {
		ff = recorder.ResolveFFmpegSDK(api.CoreManager, logger)
	} else {
		logger.Warn("nvr-local: no CoreManager available; falling back to env/PATH ffmpeg resolution")
		ff = recorder.ResolveFFmpeg()
	}

	// Open the embedded SQLite database against the host-provided storage
	// directory (api.StoragePath — see plugin_api.go: "absolute path to the
	// plugin's writable storage directory"). A failure here is logged, not
	// fatal: NewPlugin's signature (sdk.pluginConstructor) has no error
	// return, so the alternative would be a panic that takes the whole
	// plugin process down over what later tasks can treat as "events/
	// recording unavailable this run" — p.events stays nil and
	// attachDetectionIngestion/-Released below no-op accordingly.
	db, err := store.Open(api.StoragePath)
	if err != nil {
		logger.Error("nvr-local: open store failed:", err)
	} else {
		p.db = db
		p.events = store.NewEventStore(db)
		p.segments = store.NewSegmentStore(db)
		p.systemEvents = store.NewSystemEventStore(db)
		// Wires RunRetentionOnce/the background ticker (Task 9,
		// recorder/retention.go) with the stores it needs to actually delete
		// anything; p.nvrQuotaGB is the instance-wide disk cap getter (read
		// from this plugin's own storage, not any camera's — see
		// nvrQuotaGBStorageKey's doc comment), and ClipVectors/FaceVectors
		// are where a deleted event's face/clip embedding rows (if any —
		// nothing populates them for events yet) get cascaded to. A no-op
		// call when db failed to open above: p.recorder.ConfigureRetention is
		// simply never reached, so StartRetention below stays a no-op too
		// (see its own doc comment).
		p.recorder.ConfigureRetention(p.segments, p.events, p.recordingsDir, p.nvrQuotaGB, db.ClipVectors, db.FaceVectors)

		// Wires detectionEventIngester's thumbnail generation (Task 11,
		// events_ingest.go's generateThumbnail): p.segments finds each
		// event's covering recorded segment, p.events persists the
		// generated JPEG's path back onto the event row, and ff.Path()
		// (resolved above, once, via the SDK when available) is the same
		// ffmpeg binary every recorder execs — this Generator never resolves
		// its own path (see media/thumbs.go's package doc comment).
		// p.recordingsDir (Feature #1), not api.StoragePath directly, so
		// notification-thumbs/ lands alongside recordings/ under whichever
		// base dir is actually configured.
		p.thumbs = media.NewGenerator(p.recordingsDir, ff.Path(), p.segments, p.events, p.Logger)

		// Wires detectionEventIngester's AI description generation (Feature:
		// AI Descriptions, events_ingest.go's describe): a
		// media.FrameSampler over the same p.segments and resolved ffmpeg
		// binary ff.Path() that p.thumbs above already uses — sampling
		// several frames across an event's window is the same "look up the
		// covering segment, exec ffmpeg against it" shape as generating one
		// thumbnail from it — plus p.events to persist the result
		// (SetDescription, its own column so Upsert can't clobber it),
		// p.recorder to resolve a camera's display name for the prompt
		// (CameraName: "Sideyard" is a location a model can reason about, a
		// UUID isn't), and p.store — this plugin's OWN instance-level
		// DeviceStorage, not any camera's — as the settings source, which
		// describe re-reads per event so a settings edit needs no restart.
		//
		// Constructed unconditionally, even with aiDescriptionsEnabled
		// false: see the describer field's own doc comment.
		p.describer = describe.NewDescriber(
			p.store,
			media.NewFrameSampler(ff.Path(), p.segments, p.Logger),
			p.events,
			p.recorder,
			p.Logger,
		)

		// Wires NvrScrub/NvrPreviewFrames (rpc_playback.go, Task SCRUB):
		// same p.segments (via CoveringSegmentForRole) and resolved
		// ffmpeg binary as p.thumbs above, since scrub/preview frame
		// extraction is the same "look up the covering segment, exec
		// ffmpeg against it" shape as thumbnail generation, just with a
		// different ffmpeg invocation and output format (Annex-B H.264
		// straight off stdout, not a JPEG written to disk).
		p.scrubber = media.NewScrubber(ff.Path(), p.segments, p.Logger)

		// Wires NvrPlayback (rpc_playback.go, Task PLAYBACK): same
		// p.segments and resolved ffmpeg as p.scrubber immediately above
		// — see player's own doc comment for why streaming playback
		// reuses that exact "look up the covering segment, exec ffmpeg"
		// shape.
		p.player = media.NewPlayer(ff.Path(), p.segments, p.Logger)

		// Wires StartAll/Add/Remove (Task ORCH, recorder/manager.go) with
		// what they need to actually build and start a live *recorder.
		// Recorder per managed camera: p.recordingsDir (Feature #1 —
		// api.StoragePath unless a usable recordingPath override was
		// configured; NOT the same directory store.Open opened the SQLite
		// database against, which always stays at api.StoragePath — see
		// recorder.go's outDir, "<DataDir>/recordings/..."), 0 (meaning "use
		// RecorderManager's own default", currently 60s — see
		// defaultSegmentSeconds), and newRecorderFactory's closure, which
		// builds a real *recorder.Recorder against p.segments/ff/p.Logger
		// and wraps it so Start/Stop also keep p.recorders (this plugin's
		// event-mode recorder registry) in sync. Guarded the same way
		// ConfigureRetention is: only reached when db opened successfully,
		// since a real Recorder would otherwise index segments into a nil
		// p.segments.
		p.recorder.ConfigureRecording(p.recordingsDir, 0, p.newRecorderFactory(ff))
		// Wires RecorderManager's own lifecycle logging (recorder
		// started/stopped/restarted, StartAll summaries, start failures —
		// see RecorderManager.SetLogger's doc comment) through this
		// plugin's logger, so operators can actually see recording
		// lifecycle events rather than recording running with no
		// visibility at all.
		p.recorder.SetLogger(p.Logger)
		// Wires OnRecordingState/OnSystemEvent's producer side
		// (rpc_subscriptions.go, Task SUBS): every recorder start/stop
		// startRecorder/stopRecorder (manager.go) reports through
		// notifyState now fans out to this plugin's own subscriber
		// registries via onRecorderStateChange.
		p.recorder.SetStateNotifier(p.onRecorderStateChange)
	}

	api.On(string(sdk.APIEventFinishLaunching), func(...any) {
		p.Logger.Log("nvr-local: finished launching")
		if err := p.recorder.StartRetention(0, nil); err != nil {
			p.Logger.Error("nvr-local: start retention gc failed:", err)
		}
		// StartAll (recorder/manager.go) starts one *recorder.Recorder per
		// camera ConfigureCameras already registered above (mode != off) —
		// safe to call here specifically because the SDK guarantees
		// ConfigureCameras has already returned by the time
		// APIEventFinishLaunching fires (see sdk.APIEventFinishLaunching's
		// own doc comment), so the managed-camera registry is stable before
		// any Recorder is built from it.
		if err := p.recorder.StartAll(); err != nil {
			p.Logger.Error("nvr-local: start recorders failed:", err)
		}
		// StartReconcile (recorder/reconcile.go) runs a periodic pass that
		// re-reads each managed camera's stored recording config and starts/
		// stops recorders to match. This is what makes runtime camera changes
		// take effect WITHOUT a plugin restart: a camera adopted at runtime
		// (OnCameraAdded) whose recordingMode was still the default "off" at
		// add time, then set to "continuous"/"events" in the UI, has no SDK
		// config-changed hook to trigger its recorder — the reconcile pass
		// picks it up on its next tick (also retries a camera whose stream
		// wasn't ready when it was first added).
		p.recorder.StartReconcile(0)
	})
	api.On(string(sdk.APIEventShutdown), func(...any) {
		p.Logger.Log("nvr-local: shutdown")
		// StopAll blocks until every active Recorder's own Stop has
		// returned (each one waits out its supervised ffmpeg
		// goroutines) — deliberately before StopRetention/db.Close, so
		// nothing is still indexing segments into p.segments/p.db by the
		// time the database is closed.
		p.recorder.StopReconcile()
		p.recorder.StopAll()
		p.recorder.StopRetention()
		// Close stops the Describer accepting new work (a detection callback
		// arriving after this point has its event dropped, deliberately and
		// safely — see Describer.Close), lets its worker drain whatever is
		// already queued, and blocks until that worker has exited. Called
		// BEFORE db.Close below for the same reason StopAll is: an in-flight
		// SetDescription must not still be writing into a connection that has
		// already gone away. Bounded by one event's remaining aiTimeoutSeconds
		// plus the queued events behind it.
		if p.describer != nil {
			p.describer.Close()
		}
		if p.db != nil {
			if err := p.db.Close(); err != nil {
				p.Logger.Error("nvr-local: close store failed:", err)
			}
		}
	})

	return p
}

// newRecorderFactory returns a recorder.RecorderFactory that builds a real
// *recorder.Recorder (backed by p.segments/ff/p.Logger) for every
// RecorderConfig RecorderManager asks for, and wraps it in
// recorderHandleWithRegistry so that Recorder's Start/Stop lifecycle also
// keeps p.recorders (the recorderRegistry backing detectionEventIngester's
// MarkEvent lookup, above) in sync — registered while running, deregistered
// once stopped. This is the only place a real *recorder.Recorder is ever
// constructed in this plugin.
func (p *NVRPlugin) newRecorderFactory(ff *recorder.FFmpeg) recorder.RecorderFactory {
	return func(cfg recorder.RecorderConfig) recorder.RecorderHandle {
		rec := recorder.NewRecorder(cfg, p.segments, ff, p.Logger)
		return recorderHandleWithRegistry{rec: rec, cameraID: cfg.CameraID, registry: &p.recorders}
	}
}

// recorderHandleWithRegistry adapts a real *recorder.Recorder to
// recorder.RecorderHandle while also keeping the parent plugin's
// recorderRegistry (p.recorders) in sync with RecorderManager's own
// start/stop lifecycle: Set on a successful Start (so a start failure never
// registers a recorder that isn't actually running), Remove on Stop. Without
// this wrapper, *recorder.Recorder already satisfies RecorderHandle directly
// (Start/Stop match exactly) — this type exists purely for the registry side
// effect, the same reason sdkManagedCamera below exists to bridge a
// return-type mismatch rather than behavior RecorderManager itself needs.
type recorderHandleWithRegistry struct {
	rec      *recorder.Recorder
	cameraID string
	registry *recorderRegistry
}

func (h recorderHandleWithRegistry) Start(ctx context.Context) error {
	if err := h.rec.Start(ctx); err != nil {
		return err
	}
	h.registry.Set(h.cameraID, h.rec)
	return nil
}

func (h recorderHandleWithRegistry) Stop() error {
	h.registry.Remove(h.cameraID)
	return h.rec.Stop()
}

// ActiveOutputDirs forwards to h.rec's own ActiveOutputDirs — satisfying
// recorder.RecorderManager's (unexported) activeOutputDirsProvider
// capability so RunRetentionOnce's orphan sweep (recorder/retention.go)
// learns which directories this camera's real *recorder.Recorder is
// currently writing segments into, the same as if RecorderManager held
// *recorder.Recorder directly instead of this wrapper.
func (h recorderHandleWithRegistry) ActiveOutputDirs() []string {
	return h.rec.ActiveOutputDirs()
}

// ConfigureCameras, OnCameraAdded and OnCameraReleased satisfy sdk.Plugin.
// This is a Hub-role plugin (PluginRoleHub, contract.ts) that "attaches to
// cameras owned by other plugins" — cameras are handed to it here via the
// host's hub assignment, not because it owns them. Recording (ffmpeg,
// segment writing) is still a later task's no-op-for-now scope, but two
// pieces of camera-aware wiring are real as of this task: DetectionEvent
// ingestion (attachDetectionIngestion, unchanged since Task 1) and the
// recorder registry (p.recorder, Task 6) that backs GetManagedCameraIds.
// Every camera handed in is adapted to recorder.ManagedCamera via
// sdkManagedCamera below and registered/unregistered accordingly.
func (p *NVRPlugin) ConfigureCameras(cameras []*sdk.CameraDevice) error {
	managed := make([]recorder.ManagedCamera, 0, len(cameras))
	for _, cam := range cameras {
		p.attachDetectionIngestion(cam)
		managed = append(managed, sdkManagedCamera{dev: cam})
	}
	return p.recorder.Configure(managed)
}

func (p *NVRPlugin) OnCameraAdded(camera *sdk.CameraDevice) error {
	p.attachDetectionIngestion(camera)
	return p.recorder.Add(sdkManagedCamera{dev: camera})
}

func (p *NVRPlugin) OnCameraReleased(cameraID string) error {
	p.detectionSubs.remove(cameraID)
	p.recorders.Remove(cameraID)
	return p.recorder.Remove(cameraID)
}

// recorderRegistry maps camera IDs to the *recorder.Recorder instance
// currently recording them, backing detectionEventIngester's
// eventRecorderLookup (events_ingest.go: RecorderFor). Nothing populates it
// yet in this build: constructing and Start()ing one *recorder.Recorder per
// managed camera — tying RecorderManager's RecorderEntry/RecordingConfig
// (manager.go) to a live ffmpeg process — is explicitly deferred to a later
// task per Task 7's own report ("Wiring one *Recorder per
// continuously-recorded RecorderEntry is left to whichever later task
// orchestrates RecorderManager against real cameras"). This type is that
// later task's extension point: Set/Remove for it to call as recorders
// start/stop, RecorderFor for detectionEventIngester to query on every
// DetectionEvent. OnCameraReleased above already calls Remove so a stale
// entry doesn't outlive its camera once something starts adding them.
//
// *recorder.Recorder satisfies eventRecorder (MarkEvent(eventID string,
// startMs, endMs int64)) directly — no adapter needed, unlike
// sdkManagedCamera above.
type recorderRegistry struct {
	mu   sync.Mutex
	recs map[string]*recorder.Recorder
}

// Set registers rec as cameraID's live recorder, replacing any previous
// entry for the same id.
func (r *recorderRegistry) Set(cameraID string, rec *recorder.Recorder) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.recs == nil {
		r.recs = make(map[string]*recorder.Recorder)
	}
	r.recs[cameraID] = rec
}

// Remove unregisters cameraID's recorder, if any. A no-op for an unknown id.
func (r *recorderRegistry) Remove(cameraID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.recs, cameraID)
}

// RecorderFor implements eventRecorderLookup.
func (r *recorderRegistry) RecorderFor(cameraID string) (eventRecorder, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.recs[cameraID]
	if !ok {
		return nil, false
	}
	return rec, true
}

// sdkManagedCamera adapts a real *sdk.CameraDevice to recorder.ManagedCamera.
// This is the only place a *sdk.CameraDevice is bridged into the recorder
// package — package recorder has no dependency on *sdk.CameraDevice itself
// (see manager.go: sdk.CameraDevice's only constructor, newCameraDeviceProxy,
// is unexported, so recorder's own tests use fakes instead). *sdk.CameraDevice
// doesn't satisfy recorder.ManagedCamera on its own because its Storage()
// method returns the concrete *sdk.DeviceStorage rather than the
// recorder.CameraStorage interface; this adapter's Storage() method bridges
// that return-type mismatch (*sdk.DeviceStorage does implement
// recorder.CameraStorage's method set — GetValue/DefineSchemas — it's just
// not the interface's declared return type until wrapped here).
type sdkManagedCamera struct{ dev *sdk.CameraDevice }

func (c sdkManagedCamera) ID() string   { return c.dev.ID() }
func (c sdkManagedCamera) Name() string { return c.dev.Name() }
func (c sdkManagedCamera) Storage() recorder.CameraStorage {
	return c.dev.Storage()
}

// StreamURL resolves the RTSP/go2rtc URL for one of this camera's configured
// stream sources by role (e.g. sdk.CameraRoleHighRes, "high-resolution" —
// see recorder/manager.go's defaultRoles). *sdk.CameraDevice has no public
// StreamUrl(role) method of its own — only per-role source getters
// (HighResolutionSource, MidResolutionSource, ...) and the generic Sources/
// GetSourceByID, each returning a *CameraDeviceSource whose SourceURL()
// method is the public equivalent of the device's own unexported
// getStreamURL default path (camera_device.go: "Default: return the
// source's default RTSP URL"). Iterating Sources() by Role() rather than
// switching on the known role constants keeps this forward-compatible with
// any role string a camera's config happens to store, without this adapter
// needing to enumerate sdk.CameraRole's cases itself.
func (c sdkManagedCamera) StreamURL(role string) (string, error) {
	for _, src := range c.dev.Sources() {
		if string(src.Role()) == role {
			return src.SourceURL(), nil
		}
	}
	return "", fmt.Errorf("nvr-local: camera %s: no stream source for role %q", c.dev.ID(), role)
}

// SourceRoles enumerates this camera's actual stream source roles, in
// source order (e.g. ["high-resolution", "low-resolution"]) — the
// recorder.ManagedCamera method startRecorder's resolveRoles uses to narrow
// a camera's configured/default recording roles down to ones it actually
// has, and to fall back to its real roles when the configured one (e.g. the
// "high-resolution" default) doesn't exist on this camera's sources. Returns
// nil, not an error, for a camera with no sources at all — resolveRoles
// treats that as "can't narrow, keep the configured/default roles" rather
// than "record nothing".
func (c sdkManagedCamera) SourceRoles() []string {
	sources := c.dev.Sources()
	if len(sources) == 0 {
		return nil
	}
	roles := make([]string, 0, len(sources))
	for _, src := range sources {
		roles = append(roles, string(src.Role()))
	}
	return roles
}

// attachDetectionIngestion subscribes to cam's detection-event stream via
// sdk.CameraDevice.OnDetectionEvent (camera_device.go:547) and upserts every
// event into p.events through a detectionEventIngester (events_ingest.go).
// A no-op if p.events is nil (store.Open failed in NewPlugin — see there).
//
// DEFERRED live-verification: this subscription line itself — cam.
// OnDetectionEvent(ingester.handle) actually firing when a live core
// delivers a detection-event NATS message — is not covered by a unit test.
// sdk.CameraDevice's only constructor, newCameraDeviceProxy
// (camera_device.go), is unexported, so no test outside package sdk can
// build a real *sdk.CameraDevice to subscribe against; doing so needs a
// live core + NATS connection. What IS unit-tested (events_ingest_test.go)
// is detectionEventIngester.handle itself, called directly with a
// synthetic sdk.DetectionEvent and a fake eventUpserter — i.e. everything
// on this side of the OnDetectionEvent callback boundary.
func (p *NVRPlugin) attachDetectionIngestion(cam *sdk.CameraDevice) {
	if p.events == nil {
		return
	}
	// p.thumbs is only ever wrapped into the eventThumbnailer interface
	// when it's actually non-nil: newDetectionEventIngester's nil-checks
	// (i.thumbs == nil) work by comparing the *interface* to nil, which is
	// only true when nothing was ever assigned to it — an interface
	// holding a typed nil *media.Generator would compare != nil and then
	// nil-pointer-panic on the first GenerateAsync call. p.thumbs is set
	// alongside p.events in NewPlugin (both nil together whenever
	// store.Open failed), so this guard is largely defensive, but cheap
	// insurance against that exact Go footgun.
	var thumbs eventThumbnailer
	if p.thumbs != nil {
		thumbs = p.thumbs
	}
	// notifier (FIX C: object-detection push notifications) wraps
	// p.API.NotificationManager — the SDK's Publish(*sdk.Notification) error
	// accessor, confirmed in manager_notification.go — into the
	// eventNotifier interface only when it's actually available: p.API is
	// nil for any test that builds NVRPlugin directly rather than through
	// NewPlugin/sdk.Run (see BasePlugin's own doc comment), and even a real
	// *sdk.PluginAPI's NotificationManager field could in principle be left
	// unset by a future SDK version, so both are guarded the same way
	// thumbs above already is.
	var notifier eventNotifier
	if p.API != nil && p.API.NotificationManager != nil {
		notifier = p.API.NotificationManager
	}
	// p.segments satisfies recordingCoverageChecker directly (CoversRange).
	// Like thumbs above, it's only nil when store.Open failed in NewPlugin —
	// attachDetectionIngestion already returned before this point in that
	// case (the p.events nil guard above), so p.segments is always non-nil
	// here; passed as-is (no interface-nil footgun like thumbs's typed-nil
	// case, since *store.SegmentStore's methods are safe to reach here).
	// p.recorder (never nil — constructed alongside NVRPlugin itself)
	// satisfies cameraNamer directly (recorder.RecorderManager.CameraName),
	// so notify (events_ingest.go) can title a notification with a camera's
	// real display name instead of falling back to its bare ID.
	//
	// p.describer (Feature: AI Descriptions) is wrapped into the
	// eventDescriber interface only when it's actually non-nil, for the
	// identical typed-nil reason documented for thumbs above: describe's
	// nil-check (i.describer == nil) compares the *interface* to nil, and an
	// interface holding a typed nil *describe.Describer would compare != nil
	// and then nil-pointer-panic on the first DescribeAsync call. Set
	// alongside p.thumbs/p.events in NewPlugin (all nil together whenever
	// store.Open failed), so this is defensive in the same cheap way.
	var describer eventDescriber
	if p.describer != nil {
		describer = p.describer
	}
	// The notification filter reads the per-detection-type toggles from this
	// plugin's own storage on every event (see notify_filter.go), so it is
	// constructed here rather than held on NVRPlugin: it owns no state worth
	// sharing, and p.store is available whether or not the database opened.
	// Nothing suppresses notifications until a user turns a type off, since
	// every toggle defaults to true.
	ingester := newDetectionEventIngester(p.events, &p.recorders, thumbs, p.segments, notifier, p.recorder, describer, newNotifyLabelFilter(p.store), p.Logger)
	p.detectionSubs.add(cam.ID(), cam.OnDetectionEvent(ingester.handle))
}
