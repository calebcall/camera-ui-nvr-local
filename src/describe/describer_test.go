package describe

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/cameraui/sdk/go"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/store"
)

// awaitTimeout bounds every "wait for the worker to get there" step in this
// file. Generous on purpose: these tests do no real work, so the only thing
// this needs to survive is a loaded CI box or the race detector's overhead —
// and a timeout that trips under load is a flaky test, which is worse than a
// slow one.
const awaitTimeout = 5 * time.Second

// sampleCall records one SampleFrames invocation. The deadline fields are the
// point of recording anything at all: the Describer's contract is that frame
// sampling and the model call share ONE deadline derived from aiTimeoutSeconds,
// and the only way to verify that without waiting out the 10-second minimum
// timeout is to inspect the context the worker hands down.
type sampleCall struct {
	cameraID    string
	startMs     int64
	endMs       int64
	frameCount  int
	deadline    time.Time
	hasDeadline bool
}

// fakeSampler stands in for *media.FrameSampler: no ffmpeg, no filesystem.
//
// block and entered together give the queue tests a barrier instead of a sleep.
// entered is signalled on entry (before blocking), so a test that has received
// from it knows for certain the worker has dequeued an event and is inside the
// expensive path; block then holds the worker there until the test releases it.
// That pairing is what makes the drop-oldest test deterministic rather than
// timing-dependent.
type fakeSampler struct {
	mu    sync.Mutex
	calls []sampleCall

	// frames and err are the canned result. Written once at construction and
	// only read afterwards, so they need no lock.
	frames [][]byte
	err    error

	block   chan struct{} // when non-nil, hold until closed (or ctx ends)
	entered chan struct{} // when non-nil, receives one token per entry
}

func (f *fakeSampler) SampleFrames(ctx context.Context, cameraID string, startMs, endMs int64, n int) ([][]byte, error) {
	deadline, hasDeadline := ctx.Deadline()

	f.mu.Lock()
	f.calls = append(f.calls, sampleCall{
		cameraID:    cameraID,
		startMs:     startMs,
		endMs:       endMs,
		frameCount:  n,
		deadline:    deadline,
		hasDeadline: hasDeadline,
	})
	f.mu.Unlock()

	// Non-blocking send into a buffered channel: a test that isn't watching
	// entered must not stall the worker, which would turn an assertion failure
	// into a hang.
	if f.entered != nil {
		select {
		case f.entered <- struct{}{}:
		default:
		}
	}

	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.frames, f.err
}

func (f *fakeSampler) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeSampler) callAt(i int) sampleCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[i]
}

// newRelease returns the channel a fakeSampler blocks on plus the func that
// unblocks it.
//
// The func is idempotent and also registered with t.Cleanup, so a test that
// fails an assertion while the worker is held still releases it. Without that,
// one failed assertion would leave a goroutine wedged inside the sampler
// forever, and any later Close would block in wg.Wait — turning a clean test
// failure into a hung run that only the package timeout ends.
func newRelease(t *testing.T) (chan struct{}, func()) {
	t.Helper()
	ch := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(ch) }) }
	t.Cleanup(release)
	return ch, release
}

// awaitEntry blocks until the sampler has been entered once more, i.e. the
// worker has dequeued an event and started work on it.
func (f *fakeSampler) awaitEntry(t *testing.T) {
	t.Helper()
	select {
	case <-f.entered:
	case <-time.After(awaitTimeout):
		t.Fatal("timed out waiting for the worker to start sampling frames")
	}
}

// fakeWriter stands in for *store.EventStore. done is buffered so a write
// never blocks the worker even when a test only ever reads one token.
type fakeWriter struct {
	mu     sync.Mutex
	writes map[string]sdk.EventDescription
	err    error
	done   chan string
}

func newFakeWriter() *fakeWriter {
	return &fakeWriter{writes: map[string]sdk.EventDescription{}, done: make(chan string, 64)}
}

