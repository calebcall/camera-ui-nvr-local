# Changelog

All notable changes to **NVR (Local)** are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

> **Versioning note:** the package version tracks the **5.x line** deliberately — it must stay at or
> above the closed `@camera.ui/camera-ui-nvr` release published to npm, so camera.ui's plugin
> auto-updater never replaces this local build with the license-gated original. The jump from
> `0.1.0` to `5.x` reflects that pin, not 5 major releases of change.

## [5.8.3] - 2026-07-27

### Fixed

- **AI descriptions are no longer lost to a race with segment finalization.** An event is described
  the moment it ends, but frame sampling resolves footage through the segment index, which only
  contains *finalized* segments — the recorder deliberately skips the file it is still writing. With
  60-second segments the footage covering an event is routinely not indexed until up to a minute
  later, so the first sample came back empty and the event was abandoned with
  `no frames available … nothing to describe`.

  Whether an event got described was therefore luck: it depended on the event's start happening to
  fall inside an already-closed segment. On one live install, 7 of 8 vehicle events over three hours
  got no description; the one that did ran 43 seconds from well inside an earlier segment.

  Sampling is now retried while it comes back empty, spanning a full segment length. Retries stop
  early if the remaining deadline would not leave room for the model call — the single deadline
  covering sampling and inference is deliberate, and waiting until it expires would only turn a
  missing description into a slower one. Footage that never arrives, because it was never recorded or
  has been pruned, still gives up and logs exactly as before, and a sampler *error* is not retried.

## [5.8.2] - 2026-07-27

### Fixed

- **Recording a second stream from a camera no longer crash-loops.** The recorder opened its input
  through the URL camera.ui builds for general streaming, which asks go2rtc for a backchannel
  (`backchannel=opus,pcma,pcmu`). A camera's main stream genuinely has one, so recording it worked. A
  substream does not, and go2rtc answers the request with an extra audio track whose clock disagrees
  with the recorded audio — the mp4 muxer then rejected the packets (`non monotonically increasing
  dts to muxer in stream 1`) and ffmpeg died roughly every 36 seconds, leaving truncated, overlapping
  segments averaging 24s instead of 60s.

  The recorder now strips the backchannel parameter from its input URL for every role. A recorder is
  a one-way capture and never needs one. Audio is preserved in the recordings.

  Isolated on a live install by varying one thing at a time against the same stream: production
  arguments reproduced the failure, the same arguments with the parameter removed ran clean for the
  full test with audio intact. Camera-side audio codec was ruled out — AAC and G.711A fail
  identically, and `pcm_alaw` cannot be muxed into mp4 at all.

## [5.8.1] - 2026-07-27

### Fixed

- **Recording settings edited in camera.ui now take effect without restarting the plugin.** The
  reconcile pass re-read each camera's config correctly but only acted when the on/off state flipped:
  `desired && !active` started a recorder, `!desired && active` stopped one. Changing `continuous` to
  `event` leaves both true, so no branch ran and the recorder kept its original settings indefinitely.
  Adding or removing a recorded stream tier had the same problem.

  The pass now also restarts a running recorder when the resolved config changed in a way it depends
  on — mode, roles, pre-roll or post-roll. A retention-only change still does not restart anything,
  since retention is applied to already-written segments and churning ffmpeg for it would cost more
  than it gains. An unchanged config is untouched, so there is no churn on the 60s tick.

  Config changes picked up by the pass are now logged, which makes it visible whether an edit in
  camera.ui reached the plugin at all.

## [5.8.0] - 2026-07-27

### Fixed

