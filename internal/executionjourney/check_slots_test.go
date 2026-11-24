package executionjourney

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
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
func (c *slotChecks) List(context.Context, string, api.CheckIdentity) ([]string, error) {
	return nil, nil
}
func (c *slotChecks) Check(ctx context.Context, _ string, _ api.CheckIdentity, _ []string) (CheckResult, error) {
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
		files.write(t, files.topText+fmt.Sprintf("\n[capacity]\nchecks = %d\n", limit))
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

func TestCheckSlotsSharedByUnitAndSessionsAcrossProjects(t *testing.T) {
	t.Parallel()
	checks := newSlotChecks()
	f, unitStream, repo := newChecksFixture(t, "factory-slots", checks)
	_, unitOp := requestUnitCheck(t, f.s, repo, unitStream)
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
	sessionDone := make(chan error, 2)
	for i, session := range sessions {
		go func() {
			run, err := f.c.SessionCheck(ctx, session)
			if err == nil && run.Status != ChecksPassed {
				err = fmt.Errorf("session run %+v", run)
			}
			sessionDone <- err
		}()
		if i == 0 {
			awaitCheck(t, checks.entered)
			continue
		}
		st := awaitCheckSlots(t, f.s, 2, 1)
		if w := st.Waiting[0]; w.Project != added.Project.ID || w.Workstream != session || !w.Session {
			t.Fatalf("waiting session identity %+v", w)
		}
	}
	// Both executing runs keep the slots until their runner returns. Each
	// return admits the waiter; all three entry points eventually complete.
	checks.release <- checkOutcome{Result: CheckResult{Output: passedReport}}
	awaitCheck(t, checks.entered)
	for range 2 {
		checks.release <- checkOutcome{Result: CheckResult{Output: passedReport}}
	}
	must(t, awaitCheck(t, unitDone))
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
