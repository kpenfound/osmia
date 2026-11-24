package executionjourney

import (
	"context"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestFailedReviewTurnContestsTheUnit(t *testing.T) {
	t.Parallel()
	f, stream, repository := newReviewFixture(t, "failed-review-turn")
	r := &reviewers{masons: newMasonController(f.s, repository)}
	ctx := context.Background()
	subject := trace.UnitSubject("resume")
	agent := intake.ReviewerAgent("resume")
	state, err := repository.Workflow(stream, subject)
	fixturekit.Must(t, err)
	fixturekit.Must(t, r.one(ctx, stream, "resume", state, false))
	th, err := repository.Thread(stream, agent)
	fixturekit.Must(t, err)
	token := "timed-out-" + th.Turns[len(th.Turns)-1].Request.TurnID
	q, err := repository.ClaimTurn(ctx, stream, agent, token, t.TempDir(), f.clock.Now())
	fixturekit.Must(t, err)
	h := q.Request.Header
	h.Schema, h.ID, h.At = "osmia.trace.turn-response", trace.EventID(q.Request.ID, "response"), f.clock.Now()
	response := trace.TurnResponse{Header: h, AgentID: agent, ThreadID: agent, TurnID: q.Request.TurnID, RequestID: q.Request.ID, RequestRevision: q.Request.Revision,
		Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: q.Request.Profile.Backend, ID: token}, SessionDirectory: q.Claim.SessionDirectory, StartedAt: q.Claim.At, TimedOut: true}}
	fixturekit.Must(t, repository.CaptureTurn(ctx, q.Claim.Token, response))
	fixturekit.Must(t, repository.CompleteTurn(ctx, stream, agent, q.Request.TurnID, q.Claim.Token, f.clock.Now()))

	for range 2 {
		fixturekit.Must(t, r.one(ctx, stream, "resume", state, false))
	}
	got, err := repository.Workflow(stream, subject)
	fixturekit.Must(t, err)
	if got.Value != intake.UnitContested {
		t.Fatalf("after a failed review turn the unit is %s", got.Value)
	}
	transitions := intakefixture.AllTransitions(t, f.s.cfg.Root, f.project, stream)
	contest := transitions[len(transitions)-1]
	if !execution.FailedReview(contest, "resume") || !strings.Contains(contest.Reason, "ended without a verdict because it ran out of time") {
		t.Fatalf("the contest %+v", contest)
	}
	contests := 0
	for _, tr := range transitions {
		if tr.Subject == subject && tr.To == intake.UnitContested {
			contests++
		}
	}
	if contests != 1 {
		t.Fatalf("the failed turn contested the unit %d times", contests)
	}

	if _, apiErr := f.s.recordContestedRuling(ctx, repository, stream, "resume", api.ContestedRulingRequest{Decision: "review", Note: "Review it again."}, intake.ChiefActor); apiErr != nil {
		t.Fatalf("the chief's ruling: %+v", apiErr)
	}
	state, err = repository.Workflow(stream, subject)
	fixturekit.Must(t, err)
	if state.Value != intake.UnitReviewing {
		t.Fatalf("after the ruling the unit is %s", state.Value)
	}
	fixturekit.Must(t, r.one(ctx, stream, "resume", state, false))
	ids := intakefixture.TurnIDs(t, repository, stream, agent)
	if len(ids) != 2 || slices.Index(ids, q.Request.TurnID) != 0 {
		t.Fatalf("reviewer turns after the ruling %v", ids)
	}
}
