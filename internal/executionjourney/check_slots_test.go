package executionjourney

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// slotChecks records admission at the runner boundary, including each run's
// remaining timeout, and holds it until the test releases it or cancels it.
type slotChecks struct {
	mu         sync.Mutex
	used, peak int
	entered    chan time.Duration
	release    chan checkOutcome
}

func newSlotChecks() *slotChecks {
	return &slotChecks{entered: make(chan time.Duration, 16), release: make(chan checkOutcome, 16)}
}
func (c *slotChecks) List(context.Context, string) ([]string, error) { return nil, nil }
func (c *slotChecks) Check(ctx context.Context, _ string, _ []string) (CheckResult, error) {
	c.mu.Lock()
	c.used++
	c.peak = max(c.peak, c.used)
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.used--; c.mu.Unlock() }()
	deadline, _ := ctx.Deadline()
	c.entered <- time.Until(deadline)
	select {
	case o := <-c.release:
		return o.Result, o.Err
	case <-ctx.Done():
		return CheckResult{ExitCode: -1}, ctx.Err()
	}
}

func requestUnitCheck(t *testing.T, s *Service, repo *trace.Repository, stream config.WorkstreamID) (*checkers, coreadapter.Operation) {
	t.Helper()
	c := &checkers{masons: newMasonController(s, repo)}
	must(t, c.Pass(context.Background()))
	ops, err := repo.Operations(stream)
	must(t, err)
	for _, op := range ops {
		if op.Operation.Action == CheckAction {
			return c, op.Operation
		}
	}
	t.Fatal("no unit check requested")
	return nil, coreadapter.Operation{}
}

func awaitCheckSlots(t *testing.T, s *Service, used, waiting int) CheckSlotStatus {
	t.Helper()
	soon(t, fmt.Sprintf("%d used check slots and %d waiting", used, waiting), func() bool {
		st := s.CheckSlots()
		return st.Used == used && len(st.Waiting) == waiting
	})
	return s.CheckSlots()
}

func queuedSlot(ctx context.Context, s *Service, run CheckSlotRun) <-chan func() {
	return fixturekit.QueueSlot(ctx, func(ctx context.Context) (func(), error) { return InspectForTest(s).AcquireCheckSlot(ctx, run) })
}

