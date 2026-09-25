package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

const pauseNote = "A hard pause stopped your last turn."

func ownerPause(scope string, p config.ProjectID, w config.WorkstreamID, mode, reason string) runtime.Pause {
	return runtime.Pause{Target: runtime.Target{Scope: scope, Project: p, Workstream: w}, Mode: mode, Reason: reason, Source: runtime.PauseOwner, SetAt: time.Now().UTC()}
}

// Every scope of a pause holds the reconciler operations of the workstreams it
// covers, and no others: not those of a workstream outside it, not the turn
// operations the scheduler gates, and not those of an abandoned workstream.
func TestPauseHoldsReconcilerOperationsInItsScope(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg, repo, clock := hardPauseRepository(t, 0)
	owner := trace.Actor{Kind: "owner", ID: "local"}
	must(t, repo.CreateWorkstream(ctx, sibling, clock.Now(), owner))
	s, _ := hardPauseService(t, cfg, repo, clock, nil)
	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: []config.WorkstreamID{stream, sibling}})
	must(t, err)
	t.Cleanup(func() { store.Close() })
	s.store = store
	hold := s.holding(repo)
	actions := []string{DraftAction, AmendmentDraftAction, RoundAction, ReplyAction, RedraftAction, AmendmentRoundAction, AmendmentReplyAction, FinalReviewAction}
	turn, err := thread.TurnOperation(project, "event", thread.TurnInput{Workstream: stream, Agent: "agent-0", Turn: "t-0"})
	must(t, err)
	check := func(what string, want map[config.WorkstreamID]bool) {
		t.Helper()
		for _, w := range []config.WorkstreamID{stream, sibling} {
			for _, action := range actions {
				op := coreadapter.Operation{ID: "op", Boundary: coreadapter.RunnerBoundary, Action: action}
				if got := hold(w, op); got != want[w] {
					t.Fatalf("%s: %s of %s held %v, want %v", what, action, w, got, want[w])
				}
				if hold(w, coreadapter.Operation{ID: "op", Boundary: coreadapter.RepositoryBoundary, Action: action}) {
					t.Fatalf("%s: a repository operation named %s is held", what, action)
				}
			}
			if hold(w, turn) {
				t.Fatalf("%s: a thread turn operation is held", what)
			}
			if err := s.held(repo, w); errors.Is(err, errPaused) != want[w] {
				t.Fatalf("%s: held %s: %v", what, w, err)
			}
		}
	}
	check("no pause", nil)
	for _, p := range []runtime.Pause{
		ownerPause("workstream", project, stream, "soft", "Soft workstream"),
		ownerPause("workstream", project, stream, "hard", "Hard workstream"),
	} {
		must(t, s.setPause(p))
		check(p.Reason, map[config.WorkstreamID]bool{stream: true})
		must(t, store.ClearPause(p.Target, runtime.PauseOwner))
	}
	must(t, s.setPause(ownerPause("workstream", project, sibling, "soft", "Sibling")))
	check("sibling", map[config.WorkstreamID]bool{sibling: true})
	must(t, store.ClearPause(runtime.Target{Scope: "workstream", Project: project, Workstream: sibling}, runtime.PauseOwner))
	for _, p := range []runtime.Pause{ownerPause("project", project, "", "soft", "Project"), ownerPause("factory", "", "", "hard", "Factory")} {
		must(t, s.setPause(p))
		check(p.Reason, map[config.WorkstreamID]bool{stream: true, sibling: true})
		must(t, store.ClearPause(p.Target, runtime.PauseOwner))
	}
	check("resumed", nil)

	must(t, s.setPause(ownerPause("factory", "", "", "soft", "Factory")))
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: abandonTransition, Revision: 1, Project: project, Workstream: stream, At: clock.Now(), Actor: ownerActor, Cause: abandonTransition}
	_, err = repo.SetFeatureStateUnless(ctx, h, AbandonedState, "Superseded", AbandonedState, DeliveredState)
	must(t, err)
	check("abandoned", map[config.WorkstreamID]bool{sibling: true})
}

// operationRecord returns the workstream's one operation of the action.
func operationRecord(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, action string) trace.OperationRecord {
	t.Helper()
	ops, err := repository.Operations(stream)
	must(t, err)
	ops = slices.DeleteFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.Action != action })
	if len(ops) != 1 {
		t.Fatalf("%s operations: %+v", action, ops)
	}
	return ops[0]
}

