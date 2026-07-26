// Package describe generates AI descriptions of detection events by sending
// frames sampled from the event's recording to an OpenAI-compatible vision
// endpoint, and persists the resulting sdk.EventDescription.
//
// The endpoint is entirely the user's choice: the same request shape works
// against Ollama, LM Studio, vLLM, llama.cpp, OpenAI, and OpenRouter. That
// keeps a fully-local deployment possible (point aiBaseURL at Ollama) while
// defaulting to OpenAI's hosted API, which is what most people can turn on
// without installing anything first. The feature ships DISABLED, because
// enabling it with default settings sends recorded frames from the user's
// cameras to a third party and costs real money on every detection event —
// neither of which anyone should discover by upgrading.
//
// This package deliberately knows nothing about ffmpeg — frames arrive as JPEG
// bytes from media.FrameSampler, keeping every ffmpeg invocation in src/media —
// and nothing about SQLite: it writes through a narrow writer interface. What
// lives here is the LLM contract and nothing else: this configuration layer,
// the prompt, the HTTP client, and the serial work queue that gates and
// rate-limits generation.
package describe

import (
	"errors"
	"strings"
	"time"
)

// Storage keys under this plugin's own (instance-level, not per-camera)
// DeviceStorage. Every one is declared in NVRPlugin.StorageSchema, which the
// settings page renders as a config form, so these strings are the contract
// between that form and this package — they must not change without a
// migration story for anyone who has already configured them.
//
// Instance-level for the same reason nvrQuotaGBStorageKey is (see plugin.go):
// one endpoint, one API key, and one model serve the whole NVR. Per-camera
// keys would mean re-entering an API key for every camera.
const (
	KeyEnabled        = "aiDescriptionsEnabled"
	KeyBaseURL        = "aiBaseURL"
	KeyAPIKey         = "aiAPIKey"
	KeyModel          = "aiModel"
	KeyFrameCount     = "aiFrameCount"
	KeyLabels         = "aiLabels"
	KeyMinConfidence  = "aiMinConfidence"
	KeyTimeoutSeconds = "aiTimeoutSeconds"
	KeyQueueDepth     = "aiQueueDepth"
)

// Defaults for the schema fields and for Load's fallbacks. These two uses must
// agree: the schema's DefaultValue is what a fresh install shows in the
// settings form, and these are what Load falls back to when a key was never
// written. Exported so StorageSchema states each default exactly once, here,
// instead of restating literals that could drift apart.
//
// Note that there is deliberately no DefaultEnabled: the zero value (false) is
// the default, and giving it a name would invite someone to change it.
const (
	// DefaultBaseURL points at OpenAI's hosted API rather than a local
	// runtime, because it is the only endpoint that works without the user
	// installing anything — a default of http://localhost:11434/v1 would fail
	// for everyone who hasn't already got Ollama running.
	DefaultBaseURL = "https://api.openai.com/v1"

	// DefaultModel is OpenAI's cost-sensitive, high-volume tier. Chosen over
	// the balanced tier because this runs once per detection event — a busy
	// multi-camera site can generate hundreds a day — and a default that
	// quietly costs several times more is the wrong default to ship. Users who
	// find descriptions too generic can move up to gpt-5.6-terra.
	DefaultModel = "gpt-5.6-luna"

	// DefaultFrameCount is enough frames to show how a scene developed
	// (someone arriving, then leaving) without multiplying the dominant cost:
	// image tokens scale linearly with frame count.
	DefaultFrameCount = 4

	// DefaultTimeoutSeconds is generous on purpose: Ollama cold-loads a model
	// into VRAM on its first request after a restart, which routinely takes
	// far longer than a warm inference. A tight default would make a local
	// setup look broken on exactly the request the user is watching.
	DefaultTimeoutSeconds = 90

	// DefaultQueueDepth bounds how many events can be waiting on generation
	// at once. Small on purpose: the queue is a burst absorber, not a backlog.
	// Descriptions of events from minutes ago have little value, so shedding
	// work under sustained load beats accumulating it.
	DefaultQueueDepth = 8
)

