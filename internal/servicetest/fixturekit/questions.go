package fixturekit

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// ClaimTurn queues and claims a turn of the agent in an offline trace and
// returns the turn's scope, so a test can call the question writes directly.
func ClaimTurn(t *testing.T, repo *trace.Repository, home string, ws config.WorkstreamID, agent, thread, turn string, at time.Time) coreadapter.Scope {
	t.Helper()
	ctx := context.Background()
	h := trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_" + turn, Project: Project, Workstream: ws, At: at, Actor: trace.Actor{Kind: "owner", ID: "local"}, Cause: "message_" + turn}
	_, err := repo.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: agent, ThreadID: thread, TurnID: turn, Profile: coreadapter.Profile{Name: "other", Backend: "codex", Model: "other"}, Prompt: "Work"})
	Must(t, err)
	_, err = repo.ClaimTurn(ctx, ws, agent, "token_"+turn, filepath.Join(home, turn), at)
	Must(t, err)
	th, err := repo.Thread(ws, agent)
	Must(t, err)
	return coreadapter.Scope{Project: string(Project), Workstream: string(ws), Thread: thread, Turn: turn, Role: th.Identity.Role}
}
