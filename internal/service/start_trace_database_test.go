package service

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestStartOpensAndSchedulesProjectsWhoseTracesAreInTheDatabase(t *testing.T) {
	t.Parallel()
	f := newTwoProjectFixture(t)
	entered := map[config.ProjectID]chan struct{}{}
	release := map[config.ProjectID]chan struct{}{}
	for _, id := range []config.ProjectID{project, otherProject} {
		entered[id], release[id] = f.block(id, "first")
	}
	s, err := Start(context.Background(), f.opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })

	for _, id := range []config.ProjectID{project, otherProject} {
		if _, active := s.runtimeOf(id); active == nil {
			t.Fatalf("project %s was not opened", id)
		}
		await(t, "project "+string(id)+" turn", entered[id])
		close(release[id])
	}
	for _, id := range []config.ProjectID{project, otherProject} {
		awaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord {
			repo := f.repository(id)
			if repo == nil {
				return nil
			}
			ops, err := repo.Operations(f.streams[id])
			must(t, err)
			return ops
		})
	}
}