func (f *fakeWriter) SetDescription(eventID string, desc sdk.EventDescription) error {
	f.mu.Lock()
	f.writes[eventID] = desc
	err := f.err
	f.mu.Unlock()

	select {
	case f.done <- eventID:
	default:
	}
	return err
}

func (f *fakeWriter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

func (f *fakeWriter) wrote(eventID string) (sdk.EventDescription, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	desc, ok := f.writes[eventID]
	return desc, ok
}

// awaitWrite blocks until any description has been persisted.
func (f *fakeWriter) awaitWrite(t *testing.T) string {
	t.Helper()
	select {
	case id := <-f.done:
		return id
	case <-time.After(awaitTimeout):
		t.Fatal("timed out waiting for SetDescription")
		return ""
	}
}

// completeCall records one Complete invocation. contextText is captured because
// it is the only observable proof that the camera's display name reached the
// prompt.
type completeCall struct {
	cfg         Config
	contextText string
	frames      [][]byte
	deadline    time.Time
	hasDeadline bool
}

// fakeCompleter stands in for *Client, so no test in this file needs an HTTP
// server — client_test.go already covers the wire contract.
type fakeCompleter struct {
	mu    sync.Mutex
	calls []completeCall

	desc sdk.EventDescription
	err  error
}

func (f *fakeCompleter) Complete(ctx context.Context, cfg Config, contextText string, frames [][]byte) (sdk.EventDescription, error) {
	deadline, hasDeadline := ctx.Deadline()

	f.mu.Lock()
	f.calls = append(f.calls, completeCall{
		cfg:         cfg,
		contextText: contextText,
		frames:      frames,
		deadline:    deadline,
		hasDeadline: hasDeadline,
	})
	f.mu.Unlock()

	return f.desc, f.err
}

func (f *fakeCompleter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeCompleter) callAt(i int) completeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[i]
}

type fakeNamer map[string]string

func (f fakeNamer) CameraName(cameraID string) (string, bool) {
	n, ok := f[cameraID]
	return n, ok
}

// flippingGetter answers KeyEnabled true for the first read and false for every
// read after it, which is exactly the shape of "the user turned the feature off
// while an event was sitting in the queue": DescribeAsync's Load sees enabled,
// the worker's Load does not. Deterministic because DescribeAsync's read
// happens-before the event is visible to the worker.
type flippingGetter struct {
	base fakeGetter

	mu           sync.Mutex
	enabledReads int
}

func (g *flippingGetter) GetValue(key string, fallback ...any) any {
	if key == KeyEnabled {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.enabledReads++
		return g.enabledReads == 1
	}
	return g.base.GetValue(key, fallback...)
}

// enabledStore is a ConfigGetter with the feature on and both required fields
// set, plus whatever the caller wants to override.
func enabledStore(extra map[string]any) fakeGetter {
	g := fakeGetter{
		KeyEnabled: true,
		KeyBaseURL: "http://localhost:11434/v1",
		KeyModel:   "test-model",
	}
	for k, v := range extra {
		g[k] = v
	}
	return g
}

// labeledDetection is one (label, score) pair for endedEvent. A slice of these
// rather than a map, so the generated event's detection order — and therefore
// EventLabels' output — is identical on every run.
type labeledDetection struct {
	label string
	score float64
}

// endedEvent builds a terminal object-detection event that clears every gate by
// default.
//
// Types is populated from the labels and NOT left empty, because
// store.EventHasDetections keys off Types rather than Segments (see its doc
// comment): an event with detections in its segments but no types is a
// motion-only event as far as the gate is concerned, and would never be
// described.
func endedEvent(id string, dets ...labeledDetection) store.DetectionEvent {
	segDets := make([]sdk.EventDetection, 0, len(dets))
	types := make([]string, 0, len(dets))
	for _, d := range dets {
		segDets = append(segDets, sdk.EventDetection{Label: d.label, Score: d.score})
		types = append(types, d.label)
	}
	return store.DetectionEvent{
		ID:        id,
		CameraID:  "cam-1",
		State:     sdk.DetectionEventStateEnded,
		StartTime: 1_700_000_000_000,
		EndTime:   1_700_000_010_000,
		Types:     types,
		Segments: []sdk.EventSegment{{
			FirstSeen:  1_700_000_000_000,
			LastSeen:   1_700_000_010_000,
			Detections: segDets,
		}},
	}
}

