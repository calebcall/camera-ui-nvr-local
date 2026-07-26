package media

import (
	"bytes"
	"context"
	"errors"
	"image/jpeg"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/store"
)

// fakeSampleRunner records every ffmpeg invocation and, by default, writes a
// non-empty stub file at the output path (always the last argument) so
// SampleFrames' read-back succeeds without a real ffmpeg process. failCalls
// and emptyCalls make a specific invocation (by zero-based index) fail
// outright, or "succeed" while producing nothing — the two ways a real ffmpeg
// leaves a sample point with no usable frame.
//
// Distinct from thumbs_test.go's failingRunner, which exists to prove ffmpeg
// is NOT invoked at all; here the invocations themselves are the assertions.
type fakeSampleRunner struct {
	mu         sync.Mutex
	calls      [][]string
	failCalls  map[int]bool
	emptyCalls map[int]bool
}

func (f *fakeSampleRunner) Run(ctx context.Context, name string, args []string) error {
	f.mu.Lock()
	idx := len(f.calls)
	f.calls = append(f.calls, args)
	f.mu.Unlock()

	if f.failCalls[idx] {
		return errors.New("fake ffmpeg failure")
	}
	if f.emptyCalls[idx] {
		return nil
	}
	return os.WriteFile(args[len(args)-1], []byte("jpeg-bytes"), 0o644)
}

// fakeSegments is a SegmentFinder returning one covering segment for every
// timestamp except those listed in absent, or err for all of them. The rest
// of this package's tests drive real stores (see newTestStores), which is
// still what the real-ffmpeg tests below do; a fake is used for the
// arithmetic/skip cases because it makes each sample point's covering segment
// explicit, and because a lookup *error* can't be provoked from a healthy
// SQLite store at all.
type fakeSegments struct {
	absent map[int64]bool
	err    error
}

func (f fakeSegments) CoveringSegment(cameraID string, atMs int64) (store.Segment, bool, error) {
	if f.err != nil {
		return store.Segment{}, false, f.err
	}
	if f.absent[atMs] {
		return store.Segment{}, false, nil
	}
	return store.Segment{CameraID: cameraID, Path: "/tmp/seg.mp4", StartMs: 0, EndMs: 60_000}, true, nil
}

// newFakeSampler returns a FrameSampler wired to fake segment lookup and a
// fake ffmpeg, for the cases that need neither.
func newFakeSampler(t *testing.T, segs SegmentFinder, runner commandRunner) *FrameSampler {
	t.Helper()
	s := NewFrameSampler("ffmpeg", segs, nil)
	s.runner = runner
	return s
}

// TestSampleFrames_ReturnsNFramesEvenlySpaced proves the requested number of
// frames comes back, sampled at evenly spaced timestamps inclusive of both
// ends of the window — the point of sampling several frames at all is that
// they show how a scene developed, which only holds if they're spread across
// the event rather than clustered.
func TestSampleFrames_ReturnsNFramesEvenlySpaced(t *testing.T) {
	runner := &fakeSampleRunner{}
	s := newFakeSampler(t, fakeSegments{}, runner)

	frames, err := s.SampleFrames(context.Background(), "cam1", 10_000, 20_000, 3)
	if err != nil {
		t.Fatalf("SampleFrames: %v", err)
	}
	if len(frames) != 3 {
		t.Fatalf("got %d frames, want 3", len(frames))
	}
	for i, f := range frames {
		if len(f) == 0 {
			t.Errorf("frame %d is empty", i)
		}
	}

	// The fake's segment starts at 0ms, so the -ss offsets are the sample
	// timestamps themselves: 10.000s, 15.000s, 20.000s.
	if len(runner.calls) != 3 {
		t.Fatalf("got %d ffmpeg calls, want 3", len(runner.calls))
	}
	for i, want := range []string{"10.000", "15.000", "20.000"} {
		if got := runner.calls[i][1]; got != want {
			t.Errorf("call %d -ss = %q, want %q", i, got, want)
		}
	}
}

