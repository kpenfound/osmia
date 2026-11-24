package lifecycle_test

import (
	"context"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func newFinalFixture(t *testing.T, key string) (*shedFixture, config.WorkstreamID, *trace.Repository, *finalReviewer) {
	t.Helper()
	return newFinalFixtureWith(t, key, "")
}

func newFinalFixtureWith(t *testing.T, key, extra string) (*shedFixture, config.WorkstreamID, *trace.Repository, *finalReviewer) {
	t.Helper()
	f := newDebateFixtureWith(t, 1, 1, extra, nil)
	stream, repository := f.buildFixture().SeedGit(t, key)
	return f, stream, repository, &finalReviewer{s: f.s, repository: repository}
}

func hostFinalReviews(t *testing.T, f *shedFixture, stream config.WorkstreamID) (string, *fixturekit.FakePulls) {
	t.Helper()
	return hostService(t, f, func() config.WorkstreamID { return stream })
}

func hostService(t *testing.T, f *shedFixture, stream func() config.WorkstreamID) (string, *fixturekit.FakePulls) {
	t.Helper()
	var fork string
	var pulls *fixturekit.FakePulls
	fork, pulls = fixturekit.PublicationFork(t, f.clone, func() string {
		tip, _ := fixturekit.PublicationForkBranch(t, fork, intake.FeatureBranch(stream()))
		return tip
	})
	pulls.SyncHead = true
	f.s.options.PullRequests = pulls
	f.opts.PullRequests = pulls
	return fork, pulls
}

func hostBranches(t *testing.T, f *shedFixture) (string, *fixturekit.FakePulls) {
	t.Helper()
	var fork string
	var pulls *fixturekit.FakePulls
	fork, pulls = fixturekit.PublicationFork(t, f.clone, func() string { return "" })
	pulls.Branches = func(branch string) string {
		tip, _ := fixturekit.PublicationForkBranch(t, fork, branch)
		return tip
	}
	pulls.SyncHead = true
	f.s.options.PullRequests = pulls
	f.opts.PullRequests = pulls
	return fork, pulls
}

func mergeDirectly(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, unit string, files map[string]string) string {
	t.Helper()
	return intakefixture.MergeDirectly(t, repository, stream, unit, moveFeature(t, f, stream, files), service.TestForemanActor, intake.UnitMerged)
}

func finalOperations(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	return intakefixture.FinalOperations(t, repository, stream, delivery.FinalReviewAction)
}

func assembleBoth(t *testing.T, f *shedFixture, repository *trace.Repository, a *finalReviewer, stream config.WorkstreamID, resume, dedupe map[string]string) (string, coreadapter.Operation) {
	t.Helper()
	return intakefixture.AssembleBoth(t, repository, a, stream, func(unit string, files map[string]string) string {
		return mergeDirectly(t, f, repository, stream, unit, files)
	}, resume, dedupe, intake.AssembledState, delivery.FinalReviewAction)
}

func (f *shedFixture) finalTurn(k, attempt int, run func(context.Context, *mcp.ClientSession) error) string {
	return intakefixture.FinalTurn(f.engine, delivery.FinalTurnID(k, intake.CommitteeAgent(1), attempt), run)
}

func newReviewFixture(t *testing.T, key string) (*shedFixture, config.WorkstreamID, *trace.Repository) {
	t.Helper()
	f, _ := newMasonFixture(t, 1, independentPlan)
	base := strings.TrimSpace(fixturekit.DemoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	stream, repository := seedBuild(t, f, key, independentPlan, config.WorkspacesGit, base)
	seedReview(t, f, repository, stream, "resume", resumeReport)
	return f, stream, repository
}

func advanceUpstream(t *testing.T, f *shedFixture, files map[string]string) string {
	t.Helper()
	return intakefixture.AdvanceUpstream(t, f.clone, files)
}

func runChecks(t *testing.T, s *Service, repository *trace.Repository, stream config.WorkstreamID) {
	t.Helper()
	intakefixture.RunChecks(t, repository, stream, &checkers{masons: newMasonController(s, repository)}, service.CheckAction, func(op coreadapter.Operation, r coreadapter.Reconciler) (coreadapter.OperationResult, error) {
		return attemptOperation(t, s, repository, stream, op, r)
	})
}
