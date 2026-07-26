package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sdk "github.com/cameraui/sdk/go"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/describe"
	"github.com/calebcall/plugins/camera-ui-nvr-local/src/store"
)

// AI Descriptions: the end-to-end proof of the epic's acceptance criterion —
// "an enabled install produces a title/narrative/summary/threat level on new
// object-detection events", surfaced to the frontend with no frontend changes,
// which in Go terms means the description comes back out of EventStore.Query
// attached to Segments[0].Description.
//
// Every task in the feature has thorough unit tests, and every one of them
// substitutes the component on the other side of a seam: describe's tests stub
// the completer, the store's tests call SetDescription directly, and
// events_ingest_test.go's describe tests assert against a spy describer. That
// is the right shape for unit tests, and it is exactly why none of them can
// catch a seam being wired up wrong. This file wires the REAL components
// together — a real EventStore on a real temp SQLite database, a real Describer
// driving a real Client against an httptest.Server, and the real
// detectionEventIngester driven through a realistic lifecycle — so the five
// joints below are actually exercised end to end:
//
//  1. MERGE -> GATING. detectionEventIngester.describe hands the DescribeAsync
//     gate `merged`, not the raw wire message. The terminal 'end' message a real
//     producer sends is sparse (Segments:[], no Types), so a describer handed
//     the raw message would be rejected by its own has-detections check and no
//     event would ever be described. The lifecycle below is deliberately shaped
//     that way, and the settings deliberately turn on the label allow-list and
//     the confidence floor as well, so the accumulated detections have to
//     survive all three gates for this test to pass.
//  2. GATING -> HTTP. Whether the feature is enabled decides whether an HTTP
//     request happens at all. The disabled case asserts on the server's own
//     request count, which is the only way to prove "inert" rather than
//     "described and then discarded".
//  3. HTTP CONTRACT. The request the real Client puts on the wire has to be one
//     a real OpenAI-compatible endpoint accepts (POST /chat/completions, the
//     configured model, the context text plus base64 JPEG data URIs), and the
//     realistic reply below has to parse back into an sdk.EventDescription.
//     Both halves are the Client's, not a stub's.
//  4. SEPARATE description COLUMN. The description is written by SetDescription
//     into its own column, while ingestion has already rewritten `raw`
//     wholesale several times. Anything that folded the description into `raw`
//     would pass describe's unit tests and lose the description here.
//  5. QUERY-TIME MERGE. attachDescription has to hang the stored JSON back onto
//     Segments[0] of the decoded event, because that is where every frontend
//     consumer looks for it.
//
// No sleeps and no polling: Describer.Close drains the queue and waits for the
// worker, so it is the synchronization point for the whole async path.

// aiDescriptionChatServer is a stand-in for the user's OpenAI-compatible vision
// endpoint. It records every request it receives — the count is the assertion
// the disabled case turns on — and answers with a realistically-shaped
// chat-completions response whose message content is the JSON object the prompt
// asks for.
type aiDescriptionChatServer struct {
	*httptest.Server

	// reply is what every request is answered with, as an EventDescription;
	// it is marshalled into the assistant message's content string.
	reply sdk.EventDescription

	mu       sync.Mutex
	requests []aiDescriptionChatRequest
}

// aiDescriptionChatRequest is the subset of the chat-completions request body
// this test asserts on. Declared here rather than reusing describe's own wire
// types, which are unexported: decoding the payload from the outside is the
// point — it proves the bytes on the wire are what an endpoint would receive,
// not merely that a struct round-trips through itself.
type aiDescriptionChatRequest struct {
	Path     string
	Method   string
	Model    string                   `json:"model"`
	Messages []aiDescriptionChatMsg   `json:"messages"`
	Format   *aiDescriptionChatFormat `json:"response_format"`
}

type aiDescriptionChatMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type aiDescriptionChatFormat struct {
	Type string `json:"type"`
}

type aiDescriptionContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

// userParts decodes the parts array of the request's user message — the text
// context followed by one image_url part per frame.
func (r aiDescriptionChatRequest) userParts(t *testing.T) []aiDescriptionContentPart {
	t.Helper()
	for _, msg := range r.Messages {
		if msg.Role != "user" {
			continue
		}
		var parts []aiDescriptionContentPart
		if err := json.Unmarshal(msg.Content, &parts); err != nil {
			t.Fatalf("decode user message content parts: %v", err)
		}
		return parts
	}
	t.Fatal("request carried no user message")
	return nil
}