- **The camera's "Recorded streams" setting now actually controls what gets recorded.** camera.ui core
  took ownership of recording settings in its `recording settings move from NVR plugin storage to the
  camera record` migration — the camera record carries `recordingSettings` (enabled / mode / preBuffer
  / sources) and core's settings UI edits them. This plugin never read them, so it kept recording from
  its own stored keys: a settings panel that looked authoritative while changing nothing. A camera
  showing all three qualities selected — core's default — was recording only high-resolution.

  The two had also drifted apart. Core spells the event mode `event` where this plugin spells it
  `events`; core measures the pre-roll as `preBuffer` where this plugin calls it `preRollS` with a
  different default; core lists tiers as `high`/`mid`/`low` where this plugin stores
  `*-resolution`; and core defaults to recording all three tiers where this plugin defaulted to
  high only. Core's `adhoc` mode has no automatic-recording equivalent here and now resolves to off
  rather than being mistaken for continuous.

  `postRoll` and `retentionDays` have no equivalent on the camera record and remain plugin settings.
  When core sends no recording settings at all — an older core — the plugin's own stored values still
  apply, so upgrading cannot silently stop a recorder.

  Core's own migration could not read this fork's key names, so every camera arrived carrying core's
  untouched defaults — and the one key whose name did match, `recordingMode`, has values core does not
  recognise: both `off` and `events` collapsed to `continuous`. Adopting that as intent would have
  started continuous recording on every camera deliberately switched off. A payload identical to
  core's defaults is therefore read as "never configured" and the plugin's own config stands; core
  takes over the moment anything in that panel is actually edited.

  Edits are picked up by the existing reconcile pass (within 60s), no plugin restart required.

  Core's `adhoc` mode ("record only when started manually") resolves to off, since this plugin has no
  manual-start path — see #28. It is deliberately not treated as continuous.

## [5.7.1] - 2026-07-27

### Fixed

- **Playback no longer reports "no recording available" for a stream quality that was never
  recorded.** The recorder writes only the roles a camera is configured for — by default just
  `high-resolution` — while the frontend's quality selector offers every role the camera's *sources*
  advertise. Scrub and playback matched the requested role strictly, so every option except the
  recorded one reported nothing for moments that are fully covered on disk. Verified on a live
  install: a `sourceRole="low"` request returned `segmentFound=false` at a timestamp where a
  `high-resolution` segment spanning it existed.

  The requested role still wins whenever it has footage. Only when it has none does playback fall
  back to whichever role was recorded, preferring high over mid over low. A timestamp no segment
  covers in any role still reports not-found, unchanged — the fallback widens which role is
  acceptable, never which moment is.

  This matters more as cameras advertise more streams: `@calebcall/camera-ui-amcrest` 1.3.0 registers
  every enabled camera stream, so a re-adopted camera now offers three roles where it offered two.

## [5.7.0] - 2026-07-26

### Changed

- **The detection toggles are now one control.** The Detections tab had five separate boolean
  switches, and the frontend draws each as its own bordered row — so the tab was mostly empty space.
  They are replaced by a single **Notify for** multi-select showing the chosen types as chips, both on
  the Detections tab and in the per-camera override panel, which drops from six controls to two.

  Everything is selected by default, so a fresh install notifies exactly as before. An empty selection
  is meaningful and preserved: it means no detection notifications, and is deliberately distinguished
  from "never configured" rather than being papered over with a default.

  Configs written by 5.5.0 or 5.6.0 are migrated on read — when the new key is absent, the five legacy
  booleans are used, each defaulting to on. Those keys are now read-only and no longer appear in any
  schema, so nothing writes to them again.

### Fixed

- **Empty help text no longer leaves a gap.** 5.6.1 removed field descriptions, but the frontend's
  boolean field renders its description element unconditionally — unlike its string field, which
  guards on the description being non-empty. Every toggle was therefore left with an empty hint block
  still taking up height. Collapsing five booleans into one enum removes five of them.

## [5.6.1] - 2026-07-26

### Changed

- **Settings are much less bulky.** Every schema field carried a paragraph of help text, rendered under
  its input; across three tabs plus the per-camera panel that turned the settings pages into a wall of
  prose. Descriptions are gone and titles are shortened now that the tab supplies the context —
  `Notify: Person` is just `Person`, `Enable AI Descriptions` is `Enabled`, `Only Describe These
  Labels` is `Labels`. The README tables are the reference for what each setting does.

  One field keeps a single line: the AI-descriptions master toggle still states that enabling it sends
  frames to the selected provider and costs money unless pointed at Ollama. That is a cost and privacy
  disclosure rather than an explanation of the control, and a test now fails if it is removed.

## [5.6.0] - 2026-07-26

### Added

- **Per-camera notification overrides.** A camera can now have its own notification settings instead
  of following the plugin-wide ones. Open the camera's drawer → Plugins tab → NVR (Local) — where its
  recording mode and retention already live — and turn on **Override notification settings** to reveal
  that camera's own five toggles. While the override is off, the camera inherits the global settings,
  so nothing changes for anyone who does not opt in.

  Overrides **replace** the global settings for that camera rather than narrowing them, so a camera can
  be more permissive as well as stricter: Vehicle can be off globally and on for the one camera
  watching the driveway. ANDing the two was rejected precisely because it would make that impossible.

### Changed

