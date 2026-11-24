package journey

import (
	context "context"
	testing "testing"

	"github.com/kpenfound/osmia/internal/api"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	delivery "github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	lifecycle "github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/seal"
	service "github.com/kpenfound/osmia/internal/service"
	fixturekit "github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func DraftDelivery(cfg *config.Config, r *trace.Repository, w config.WorkstreamID, report api.FinalReport) (string, string, error) {
	return delivery.New(delivery.Dependencies{PendingMergeOrderReferences: lifecycle.PendingMergeOrderReferences}).DraftDelivery(cfg, r, w, report)
}

func (s *Service) Lifecycle() *lifecycle.Controller {
	c := service.InspectForTest(s.Core()).LifecycleController()
	c.Options.Architect = s.Options.Architect
	c.Options.Committee = s.Options.Committee
	c.Options.PullRequests = s.Options.PullRequests
	c.Execution = s.Execution
	c.Intake = s.Intake
	if s.Options.Reconciliation.Now != nil {
		c.Now = s.Options.Reconciliation.Now
	}
	if s.Boundary != nil {
		c.Step = s.Boundary
	}
	return c
}

func (s *Service) WorkstreamStatus(raw string) (api.WorkstreamStatus, *api.APIError) {
	return service.InspectForTest(s.Core()).WorkstreamStatus(raw)
}

func (s *Service) RuntimeOf(id config.ProjectID) (*config.Config, *ActiveProject) {
	cfg, ps := s.Runtimes()
	for _, p := range ps {
		if p.Id == id {
			return cfg, p
		}
	}
	return cfg, nil
}

func (s *Service) StatusList() api.StatusResponse {
	return service.InspectForTest(s.Core()).StatusList()
}

func CurrentChange(t *testing.T, s *Service, address config.WorkstreamID, id string) lifecycle.WorkgroupChange {
	t.Helper()
	wt, g, _, close, err := s.Lifecycle().ChangeContext(context.Background(), string(address))
	fixturekit.Must(t, err)
	defer close()
	changes, err := lifecycle.ReadChanges(wt, g.Members[0])
	fixturekit.Must(t, err)
	for _, c := range changes {
		if c.ID == id {
			return c
		}
	}
	return lifecycle.WorkgroupChange{}
}

func CompleteClaimedTurn(t *testing.T, f *ShedFixture, r *trace.Repository, w config.WorkstreamID, a string, q trace.QueuedTurn, o *coreadapter.Outcome) {
	t.Helper()
	f.SharedTurns().CompleteClaimedTurn(t, r, w, a, q, o)
}

func ClaimOldestTurn(t *testing.T, f *ShedFixture, r *trace.Repository, w config.WorkstreamID, a string) trace.QueuedTurn {
	t.Helper()
	return intakefixture.ClaimOldestTurn(t, f.SharedTurns(), r, w, a)
}

func SealDependent(t *testing.T, f *ShedFixture, repo *trace.Repository, backend string, base config.WorkstreamID, files map[string]string) (config.WorkstreamID, string) {
	t.Helper()
	return intakefixture.SealDependent(t, f.S.Intake(), repo, backend, base, files)
}

// RecordResolutionMerge records the delivery merge document a member's
// resolution turn reads.
func RecordResolutionMerge(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) {
	t.Helper()
	fixturekit.ResolutionDocument(t, repo, stream, delivery.MergeDocument, delivery.MergePath, api.DeliveryMerge{Branch: intake.FeatureBranch(stream), Commit: "merged-feature", Upstream: seal.Base{Remote: "upstream", Branch: "main", Commit: "merged-upstream"}}, fixturekit.DemoStart, intake.OwnerActor)
}