func newAIDescriptionChatServer(t *testing.T, reply sdk.EventDescription) *aiDescriptionChatServer {
	t.Helper()
	s := &aiDescriptionChatServer{reply: reply}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body aiDescriptionChatRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return
		}
		body.Path = r.URL.Path
		body.Method = r.Method

		s.mu.Lock()
		s.requests = append(s.requests, body)
		s.mu.Unlock()

		// The model answers with the description as a JSON string inside the
		// message content — the double-encoding a real endpoint does, and the
		// layer parseDescription has to peel back off.
		content, err := json.Marshal(s.reply)
		if err != nil {
			http.Error(w, "marshal reply", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-nvr-integration",
			"object":  "chat.completion",
			"created": 1_700_000_000,
			"model":   body.Model,
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": string(content)},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 1731, "completion_tokens": 92, "total_tokens": 1823},
		})
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *aiDescriptionChatServer) received() []aiDescriptionChatRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]aiDescriptionChatRequest(nil), s.requests...)
}

// stubFrameSampler stands in for media.FrameSampler, returning fixed JPEG-ish
// bytes and recording the arguments it was called with.
//
// Deliberately NOT a real FrameSampler: proving ffmpeg actually produces frames
// is media/frames_test.go's job (it runs a real ffmpeg against a real fixture
// segment), and repeating it here would make this test slow, environment-
// dependent, and skipped on any box without ffmpeg — while adding nothing about
// the seams above. The bytes start with the JPEG SOI marker so that what reaches
// the wire is recognisably an image rather than an opaque blob.
type stubFrameSampler struct {
	frames [][]byte

	mu    sync.Mutex
	calls []stubFrameSampleCall
}

type stubFrameSampleCall struct {
	cameraID string
	startMs  int64
	endMs    int64
	n        int
}

func (s *stubFrameSampler) SampleFrames(ctx context.Context, cameraID string, startMs, endMs int64, n int) ([][]byte, error) {
	s.mu.Lock()
	s.calls = append(s.calls, stubFrameSampleCall{cameraID: cameraID, startMs: startMs, endMs: endMs, n: n})
	s.mu.Unlock()

	if n < len(s.frames) {
		return s.frames[:n], nil
	}
	return s.frames, nil
}

func (s *stubFrameSampler) sampleCalls() []stubFrameSampleCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stubFrameSampleCall(nil), s.calls...)
}

// aiDescriptionRig is the production object graph from plugin.go's
// initialization (see the p.describer wiring there), with exactly two
// substitutions: the frame sampler (see stubFrameSampler) and the endpoint
// (an httptest.Server, since a test may not reach the network). Everything
// else — the ingester, the accumulator inside it, the Describer, its queue and
// worker, the real Client, the real EventStore, a real SQLite file — is the
// shipping code.
type aiDescriptionRig struct {
	events    *store.EventStore
	settings  *fakeInstanceStore
	sampler   *stubFrameSampler
	server    *aiDescriptionChatServer
	describer *describe.Describer
	ingester  *detectionEventIngester
}

