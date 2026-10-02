package thread

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// Advice ends the prompt the session receives and is recorded with the
// turn's response; the request keeps the prompt it was queued with.
func TestAdviceEndsThePromptAndIsRecorded(t *testing.T) {
	repo, _, _ := setup(t)
	req := queue(t, repo, "advised")
	var asked trace.TurnRequest
	var scope coreadapter.Scope
	runner := Runner{Store: repo, Now: func() time.Time { return timestamp.Add(time.Second) },
		Advise: func(_ context.Context, r trace.TurnRequest, s coreadapter.Scope) string {
			asked, scope = r, s
			return "Advice for this turn."
		},
		Turns: fakeTurns(func(_ context.Context, got coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
			if got.Prompt != req.Prompt+"\n\nAdvice for this turn." {
				t.Errorf("prompt %q", got.Prompt)
			}
			return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session"}, FinalResponse: "Read", Outcome: &coreadapter.Outcome{Status: "complete", Report: "Done"}}, nil
		})}
	q, err := runner.RunNext(context.Background(), stream, "agent", coreadapter.PreparedTurn{SessionDirectory: "/owned/advised"})
	if err != nil {
		t.Fatal(err)
	}
	if asked.ID != req.ID || scope.Turn != "advised" || scope.Role != "mason" || scope.Workstream != string(stream) {
		t.Fatalf("advice asked for %+v in %+v", asked, scope)
	}
	th, err := repo.Thread(stream, "agent")
	if err != nil || q.Response.Advice != "Advice for this turn." || th.Turns[0].Response.Advice != "Advice for this turn." || th.Turns[0].Request.Prompt != req.Prompt {
		t.Fatalf("recorded turn %+v %v", th.Turns, err)
	}
}

// A turn stopped while its advice is asked runs no session and records no
// advice.
func TestStopDuringAdviceRecordsNone(t *testing.T) {
	repo, _, _ := setup(t)
	queue(t, repo, "first")
	ctx, stop := Stoppable(context.Background())
	runner := Runner{Store: repo, Now: func() time.Time { return timestamp.Add(time.Second) },
		Advise: func(run context.Context, _ trace.TurnRequest, _ coreadapter.Scope) string {
			stop(&Stop{pauseStop})
			if run.Err() == nil {
				t.Error("the stop did not cancel the advice")
			}
			return "Advice nobody reads."
		},
		Turns: fakeTurns(func(context.Context, coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
			t.Error("a stopped turn ran its session")
			return coreadapter.SessionResult{}, nil
		})}
	q, err := runner.RunNext(ctx, stream, "agent", coreadapter.PreparedTurn{SessionDirectory: "/owned/stopped"})
	var s *Stop
	if !errors.As(err, &s) || q.Response == nil || q.Response.Advice != "" || q.Status() != "interrupted" {
		t.Fatalf("stopped turn %+v %v", q, err)
	}
	// A turn already stopped asks for no advice.
	queue(t, repo, "second")
	runner.Advise = func(context.Context, trace.TurnRequest, coreadapter.Scope) string {
		t.Error("a stopped turn asked for advice")
		return ""
	}
	if _, err := runner.RunNext(ctx, stream, "agent", coreadapter.PreparedTurn{SessionDirectory: "/owned/second"}); !errors.As(err, &s) || !strings.Contains(err.Error(), "Travelling") {
		t.Fatalf("second stopped turn: %v", err)
	}
}
