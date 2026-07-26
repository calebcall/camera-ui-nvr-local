<h1 align="center">NVR (Local)</h1>

<p align="center">
  An open-source, fully-local NVR hub for <a href="https://github.com/seydx/camera.ui">camera.ui</a> —
  records, stores, and plays back your camera video entirely on your own hardware, with no cloud
  dependency and no license check.
</p>

---

## Why this exists

This plugin is a **drop-in, open-source replacement** for that backend. It implements the same
frontend contract, so the existing (unmodified) camera.ui web/mobile UI talks to it directly, but
everything runs locally:

- No license check — it never phones home.
- No cloud dependency for recording, retention, timeline, scrubbing, or playback.
- Paired detection plugins (e.g. an object/face detector that calls an LLM) may still use the
  internet if *they* need it — that's their choice. The NVR itself stays local.
- One feature *can* reach the internet, and it ships off: optional
  [AI event descriptions](#ai-event-descriptions), which send frames to whatever endpoint you point
  them at — a paid API or a model on your own hardware.

## Features

- **Continuous recording** — per-camera H.264 segments written to disk via core-provided ffmpeg.
- **Timeline + read path** — recording days/segments, merged and queryable per camera.
- **Streaming playback & scrub** — frame-accurate Annex-B playback and keyframe scrubbing.
- **Detection events** — object/motion/audio detection events ingested, merged across the event
  lifecycle, and linked to their recordings; thumbnails generated on disk.
- **Configurable storage** — point recordings at any volume (`recordingPath`) and cap total disk
  usage (`nvrQuotaGB`); retention evicts the oldest segments when over the cap.
- **In-app notifications** — publishes object-detection events to the camera.ui notification center.
- **AI event descriptions** *(optional, off by default)* — sends a few frames of a finished event to
  any OpenAI-compatible vision endpoint and stores the title, narrative, and summary it returns,
  which the unmodified camera.ui UI already renders on event cards, in the recordings list, and over
  the player. See [AI event descriptions](#ai-event-descriptions) — **this one can cost money and can
  send video frames off your network.**
- **Local "License & Cloud" panel** — reports a fixed *connected-locally* state so the settings
  page renders normally instead of "Not connected".

### Notifications: what's local and what isn't

This plugin is a notification **publisher** — object-detection events appear in the camera.ui
notification center while the app is connected. It is **not** a device-owning notifier.

True **background push** to the camera.ui mobile app is *not* possible locally: that path runs
through camera.ui's own proprietary FCM/APNs cloud relay, which we neither have credentials for nor
can replicate. If you want real background push without that cloud, the local option is a
self-hosted push service (e.g. [ntfy](https://ntfy.sh) or [Gotify](https://gotify.net/)) delivered
to *that* app — not implemented here.

## Important: the package name

> **The package name in `package.json` is deliberately `@camera.ui/camera-ui-nvr` — the same id as
> the closed plugin.**

The camera.ui frontend does **not** discover the NVR backend by interface. It **hardcodes the
package id `@camera.ui/camera-ui-nvr`** as its RPC target. A differently-named plugin will load and
even record, but the UI will never send it the events/recordings/playback calls. So this plugin must
be **installed into the `@camera.ui/camera-ui-nvr` slot**, replacing the closed backend.

Because that namespace is not ours, **this plugin is not published to npm.** You build it and deploy
it to your server yourself — see below.

## Tech stack

- **Go** (chosen for high-concurrency recording across many cameras).
- **ffmpeg** resolved from the camera.ui core (`CoreManager.GetFFmpegPath()`); no separate binary or
  ffprobe required.
- **SQLite** via the pure-Go, cgo-free [`ncruces/go-sqlite3`](https://github.com/ncruces/go-sqlite3)
  (WASM) driver, with a brute-force cosine vector store for future semantic search.

## Prerequisites

- **Go 1.26+** (to build the plugin binary).
- **Node.js 22+** and the plugin's dev dependencies (`npm install`) to produce the `contract.cjs`
  bundle via the camera.ui CLI.
- A running camera.ui instance you control (the host where the plugin gets installed).

## Build & deploy locally

camera.ui loads the **built artifact**, not source — a `git pull` alone does nothing. You must
build the bundle (`contract.cjs` + the platform binary) and copy it into the install slot.

The install slot is:

```
<camera.ui-install>/plugins/@camera.ui/camera-ui-nvr/
```

and the plugin's recordings/database live under
`<camera.ui-install>/volume/plugins/storage/@camera.ui/camera-ui-nvr/` — reinstalling the code does
not touch them.

### Option A — build natively on the server (preferred)

If Go 1.26+ and Node are available on the host, build there and skip the cross-compile round-trip:

```bash
# 1. Get the latest source
cd ~/path/to/plugins && git pull
cd camera-ui-nvr-local
npm install                 # first time only — pulls the camera.ui CLI bundler

# 2. Build the bundle (produces bundle/{contract.cjs, package.json, dist/bin/plugin})
npm run bundle:dev

# 3. Install into the @camera.ui/camera-ui-nvr slot
D=<camera.ui-install>/plugins/@camera.ui/camera-ui-nvr
rm -rf "$D"/* && cp -a bundle/. "$D/"

# 4. First time only: enable it — remove the "@camera.ui/camera-ui-nvr" line
#    from disabledPlugins in <camera.ui-install>/volume/camera.ui.yaml

# 5. Restart camera.ui
systemctl restart cameraui   # or however your instance is managed
```

### Option B — cross-compile from a workstation

Build a statically-linked Linux binary locally, then ship the bundle to the server:

```bash
cd camera-ui-nvr-local

# 1. Produce contract.cjs (+ package.json) under bundle/
npm install
npm run bundle:dev

# 2. Cross-compile the Linux binary into the bundle's dev path
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -ldflags "-s -w" -o bundle/dist/bin/plugin ./src/
chmod 755 bundle/dist/bin/plugin

# 3. Ship it (strip macOS junk so the server dir stays clean)
COPYFILE_DISABLE=1 tar czf /tmp/nvr-install.tgz -C bundle .
scp /tmp/nvr-install.tgz root@YOUR_SERVER:/tmp/

# 4. On the server: install into the slot, enable (first time), restart
ssh root@YOUR_SERVER '
  D=<camera.ui-install>/plugins/@camera.ui/camera-ui-nvr
  rm -rf "$D"/* && tar xzf /tmp/nvr-install.tgz -C "$D"
  systemctl restart cameraui
'
```

Set `GOARCH=arm64` for ARM hosts. The binary is resolved from the dev path
`<slot>/dist/bin/plugin` before any platform `node_modules` package, so no npm publish is involved.

### Verify it loaded and is being used

Tail the camera.ui log after restart:

```bash
grep -iE "NVR|Spawning Go|nvr-local: rpc" <camera.ui-install>/volume/camera.ui.log | tail
```

You want to see the plugin spawn (`Spawning Go plugin ... dist/bin/plugin`) **and** the frontend
actually routing to it — e.g. `nvr-local: rpc getManagedCameraIds` / `getInstanceId` /
`getCameraEvents`. If you only see it spawn but never any `rpc` lines, the install slot/name is
wrong (see [Important: the package name](#important-the-package-name)).

## Configuration

Configured from the camera.ui **Settings → Recordings** page:

| Setting         | Meaning                                                                                  |
| --------------- | ---------------------------------------------------------------------------------------- |
| `recordingPath` | Directory (ideally on a large/dedicated volume) where recordings are written.            |
| `nvrQuotaGB`    | Instance-wide disk cap. Retention evicts the oldest segments once usage exceeds the cap. |

> **Set a quota.** With no cap, continuous recording of several high-resolution cameras can fill a
> disk in hours. Point `recordingPath` at a roomy volume and set `nvrQuotaGB` to a safe ceiling.

### AI event descriptions

Optional, and **off by default**. When it's on, every finished object-detection event has a few
frames pulled from its own recording and sent to a vision model, which returns a short title, a
chronological narrative, a notification-length summary, and a threat level (0 normal, 1 suspicious,
2 a genuine threat). No frontend changes were needed for any of it — camera.ui already reads
`segments[].description` and renders it on event cards, in the recordings list, and over the player.

Settings live on the same **Settings → Recordings** page, in a collapsible **AI Descriptions** group.
Every field below is hidden until the master toggle is on, and every one of them except the queue
depth takes effect on the *next* event — no restart:

| Setting (storage key)                            | Default                     | Meaning                                                                                                                                                                                                                       |
| ------------------------------------------------ | --------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Enable AI Descriptions (`aiDescriptionsEnabled`) | off                         | Master switch. While it's off nothing is sampled, nothing is sent, and nothing is billed.                                                                                                                                       |
| API Base URL (`aiBaseURL`)                       | `https://api.openai.com/v1` | Any OpenAI-compatible `/chat/completions` endpoint — OpenAI, Ollama, LM Studio, vLLM, llama.cpp, OpenRouter. A trailing slash is stripped for you.                                                                              |
| API Key (`aiAPIKey`)                             | empty                       | Sent as `Authorization: Bearer …`. Leave empty for a local endpoint that doesn't authenticate — the header is then omitted entirely. Rendered masked, but stored in the same plaintext plugin storage as every other setting.    |
| Model (`aiModel`)                                | `gpt-5.6-luna`              | Must be **vision-capable**. `gpt-5.6-terra` reads less generic and costs more; for Ollama use a local vision model such as `qwen2.5vl:7b`.                                                                                      |
| Frames Per Event (`aiFrameCount`)                | 4 (1-8)                     | Frames sampled evenly across the event's time window and sent in one request. The biggest cost lever here.                                                                                                                      |
| Only Describe These Labels (`aiLabels`)          | empty                       | Comma-separated and case-insensitive, e.g. `person,vehicle`. Empty describes every detection event.                                                                                                                             |
| Minimum Confidence (`aiMinConfidence`)           | 0 (0-1)                     | Skips events whose best detection scores below this — the same score the recordings list filters on.                                                                                                                            |
| Timeout (seconds) (`aiTimeoutSeconds`)           | 90 (10-600)                 | One deadline for the whole event, covering frame extraction *and* the model call. 90s is deliberately generous: a local model cold-loading into VRAM on the first request after a restart is slow, and a tight budget would make a working local setup look broken. |
| Queue Depth (`aiQueueDepth`)                     | 8 (1-64)                    | How many events wait for description. Generation is serial — one event at a time — and when the queue is full the **oldest** waiting event is dropped. The only setting here that needs a plugin restart to change.             |

**Test Connection** sends one tiny generated image (a 64x64 JPEG) through the same saved settings,
HTTP client, request shape, and timeout a real event uses, then reports what came back as a toast —
echoing the model's own title on success. That's what makes the four failures that otherwise all look
identical from the outside ("descriptions never appear") tell themselves apart: a wrong base URL, a
rejected API key, a model that doesn't exist on that server, and a model that exists but can't see
images. A text-only model answers a plain text probe perfectly happily, which is why the test sends a
real image.

Two things are never described no matter how the settings are set: events still in progress (only the
event's final message is considered, because a description needs the whole time window), and
motion-only or audio-only events (no detections, so there's nothing worth looking at).

#### Read this before you enable it

**With the default settings, turning this on uploads frames of your property to OpenAI's paid API and
bills you per event.** That's a deliberate exception to this plugin's no-cloud stance — the NVR still
never phones home, and this stays off until you switch it on — but it's your footage and your
invoice, so:

- Rough cost at the default model and 4 frames: **$0.003-0.006 per event**, or about **$20-35/month
  at 200 events a day**. `gpt-5.6-terra` is roughly 2-3x that.
- **Frames Per Event scales cost almost linearly.** Four is a reasonable default; eight roughly
  doubles the bill.
- **Use the label allow-list and the minimum confidence.** They're cost controls, not niceties. A
  camera pointed at a public road can produce thousands of events a day; left unfiltered, that's a
  multi-hundred-dollar monthly surprise, and you'll find out from the invoice.

Treat all of those numbers as estimates. Image tokens dominate the cost of a request and they scale
with resolution, so your actual per-event price depends on your cameras and on how the provider
happens to tile the frames.

> **To keep everything local and free**, run [Ollama](https://ollama.com) with a vision model, set the
> base URL to `http://localhost:11434/v1` and the model to something like `qwen2.5vl:7b`, and leave
> the API key empty. Nothing leaves your network and the only cost is your own GPU time. The plugin
> talks the same OpenAI-compatible protocol either way, so nothing else changes.

Generation is **forward-only**:

- Events that predate enabling the feature are never backfilled. Only events that finish after the
  toggle goes on are described.
- A failed event isn't picked up again later. The HTTP client retries once, after 3 seconds, and only
  for failures that could plausibly clear on their own — a refused connection, a timeout, HTTP 429, an
  HTTP 5xx (the cold-load case above is exactly why that retry exists). Anything else (a rejected key,
  a missing model, a reply that won't parse) fails immediately rather than buying a second inference to
  reach the same answer. Either way the event stays undescribed and the reason is in the camera.ui log.
- A description lands seconds *after* its event, and nothing pushes it to the UI: the description
  shows up the next time the frontend asks for events, so reopen or refresh the camera or recordings
  view if you're watching for it. In-app notifications are published at ingestion, before any
  description exists, so they never carry the AI text.

Every failure in this path is logged and swallowed. A broken endpoint, an expired key, or an
unreachable local model costs you descriptions and nothing else — recording, retention, event
ingestion, thumbnails, and notifications all carry on untouched.

## Development

```bash
go test ./src/...                 # full suite
go test ./src/... -race -count=2  # race detector
```

## License

[MIT](./LICENSE.md).
