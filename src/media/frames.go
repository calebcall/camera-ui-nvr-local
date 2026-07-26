// Multi-frame sampling for AI event descriptions: pull several JPEGs spread
// across a detection event's time window out of the recorded segments behind
// it, so a vision model can be shown how a scene developed rather than a
// single frozen instant.
//
// This lives in media, alongside Generator (thumbs.go), Scrubber (scrub.go)
// and Player (playback.go), because it is the same "find the segment covering
// this moment, exec ffmpeg against it at the right offset" shape all three
// already have — and because keeping every ffmpeg invocation in this one
// package is what lets src/describe stay purely about prompts and HTTP. Like
// the rest of the package it never resolves the ffmpeg binary itself; callers
// pass in the already-resolved path (recorder.ResolveFFmpegSDK/ResolveFFmpeg).
package media

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	sdk "github.com/cameraui/sdk/go"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/store"
)

// maxSampledFrameWidth caps the width of every sampled frame, preserving
// aspect ratio.
//
// This is the single most effective cost control in the AI-description
// feature: image tokens dominate a vision request's input cost and they scale
// with pixel count, so halving a frame's width quarters what it costs. 768px
// is wide enough for a model to read a scene — how many people, what they're
// carrying, a vehicle's colour — and small enough that four frames per event
// stay cheap. Cameras narrower than this are left alone rather than upscaled
// (see sampleFrameArgs' min()), since upscaling would buy nothing but tokens.
const maxSampledFrameWidth = 768

// sampleFrameTimeout bounds a single ffmpeg frame-extraction invocation, for
// the same reason defaultGenerateTimeout (thumbs.go) and scrubTimeout
// (scrub.go) do: sampling runs on a serial work queue behind an inference
// call that already has its own deadline, and a hung ffmpeg process must cost
// one frame rather than the whole description attempt's budget.
const sampleFrameTimeout = 8 * time.Second

// FrameSampler extracts up to N downscaled JPEG frames spread across a time
// window, out of whichever recorded segments cover it. Safe for concurrent
// use: each call works in its own temp directory and shares only immutable
// fields.
type FrameSampler struct {
	ffmpegPath string
	segments   SegmentFinder
	timeout    time.Duration
	log        *sdk.Logger
	runner     commandRunner
}

// NewFrameSampler returns a FrameSampler that finds segments via segments and
// execs ffmpegPath (the already-resolved ffmpeg binary — see the package
// comment above; this package never resolves it itself). log may be nil.
func NewFrameSampler(ffmpegPath string, segments SegmentFinder, log *sdk.Logger) *FrameSampler {
	return &FrameSampler{
		ffmpegPath: ffmpegPath,
		segments:   segments,
		timeout:    sampleFrameTimeout,
		log:        log,
		runner:     execCommandRunner{},
	}
}

// SampleFrames returns up to n JPEG frames taken at evenly spaced timestamps
// across [startMs, endMs] inclusive, oldest first, bounded by ctx.
//
// It is deliberately forgiving. A sample point no recording covers, an ffmpeg
// failure, or an ffmpeg exit that produced no bytes each skip that one frame
// and continue: a single unreadable moment must not cost the caller every
// other frame in the window. Returning zero frames is therefore NOT an error
// — the caller decides what an empty result means (for src/describe:
// "nothing to describe, abandon this event"). The only error returned is a
// failure to create the scratch directory, which makes the whole operation
// impossible rather than partially degraded.
//
// n is clamped to at least 1, and to exactly 1 when the window has no
// duration (endMs <= startMs — e.g. an event whose EndTime never arrived), in
// which case the single sample is taken at startMs. Sampling a zero-length
// window n times would just extract the same frame n times at n times the
// cost.
func (s *FrameSampler) SampleFrames(ctx context.Context, cameraID string, startMs, endMs int64, n int) ([][]byte, error) {
	if n < 1 || endMs <= startMs {
		n = 1
	}

	dir, err := os.MkdirTemp("", "nvr-frames-")
	if err != nil {
		return nil, fmt.Errorf("media: create temp frame dir: %w", err)
	}
	defer os.RemoveAll(dir)

	frames := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		// sampleTimestamp (scrub.go) is shared with PreviewFrames rather
		// than reimplemented here: the "i-th of n, inclusive of both ends"
		// arithmetic is unit-agnostic, so feeding it milliseconds gives
		// milliseconds back.
		atMs := sampleTimestamp(startMs, endMs, i, n)

		seg, ok, err := s.segments.CoveringSegment(cameraID, atMs)
		if err != nil {
			s.logf("media: sample frame %d/%d for %s: find covering segment: %v", i+1, n, cameraID, err)
			continue
		}
		if !ok {
			// Expected: no recording covers this instant (a window spanning
			// a recording gap, or reaching into a segment ffmpeg hasn't
			// finalized yet). Not logged, exactly as Generator treats it.
			continue
		}

		outPath := filepath.Join(dir, fmt.Sprintf("frame-%02d.jpg", i))
		data, err := s.extractFrame(ctx, seg, atMs, outPath)
		if err != nil {
			s.logf("media: sample frame %d/%d for %s: %v", i+1, n, cameraID, err)
			continue
		}
		frames = append(frames, data)
	}

	return frames, nil
}