// retries counts the retries recorded for the operation.
func retries(op trace.OperationRecord) int {
	n := 0
	for _, a := range op.History {
		if a.Kind == "retry" {
			n++
		}
	}
	return n
}

// checkHeld checks that the operation of a paused workstream is pending with
// one retry naming the pause, and stays so over later passes.
func checkHeld(t *testing.T, fetch func() trace.OperationRecord) {
	t.Helper()
	op := fetch()
	if op.Acknowledged || op.Result != nil || retries(op) != 1 || !slices.ContainsFunc(op.History, func(a trace.OperationAction) bool {
		return a.Kind == "retry" && strings.Contains(a.Failure, errPaused.Error())
	}) {
		t.Fatalf("the stopped operation is not held pending: %+v", op)
	}
	settle()
	if again := fetch(); again.Acknowledged || len(again.History) != len(op.History) {
		t.Fatalf("the held operation was reconciled under the pause: %+v", again.History)
	}
}

// blockTurn scripts the turn to run work, signal entered and run until its
// session is stopped.
func blockTurn(f *architectFixture, turn string, work func(context.Context, *mcp.ClientSession) error) chan struct{} {
	entered := make(chan struct{})
	f.engine.mu.Lock()
	defer f.engine.mu.Unlock()
	f.engine.turns[turn] = func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if err := work(ctx, tools); err != nil {
			return nil, err
		}
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return entered
}

// continueTurn scripts the continuation of the stopped turn: it checks the
// prompt names the stop, runs work, and ends normally.
func continueTurn(f *architectFixture, stopped trace.QueuedTurn, work func(context.Context, *mcp.ClientSession) error) string {
	turn := fmt.Sprintf("%s-continue-%d", stopped.Request.TurnID, stopped.Sequence)
	f.engine.mu.Lock()
	defer f.engine.mu.Unlock()
	f.engine.turns[turn] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if !strings.Contains(req.Prompt, stopped.Request.Prompt) || !strings.Contains(req.Prompt, pauseNote) {
			return nil, fmt.Errorf("continuation prompt %q", req.Prompt)
		}
		if work != nil {
			if err := work(ctx, tools); err != nil {
				return nil, err
			}
		}
		return &agent.Result{ClaudeID: "session-" + turn, ResultText: "Continued", SessionDir: req.SessionDir, NumTurns: 1}, nil
	}
	return turn
}

// awaitTurn waits until the i-th turn of the agent's thread completed.
func awaitTurn(t *testing.T, repository func() *trace.Repository, stream config.WorkstreamID, agent string, i int) trace.QueuedTurn {
	t.Helper()
	var q trace.QueuedTurn
	eventually(t, fmt.Sprintf("turn %d of %s did not complete", i, agent), func() bool {
		th, err := repository().Thread(stream, agent)
		if err != nil || len(th.Turns) <= i || th.Turns[i].CompletedAt.IsZero() {
			return false
		}
		q = th.Turns[i]
		return true
	})
	return q
}

// checkContinued checks that the thread holds the stopped turn and its
// continuation, completed normally, and no further attempt.
func checkContinued(t *testing.T, th trace.Thread, prefix, stopped, continuation string) {
	t.Helper()
	var ids []string
	for _, q := range th.Turns {
		if strings.HasPrefix(q.Request.TurnID, prefix) {
			ids = append(ids, q.Request.TurnID)
		}
	}
	if !slices.Equal(ids, []string{stopped, continuation}) {
		t.Fatalf("turns %v, want the stopped turn and its continuation", ids)
	}
	last := th.Turns[slices.IndexFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == continuation })]
	if last.Status() != "idle" || last.Request.Cause != th.Turns[slices.IndexFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == stopped })].Response.ID {
		t.Fatalf("continuation %+v", last)
	}
}

