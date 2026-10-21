package lifecycle_test

import (
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func conversationFixture(t *testing.T, key string) (Options, *config.Config) {
	o, cfg := servicetest.ConversationFixture(t, key)
	return Options{Options: o}, cfg
}
func runtimeFor(r *trace.Repository) *activeProject {
	return &activeProject{id: r.Project(), repository: r}
}

const quiet = fixturekit.Quiet

func (s *Service) holding(r *trace.Repository) func(config.WorkstreamID, coreadapter.Operation) bool {
	service.InspectForTest(s.core()).SetStoppedPullRequests(s.options.PullRequests)
	return service.InspectForTest(s.core()).Holding(r)
}
