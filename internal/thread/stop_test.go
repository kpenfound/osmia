package thread

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

var pauseStop = trace.TurnStop{Cause: trace.TurnStopHardPause, Scope: "workstream", Source: "owner", Reason: "Travelling"}

// resumeAll accepts every session the runner asks to resume.
func resumeAll(context.Context, coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error {
	return nil
}

// runStopped runs the thread's next turn under a stoppable context, stopping
// it before the session starts when early, or from inside the session.
func runStopped(t *testing.T, repo *trace.Repository, early bool) (trace.QueuedTurn, int32) {
	t.Helper()
	ctx, stop := Stoppable(context.Background())
	if early {
		stop(&Stop{pauseStop})
	}
	var calls atomic.Int32
	runner := Runner{Store: repo, Now: func() time.Time { return timestamp.Add(time.Second) }, Turns: fakeTurns(func(run context.Context, _ coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		calls.Add(1)
		stop(&Stop{pauseStop})
		<-run.Done()
		return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "stopped"}, FinalResponse: "Half done", IsError: true, Signal: 15}, run.Err()
	})}
	q, err := runner.RunNext(ctx, stream, "agent", coreadapter.PreparedTurn{SessionDirectory: "/owned/stopped"})
	var s *Stop
	if !errors.As(err, &s) || s.TurnStop != pauseStop {
		t.Fatalf("stopped turn error: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("stopping cancelled the persistence context")
	}
	return q, calls.Load()
}

// checkStopped checks that the turn completed interrupted with its stop and
// no failure, and that the durable record says the same after a reopen.
func checkStopped(t *testing.T, repo *trace.Repository, q trace.QueuedTurn) {
	t.Helper()
	r := q.Response
	if r == nil || q.CompletedAt.IsZero() || q.Status() != "interrupted" || r.Stop == nil || *r.Stop != pauseStop || r.Failure != "" || r.FailureClass != "" || r.Classification != nil || !r.Result.Cancelled {
		t.Fatalf("stopped turn: %+v", q)
	}
	for _, a := range q.Attempts {
		if a.Failure != "" || a.FailureClass != "" {
			t.Fatalf("stopped attempt records a failure: %+v", a)
		}
	}
	responses, err := trace.Read[trace.TurnResponse](repo, stream)
	if err != nil || !reflect.DeepEqual(responses[len(responses)-1], *r) {
		t.Fatalf("recorded response: %+v %v", responses, err)
	}
	th, err := repo.Thread(stream, "agent")
	if err != nil || th.Status != "interrupted" || th.Active != "" {
		t.Fatalf("stopped thread: %+v %v", th, err)
	}
}

func TestStopDuringSessionInterruptsWithoutFailure(t *testing.T) {
	repo, root, p := setup(t)
	queue(t, repo, "first")
	queue(t, repo, "second")
	q, calls := runStopped(t, repo, false)
	if calls != 1 || len(q.Attempts) != 1 || q.Response.Result.Session.ID != "stopped" || q.Response.Result.FinalResponse != "Half done" {
		t.Fatalf("stopped turn lost its partial result: calls %d, %+v", calls, q)
	}
	checkStopped(t, repo, q)
	repo.Close()
	repo, err := trace.Open(root, p)
	if err != nil {
		t.Fatalf("reopen with a stopped turn: %v", err)
	}
	defer repo.Close()
	checkStopped(t, repo, q)

	// The queued turn runs next on the same thread and resumes the stopped
	// session.
	var resumed *coreadapter.BackendSession
	runner := Runner{Store: repo, Now: func() time.Time { return timestamp.Add(2 * time.Second) }, Turns: resumableTurns{check: resumeAll, run: func(_ context.Context, prepared coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		resumed = prepared.Resume
		return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "stopped"}, FinalResponse: "Finished"}, nil
	}}}
	next, err := runner.RunNext(context.Background(), stream, "agent", coreadapter.PreparedTurn{SessionDirectory: "/owned/second"})
	if err != nil {
		t.Fatal(err)
	}
	if next.Request.TurnID != "second" || resumed == nil || resumed.ID != "stopped" || next.Attempts[0].Path != "resume" || next.Attempts[0].SourceSequence != q.Sequence {
		t.Fatalf("continuation did not resume the stopped session: resumed %+v, %+v", resumed, next.Attempts)
	}
}