func TestCheckSlotsReloadAndFIFO(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	s, client := start(t, opts)
	files := configFiles(t, opts)
	setLimit := func(limit int) {
		files.write(t, files.topText+fmt.Sprintf("\n[capacity]\nchecks = %d\n", limit), files.projectText)
		_, err := client.Reload(context.Background())
		must(t, err)
		effective, err := client.Configuration(context.Background())
		must(t, err)
		if effective.Effective.Capacity.Checks != limit || s.CheckSlots().Limit != limit {
			t.Fatalf("effective checks %d, pool %+v", effective.Effective.Capacity.Checks, s.CheckSlots())
		}
	}
	setLimit(1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, err := InspectForTest(s).AcquireCheckSlot(ctx, CheckSlotRun{Workstream: stream, Unit: "first"})
	must(t, err)
	defer first()
	second := queuedSlot(ctx, s, CheckSlotRun{Workstream: sibling, Review: 1})
	awaitCheckSlots(t, s, 1, 1)
	third := queuedSlot(ctx, s, CheckSlotRun{Workstream: otherStream, Session: true})
	st := awaitCheckSlots(t, s, 1, 2)
	if st.Waiting[0].Review != 1 || !st.Waiting[1].Session {
		t.Fatalf("queue order %+v", st)
	}
	// Increasing the limit wakes the oldest waiter without a release or restart.
	setLimit(2)
	releaseSecond := awaitCheck(t, second)
	defer releaseSecond()
	awaitCheckSlots(t, s, 2, 1)
	// Lowering lets executing runs finish, admitting nobody above the new limit.
	setLimit(1)
	first()
	awaitCheckSlots(t, s, 1, 1)
	select {
	case <-third:
		t.Fatal("run admitted past lowered limit")
	default:
	}
	releaseSecond()
	releaseThird := awaitCheck(t, third)
	releaseThird()
	awaitCheckSlots(t, s, 0, 0)
}

func TestUnitCheckSlotWaitPreservesTimeoutAndIncompleteCount(t *testing.T) {
	t.Parallel()
	checks := newSlotChecks()
	f, stream, repo := newChecksFixture(t, "slot-timeout", checks)
	InspectForTest(f.s).ResizeCheckSlots(1)
	f.s.cfg.Project.ChecksTimeout = "1s"
	holder, err := InspectForTest(f.s).AcquireCheckSlot(context.Background(), CheckSlotRun{Workstream: sibling, Session: true})
	must(t, err)
	defer holder()
	c, op := requestUnitCheck(t, f.s, repo, stream)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := applyCheck(ctx, c, op)
	awaitCheckSlots(t, f.s, 1, 1)
	// This exceeds the run's entire checks_timeout, while it has no start
	// evidence, result, or incomplete transition.
	time.Sleep(1200 * time.Millisecond)
	runs, err := checkRuns(repo, stream)
	must(t, err)
	if len(runs) != 0 || len(streamDocuments(t, repo, stream, checkSelectionDocument("resume", 1))) != 0 {
		t.Fatalf("waiting run started or finished: %+v", runs)
	}
	state, err := repo.Workflow(stream, checksSubject("resume"))
	must(t, err)
	if state.Value != "requested-1" {
		t.Fatalf("waiting changed checks state: %+v", state)
	}
	holder()
	if remaining := awaitCheck(t, checks.entered); remaining < 500*time.Millisecond {
		t.Fatalf("timeout lost to wait: %s", remaining)
	}
	checks.release <- checkOutcome{Result: CheckResult{Output: passedReport}}
	must(t, awaitCheck(t, finished))
	run := onlyRun(t, repo, stream)
	if run.Status != ChecksPassed {
		t.Fatalf("wait produced %+v", run)
	}
	n, err := c.incompleteSinceEntry(stream, "resume", run, []UnitCheckRun{run})
	must(t, err)
	if n != 0 {
		t.Fatalf("wait counted as %d incomplete runs", n)
	}
	awaitCheckSlots(t, f.s, 0, 0)
}

func TestUnitCheckSlotsReleaseEveryOutcome(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{ChecksPassed, ChecksFailed, ChecksIncomplete, "timeout", "cancelled", ChecksSuperseded} {
		t.Run(outcome, func(t *testing.T) {
			t.Parallel()
			checks := newSlotChecks()
			f, stream, repo := newChecksFixture(t, "slot-"+outcome, checks)
			InspectForTest(f.s).ResizeCheckSlots(1)
			c, op := requestUnitCheck(t, f.s, repo, stream)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var holder func()
			if outcome == ChecksSuperseded {
				var err error
				holder, err = InspectForTest(f.s).AcquireCheckSlot(ctx, CheckSlotRun{Session: true})
				must(t, err)
				defer holder()
			}
			if outcome == "timeout" {
				f.s.cfg.Project.ChecksTimeout = "20ms"
			}
			finished := applyCheck(ctx, c, op)
			if outcome == ChecksSuperseded {
				awaitCheckSlots(t, f.s, 1, 1)
				state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
				must(t, err)
				h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "supersede-waiter", Revision: 1, Project: repo.Project(), Workstream: stream, Unit: "resume", At: f.s.now(), Actor: foremanActor, Cause: reportDocument("resume")}
				_, err = repo.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: trace.UnitSubject("resume"), From: UnitChecking, To: UnitImplementing, Reason: "candidate replaced while waiting"}})
				must(t, err)
				holder()
			} else {
				awaitCheck(t, checks.entered)
				switch outcome {
				case ChecksPassed:
					checks.release <- checkOutcome{Result: CheckResult{Output: passedReport}}
				case ChecksFailed:
					checks.release <- checkOutcome{Result: CheckResult{ExitCode: 1, Output: failedReport("dag://test")}}
				case ChecksIncomplete:
					checks.release <- checkOutcome{Result: CheckResult{ExitCode: -1}, Err: errors.New("engine unavailable")}
				case "cancelled":
					cancel()
				}
			}
			err := awaitCheck(t, finished)
			if outcome == "cancelled" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled run: %v", err)
				}
				runs, readErr := checkRuns(repo, stream)
				must(t, readErr)
				if len(runs) != 0 {
					t.Fatalf("cancelled run recorded: %+v", runs)
				}
			} else {
				must(t, err)
				want := outcome
				if want == "timeout" {
					want = ChecksIncomplete
				}
				if run := onlyRun(t, repo, stream); run.Status != want {
					t.Fatalf("run %+v, want %s", run, want)
				}
			}
			// The next run can take the only slot regardless of the preceding exit.
			nextCtx, nextCancel := context.WithTimeout(context.Background(), time.Second)
			defer nextCancel()
			release, err := InspectForTest(f.s).AcquireCheckSlot(nextCtx, CheckSlotRun{Workstream: sibling, Review: 1})
			must(t, err)
			release()
			awaitCheckSlots(t, f.s, 0, 0)
		})
	}
}

