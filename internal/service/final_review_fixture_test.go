package service

import (
	"io"
	"net/http"
	"net/url"
	"strings"

	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	fixturekit "github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	testing "testing"
)

// newFinalFixture seeds a sealed validPlan with a committee runner and no
// masons. It returns the stopped service, workstream, open trace and assembly
// controller. Intake, debate and ratification are covered by lifecycle tests.
func newFinalFixture(t *testing.T, key string) (*shedFixture, config.WorkstreamID, *trace.Repository, *finalReviewer) {
	t.Helper()
	f, stream, repository, a := newFinalFixtureWith(t, key, "")
	hostFinalReviews(t, f, stream)
	return f, stream, repository, a
}

// hostFinalReviews gives the service a credential for the pull request host:
// a local bare fork of owner/dagger as the clone's origin and a fake host
// that follows the fork's feature branch and shows a passing Cloud run for
// every pushed commit. A final review then publishes its draft and reads the
// result at once, as the fixtures' zero wait after a push lets it.
func hostFinalReviews(t *testing.T, f *shedFixture, stream config.WorkstreamID) {
	t.Helper()
	var fork string
	var pulls *fakePulls
	fork, pulls = fixturekit.PublicationFork(t, f.clone, func() string {
		tip, _ := fixturekit.PublicationForkBranch(t, fork, featureBranch(stream))
		return tip
	})
	pulls.SyncHead = true
	wait := fixturekit.NoPostPushWait()
	for _, o := range []*Options{&f.s.options, &f.opts} {
		o.PullRequests, o.FinalChecksWait, o.CloudStatusFetcher = pulls, wait, passingCloudFetcher()
	}
}

// passingCloudRun is the JSON of a completed "Run Dagger Checks" status in
// which every check passed.
const passingCloudRun = `{"dagger":{"status":"completed","checks":{"total":3,"passed":3,"failed":0,"cancelled":0}}}`

type passingCloudTransport struct{}

func (passingCloudTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(passingCloudRun)), Request: r}, nil
}

// passingCloudFetcher answers every read of a "Run Dagger Checks" JSON with a
// passing run, without a network.
func passingCloudFetcher() CloudStatusFetcher {
	return CloudStatusFetcher{HTTP: &http.Client{Transport: passingCloudTransport{}}, Allow: func(*url.URL) bool { return true }}
}

// newFinalFixtureWith is newFinalFixture with extra configuration appended.
func newFinalFixtureWith(t *testing.T, key, extra string) (*shedFixture, config.WorkstreamID, *trace.Repository, *finalReviewer) {
	t.Helper()
	f := newDebateFixtureWith(t, 1, 1, extra, nil)
	stream, repository := f.buildFixture().SeedGit(t, key)
	return f, stream, repository, &finalReviewer{s: f.s, repository: repository}
}

// mergeDirectly commits files onto the feature branch as the unit's landing
// and records the unit merged, as a landing does, and returns the new tip.
func mergeDirectly(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, unit string, files map[string]string) string {
	t.Helper()
	return intakefixture.MergeDirectly(t, repository, stream, unit, moveFeature(t, f, stream, files), foremanActor, UnitMerged)
}

// advanceUpstream commits files on upstream's main and returns the commit.
func advanceUpstream(t *testing.T, f *shedFixture, files map[string]string) string {
	t.Helper()
	return intakefixture.AdvanceUpstream(t, f.clone, files)
}

// finalOperations returns the workstream's final review operations.
func finalOperations(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	return intakefixture.FinalOperations(t, repository, stream, FinalReviewAction)
}

var transitionByID = intakefixture.TransitionByID

var chiefEvent = intakefixture.ChiefEvent

// assembleBoth merges both units of validPlan, checking that the workstream
// stays building while one is unmerged, and asks for the first final review.
// It returns the feature branch tip and the review's operation.
func assembleBoth(t *testing.T, f *shedFixture, repository *trace.Repository, a *finalReviewer, stream config.WorkstreamID, resume, dedupe map[string]string) (string, coreadapter.Operation) {
	t.Helper()
	return intakefixture.AssembleBoth(t, repository, a, stream, func(unit string, files map[string]string) string {
		return mergeDirectly(t, f, repository, stream, unit, files)
	}, resume, dedupe, AssembledState, FinalReviewAction)
}