// Bounds every numeric setting is clamped into, mirroring the Minimum/Maximum
// the storage schema advertises. Clamped rather than rejected because a stored
// value can predate a bound, or be written straight through SetValue and so
// bypass the schema's validation entirely — and a clamped value always beats
// refusing to run.
const (
	minFrameCount  = 1
	maxFrameCount  = 8
	minTimeoutSecs = 10
	maxTimeoutSecs = 600
	minQueueDepth  = 1
	maxQueueDepth  = 64
)

// ConfigGetter is the subset of *sdk.DeviceStorage this package needs, so
// nothing here depends on the whole storage type (and tests can substitute a
// map).
//
// GetValue's fallback is variadic because *sdk.DeviceStorage's method is, and
// matching it exactly is the entire point of this interface — the friendlier
// non-variadic `GetValue(string, any) any` does not satisfy it. config_test.go
// asserts the satisfaction at compile time so a future SDK signature change
// fails in this package's tests rather than at wire-up in plugin.go.
type ConfigGetter interface {
	GetValue(key string, fallback ...any) any
}

// Config is one resolved, already-clamped snapshot of the AI-description
// settings. Every field is safe to use directly: Load has coerced the types,
// pulled the numbers into range, and normalized the strings, so no consumer
// needs to re-check any of it.
type Config struct {
	Enabled       bool
	BaseURL       string
	APIKey        string
	Model         string
	FrameCount    int
	Labels        []string
	MinConfidence float64
	Timeout       time.Duration
}

// Load reads a fresh Config out of g.
//
// Called per event rather than cached at construction, deliberately: the same
// pattern NVRPlugin.nvrQuotaGB uses, and for the same reason — editing a
// setting in the UI must take effect on the next event without restarting the
// plugin, and this feature in particular is one users will tune by watching
// what it produces (turning it off, narrowing the label allow-list, raising the
// confidence floor). Reading nine map entries per event is free next to an
// inference call.
//
// Note that the queue depth is NOT part of Config: the queue's channel is
// sized once at construction, so changing it does need a restart, and folding
// it into a per-event snapshot would falsely imply otherwise. It has its own
// reader, QueueDepth.
func Load(g ConfigGetter) Config {
	enabled, _ := g.GetValue(KeyEnabled, false).(bool)

	// A trailing slash is stripped so joining "/chat/completions" can never
	// produce a double slash, which some servers 404 on. A blank value falls
	// back rather than being kept, so a field the user cleared behaves like one
	// never set instead of producing a Config that Validate rejects.
	baseURL, _ := g.GetValue(KeyBaseURL, DefaultBaseURL).(string)
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}

	model, _ := g.GetValue(KeyModel, DefaultModel).(string)
	model = strings.TrimSpace(model)
	if model == "" {
		model = DefaultModel
	}

	// An empty API key is legitimate, not a misconfiguration: a local Ollama
	// or llama.cpp server needs none. It is still trimmed, because stray
	// whitespace from a copy-paste is invalid in an HTTP header value.
	apiKey, _ := g.GetValue(KeyAPIKey, "").(string)

	rawLabels, _ := g.GetValue(KeyLabels, "").(string)

	return Config{
		Enabled:       enabled,
		BaseURL:       baseURL,
		APIKey:        strings.TrimSpace(apiKey),
		Model:         model,
		FrameCount:    clampInt(coerceInt(g.GetValue(KeyFrameCount, float64(DefaultFrameCount)), DefaultFrameCount), minFrameCount, maxFrameCount),
		Labels:        parseLabels(rawLabels),
		MinConfidence: clampFloat(coerceFloat(g.GetValue(KeyMinConfidence, float64(0)), 0), 0, 1),
		Timeout:       time.Duration(clampInt(coerceInt(g.GetValue(KeyTimeoutSeconds, float64(DefaultTimeoutSeconds)), DefaultTimeoutSeconds), minTimeoutSecs, maxTimeoutSecs)) * time.Second,
	}
}