// A hard pause on a workstream stops the architect's running draft without
// failing it or spending an attempt, holds the draft while it is in force and
// leaves another workstream's draft running. After resume, the draft's
// continuation adds to what the stopped turn delivered.
func TestHardPauseStopsAnArchitectDraftAndResumeContinuesIt(t *testing.T) {
	t.Parallel()
	f := newArchitectFixture(t)
	defer f.stop(t)
	entered := make(chan struct{})
	var runs atomic.Int32
	f.engine.mu.Lock()
	f.engine.turns["draft-1-1"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if runs.Add(1) > 1 {
			for path, content := range map[string]string{plan.SpecPath: validSpec, plan.PlanPath: validPlan} {
				if _, err := callTool(ctx, tools, DraftTool, map[string]any{"path": path, "content": content}); err != nil {
					return nil, err
				}
			}
			return &agent.Result{ClaudeID: "session-other", ResultText: "Draft delivered", SessionDir: req.SessionDir, NumTurns: 1}, nil
		}
		if _, err := callTool(ctx, tools, DraftTool, map[string]any{"path": plan.SpecPath, "content": validSpec}); err != nil {
			return nil, err
		}
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	f.engine.mu.Unlock()
	paused := f.handIn(t, "paused", handedDesign)
	select {
	case <-entered:
	case <-time.After(demoTimeout):
		t.Fatal("the draft did not start")
	}
	target := runtime.Target{Scope: "workstream", Project: f.project, Workstream: paused}
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: target, Mode: "hard", Reason: "Stop the architect", Source: "owner"})
	stopped := awaitTurn(t, f.repository, paused, architectAgent, 0)
	checkPauseStop(t, stopped, "workstream", "Stop the architect")

	// Another workstream drafts through the pause.
	other := f.handIn(t, "other", handedDesign)
	f.await(t, other, sketched)
	draft := func() trace.OperationRecord { return operationRecord(t, f.repository(), paused, DraftAction) }
	eventually(t, "the stopped draft recorded no retry", func() bool { return retries(draft()) > 0 })
	checkHeld(t, draft)
	if state, err := f.repository().Workflow(paused, draftSubject); err != nil || state.Value != "drafting-1" {
		t.Fatalf("the stopped draft is %+v: %v", state, err)
	}
	if th := f.architectThread(t, paused); len(th.Turns) != 1 {
		t.Fatalf("turns queued under the hard pause: %+v", th.Turns)
	}

	continuation := continueTurn(f, stopped, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := callTool(ctx, tools, DraftTool, map[string]any{"path": plan.PlanPath, "content": validPlan})
		return err
	})
	mutation(t, f.c, "DELETE", "pause", target)
	f.await(t, paused, sketched)
	checkContinued(t, f.architectThread(t, paused), "draft-1-", "draft-1-1", continuation)
	if spec := f.documents(t, paused, plan.SpecDocument); len(spec) != 1 || spec[0].Content != validSpec {
		t.Fatalf("the spec the stopped turn delivered: %+v", spec)
	}
	if op := draft(); !op.Acknowledged || op.Result == nil || op.Result.Outcome != "succeeded" || retries(op) != 1 {
		t.Fatalf("draft operation %+v", op)
	}
}

// A soft pause lets the architect's running draft finish and be recorded,
// and asks for no draft of a workstream handed in while it is in force until
// it is cleared.
func TestSoftPauseLetsARunningDraftFinishAndRequestsNoNewOne(t *testing.T) {
	t.Parallel()
	f := newArchitectFixture(t)
	defer f.stop(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.engine.mu.Lock()
	f.engine.turns["draft-1-1"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		once.Do(func() {
			close(entered)
			<-release
		})
		for path, content := range map[string]string{plan.SpecPath: validSpec, plan.PlanPath: validPlan} {
			if _, err := callTool(ctx, tools, DraftTool, map[string]any{"path": path, "content": content}); err != nil {
				return nil, err
			}
		}
		return &agent.Result{ClaudeID: "session-draft", ResultText: "Draft delivered", SessionDir: req.SessionDir, NumTurns: 1}, nil
	}
	f.engine.mu.Unlock()
	running := f.handIn(t, "running", handedDesign)
	select {
	case <-entered:
	case <-time.After(demoTimeout):
		t.Fatal("the draft did not start")
	}
	target := runtime.Target{Scope: "factory"}
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: target, Mode: "soft", Reason: "Hold new work", Source: "owner"})
	close(release)
	f.await(t, running, sketched)

	waiting := f.handIn(t, "waiting", handedDesign)
	settle()
	if ops := f.draftOperations(t, waiting); len(ops) != 0 {
		t.Fatalf("a draft was asked for under a soft pause: %+v", ops)
	}
	if state, err := f.repository().Workflow(waiting, draftSubject); err != nil || state.Value != "" {
		t.Fatalf("draft state %+v: %v", state, err)
	}
	mutation(t, f.c, "DELETE", "pause", target)
	f.await(t, waiting, sketched)
}

