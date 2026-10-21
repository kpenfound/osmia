package fixturekit

import (
	"context"
	"fmt"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
	"testing"
)

// HardPauseRepository creates the demo project's trace with the workstream
// and n mason threads, agent-<i>, each with its turn t-<i> queued.
func HardPauseRepository(t *testing.T, n int) (*config.Config, *trace.Repository, *DemoClock) {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	cfg, err := config.Load(ConfigAt(t, home))
	Must(t, err)
	clock := &DemoClock{At: DemoStart}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, clock.Now(), owner)
	Must(t, err)
	t.Cleanup(func() { repo.Close() })
	Must(t, repo.CreateWorkstream(ctx, Stream, clock.Now(), owner))
	for i := range n {
		agent := fmt.Sprintf("agent-%d", i)
		identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: agent, Project: Project, Workstream: Stream, At: clock.Now(), Actor: owner, Cause: "workstream_created"}, Role: MasonRole, ThreadID: agent}
		Must(t, repo.CreateThread(ctx, identity))
		turn := fmt.Sprintf("t-%d", i)
		req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_" + turn, Project: Project, Workstream: Stream, At: clock.Now(), Actor: owner, Cause: "message_" + turn, Depth: 1},
			AgentID: agent, ThreadID: agent, TurnID: turn, Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Work " + turn}
		_, err := repo.EnqueueTurn(ctx, req)
		Must(t, err)
	}
	return cfg, repo, clock
}
