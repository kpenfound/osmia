package lifecycle_test

import (
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/trace"
)

func (f *architectFixture) runs() []string {
	return f.scenario().Runs()
}
func (s *Service) answers(cfg *config.Config, r *trace.Repository) *questions.Deliverer {
	return s.intake().Answers(cfg, r)
}