// personEvent is the canonical qualifying event: one high-confidence person.
func personEvent(id string) store.DetectionEvent {
	return endedEvent(id, labeledDetection{"person", 0.9})
}

// newTestDescriber wires a Describer with fakes and substitutes the stub
// completer for the real HTTP Client.
func newTestDescriber(t *testing.T, g ConfigGetter, sampler *fakeSampler, w *fakeWriter, comp *fakeCompleter) *Describer {
	t.Helper()
	d := NewDescriber(g, sampler, w, fakeNamer{"cam-1": "Sideyard"}, nil)
	d.client = comp
	// Frame sampling retries while empty (frame_retry.go); wait instantly so
	// these tests exercise the behaviour without spending the schedule.
	d.wait = func(context.Context, time.Duration) bool { return true }
	return d
}

// TestDescriber_QualifyingEvent_WritesDescription is the end-to-end happy path,
// and also pins what the worker passes down: the event's full time window (a
// description of a 10-second event needs all 10 seconds, not an instant) and
// the configured frame count.
func TestDescriber_QualifyingEvent_WritesDescription(t *testing.T) {
	sampler := &fakeSampler{frames: [][]byte{[]byte("f1"), []byte("f2")}}
	w := newFakeWriter()
	comp := &fakeCompleter{desc: sdk.EventDescription{Title: "Person at door", Description: "Narrative.", ThreatLevel: 1}}
	d := newTestDescriber(t, enabledStore(nil), sampler, w, comp)
	defer d.Close()

	ev := personEvent("ev-1")
	d.DescribeAsync(ev)

	if id := w.awaitWrite(t); id != "ev-1" {
		t.Errorf("wrote description for %q, want ev-1", id)
	}
	desc, ok := w.wrote("ev-1")
	if !ok {
		t.Fatal("no description stored for ev-1")
	}
	if desc.Title != "Person at door" || desc.ThreatLevel != 1 {
		t.Errorf("stored description = %+v, want the completer's reply verbatim", desc)
	}

	call := sampler.callAt(0)
	if call.cameraID != "cam-1" || call.startMs != ev.StartTime || call.endMs != ev.EndTime {
		t.Errorf("sampled %q over [%d,%d], want cam-1 over [%d,%d]", call.cameraID, call.startMs, call.endMs, ev.StartTime, ev.EndTime)
	}
	if call.frameCount != DefaultFrameCount {
		t.Errorf("sampled %d frames, want %d", call.frameCount, DefaultFrameCount)
	}
	if got := comp.callAt(0).frames; len(got) != 2 {
		t.Errorf("completer got %d frames, want the sampler's 2", len(got))
	}
}

