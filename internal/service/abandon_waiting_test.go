package service

import (
	"context"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
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
		th, err := repo.Thread(fixturekit.Stream, "agent-0")
		fixturekit.Must(t, err)
		if th.Active != "t-0" || len(th.Turns[0].Attempts) != 1 {
			t.Fatalf("turn did not start before abandon: %+v", th)
		}
		if _, api := s.abandon(ctx, string(fixturekit.Stream), AbandonRequest{Reason: "Superseded while running."}); api != nil {
			t.Fatal(api)
		}
		if run.Err() != context.Canceled {
			t.Fatal("abandon did not cancel the running turn")
		}
		return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "waiting"}, Outcome: &coreadapter.Outcome{Status: "waiting", Report: "Waiting for the routed decision."}}, nil
	})
	a.Reconciler = d
	th, err := repo.Thread(fixturekit.Stream, "agent-0")
	fixturekit.Must(t, err)
	next := th.Turns[0].Request
	next.ID, next.TurnID = "request_later", "later"
	_, err = repo.EnqueueTurn(ctx, next)
	fixturekit.Must(t, err)
	op, err := thread.TurnOperation(fixturekit.Project, "waiting-turn", thread.TurnInput{Workstream: fixturekit.Stream, Agent: "agent-0", Turn: "t-0"})
	fixturekit.Must(t, err)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "handin", Revision: 1, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: clock.Now(), Actor: ownerActor, Cause: "handin"}
	_, err = repo.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: h, Subject: trace.FeatureSubject, To: HandedState, Reason: "Handed in"}, Events: []trace.Event{{ID: "waiting-turn", Kind: "turn", Body: "Run the admitted mason", Operation: &op}}})
	fixturekit.Must(t, err)
	controller, err := reconcile.New(repo, reconcile.Options{Worker: "test", Now: clock.Now, Adapters: map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RunnerBoundary: a}})
	fixturekit.Must(t, err)
	fixturekit.Must(t, controller.Pass(ctx))
	records, err := repo.Operations(fixturekit.Stream)
	fixturekit.Must(t, err)
	if len(records) != 1 || !records[0].Acknowledged || records[0].Result == nil {
		logUnsettledLifecycleOperations(t, repo, fixturekit.Stream)
		t.Fatalf("returned turn stranded its operation: %+v", records)
	}
	th, err = repo.Thread(fixturekit.Stream, "agent-0")
	fixturekit.Must(t, err)
	if th.Turns[0].Response == nil || th.Turns[0].Response.Result.Outcome == nil || th.Turns[0].Response.Result.Outcome.Status != "waiting" || th.Turns[0].Status() != "waiting" || th.Turns[0].CompletedAt.IsZero() {
		t.Fatalf("captured waiting response changed: %+v", th)
	}
	if th.Turns[1].Response == nil || !th.Turns[1].Response.Result.Cancelled || th.Turns[1].CompletedAt.IsZero() {
		t.Fatalf("later turn not cancelled: %+v", th)
	}
	fixturekit.Must(t, controller.Pass(ctx))
	if calls != 1 {
		t.Fatalf("completed waiting turn ran %d times", calls)
	}
	if _, api := s.archive(string(fixturekit.Stream)); api != nil {
		t.Fatal(api)
	}
	removed := make(chan error, 1)
	cleanup := &archiveCleanup{S: s.lifecycle(), Workspaces: &workspaceCleanup{Cfg: cfg, Repository: repo, Now: s.now}, Finish: func(root config.Root, p config.ProjectID, w config.WorkstreamID) error {
		err := trace.FinishWorkstreamTraceDeletion(root, p, w)
		removed <- err
		return err
	}}
	fixturekit.Must(t, cleanup.Pass(ctx))
	select {
	case err := <-removed:
		fixturekit.Must(t, err)
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("eligible archive cleanup did not finish")
	}
	fixturekit.Must(t, cleanup.Pass(ctx))
	archived, found := archiveRecord(t, s.store, fixturekit.Stream)
	if !found || archived.CleanedAt.IsZero() {
		t.Fatalf("waiting operation blocked same-session cleanup: %+v", archived)
	}
	if gone, err := trace.WorkstreamTraceGone(cfg.Root, fixturekit.Project, fixturekit.Stream); err != nil || !gone {
		t.Fatalf("cleanup did not remove the retired trace: %v", err)
	}
}