// TestSampleFrames_SingleFrameOrDegenerateWindow_SamplesStartMs proves the
// collapse cases all resolve to exactly one frame taken at startMs: n == 1,
// and a window with no duration (an event whose EndTime never arrived, or
// arrived garbled) — never zero frames, and never a descending sweep
// backwards out of the window.
func TestSampleFrames_SingleFrameOrDegenerateWindow_SamplesStartMs(t *testing.T) {
	for _, tc := range []struct {
		name           string
		startMs, endMs int64
		n              int
	}{
		{"n is one", 5000, 20_000, 1},
		{"n is zero", 5000, 5000, 0},
		{"end equals start", 5000, 5000, 4},
		{"end before start", 5000, 1000, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeSampleRunner{}
			s := newFakeSampler(t, fakeSegments{}, runner)

			frames, err := s.SampleFrames(context.Background(), "cam1", tc.startMs, tc.endMs, tc.n)
			if err != nil {
				t.Fatalf("SampleFrames: %v", err)
			}
			if len(frames) != 1 {
				t.Fatalf("got %d frames, want 1", len(frames))
			}
			if len(runner.calls) != 1 {
				t.Fatalf("got %d ffmpeg calls, want 1", len(runner.calls))
			}
			if got, want := runner.calls[0][1], "5.000"; got != want {
				t.Errorf("-ss = %q, want %q", got, want)
			}
		})
	}
}

// TestSampleFrames_NoCoveringSegment_SkipsTimestamp proves a sample point
// that no recording covers (an event window spanning a recording gap) costs
// only that one frame, not the whole call.
func TestSampleFrames_NoCoveringSegment_SkipsTimestamp(t *testing.T) {
	runner := &fakeSampleRunner{}
	// Window 0..20000 with n=3 samples 0, 10000, 20000; drop the middle one.
	s := newFakeSampler(t, fakeSegments{absent: map[int64]bool{10_000: true}}, runner)

	frames, err := s.SampleFrames(context.Background(), "cam1", 0, 20_000, 3)
	if err != nil {
		t.Fatalf("SampleFrames must not fail when a segment is missing: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2 (the middle one skipped)", len(frames))
	}
	if len(runner.calls) != 2 {
		t.Errorf("got %d ffmpeg calls, want 2 (the uncovered point must not exec ffmpeg)", len(runner.calls))
	}
}

// TestSampleFrames_SegmentLookupError_SkipsTimestamp proves a failing segment
// lookup is treated like any other unusable sample point — logged and skipped
// — rather than aborting the whole window.
func TestSampleFrames_SegmentLookupError_SkipsTimestamp(t *testing.T) {
	runner := &fakeSampleRunner{}
	s := newFakeSampler(t, fakeSegments{err: errors.New("database is locked")}, runner)

	frames, err := s.SampleFrames(context.Background(), "cam1", 0, 20_000, 3)
	if err != nil {
		t.Fatalf("SampleFrames must not surface a segment lookup error: %v", err)
	}
	if len(frames) != 0 {
		t.Errorf("got %d frames, want 0", len(frames))
	}
	if len(runner.calls) != 0 {
		t.Errorf("got %d ffmpeg calls, want 0", len(runner.calls))
	}
}

// TestSampleFrames_FailedOrEmptyExtraction_SkipsFrame proves the two ffmpeg
// failure modes — a non-zero exit, and an exit-zero that produced no bytes —
// each cost one frame and nothing more.
func TestSampleFrames_FailedOrEmptyExtraction_SkipsFrame(t *testing.T) {
	runner := &fakeSampleRunner{
		failCalls:  map[int]bool{0: true},
		emptyCalls: map[int]bool{1: true},
	}
	s := newFakeSampler(t, fakeSegments{}, runner)

	frames, err := s.SampleFrames(context.Background(), "cam1", 0, 20_000, 3)
	if err != nil {
		t.Fatalf("SampleFrames: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1 (one ffmpeg error, one empty output)", len(frames))
	}
}

// TestSampleFrames_NothingUsable_ReturnsNoFramesWithoutError pins the
// contract callers depend on: zero frames is an outcome, not an error, so
// src/describe decides for itself what "nothing to describe" means.
func TestSampleFrames_NothingUsable_ReturnsNoFramesWithoutError(t *testing.T) {
	runner := &fakeSampleRunner{}
	s := newFakeSampler(t, fakeSegments{absent: map[int64]bool{0: true, 10_000: true, 20_000: true}}, runner)

	frames, err := s.SampleFrames(context.Background(), "cam1", 0, 20_000, 3)
	if err != nil {
		t.Fatalf("zero frames is not an error: %v", err)
	}
	if len(frames) != 0 {
		t.Errorf("got %d frames, want 0", len(frames))
	}
}