// TestDescriber_RejectedByGate_DoesNoWork covers every reason an event must not
// cost an ffmpeg run or an inference. Each case asserts on all three fakes,
// because "no work" means no frames, no model call, AND no write — a gate that
// skipped only the model call would still burn CPU on frame extraction.
//
// Close() before asserting is what makes this deterministic: it closes the
// queue and waits for the worker to drain, so if the event HAD been enqueued
// the fakes would have recorded it by the time the assertions run.
func TestDescriber_RejectedByGate_DoesNoWork(t *testing.T) {
	active := personEvent("ev-active")
	active.State = sdk.DetectionEventStateActive
	active.EndTime = 0

	// A motion-only event: a trigger but no detection types, which is exactly
	// what store.EventHasDetections rejects.
	motionOnly := store.DetectionEvent{
		ID: "ev-motion", CameraID: "cam-1", State: sdk.DetectionEventStateEnded,
		StartTime: 1000, EndTime: 2000,
		Types:    []string{"motion"},
		Triggers: []sdk.EventTrigger{{Type: sdk.EventTriggerMotion, Score: 0.99}},
		Segments: []sdk.EventSegment{},
	}

	for _, tc := range []struct {
		name  string
		store ConfigGetter
		event store.DetectionEvent
	}{
		{"disabled", fakeGetter{KeyEnabled: false}, personEvent("ev-1")},
		{"not terminal", enabledStore(nil), active},
		{"no detections", enabledStore(nil), motionOnly},
		{"label not allowed", enabledStore(map[string]any{KeyLabels: "person,vehicle"}), endedEvent("ev-1", labeledDetection{"cat", 0.9})},
		{"below min confidence", enabledStore(map[string]any{KeyMinConfidence: float64(0.8)}), endedEvent("ev-1", labeledDetection{"person", 0.5})},
		{"disabled between enqueue and dequeue", &flippingGetter{base: enabledStore(nil)}, personEvent("ev-1")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sampler := &fakeSampler{frames: [][]byte{[]byte("f1")}}
			w := newFakeWriter()
			comp := &fakeCompleter{desc: sdk.EventDescription{Title: "T", Description: "D"}}
			d := newTestDescriber(t, tc.store, sampler, w, comp)

			d.DescribeAsync(tc.event)
			d.Close()

			if got := sampler.callCount(); got != 0 {
				t.Errorf("sampler called %d times, want 0", got)
			}
			if got := comp.callCount(); got != 0 {
				t.Errorf("completer called %d times, want 0", got)
			}
			if got := w.count(); got != 0 {
				t.Errorf("writer called %d times, want 0", got)
			}
		})
	}
}

// TestDescriber_ValidateGate_IsUnreachableThroughStoredSettings documents why
// there is no "misconfigured endpoint" case in the gate table above, and why
// DescribeAsync's Validate check is nonetheless worth keeping.
//
// Load substitutes DefaultBaseURL and DefaultModel for a blank, whitespace-only,
// or wrong-typed stored value, so no combination of settings can produce a
// Config that Validate rejects — the gate is purely defensive, against a Config
// assembled in code (which is what config_test.go covers). Asserting that here
// stops someone from later "fixing" a gate-table case that could never have
// failed, or from removing the guard on the assumption that it is dead code.
func TestDescriber_ValidateGate_IsUnreachableThroughStoredSettings(t *testing.T) {
	cfg := Load(fakeGetter{KeyEnabled: true, KeyBaseURL: "   ", KeyModel: ""})
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil: Load must substitute defaults for blank settings", err)
	}
	if err := (Config{BaseURL: "http://x/v1"}).Validate(); err == nil {
		t.Error("Validate() with no model = nil, want an error")
	}
}

// TestDescriber_MatchingLabelAndConfidence_Describes is the positive half of
// the two configurable gates: a case-insensitive allow-list match and a score
// above the floor must still get through.
func TestDescriber_MatchingLabelAndConfidence_Describes(t *testing.T) {
	sampler := &fakeSampler{frames: [][]byte{[]byte("f1")}}
	w := newFakeWriter()
	comp := &fakeCompleter{desc: sdk.EventDescription{Title: "T", Description: "D"}}
	d := newTestDescriber(t, enabledStore(map[string]any{
		KeyLabels:        "PERSON, vehicle",
		KeyMinConfidence: float64(0.5),
	}), sampler, w, comp)
	defer d.Close()

	d.DescribeAsync(personEvent("ev-1"))

	if id := w.awaitWrite(t); id != "ev-1" {
		t.Errorf("described %q, want ev-1", id)
	}
}

