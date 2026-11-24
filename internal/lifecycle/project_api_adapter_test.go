package lifecycle_test

import (
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/service"
)

func (s *Service) dailyBudgetStatus() *api.DailyBudgetStatus {
	return service.InspectForTest(s.core()).DailyBudget()
}
