package lifecycle_test

import (
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/service"
)

type pendingProject = service.TestPendingProject

var conversationEntries = service.TestConversationEntries

const ExtractAction = service.ExtractAction

func (s *Service) validateAddRepo(req api.RepoAddRequest) (config.Project, config.Project, *api.APIError) {
	return service.InspectForTest(s.core()).ValidateAddRepo(req)
}
func (s *Service) dailyBudgetStatus() *api.DailyBudgetStatus {
	return service.InspectForTest(s.core()).DailyBudget()
}

const Internal = api.Internal
