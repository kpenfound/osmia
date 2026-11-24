package lifecycle_test

import (
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestHandInCreatesAOneWorkstreamWorkgroup(t *testing.T) {
	t.Parallel()
	f := newHandInFixture(t)
	text := "# Resumable uploads\n\nDo the thing.\n"
	out := f.handIn(t, api.HandInRequest{Key: "wg", Stdin: &text})

	groups, _, err := trace.Workgroups(f.repository())
	fixturekit.Must(t, err)
	g, ok := trace.WorkgroupOf(groups, out.Workstream)
	if !ok || len(g.Members) != 1 || g.Members[0] != out.Workstream {
		t.Fatalf("workgroup of handed-in workstream %s: %+v ok=%v", out.Workstream, g, ok)
	}

	host, err := f.repository().ChiefOfStaffHost(out.Workstream)
	fixturekit.Must(t, err)
	if host != out.Workstream {
		t.Fatalf("a one-workstream workgroup's chief of staff should be the workstream's own thread, got host %s", host)
	}

	firstGroupID := g.ID
	other := "# Another feature\n"
	second := f.handIn(t, api.HandInRequest{Key: "wg-2", Stdin: &other})
	groups, _, err = trace.Workgroups(f.repository())
	fixturekit.Must(t, err)
	if g, ok := trace.WorkgroupOf(groups, second.Workstream); !ok || len(g.Members) != 1 || g.ID == firstGroupID {
		t.Fatalf("second hand-in should get its own one-workstream workgroup: %+v", g)
	}
}
