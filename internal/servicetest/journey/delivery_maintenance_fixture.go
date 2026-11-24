package journey

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func MaintenanceFixture(t *testing.T) *PublicationFixture {
	t.Helper()

	p := NewUnpushedPublicationFixture(t, "commit-per-unit", "")
	intakefixture.PrepareDeliveryMaintenance(t, intakefixture.DeliveryMaintenance{
		Config: p.S.Cfg, Repository: p.Repository, Stream: p.Stream, Clone: p.Clone, Now: p.S.Now,
		Approve: func() { p.PublishDraft(t); p.Approve(t, nil) },
		Publish: func() (coreadapter.OperationResult, error) {
			return p.Publisher().Apply(context.Background(), p.Request(t))
		},
		AdvanceUpstream: func(files map[string]string) { AdvanceUpstream(t, p.ShedFixture, files) },
		ForkTip:         func() string { tip, _ := p.ForkBranch(t); return tip },
		Refresh: func(host *fixturekit.FakePulls, upstream string) coreadapter.OperationResult {
			p.S.Options.PullRequests = fixturekit.RepositoryPulls{upstream: host, p.S.Cfg.Project.Fork: p.Pulls}
			refresh := &BaseRefresher{S: p.S, Repository: p.Repository}
			fixturekit.Must(t, refresh.Pass(context.Background()))
			op := fixturekit.ActionOperation(t, p.Repository, p.Stream, lifecycle.BaseRefreshAction)
			return SettleOperation(t, p.S, p.Repository, p.Stream, op, refresh)
		},
		Observation: func(r *trace.Repository, w config.WorkstreamID) (seal.Base, bool, int, error) {
			o, rev, err := lifecycle.BaseObservationAt(r, w)
			return o.Base, o.Unavailable, rev, err
		},
	})
	return p
}
