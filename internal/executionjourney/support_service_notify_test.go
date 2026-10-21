package executionjourney

import (
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	testing "testing"
)

func settled(t *testing.T, count func() int) {
	t.Helper()
	intakefixture.Settled(t, count)
}
