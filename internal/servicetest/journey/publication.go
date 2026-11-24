package journey

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

type PublicationFixture struct {
	*ShedFixture
	Stream     config.WorkstreamID
	Repository *trace.Repository
	Report     api.FinalReport
	Fork       string
	Pulls      *fixturekit.FakePulls

	Pushed string
	Finds  int
}

func NewPublicationFixture(t *testing.T, style string) *PublicationFixture {
	t.Helper()
	return NewPublicationFixtureWith(t, style, "")
}

func NewPublicationFixtureWith(t *testing.T, style, extra string) *PublicationFixture {
	t.Helper()
	p := NewUnpushedPublicationFixture(t, style, extra)
	p.PublishDraft(t)
	return p
}

func NewUnpushedPublicationFixture(t *testing.T, style, extra string) *PublicationFixture {
	t.Helper()
	f, stream, repository, report := DeliveryFixtureWith(t, extra)
	p := &PublicationFixture{ShedFixture: f, Stream: stream, Repository: repository, Report: report}
	p.Fork, p.Pulls = fixturekit.PublicationFork(t, f.Clone, func() string { commit, _ := p.ForkBranch(t); return commit })
	f.S.Cfg.Project.Landing = style
	f.S.Options.PullRequests = p.Pulls
	return p
}

func (p *PublicationFixture) TryPublishDraft(t *testing.T) (string, error) {
	t.Helper()
	report := p.Report
	report.Review, report.Operation, report.Branch = 1, "operation_final_review_1", intake.FeatureBranch(p.Stream)
	reason, err := (&FinalReviewer{S: p.S, Repository: p.Repository}).PublishDraft(context.Background(), p.S.Cfg, p.Stream, coreadapter.Operation{ID: report.Operation}, report)
	if err == nil && reason == "" {
		p.Pushed, _ = p.ForkBranch(t)
		p.Finds = p.Pulls.Finds
	}
	return reason, err
}

func (p *PublicationFixture) PublishDraft(t *testing.T) {
	t.Helper()
	if reason, err := p.TryPublishDraft(t); err != nil || reason != "" {
		t.Fatalf("publishing the draft: %q, %v", reason, err)
	}
}

func (p *PublicationFixture) ForkBranch(t *testing.T) (string, bool) {
	t.Helper()
	return fixturekit.PublicationForkBranch(t, p.Fork, intake.FeatureBranch(p.Stream))
}

func (p *PublicationFixture) Approve(t *testing.T, description *string) api.DeliveryApproval {
	t.Helper()
	return fixturekit.ApprovePublication(t, p.Stream, description, p.S.DeliveryPresentation, p.S.ApproveDelivery)
}

func (p *PublicationFixture) Publisher() *Publisher {
	return &Publisher{S: p.S, Repository: p.Repository}
}

func (p *PublicationFixture) Operations(t *testing.T) []coreadapter.Operation {
	t.Helper()
	return fixturekit.PublicationOperations(t, p.Repository, p.Stream, delivery.PublishAction)
}

func (p *PublicationFixture) Request(t *testing.T) coreadapter.Operation {
	t.Helper()
	return fixturekit.RequestPublication(t, p.Repository, p.Stream, delivery.PublishAction, p.Publisher().Pass)
}

func (p *PublicationFixture) Feature(t *testing.T) string {
	t.Helper()
	return fixturekit.PublicationFeature(t, p.Repository, p.Stream)
}

func (p *PublicationFixture) Reopen(t *testing.T) {
	t.Helper()
	p.Repository = fixturekit.ReopenPublication(t, p.Repository, p.S.Cfg.Root, p.S.Cfg.Project)
	p.S.SetSole(&ActiveProject{Repository: p.Repository})
}
