package lifecycle_test

import (
	"github.com/kpenfound/osmia/internal/browsertest"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest"
	"github.com/kpenfound/osmia/internal/trace"
	"testing"
)

var openBrowser = browsertest.OpenBrowser

func sidebarFixture(t *testing.T) (Options, *trace.Repository) {
	t.Helper()
	opts, _, repo := servicetest.ActivityFixture(t)
	servicetest.WithWeb(t, opts, "127.0.0.1:0")
	return Options{Options: opts}, repo
}

func (s *Service) setPause(p runtime.Pause) error {
	return service.InspectForTest(s.core()).SetPause(p)
}