// A hard pause on the project stops a committee member's running turn in
// its round and holds the round; after resume, the member's continuation
// completes its one attempt and the round records what the stopped turn
// contributed.
func TestHardPauseStopsACommitteeMemberAndResumeContinuesIt(t *testing.T) {
	t.Parallel()
	f := newShedFixture(t, 2)
	defer f.stop(t)
	member := committeeAgent(1)
	entered := blockTurn(f.architectFixture, roundTurnID(1, member, 1), func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := callTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": "size", "part": "plan#resume", "argument": "Too wide.", "citations": []string{"kb/entities.json#internal.trace"}})
		return err
	})
	f.member(1, 2, 1, func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) error { return nil })
	stream := f.handIn(t, "committee", handedDesign)
	select {
	case <-entered:
	case <-time.After(demoTimeout):
		t.Fatal("the member's turn did not start")
	}
	awaitTurn(t, f.repository, stream, committeeAgent(2), 0)
	target := runtime.Target{Scope: "project", Project: f.project}
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: target, Mode: "hard", Reason: "Stop the committee", Source: "owner"})
	stopped := awaitTurn(t, f.repository, stream, member, 0)
	checkPauseStop(t, stopped, "project", "Stop the committee")
	round := func() trace.OperationRecord { return operationRecord(t, f.repository(), stream, RoundAction) }
	eventually(t, "the stopped round recorded no retry", func() bool { return retries(round()) > 0 })
	checkHeld(t, round)
	if state, err := f.repository().Workflow(stream, shedSubject); err != nil || state.Value != "round-1" {
		t.Fatalf("the stopped round is %+v: %v", state, err)
	}

	continuation := continueTurn(f.architectFixture, stopped, nil)
	mutation(t, f.c, "DELETE", "pause", target)
	f.awaitShed(t, stream, "heard-1")
	th, err := f.repository().Thread(stream, member)
	must(t, err)
	checkContinued(t, th, roundTurnPrefix(1, member), stopped.Request.TurnID, continuation)
	records, err := shed.Records(f.repository(), stream)
	must(t, err)
	i := slices.IndexFunc(records, func(r shed.Record) bool { return r.Round == 1 && r.Member == member })
	if i < 0 || records[i].Failure != "" || records[i].Turn != stopped.Request.TurnID || len(records[i].Objections) != 1 {
		t.Fatalf("records %+v", records)
	}
}

// openPauseStore gives the stopped fixture's service a runtime store that
// takes pauses, for reconcilers applied directly.
func openPauseStore(t *testing.T, f *architectFixture, streams ...config.WorkstreamID) *runtime.Store {
	t.Helper()
	store, _, err := runtime.Open(runtime.Inputs{Config: f.s.cfg, Workstreams: streams})
	must(t, err)
	t.Cleanup(func() { store.Close() })
	f.s.store = store
	return store
}

// stopWhileRunning applies op until the turn it runs has entered, sets the
// hard pause and returns what the stopped application returned.
func stopWhileRunning(t *testing.T, s *Service, r coreadapter.Reconciler, op coreadapter.Operation, entered chan struct{}, pause runtime.Pause) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := r.Apply(context.Background(), op)
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(demoTimeout):
		t.Fatal("the turn did not start")
	}
	must(t, s.setPause(pause))
	select {
	case err := <-done:
		return err
	case <-time.After(demoTimeout):
		t.Fatal("the hard pause did not stop the turn")
	}
	return nil
}