func TestCheckSlotsSharedByUnitFinalAndSessionsAcrossProjects(t *testing.T) {
	t.Parallel()
	checks := newSlotChecks()
	f, unitStream, repo := newChecksFixture(t, "factory-slots", checks)
	_, unitOp := requestUnitCheck(t, f.s, repo, unitStream)
	base := currentReport(t, repo, unitStream, "resume").Base
	finalStream := recordBuild(t, f, repo, "final-slots", validPlan, config.WorkspacesGit, base)
	commit := moveFeature(t, f, finalStream, map[string]string{"final.txt": "final candidate\n"})
	primary := repo.Project()
	must(t, repo.Close())
	// Drive the real check entry points without unrelated model turns.
	f.opts.Reconciliation.Hold = func(config.WorkstreamID, coreadapter.Operation) bool { return true }
	f.opts.reviewChecks = checks
	f.start(t)
	defer f.stop(t)
	repo, err := f.s.repository(primary)
	must(t, err)
	// Register another project in the same service, backed entirely by local
	// repositories, and open two independently controlled owner sessions.
	home := filepath.Dir(f.clone)
	clone := filepath.Join(home, "other-clone")
	bare := filepath.Join(home, "remotes", "acme", "other.git")
	must(t, os.MkdirAll(filepath.Dir(bare), 0700))
	demoGit(t, home, "clone", "--quiet", "--bare", "--no-hardlinks", f.clone, bare)
	demoGit(t, home, "clone", "--quiet", "--no-hardlinks", bare, clone)
	added, err := f.c.AddProject(context.Background(), ProjectAddRequest{Name: "other", Upstream: "acme/other", Clone: clone})
	must(t, err)
	writeCharter(t, f.c, added.Project.ID, "1. Keep changes small.\n")
	sessions := make([]config.WorkstreamID, 2)
	for i := range sessions {
		opened, err := f.c.OpenSession(context.Background(), SessionOpenRequest{Project: added.Project.ID, Profile: "default"})
		must(t, err)
		sessions[i] = opened.Session.Workstream
	}
	if primary == added.Project.ID {
		t.Fatal("fixture needs two projects")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &checkers{masons: &masons{s: f.s, cfg: f.s.about(repo), repository: repo}}
	unitDone := applyCheck(ctx, c, unitOp)
	awaitCheck(t, checks.entered)
	finalDone := make(chan error, 1)
	go func() {
		a := &finalReviewer{s: f.s, repository: repo}
		run, err := a.finalChecks(ctx, f.s.about(repo), finalStream, FinalReport{Review: 1, Operation: "final-slot-test", Commit: commit, Upstream: &seal.Base{Commit: base}})
		if err == nil && run.Status != ChecksPassed {
			err = fmt.Errorf("final run %+v", run)
		}
		finalDone <- err
	}()
	awaitCheck(t, checks.entered)
	sessionDone := make(chan error, 2)
	for i, session := range sessions {
		go func() {
			run, err := f.c.SessionCheck(ctx, session)
			if err == nil && run.Status != ChecksPassed {
				err = fmt.Errorf("session run %+v", run)
			}
			sessionDone <- err
		}()
		st := awaitCheckSlots(t, f.s, 2, i+1)
		if w := st.Waiting[i]; w.Project != added.Project.ID || w.Workstream != session || !w.Session {
			t.Fatalf("waiting session identity %+v", w)
		}
	}
	// Both executing runs keep the slots until their runner returns. Each
	// return admits one waiter; all four entry points eventually complete.
	for range 2 {
		checks.release <- checkOutcome{Result: CheckResult{Output: passedReport}}
		awaitCheck(t, checks.entered)
	}
	for range 2 {
		checks.release <- checkOutcome{Result: CheckResult{Output: passedReport}}
	}
	must(t, awaitCheck(t, unitDone))
	must(t, awaitCheck(t, finalDone))
	for range 2 {
		must(t, awaitCheck(t, sessionDone))
	}
	awaitCheckSlots(t, f.s, 0, 0)
	checks.mu.Lock()
	defer checks.mu.Unlock()
	if checks.peak != 2 || checks.used != 0 {
		t.Fatalf("runner peak %d, still executing %d", checks.peak, checks.used)
	}
}

func TestSessionCheckSlotWaitRespectsRequestCancellation(t *testing.T) {
	t.Parallel()
	s, client, _, _, stream, _, _ := newSessionControlsFixture(t)
	InspectForTest(s).ResizeCheckSlots(1)
	checks := newSlotChecks()
	s.setReviewChecks(checks)
	holder, err := InspectForTest(s).AcquireCheckSlot(context.Background(), CheckSlotRun{Workstream: sibling, Unit: "held"})
	must(t, err)
	defer holder()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { _, err := client.SessionCheck(ctx, stream); finished <- err }()
	awaitCheckSlots(t, s, 1, 1)
	cancel()
	if err := awaitCheck(t, finished); err == nil || !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("request cancellation: %v (context %v)", err, ctx.Err())
	}
	awaitCheckSlots(t, s, 1, 0)
	_, _, repo, api := InspectForTest(s).SessionTrace(string(stream))
	if api != nil {
		t.Fatal(api)
	}
	runs, err := sessionCheckRuns(repo, stream)
	must(t, err)
	if len(runs) != 0 {
		t.Fatalf("waiting request recorded check runs: %+v", runs)
	}
	holder()
	finished = make(chan error, 1)
	go func() { _, err := client.SessionCheck(context.Background(), stream); finished <- err }()
	awaitCheck(t, checks.entered)
	checks.release <- checkOutcome{Result: CheckResult{Output: passedReport}}
	must(t, awaitCheck(t, finished))
	awaitCheckSlots(t, s, 0, 0)
}

