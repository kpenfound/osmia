package trace

import (
	"context"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
)

// A turn response attributed to the Beekeeper actor kind is accepted, the
// same way an owner, service or agent actor is, and an unknown kind is
// still refused.
func TestBeekeeperActorKindIsAccepted(t *testing.T) {
	ctx := context.Background()
	r, _, _ := create(t)
	agent := Agent{Header: header("agent", "beekeeper"), Role: "beekeeper", ThreadID: "beekeeper"}
	if err := r.CreateThread(ctx, agent); err != nil {
		t.Fatal(err)
	}
	req := TurnRequest{Header: header("turn-request", "request_1"), AgentID: "beekeeper", ThreadID: "beekeeper", TurnID: "turn_1", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "List the workstreams."}
	if _, err := r.EnqueueTurn(ctx, req); err != nil {
		t.Fatal(err)
	}
	q, err := r.ClaimTurn(ctx, streamID, "beekeeper", "token", "/owned/session/token", at)
	if err != nil {
		t.Fatal(err)
	}
	h := req.Header
	h.Schema, h.ID, h.At, h.Actor = "osmia.trace.turn-response", "response_1", at.Add(time.Second), Actor{Kind: "beekeeper", ID: "beekeeper"}
	resp := TurnResponse{Header: h, AgentID: "beekeeper", ThreadID: "beekeeper", TurnID: "turn_1", RequestID: req.ID, RequestRevision: 1, Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session"}, SessionDirectory: q.Claim.SessionDirectory, StartedAt: at, Duration: time.Second, FinalResponse: "Three workstreams are idle."}}
	if err := r.CaptureTurn(ctx, q.Claim.Token, resp); err != nil {
		t.Fatal(err)
	}
	if err := r.CompleteTurn(ctx, streamID, "beekeeper", "turn_1", q.Claim.Token, at.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	th, err := r.Thread(streamID, "beekeeper")
	if err != nil {
		t.Fatal(err)
	}
	if th.Turns[0].Response.Actor != (Actor{Kind: "beekeeper", ID: "beekeeper"}) {
		t.Fatalf("beekeeper actor not retained: %+v", th.Turns[0].Response.Actor)
	}

	if validActor(Actor{Kind: "hive", ID: "beekeeper"}) {
		t.Fatal("an unknown actor kind must still be refused")
	}
}
