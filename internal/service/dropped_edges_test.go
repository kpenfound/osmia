package service

import (
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

// The pane lists the shared graph's edges as the spec names them, and says which
// of them their dependent dropped: only the edge with a waiver in the
// dependent's own trace, never one that merely lacks a merge.
func TestWorkgroupViewNamesTheEdgesTheirDependentDropped(t *testing.T) {
	f := newPublisherFixture(t)
	fixturekit.RecordHanded(t, f.repo, fixturekit.Stream, "stdin", fixturekit.DemoStart)
	dropped := config.WorkstreamID("w_00000000000000000000000000000a01")
	kept := config.WorkstreamID("w_00000000000000000000000000000a02")
	group := &WorkgroupView{ID: "wg", Members: []config.WorkstreamID{dropped, kept, fixturekit.Stream}, Edges: []trace.Edge{
		{From: dropped, To: fixturekit.Stream, Type: trace.EdgeHardBlock},
		{From: kept, To: fixturekit.Stream, Type: trace.EdgeHardBlock},
	}}
	s := &Service{}
	must(t, s.markDroppedEdges(f.repo, fixturekit.Stream, group))
	if len(group.Dropped) != 0 {
		t.Fatalf("an edge nobody dropped is shown as dropped: %+v", group.Dropped)
	}

	recordWaiver(t, f, lifecycle.EdgeWaiver{Upstream: dropped, Type: trace.EdgeHardBlock, Ended: "abandoned", Reason: "dropped", DecidedBy: trace.DecidedByOwner, At: fixturekit.DemoStart.Add(time.Minute)},
		trace.Routing{DecidedBy: trace.DecidedByOwner, DefaultTo: lifecycle.DependencyKept})
	must(t, s.markDroppedEdges(f.repo, fixturekit.Stream, group))
	if len(group.Dropped) != 1 || group.Dropped[0].From != dropped || len(group.Edges) != 2 {
		t.Fatalf("the dropped edge is not the only one marked, or the graph changed: dropped=%+v edges=%+v", group.Dropped, group.Edges)
	}
}
