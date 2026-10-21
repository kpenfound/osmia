package executionjourney

import (
	testing "testing"

	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

func settled(t *testing.T, count func() int) {
	t.Helper()
	intakefixture.Settled(t, count)
}