func TestStopBeforeSessionRunsNothing(t *testing.T) {
	repo, _, _ := setup(t)
	queue(t, repo, "earlier")
	queue(t, repo, "first")
	queue(t, repo, "second")
	runner := Runner{Store: repo, Now: func() time.Time { return timestamp.Add(time.Second) }, Turns: fakeTurns(func(context.Context, coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "earlier"}, FinalResponse: "Earlier"}, nil
	})}
	if _, err := runner.RunNext(context.Background(), stream, "agent", coreadapter.PreparedTurn{SessionDirectory: "/owned/earlier"}); err != nil {
		t.Fatal(err)
	}
	q, calls := runStopped(t, repo, true)
	if calls != 0 || len(q.Attempts) != 0 || q.Response.Result.Session != (coreadapter.BackendSession{}) {
		t.Fatalf("turn stopped before its session ran: calls %d, %+v", calls, q)
	}
	checkStopped(t, repo, q)

	// The turn before the stopped one is the source of the next.
	var resumed *coreadapter.BackendSession
	runner.Turns = resumableTurns{check: resumeAll, run: func(_ context.Context, prepared coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		resumed = prepared.Resume
		return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "earlier"}, FinalResponse: "Finished"}, nil
	}}
	next, err := runner.RunNext(context.Background(), stream, "agent", coreadapter.PreparedTurn{SessionDirectory: "/owned/second"})
	if err != nil {
		t.Fatal(err)
	}
	if resumed == nil || resumed.ID != "earlier" || next.Attempts[0].SourceSequence != 1 {
		t.Fatalf("continuation source: resumed %+v, %+v", resumed, next.Attempts)
	}
}

func TestSessionEndingBeforeStopKeepsItsResult(t *testing.T) {
	repo, _, _ := setup(t)
	queue(t, repo, "first")
	ctx, stop := Stoppable(context.Background())
	runner := Runner{Store: repo, Now: func() time.Time { return timestamp.Add(time.Second) }, Turns: fakeTurns(func(context.Context, coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		stop(&Stop{pauseStop})
		return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "done"}, FinalResponse: "Done", Outcome: &coreadapter.Outcome{Status: "done", Report: "Built"}}, nil
	})}
	q, err := runner.RunNext(ctx, stream, "agent", coreadapter.PreparedTurn{SessionDirectory: "/owned/first"})
	if err != nil || q.Response.Stop != nil || q.Status() != "idle" || q.Response.Result.Outcome == nil {
		t.Fatalf("finished session recorded as stopped: %+v %v", q, err)
	}
}

func TestCancellationWithoutStopStaysAFailure(t *testing.T) {
	repo, _, _ := setup(t)
	queue(t, repo, "first")
	parent, cancel := context.WithCancel(context.Background())
	ctx, _ := Stoppable(parent)
	runner := Runner{Store: repo, Now: func() time.Time { return timestamp.Add(time.Second) }, Turns: fakeTurns(func(run context.Context, _ coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		cancel()
		<-run.Done()
		return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "cancelled"}}, run.Err()
	})}
	q, err := runner.RunNext(ctx, stream, "agent", coreadapter.PreparedTurn{SessionDirectory: "/owned/first"})
	if !errors.Is(err, context.Canceled) || q.Response.Stop != nil || q.Response.Failure == "" || q.Status() != "interrupted" {
		t.Fatalf("cancelled turn: %+v %v", q, err)
	}
}
