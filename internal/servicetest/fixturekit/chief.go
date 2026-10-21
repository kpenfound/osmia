package fixturekit

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// answerChief captures final as the response of the workstream's claimed
// chief-of-staff turn and completes it, as a finished turn of the chief of
// staff would.
func AnswerChief(t *testing.T, repo *trace.Repository, ws config.WorkstreamID, turn, final string) {
	t.Helper()
	ctx := context.Background()
	thread, err := repo.ChiefOfStaffThread(ws)
	Must(t, err)
	i := slices.IndexFunc(thread.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == turn })
	if i < 0 || thread.Turns[i].Claim == nil {
		t.Fatalf("no claimed chief-of-staff turn %s", turn)
	}
	q := thread.Turns[i]
	h := q.Request.Header
	h.Schema, h.ID, h.At = "osmia.trace.turn-response", trace.EventID(q.Request.ID, "response"), time.Now().UTC()
	response := trace.TurnResponse{Header: h, AgentID: trace.ChiefOfStaff, ThreadID: q.Request.ThreadID, TurnID: turn, RequestID: q.Request.ID, RequestRevision: q.Request.Revision,
		Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: q.Request.Profile.Backend, ID: q.Claim.Token}, SessionDirectory: q.Claim.SessionDirectory, StartedAt: q.Claim.At, FinalResponse: final}}
	Must(t, repo.CaptureTurn(ctx, q.Claim.Token, response))
	Must(t, repo.CompleteTurn(ctx, ws, trace.ChiefOfStaff, turn, q.Claim.Token, time.Now().UTC()))
}