func TestHeldCheckSlotsAllowOtherMasonsAndReviewersAndRestartEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, fake := newCappedMasonFixture(t, "masons = 4\nchecks = 1\n", independentPlan)
	base := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	unitStream, repo := seedBuild(t, f, "running-check", independentPlan, config.WorkspacesGit, base)
	seedChecking(t, f, repo, unitStream, "resume", resumeReport)
	requestUnitCheck(t, f.s, repo, unitStream)
	finalStream := recordBuild(t, f, repo, "waiting-final", validPlan, config.WorkspacesGit, base)
	a := &finalReviewer{s: f.s, repository: repo}
	assembleBoth(t, f, repo, a, finalStream, map[string]string{"one.txt": "one\n"}, map[string]string{"two.txt": "two\n"})
	masonStream := recordBuild(t, f, repo, "independent-mason", independentPlan, config.WorkspacesGit, base)
	reviewerStream := recordBuild(t, f, repo, "independent-reviewer", independentPlan, config.WorkspacesGit, base)
	seedReview(t, f, repo, reviewerStream, "resume", resumeReport)
	review := reviewTurn(t, f, repo, reviewerStream)
	must(t, repo.Close())
	checks := newSlotChecks()
	f.opts.reviewChecks = checks
	var allowFinal, allowTurns atomic.Bool
	ticks := make(chan time.Time, 1)
	f.opts.Reconciliation.Ticks = ticks
	f.opts.Reconciliation.Hold = func(stream config.WorkstreamID, op coreadapter.Operation) bool {
		if op.Action == CheckAction && stream == unitStream {
			return false
		}
		if op.Action == FinalReviewAction && stream == finalStream {
			return !allowFinal.Load()
		}
		if in, err := thread.DecodeTurn(op); err == nil && stream == masonStream && in.Turn == masonTurnID("resume") {
			return !allowTurns.Load()
		}
		if in, err := thread.DecodeTurn(op); err == nil && stream == reviewerStream && in.Turn == review.TurnID {
			return !allowTurns.Load()
		}
		return true
	}
	masonEntered := make(chan struct{})
	fake.Play[masonTurnID("resume")] = func(context.Context, agent.Request, *mcp.ClientSession) error {
		close(masonEntered)
		return errFailTurn
	}
	reviewerEntered := make(chan struct{})
	f.engine.Mu.Lock()
	f.engine.Turns[review.TurnID] = func(ctx context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
		close(reviewerEntered)
		return &agent.Result{ResultText: "Reviewed", NumTurns: 1}, nil
	}
	f.engine.Mu.Unlock()
	f.start(t)
	running := true
	defer func() {
		if running {
			f.stop(t)
		}
	}()
	awaitCheck(t, checks.entered)
	allowFinal.Store(true)
	ticks <- f.clock.Now()
	st := awaitCheckSlots(t, f.s, 1, 1)
	if w := st.Waiting[0]; w.Workstream != finalStream || w.Review != 1 {
		t.Fatalf("waiting final identity %+v", w)
	}
	repo, err := f.s.repository(f.project)
	must(t, err)
	if starts := finalCheckStarts(t, repo, finalStream); len(starts) != 0 {
		t.Fatalf("waiting final recorded attempts: %+v", starts)
	}
	rebase, found, err := (&finalReviewer{s: f.s, repository: repo}).rebase(finalStream, 1)
	must(t, err)
	if !found {
		t.Fatal("waiting final review has no rebased candidate")
	}
	n, _, err := incompleteFinalChecks(repo, finalStream, 1, rebase.Commit)
	must(t, err)
	if n != 0 {
		t.Fatalf("waiting final counted %d incomplete runs", n)
	}
	// The final-review operation waiting in Apply must leave reconciliation
	// free to execute a newly eligible turn from another workstream.
	allowTurns.Store(true)
	ticks <- f.clock.Now()
	await(t, "another workstream's mason with every check slot occupied", masonEntered)
	await(t, "another workstream's reviewer with every check slot occupied", reviewerEntered)
	awaitCheckSlots(t, f.s, 1, 1)
	assertSingleReviewEffect(t, repo, finalStream, FinalReviewAction)
	// Shutdown cancels and joins the executing unit run and the waiting final
	// operation. No persisted slot or incomplete-run retry survives restart.
	f.stop(t)
	running = false
	f.opts.Reconciliation.Hold = func(config.WorkstreamID, coreadapter.Operation) bool { return true }
	f.start(t)
	running = true
	awaitCheckSlots(t, f.s, 0, 0)
	repo, err = f.s.repository(f.project)
	must(t, err)
	runs, err := checkRuns(repo, unitStream)
	must(t, err)
	if len(runs) != 0 {
		t.Fatalf("interrupted check recorded results: %+v", runs)
	}
	if starts := finalCheckStarts(t, repo, finalStream); len(starts) != 0 {
		t.Fatalf("interrupted waiting final started: %+v", starts)
	}
	// A fresh request can use the entire restored capacity immediately.
	release, err := InspectForTest(f.s).AcquireCheckSlot(ctx, CheckSlotRun{Workstream: sibling, Session: true})
	must(t, err)
	release()
}

func assertSingleReviewEffect(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, action string) {
	t.Helper()
	ops, err := repo.Operations(stream)
	must(t, err)
	count := 0
	for _, op := range ops {
		if op.Operation.Action != action {
			continue
		}
		count++
		effects := 0
		for _, h := range op.History {
			if h.Kind == "effect" {
				effects++
			}
		}
		if effects != 1 {
			t.Fatalf("review started %d times: %+v", effects, op)
		}
	}
	if count != 1 {
		t.Fatalf("requested %d reviews", count)
	}
}