// A soft pause asks for no final review; a hard pause stops the running
// final review, which is not retried or failed while the pause holds. After
// resume, its continuation completes the review with the report the stopped
// turn recorded.
func TestPauseHoldsAndStopsAFinalReview(t *testing.T) {
	t.Parallel()
	f, stream, repository, a := newFinalFixture(t, "paused-final")
	ctx := context.Background()
	store := openPauseStore(t, f.architectFixture, stream)
	_, op := assembleBoth(t, f, repository, a, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	turn := finalTurnID(1, committeeAgent(1), 1)
	entered := blockTurn(f.architectFixture, turn, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Both hold.", "criteria": []any{
			map[string]any{"criterion": "spec#1", "evidence": "resume.go"}, map[string]any{"criterion": "spec#2", "evidence": "dedupe.go"}}})
		return err
	})
	hard := ownerPause("workstream", f.project, stream, "hard", "Stop the review")
	if err := stopWhileRunning(t, f.s, a, op, entered, hard); !errors.Is(err, errPaused) {
		t.Fatalf("the stopped review returned %v", err)
	}
	th, err := repository.Thread(stream, committeeAgent(1))
	must(t, err)
	stopped := th.Turns[len(th.Turns)-1]
	checkPauseStop(t, stopped, "workstream", "Stop the review")
	if !f.s.holding(repository)(stream, op) {
		t.Fatal("the stopped review is not held")
	}
	if _, err := a.Apply(ctx, op); !errors.Is(err, errPaused) {
		t.Fatalf("the review applied under the hard pause: %v", err)
	}
	if again, err := repository.Thread(stream, committeeAgent(1)); err != nil || len(again.Turns) != len(th.Turns) {
		t.Fatalf("turns queued under the hard pause: %+v %v", again.Turns, err)
	}
	if result, err := a.outcome(stream, 1); err != nil || result != nil {
		t.Fatalf("the stopped review has an outcome %+v: %v", result, err)
	}
	must(t, store.ClearPause(hard.Target, runtime.PauseOwner))

	continuation := continueTurn(f.architectFixture, stopped, nil)
	result := settleOperation(t, f.s, repository, stream, op, a)
	report, _, err := latestFinalReport(repository, stream)
	must(t, err)
	if result.Outcome != "succeeded" || report.Outcome != finalReviewed || report.Turn != continuation || report.Summary != "Both hold." {
		t.Fatalf("result %+v, report %+v", result, report)
	}
	th, err = repository.Thread(stream, committeeAgent(1))
	must(t, err)
	checkContinued(t, th, finalTurnPrefix(1, committeeAgent(1)), turn, continuation)

	// A soft pause asks for no new review once the reviewed branch moves.
	soft := ownerPause("factory", "", "", "soft", "Hold new work")
	must(t, f.s.setPause(soft))
	moveFeature(t, f, stream, map[string]string{"LATER.md": "later\n"})
	must(t, a.Pass(ctx))
	must(t, a.Pass(ctx))
	if ops := finalOperations(t, repository, stream); len(ops) != 1 {
		t.Fatalf("a final review was asked for under a soft pause: %+v", ops)
	}
	must(t, store.ClearPause(soft.Target, runtime.PauseOwner))
	must(t, a.Pass(ctx))
	if ops := finalOperations(t, repository, stream); len(ops) != 2 {
		t.Fatalf("no final review was asked for after resume: %+v", ops)
	}
}

