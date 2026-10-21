package service

import (
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

// crossRepoRatifyFixture is two active projects in one service, each with
// its own clone and charter, ready for the architect to draft a cross-repo
// workgroup's project spec and for one owner ratify to seal it.
type crossRepoRatifyFixture struct {
	*architectFixture
	project2         config.ProjectID
	stream1, stream2 config.WorkstreamID
}

// scriptDrafts installs the fake architect's behaviour for both members'
// own draft turns, routed by the workspace path's workstream, and for the
// workgroup's project-spec draft turn.
func (f *crossRepoRatifyFixture) scriptDrafts(t *testing.T) {
	t.Helper()
	intakefixture.ScriptCrossRepoDrafts(t, f.engine, func() (config.WorkstreamID, config.WorkstreamID) { return f.stream1, f.stream2 })
}

// writeCorruptedProjectSpec bypasses graph validation to simulate corrupt storage.
func writeCorruptedProjectSpec(t *testing.T, wt *trace.Repository, workgroup config.WorkgroupID, spec trace.ProjectSpec) {
	t.Helper()
	fixturekit.CorruptProjectSpec(t, wt, workgroup, spec, ownerActor)
}