func TestDeliveryReviewWaitRunsBesidePassAndStartsOnce(t *testing.T) {
	t.Parallel()
	p := maintenanceFixture(t)
	defer p.repository.Close()
	checks := newSlotChecks()
	p.s.setReviewChecks(checks)
	InspectForTest(p.s).ResizeCheckSlots(1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	holder, err := InspectForTest(p.s).AcquireCheckSlot(ctx, CheckSlotRun{Workstream: sibling, Unit: "held"})
	must(t, err)
	defer holder()
	a := &finalReviewer{s: p.s, repository: p.repository}
	must(t, a.Pass(ctx))
	op := actionOperation(t, p.repository, p.stream, deliveryReviewAction)
	in, err := decodeFinalReview(op)
	must(t, err)
	startsBefore := len(finalCheckStarts(t, p.repository, p.stream))
	var reads atomic.Int32
	p.finalTurn(in.Review, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		reads.Add(1)
		_, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Ready upstream", "findings": []any{}})
		return err
	})
	var passes atomic.Int32
	ticks := make(chan time.Time, 1)
	c, err := reconcile.New(p.repository, reconcile.Options{
		Worker: "delivery-slot-test", Now: p.s.now, Ticks: ticks,
		Class: operationClass, Limit: InspectForTest(p.s).ModelOperationLimit(p.repository, nil),
		Concurrent: concurrentOperation,
		Adapters:   map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RunnerBoundary: a},
		Hold:       func(_ config.WorkstreamID, candidate coreadapter.Operation) bool { return candidate.ID != op.ID },
		Schedule: func(ctx context.Context) error {
			if err := a.Pass(ctx); err != nil {
				return err
			}
			passes.Add(1)
			return nil
		},
	})
	must(t, err)
	finished := make(chan error, 1)
	go func() { finished <- c.Run(ctx) }()
	defer func() {
		cancel()
		if err := awaitCheck(t, finished); err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	}()
	awaitCheckSlots(t, p.s, 1, 1)
	passAgain := func() {
		n := passes.Load()
		ticks <- p.s.now()
		soon(t, "reconciliation pass beside delivery review", func() bool { return passes.Load() > n })
	}
	for range 3 {
		passAgain()
	}
	assertSingleReviewEffect(t, p.repository, p.stream, deliveryReviewAction)
	if starts := finalCheckStarts(t, p.repository, p.stream); len(starts) != startsBefore {
		t.Fatalf("waiting maintenance started a check attempt: %+v", starts)
	}
	holder()
	awaitCheck(t, checks.entered)
	for range 3 {
		passAgain()
	}
	assertSingleReviewEffect(t, p.repository, p.stream, deliveryReviewAction)
	checks.release <- checkOutcome{Result: CheckResult{Output: passedReport}}
	soon(t, "delivery review completes", func() bool {
		ops, err := p.repository.Operations(p.stream)
		must(t, err)
		for _, record := range ops {
			if record.Operation.ID == op.ID {
				return record.Acknowledged
			}
		}
		return false
	})
	for range 3 {
		passAgain()
	}
	assertSingleReviewEffect(t, p.repository, p.stream, deliveryReviewAction)
	if reads.Load() != 1 {
		t.Fatalf("delivery reader started %d times", reads.Load())
	}
	awaitCheckSlots(t, p.s, 0, 0)
}

// queuedCommittee provides a real scheduler candidate on another workstream.
// Its dispatched turn can hold committee capacity without a model provider.
func queuedCommittee(t *testing.T, f *shedFixture, repo *trace.Repository) (config.WorkstreamID, *scheduler.Scheduler) {
	t.Helper()
	ctx := context.Background()
	stream, err := config.NewWorkstreamID()
	must(t, err)
	must(t, repo.CreateWorkstream(ctx, stream, f.s.now(), ownerActor))
	header := func(schema, id string) trace.Header {
		return trace.Header{Schema: schema, Version: trace.Version, Revision: 1, ID: id, Project: repo.Project(), Workstream: stream, At: f.s.now(), Actor: ownerActor, Cause: "committee-capacity"}
	}
	member := committeeAgent(1)
	must(t, repo.CreateThread(ctx, trace.Agent{Header: header("osmia.trace.agent", member), Role: committeeRole, ThreadID: member}))
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: header("osmia.trace.turn-request", "committee-request"), AgentID: member, ThreadID: member, TurnID: roundTurnID(1, member, 1), Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Read the plan"})
	must(t, err)
	cfg := f.s.about(repo)
	limits := cfg.Capacity
	limits.PerWorkstream = cfg.Project.Capacity.PerWorkstream
	sched, err := scheduler.New(repo, scheduler.Options{Now: f.s.now, Capacity: &limits, Shared: InspectForTest(f.s).Pool(), Holds: func(c scheduler.Candidate) (bool, error) { return c.Workstream != stream, nil }})
	must(t, err)
	return stream, sched
}

