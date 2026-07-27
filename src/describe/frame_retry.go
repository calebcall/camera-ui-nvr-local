package describe

import (
	"context"
	"time"
)

// Frame sampling resolves footage through the segment index, which only
// contains FINALIZED segments — the recorder deliberately skips the file it is
// still writing. An event is described the moment it ends, so with 60-second
// segments the footage covering it is routinely not indexed for up to a minute
// afterwards, and the first sample comes back empty:
//
//	describe: no frames available for event 0d6a35e5…; nothing to describe
//
// Observed on a live install: an event spanning 13:12:37.954–13:13:07.949 was
// sampled at 13:13:08, when the newest finalized segment ended 0.95s before the
// event started. The two segments covering it were indexed at 13:13:38.
//
// Whether an event got described was therefore luck — it depended on the
// event's start happening to fall inside an already-closed segment. These
// constants trade a bounded wait for that being deterministic instead.
const (
	// frameRetryAttempts includes the first try, so this is one initial sample
	// plus three retries.
	frameRetryAttempts = 4

	// frameRetryDelay times the retries against a 60s segment: three waits
	// span a full segment length, so footage that is going to appear has.
	frameRetryDelay = 20 * time.Second

	// frameRetryReserve is the budget kept back for the model call. The single
	// deadline in describe() covers sampling AND inference on purpose, so
	// retrying until it expires would just turn a missing description into a
	// slower missing description.
	frameRetryReserve = 25 * time.Second
)

// waitFunc sleeps for d, reporting false if ctx ended first. Injected so the
// tests exercise the retry schedule without spending it.
type waitFunc func(ctx context.Context, d time.Duration) bool

func realWait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// sampleFramesWithRetry samples frames for an event, retrying while the result
// is empty because the covering segment may not be indexed yet.
//
// Empty is not an error — footage may never have been recorded, or may already
// have been pruned — so exhausting the retries returns (nil, nil) and the
// caller logs "nothing to describe" exactly as before. A sampler *error* is
// returned immediately: that is a real failure, not a not-yet.
func (d *Describer) sampleFramesWithRetry(
	ctx context.Context,
	cameraID string,
	startMs, endMs int64,
	frameCount int,
) ([][]byte, error) {
	wait := d.wait
	if wait == nil {
		wait = realWait
	}

	for attempt := 1; ; attempt++ {
		frames, err := d.frames.SampleFrames(ctx, cameraID, startMs, endMs, frameCount)
		if err != nil {
			return nil, err
		}
		if len(frames) > 0 {
			return frames, nil
		}
		if attempt >= frameRetryAttempts {
			return nil, nil
		}
		// Only wait if the model call would still have room afterwards.
		if deadline, ok := ctx.Deadline(); ok {
			if time.Until(deadline) < frameRetryDelay+frameRetryReserve {
				return nil, nil
			}
		}
		if !wait(ctx, frameRetryDelay) {
			return nil, nil
		}
	}
}
