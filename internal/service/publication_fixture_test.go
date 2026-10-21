package service

import (
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
}

func newPublicationFixtureWith(t *testing.T, style, extra string) *publicationFixture {
	t.Helper()
	f, stream, repository, report := deliveryFixtureWith(t, extra)
	p := &publicationFixture{shedFixture: f, stream: stream, repository: repository, report: report}
	p.fork, p.pulls = fixturekit.PublicationFork(t, f.clone, func() string { commit, _ := p.forkBranch(t); return commit })
	f.s.mu.Lock()
	f.s.cfg.Project.Landing = style
	f.s.options.PullRequests = p.pulls
	f.s.mu.Unlock()
	return p
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

func newPublicationFixture(t *testing.T, style string) *publicationFixture {
	t.Helper()
	return newPublicationFixtureWith(t, style, "")
}