func assertReviewSlotsFree(t *testing.T, s *Service, repo *trace.Repository, stream config.WorkstreamID, op coreadapter.Operation) {
	t.Helper()
	awaitCheckSlots(t, s, 0, 0)
	if used := InspectForTest(s).Pool().Slots(nil).Used[committeeRole]; used != 0 {
		t.Fatalf("committee slots still held: %d", used)
	}
	if found := InspectForTest(s).HasReviewReservation(op.ID); found {
		t.Fatal("finished operation retained its reservation handle")
	}
	// A following admission can reserve both slots, using the same pool and
	// project limits that a review uses, then release them idempotently.
	releaseCommittee, admitted := InspectForTest(s).ModelOperationLimit(repo, nil)(op)
	if !admitted {
		t.Fatal("following review cannot take committee capacity")
	}
	defer releaseCommittee()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	releaseChecks, err := InspectForTest(s).AcquireReviewCheckSlot(ctx, CheckSlotRun{Project: repo.Project(), Workstream: stream, Review: 2}, op.ID)
	must(t, err)
	releaseChecks()
	releaseCommittee()
	releaseCommittee()
	awaitCheckSlots(t, s, 0, 0)
	if got := InspectForTest(s).Pool().Slots(nil).Used[committeeRole]; got != 0 {
		t.Fatalf("following review leaked committee slot: %d", got)
	}
}

func TestReviewCheckSlotWaitYieldsCommittee(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"final", "delivery"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			var f *shedFixture
			var repo *trace.Repository
			var stream config.WorkstreamID
			var a *finalReviewer
			var op coreadapter.Operation
			if kind == "delivery" {
				p := maintenanceFixture(t)
				f, repo, stream = p.shedFixture, p.repository, p.stream
				a = &finalReviewer{s: f.s, repository: repo}
				must(t, a.Pass(context.Background()))
				op = actionOperation(t, repo, stream, deliveryReviewAction)
			} else {
				f, stream, repo, a = newFinalFixture(t, "committee-yield")
				_, op = assembleBoth(t, f, repo, a, stream, map[string]string{"one.go": "package feature\n"}, map[string]string{"two.go": "package feature\n"})
			}
			defer repo.Close()
			InspectForTest(f.s).ResizeCheckSlots(1)
			f.s.cfg.Project.ChecksTimeout = "1s"
			if f.s.cfg.Capacity.Committee != 1 {
				t.Fatal("fixture must have committee=1")
			}
			checks := newSlotChecks()
			f.s.setReviewChecks(checks)
			in, err := decodeFinalReview(op)
			must(t, err)
			startsBefore := len(finalCheckStarts(t, repo, stream))
			var reads atomic.Int32
			readerEntered, readerRelease := make(chan struct{}, 1), make(chan struct{})
			f.finalTurn(in.Review, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
				reads.Add(1)
				readerEntered <- struct{}{}
				select {
				case <-readerRelease:
				case <-ctx.Done():
					return ctx.Err()
				}
				_, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Ready to deliver", "findings": []any{}})
				return err
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			holder, err := InspectForTest(f.s).AcquireCheckSlot(ctx, CheckSlotRun{Workstream: sibling, Unit: "held"})
			must(t, err)
			defer holder()
			c, err := reconcile.New(repo, reconcile.Options{Worker: "review-committee-yield", Now: f.s.now, Concurrent: concurrentOperation, Class: operationClass, Limit: InspectForTest(f.s).ModelOperationLimit(repo, nil), Hold: func(_ config.WorkstreamID, candidate coreadapter.Operation) bool { return candidate.ID != op.ID }, Adapters: map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RunnerBoundary: a}})
			must(t, err)
			defer func() { cancel(); c.Wait() }()
			must(t, c.Pass(ctx))
			awaitCheckSlots(t, f.s, 1, 1)
			if got := InspectForTest(f.s).Pool().Slots(nil).Used[committeeRole]; got != 0 {
				t.Fatalf("waiting review holds %d committee slots", got)
			}
			// Even with spare committee capacity, the admitted operation's
			// identity remains exclusive while its capacity is yielded.
			if r, admitted := InspectForTest(f.s).ModelOperationLimit(repo, nil)(op); admitted {
				r()
				t.Fatal("duplicate review admitted while committee capacity was free")
			}
			other, sched := queuedCommittee(t, f, repo)
			must(t, sched.Pass(ctx))
			ops, err := repo.Operations(other)
			must(t, err)
			if len(ops) != 1 {
				t.Fatalf("committee turn was not dispatched: %+v", ops)
			}
			if got := InspectForTest(f.s).Pool().Slots(nil).Used[committeeRole]; got != 1 {
				t.Fatalf("dispatched committee uses %d slots", got)
			}
			// Stay queued beyond the whole checks_timeout; neither evidence nor
			// an incomplete result can be written before acquiring both slots.
			time.Sleep(1200 * time.Millisecond)
			assertWaiting := func() {
				t.Helper()
				for range 3 {
					must(t, c.Pass(ctx))
				}
				assertSingleReviewEffect(t, repo, stream, op.Action)
				if len(finalCheckStarts(t, repo, stream)) != startsBefore {
					t.Fatal("waiting review started checks")
				}
				if len(streamDocuments(t, repo, stream, finalChecksDocument(in.Review))) != 0 {
					t.Fatal("waiting review recorded a result")
				}
				if reads.Load() != 0 {
					t.Fatal("waiting review started its reader")
				}
				// Admission identity survives yielding, including at spare committee
				// capacity: a duplicate operation cannot replace the live handle.
				if r, admitted := InspectForTest(f.s).ModelOperationLimit(repo, nil)(op); admitted {
					r()
					t.Fatal("duplicate review admitted")
				}
			}
			assertWaiting()
			holder()
			// The committee turn is still in flight, so every attempted acquisition
			// must return its check slot rather than blocking other check work.
			awaitCheckSlots(t, f.s, 0, 0)
			time.Sleep(1200 * time.Millisecond)
			assertWaiting()
			probe, err := InspectForTest(f.s).AcquireCheckSlot(ctx, CheckSlotRun{Workstream: other, Unit: "independent-check"})
			must(t, err)
			probe()
			claimed, err := repo.ClaimTurn(ctx, other, committeeAgent(1), "committee-token", t.TempDir(), f.s.now())
			must(t, err)
			completeClaimedTurn(t, f, repo, other, committeeAgent(1), claimed, nil)
			must(t, sched.Pass(ctx))
			if remaining := awaitCheck(t, checks.entered); remaining < 500*time.Millisecond {
				t.Fatalf("wait consumed timeout: %s", remaining)
			}
			if got := InspectForTest(f.s).Pool().Slots(nil).Used[committeeRole]; got != 1 {
				t.Fatalf("checking review uses %d committee slots", got)
			}
			for range 3 {
				must(t, c.Pass(ctx))
			}
			assertSingleReviewEffect(t, repo, stream, op.Action)
			starts := finalCheckStarts(t, repo, stream)
			if len(starts) != startsBefore+1 || starts[len(starts)-1].Operation != op.ID {
				t.Fatalf("check identity changed or duplicated: %+v", starts)
			}
			checks.release <- checkOutcome{Result: CheckResult{Output: passedReport}}
			awaitCheck(t, readerEntered)
			if got := InspectForTest(f.s).Pool().Slots(nil).Used[committeeRole]; got != 1 {
				t.Fatalf("reader uses %d committee slots", got)
			}
			// A committee turn cannot overlap the review's checks or reader.
			_, admitted, err := InspectForTest(f.s).Pool().TryReserve(repo, other, committeeRole, committeeAgent(1), &f.s.cfg.Capacity)
			must(t, err)
			if admitted {
				t.Fatal("committee bound exceeded during reader")
			}
			close(readerRelease)
			must(t, c.Wait())
			if reads.Load() != 1 {
				t.Fatalf("reader ran %d times", reads.Load())
			}
			assertSingleReviewEffect(t, repo, stream, op.Action)
			assertReviewSlotsFree(t, f.s, repo, stream, op)
		})
	}
}

