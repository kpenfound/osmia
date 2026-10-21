package lifecycle_test

import (
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

var requireJJ = intakefixture.RequireJJ

func backendOf(t *testing.T, f *shedFixture, stream config.WorkstreamID) string {
	t.Helper()
	return intakefixture.BackendOf(t, f.opts.Config, f.project, stream)
}
