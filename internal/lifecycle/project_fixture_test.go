package lifecycle_test

import (
	testing "testing"

	api "github.com/kpenfound/osmia/internal/api"
	config "github.com/kpenfound/osmia/internal/config"
	delivery "github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/lifecycle"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	service "github.com/kpenfound/osmia/internal/service"
	servicetest "github.com/kpenfound/osmia/internal/servicetest"
	fixturekit "github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
)

const listenWeb = "[listen]\nweb = \"127.0.0.1:0\"\n"

func factoryPause(t *testing.T, s *Service) (runtime.Pause, bool) {
	t.Helper()
	st, _ := s.store.Effective()
	return fixturekit.PauseOf(api.RuntimeResponse{Effective: st}, runtime.Target{Scope: "factory"})
}

func documentRevisions(t *testing.T, s *Service, id string) []trace.Document {
	t.Helper()
	return intakefixture.DocumentRevisions(t, s.sole().repository, id)
}

func draftDelivery(cfg *config.Config, r *trace.Repository, w config.WorkstreamID, report api.FinalReport) (string, string, error) {
	return delivery.New(delivery.Dependencies{PendingMergeOrderReferences: lifecycle.PendingMergeOrderReferences}).DraftDelivery(cfg, r, w, report)
}

func (s *Service) traces() []*trace.Repository {
	_, projects := s.runtimes()
	out := make([]*trace.Repository, len(projects))
	for i, p := range projects {
		out[i] = p.repository
	}
	return out
}

func mutation(t *testing.T, c *Client, method, kind string, input any) {
	servicetest.Mutation(t, c.Client, method, kind, input)
}

func (s *Service) capacityStatus(list []api.WorkstreamStatus) *api.CapacityStatus {
	return service.InspectForTest(s.core()).Capacity(list)
}

func budgetStatus(t *testing.T, s *Service) api.DailyBudgetStatus {
	t.Helper()
	got := service.InspectForTest(s.core()).DailyBudget()
	if got == nil {
		t.Fatal("no daily budget status")
	}
	return *got
}
