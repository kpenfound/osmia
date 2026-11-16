package lifecycle_test

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

type publicationFixture struct {
	*shedFixture
	stream     config.WorkstreamID
	repository *trace.Repository
	report     FinalReport
	fork       string
	pulls      *fakePulls
	// pushed is the commit the draft push left on the fork's feature branch,
	// and finds how many times the host had been asked for pull requests by
	// then.
	pushed string
	finds  int
}

// newPublicationFixture reviews an assembled workstream, gives the clone a
// local bare fork of owner/dagger as its origin remote and a fake pull
// request host, and configures the delivery style.
func newPublicationFixture(t *testing.T, style string) *publicationFixture {
	t.Helper()
	return newPublicationFixtureWith(t, style, "")
}
func newPublicationFixtureWith(t *testing.T, style, extra string) *publicationFixture {
	t.Helper()
	p := newUnpushedPublicationFixture(t, style, extra)
	p.publishDraft(t)
	return p
}

// newUnpushedPublicationFixture is a publication fixture whose reviewed
// commit has no recorded push: the service has not yet signed it, pushed it or
// opened the draft pull request.
func newUnpushedPublicationFixture(t *testing.T, style, extra string) *publicationFixture {
	t.Helper()
	f, stream, repository, report := deliveryFixtureWith(t, extra)
	p := &publicationFixture{shedFixture: f, stream: stream, repository: repository, report: report}
	p.fork, p.pulls = fixturekit.PublicationFork(t, f.clone, func() string { commit, _ := p.forkBranch(t); return commit })
	f.s.cfg.Project.Landing = style
	f.s.options.PullRequests = p.pulls
	return p
}

// tryPublishDraft has the final review's publication sign the reviewed
// commit, push it and open the draft pull request, as the review does before
// its checks are watched, and returns why it refused and any error.
func (p *publicationFixture) tryPublishDraft(t *testing.T) (string, error) {
	t.Helper()
	report := p.report
	report.Review, report.Operation, report.Branch = 1, "operation_final_review_1", featureBranch(p.stream)
	reason, err := (&finalReviewer{s: p.s, repository: p.repository}).PublishDraft(context.Background(), p.s.cfg, p.stream, coreadapter.Operation{ID: report.Operation}, report)
	if err == nil && reason == "" {
		p.pushed, _ = p.forkBranch(t)
		p.finds = p.pulls.Finds
	}
	return reason, err
}

// publishDraft is tryPublishDraft for a publication that must succeed.
func (p *publicationFixture) publishDraft(t *testing.T) {
	t.Helper()
	if reason, err := p.tryPublishDraft(t); err != nil || reason != "" {
		t.Fatalf("publishing the draft: %q, %v", reason, err)
	}
}
func (p *publicationFixture) forkBranch(t *testing.T) (string, bool) {
	t.Helper()
	return fixturekit.PublicationForkBranch(t, p.fork, featureBranch(p.stream))
}
func (p *publicationFixture) approve(t *testing.T, description *string) DeliveryApproval {
	t.Helper()
	return fixturekit.ApprovePublication(t, p.stream, description, p.s.deliveryPresentation, p.s.approveDelivery)
}
func (p *publicationFixture) publisher() *publisher {
	return &publisher{s: p.s, repository: p.repository}
}
func (p *publicationFixture) operations(t *testing.T) []coreadapter.Operation {
	t.Helper()
	return fixturekit.PublicationOperations(t, p.repository, p.stream, PublishAction)
}
func (p *publicationFixture) request(t *testing.T) coreadapter.Operation {
	t.Helper()
	return fixturekit.RequestPublication(t, p.repository, p.stream, PublishAction, p.publisher().Pass)
}
func (p *publicationFixture) feature(t *testing.T) string {
	t.Helper()
	return fixturekit.PublicationFeature(t, p.repository, p.stream)
}
func (p *publicationFixture) reopen(t *testing.T) {
	t.Helper()
	p.repository = fixturekit.ReopenPublication(t, p.repository, p.s.cfg.Root, p.s.cfg.Project)
	p.s.setSole(&activeProject{repository: p.repository})
}
