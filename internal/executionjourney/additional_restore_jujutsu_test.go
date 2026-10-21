package executionjourney

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"

	"github.com/kpenfound/osmia/internal/workspace"
)

func stopAt(t *testing.T, s *Service, step string) context.Context {
	t.Helper()
	return intakefixture.StopAt(t, step, func(h func(string) error) { s.boundary = h })
}

func restoredTo(t *testing.T, f *shedFixture, directory, entry string, changed bool) {
	t.Helper()
	intakefixture.RestoredTo(t, directory, entry, changed, operationLog(t, f, directory), checkpoints(t, f, directory))
}

func onCommit(t *testing.T, g workspace.Provider, w workspace.Worktree, commit string) {
	t.Helper()
	intakefixture.OnCommit(t, g, w, commit)
}

func (s *Service) recoverWorkspaces(ctx context.Context, cfg *config.Config) error {
	return service.InspectForTest(s.core()).RecoverWorkspaces(ctx, cfg)
}