// newAIDescriptionRig builds that graph. settings is merged over a working
// baseline (feature on, endpoint pointed at the httptest server) so each test
// states only what it is varying — the disabled case flips one key.
func newAIDescriptionRig(t *testing.T, reply sdk.EventDescription, settings map[string]any) *aiDescriptionRig {
	t.Helper()

	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	// Registered before the Describer's cleanup below so it runs AFTER it
	// (t.Cleanup is LIFO): the worker must be finished before the connection it
	// writes descriptions through goes away, which is the same ordering
	// plugin.go's shutdown handler guarantees.
	t.Cleanup(func() { db.Close() })

	events := store.NewEventStore(db)
	server := newAIDescriptionChatServer(t, reply)

	// The plugin's own instance-level DeviceStorage is the settings source in
	// production (p.store); fakeInstanceStore is its test double and already
	// mirrors GetValue's variadic-fallback contract, so it satisfies
	// describe.ConfigGetter directly with no adapter.
	cfg := newFakeInstanceStore()
	baseline := map[string]any{
		describe.KeyEnabled: true,
		describe.KeyBaseURL: server.URL,
		describe.KeyModel:   "qwen2.5vl:7b",
		describe.KeyAPIKey:  "sk-integration",
		// Two frames, matching what the stub sampler can supply.
		describe.KeyFrameCount: float64(2),
		// The allow-list and the confidence floor are ON deliberately: both read
		// the event's DETECTIONS, which only exist on the merged event, so they
		// double as assertions that merge accumulation reached the gate.
		describe.KeyLabels:        "person,vehicle",
		describe.KeyMinConfidence: float64(0.5),
	}
	for k, v := range settings {
		baseline[k] = v
	}
	for k, v := range baseline {
		// Schema-declared first: fakeInstanceStore mirrors DeviceStorage's rule
		// that SetValue on an undeclared key is a silent no-op.
		cfg.declareSchema(k)
		if err := cfg.SetValue(k, v); err != nil {
			t.Fatalf("SetValue(%s): %v", k, err)
		}
	}

	sampler := &stubFrameSampler{frames: [][]byte{
		{0xFF, 0xD8, 0xFF, 0xE0, 'f', 'r', 'a', 'm', 'e', '1'},
		{0xFF, 0xD8, 0xFF, 0xE0, 'f', 'r', 'a', 'm', 'e', '2'},
	}}

	describer := describe.NewDescriber(cfg, sampler, events, &fakeCameraNames{names: map[string]string{"cam-1": "Front Door"}}, nil)
	t.Cleanup(describer.Close) // idempotent; the tests call it themselves as their sync point

	return &aiDescriptionRig{
		events:    events,
		settings:  cfg,
		sampler:   sampler,
		server:    server,
		describer: describer,
		// Only the store and the describer are wired: recorders, thumbs,
		// coverage, and the notifier are the other tasks' concerns and each has
		// its own integration test. nil is their documented "not wired" state.
		ingester: newDetectionEventIngester(events, nil, nil, nil, nil, nil, describer, nil, nil),
	}
}

// ingestPersonEventLifecycle drives one realistic person-detection event
// through the ingester, message by message, exactly as the core delivers them.
//
// The shape is the whole point:
//
//   - 'start' opens the event with Types:["motion"] and no segments, which is
//     all the producer knows yet.
//   - 'segment-start' and 'segment-update' carry the actual detections, the
//     recognized face, and the zone — the only messages that ever do.
//   - the terminal 'end' message is SPARSE: Segments:[], Types:nil, no
//     triggers, just the final EndTime and State. This is not a contrived edge
//     case, it is what real producers send (see events_ingest_merge.go), and
//     it is what makes the accumulator load-bearing: a describer handed this
//     message raw sees an event with no detections, no labels, and confidence
//     0, and every gate in DescribeAsync rejects it.
func ingestPersonEventLifecycle(ing *detectionEventIngester) {
	const (
		eventID  = "evt-ai-1"
		cameraID = "cam-1"
		startMs  = int64(1_700_000_000_000)
		endMs    = startMs + 12_000
	)

	ing.handle(sdk.DetectionEventStart, sdk.DetectionEvent{
		ID:         eventID,
		CameraID:   cameraID,
		State:      sdk.DetectionEventStateActive,
		StartTime:  startMs,
		LastUpdate: startMs,
		Types:      []string{"motion"},
		Triggers: []sdk.EventTrigger{
			{Type: sdk.EventTriggerMotion, Score: 0.41, FirstSeen: startMs, LastSeen: startMs},
		},
	})

	ing.handle(sdk.DetectionEventSegmentStart, sdk.DetectionEvent{
		ID:         eventID,
		CameraID:   cameraID,
		State:      sdk.DetectionEventStateActive,
		StartTime:  startMs,
		LastUpdate: startMs + 3_000,
		Types:      []string{"person"},
		Segments: []sdk.EventSegment{{
			FirstSeen:  startMs,
			LastSeen:   startMs + 3_000,
			Zones:      []string{"driveway"},
			Detections: []sdk.EventDetection{{Label: "person", Score: 0.78}},
		}},
	})

	ing.handle(sdk.DetectionEventSegmentUpdate, sdk.DetectionEvent{
		ID:         eventID,
		CameraID:   cameraID,
		State:      sdk.DetectionEventStateActive,
		StartTime:  startMs,
		LastUpdate: startMs + 9_000,
		Types:      []string{"person", "face"},
		Segments: []sdk.EventSegment{{
			FirstSeen:  startMs,
			LastSeen:   startMs + 9_000,
			Zones:      []string{"porch"},
			Detections: []sdk.EventDetection{{Label: "person", Score: 0.93}},
			Attributes: []sdk.EventAttribute{{Type: "face", Label: "Caleb", Confidence: 0.88}},
		}},
	})

	// The sparse terminal message. Everything the description path needs has to
	// come from the accumulator, not from here.
	ing.handle(sdk.DetectionEventEnd, sdk.DetectionEvent{
		ID:         eventID,
		CameraID:   cameraID,
		State:      sdk.DetectionEventStateEnded,
		StartTime:  startMs,
		LastUpdate: endMs,
		EndTime:    endMs,
	})
}

