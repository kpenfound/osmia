package intakefixture

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

// ChiefTurnInput supplies the request and claim identity of a test turn.
type ChiefTurnInput struct {
	Turn, Prompt, Cause, Token, Directory string
	Actor                                 trace.Actor
}

// ClaimChiefTurn ensures the chief thread, queues its request and claims it.
// Each trace operation receives a separate clock reading.
func ClaimChiefTurn(t *testing.T, repository *trace.Repository, project config.ProjectID, stream config.WorkstreamID, now func() time.Time, in ChiefTurnInput) coreadapter.Scope {
	t.Helper()
	ctx := context.Background()
	_, err := repository.EnsureChiefOfStaff(ctx, stream, now(), fixturekit.ServiceActor)
	Must(t, err)
	_, err = repository.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + in.Turn, Revision: 1, Project: project, Workstream: stream, At: now(), Actor: in.Actor, Cause: in.Cause},
		AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: in.Turn, Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "chief", Prompt: in.Prompt})
	Must(t, err)
	_, err = repository.ClaimTurn(ctx, stream, trace.ChiefOfStaff, in.Token, filepath.Join(t.TempDir(), in.Directory), now())
	Must(t, err)
	return coreadapter.Scope{Project: string(project), Workstream: string(stream), Role: trace.ChiefOfStaff, Thread: trace.ChiefOfStaff, Turn: in.Turn}
}