// extractFrame runs one ffmpeg invocation (bounded by s.timeout on top of
// ctx) and reads back the JPEG it wrote, reporting an empty output file as an
// error of its own: ffmpeg can exit zero having produced nothing when a seek
// lands somewhere it can't decode, and an empty "frame" handed to a vision
// model is worse than no frame at all.
func (s *FrameSampler) extractFrame(ctx context.Context, seg store.Segment, atMs int64, outPath string) ([]byte, error) {
	runCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	if err := s.runner.Run(runCtx, s.ffmpegPath, sampleFrameArgs(seg, atMs, outPath)); err != nil {
		return nil, fmt.Errorf("ffmpeg extract frame from %s: %w", seg.Path, err)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		return nil, fmt.Errorf("read extracted frame %s: %w", outPath, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("ffmpeg produced an empty frame file %s", outPath)
	}
	return data, nil
}

// sampleFrameArgs builds the ffmpeg args pulling one downscaled JPEG out of
// seg at the offset atMs falls at within it. Same shape as extractFrameArgs
// (thumbs.go) — including -ss before -i for fast input seeking, which
// frameOffsetSeconds' clamping makes safe — plus the -vf scale filter that
// keeps request cost down. The two are kept as separate functions rather than
// parameterizing extractFrameArgs with a scale so the notification-thumbnail
// path stays exactly as it was.
//
// The scale expression is `scale=min(<w>\,iw):-2`:
//
//   - min(w, iw) caps the output width at w while leaving a narrower source
//     at its own width, so nothing is ever upscaled (see
//     maxSampledFrameWidth).
//   - -2 lets ffmpeg derive the height from the source's aspect ratio,
//     rounded to a multiple of two as the JPEG encoder's chroma subsampling
//     requires.
//   - The comma is BACKSLASH-ESCAPED, not quoted. A filtergraph splits on
//     unescaped commas, so a bare one would be read as two filters —
//     `scale=min(768` and `iw):-2` — and ffmpeg would reject the graph,
//     failing every extraction. Escaping (rather than the `'min(768,iw)'`
//     quoting ffmpeg also accepts) keeps this correct with one fewer parsing
//     layer involved, which matters because these args go straight to execve:
//     there is no shell here to strip quotes, so any quote characters would
//     reach ffmpeg literally and rely on ffmpeg's own quote handling.
func sampleFrameArgs(seg store.Segment, atMs int64, outPath string) []string {
	return []string{
		"-ss", strconv.FormatFloat(frameOffsetSeconds(seg, atMs), 'f', 3, 64),
		"-i", seg.Path,
		"-frames:v", "1",
		"-vf", fmt.Sprintf(`scale=min(%d\,iw):-2`, maxSampledFrameWidth),
		"-q:v", jpegQuality,
		"-y",
		outPath,
	}
}

// logf logs through s.log if one was provided (log may be nil — see
// NewFrameSampler's doc comment).
func (s *FrameSampler) logf(format string, args ...any) {
	if s.log == nil {
		return
	}
	s.log.Log(fmt.Sprintf(format, args...))
}
