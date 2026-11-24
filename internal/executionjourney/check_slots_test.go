package executionjourney

import (
	"context"
	"errors"
	"fmt"
	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
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

type slotChecks struct {
	mu         sync.Mutex
	used, peak int
	entered    chan time.Duration
	release    chan fixturekit.CheckOutcome
}

func newSlotChecks() *slotChecks {
	return &slotChecks{entered: make(chan time.Duration, 16), release: make(chan fixturekit.CheckOutcome, 16)}
}
func (c *slotChecks) List(context.Context, string, api.CheckIdentity) ([]string, error) {
	return nil, nil
}
func (c *slotChecks) Check(ctx context.Context, _ string, _ api.CheckIdentity, _ []string) (api.CheckResult, error) {
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
		return api.CheckResult{ExitCode: -1}, ctx.Err()
	}
}

func requestUnitCheck(t *testing.T, s *Service, repo *trace.Repository, stream config.WorkstreamID) (*checkers, coreadapter.Operation) {
	t.Helper()
	c := &checkers{masons: newMasonController(s, repo)}
	fixturekit.Must(t, c.Pass(context.Background()))
	ops, err := repo.Operations(stream)
	fixturekit.Must(t, err)
	for _, op := range ops {
		if op.Operation.Action == service.CheckAction {
			return c, op.Operation
		}
	}
	t.Fatal("no unit check requested")
	return nil, coreadapter.Operation{}
}

func awaitCheckSlots(t *testing.T, s *Service, used, waiting int) service.CheckSlotStatus {
	t.Helper()
	intakefixture.Soon(t, fmt.Sprintf("%d used check slots and %d waiting", used, waiting), func() bool {
		st := s.CheckSlots()
		return st.Used == used && len(st.Waiting) == waiting
	})
	return s.CheckSlots()
}

func queuedSlot(ctx context.Context, s *Service, run service.CheckSlotRun) <-chan func() {
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
		fixturekit.Must(t, err)
		effective, err := client.Configuration(context.Background())
		fixturekit.Must(t, err)
		if effective.Effective.Capacity.Checks != limit || s.CheckSlots().Limit != limit {
			t.Fatalf("effective checks %d, pool %+v", effective.Effective.Capacity.Checks, s.CheckSlots())
		}
	}
	setLimit(1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, err := InspectForTest(s).AcquireCheckSlot(ctx, service.CheckSlotRun{Workstream: fixturekit.Stream, Unit: "first"})
	fixturekit.Must(t, err)
	defer first()
	second := queuedSlot(ctx, s, service.CheckSlotRun{Workstream: sibling, Review: 1})
	awaitCheckSlots(t, s, 1, 1)
	third := queuedSlot(ctx, s, service.CheckSlotRun{Workstream: fixturekit.OtherStream, Session: true})
	st := awaitCheckSlots(t, s, 1, 2)
	if st.Waiting[0].Review != 1 || !st.Waiting[1].Session {
		t.Fatalf("queue order %+v", st)
	}

	setLimit(2)
	releaseSecond := awaitCheck(t, second)
	defer releaseSecond()
	awaitCheckSlots(t, s, 2, 1)

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
	checker := newSlotChecks()
	f, stream, repo := newChecksFixture(t, "slot-timeout", checker)
	InspectForTest(f.s).ResizeCheckSlots(1)
	f.s.cfg.Project.ChecksTimeout = "1s"
	holder, err := InspectForTest(f.s).AcquireCheckSlot(context.Background(), service.CheckSlotRun{Workstream: sibling, Session: true})
	fixturekit.Must(t, err)
	defer holder()
	c, op := requestUnitCheck(t, f.s, repo, stream)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := intakefixture.ApplyCheck(ctx, c, op)
	awaitCheckSlots(t, f.s, 1, 1)

	time.Sleep(1200 * time.Millisecond)
	runs, err := execution.CheckRuns(repo, stream)
	fixturekit.Must(t, err)
	if len(runs) != 0 || len(intakefixture.StreamDocuments(t, repo, stream, execution.CheckSelectionDocument("resume", 1))) != 0 {
		t.Fatalf("waiting run started or finished: %+v", runs)
	}
	state, err := repo.Workflow(stream, execution.ChecksSubject("resume"))
	fixturekit.Must(t, err)
	if state.Value != "requested-1" {
		t.Fatalf("waiting changed checks state: %+v", state)
	}
	holder()
	if remaining := awaitCheck(t, checker.entered); remaining < 500*time.Millisecond {
		t.Fatalf("timeout lost to wait: %s", remaining)
	}
	checker.release <- fixturekit.CheckOutcome{Result: api.CheckResult{Output: fixturekit.PassedReport}}
	fixturekit.Must(t, awaitCheck(t, finished))
	run := onlyRun(t, repo, stream)
	if run.Status != checks.ChecksPassed {
		t.Fatalf("wait produced %+v", run)
	}
	n, err := c.incompleteSinceEntry(stream, "resume", run, []execution.UnitCheckRun{run})
	fixturekit.Must(t, err)
	if n != 0 {
		t.Fatalf("wait counted as %d incomplete runs", n)
	}
	awaitCheckSlots(t, f.s, 0, 0)
}