- **Per-camera schema declaration is now additive.** `readRecordingConfig` previously called
  `DeviceStorage.DefineSchemas`, which **replaces** a scope's entire schema list — and it runs on every
  reconcile tick. Since a camera's storage scope is shared by everything this plugin declares for that
  camera, any second set of fields would have survived only until the next tick and then silently
  vanished from the settings form. Declaration now goes through `HasSchema`/`AddSchema`, and
  `recorder.CameraStorage` no longer exposes `DefineSchemas` at all, so the clobbering case is not
  merely avoided but unrepresentable. This was prerequisite work: per-camera AI settings and face
  recognition would have hit the identical wall.

### Fixed

- **Documentation corrected.** The README and issue #16 both claimed the plugin API exposes no
  per-camera settings hook. That was wrong — `CameraDevice.Storage()` provides storage scoped per
  plugin and per camera, the camera drawer's Plugins tab already renders it for hub-role plugins, and
  this plugin has been shipping per-camera recording settings through exactly that mechanism since
  before AI descriptions existed.

## [5.5.0] - 2026-07-26

### Added

- **Per-detection-type notification toggles.** A new **Detections** tab on Settings → Recordings
  controls which detection types are worth a notification: Person, Vehicle, Animal, Package, and a
  catch-all for classifier-produced labels outside the standard set. Every toggle **defaults to on**,
  so upgrading changes nothing until something is turned off — a user who never opens the tab keeps
  receiving exactly the notifications they received before it existed.

  This closes a real gap rather than adding a preference. Notifications were previously
  all-or-nothing: camera.ui's own notification settings offer a master switch, a per-plugin source
  switch, per-system-type switches, and quiet hours, but nothing about detection labels — so someone
  who wanted "tell me about people, not passing cars" had no option short of silencing this plugin
  entirely.

  An event notifies when **any** of its labels is enabled, not all of them. A person arriving in a
  vehicle is a single event carrying both labels, and turning Vehicle off is a request to stop being
  pinged about passing traffic, not to stop hearing that somebody showed up.

  The filter is applied **before** the once-per-event notification latch, so a suppressed event does
  not consume its own notification slot — re-enabling a type mid-event still works, which it would not
  if the order were reversed.

  Scope is deliberately narrow. There is **no Motion or Audio toggle**, because motion-only and
  audio-only events already never notify, making such a switch a control that does nothing. The
  toggles affect **notifications only** — recording, retention, timeline markers, thumbnails, and AI
  descriptions are untouched, since a detection you don't want to be pinged about is still footage you
  want to review. And they are **plugin-wide**, because the plugin API exposes no per-camera settings
  hook; camera.ui automations already do per-camera label filtering properly, and the README points
  there for anyone who needs it.

### Changed

- **The settings tab strip is now Storage / Detections / GenAI.** The new Detections tab is also where
  face-recognition settings belong when those are built, rather than in a tab of their own — faces are
  a detection concern, and splitting detection settings across two tabs would leave neither complete.

## [5.4.0] - 2026-07-26

### Added

- **GenAI provider selection.** The AI-description endpoint is now chosen from a **Provider**
  dropdown — `openai`, `ollama`, or `gemini` — instead of being typed in as a base URL. All three
  speak the same OpenAI-compatible `/chat/completions` API (Gemini via its OpenAI compatibility
  layer), so this adds no provider-specific code: picking a provider resolves an endpoint and a
  default model, and the request path is byte-for-byte the one that already worked.

  The base URL field now appears **only for Ollama**, because only its host and port are genuinely
  site-specific. The two hosted providers each have exactly one correct URL, so a field for them was
  nothing but a way to get it subtly wrong — and a stale value left in it would silently follow the
  user to whichever provider they picked next. Use the Ollama provider for any other
  OpenAI-compatible runtime as well (LM Studio, vLLM, llama.cpp, OpenRouter).

  The model is now stored **per provider** (`aiModelOpenAI`, `aiModelGemini`, `aiModelOllama`) rather
  than in one shared key. A schema field carries exactly one default, so a single shared field kept
  showing the previous provider's model after a switch — sending `gpt-5.6-luna` to Gemini, which
  fails on every event until somebody notices and retypes it. Per-provider keys also mean a
  hand-tuned local model name survives a round trip through the hosted providers.

### Changed

