package describe

import (
	"context"
	"fmt"
	"sync"

	sdk "github.com/cameraui/sdk/go"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/store"
)

// seenEventsCap bounds the dedup set so a producer emitting unbounded distinct
// event IDs can't leak this map's memory. Same approach, and the same reason, as
// notifiedEvents in events_ingest.go — smaller only because this set exists to
// catch a duplicate terminal message for an event that was described moments
// ago, not to remember an entire session's events.
const seenEventsCap = 512

// FrameSampler is the subset of *media.FrameSampler the Describer needs, so
// this package depends on the shape of "give me some frames" rather than on
// ffmpeg. Zero frames with a nil error is a legitimate result (nothing was
// recorded over the event's window), which is why describe checks the length
// separately from the error.
type FrameSampler interface {
	SampleFrames(ctx context.Context, cameraID string, startMs, endMs int64, n int) ([][]byte, error)
}

// DescriptionWriter is the subset of *store.EventStore the Describer needs. A
// one-method interface both because it is all this package uses and because it
// keeps SQLite entirely out of here — the description path can be tested with no
// database at all.
type DescriptionWriter interface {
	SetDescription(eventID string, desc sdk.EventDescription) error
}

// CameraNamer resolves a camera ID to its display name, so the prompt can say
// "Sideyard" rather than a UUID — a name the user chose is a location the model
// can reason about. *recorder.RecorderManager satisfies it. The bool is false
// when the camera is unknown (removed between ingestion and generation, most
// likely), and describe falls back to the raw ID rather than sending nothing.
type CameraNamer interface {
	CameraName(cameraID string) (string, bool)
}

// completer is the subset of *Client the worker calls, so tests can stub the
// model without standing up an HTTP server. Its signature must match
// Client.Complete exactly; the assertion below fails the build if it drifts.
type completer interface {
	Complete(ctx context.Context, cfg Config, contextText string, frames [][]byte) (sdk.EventDescription, error)
}

var _ completer = (*Client)(nil)

// Describer turns terminal detection events into stored AI descriptions.
//
// Work is strictly serial: one worker goroutine drains a bounded queue. That is
// not a simplification, it is the point — a burst across eight cameras would
// otherwise launch eight concurrent ffmpeg processes and eight concurrent
// inference calls, which a single local GPU handles catastrophically badly and a
// paid API bills for enthusiastically. Serial work plus a shallow queue means
// the feature's worst case is "some events go undescribed", never "the box falls
// over".
//
// Every exported method is safe to call from any goroutine: DescribeAsync runs
// on the core's per-camera detection callbacks, which carry no
// single-goroutine guarantee, and Close runs on the shutdown path.
type Describer struct {
	cfg    ConfigGetter
	frames FrameSampler
	writer DescriptionWriter
	names  CameraNamer
	client completer
	log    *sdk.Logger

	// queue hands events to the single worker; wg tracks that worker so Close
	// can wait for the in-flight event to land before the database goes away.
	queue chan store.DetectionEvent
	wg    sync.WaitGroup

	// mu guards everything below it, and is also held across the (non-blocking)
	// send in enqueue. Two reasons that matters, both load-bearing:
	//
	//   - closed must be checked and the send performed atomically, because a
	//     send on a channel Close already closed panics, and a panic inside a
	//     detection callback takes the whole plugin down.
	//   - it serializes producers, which is what makes the drop-oldest eviction
	//     in enqueue provably terminate. See that method.
	mu        sync.Mutex
	closed    bool
	seen      map[string]struct{}
	seenOrder []string
}

// NewDescriber returns a running Describer: the worker goroutine starts here, so
// DescribeAsync is usable the moment this returns. Call Close at shutdown.
//
// The queue's capacity is read once, now (QueueDepth) — unlike every other
// setting, which Load re-reads per event. A channel cannot be resized, so
// changing aiQueueDepth needs a plugin restart, and the schema's help text says
// so.
//
// log may be nil, as it is in tests; every log call goes through logf.
func NewDescriber(cfg ConfigGetter, frames FrameSampler, writer DescriptionWriter, names CameraNamer, log *sdk.Logger) *Describer {
	d := &Describer{
		cfg:    cfg,
		frames: frames,
		writer: writer,
		names:  names,
		client: NewClient(),
		log:    log,
		queue:  make(chan store.DetectionEvent, QueueDepth(cfg)),
	}

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		// Ranging over the channel (rather than selecting on a done channel)
		// makes Close drain what is already queued instead of discarding it:
		// those events are the most recent activity, and they cost nothing but
		// the time Close already spends waiting for the in-flight one.
		for ev := range d.queue {
			d.describe(ev)
		}
	}()

	return d
}

