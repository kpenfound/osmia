package fixturekit

import (
	"context"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// QueueRoleTurn creates the agent's thread in Stream with the role and queues
// one turn on it, for unit.
func QueueRoleTurn(t *testing.T, repo *trace.Repository, Stream config.WorkstreamID, agent, role, unit string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	owner := trace.Actor{Kind: "owner", ID: "local"}
	Must(t, repo.CreateThread(ctx, trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: agent, Project: Project, Workstream: Stream, At: at, Actor: owner, Cause: "workstream_created"}, Role: role, ThreadID: agent}))
	_, err := repo.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_" + agent, Project: Project, Workstream: Stream, Unit: unit, At: at, Actor: owner, Cause: "message_" + agent, Depth: 1},
		AgentID: agent, ThreadID: agent, TurnID: agent + "-turn", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Work"})
	Must(t, err)
}