func TestUnitCheckSlotsReleaseEveryOutcome(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{checks.ChecksPassed, checks.ChecksFailed, checks.ChecksIncomplete, "timeout", "cancelled", checks.ChecksSuperseded} {
		t.Run(outcome, func(t *testing.T) {
			t.Parallel()
			checker := newSlotChecks()
			f, stream, repo := newChecksFixture(t, "slot-"+outcome, checker)
			InspectForTest(f.s).ResizeCheckSlots(1)
			c, op := requestUnitCheck(t, f.s, repo, stream)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var holder func()
			if outcome == checks.ChecksSuperseded {
				var err error
				holder, err = InspectForTest(f.s).AcquireCheckSlot(ctx, service.CheckSlotRun{Session: true})
				fixturekit.Must(t, err)
				defer holder()
			}
			if outcome == "timeout" {
				f.s.cfg.Project.ChecksTimeout = "20ms"
			}
			finished := intakefixture.ApplyCheck(ctx, c, op)
			if outcome == checks.ChecksSuperseded {
				awaitCheckSlots(t, f.s, 1, 1)
				state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
				fixturekit.Must(t, err)
				h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "supersede-waiter", Revision: 1, Project: repo.Project(), Workstream: stream, Unit: "resume", At: f.s.now(), Actor: execution.ForemanActor, Cause: execution.ReportDocument("resume")}
				_, err = repo.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: trace.UnitSubject("resume"), From: execution.UnitChecking, To: intake.UnitImplementing, Reason: "candidate replaced while waiting"}})
				fixturekit.Must(t, err)
				holder()
			} else {
				awaitCheck(t, checker.entered)
				switch outcome {
				case checks.ChecksPassed:
					checker.release <- fixturekit.CheckOutcome{Result: api.CheckResult{Output: fixturekit.PassedReport}}
				case checks.ChecksFailed:
					checker.release <- fixturekit.CheckOutcome{Result: api.CheckResult{ExitCode: 1, Output: fixturekit.FailedReport("dag://test")}}
				case checks.ChecksIncomplete:
					checker.release <- fixturekit.CheckOutcome{Result: api.CheckResult{ExitCode: -1}, Err: errors.New("engine unavailable")}
				case "cancelled":
					cancel()
				}
			}
			err := awaitCheck(t, finished)
			if outcome == "cancelled" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled run: %v", err)
				}
				runs, readErr := execution.CheckRuns(repo, stream)
				fixturekit.Must(t, readErr)
				if len(runs) != 0 {
					t.Fatalf("cancelled run recorded: %+v", runs)
				}
			} else {
				fixturekit.Must(t, err)
				want := outcome
				if want == "timeout" {
					want = checks.ChecksIncomplete
				}
				if run := onlyRun(t, repo, stream); run.Status != want {
					t.Fatalf("run %+v, want %s", run, want)
				}
			}

			nextCtx, nextCancel := context.WithTimeout(context.Background(), time.Second)
			defer nextCancel()
			release, err := InspectForTest(f.s).AcquireCheckSlot(nextCtx, service.CheckSlotRun{Workstream: sibling, Review: 1})
			fixturekit.Must(t, err)
			release()
			awaitCheckSlots(t, f.s, 0, 0)
		})
	}
}