// DescribeAsync enqueues event for description if it qualifies, and returns
// immediately. Every check here is cheap and synchronous; anything that touches
// ffmpeg or the network happens on the worker, so the core's detection callback
// is never blocked by this call.
//
// Ordered cheapest-first, so the overwhelmingly common case (the feature is off)
// costs two field reads:
//
//  1. terminal only — a description needs the event's full time window, and an
//     active event's EndTime is still 0. Also the natural dedup point: exactly
//     one message per event is terminal.
//  2. detections only — the same rule the notification path uses
//     (store.EventHasDetections), so motion-only and audio-only events never
//     spend an inference on an empty frame.
//  3. enabled and configured.
//  4. label allow-list.
//  5. minimum confidence — store.BestConfidence, deliberately the same ranking
//     that populates the indexed confidence column the event list filters on, so
//     a user's floor means the same thing in both places.
//  6. not already seen — this one guards spend rather than noise: a duplicate
//     terminal message would otherwise buy a second inference for an identical
//     result.
func (d *Describer) DescribeAsync(event store.DetectionEvent) {
	if !isTerminal(event) {
		return
	}
	if !store.EventHasDetections(event) {
		return
	}

	cfg := Load(d.cfg)
	if !cfg.Enabled {
		return
	}
	if err := cfg.Validate(); err != nil {
		// Unreachable through stored settings — Load substitutes defaults for
		// both fields Validate checks — so this is a guard against a Config
		// assembled in code, logged rather than silent because a
		// half-configured install should say so once per event instead of
		// failing deep inside the HTTP client.
		d.logf("describe: skipping event %s: %v", event.ID, err)
		return
	}
	if !cfg.AllowsLabels(EventLabels(event)) {
		return
	}
	if store.BestConfidence(event) < cfg.MinConfidence {
		return
	}
	if !d.markSeen(event.ID) {
		return
	}

	d.enqueue(event)
}

// enqueue puts event on the queue, evicting the OLDEST queued event when the
// queue is full, and never blocks the caller.
//
// Dropping the oldest rather than the newest is the whole shape of the feature's
// load shedding: under a burst, a description of what just happened is worth
// more than one of an event from minutes ago, which nobody is still watching
// for. Every drop is logged, because a silently truncated queue reads afterwards
// as "everything was described" when it wasn't.
//
// The loop terminates in at most two iterations, and that is a guarantee rather
// than a hope — it depends on holding d.mu:
//
//   - iteration 1's send can only fail because the queue is full;
//   - the receive that follows either takes an event (freeing a slot that no
//     other producer can steal, since they are all blocked on d.mu, and that
//     the worker cannot fill because the worker only ever receives) or finds
//     the queue empty because the worker drained it;
//   - either way iteration 2's send has a free slot and returns.
//
// Without the mutex this would be an unbounded spin under concurrent producers,
// each repeatedly evicting another's just-enqueued event. Holding a mutex across
// a channel send is normally a smell; it is safe here precisely because every
// channel operation in this method is non-blocking.
func (d *Describer) enqueue(event store.DetectionEvent) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// A detection callback can still fire during or after shutdown. Dropping
	// the event is the only correct answer: the worker is already winding down,
	// and sending on the closed channel would panic inside the core's callback.
	if d.closed {
		d.logf("describe: dropping event %s: the describer is shutting down", event.ID)
		return
	}

	for {
		select {
		case d.queue <- event:
			return
		default:
		}

		select {
		case dropped := <-d.queue:
			d.logf("describe: queue full (capacity %d), dropping event %s to make room for %s", cap(d.queue), dropped.ID, event.ID)
		default:
			// Drained by the worker between the two selects; the send above
			// succeeds on the next pass.
		}
	}
}

