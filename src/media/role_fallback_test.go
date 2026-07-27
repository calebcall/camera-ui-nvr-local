package media

import (
	"errors"
	"testing"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/store"
)

// fakeCoveringFinder records which of the two lookups were called so the
// tests below can assert not just the returned segment but whether the
// fallback was consulted at all — the whole point of the fix is that the
// role-specific lookup stays authoritative when it succeeds.
type fakeCoveringFinder struct {
	roleSeg store.Segment
	roleOK  bool
	roleErr error

	anySeg store.Segment
	anyOK  bool
	anyErr error

	roleCalls int
	anyCalls  int
	gotRole   string
	gotAtMs   int64
}

func (f *fakeCoveringFinder) CoveringSegmentForRole(cameraID, role string, atMs int64) (store.Segment, bool, error) {
	f.roleCalls++
	f.gotRole = role
	f.gotAtMs = atMs
	return f.roleSeg, f.roleOK, f.roleErr
}

func (f *fakeCoveringFinder) CoveringSegment(cameraID string, atMs int64) (store.Segment, bool, error) {
	f.anyCalls++
	return f.anySeg, f.anyOK, f.anyErr
}

func TestCoveringSegmentForRoleOrAny_PrefersTheRequestedRole(t *testing.T) {
	f := &fakeCoveringFinder{
		roleSeg: store.Segment{Path: "/low.mp4", Role: "low-resolution"},
		roleOK:  true,
		anySeg:  store.Segment{Path: "/high.mp4", Role: "high-resolution"},
		anyOK:   true,
	}

	seg, ok, err := coveringSegmentForRoleOrAny(f, "cam1", "low-resolution", 1500)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok || seg.Path != "/low.mp4" {
		t.Fatalf("expected the requested role's segment, got ok=%v path=%q", ok, seg.Path)
	}
	if f.anyCalls != 0 {
		t.Errorf("fallback must not run when the requested role resolves, got %d calls", f.anyCalls)
	}
	if f.gotRole != "low-resolution" || f.gotAtMs != 1500 {
		t.Errorf("role lookup args not passed through: role=%q atMs=%d", f.gotRole, f.gotAtMs)
	}
}

// The production bug: the recorder only ever wrote high-resolution, so a
// playback request for any other role reported "no recording available"
// even though a segment covered that exact moment.
func TestCoveringSegmentForRoleOrAny_FallsBackToAnyRecordedRole(t *testing.T) {
	f := &fakeCoveringFinder{
		roleOK: false,
		anySeg: store.Segment{Path: "/high.mp4", Role: "high-resolution"},
		anyOK:  true,
	}

	seg, ok, err := coveringSegmentForRoleOrAny(f, "cam1", "low-resolution", 1500)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected the covering segment from another role, got not-found")
	}
	if seg.Path != "/high.mp4" {
		t.Errorf("expected the fallback segment, got %q", seg.Path)
	}
	if f.anyCalls != 1 {
		t.Errorf("expected exactly one fallback lookup, got %d", f.anyCalls)
	}
}

func TestCoveringSegmentForRoleOrAny_ReportsGenuineGaps(t *testing.T) {
	f := &fakeCoveringFinder{roleOK: false, anyOK: false}

	_, ok, err := coveringSegmentForRoleOrAny(f, "cam1", "high-resolution", 1500)
	if err != nil {
		t.Fatalf("a gap is not an error, got %v", err)
	}
	if ok {
		t.Fatal("expected not-found when nothing covers the timestamp in any role")
	}
}

func TestCoveringSegmentForRoleOrAny_PropagatesLookupErrors(t *testing.T) {
	roleErr := errors.New("boom")
	f := &fakeCoveringFinder{roleErr: roleErr, anyOK: true}

	if _, _, err := coveringSegmentForRoleOrAny(f, "cam1", "high-resolution", 1500); !errors.Is(err, roleErr) {
		t.Fatalf("expected the role lookup error, got %v", err)
	}
	if f.anyCalls != 0 {
		t.Error("a failed role lookup must not silently fall back")
	}

	anyErr := errors.New("bang")
	f2 := &fakeCoveringFinder{roleOK: false, anyErr: anyErr}
	if _, _, err := coveringSegmentForRoleOrAny(f2, "cam1", "high-resolution", 1500); !errors.Is(err, anyErr) {
		t.Fatalf("expected the fallback lookup error, got %v", err)
	}
}
