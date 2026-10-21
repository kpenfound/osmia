package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/coreadapter/adaptertest"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestAbandonCompetingUnclaimedInvocationsPreserveRunningClaim(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg, repo, clock := hardPauseRepository(t, 1)
	backend := &adaptertest.Turns{}
	s, a := hardPauseService(t, cfg, repo, clock, backend)
	s.cfg = cfg
	s.setSole(runtimeFor(repo))
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "handin", Revision: 1, Project: project, Workstream: stream, At: clock.Now(), Actor: ownerActor, Cause: "handin"}
	_, err := repo.SetFeatureState(ctx, h, HandedState, "handed in")
	must(t, err)
	th, err := repo.Thread(stream, "agent-0")
	must(t, err)
	next := th.Turns[0].Request
	next.ID, next.TurnID = "request_later", "later"
	_, err = repo.EnqueueTurn(ctx, next)
	must(t, err)

	entered, release := make(chan struct{}), make(chan struct{})
	releaseCompetitor := sync.OnceFunc(func() { close(release) })
	defer releaseCompetitor()
	competingDone := make(chan error, 1)
	d := a.Reconciler.(thread.Dispatcher)
	prepare := d.Prepare
	var preparations atomic.Int32
	d.Prepare = func(ctx context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
		th, err := repo.Thread(stream, "agent-0")
		if err != nil {
			return coreadapter.PreparedTurn{}, err
		}
		if th.Turns[0].Claim != nil || th.Turns[0].Response != nil {
			return coreadapter.PreparedTurn{}, fmt.Errorf("both invocations must observe an unclaimed turn: %+v", th)
		}
		// Both dispatchers have read the unclaimed turn. Hold the first
		// invocation here while the second acquires it and stays in Advise.
		if preparations.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-time.After(demoTimeout):
				return coreadapter.PreparedTurn{}, errors.New("competing invocation was not released")
			}
		}
		return prepare(ctx, in)
	}
	var claim trace.TurnClaim
	d.Runner.Advise = func(run context.Context, _ trace.TurnRequest, _ coreadapter.Scope) string {
		th, err := repo.Thread(stream, "agent-0")
		must(t, err)
		q := th.Turns[0]
		if preparations.Load() != 2 || q.Claim == nil || q.Response != nil || len(q.Attempts) != 0 {
			t.Fatalf("overlap did not precede the first attempt: %+v", th)
		}
		claim = *q.Claim
		if _, api := s.abandon(ctx, string(stream), AbandonRequest{Reason: "Superseded."}); api != nil {
			t.Fatal(api)
		}
		if run.Err() != context.Canceled {
			t.Fatal("abandon did not cancel the owning invocation")
		}
		releaseCompetitor()
		select {
		case err := <-competingDone:
			if !errors.Is(err, trace.ErrClaimed) {
				t.Fatalf("competitor did not lose acquisition: %v", err)
			}
		case <-time.After(demoTimeout):
			t.Fatal("competing invocation did not return before the owner")
		}
		th, err = repo.Thread(stream, "agent-0")
		must(t, err)
		if th.Active != "t-0" || *th.Turns[0].Claim != claim || th.Turns[0].Response != nil || !th.Turns[0].CompletedAt.IsZero() || th.Turns[1].Claim != nil || th.Turns[1].Response != nil || !th.Turns[1].CompletedAt.IsZero() {
			t.Fatalf("losing invocation completed the running owner or successor: %+v", th)
		}
		return ""
	}
	a.Reconciler = d
	op, err := thread.TurnOperation(project, "event-claim", thread.TurnInput{Workstream: stream, Agent: "agent-0", Turn: "t-0"})
	must(t, err)
	go func() {
		_, err := a.Apply(ctx, op)
		competingDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(demoTimeout):
		t.Fatal("competitor did not observe the unclaimed turn")
	}
	_, err = a.Apply(ctx, op)
	if !errors.Is(err, context.Canceled) || len(backend.Calls()) != 0 {
		t.Fatalf("owning invocation: %v, backend calls %d", err, len(backend.Calls()))
	}
	th, err = repo.Thread(stream, "agent-0")
	must(t, err)
	if th.Active != "" || *th.Turns[0].Claim != claim {
		t.Fatalf("owning invocation did not settle its returned claim: %+v", th)
	}
	for _, q := range th.Turns {
		if q.CompletedAt.IsZero() || q.Response == nil || !q.Response.Result.Cancelled || q.Response.Actor != abandonActor || q.Response.Failure != cancelReason {
			t.Fatalf("uncancelled turn: %+v", q)
		}
	}
	result, err := a.Apply(ctx, op)
	must(t, err)
	if result.Outcome != "interrupted" {
		t.Fatalf("retry: %+v", result)
	}
	responses, err := trace.Read[trace.TurnResponse](repo, stream)
	must(t, err)
	if len(responses) != 2 {
		t.Fatalf("cancellation responses duplicated: %+v", responses)
	}
	if _, api := s.archive(string(stream)); api != nil {
		t.Fatal(api)
	}
	removed := make(chan error, 1)
	cleanup := &archiveCleanup{S: s.lifecycle(), Workspaces: &workspaceCleanup{Cfg: cfg, Repository: repo, Now: s.now}, Finish: func(root config.Root, p config.ProjectID, w config.WorkstreamID) error {
		err := trace.FinishWorkstreamTraceDeletion(root, p, w)
		removed <- err
		return err
	}}
	must(t, repo.Serialize(func() error { return cleanup.Pass(ctx) }))
	select {
	case err := <-removed:
		must(t, err)
	case <-time.After(demoTimeout):
		t.Fatal("archive cleanup did not remove the cancelled turn's trace")
	}
	must(t, repo.Serialize(func() error { return cleanup.Pass(ctx) }))
	archived, ok := archiveRecord(t, s.store, stream)
	if !ok || archived.CleanedAt.IsZero() {
		t.Fatalf("archive cleanup did not complete in the same session: %+v", archived)
	}
	if gone, err := trace.WorkstreamTraceGone(cfg.Root, project, stream); err != nil || !gone {
		t.Fatalf("trace remains after cleanup: %v", err)
	}
}

