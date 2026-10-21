package lifecycle_test

import (
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

// newShedFixture debates for one round, so a test of a round finds the shed
// at rest once the round's debate concluded.
func newShedFixture(t *testing.T, members int) *shedFixture {
	t.Helper()
	return newDebateFixture(t, members, 1)
}

var shedTool = intakefixture.ShedTool
