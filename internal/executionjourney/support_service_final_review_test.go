package executionjourney

import (
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	testing "testing"
)

func advanceUpstream(t *testing.T, f *shedFixture, files map[string]string) string {
	t.Helper()
	return intakefixture.AdvanceUpstream(t, f.clone, files)
}