- **Settings are split into tabs.** The plugin's settings on Settings → Recordings now render as a
  **Storage** tab and a **GenAI** tab instead of one tab plus two loose fields. The frontend builds
  the tab strip from each schema field's group and renders any *ungrouped* field stranded beneath it,
  so the recording settings — which never had a group — had been sitting below the AI section they
  have nothing to do with. Storage is declared first and is therefore the tab that opens by default.

  There is deliberately **no Faces tab**: the database tables exist but nothing reads or writes them,
  and a tab advertising a feature that does nothing is worse than no tab. Nor a **License tab** — the
  page already shows license status in its own card directly above these settings.

### Fixed

- **Upgrading from 5.3.0 no longer redirects a local setup to a paid API.** 5.3.0 had no provider
  setting, so a user pointing the base URL at a local Ollama had no way to express that beyond the
  URL itself. Defaulting them to OpenAI would have sent frames of their property to a third party,
  and billed them for it, without them changing a thing. An unset provider with a base URL that
  isn't OpenAI's is now read as Ollama. The pre-provider `aiModel` key is also still honored as a
  fallback, so a model chosen under 5.3.0 is not silently discarded.

## [5.3.0] - 2026-07-26

### Added

- **AI event descriptions (optional, off by default).** Each finished object-detection event can now
  be described by a vision model: frames are sampled across the event's own recording and sent in one
  request to any OpenAI-compatible `/chat/completions` endpoint, and the returned title, narrative,
  summary, and threat level are stored on the event. The camera.ui frontend already reads
  `segments[].description`, so it renders these on event cards, in the recordings list, and over the
  player with **no frontend changes at all**. Configured in a new "AI Descriptions" group on Settings
  → Recordings: endpoint, API key (masked), model, frames per event, a label allow-list, a minimum
  confidence, a per-event timeout, and the work-queue depth — plus a **Test Connection** button that
  sends one tiny image through the real path, so a wrong URL, a rejected key, a missing model, and a
  text-only model stop looking like the same symptom.

  It ships **disabled** because enabling it with the default endpoint sends recorded frames of a
  user's property to a third party and bills them per event; neither is something anyone should
  discover by upgrading. Defaults to OpenAI's `gpt-5.6-luna` at roughly $0.003-0.006 per event —
  point the base URL at a local Ollama and it's free and never leaves the network. The README carries
  the full cost disclosure; read it before switching this on.

  Generation is deliberately conservative: one event at a time behind a shallow queue that sheds the
  *oldest* waiting event under a burst (a description of what just happened is worth more than one
  from ten minutes ago), gated on terminal object-detection events only, and forward-only — nothing
  predating the feature is backfilled and a failed event is never re-attempted, beyond one automatic
  retry for transient endpoint failures. Every failure is logged and swallowed: a broken endpoint
  costs descriptions and cannot affect recording, ingestion, thumbnails, or notifications. The
  default 90-second timeout is generous on purpose — a local model cold-loading into VRAM on its
  first request after a restart is slow, and a tight budget would make a working Ollama setup look
  broken on exactly the request the user is watching.

### Changed

- **Database schema version 3.** The `events` table gained a nullable `description` column, added by
  migration on existing databases (`NULL` is exactly the right state for every pre-existing event).
  Deliberately its own column rather than a field inside the existing `raw` JSON: every event
  lifecycle message rewrites `raw` wholesale, so a description stored there would be silently erased
  by any late or duplicate `end` message that arrived after generation finished.
- **`store.BestConfidence` is now exported** (was `bestConfidence`), so the AI-description confidence
  gate filters on the same ranking that populates the indexed `confidence` column the recordings list
  filters on, instead of a second implementation that could drift from it.

## [5.2.0] - 2026-07-25

### Added

- **Runtime recorder reconcile.** A periodic pass re-reads each managed camera's stored recording
  config and starts/stops recorders to match, so adding, re-adding, or reconfiguring a camera at
  runtime takes effect without a plugin restart (also retries a camera whose stream wasn't ready
  when it was first added).
- **Notification deep link & body.** Object-detection notifications now carry a `DeepLink`
  (`/cameras/<camera-name>?startTs=<ms>`) so a tap opens the right camera at the event time, and a
  body summarizing the detected objects by confidence plus any recognized face/plate
  (e.g. `Person 94%, Vehicle 81% · Caleb`).

### Fixed

- **Blank camera timeline.** Event slices (`triggers`, `segments`, and each segment's
  `detections`/`attributes`) are now serialized as `[]` rather than `null`. The closed frontend
  timeline dereferences these with no null guards, so a `null` (e.g. a motion event's empty
  segments) threw and blanked the entire timeline — no activity markers or coverage.
