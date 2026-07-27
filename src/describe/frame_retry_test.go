package describe

import (
	"context"
	"errors"
	"testing"
	"time"
)

// waitRecorder stands in for the real sleep so the tests below assert the
// waiting behaviour without spending it.
type waitRecorder struct {
	waits []time.Duration
	deny  bool // simulate ctx ending during the wait
}

func (w *waitRecorder) wait(_ context.Context, d time.Duration) bool {
	w.waits = append(w.waits, d)
	return !w.deny
}

func newRetryDescriber(s FrameSampler, w *waitRecorder) *Describer {
	d := &Describer{frames: s}
	if w != nil {
		d.wait = w.wait
	}
	return d
}

// framesAfter returns empty for the first n calls, then real frames — the
// shape of a segment that has not been finalized yet.
type framesAfter struct {
	emptyCalls int
	calls      int
	err        error
}

func (f *framesAfter) SampleFrames(_ context.Context, _ string, _, _ int64, _ int) ([][]byte, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.calls <= f.emptyCalls {
		return nil, nil
	}
	return [][]byte{[]byte("frame")}, nil
}

func TestSampleFramesWithRetry_RecoversOnceTheSegmentIsIndexed(t *testing.T) {
	s := &framesAfter{emptyCalls: 2}
	w := &waitRecorder{}
	d := newRetryDescriber(s, w)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	frames, err := d.sampleFramesWithRetry(ctx, "cam-1", 0, 1000, 4)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(frames) == 0 {
		t.Fatal("expected the retry to pick up the finalized segment")
	}
	if s.calls != 3 {
		t.Errorf("expected 3 sample attempts, got %d", s.calls)
	}
	if len(w.waits) != 2 {
		t.Errorf("expected 2 waits, got %d", len(w.waits))
	}
}

// The happy path must not pay for the fix.
func TestSampleFramesWithRetry_NoDelayWhenFramesArePresent(t *testing.T) {
	s := &framesAfter{}
	w := &waitRecorder{}
	d := newRetryDescriber(s, w)

	frames, err := d.sampleFramesWithRetry(context.Background(), "cam-1", 0, 1000, 4)
	if err != nil || len(frames) == 0 {
		t.Fatalf("expected frames on the first call, got %v %v", frames, err)
	}
	if s.calls != 1 {
		t.Errorf("expected exactly 1 attempt, got %d", s.calls)
	}
	if len(w.waits) != 0 {
		t.Errorf("expected no waiting, got %v", w.waits)
	}
}

// Footage that never existed, or was pruned, must not hang the worker.
func TestSampleFramesWithRetry_GivesUpWhenFootageNeverArrives(t *testing.T) {
	s := &framesAfter{emptyCalls: 1000}
	w := &waitRecorder{}
	d := newRetryDescriber(s, w)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	frames, err := d.sampleFramesWithRetry(ctx, "cam-1", 0, 1000, 4)
	if err != nil {
		t.Fatalf("running out of retries is not an error, got %v", err)
	}
	if len(frames) != 0 {
		t.Fatal("expected no frames")
	}
	if s.calls > frameRetryAttempts {
		t.Errorf("expected at most %d attempts, got %d", frameRetryAttempts, s.calls)
	}
}

// The deadline covers sampling AND inference. Retrying until it expires would
// convert a missing description into a slower missing description.
func TestSampleFramesWithRetry_StopsWhileInferenceStillHasBudget(t *testing.T) {
	s := &framesAfter{emptyCalls: 1000}
	w := &waitRecorder{}
	d := newRetryDescriber(s, w)

	ctx, cancel := context.WithTimeout(context.Background(), frameRetryDelay+time.Second)
	defer cancel()

	if _, err := d.sampleFramesWithRetry(ctx, "cam-1", 0, 1000, 4); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(w.waits) != 0 {
		t.Errorf("expected no wait when the budget cannot cover one, got %v", w.waits)
	}
	if s.calls != 1 {
		t.Errorf("expected a single attempt, got %d", s.calls)
	}
}

// A sampler error is a real failure — describe() already logs and returns.
func TestSampleFramesWithRetry_DoesNotRetryErrors(t *testing.T) {
	boom := errors.New("ffmpeg exploded")
	s := &framesAfter{err: boom}
	w := &waitRecorder{}
	d := newRetryDescriber(s, w)

	if _, err := d.sampleFramesWithRetry(context.Background(), "cam-1", 0, 1000, 4); !errors.Is(err, boom) {
		t.Fatalf("expected the sampler error, got %v", err)
	}
	if s.calls != 1 {
		t.Errorf("expected no retry on error, got %d attempts", s.calls)
	}
}

func TestSampleFramesWithRetry_StopsWhenTheWaitIsCutShort(t *testing.T) {
	s := &framesAfter{emptyCalls: 1000}
	w := &waitRecorder{deny: true}
	d := newRetryDescriber(s, w)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if _, err := d.sampleFramesWithRetry(ctx, "cam-1", 0, 1000, 4); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.calls != 1 {
		t.Errorf("expected to stop after the interrupted wait, got %d attempts", s.calls)
	}
}
