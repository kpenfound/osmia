package lifecycle_test

import (
	"testing"

	"github.com/kpenfound/osmia/internal/trace"
)

// TestHandInCreatesAOneWorkstreamWorkgroup shows that a one-repo hand-in
// creates a one-workstream workgroup with the handed document as its
// brief, whose chief of staff is the workstream's own thread: the same
// owner steps as today reach it, and no new approval is added. The rest of
// the flow from here (ratify, accept the final review, approve delivery)
// is unchanged existing machinery, run by the many other hand-in tests in
// this package.
func TestHandInCreatesAOneWorkstreamWorkgroup(t *testing.T) {
	t.Parallel()
	f := newHandInFixture(t)
	text := "# Resumable uploads\n\nDo the thing.\n"
	out := f.handIn(t, HandInRequest{Key: "wg", Stdin: &text})

	groups, _, err := trace.Workgroups(f.repository())
	must(t, err)
	g, ok := trace.WorkgroupOf(groups, out.Workstream)
	if !ok || len(g.Members) != 1 || g.Members[0] != out.Workstream {
		t.Fatalf("workgroup of handed-in workstream %s: %+v ok=%v", out.Workstream, g, ok)
	}

	// The brief is the handed document itself: the fast path needs no
	// separate brief document, and the workgroup's chief of staff is the
	// one-workstream workgroup's own thread.
	host, err := f.repository().ChiefOfStaffHost(out.Workstream)
	must(t, err)
	if host != out.Workstream {
		t.Fatalf("a one-workstream workgroup's chief of staff should be the workstream's own thread, got host %s", host)
	}

	// A second hand-in of a different key never joins the first's
	// workgroup: each fast-path request gets its own.
	firstGroupID := g.ID
	other := "# Another feature\n"
	second := f.handIn(t, HandInRequest{Key: "wg-2", Stdin: &other})
	groups, _, err = trace.Workgroups(f.repository())
	must(t, err)
	if g, ok := trace.WorkgroupOf(groups, second.Workstream); !ok || len(g.Members) != 1 || g.ID == firstGroupID {
		t.Fatalf("second hand-in should get its own one-workstream workgroup: %+v", g)
	}
}