func TestFinalReviewSlotsReleaseEveryOutcome(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{ChecksPassed, ChecksFailed, ChecksIncomplete, "timeout", "cancelled", "superseded"} {
		t.Run(outcome, func(t *testing.T) {
			t.Parallel()
			f, stream, repo, a := newFinalFixture(t, "final-slots-"+outcome)
			_, op := assembleBoth(t, f, repo, a, stream, map[string]string{"one.go": "package feature\n"}, map[string]string{"two.go": "package feature\n"})
			checks := newSlotChecks()
			f.s.setReviewChecks(checks)
			InspectForTest(f.s).ResizeCheckSlots(1)
			if outcome == "timeout" {
				f.s.cfg.Project.ChecksTimeout = "20ms"
			}
			var reads atomic.Int32
			f.finalTurn(1, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
				reads.Add(1)
				_, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Read the checks", "findings": []any{}})
				return err
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c, err := reconcile.New(repo, reconcile.Options{Worker: "final-slot-outcome", Now: f.s.now, Concurrent: concurrentOperation, Class: operationClass, Limit: InspectForTest(f.s).ModelOperationLimit(repo, nil), Adapters: map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RunnerBoundary: a}})
			must(t, err)
			defer func() { cancel(); c.Wait() }()
			must(t, c.Pass(ctx))
			awaitCheck(t, checks.entered)
			// A deliberately short timeout may already have returned by the
			// time admission is probed. Other outcomes remain held by the fake.
			if outcome != "timeout" {
				if got := InspectForTest(f.s).Pool().Slots(nil).Used[committeeRole]; got != 1 {
					t.Fatalf("checks hold %d committee slots", got)
				}
				// A second committee operation cannot overlap running review
				// checks beyond committee=1.
				other, err := config.NewWorkstreamID()
				must(t, err)
				must(t, repo.CreateWorkstream(ctx, other, f.s.now(), ownerActor))
				r, admitted, err := InspectForTest(f.s).Pool().TryReserve(repo, other, committeeRole, committeeAgent(1), &f.s.cfg.Capacity)
				must(t, err)
				if admitted {
					r()
					t.Fatal("review checks exceeded committee capacity")
				}
			}
			switch outcome {
			case ChecksPassed:
				checks.release <- checkOutcome{Result: CheckResult{Output: passedReport}}
			case ChecksFailed:
				checks.release <- checkOutcome{Result: CheckResult{ExitCode: 1, Output: failedReport("dag://test")}}
			case ChecksIncomplete:
				checks.release <- checkOutcome{Result: CheckResult{ExitCode: -1}, Err: errors.New("engine unavailable")}
			case "cancelled":
				cancel()
			case "superseded":
				h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, Revision: 1, ID: "supersede-final", Project: repo.Project(), Workstream: stream, At: f.s.now(), Actor: ownerActor, Cause: "supersede-final"}
				_, err := repo.SetFeatureState(ctx, h, AbandonedState, "Replaced by another workstream")
				must(t, err)
				checks.release <- checkOutcome{Result: CheckResult{Output: passedReport}}
			}
			must(t, c.Wait())
			if outcome == "cancelled" {
				if len(streamDocuments(t, repo, stream, finalChecksDocument(1))) != 0 {
					t.Fatal("cancelled checks recorded a result")
				}
			} else {
				docs := streamDocuments(t, repo, stream, finalChecksDocument(1))
				if len(docs) != 1 {
					t.Fatalf("check results: %+v", docs)
				}
				var run FinalCheckRun
				must(t, json.Unmarshal([]byte(docs[0].Content), &run))
				want := outcome
				if outcome == "timeout" {
					want = ChecksIncomplete
				}
				if outcome == "superseded" {
					want = ChecksPassed
				}
				if run.Status != want {
					t.Fatalf("checks %s, want %s", run.Status, want)
				}
			}
			if (outcome == ChecksPassed || outcome == ChecksFailed) && reads.Load() != 1 {
				t.Fatal("completed checks did not reach the reader")
			}
			if outcome == "superseded" && reads.Load() != 0 {
				t.Fatal("superseded review ran a reader")
			}
			assertReviewSlotsFree(t, f.s, repo, stream, op)
		})
	}
}

