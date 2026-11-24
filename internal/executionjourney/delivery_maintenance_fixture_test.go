package executionjourney

import (
	"context"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/service"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func maintenanceFixture(t *testing.T) *publicationFixture {
	t.Helper()

	p := newUnpushedPublicationFixture(t, "commit-per-unit", "")
	intakefixture.PrepareDeliveryMaintenance(t, intakefixture.DeliveryMaintenance{
		Config: p.s.cfg, Repository: p.repository, Stream: p.stream, Clone: p.clone, Now: p.s.now,
		Approve: func() { p.publishDraft(t); p.approve(t, nil) },
		Publish: func() (coreadapter.OperationResult, error) {
			return p.publisher().Apply(context.Background(), p.request(t))
		},
		AdvanceUpstream: func(files map[string]string) { advanceUpstream(t, p.shedFixture, files) },
		ForkTip:         func() string { tip, _ := p.forkBranch(t); return tip },
		Refresh: func(host *fixturekit.FakePulls, upstream string) coreadapter.OperationResult {
			p.s.options.PullRequests = fixturekit.RepositoryPulls{upstream: host, p.s.cfg.Project.Fork: p.pulls}
			refresh := &baseRefresher{s: p.s, repository: p.repository}
			fixturekit.Must(t, refresh.Pass(context.Background()))
			op := fixturekit.ActionOperation(t, p.repository, p.stream, lifecycle.BaseRefreshAction)
			return settleOperation(t, p.s, p.repository, p.stream, op, refresh)
		},
		Observation: func(r *trace.Repository, w config.WorkstreamID) (seal.Base, bool, int, error) {
			o, rev, err := service.TestBaseObservationAt(r, w)
			return o.Base, o.Unavailable, rev, err
		},
	})
	return p
}
