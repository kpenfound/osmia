package service

import (
	"testing"

	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

func attemptOperation(t *testing.T, s *Service, repository *trace.Repository, stream config.WorkstreamID, op coreadapter.Operation, r coreadapter.Reconciler) (coreadapter.OperationResult, error) {
	t.Helper()
	return intakefixture.AttemptOperation(t, s.now(), foremanActor, repository, stream, op, r)
}

func awaitResolution(t *testing.T, s *Service, d drifter, stream config.WorkstreamID, op coreadapter.Operation, want string) {
	t.Helper()
	intakefixture.AwaitResolution(t, func() (coreadapter.OperationResult, error) {
		return attemptOperation(t, s, d.repository, stream, op, d)
	}, lifecycle.ErrDriftAwaits, want)
}

func completeDriftTurn(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, agent string, outcome *coreadapter.Outcome) trace.QueuedTurn {
	t.Helper()
	return intakefixture.CompleteDriftTurn(t, f.sharedTurns(), repository, stream, agent, outcome)
}

func resolvedDone(outcome string) *coreadapter.Outcome {
	return intakefixture.ResolvedDone(execution.MasonDone, MasonReport{Outcome: outcome})
}

var (
	approvedResolution = UnitVerdict{Decision: "satisfactory", Summary: fixturekit.ReviewSummary}
)

func resolutionWorkspace(t *testing.T, f *shedFixture, stream config.WorkstreamID) workspace.Worktree {
	t.Helper()
	return intakefixture.Workspace(t, workspaces(f.s.cfg, driftsDirectory, backendOf(t, f, stream)), stream)
}

func featureTip(t *testing.T, f *shedFixture, stream config.WorkstreamID) string {
	t.Helper()
	return intakefixture.BranchTip(t, workspaces(f.s.cfg, branchesDirectory, backendOf(t, f, stream)), featureBranch(stream))
}

func conflictedDrift(t *testing.T, f *shedFixture, d drifter, stream config.WorkstreamID) (string, string, coreadapter.Operation) {
	t.Helper()
	return intakefixture.ConflictedDrift(t,
		func(files map[string]string) string { return moveFeature(t, f, stream, files) },
		func(files map[string]string) string { return advanceUpstream(t, f, files) },
		func() coreadapter.Operation { return requestDrift(t, d, stream) })
}
