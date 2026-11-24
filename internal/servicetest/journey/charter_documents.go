package journey

import (
	testing "testing"

	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func DocumentRevisions(t *testing.T, s *Service, id string) []trace.Document {
	t.Helper()
	return intakefixture.DocumentRevisions(t, s.Sole().Repository, id)
}