// TestIngestion_AIDescriptionsEnabled_QueryReturnsDescriptionOnSegmentsZero is
// the epic's acceptance criterion, proved through the real components: ingest a
// person event whose terminal message is sparse, let the Describer drain, and
// EventStore.Query hands the event back with the model's
// title/description/summary/threat level hanging off Segments[0].Description —
// the exact field CameraEvent.vue, RecordingCard.vue, and
// CameraStreamEvent.vue read, which is what "no frontend changes" means.
//
// It also asserts on the request the real Client actually sent, because a
// description that arrives having been generated from the wrong material (a
// prompt with no detections, or no images) is a subtler failure than no
// description at all: it looks like the feature works.
func TestIngestion_AIDescriptionsEnabled_QueryReturnsDescriptionOnSegmentsZero(t *testing.T) {
	want := sdk.EventDescription{
		Title:       "Person approaches the front door",
		Description: "A person walks up the driveway, pauses at the front door, and waits before stepping out of frame.",
		Summary:     "Someone approached the front door and waited briefly.",
		ThreatLevel: 1,
	}
	rig := newAIDescriptionRig(t, want, nil)

	ingestPersonEventLifecycle(rig.ingester)

	// The synchronization point for the entire async path: Close stops accepting
	// work, lets the worker finish what is queued, and waits for it to exit. By
	// the time it returns, SetDescription has either run or failed — no sleeps,
	// no polling, nothing to flake.
	rig.describer.Close()

	// --- The description is served back out of the store ------------------

	got, err := rig.events.Query([]string{"cam-1"}, GetEventsOptions{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(got.Events))
	}
	ev := got.Events[0]
	if len(ev.Segments) == 0 {
		t.Fatalf("event has no segments, so there is nowhere for a description to hang: %+v", ev)
	}
	desc := ev.Segments[0].Description
	if desc == nil {
		t.Fatalf("Segments[0].Description is nil; the description never made it from the model back out of Query (event: %+v)", ev)
	}
	if *desc != want {
		t.Errorf("Segments[0].Description = %+v, want %+v", *desc, want)
	}

	// The event's own accumulated data must be intact alongside it: ingestion
	// rewrote `raw` on every one of the four messages, and SetDescription wrote
	// the description afterwards into its own column. Both surviving together is
	// the point of that column.
	if got, want := store.PrimaryLabel(ev), "person"; got != want {
		t.Errorf("PrimaryLabel = %q, want %q — the merged detections were lost", got, want)
	}
	if got := store.BestConfidence(ev); got != 0.93 {
		t.Errorf("BestConfidence = %v, want 0.93 — the sparse terminal message clobbered the merged detections", got)
	}

	// --- The frames were sampled over the event's full window -------------

	calls := rig.sampler.sampleCalls()
	if len(calls) != 1 {
		t.Fatalf("frame sampler called %d times, want exactly 1 (one description per event)", len(calls))
	}
	if calls[0].cameraID != "cam-1" || calls[0].startMs != 1_700_000_000_000 || calls[0].endMs != 1_700_000_012_000 {
		t.Errorf("SampleFrames(%q, %d, %d, _), want (cam-1, 1700000000000, 1700000012000, _)", calls[0].cameraID, calls[0].startMs, calls[0].endMs)
	}
	if calls[0].n != 2 {
		t.Errorf("SampleFrames n = %d, want 2 (the configured aiFrameCount)", calls[0].n)
	}

	// --- The request on the wire is one a real endpoint would accept ------

	reqs := rig.server.received()
	if len(reqs) != 1 {
		t.Fatalf("endpoint received %d requests, want exactly 1", len(reqs))
	}
	req := reqs[0]
	if req.Method != http.MethodPost || req.Path != "/chat/completions" {
		t.Errorf("request = %s %s, want POST /chat/completions", req.Method, req.Path)
	}
	if req.Model != "qwen2.5vl:7b" {
		t.Errorf("model = %q, want the configured %q", req.Model, "qwen2.5vl:7b")
	}
	if req.Format == nil || req.Format.Type != "json_object" {
		t.Errorf("response_format = %+v, want {json_object} on the first attempt", req.Format)
	}

	parts := req.userParts(t)
	if len(parts) != 3 {
		t.Fatalf("user message had %d parts, want 3 (context text + 2 frames)", len(parts))
	}
	if parts[0].Type != "text" {
		t.Errorf("part 0 type = %q, want %q", parts[0].Type, "text")
	}
	for i, part := range parts[1:] {
		if part.Type != "image_url" || part.ImageURL == nil {
			t.Fatalf("part %d = %+v, want an image_url part", i+1, part)
		}
		if !strings.HasPrefix(part.ImageURL.URL, "data:image/jpeg;base64,") {
			t.Errorf("part %d URL = %q, want a base64 JPEG data URI", i+1, part.ImageURL.URL)
		}
	}

	// The prompt context is built from the MERGED event, so it carries material
	// that only ever arrived on the segment-* messages — the detection and its
	// best score, the recognized face, both zones — and the display name the
	// namer resolved. Any of these missing means the sparse terminal message won
	// and the model was asked to describe an empty event.
	for _, want := range []string{"Front Door", "person 93%", "Caleb", "driveway", "porch", "Duration: 12 seconds"} {
		if !strings.Contains(parts[0].Text, want) {
			t.Errorf("prompt context is missing %q:\n%s", want, parts[0].Text)
		}
	}

	// Only the API key is asserted indirectly here (the header is set by the
	// same attempt that produced the request above); a missing Authorization
	// header against a real hosted endpoint is a 401, which client_test.go
	// covers directly.
}

