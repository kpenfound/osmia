package service

import (
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func newFinalFixture(t *testing.T, key string) (*shedFixture, config.WorkstreamID, *trace.Repository, *delivery.FinalReviewer) {
	t.Helper()
	f, stream, repository, _ := newFinalFixtureWith(t, key, "")
	hostFinalReviews(t, f, stream)
	return f, stream, repository, &delivery.FinalReviewer{S: f.s.delivery(), Repository: repository}
}

func hostFinalReviews(t *testing.T, f *shedFixture, stream config.WorkstreamID) {
	t.Helper()
	var fork string
	var pulls *fixturekit.FakePulls
	fork, pulls = fixturekit.PublicationFork(t, f.clone, func() string {
		tip, _ := fixturekit.PublicationForkBranch(t, fork, featureBranch(stream))
		return tip
	})
	pulls.SyncHead = true
	wait := fixturekit.NoPostPushWait()
	for _, o := range []*Options{&f.s.options, &f.opts} {
		o.PullRequests, o.FinalChecksWait = pulls, wait
	}
}

func newFinalFixtureWith(t *testing.T, key, extra string) (*shedFixture, config.WorkstreamID, *trace.Repository, *delivery.FinalReviewer) {
	t.Helper()
	f := newDebateFixtureWith(t, 1, 1, extra, nil)
	stream, repository := f.buildFixture().SeedGit(t, key)
	return f, stream, repository, &delivery.FinalReviewer{S: f.s.delivery(), Repository: repository}
}

func mergeDirectly(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, unit string, files map[string]string) string {
	t.Helper()
	return intakefixture.MergeDirectly(t, repository, stream, unit, moveFeature(t, f, stream, files), foremanActor, UnitMerged)
}

func advanceUpstream(t *testing.T, f *shedFixture, files map[string]string) string {
	t.Helper()
	return intakefixture.AdvanceUpstream(t, f.clone, files)
}

func finalOperations(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	return intakefixture.FinalOperations(t, repository, stream, FinalReviewAction)
}

func assembleBoth(t *testing.T, f *shedFixture, repository *trace.Repository, a *delivery.FinalReviewer, stream config.WorkstreamID, resume, dedupe map[string]string) (string, coreadapter.Operation) {
	t.Helper()
	return intakefixture.AssembleBoth(t, repository, a, stream, func(unit string, files map[string]string) string {
		return mergeDirectly(t, f, repository, stream, unit, files)
	}, resume, dedupe, AssembledState, FinalReviewAction)
}
