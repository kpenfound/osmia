package service

import (
	"context"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestStartOpensAndSchedulesEveryProjectOfTheRoot(t *testing.T) {
	t.Parallel()
	f := newTwoProjectFixture(t)
	entered := map[config.ProjectID]chan struct{}{}
	for _, id := range []config.ProjectID{intakefixture.Project, otherProject} {
		var release chan struct{}
		entered[id], release = f.block(id, "first")
		close(release)
	}
	s, err := Start(context.Background(), f.opts)
	intakefixture.Must(t, err)
	t.Cleanup(func() { s.Close() })

	for _, id := range []config.ProjectID{intakefixture.Project, otherProject} {
		if _, active := s.runtimeOf(id); active == nil {
			t.Fatalf("project %s was not opened", id)
		}
		intakefixture.Await(t, "project "+string(id)+" turn", entered[id])
	}
	for _, id := range []config.ProjectID{intakefixture.Project, otherProject} {
		fixturekit.AwaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord {
			repo := f.repository(id)
			if repo == nil {
				return nil
			}
			ops, err := repo.Operations(f.streams[id])
			intakefixture.Must(t, err)
			return ops
		})
	}
}