func TestFinalReviewRestartDuringCapacityWaits(t *testing.T) {
	t.Parallel()
	for _, wait := range []string{"checks", "committee"} {
		t.Run(wait, func(t *testing.T) {
			t.Parallel()
			f, stream, repo, a := newFinalFixture(t, "restart-wait-"+wait)
			_, op := assembleBoth(t, f, repo, a, stream, map[string]string{"one.go": "package feature\n"}, map[string]string{"two.go": "package feature\n"})
			checks := newSlotChecks()
			f.s.setReviewChecks(checks)
			f.opts.reviewChecks = checks
			InspectForTest(f.s).ResizeCheckSlots(1)
			var reads atomic.Int32
			f.finalTurn(1, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
				reads.Add(1)
				_, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Resumed review", "findings": []any{}})
				return err
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			holder, err := InspectForTest(f.s).AcquireCheckSlot(ctx, CheckSlotRun{Workstream: sibling, Unit: "held"})
			must(t, err)
			defer holder()
			c, err := reconcile.New(repo, reconcile.Options{Worker: "restart-wait", Now: f.s.now, Concurrent: concurrentOperation, Class: operationClass, Limit: InspectForTest(f.s).ModelOperationLimit(repo, nil), Adapters: map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RunnerBoundary: a}})
			must(t, err)
			defer func() { cancel(); c.Wait() }()
			must(t, c.Pass(ctx))
			awaitCheckSlots(t, f.s, 1, 1)
			var releaseOther func()
			if wait == "committee" {
				other, err := config.NewWorkstreamID()
				must(t, err)
				must(t, repo.CreateWorkstream(ctx, other, f.s.now(), ownerActor))
				var admitted bool
				releaseOther, admitted, err = InspectForTest(f.s).Pool().TryReserve(repo, other, committeeRole, committeeAgent(1), &f.s.cfg.Capacity)
				must(t, err)
				if !admitted {
					t.Fatal("waiting review did not yield committee")
				}
				defer releaseOther()
				holder()
				awaitCheckSlots(t, f.s, 0, 0)
			}
			for range 3 {
				must(t, c.Pass(ctx))
			}
			assertSingleReviewEffect(t, repo, stream, FinalReviewAction)
			if len(finalCheckStarts(t, repo, stream)) != 0 {
				t.Fatal("waiting review recorded an attempt")
			}
			cancel()
			must(t, c.Wait())
			holder()
			if releaseOther != nil {
				releaseOther()
			}
			assertReviewSlotsFree(t, f.s, repo, stream, op)
			must(t, repo.Close())
			var resume atomic.Bool
			ticks := make(chan time.Time, 1)
			f.opts.Reconciliation.Ticks = ticks
			f.opts.Reconciliation.Hold = func(_ config.WorkstreamID, candidate coreadapter.Operation) bool {
				return candidate.ID != op.ID || !resume.Load()
			}
			f.start(t)
			defer f.stop(t)
			repo, err = f.s.repository(f.project)
			must(t, err)
			assertReviewSlotsFree(t, f.s, repo, stream, op)
			if len(finalCheckStarts(t, repo, stream)) != 0 {
				t.Fatal("restart manufactured an attempt")
			}
			resume.Store(true)
			ticks <- f.s.now()
			awaitCheck(t, checks.entered)
			checks.release <- checkOutcome{Result: CheckResult{Output: passedReport}}
			records := awaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return finalOperations(t, repo, stream) })
			if len(records) != 1 || records[0].Operation.ID != op.ID || records[0].Result == nil || records[0].Result.Outcome != "succeeded" {
				t.Fatalf("restarted review identity/result: %+v", records)
			}
			if len(finalCheckStarts(t, repo, stream)) != 1 || reads.Load() != 1 {
				t.Fatal("restart duplicated checks or reader")
			}
			soon(t, "restarted review releases committee", func() bool { return InspectForTest(f.s).Pool().Slots(nil).Used[committeeRole] == 0 })
			assertReviewSlotsFree(t, f.s, repo, stream, op)
		})
	}
}
