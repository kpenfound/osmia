package lifecycle_test

import (
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/service"
)

type pendingProject = service.TestPendingProject

var conversationEntries = service.TestConversationEntries

const ExtractAction = service.ExtractAction

func (s *Service) dailyBudgetStatus() *api.DailyBudgetStatus {
	return service.InspectForTest(s.core()).DailyBudget()
}

const Internal = api.Internal