// describe runs the expensive path for one event, on the worker goroutine.
//
// Every failure is logged and swallowed. OnDetectionEvent's callback has no
// error return, and more importantly a missing description is a cosmetic loss
// while a failed ingestion is a lost event: nothing here may affect recording,
// upsert, thumbnails, or notification.
func (d *Describer) describe(event store.DetectionEvent) {
	// Re-read the settings rather than reusing DescribeAsync's snapshot: an
	// event can sit in the queue behind a slow inference for minutes, and if the
	// user turned the feature off in the meantime — the most likely reason
	// anyone opens these settings — the queued work must not still be billed.
	cfg := Load(d.cfg)
	if !cfg.Enabled {
		return
	}

	// One deadline covers frame sampling AND the model call, deliberately: the
	// budget the user configured is "how long may one event hold the worker",
	// and giving each phase its own timeout would let a slow ffmpeg plus a slow
	// endpoint quietly double it. A hung endpoint must not wedge every event
	// queued behind it.
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	frames, err := d.frames.SampleFrames(ctx, event.CameraID, event.StartTime, event.EndTime, cfg.FrameCount)
	if err != nil {
		d.logf("describe: sample frames for event %s: %v", event.ID, err)
		return
	}
	// Not an error from the sampler's point of view, and not one here either:
	// footage covering the window may never have been recorded, or may already
	// have been pruned. There is simply nothing to describe, and a request with
	// no images would spend money to be told so.
	if len(frames) == 0 {
		d.logf("describe: no frames available for event %s; nothing to describe", event.ID)
		return
	}

	desc, err := d.client.Complete(ctx, cfg, EventContext(d.cameraName(event.CameraID), event), frames)
	if err != nil {
		d.logf("describe: describe event %s: %v", event.ID, err)
		return
	}

	if err := d.writer.SetDescription(event.ID, desc); err != nil {
		d.logf("describe: persist description for event %s: %v", event.ID, err)
		return
	}
	d.logf("describe: described event %s (%q, threat %d)", event.ID, desc.Title, desc.ThreatLevel)
}

// cameraName resolves cameraID's display name for the prompt, falling back to
// the ID itself. The fallback covers three cases that all deserve the same
// answer — no namer wired, an unknown camera, and a camera with a blank name —
// because the ID is at least a stable identifier the user can search their
// config for, whereas an empty name invites the model to invent a location.
func (d *Describer) cameraName(cameraID string) string {
	if d.names == nil {
		return cameraID
	}
	if name, ok := d.names.CameraName(cameraID); ok && name != "" {
		return name
	}
	return cameraID
}

// Close stops accepting work, lets the worker finish what is already queued, and
// waits for it to exit. Safe to call more than once, and safe on a Describer
// that never received any work at all — the common case, since the feature ships
// disabled.
//
// Called from the plugin's shutdown handler BEFORE the database is closed, so an
// in-flight SetDescription is never writing into a closed connection. It can
// therefore take as long as one event's remaining work (bounded by
// aiTimeoutSeconds) plus the queued events behind it.
func (d *Describer) Close() {
	d.mu.Lock()
	if !d.closed {
		d.closed = true
		close(d.queue)
	}
	d.mu.Unlock()

	// Outside the lock: the worker doesn't take d.mu today, but waiting for
	// arbitrary user-visible work (ffmpeg, HTTP, SQLite) while holding a lock
	// that every detection callback needs would be a deadlock waiting to be
	// introduced.
	d.wg.Wait()
}

// markSeen reports whether id has NOT been enqueued before, recording it when
// true. Bounded to seenEventsCap by evicting the oldest entry, exactly as
// notifiedEvents.markFirst does.
//
// An event dropped by the queue is still marked seen and is never retried: a
// retry would land it behind the same backlog that caused the drop, and the
// alternative — remembering only what was actually described — would let a
// duplicate terminal message resurrect work the Describer deliberately shed.
func (d *Describer) markSeen(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.seen == nil {
		d.seen = make(map[string]struct{})
	}
	if _, ok := d.seen[id]; ok {
		return false
	}
	d.seen[id] = struct{}{}
	d.seenOrder = append(d.seenOrder, id)
	for len(d.seenOrder) > seenEventsCap {
		oldest := d.seenOrder[0]
		d.seenOrder = d.seenOrder[1:]
		delete(d.seen, oldest)
	}
	return true
}

// isTerminal mirrors events_ingest.go's isTerminalEvent: State "ended", or a
// nonzero EndTime (which is 0/omitempty until the terminal message). Duplicated
// rather than exported from package main, which cannot be imported from here;
// both definitions must stay in step, since both answer the same question of
// "is this the last message for this event".
func isTerminal(event store.DetectionEvent) bool {
	return event.State == sdk.DetectionEventStateEnded || event.EndTime > 0
}

// logf logs through d.log if one was provided (log may be nil — see
// NewDescriber).
func (d *Describer) logf(format string, args ...any) {
	if d.log == nil {
		return
	}
	d.log.Log(fmt.Sprintf(format, args...))
}