// TestDescriber_DuplicateEvent_DescribesOnce guards spend, not just noise: the
// core can deliver more than one terminal message for an event, and each extra
// one would otherwise buy a second inference for an identical result.
func TestDescriber_DuplicateEvent_DescribesOnce(t *testing.T) {
	sampler := &fakeSampler{frames: [][]byte{[]byte("f1")}}
	w := newFakeWriter()
	comp := &fakeCompleter{desc: sdk.EventDescription{Title: "T", Description: "D"}}
	d := newTestDescriber(t, enabledStore(nil), sampler, w, comp)

	ev := personEvent("ev-1")
	d.DescribeAsync(ev)
	d.DescribeAsync(ev)
	d.Close()

	if got := comp.callCount(); got != 1 {
		t.Errorf("completer called %d times, want exactly 1", got)
	}
}

// TestDescriber_NoFramesAvailable_SkipsModelCall covers the ordinary case of an
// event whose footage was never recorded or has already been pruned. Zero
// frames is not an error from the sampler, so the worker has to notice it
// itself — sending a request with no images would spend money to be told
// nothing.
//
// It is sampled frameRetryAttempts times rather than once: an empty result
// usually means the covering segment has not been finalized yet, so it is
// retried before being abandoned (frame_retry.go). Footage that never arrives
// still ends here, with no model call and nothing written.
func TestDescriber_NoFramesAvailable_SkipsModelCall(t *testing.T) {
	sampler := &fakeSampler{frames: nil}
	w := newFakeWriter()
	comp := &fakeCompleter{desc: sdk.EventDescription{Title: "T", Description: "D"}}
	d := newTestDescriber(t, enabledStore(nil), sampler, w, comp)

	d.DescribeAsync(personEvent("ev-1"))
	d.Close()

	if got := sampler.callCount(); got != frameRetryAttempts {
		t.Errorf("sampler called %d times, want %d", got, frameRetryAttempts)
	}
	if got := comp.callCount(); got != 0 {
		t.Errorf("completer called %d times, want 0 (no frames to send)", got)
	}
	if got := w.count(); got != 0 {
		t.Errorf("writer called %d times, want 0", got)
	}
}

// TestDescriber_FailuresAreSwallowed pins the package's central promise: every
// failure in the description path is logged and dropped. OnDetectionEvent's
// callback has no error return, so a panic or a propagated error here would
// take out ingestion, recording, and notification for an event that was
// otherwise handled perfectly.
func TestDescriber_FailuresAreSwallowed(t *testing.T) {
	for _, tc := range []struct {
		name       string
		sampler    *fakeSampler
		comp       *fakeCompleter
		writerErr  error
		wantWrites int
	}{
		{
			name:    "sampler error",
			sampler: &fakeSampler{err: errors.New("ffmpeg exploded")},
			comp:    &fakeCompleter{desc: sdk.EventDescription{Title: "T", Description: "D"}},
		},
		{
			name:    "completer error",
			sampler: &fakeSampler{frames: [][]byte{[]byte("f1")}},
			comp:    &fakeCompleter{err: errors.New("model exploded")},
		},
		{
			name:       "writer error",
			sampler:    &fakeSampler{frames: [][]byte{[]byte("f1")}},
			comp:       &fakeCompleter{desc: sdk.EventDescription{Title: "T", Description: "D"}},
			writerErr:  errors.New("database exploded"),
			wantWrites: 1, // attempted, and its error swallowed
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newFakeWriter()
			w.err = tc.writerErr
			d := newTestDescriber(t, enabledStore(nil), tc.sampler, w, tc.comp)

			d.DescribeAsync(personEvent("ev-1"))
			d.Close() // must neither panic nor hang

			if got := w.count(); got != tc.wantWrites {
				t.Errorf("writer called %d times, want %d", got, tc.wantWrites)
			}
		})
	}
}

// TestDescriber_SamplerError_SkipsModelCall is the cost half of the sampler's
// failure case: a failed extraction must not send a request with whatever
// partial frames came back alongside the error.
func TestDescriber_SamplerError_SkipsModelCall(t *testing.T) {
	sampler := &fakeSampler{frames: [][]byte{[]byte("f1")}, err: errors.New("ffmpeg exploded")}
	w := newFakeWriter()
	comp := &fakeCompleter{desc: sdk.EventDescription{Title: "T", Description: "D"}}
	d := newTestDescriber(t, enabledStore(nil), sampler, w, comp)

	d.DescribeAsync(personEvent("ev-1"))
	d.Close()

	if got := comp.callCount(); got != 0 {
		t.Errorf("completer called %d times, want 0", got)
	}
}

