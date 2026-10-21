package service

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

type repositoryPulls = fixturekit.RepositoryPulls

func maintenanceFixture(t *testing.T) *publicationFixture {
	t.Helper()
	p := newPublicationFixtureWith(t, "commit-per-unit", "")
	intakefixture.PrepareDeliveryMaintenance(t, intakefixture.DeliveryMaintenance{
		Config: p.s.cfg, Repository: p.repository, Stream: p.stream, Clone: p.clone, Now: p.s.now,
		Approve: func() { p.approve(t, nil) },
		Publish: func() (coreadapter.OperationResult, error) {
			return p.publisher().Apply(context.Background(), p.request(t))
		},
		AdvanceUpstream: func(files map[string]string) { advanceUpstream(t, p.shedFixture, files) },
		ForkTip:         func() string { tip, _ := p.forkBranch(t); return tip },
		Refresh: func(host *fakePulls, upstream string) coreadapter.OperationResult {
			p.s.options.PullRequests = repositoryPulls{upstream: host, p.s.cfg.Project.Fork: p.pulls}
			refresh := &baseRefresher{S: p.s.lifecycle(), Repository: p.repository}
			must(t, refresh.Pass(context.Background()))
			op := actionOperation(t, p.repository, p.stream, baseRefreshAction)
			return settleOperation(t, p.s, p.repository, p.stream, op, refresh)
		},
		Observation: func(r *trace.Repository, w config.WorkstreamID) (seal.Base, bool, int, error) {
			o, rev, err := baseObservationAt(r, w)
			return o.Base, o.Unavailable, rev, err
		},
	})
	return p
}
