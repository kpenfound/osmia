package lifecycle_test

import (
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	trace "github.com/kpenfound/osmia/internal/trace"
)

func traces(s *journey.Service) []*trace.Repository {
	_, projects := s.Runtimes()
	out := make([]*trace.Repository, len(projects))
	for i, p := range projects {
		out[i] = p.Repository
	}
	return out
}