// TestDescriber_ContextText_NamesTheCamera proves the prompt gets the camera's
// display name when one resolves and falls back to the ID when it doesn't —
// including when no CameraNamer was wired at all, which must not panic.
func TestDescriber_ContextText_NamesTheCamera(t *testing.T) {
	for _, tc := range []struct {
		name  string
		namer CameraNamer
		want  string
	}{
		{"known camera", fakeNamer{"cam-1": "Sideyard"}, "Camera: Sideyard"},
		{"unknown camera", fakeNamer{}, "Camera: cam-1"},
		{"blank name", fakeNamer{"cam-1": ""}, "Camera: cam-1"},
		{"no namer wired", nil, "Camera: cam-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sampler := &fakeSampler{frames: [][]byte{[]byte("f1")}}
			w := newFakeWriter()
			comp := &fakeCompleter{desc: sdk.EventDescription{Title: "T", Description: "D"}}
			d := NewDescriber(enabledStore(nil), sampler, w, tc.namer, nil)
			d.client = comp

			d.DescribeAsync(personEvent("ev-1"))
			d.Close()

			if comp.callCount() != 1 {
				t.Fatalf("completer called %d times, want 1", comp.callCount())
			}
			if got := comp.callAt(0).contextText; !strings.Contains(got, tc.want) {
				t.Errorf("context text %q does not contain %q", got, tc.want)
			}
		})
	}
}

// TestDescriber_QueueFull_DropsOldestQueuedEvent is the load-shedding contract:
// under a burst, the queue keeps the newest work and drops the oldest, because
// a description of what just happened is worth more than one of an event from
// minutes ago.
//
// Deterministic by construction, not by timing:
//
//  1. queue depth 1, and a sampler that blocks;
//  2. ev-1 is enqueued, and the test WAITS on the sampler's entry barrier —
//     receiving from it proves the worker has already dequeued ev-1, so the
//     buffer is empty and the worker cannot advance until the test releases it;
//  3. ev-2 therefore fills the (empty, capacity-1) buffer;
//  4. ev-3 finds it full and must evict ev-2.
//
// Nothing here depends on how fast anything runs — step 2's barrier is what
// removes the race a sleep or a poll would leave behind.
func TestDescriber_QueueFull_DropsOldestQueuedEvent(t *testing.T) {
	block, release := newRelease(t)
	sampler := &fakeSampler{
		frames:  [][]byte{[]byte("f1")},
		block:   block,
		entered: make(chan struct{}, 8),
	}
	w := newFakeWriter()
	comp := &fakeCompleter{desc: sdk.EventDescription{Title: "T", Description: "D"}}
	d := newTestDescriber(t, enabledStore(map[string]any{KeyQueueDepth: float64(1)}), sampler, w, comp)

	d.DescribeAsync(personEvent("ev-1"))
	sampler.awaitEntry(t) // ev-1 is now off the queue and held inside the sampler

	d.DescribeAsync(personEvent("ev-2")) // fills the buffer
	d.DescribeAsync(personEvent("ev-3")) // must evict ev-2

	release()
	d.Close() // drains ev-3 and waits for the worker to exit

	if _, ok := w.wrote("ev-1"); !ok {
		t.Error("ev-1 was not described; the in-flight event must still complete")
	}
	if _, ok := w.wrote("ev-2"); ok {
		t.Error("ev-2 was described; it should have been dropped as the oldest queued event")
	}
	if _, ok := w.wrote("ev-3"); !ok {
		t.Error("ev-3 was not described; the newest event must survive the overflow")
	}
}

