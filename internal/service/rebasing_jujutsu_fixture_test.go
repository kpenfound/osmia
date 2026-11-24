package service

import (
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

func onWorkspaces(t *testing.T, opts Options, backend string) {
	t.Helper()
	intakefixture.OnWorkspaces(t, opts.Config.Root, backend)
}

func backendOf(t *testing.T, f *shedFixture, stream config.WorkstreamID) string {
	t.Helper()
	return intakefixture.BackendOf(t, f.opts.Config, f.project, stream)
}