func TestCheckSlotsSharedByUnitAndSessionsAcrossProjects(t *testing.T) {
	t.Parallel()
	checker := newSlotChecks()
	f, unitStream, repo := newChecksFixture(t, "factory-slots", checker)
	_, unitOp := requestUnitCheck(t, f.s, repo, unitStream)
	primary := repo.Project()
	fixturekit.Must(t, repo.Close())

	f.opts.Reconciliation.Hold = func(config.WorkstreamID, coreadapter.Operation) bool { return true }
	f.opts.reviewChecks = checker
	f.start(t)
	defer f.stop(t)
	repo, err := f.s.repository(primary)
	fixturekit.Must(t, err)

	home := filepath.Dir(f.clone)
	clone := filepath.Join(home, "other-clone")
	bare := filepath.Join(home, "remotes", "acme", "other.git")
	fixturekit.Must(t, os.MkdirAll(filepath.Dir(bare), 0700))
	fixturekit.DemoGit(t, home, "clone", "--quiet", "--bare", "--no-hardlinks", f.clone, bare)
	fixturekit.DemoGit(t, home, "clone", "--quiet", "--no-hardlinks", bare, clone)
	added, err := f.c.AddProject(context.Background(), api.ProjectAddRequest{Name: "other", Upstream: "acme/other", Clone: clone})
	fixturekit.Must(t, err)
	writeCharter(t, f.c, added.Project.ID, "1. Keep changes small.\n")
	sessions := make([]config.WorkstreamID, 2)
	for i := range sessions {
		opened, err := f.c.OpenSession(context.Background(), api.SessionOpenRequest{Project: added.Project.ID, Profile: "default"})
		fixturekit.Must(t, err)
		sessions[i] = opened.Session.Workstream
	}
	if primary == added.Project.ID {
		t.Fatal("fixture needs two projects")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &checkers{masons: &masons{s: f.s, cfg: f.s.about(repo), repository: repo}}
	unitDone := intakefixture.ApplyCheck(ctx, c, unitOp)
	awaitCheck(t, checker.entered)
	sessionDone := make(chan error, 2)
	for i, session := range sessions {
		go func() {
			run, err := f.c.SessionCheck(ctx, session)
			if err == nil && run.Status != checks.ChecksPassed {
				err = fmt.Errorf("session run %+v", run)
			}
			sessionDone <- err
		}()
		if i == 0 {
			awaitCheck(t, checker.entered)
			continue
		}
		st := awaitCheckSlots(t, f.s, 2, 1)
		if w := st.Waiting[0]; w.Project != added.Project.ID || w.Workstream != session || !w.Session {
			t.Fatalf("waiting session identity %+v", w)
		}
	}

	checker.release <- fixturekit.CheckOutcome{Result: api.CheckResult{Output: fixturekit.PassedReport}}
	awaitCheck(t, checker.entered)
	for range 2 {
		checker.release <- fixturekit.CheckOutcome{Result: api.CheckResult{Output: fixturekit.PassedReport}}
	}
	fixturekit.Must(t, awaitCheck(t, unitDone))
	for range 2 {
		fixturekit.Must(t, awaitCheck(t, sessionDone))
	}
	awaitCheckSlots(t, f.s, 0, 0)
	checker.mu.Lock()
	defer checker.mu.Unlock()
	if checker.peak != 2 || checker.used != 0 {
		t.Fatalf("runner peak %d, still executing %d", checker.peak, checker.used)
	}
}

func TestSessionCheckSlotWaitRespectsRequestCancellation(t *testing.T) {
	t.Parallel()
	s, client, _, _, stream, _, _ := newSessionControlsFixture(t)
	InspectForTest(s).ResizeCheckSlots(1)
	checker := newSlotChecks()
	s.setReviewChecks(checker)
	holder, err := InspectForTest(s).AcquireCheckSlot(context.Background(), service.CheckSlotRun{Workstream: sibling, Unit: "held"})
	fixturekit.Must(t, err)
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
	_, _, repo, apiErr := InspectForTest(s).SessionTrace(string(stream))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	runs, err := service.SessionCheckRuns(repo, stream)
	fixturekit.Must(t, err)
	if len(runs) != 0 {
		t.Fatalf("waiting request recorded check runs: %+v", runs)
	}
	holder()
	finished = make(chan error, 1)
	go func() { _, err := client.SessionCheck(context.Background(), stream); finished <- err }()
	awaitCheck(t, checker.entered)
	checker.release <- fixturekit.CheckOutcome{Result: api.CheckResult{Output: fixturekit.PassedReport}}
	fixturekit.Must(t, awaitCheck(t, finished))
	awaitCheckSlots(t, s, 0, 0)
}