// TestIngestion_AIDescriptionsDisabled_NeverCallsEndpointOrStoresDescription is
// the other half of the criterion, and the one that protects every install that
// leaves the feature off — which is the default, and which must cost nothing.
//
// "Inert" is asserted where it actually matters: the endpoint receives ZERO
// requests. Asserting only that the stored description is nil would pass a
// build that generated a description, spent the user's money, and then dropped
// it on the floor.
func TestIngestion_AIDescriptionsDisabled_NeverCallsEndpointOrStoresDescription(t *testing.T) {
	rig := newAIDescriptionRig(t, sdk.EventDescription{Title: "should never be requested"}, map[string]any{
		describe.KeyEnabled: false,
	})

	ingestPersonEventLifecycle(rig.ingester)
	rig.describer.Close()

	if reqs := rig.server.received(); len(reqs) != 0 {
		t.Errorf("endpoint received %d requests with aiDescriptionsEnabled=false, want 0: %+v", len(reqs), reqs)
	}
	// Frames are sampled on the worker, after the gate, so a disabled install
	// must not even pay for ffmpeg.
	if calls := rig.sampler.sampleCalls(); len(calls) != 0 {
		t.Errorf("frame sampler called %d times with the feature disabled, want 0", len(calls))
	}

	got, err := rig.events.Query([]string{"cam-1"}, GetEventsOptions{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got.Events) != 1 {
		t.Fatalf("got %d events, want 1 — ingestion itself must be unaffected by the feature being off", len(got.Events))
	}
	ev := got.Events[0]
	if len(ev.Segments) == 0 {
		t.Fatalf("event has no segments; ingestion regressed independently of descriptions: %+v", ev)
	}
	if desc := ev.Segments[0].Description; desc != nil {
		t.Errorf("Segments[0].Description = %+v, want nil with the feature disabled", desc)
	}
}
