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
	time "time"
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

// awaitExtraction polls status until the latest extraction is terminal.
func awaitExtraction(t *testing.T, c *Client) ExtractionState {
	t.Helper()
	return intakefixture.AwaitExtraction(t, c.Configuration)
}

// awaitExtractionAcknowledged polls the librarian workstream until the latest
// extraction operation is acknowledged. The reconcile controller records the
// acknowledgement as a commit after the one carrying the result, so a terminal
// extraction state is not yet the operation's last commit.
func awaitExtractionAcknowledged(t *testing.T, s *Service) {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for {
		if s.sole() != nil {
			ops, err := s.sole().repository.Operations(librarianWorkstream(s.sole().repository.Project()))
			must(t, err)
			extractions, acknowledged := 0, 0
			for _, o := range ops {
				if o.Operation.Action != ExtractAction {
					continue
				}
				extractions++
				if o.Acknowledged {
					acknowledged++
				}
			}
			if extractions > 0 && extractions == acknowledged {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("extraction was not acknowledged")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func budgetStatus(t *testing.T, s *Service) DailyBudgetStatus {
	t.Helper()
	got := s.dailyBudgetStatus()
	if got == nil {
		t.Fatal("no daily budget status")
	}
	return *got
}