- **Notification deep link target.** The link now uses the camera's display **name**, not its id —
  the `/cameras/:cameraname` route resolves by name, so a UUID produced a "camera not exists" error.
- **Build tooling.** Reconciled `package-lock.json` so `npm ci` / `cui bundle` work from a clean
  checkout (the copied lockfile referenced non-existent per-platform binary packages).

## [0.1.0] - 2026-07-21

Initial release — an open-source, fully-local, drop-in replacement for the closed-source
`@camera.ui/camera-ui-nvr` backend, which stops working offline due to a license check. Implements
the existing camera.ui frontend contract, so the unmodified web/mobile UI drives it directly.

### Added

- **Recording** — continuous per-camera H.264 segment recording via core-provided ffmpeg, with a
  supervised recorder manager (per-camera locking to prevent handle leaks) and event-aware protected
  windows.
- **Storage & read path** — pure-Go, cgo-free SQLite store for events and segments; queryable
  recording days/segments (merged) per camera; brute-force cosine vector store for future semantic
  search.
- **Playback & scrub** — frame-accurate Annex-B (in-band SPS/PPS) streaming playback and keyframe
  scrubbing/preview-frame extraction; codec string derived from the SPS.
- **Detection events** — full DetectionEvent lifecycle ingestion, with object-detection data merged
  across `start`/`update`/`segment-*`/`end` messages so sparse messages never clobber earlier data;
  events linked to their recordings.
- **Thumbnails** — on-disk thumbnail generation for detection events (home "recent detections",
  camera view, and the recordings list).
- **Configurable storage** — `recordingPath` (write recordings to any volume) and `nvrQuotaGB`
  (instance-wide disk cap) exposed on Settings → Recordings; retention evicts the oldest segments
  when usage exceeds the cap, and sweeps untracked orphan files.
- **Subscriptions** — `onRecordingState` and `onSystemEvent` callback subscriptions.
- **In-app notifications** — publishes object-detection events to the camera.ui notification center
  (`PublishNotifications` capability).
- **Local "License & Cloud" panel** — a fixed, always-connected-locally OAuth state so the settings
  page renders normally instead of "Not connected"; no real authentication flow.

### Fixed

- **Disk retention** — enforce the `nvrQuotaGB` cap reliably: run retention immediately at startup
  and every 10 minutes (previously the cap could be exceeded for long stretches). Added an orphan
  sweep that rechecks the database per-file immediately before deleting and skips any file a running
  recorder is actively writing, so a concurrently-finalized recording can never be deleted.
- **Detection labels** — derive the event's primary label from the highest-score detection (falling
  back to the first non-motion/audio/clip type), so events show `person`/`vehicle`/`animal` instead
  of `clip` or `motion`.
- **Recordings label filter** — filtering the recordings list by a label (e.g. "person") now returns
  the matching object events. The frontend sends `hasDetections:false` alongside an explicit
  `types:[...]` chip to mean "no detections-only constraint"; treating it as strict equality
  previously excluded every object event whenever a chip was selected.
- **`hasDetections` semantics** — the detections-only view keys off event *types* (an object type is
  present) rather than segment presence, which the core delivers empty.

### Changed

- **Notifications** — the plugin is a pure notification *publisher*; the unimplemented `Notifier`
  interface was removed from the contract. Declaring it without implementing the device methods
  caused repeated `getDevices` "no responders" warnings on the host and broke the mobile app's device
  registration. Background push to the camera.ui app requires camera.ui's proprietary cloud relay and
  is intentionally out of scope for a local plugin.

[5.7.0]: https://github.com/calebcall/camera-ui-nvr-local/releases/tag/v5.7.0
[5.6.1]: https://github.com/calebcall/camera-ui-nvr-local/releases/tag/v5.6.1
[5.6.0]: https://github.com/calebcall/camera-ui-nvr-local/releases/tag/v5.6.0
[5.5.0]: https://github.com/calebcall/camera-ui-nvr-local/releases/tag/v5.5.0
[5.4.0]: https://github.com/calebcall/camera-ui-nvr-local/releases/tag/v5.4.0
[5.3.0]: https://github.com/calebcall/camera-ui-nvr-local/releases/tag/v5.3.0
[5.2.0]: https://github.com/calebcall/camera-ui-nvr-local/releases/tag/v5.2.0
[0.1.0]: https://github.com/calebcall/camera-ui-nvr-local/releases/tag/v0.1.0
