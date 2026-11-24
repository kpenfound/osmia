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

	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

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

func requestUnitCheck(t *testing.T, s *journey.Service, repo *trace.Repository, stream config.WorkstreamID) (*journey.Checkers, coreadapter.Operation) {
	t.Helper()
	c := &journey.Checkers{Masons: journey.NewMasonController(s, repo)}
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

func awaitCheckSlots(t *testing.T, s *journey.Service, used, waiting int) service.CheckSlotStatus {
	t.Helper()
	intakefixture.Soon(t, fmt.Sprintf("%d used check slots and %d waiting", used, waiting), func() bool {
		st := s.CheckSlots()
		return st.Used == used && len(st.Waiting) == waiting
	})
	return s.CheckSlots()
}

func TestCheckSlotsReloadAndFIFO(t *testing.T) {
	t.Parallel()
	opts := journey.Fixture(t)
	s, client := journey.StartT(t, opts)
	files := fixturekit.ConfigFiles(t, opts.Config.Root)
	setLimit := func(limit int) {
		files.Write(t, files.TopText+fmt.Sprintf("\n[capacity]\nchecks = %d\n", limit))
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
	first, err := journey.InspectForTest(s).AcquireCheckSlot(ctx, service.CheckSlotRun{Workstream: fixturekit.Stream, Unit: "first"})
	fixturekit.Must(t, err)
	defer first()
	second := fixturekit.QueueSlot(ctx, func(ctx context.Context) (func(), error) {
		return journey.InspectForTest(s).AcquireCheckSlot(ctx, (service.CheckSlotRun{Workstream: fixturekit.Sibling, Review: 1}))
	})
	awaitCheckSlots(t, s, 1, 1)
	third := fixturekit.QueueSlot(ctx, func(ctx context.Context) (func(), error) {
		return journey.InspectForTest(s).AcquireCheckSlot(ctx, (service.CheckSlotRun{Workstream: fixturekit.OtherStream, Session: true}))
	})
	st := awaitCheckSlots(t, s, 1, 2)
	if st.Waiting[0].Review != 1 || !st.Waiting[1].Session {
		t.Fatalf("queue order %+v", st)
	}

	setLimit(2)
	releaseSecond := intakefixture.AwaitCheck(t, second)
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
	releaseThird := intakefixture.AwaitCheck(t, third)
	releaseThird()
	awaitCheckSlots(t, s, 0, 0)
}

func TestUnitCheckSlotWaitPreservesTimeoutAndIncompleteCount(t *testing.T) {
	t.Parallel()
	checker := newSlotChecks()
	f, stream, repo := newChecksFixture(t, "slot-timeout", checker)
	journey.InspectForTest(f.S).ResizeCheckSlots(1)
	f.S.Cfg.Project.ChecksTimeout = "1s"
	holder, err := journey.InspectForTest(f.S).AcquireCheckSlot(context.Background(), service.CheckSlotRun{Workstream: fixturekit.Sibling, Session: true})
	fixturekit.Must(t, err)
	defer holder()
	c, op := requestUnitCheck(t, f.S, repo, stream)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := intakefixture.ApplyCheck(ctx, c, op)
	awaitCheckSlots(t, f.S, 1, 1)

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
	if remaining := intakefixture.AwaitCheck(t, checker.entered); remaining < 500*time.Millisecond {
		t.Fatalf("timeout lost to wait: %s", remaining)
	}
	checker.release <- fixturekit.CheckOutcome{Result: api.CheckResult{Output: fixturekit.PassedReport}}
	fixturekit.Must(t, intakefixture.AwaitCheck(t, finished))
	run := onlyRun(t, repo, stream)
	if run.Status != checks.ChecksPassed {
		t.Fatalf("wait produced %+v", run)
	}
	n, err := c.Driver().IncompleteSinceEntry(stream, "resume", run, []execution.UnitCheckRun{run})
	fixturekit.Must(t, err)
	if n != 0 {
		t.Fatalf("wait counted as %d incomplete runs", n)
	}
	awaitCheckSlots(t, f.S, 0, 0)
}

func TestUnitCheckSlotsReleaseEveryOutcome(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{checks.ChecksPassed, checks.ChecksFailed, checks.ChecksIncomplete, "timeout", "cancelled", checks.ChecksSuperseded} {
		t.Run(outcome, func(t *testing.T) {
			t.Parallel()
			checker := newSlotChecks()
			f, stream, repo := newChecksFixture(t, "slot-"+outcome, checker)
			journey.InspectForTest(f.S).ResizeCheckSlots(1)
			c, op := requestUnitCheck(t, f.S, repo, stream)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var holder func()
			if outcome == checks.ChecksSuperseded {
				var err error
				holder, err = journey.InspectForTest(f.S).AcquireCheckSlot(ctx, service.CheckSlotRun{Session: true})
				fixturekit.Must(t, err)
				defer holder()
			}
			if outcome == "timeout" {
				f.S.Cfg.Project.ChecksTimeout = "20ms"
			}
			finished := intakefixture.ApplyCheck(ctx, c, op)
			if outcome == checks.ChecksSuperseded {
				awaitCheckSlots(t, f.S, 1, 1)
				state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
				fixturekit.Must(t, err)
				h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "supersede-waiter", Revision: 1, Project: repo.Project(), Workstream: stream, Unit: "resume", At: f.S.Now(), Actor: execution.ForemanActor, Cause: execution.ReportDocument("resume")}
				_, err = repo.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: trace.UnitSubject("resume"), From: execution.UnitChecking, To: intake.UnitImplementing, Reason: "candidate replaced while waiting"}})
				fixturekit.Must(t, err)
				holder()
			} else {
				intakefixture.AwaitCheck(t, checker.entered)
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
			err := intakefixture.AwaitCheck(t, finished)
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
			release, err := journey.InspectForTest(f.S).AcquireCheckSlot(nextCtx, service.CheckSlotRun{Workstream: fixturekit.Sibling, Review: 1})
			fixturekit.Must(t, err)
			release()
			awaitCheckSlots(t, f.S, 0, 0)
		})
	}
}

