package journey

import (
	context "context"
	testing "testing"

	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	workspace "github.com/kpenfound/osmia/internal/workspace"
)

func DriftRecords(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) []service.DriftRebase {
	t.Helper()
	return intakefixture.DriftRecords[service.DriftRebase](t, repository, stream, lifecycle.DriftDocument)
}

func FileAt(t *testing.T, f *ShedFixture, commit, path string) string {
	t.Helper()
	return intakefixture.FileAt(t, f.Clone, commit, path)
}

func ParentOf(t *testing.T, f *ShedFixture, commit string) string {
	t.Helper()
	return intakefixture.ParentOf(t, f.Clone, commit)
}

func SettleOperation(t *testing.T, s *Service, repository *trace.Repository, stream config.WorkstreamID, op coreadapter.Operation, r coreadapter.Reconciler) coreadapter.OperationResult {
	t.Helper()
	return intakefixture.SettleOperation(t, s.Now(), execution.ForemanActor, repository, stream, op, r)
}

func RequestDrift(t *testing.T, d Drifter, stream config.WorkstreamID) coreadapter.Operation {
	t.Helper()
	return intakefixture.RequestDrift(t, d.Repository, stream, service.DriftAction, d.RequestDrifts, intakefixture.TestDrift)
}

var (
	ApprovedResolution = service.UnitVerdict{Decision: "satisfactory", Summary: fixturekit.ReviewSummary}
)

func AwaitResolution(t *testing.T, s *Service, d Drifter, stream config.WorkstreamID, op coreadapter.Operation, want string) {
	t.Helper()
	intakefixture.AwaitResolution(t, func() (coreadapter.OperationResult, error) {
		return AttemptOperation(t, s, d.Repository, stream, op, d)
	}, lifecycle.ErrDriftAwaits, want)
}

func CompleteDriftTurn(t *testing.T, f *ShedFixture, repository *trace.Repository, stream config.WorkstreamID, agent string, outcome *coreadapter.Outcome) trace.QueuedTurn {
	t.Helper()
	return intakefixture.CompleteDriftTurn(t, f.SharedTurns(), repository, stream, agent, outcome)
}

func ConflictedDrift(t *testing.T, f *ShedFixture, d Drifter, stream config.WorkstreamID) (string, string, coreadapter.Operation) {
	t.Helper()
	return intakefixture.ConflictedDrift(t,
		func(files map[string]string) string { return MoveFeature(t, f, stream, files) },
		func(files map[string]string) string { return AdvanceUpstream(t, f, files) },
		func() coreadapter.Operation { return RequestDrift(t, d, stream) })
}

func DriftResolutionDiff(t *testing.T, f *ShedFixture, repository *trace.Repository, stream config.WorkstreamID) string {
	t.Helper()
	return intakefixture.DriftResolutionDiff(t, func() (coreadapter.Tool, error) {
		return lifecycle.DriftDiffTool(context.Background(), f.S.Cfg, repository, coreadapter.Scope{Workstream: string(stream), Thread: intake.DriftReviewerAgent, Turn: lifecycle.DriftReviewTurnID(1, 1), Role: intake.ReviewerRole})
	})
}

func DriftVerdict(t *testing.T, verdict service.UnitVerdict) *coreadapter.Outcome {
	t.Helper()
	return intakefixture.VerdictOutcome(t, verdict, execution.VerdictOutcome)
}

func FeatureTip(t *testing.T, f *ShedFixture, stream config.WorkstreamID) string {
	t.Helper()
	return intakefixture.BranchTip(t, intake.Workspaces(f.S.Cfg, intake.BranchesDirectory, BackendOf(t, f, stream)), intake.FeatureBranch(stream))
}

func ResolutionWorkspace(t *testing.T, f *ShedFixture, stream config.WorkstreamID) workspace.Worktree {
	t.Helper()
	return intakefixture.Workspace(t, intake.Workspaces(f.S.Cfg, intake.DriftsDirectory, BackendOf(t, f, stream)), stream)
}

func ResolvedDone(outcome string) *coreadapter.Outcome {
	return intakefixture.ResolvedDone(execution.MasonDone, service.MasonReport{Outcome: outcome})
}

func AdvanceUpstream(t *testing.T, f *ShedFixture, files map[string]string) string {
	t.Helper()
	return intakefixture.AdvanceUpstream(t, f.Clone, files)
}

// Jujutsu locates the fixture's Jujutsu-backed workspaces.
func (f *ShedFixture) Jujutsu() intakefixture.JujutsuClone {
	return intakefixture.JujutsuClone{Root: f.S.Cfg.Root, Project: f.S.Cfg.Project.ID, Clone: f.Clone}
}

// StopAt makes the service stop at the named step with an error and returns
// the context the stopped work runs under.
func (s *Service) StopAt(t *testing.T, step string) context.Context {
	t.Helper()
	return intakefixture.StopAt(t, step, func(h func(string) error) { s.Boundary = h })
}
