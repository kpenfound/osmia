package executionjourney

import (
	context "context"
	json "encoding/json"
	errors "errors"
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
	filepath "path/filepath"
	strings "strings"
	testing "testing"
)

func driftRecords(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) []service.DriftRebase {
	t.Helper()
	return intakefixture.DriftRecords[service.DriftRebase](t, repository, stream, lifecycle.DriftDocument)
}

func fileAt(t *testing.T, f *shedFixture, commit, path string) string {
	t.Helper()
	return fixturekit.DemoGit(t, filepath.Dir(f.clone), "-C", f.clone, "show", commit+":"+path)
}

func parentOf(t *testing.T, f *shedFixture, commit string) string {
	t.Helper()
	return strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(f.clone), "-C", f.clone, "rev-parse", commit+"^"))
}

func settleOperation(t *testing.T, s *Service, repository *trace.Repository, stream config.WorkstreamID, op coreadapter.Operation, r coreadapter.Reconciler) coreadapter.OperationResult {
	t.Helper()
	return intakefixture.SettleOperation(t, s.now(), execution.ForemanActor, repository, stream, op, r)
}

func requestDrift(t *testing.T, d drifter, stream config.WorkstreamID) coreadapter.Operation {
	t.Helper()
	return intakefixture.RequestDrift(t, d.repository, stream, service.DriftAction, d.requestDrifts, testDrift)
}

func testDrift(config.WorkstreamID) (string, error) { return "the test asks for a drift rebase", nil }

var (
	approvedResolution = service.UnitVerdict{Decision: "satisfactory", Summary: reviewSummary}
)

func awaitResolution(t *testing.T, s *Service, d drifter, stream config.WorkstreamID, op coreadapter.Operation, want string) {
	t.Helper()
	if result, err := attemptOperation(t, s, d.repository, stream, op, d); !errors.Is(err, lifecycle.ErrDriftAwaits) || !strings.Contains(err.Error(), want) {
		t.Fatalf("the drift rebase did not await %q: %+v %v", want, result, err)
	}
}

func completeDriftTurn(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, agent string, outcome *coreadapter.Outcome) trace.QueuedTurn {
	t.Helper()
	return intakefixture.CompleteDriftTurn(t, f.sharedTurns(), repository, stream, agent, outcome)
}

func conflictedDrift(t *testing.T, f *shedFixture, d drifter, stream config.WorkstreamID) (string, string, coreadapter.Operation) {
	t.Helper()
	before := moveFeature(t, f, stream, map[string]string{"CODEOWNERS": "/internal/ @feature\n"})
	upstream := advanceUpstream(t, f, map[string]string{"CODEOWNERS": "/internal/ @upstream\n"})
	return before, upstream, requestDrift(t, d, stream)
}

func driftResolutionDiff(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID) string {
	t.Helper()
	return intakefixture.DriftResolutionDiff(t, func() (coreadapter.Tool, error) {
		return lifecycle.DriftDiffTool(context.Background(), f.s.cfg, repository, coreadapter.Scope{Workstream: string(stream), Thread: intake.DriftReviewerAgent, Turn: lifecycle.DriftReviewTurnID(1, 1), Role: intake.ReviewerRole})
	})
}

func driftVerdict(t *testing.T, verdict service.UnitVerdict) *coreadapter.Outcome {
	t.Helper()
	return intakefixture.VerdictOutcome(t, verdict, execution.VerdictOutcome)
}

func featureTip(t *testing.T, f *shedFixture, stream config.WorkstreamID) string {
	t.Helper()
	tip, _, err := intake.Workspaces(f.s.cfg, intake.BranchesDirectory, backendOf(t, f, stream)).Branch(context.Background(), intake.FeatureBranch(stream))
	fixturekit.Must(t, err)
	return tip
}

func resolutionWorkspace(t *testing.T, f *shedFixture, stream config.WorkstreamID) workspace.Worktree {
	t.Helper()
	return intakefixture.Workspace(t, intake.Workspaces(f.s.cfg, intake.DriftsDirectory, backendOf(t, f, stream)), stream)
}

func resolvedDone(outcome string) *coreadapter.Outcome {
	data, _ := json.Marshal(service.MasonReport{Outcome: outcome})
	return &coreadapter.Outcome{Status: execution.MasonDone, Report: string(data)}
}

func advanceUpstream(t *testing.T, f *shedFixture, files map[string]string) string {
	t.Helper()
	return intakefixture.AdvanceUpstream(t, f.clone, files)
}

func checkpoints(t *testing.T, f *shedFixture, directory string) []string {
	t.Helper()
	return intakefixture.Checkpoints(t, jujutsuRepository(f, directory))
}

func interrupted(t *testing.T, f *shedFixture, directory string) (string, bool) {
	t.Helper()
	return intakefixture.Interrupted(t, directory, checkpoints(t, f, directory), operationLog(t, f, directory))
}

func operationLog(t *testing.T, f *shedFixture, directory string) []string {
	t.Helper()
	return intakefixture.OperationLog(t, jujutsuRepository(f, directory), filepath.Dir(f.clone))
}

func jujutsuRepository(f *shedFixture, directory string) string {
	return intakefixture.JujutsuRepository(f.s.cfg.Root, f.s.cfg.Project.ID, directory)
}

func stopAt(t *testing.T, s *Service, step string) context.Context {
	t.Helper()
	return intakefixture.StopAt(t, step, func(h func(string) error) { s.boundary = h })
}

func restoredTo(t *testing.T, f *shedFixture, directory, entry string, changed bool) {
	t.Helper()
	intakefixture.RestoredTo(t, directory, entry, changed, operationLog(t, f, directory), checkpoints(t, f, directory))
}

func (s *Service) recoverWorkspaces(ctx context.Context, cfg *config.Config) error {
	return service.InspectForTest(s.core()).RecoverWorkspaces(ctx, cfg)
}