// QueueDepth reads the work-queue capacity. Separate from Load because it is
// read exactly once, when the queue's channel is created — see Load's doc
// comment for why keeping it out of Config matters.
func QueueDepth(g ConfigGetter) int {
	return clampInt(coerceInt(g.GetValue(KeyQueueDepth, float64(DefaultQueueDepth)), DefaultQueueDepth), minQueueDepth, maxQueueDepth)
}

// Validate reports whether this Config has everything a request needs.
//
// Checked per event before any work is queued, so a half-configured install
// logs one clear reason up front instead of failing deep inside the HTTP
// client with a URL-parse error. Neither field can actually be empty in a
// Config that came from Load, which substitutes defaults for both; this guards
// Configs assembled directly in code.
func (c Config) Validate() error {
	if c.BaseURL == "" {
		return errors.New("describe: " + KeyBaseURL + " is empty")
	}
	if c.Model == "" {
		return errors.New("describe: " + KeyModel + " is empty")
	}
	return nil
}

// AllowsLabels reports whether an event carrying these detection labels passes
// the allow-list — the gate that decides whether an event costs money at all.
//
// An empty allow-list permits everything, which is the default. A non-empty one
// requires at least one label to match, so an event with no labels is rejected:
// "describe everything" and "describe only these" imply opposite answers for an
// event that matches nothing, and the restrictive reading is the safe one when
// the user has explicitly narrowed the list.
//
// Comparison is case-insensitive and ignores surrounding whitespace, because
// these labels come from third-party detector plugins with no shared casing
// convention — an allow-list of "person" must match a detector emitting
// "Person".
func (c Config) AllowsLabels(labels []string) bool {
	if len(c.Labels) == 0 {
		return true
	}
	for _, l := range labels {
		norm := strings.ToLower(strings.TrimSpace(l))
		if norm == "" {
			continue
		}
		for _, allowed := range c.Labels {
			if norm == allowed {
				return true
			}
		}
	}
	return false
}

// parseLabels splits the comma-separated allow-list the settings form collects
// into normalized (lowercased, trimmed) labels, dropping empties so
// "person,,vehicle," behaves sanely. Normalizing here, once per load, is what
// lets AllowsLabels compare like against like without re-normalizing the
// allow-list on every event.
func parseLabels(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if norm := strings.ToLower(strings.TrimSpace(part)); norm != "" {
			out = append(out, norm)
		}
	}
	return out
}

// coerceFloat converts whatever numeric type GetValue hands back into a
// float64, falling back for anything that isn't a number at all.
//
// Same problem, and the same shape of solution, as NVRPlugin.nvrQuotaGB: a
// schema DefaultValue arrives as a float64, but a value that round-tripped
// through msgpack comes back as whichever narrower width msgpack chose on the
// wire. Every integer width is listed rather than just the common ones because
// a missed case is invisible — it silently returns the default and ignores the
// user's setting.
func coerceFloat(v any, fallback float64) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int8:
		return float64(n)
	case int16:
		return float64(n)
	case int32:
		return float64(n)
	case int64:
		return float64(n)
	case uint:
		return float64(n)
	case uint8:
		return float64(n)
	case uint16:
		return float64(n)
	case uint32:
		return float64(n)
	case uint64:
		return float64(n)
	default:
		return fallback
	}
}

// coerceInt is coerceFloat for the settings that are whole numbers. It
// truncates rather than rounds, which is only reachable for a value written
// directly via SetValue (the schema's number fields are integers), and
// truncation is then the conservative choice for a frame count.
func coerceInt(v any, fallback int) int {
	return int(coerceFloat(v, float64(fallback)))
}

// clampInt pulls v into [lo, hi]. See the bounds const block for why every
// numeric setting is clamped rather than rejected.
func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// clampFloat is clampInt for MinConfidence, the one fractional setting.
func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
