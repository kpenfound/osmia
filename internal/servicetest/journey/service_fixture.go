package journey

import (
	"context"
	"os"
	filepath "path/filepath"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func (s *Service) RefreshRuntime() error { return service.InspectForTest(s.Core()).RefreshRuntime() }

func (s *Service) Execution() *execution.Controller {
	c := service.InspectForTest(s.Core()).Execution()
	c.Checks = s.Options.ReviewChecks
	if s.Jev != nil {
		c.Jev = s.Jev
	}
	if s.Boundary != nil {
		c.Step = s.Boundary
	}
	return c
}

type Drifter struct{ *Foreman }

func (d Drifter) Driver() *service.TestDriftDriver {
	return service.DriftForTest(d.S.Core(), d.Repository)
}

func (d Drifter) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return lifecycle.NewDrifter(d.S.Lifecycle(), d.Cfg, d.Repository).Inspect(ctx, op)
}

func (d Drifter) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return lifecycle.NewDrifter(d.S.Lifecycle(), d.Cfg, d.Repository).Apply(ctx, op)
}

func (d Drifter) RequestDrifts(ctx context.Context, due func(config.WorkstreamID) (string, error)) ([]config.WorkstreamID, error) {
	return d.Driver().Request(ctx, due)
}

func (s *Service) CapacityStatus(list []api.WorkstreamStatus) *api.CapacityStatus {
	return service.InspectForTest(s.Core()).Capacity(list)
}

func DriftOperations(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) []trace.OperationRecord {
	return intakefixture.OperationsOf(t, repo, stream, service.DriftAction)
}

func RecordUpstreamOwnPullRequest(t *testing.T, s *Service, repo *trace.Repository, stream config.WorkstreamID, p api.DeliveryPublication) {
	t.Helper()
	fixturekit.UpstreamOwnPullRequest(t, repo, stream, p, s.Now(), execution.ForemanActor, delivery.PublicationDocument, delivery.PublicationPath)
}

// AppendConfig appends extra to the fixture's config.toml.
func AppendConfig(t *testing.T, opts *Options, extra string) {
	t.Helper()
	path := filepath.Join(opts.Config.Root, "config.toml")
	data, err := os.ReadFile(path)
	fixturekit.Must(t, err)
	fixturekit.Must(t, os.WriteFile(path, append(data, []byte(extra)...), 0600))
}