// TestDescriber_QueueFull_DoesNotBlockTheCaller is the other half of the
// overflow contract, and the reason drop-oldest exists at all: DescribeAsync
// runs on the core's detection callback, so it must return even when the queue
// is full and the worker is wedged on an unresponsive endpoint. A plain
// blocking send would stall ingestion for the whole timeout.
func TestDescriber_QueueFull_DoesNotBlockTheCaller(t *testing.T) {
	block, release := newRelease(t)
	sampler := &fakeSampler{
		frames:  [][]byte{[]byte("f1")},
		block:   block,
		entered: make(chan struct{}, 8),
	}
	w := newFakeWriter()
	comp := &fakeCompleter{desc: sdk.EventDescription{Title: "T", Description: "D"}}
	d := newTestDescriber(t, enabledStore(map[string]any{KeyQueueDepth: float64(1)}), sampler, w, comp)

	d.DescribeAsync(personEvent("ev-0"))
	sampler.awaitEntry(t)

	// Far more events than the queue can hold. Enqueued from a separate
	// goroutine and reported through a channel, so a regression to a blocking
	// send fails this test instead of hanging it until the package timeout.
	returned := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			d.DescribeAsync(personEvent(eventID(0, i)))
		}
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(awaitTimeout):
		t.Fatal("DescribeAsync blocked on a full queue; it must shed work instead")
	}

	release()
	d.Close()
}

// TestDescriber_Work_CarriesTheConfiguredDeadline pins the timeout wiring
// without waiting one out: the minimum configurable timeout is 10 seconds, far
// too slow for a unit test, so this asserts on the deadline the worker attaches
// to the context instead of on the abandonment itself.
//
// The deadline must be present, must be no further out than the configured
// timeout, and — critically — must be the SAME deadline for frame sampling and
// the model call, so a hung endpoint cannot hold the serial worker past
// aiTimeoutSeconds by splitting the budget across two phases.
func TestDescriber_Work_CarriesTheConfiguredDeadline(t *testing.T) {
	sampler := &fakeSampler{frames: [][]byte{[]byte("f1")}}
	w := newFakeWriter()
	comp := &fakeCompleter{desc: sdk.EventDescription{Title: "T", Description: "D"}}
	// 30s is inside the clamped range [10, 600], so it survives Load intact.
	d := newTestDescriber(t, enabledStore(map[string]any{KeyTimeoutSeconds: float64(30)}), sampler, w, comp)

	before := time.Now()
	d.DescribeAsync(personEvent("ev-1"))
	d.Close()
	after := time.Now()

	if sampler.callCount() != 1 || comp.callCount() != 1 {
		t.Fatalf("sampler called %d times and completer %d, want 1 each", sampler.callCount(), comp.callCount())
	}

	sampled := sampler.callAt(0)
	if !sampled.hasDeadline {
		t.Fatal("the context passed to SampleFrames carries no deadline; a hung ffmpeg would wedge the worker forever")
	}
	if sampled.deadline.Before(before.Add(30*time.Second)) || sampled.deadline.After(after.Add(30*time.Second)) {
		t.Errorf("sampler deadline = %v, want ~%v (30s out)", sampled.deadline, before.Add(30*time.Second))
	}

	completed := comp.callAt(0)
	if !completed.hasDeadline {
		t.Fatal("the context passed to Complete carries no deadline")
	}
	if !completed.deadline.Equal(sampled.deadline) {
		t.Errorf("Complete's deadline %v differs from SampleFrames' %v; both phases must share one budget", completed.deadline, sampled.deadline)
	}
	if got := completed.cfg.Timeout; got != 30*time.Second {
		t.Errorf("Complete got cfg.Timeout = %v, want 30s", got)
	}
}