func TestAbandonBeforeFirstAttemptReleasesReturnedClaim(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg, repo, clock := hardPauseRepository(t, 1)
	backend := &adaptertest.Turns{}
	s, a := hardPauseService(t, cfg, repo, clock, backend)
	s.cfg = cfg
	s.setSole(runtimeFor(repo))
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "handin", Revision: 1, Project: project, Workstream: stream, At: clock.Now(), Actor: ownerActor, Cause: "handin"}
	_, err := repo.SetFeatureState(ctx, h, HandedState, "handed in")
	must(t, err)
	th, err := repo.Thread(stream, "agent-0")
	must(t, err)
	next := th.Turns[0].Request
	next.ID, next.TurnID = "request_later", "later"
	_, err = repo.EnqueueTurn(ctx, next)
	must(t, err)
	var claim trace.TurnClaim
	advised := false
	d := a.Reconciler.(thread.Dispatcher)
	d.Runner.Advise = func(run context.Context, _ trace.TurnRequest, _ coreadapter.Scope) string {
		advised = true
		th, err := repo.Thread(stream, "agent-0")
		must(t, err)
		q := th.Turns[0]
		if th.Active != "t-0" || q.Claim == nil || q.Response != nil || len(q.Attempts) != 0 {
			t.Fatalf("injection must follow the claim and precede the first attempt: %+v", th)
		}
		claim = *q.Claim
		_, api := s.abandon(run, string(stream), AbandonRequest{Reason: "Superseded."})
		if api != nil {
			t.Fatal(api)
		}
		if run.Err() != context.Canceled {
			t.Fatal("abandon did not cancel the claimed turn")
		}
		// A competing operation must leave this invocation's still-running
		// claim alone, even though the workstream is already abandoned.
		competing, err := thread.TurnOperation(project, "competing-event", thread.TurnInput{Workstream: stream, Agent: "agent-0", Turn: "t-0"})
		must(t, err)
		if _, err := a.Apply(ctx, competing); err == nil {
			t.Fatal("a competing operation ran the claimed turn")
		}
		th, err = repo.Thread(stream, "agent-0")
		must(t, err)
		if th.Turns[0].Response != nil || !th.Turns[1].CompletedAt.IsZero() {
			t.Fatal("abandon completed a turn while its runner was executing")
		}
		return ""
	}
	a.Reconciler = d
	op, err := thread.TurnOperation(project, "event-claim", thread.TurnInput{Workstream: stream, Agent: "agent-0", Turn: "t-0"})
	must(t, err)
	_, err = a.Apply(ctx, op)
	if !errors.Is(err, context.Canceled) || !advised || len(backend.Calls()) != 0 {
		t.Fatalf("apply: %v, advice %t, backend calls %d", err, advised, len(backend.Calls()))
	}
	th, err = repo.Thread(stream, "agent-0")
	must(t, err)
	if th.Active != "" || *th.Turns[0].Claim != claim || len(th.Turns[0].Attempts) != 0 {
		t.Fatalf("returned claim not retained and released: %+v", th)
	}
	for _, q := range th.Turns {
		if q.CompletedAt.IsZero() || q.Response == nil || !q.Response.Result.Cancelled || q.Response.Failure != cancelReason || q.Response.Actor != abandonActor {
			t.Fatalf("uncancelled turn: %+v", q)
		}
	}
	observation, err := a.Inspect(ctx, op)
	must(t, err)
	if observation.State != coreadapter.EffectCompleted || observation.Result == nil || observation.Result.Outcome != "interrupted" {
		t.Fatalf("retry inspection: %+v", observation)
	}
	result, err := a.Apply(ctx, op)
	must(t, err)
	if result.Outcome != "interrupted" || len(backend.Calls()) != 0 {
		t.Fatalf("retry: %+v", result)
	}
	responses, err := trace.Read[trace.TurnResponse](repo, stream)
	must(t, err)
	if len(responses) != 2 {
		t.Fatalf("responses duplicated on retry: %+v", responses)
	}
	if _, api := s.archive(string(stream)); api != nil {
		t.Fatal(api)
	}
	removed := make(chan error, 1)
	cleanup := &archiveCleanup{S: s.lifecycle(), Workspaces: &workspaceCleanup{Cfg: cfg, Repository: repo, Now: s.now}, Finish: func(root config.Root, p config.ProjectID, w config.WorkstreamID) error {
		err := trace.FinishWorkstreamTraceDeletion(root, p, w)
		removed <- err
		return err
	}}
	// No restart or fabricated operation result is needed for cleanup.
	must(t, repo.Serialize(func() error { return cleanup.Pass(ctx) }))
	select {
	case err := <-removed:
		must(t, err)
	case <-time.After(demoTimeout):
		t.Fatal("archive cleanup did not remove the cancelled turn's trace")
	}
	must(t, repo.Serialize(func() error { return cleanup.Pass(ctx) }))
	archived, ok := archiveRecord(t, s.store, stream)
	if !ok || archived.CleanedAt.IsZero() {
		t.Fatalf("archive cleanup did not complete in the same session: %+v", archived)
	}
	if gone, err := trace.WorkstreamTraceGone(cfg.Root, project, stream); err != nil || !gone {
		t.Fatalf("trace remains after cleanup: %v", err)
	}
}

func TestCancelledClaimWithoutAbandonWaitsForRecovery(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg, repo, clock := hardPauseRepository(t, 1)
	backend := &adaptertest.Turns{}
	_, a := hardPauseService(t, cfg, repo, clock, backend)
	d := a.Reconciler.(thread.Dispatcher)
	d.Runner.Advise = func(context.Context, trace.TurnRequest, coreadapter.Scope) string {
		cancel()
		return ""
	}
	a.Reconciler = d
	op, err := thread.TurnOperation(project, "event-shutdown", thread.TurnInput{Workstream: stream, Agent: "agent-0", Turn: "t-0"})
	must(t, err)
	_, err = a.Apply(ctx, op)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("apply: %v", err)
	}
	th, err := repo.Thread(stream, "agent-0")
	must(t, err)
	q := th.Turns[0]
	if th.Active != "t-0" || q.Claim == nil || q.Response != nil || len(q.Attempts) != 0 || !q.CompletedAt.IsZero() {
		t.Fatalf("shutdown claim was not preserved for recovery: %+v", th)
	}
}
