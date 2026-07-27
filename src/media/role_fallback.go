package media

import (
	"fmt"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/store"
)

// coveringSegmentFinder is the pair of lookups the role fallback needs:
// the role-scoped one the frontend's sourceRole selects, plus the
// role-agnostic one used when that role was never recorded.
// *store.SegmentStore satisfies both directly.
type coveringSegmentFinder interface {
	CoveringSegmentForRole(cameraID, role string, atMs int64) (store.Segment, bool, error)
	CoveringSegment(cameraID string, atMs int64) (store.Segment, bool, error)
}

// coveringSegmentForRoleOrAny returns the segment covering atMs for
// cameraID, preferring role but accepting any recorded role rather than
// reporting nothing.
//
// The recorder writes only the roles a camera is configured for — by
// default just high-resolution (recorder/manager.go's defaultRoles) — while
// the frontend's quality selector offers every role the camera's *sources*
// advertise. Matching strictly on role therefore made every option except
// the recorded one report "no recording available" for moments that are
// fully covered on disk. Falling back keeps the selector honest: the
// requested role wins whenever it exists, and otherwise the viewer gets the
// footage that was actually recorded (CoveringSegment already ranks
// high > mid > low > snapshot) instead of an empty player.
//
// ok is false, with no error, only when nothing covers atMs in any role —
// a genuine gap (before recording started, or a still-open segment ffmpeg
// hasn't finalized yet), which callers must treat as "skip gracefully".
func coveringSegmentForRoleOrAny(
	finder coveringSegmentFinder,
	cameraID, role string,
	atMs int64,
) (store.Segment, bool, error) {
	seg, ok, err := finder.CoveringSegmentForRole(cameraID, role, atMs)
	if err != nil {
		return store.Segment{}, false, fmt.Errorf("media: find covering segment for role %q: %w", role, err)
	}
	if ok {
		return seg, true, nil
	}

	seg, ok, err = finder.CoveringSegment(cameraID, atMs)
	if err != nil {
		return store.Segment{}, false, fmt.Errorf("media: find covering segment in any role: %w", err)
	}
	return seg, ok, nil
}
