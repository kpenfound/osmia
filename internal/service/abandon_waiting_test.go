package service

import (
	"context"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestAbandonRunningTurnReturningWaitingSettlesOperation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg, repo, clock := hardPauseRepository(t, 1)
	s, a := hardPauseService(t, cfg, repo, clock, nil)
	s.cfg = cfg
	s.setSole(runtimeFor(repo))
	d := a.Reconciler.(thread.Dispatcher)
	calls := 0
	d.Runner.Turns = turnFunc(func(run context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		calls++
		th, err := repo.Thread(stream, "agent-0")
		must(t, err)
		if th.Active != "t-0" || len(th.Turns[0].Attempts) != 1 {
			t.Fatalf("turn did not start before abandon: %+v", th)
		}
		if _, api := s.abandon(ctx, string(stream), AbandonRequest{Reason: "Superseded while running."}); api != nil {
			t.Fatal(api)
		}
		if run.Err() != context.Canceled {
			t.Fatal("abandon did not cancel the running turn")
		}
		// A backend can finish cleanly despite its cancelled execution context.
		// Keep that captured response and settle its operation from real evidence.
		return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "waiting"}, Outcome: &coreadapter.Outcome{Status: "waiting", Report: "Waiting for the routed decision."}}, nil
	})
	a.Reconciler = d
	th, err := repo.Thread(stream, "agent-0")
	must(t, err)
	next := th.Turns[0].Request
	next.ID, next.TurnID = "request_later", "later"
	_, err = repo.EnqueueTurn(ctx, next)
	must(t, err)
	op, err := thread.TurnOperation(project, "waiting-turn", thread.TurnInput{Workstream: stream, Agent: "agent-0", Turn: "t-0"})
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "handin", Revision: 1, Project: project, Workstream: stream, At: clock.Now(), Actor: ownerActor, Cause: "handin"}
	_, err = repo.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: h, Subject: trace.FeatureSubject, To: HandedState, Reason: "Handed in"}, Events: []trace.Event{{ID: "waiting-turn", Kind: "turn", Body: "Run the admitted mason", Operation: &op}}})
	must(t, err)
	controller, err := reconcile.New(repo, reconcile.Options{Worker: "test", Now: clock.Now, Adapters: map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RunnerBoundary: a}})
	must(t, err)
	must(t, controller.Pass(ctx))
	records, err := repo.Operations(stream)
	must(t, err)
	if len(records) != 1 || !records[0].Acknowledged || records[0].Result == nil {
		logUnsettledLifecycleOperations(t, repo, stream)
		t.Fatalf("returned turn stranded its operation: %+v", records)
	}
	th, err = repo.Thread(stream, "agent-0")
	must(t, err)
	if th.Turns[0].Response == nil || th.Turns[0].Response.Result.Outcome == nil || th.Turns[0].Response.Result.Outcome.Status != "waiting" || th.Turns[0].Status() != "waiting" || th.Turns[0].CompletedAt.IsZero() {
		t.Fatalf("captured waiting response changed: %+v", th)
	}
	if th.Turns[1].Response == nil || !th.Turns[1].Response.Result.Cancelled || th.Turns[1].CompletedAt.IsZero() {
		t.Fatalf("later turn not cancelled: %+v", th)
	}
	must(t, controller.Pass(ctx))
	if calls != 1 {
		t.Fatalf("completed waiting turn ran %d times", calls)
	}
	if _, api := s.archive(string(stream)); api != nil {
		t.Fatal(api)
	}
	removed := make(chan error, 1)
	cleanup := &archiveCleanup{s: s, workspaces: &workspaceCleanup{cfg: cfg, repository: repo, now: s.now}, finish: func(root config.Root, p config.ProjectID, w config.WorkstreamID) error {
		err := trace.FinishWorkstreamTraceDeletion(root, p, w)
		removed <- err
		return err
	}}
	must(t, repo.Serialize(func() error { return cleanup.Pass(ctx) }))
	select {
	case err := <-removed:
		must(t, err)
	case <-time.After(demoTimeout):
		t.Fatal("eligible archive cleanup did not finish")
	}
	must(t, repo.Serialize(func() error { return cleanup.Pass(ctx) }))
	archived, found := archiveRecord(t, s.store, stream)
	if !found || archived.CleanedAt.IsZero() {
		t.Fatalf("waiting operation blocked same-session cleanup: %+v", archived)
	}
	if gone, err := trace.WorkstreamTraceGone(cfg.Root, project, stream); err != nil || !gone {
		t.Fatalf("cleanup did not remove the retired trace: %v", err)
	}
}
