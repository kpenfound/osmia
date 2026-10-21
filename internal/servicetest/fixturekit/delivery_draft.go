package fixturekit

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// SetStatusGoal records goal as the workstream's chief-of-staff status, the
// value workstreamTitle (and archiveTitle and status) read as the
// workstream's title.
func SetStatusGoal(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, goal string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	_, err := repository.EnsureChiefOfStaff(ctx, stream, at, ServiceActor)
	Must(t, err)
	turn := fmt.Sprintf("status-%d", at.UnixNano())
	h := trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request-" + turn, Revision: 1, Project: repository.Project(), Workstream: stream, At: at, Actor: ServiceActor, Cause: "fixture"}
	_, err = repository.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: turn, Profile: coreadapter.Profile{Name: "fake", Backend: "fake", Model: "fake"}, Prompt: "Set status"})
	Must(t, err)
	session := t.TempDir()
	_, err = repository.ClaimTurn(ctx, stream, trace.ChiefOfStaff, turn+"-token", session, at)
	Must(t, err)
	_, err = repository.SetStatus(ctx, trace.ChiefOfStaff, coreadapter.Scope{Project: string(repository.Project()), Workstream: string(stream), Role: trace.ChiefOfStaff, Thread: trace.ChiefOfStaff, Turn: turn},
		trace.StatusContent{Goal: goal, Note: "status", Agents: []string{}}, at, func(trace.StatusContent, []string, []trace.OwnerGate) error { return nil })
	Must(t, err)
}

// RecordHanded records a handed document for stream with the given recorded
// source, as hand-in would, so draftDelivery can read it.
func RecordHanded(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, source string, at time.Time) {
	t.Helper()
	Must(t, repository.Append(context.Background(), trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "handed", Revision: 1, Project: repository.Project(), Workstream: stream, At: at, Actor: OwnerActor, Cause: "fixture"},
		Path: "handed/input.md", Content: "# Handed issue title\n\nBody.\n", Source: source}))
}
