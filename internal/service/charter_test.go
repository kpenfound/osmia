package service

import (
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"

	testing "testing"
)

// documentRevisions returns the active project's revisions of one document.
func documentRevisions(t *testing.T, s *Service, id string) []trace.Document {
	t.Helper()
	return intakefixture.DocumentRevisions(t, s.sole().repository, id)
}
