# Changelog

All notable changes to **NVR (Local)** are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

> **Versioning note:** the package version tracks the **5.x line** deliberately — it must stay at or
> above the closed `@camera.ui/camera-ui-nvr` release published to npm, so camera.ui's plugin
> auto-updater never replaces this local build with the license-gated original. The jump from
> `0.1.0` to `5.x` reflects that pin, not 5 major releases of change.

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

[5.3.0]: https://github.com/calebcall/camera-ui-nvr-local/releases/tag/v5.3.0
[5.2.0]: https://github.com/calebcall/camera-ui-nvr-local/releases/tag/v5.2.0
[0.1.0]: https://github.com/calebcall/camera-ui-nvr-local/releases/tag/v0.1.0
