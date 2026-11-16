package lifecycle_test

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newFinalFixture seeds a sealed validPlan with a committee runner and no
// masons. It returns the stopped service, workstream, open trace and assembly
// controller. Intake, debate and ratification are covered by lifecycle tests.
func newFinalFixture(t *testing.T, key string) (*shedFixture, config.WorkstreamID, *trace.Repository, *finalReviewer) {
	t.Helper()
	return newFinalFixtureWith(t, key, "")
}

// newFinalFixtureWith is newFinalFixture with extra configuration appended.
func newFinalFixtureWith(t *testing.T, key, extra string) (*shedFixture, config.WorkstreamID, *trace.Repository, *finalReviewer) {
	t.Helper()
	f := newDebateFixtureWith(t, 1, 1, extra, nil)
	stream, repository := f.buildFixture().SeedGit(t, key)
	return f, stream, repository, &finalReviewer{s: f.s, repository: repository}
}

// hostFinalReviews gives the service a credential for the pull request host:
// a local bare fork of owner/dagger as the clone's origin and a fake host
// that follows the fork's feature branch and shows a passing Cloud run for
// every pushed commit. A final review then publishes its draft and reads the
// result at once, as the fixtures' zero wait after a push lets it.
func hostFinalReviews(t *testing.T, f *shedFixture, stream config.WorkstreamID) (string, *fakePulls) {
	t.Helper()
	return hostService(t, f, func() config.WorkstreamID { return stream })
}

// hostService is hostFinalReviews for a workstream the fixture creates later:
// stream names it once it exists, for a service that starts afterwards or is
// running.
func hostService(t *testing.T, f *shedFixture, stream func() config.WorkstreamID) (string, *fakePulls) {
	t.Helper()
	var fork string
	var pulls *fakePulls
	fork, pulls = fixturekit.PublicationFork(t, f.clone, func() string {
		tip, _ := fixturekit.PublicationForkBranch(t, fork, featureBranch(stream()))
		return tip
	})
	pulls.SyncHead = true
	f.s.options.PullRequests = pulls
	f.opts.PullRequests = pulls
	return fork, pulls
}

// hostBranches is hostFinalReviews for a service that holds the pull requests
// of several workstreams: the host follows the fork's branch of each pull
// request's own head.
func hostBranches(t *testing.T, f *shedFixture) (string, *fakePulls) {
	t.Helper()
	var fork string
	var pulls *fakePulls
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

// mergeDirectly commits files onto the feature branch as the unit's landing
// and records the unit merged, as a landing does, and returns the new tip.
func mergeDirectly(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, unit string, files map[string]string) string {
	t.Helper()
	return intakefixture.MergeDirectly(t, repository, stream, unit, moveFeature(t, f, stream, files), foremanActor, UnitMerged)
}

// finalOperations returns the workstream's final review operations.
func finalOperations(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	return intakefixture.FinalOperations(t, repository, stream, FinalReviewAction)
}

// assembleBoth merges both units of validPlan, checking that the workstream
// stays building while one is unmerged, and asks for the first final review.
// It returns the feature branch tip and the review's operation.
func assembleBoth(t *testing.T, f *shedFixture, repository *trace.Repository, a *finalReviewer, stream config.WorkstreamID, resume, dedupe map[string]string) (string, coreadapter.Operation) {
	t.Helper()
	return intakefixture.AssembleBoth(t, repository, a, stream, func(unit string, files map[string]string) string {
		return mergeDirectly(t, f, repository, stream, unit, files)
	}, resume, dedupe, AssembledState, FinalReviewAction)
}

// finalTurn installs the fake committee member's turn of final review k.
func (f *shedFixture) finalTurn(k, attempt int, run func(context.Context, *mcp.ClientSession) error) string {
	return intakefixture.FinalTurn(f.engine, finalTurnID(k, committeeAgent(1), attempt), run)
}
