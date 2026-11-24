package service

import (
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"testing"
)

func (f *architectFixture) upstream(t *testing.T) string {
	t.Helper()
	return intakefixture.Upstream(t, f.clone)
}
