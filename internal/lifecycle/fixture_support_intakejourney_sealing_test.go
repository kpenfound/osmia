package lifecycle_test

import (
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

func (f *architectFixture) upstream(t *testing.T) string {
	t.Helper()
	return intakefixture.Upstream(t, f.clone)
}