// TestDescriber_SlowEvent_WorkerSurvivesForLaterEvents proves the worker
// goroutine is not consumed by one slow event: a single wedged endpoint must
// delay subsequent descriptions, not end them.
func TestDescriber_SlowEvent_WorkerSurvivesForLaterEvents(t *testing.T) {
	block, release := newRelease(t)
	sampler := &fakeSampler{
		frames:  [][]byte{[]byte("f1")},
		block:   block,
		entered: make(chan struct{}, 8),
	}
	w := newFakeWriter()
	comp := &fakeCompleter{desc: sdk.EventDescription{Title: "T", Description: "D"}}
	d := newTestDescriber(t, enabledStore(nil), sampler, w, comp)

	d.DescribeAsync(personEvent("ev-1"))
	sampler.awaitEntry(t)
	release()

	if id := w.awaitWrite(t); id != "ev-1" {
		t.Fatalf("first write was for %q, want ev-1", id)
	}

	d.DescribeAsync(personEvent("ev-2"))
	if id := w.awaitWrite(t); id != "ev-2" {
		t.Errorf("second write was for %q, want ev-2 (the worker must outlive the first event)", id)
	}
	d.Close()
}

// TestDescriber_Close_IsIdempotent covers the two shapes a shutdown actually
// takes: a Describer that never received any work at all (the feature is off
// for most installs, so this is the common case), and a second Close from a
// duplicate shutdown path. Both must return rather than panic on a
// double-closed channel.
func TestDescriber_Close_IsIdempotent(t *testing.T) {
	d := newTestDescriber(t, enabledStore(nil), &fakeSampler{}, newFakeWriter(), &fakeCompleter{})
	d.Close()
	d.Close()
}

// TestDescriber_DescribeAsyncAfterClose_DropsTheEvent covers the shutdown race
// the plugin cannot fully avoid: OnDetectionEvent callbacks can still arrive
// while (or after) the shutdown handler closes the Describer. Sending on a
// closed channel panics, and a panic in a detection callback takes the plugin
// with it, so a post-Close event must be dropped quietly instead.
func TestDescriber_DescribeAsyncAfterClose_DropsTheEvent(t *testing.T) {
	sampler := &fakeSampler{frames: [][]byte{[]byte("f1")}}
	w := newFakeWriter()
	comp := &fakeCompleter{desc: sdk.EventDescription{Title: "T", Description: "D"}}
	d := newTestDescriber(t, enabledStore(nil), sampler, w, comp)

	d.Close()
	d.DescribeAsync(personEvent("ev-1")) // must not panic

	if got := w.count(); got != 0 {
		t.Errorf("writer called %d times after Close, want 0", got)
	}
}

// TestDescriber_ConcurrentDescribeAsync_IsSafe drives the queue from several
// goroutines at once, which is the state the plugin is actually in: every
// camera's detection callback can fire independently. Its real value is under
// -race, and as a liveness check — the drop-oldest path must always terminate,
// never spin or deadlock, no matter how many producers collide on a full queue.
func TestDescriber_ConcurrentDescribeAsync_IsSafe(t *testing.T) {
	block, release := newRelease(t)
	sampler := &fakeSampler{
		frames:  [][]byte{[]byte("f1")},
		block:   block,
		entered: make(chan struct{}, 64),
	}
	w := newFakeWriter()
	comp := &fakeCompleter{desc: sdk.EventDescription{Title: "T", Description: "D"}}
	d := newTestDescriber(t, enabledStore(map[string]any{KeyQueueDepth: float64(2)}), sampler, w, comp)

	// The first wave collides on a queue that is permanently full (the worker is
	// wedged inside the sampler); releasing it and running a second wave
	// exercises the other shape, a queue emptying underneath the producers.
	race := func(wave int) {
		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				for i := 0; i < 25; i++ {
					d.DescribeAsync(personEvent(eventID(wave*8+g, i)))
				}
			}(g)
		}

		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(awaitTimeout):
			t.Error("concurrent DescribeAsync never finished; the eviction loop is spinning or blocking")
		}
	}

	race(0)
	release()
	race(1) // fresh IDs, so dedup doesn't turn the second wave into a no-op

	d.Close()
}

// eventID builds a distinct event ID per producer goroutine and iteration, so
// the dedup set never collapses two producers' work into one.
func eventID(producer, seq int) string {
	return fmt.Sprintf("ev-%d-%d", producer, seq)
}
