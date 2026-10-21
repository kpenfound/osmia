package fixturekit

import (
	"context"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// EdgeGraph records two members and their project-spec edge in a fresh trace.
// The caller owns the repository until the test's cleanup closes it.
func EdgeGraph(t *testing.T, root config.Root, project config.Project, edgeType string, at time.Time, registration, owner trace.Actor) (*trace.Repository, config.WorkstreamID, config.WorkstreamID) {
	t.Helper()
	ctx := context.Background()
	repository, err := trace.Open(root, project)
	Must(t, err)
	t.Cleanup(func() { repository.Close() })
	upstream := config.WorkstreamID("w_00000000000000000000000000000a01")
	downstream := config.WorkstreamID("w_00000000000000000000000000000a02")
	Must(t, repository.CreateWorkstream(ctx, upstream, at, registration))
	Must(t, repository.CreateWorkstream(ctx, downstream, at, registration))
	g, err := trace.CreateWorkgroup(ctx, repository, []config.WorkstreamID{upstream, downstream}, at, owner, "fixture")
	Must(t, err)
	spec := trace.ProjectSpec{Behavior: "Downstream depends on upstream.", Graph: []trace.Edge{{From: upstream, To: downstream, Type: edgeType}}}
	Must(t, trace.RecordProjectSpec(ctx, repository, g, spec, at, owner, "fixture"))
	return repository, upstream, downstream
}