func TestCheckSlotsSharedByUnitAndSessionsAcrossProjects(t *testing.T) {
	t.Parallel()
	checker := newSlotChecks()
	f, unitStream, repo := newChecksFixture(t, "factory-slots", checker)
	_, unitOp := requestUnitCheck(t, f.S, repo, unitStream)
	primary := repo.Project()
	fixturekit.Must(t, repo.Close())

	f.Opts.Reconciliation.Hold = func(config.WorkstreamID, coreadapter.Operation) bool { return true }
	f.Opts.ReviewChecks = checker
	f.Start(t)
	defer f.Stop(t)
	repo, err := f.S.Repository(primary)
	fixturekit.Must(t, err)

	home := filepath.Dir(f.Clone)
	clone := filepath.Join(home, "other-clone")
	bare := filepath.Join(home, "remotes", "acme", "other.git")
	fixturekit.Must(t, os.MkdirAll(filepath.Dir(bare), 0700))
	fixturekit.DemoGit(t, home, "clone", "--quiet", "--bare", "--no-hardlinks", f.Clone, bare)
	fixturekit.DemoGit(t, home, "clone", "--quiet", "--no-hardlinks", bare, clone)
	added, err := f.C.AddProject(context.Background(), api.ProjectAddRequest{Name: "other", Upstream: "acme/other", Clone: clone})
	fixturekit.Must(t, err)
	intakefixture.WriteCharter(t, f.C, added.Project.ID, "1. Keep changes small.\n")
	sessions := make([]config.WorkstreamID, 2)
	for i := range sessions {
		opened, err := f.C.OpenSession(context.Background(), api.SessionOpenRequest{Project: added.Project.ID, Profile: "default"})
		fixturekit.Must(t, err)
		sessions[i] = opened.Session.Workstream
	}
	if primary == added.Project.ID {
		t.Fatal("fixture needs two projects")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &journey.Checkers{Masons: &journey.Masons{S: f.S, Cfg: f.S.About(repo), Repository: repo}}
	unitDone := intakefixture.ApplyCheck(ctx, c, unitOp)
	intakefixture.AwaitCheck(t, checker.entered)
	sessionDone := make(chan error, 2)
	for i, session := range sessions {
		go func() {
			run, err := f.C.SessionCheck(ctx, session)
			if err == nil && run.Status != checks.ChecksPassed {
				err = fmt.Errorf("session run %+v", run)
			}
			sessionDone <- err
		}()
		if i == 0 {
			intakefixture.AwaitCheck(t, checker.entered)
			continue
		}
		st := awaitCheckSlots(t, f.S, 2, 1)
		if w := st.Waiting[0]; w.Project != added.Project.ID || w.Workstream != session || !w.Session {
			t.Fatalf("waiting session identity %+v", w)
		}
	}

	checker.release <- fixturekit.CheckOutcome{Result: api.CheckResult{Output: fixturekit.PassedReport}}
	intakefixture.AwaitCheck(t, checker.entered)
	for range 2 {
		checker.release <- fixturekit.CheckOutcome{Result: api.CheckResult{Output: fixturekit.PassedReport}}
	}
	fixturekit.Must(t, intakefixture.AwaitCheck(t, unitDone))
	for range 2 {
		fixturekit.Must(t, intakefixture.AwaitCheck(t, sessionDone))
	}
	awaitCheckSlots(t, f.S, 0, 0)
	checker.mu.Lock()
	defer checker.mu.Unlock()
	if checker.peak != 2 || checker.used != 0 {
		t.Fatalf("runner peak %d, still executing %d", checker.peak, checker.used)
	}
}

func TestSessionCheckSlotWaitRespectsRequestCancellation(t *testing.T) {
	t.Parallel()
	s, client, _, _, stream, _, _ := newSessionControlsFixture(t)
	journey.InspectForTest(s).ResizeCheckSlots(1)
	checker := newSlotChecks()
	setReviewChecks(s, checker)
	holder, err := journey.InspectForTest(s).AcquireCheckSlot(context.Background(), service.CheckSlotRun{Workstream: fixturekit.Sibling, Unit: "held"})
	fixturekit.Must(t, err)
	defer holder()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { _, err := client.SessionCheck(ctx, stream); finished <- err }()
	awaitCheckSlots(t, s, 1, 1)
	cancel()
	if err := intakefixture.AwaitCheck(t, finished); err == nil || !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("request cancellation: %v (context %v)", err, ctx.Err())
	}
	awaitCheckSlots(t, s, 1, 0)
	_, _, repo, apiErr := journey.InspectForTest(s).SessionTrace(string(stream))
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
	intakefixture.AwaitCheck(t, checker.entered)
	checker.release <- fixturekit.CheckOutcome{Result: api.CheckResult{Output: fixturekit.PassedReport}}
	fixturekit.Must(t, intakefixture.AwaitCheck(t, finished))
	awaitCheckSlots(t, s, 0, 0)
}
