package lifecycle_test

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

type publicationFixture struct {
	*shedFixture
	stream     config.WorkstreamID
	repository *trace.Repository
	report     api.FinalReport
	fork       string
	pulls      *fixturekit.FakePulls
	pushed     string
	finds      int
}

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

func newUnpushedPublicationFixture(t *testing.T, style, extra string) *publicationFixture {
	t.Helper()
	f, stream, repository, report := deliveryFixtureWith(t, extra)
	p := &publicationFixture{shedFixture: f, stream: stream, repository: repository, report: report}
	p.fork, p.pulls = fixturekit.PublicationFork(t, f.clone, func() string { commit, _ := p.forkBranch(t); return commit })
	f.s.cfg.Project.Landing = style
	f.s.options.PullRequests = p.pulls
	return p
}

func (p *publicationFixture) tryPublishDraft(t *testing.T) (string, error) {
	t.Helper()
	report := p.report
	report.Review, report.Operation, report.Branch = 1, "operation_final_review_1", intake.FeatureBranch(p.stream)
	reason, err := (&finalReviewer{s: p.s, repository: p.repository}).PublishDraft(context.Background(), p.s.cfg, p.stream, coreadapter.Operation{ID: report.Operation}, report)
	if err == nil && reason == "" {
		p.pushed, _ = p.forkBranch(t)
		p.finds = p.pulls.Finds
	}
	return reason, err
}

func (p *publicationFixture) publishDraft(t *testing.T) {
	t.Helper()
	if reason, err := p.tryPublishDraft(t); err != nil || reason != "" {
		t.Fatalf("publishing the draft: %q, %v", reason, err)
	}
}
func (p *publicationFixture) forkBranch(t *testing.T) (string, bool) {
	t.Helper()
	return fixturekit.PublicationForkBranch(t, p.fork, intake.FeatureBranch(p.stream))
}
func (p *publicationFixture) approve(t *testing.T, description *string) api.DeliveryApproval {
	t.Helper()
	return fixturekit.ApprovePublication(t, p.stream, description, p.s.deliveryPresentation, p.s.approveDelivery)
}
func (p *publicationFixture) publisher() *publisher {
	return &publisher{s: p.s, repository: p.repository}
}
func (p *publicationFixture) operations(t *testing.T) []coreadapter.Operation {
	t.Helper()
	return fixturekit.PublicationOperations(t, p.repository, p.stream, delivery.PublishAction)
}
func (p *publicationFixture) request(t *testing.T) coreadapter.Operation {
	t.Helper()
	return fixturekit.RequestPublication(t, p.repository, p.stream, delivery.PublishAction, p.publisher().Pass)
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
