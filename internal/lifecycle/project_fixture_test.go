package lifecycle_test

import (
	api "github.com/kpenfound/osmia/internal/api"
	config "github.com/kpenfound/osmia/internal/config"
	delivery "github.com/kpenfound/osmia/internal/delivery"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	service "github.com/kpenfound/osmia/internal/service"
	servicetest "github.com/kpenfound/osmia/internal/servicetest"
	fixturekit "github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	testing "testing"
)

func factoryPause(t *testing.T, s *Service) (runtime.Pause, bool) {
	t.Helper()
	st, _ := s.store.Effective()
	return fixturekit.PauseOf(api.RuntimeResponse{Effective: st}, factoryTarget)
}

// documentRevisions returns the active project's revisions of one document.
func documentRevisions(t *testing.T, s *Service, id string) []trace.Document {
	t.Helper()
	return intakefixture.DocumentRevisions(t, s.sole().repository, id)
}

func draftDelivery(cfg *config.Config, r *trace.Repository, w config.WorkstreamID, report FinalReport) (string, string, error) {
	return delivery.New(delivery.Dependencies{PendingMergeOrderReferences: pendingMergeOrderReferences}).DraftDelivery(cfg, r, w, report)
}

// traces returns the open traces of the active projects.
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

func (s *Service) capacityStatus(list []WorkstreamStatus) *CapacityStatus {
	return service.InspectForTest(s.core()).Capacity(list)
}

func budgetStatus(t *testing.T, s *Service) DailyBudgetStatus {
	t.Helper()
	got := s.dailyBudgetStatus()
	if got == nil {
		t.Fatal("no daily budget status")
	}
	return *got
}
