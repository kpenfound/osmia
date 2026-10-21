package lifecycle_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// hardPauseRepository creates the demo project's trace with the workstream
// and n mason threads, agent-<i>, each with its turn t-<i> queued.
func hardPauseRepository(t *testing.T, n int) (*config.Config, *trace.Repository, *demoClock) {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	cfg, err := config.Load(fixtureAt(t, home).Config)
	must(t, err)
	clock := &demoClock{At: demoStart}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, clock.Now(), owner)
	must(t, err)
	t.Cleanup(func() { repo.Close() })
	must(t, repo.CreateWorkstream(ctx, stream, clock.Now(), owner))
	for i := range n {
		agent := fmt.Sprintf("agent-%d", i)
		identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: agent, Project: project, Workstream: stream, At: clock.Now(), Actor: owner, Cause: "workstream_created"}, Role: masonRole, ThreadID: agent}
		must(t, repo.CreateThread(ctx, identity))
		turn := fmt.Sprintf("t-%d", i)
		req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_" + turn, Project: project, Workstream: stream, At: clock.Now(), Actor: owner, Cause: "message_" + turn, Depth: 1},
			AgentID: agent, ThreadID: agent, TurnID: turn, Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Work " + turn}
		_, err := repo.EnqueueTurn(ctx, req)
		must(t, err)
	}
	return cfg, repo, clock
}