// A hard pause stops the architect's amendment draft and its reply to the
// amendment's round without spending attempts; a soft pause asks for no
// reply. After resume, the continuations complete them with what the
// stopped turns delivered.
func TestPauseHoldsAndStopsAmendmentTurns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newDebateFixture(t, 1, 3)
	object := func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		_, err := callTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": "fit", "part": "plan", "argument": "Checkpoint elsewhere.", "citations": []string{"spec#1"}})
		return err
	}
	f.script("amend-1-round-1-"+committeeAgent(1)+"-1", nil, object)
	stream := f.handIn(t, "amend-paused", handedDesign)
	f.await(t, stream, sketched)
	f.stop(t)
	repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	defer repo.Close()
	f.s.active = &activeProject{repository: repo}
	store := openPauseStore(t, f.architectFixture, stream)
	hard := ownerPause("workstream", f.project, stream, "hard", "Stop the amendment")

	seedAmendment(t, f.architectFixture, repo, stream)
	drafts := &amendmentDrafter{&drafter{s: f.s, repository: repo}}
	must(t, drafts.Pass(ctx))
	draftOp := amendmentOp(t, repo, stream, AmendmentDraftAction)
	amended := strings.Replace(validSpec, "last acknowledged chunk", "durable checkpoint", 1)
	entered := blockTurn(f.architectFixture, "amend-1-1", func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := callTool(ctx, tools, DraftTool, map[string]any{"path": plan.SpecPath, "content": amended})
		return err
	})
	if err := stopWhileRunning(t, f.s, drafts, draftOp, entered, hard); !errors.Is(err, errPaused) {
		t.Fatalf("the stopped amendment draft returned %v", err)
	}
	th, err := repo.Thread(stream, architectAgent)
	must(t, err)
	stopped := th.Turns[len(th.Turns)-1]
	checkPauseStop(t, stopped, "workstream", "Stop the amendment")
	if _, err := drafts.Apply(ctx, draftOp); !errors.Is(err, errPaused) {
		t.Fatalf("the amendment draft applied under the hard pause: %v", err)
	}
	must(t, store.ClearPause(hard.Target, runtime.PauseOwner))
	continuation := continueTurn(f.architectFixture, stopped, nil)
	result, err := drafts.Apply(ctx, draftOp)
	must(t, err)
	if state, err := repo.Workflow(stream, amendmentSubject("1")); err != nil || result.Outcome != "succeeded" || state.Value != "proposed" {
		t.Fatalf("amendment draft %+v, state %+v: %v", result, state, err)
	}
	th, err = repo.Thread(stream, architectAgent)
	must(t, err)
	checkContinued(t, th, "amend-1-", "amend-1-1", continuation)
	if docs := readDocs(t, repo, stream, "amendment_1_spec"); len(docs) != 1 || docs[0].Content != amended {
		t.Fatalf("the amended spec the stopped turn delivered: %+v", docs)
	}

	debate := amendmentDebate{&debate{s: f.s, repository: repo}}
	must(t, debate.Pass(ctx))
	_, err = debate.Apply(ctx, amendmentOp(t, repo, stream, AmendmentRoundAction))
	must(t, err)
	soft := ownerPause("project", f.project, "", "soft", "Hold new work")
	must(t, f.s.setPause(soft))
	must(t, debate.Pass(ctx))
	if state, err := repo.Workflow(stream, amendmentSubject("1")); err != nil || state.Value != "heard" {
		t.Fatalf("the reply was asked for under a soft pause: %+v %v", state, err)
	}
	must(t, store.ClearPause(soft.Target, runtime.PauseOwner))
	must(t, debate.Pass(ctx))
	replyOp := amendmentOp(t, repo, stream, AmendmentReplyAction)
	objection := shed.ObjectionID(1, committeeAgent(1), 1)
	entered = blockTurn(f.architectFixture, "amend-1-reply-1", func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := callTool(ctx, tools, shed.ReplyTool, map[string]any{"objection": objection, "answer": "The checkpoint stays in resume."})
		return err
	})
	if err := stopWhileRunning(t, f.s, debate, replyOp, entered, hard); !errors.Is(err, errPaused) {
		t.Fatalf("the stopped amendment reply returned %v", err)
	}
	th, err = repo.Thread(stream, architectAgent)
	must(t, err)
	stopped = th.Turns[len(th.Turns)-1]
	checkPauseStop(t, stopped, "workstream", "Stop the amendment")
	if _, err := debate.Apply(ctx, replyOp); !errors.Is(err, errPaused) {
		t.Fatalf("the amendment reply applied under the hard pause: %v", err)
	}
	must(t, store.ClearPause(hard.Target, runtime.PauseOwner))
	continuation = continueTurn(f.architectFixture, stopped, nil)
	_, err = debate.Apply(ctx, replyOp)
	must(t, err)
	th, err = repo.Thread(stream, architectAgent)
	must(t, err)
	checkContinued(t, th, "amend-1-reply-", "amend-1-reply-1", continuation)
	docs := readDocs(t, repo, stream, "amendment_1_round_1_reply")
	if len(docs) != 1 {
		t.Fatalf("reply documents %+v", docs)
	}
	reply, err := shed.ParseReply([]byte(docs[0].Content))
	must(t, err)
	if reply.Failure != "" || reply.Turn != continuation || len(reply.Answers) != 1 || reply.Answers[0].Objection != objection {
		t.Fatalf("reply %+v", reply)
	}
}