// TestSampleFrames_RemovesItsTempDir proves the scratch directory each call
// extracts into is gone by the time it returns — this runs once per detection
// event, forever, so a leak here would slowly fill /tmp on a busy site.
func TestSampleFrames_RemovesItsTempDir(t *testing.T) {
	runner := &fakeSampleRunner{}
	s := newFakeSampler(t, fakeSegments{}, runner)

	if _, err := s.SampleFrames(context.Background(), "cam1", 0, 1000, 2); err != nil {
		t.Fatalf("SampleFrames: %v", err)
	}
	if len(runner.calls) == 0 {
		t.Fatal("no ffmpeg calls recorded, so there is no temp path to check")
	}

	call := runner.calls[0]
	dir := filepath.Dir(call[len(call)-1])
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("temp dir %s still exists after SampleFrames returned (stat err = %v)", dir, err)
	}
}

// TestSampleFrameArgs_BuildsClampedSeekAndEscapedScaleFilter pins the exact
// ffmpeg argument vector, because two things about it are easy to break
// silently and impossible to notice from a passing string-contains check:
//
//   - the -vf value's comma is escaped (`min(768\,iw)`). A bare comma splits
//     the filtergraph into two filters, `scale=min(768` and `iw):-2`, which
//     ffmpeg rejects — meaning every extraction in production would fail
//     while a test asserting only that "scale=" appears somewhere still
//     passed.
//   - -ss comes first (input seeking) and is clamped into the segment by
//     frameOffsetSeconds: 1500ms into a segment starting at 1000ms is 0.5s.
func TestSampleFrameArgs_BuildsClampedSeekAndEscapedScaleFilter(t *testing.T) {
	seg := store.Segment{Path: "/rec/segment.mp4", StartMs: 1000, EndMs: 3000}

	got := sampleFrameArgs(seg, 1500, "/tmp/frames/frame-00.jpg")
	want := []string{
		"-ss", "0.500",
		"-i", "/rec/segment.mp4",
		"-frames:v", "1",
		"-vf", `scale=min(768\,iw):-2`,
		"-q:v", jpegQuality,
		"-y",
		"/tmp/frames/frame-00.jpg",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sampleFrameArgs() =\n\t%q\nwant\n\t%q", got, want)
	}
}

// TestSampleFrames_ScaleFilterCapsWidthWithoutUpscaling is the real-media
// proof that the -vf expression above is not just well-formed text but
// something ffmpeg actually accepts and acts on: genuine fMP4 fixtures are
// indexed and sampled with the real binary, and the returned bytes are
// decoded as JPEGs to check their pixel dimensions.
//
// Both directions matter. A source wider than the cap must come back at
// exactly maxSampledFrameWidth (image tokens dominate request cost, and they
// scale with pixel count). A source narrower than the cap must come back
// untouched — min(w,iw) exists precisely so a 320-wide camera isn't upscaled
// into paying for pixels it never recorded.
func TestSampleFrames_ScaleFilterCapsWidthWithoutUpscaling(t *testing.T) {
	requireFFmpeg(t)

	for _, tc := range []struct {
		name         string
		size         string
		wantW, wantH int
		fixtureStart int64
	}{
		{"wider than the cap is downscaled", "1280x720", maxSampledFrameWidth, 432, 4_000_000},
		{"narrower than the cap is left alone", "320x240", 320, 240, 5_000_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seg := genFixtureSegmentSize(t, t.TempDir(), tc.fixtureStart, 2, tc.size)

			segments, _ := newTestStores(t)
			if _, err := segments.Add(seg); err != nil {
				t.Fatalf("segments.Add: %v", err)
			}

			sampler := NewFrameSampler(resolvedFFmpegPath(), segments, nil)
			frames, err := sampler.SampleFrames(context.Background(), seg.CameraID,
				tc.fixtureStart+200, tc.fixtureStart+1200, 2)
			if err != nil {
				t.Fatalf("SampleFrames: %v", err)
			}
			if len(frames) != 2 {
				t.Fatalf("got %d frames, want 2", len(frames))
			}

			for i, f := range frames {
				if len(f) < 2 || f[0] != jpegMagic[0] || f[1] != jpegMagic[1] {
					t.Fatalf("frame %d does not start with JPEG magic 0xFFD8", i)
				}
				cfg, err := jpeg.DecodeConfig(bytes.NewReader(f))
				if err != nil {
					t.Fatalf("frame %d is not a decodable JPEG: %v", i, err)
				}
				if cfg.Width != tc.wantW || cfg.Height != tc.wantH {
					t.Errorf("frame %d is %dx%d, want %dx%d", i, cfg.Width, cfg.Height, tc.wantW, tc.wantH)
				}
			}
		})
	}
}
