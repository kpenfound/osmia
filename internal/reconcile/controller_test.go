package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

const projectID config.ProjectID = "p_00000000000000000000000000000001"
const streamID config.WorkstreamID = "w_00000000000000000000000000000001"

var epoch = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
var injected = errors.New("interrupted")

type fakeClock struct {
	mu sync.Mutex
	at time.Time
}

func (f *fakeClock) Now() time.Time          { f.mu.Lock(); defer f.mu.Unlock(); return f.at }
func (f *fakeClock) Advance(d time.Duration) { f.mu.Lock(); defer f.mu.Unlock(); f.at = f.at.Add(d) }

// External state outlives both the controller and its trace handle.
type fakeSystem struct {
	mu                        sync.Mutex
	boundary                  coreadapter.OperationBoundary
	effects                   map[string]coreadapter.OperationResult
	inspections, applications []string
	unknown                   bool
	failBeforeEffect          bool
	inspectErr, applyErr      error
	entered, unblock          chan struct{}
	inspected                 chan struct{}
}

func (f *fakeSystem) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspections = append(f.inspections, op.ID)
	if f.inspected != nil {
		select {
		case f.inspected <- struct{}{}:
		default:
		}
	}
	if op.Boundary != f.boundary {
		return coreadapter.Observation{}, fmt.Errorf("wrong adapter")
	}
	if f.unknown {
		return coreadapter.Observation{State: coreadapter.EffectUnknown, Evidence: "Resource is still running or cannot be located"}, f.inspectErr
	}
	if result, ok := f.effects[op.ID]; ok {
		return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: "Resource identity has a terminal result", Result: &result}, f.inspectErr
	}
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "No resource or in-flight work with this identity"}, f.inspectErr
}
func (f *fakeSystem) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	if f.entered != nil {
		close(f.entered)
		select {
		case <-f.unblock:
		case <-ctx.Done():
			return coreadapter.OperationResult{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applications = append(f.applications, op.ID)
	if f.failBeforeEffect {
		return coreadapter.OperationResult{}, injected
	}
	// Deliberately not idempotent: a repeated Apply must be caught by assertions.
	result := coreadapter.OperationResult{Outcome: "complete", Evidence: string(f.boundary) + " resource persisted", Data: json.RawMessage(`{"resource":"local"}`)}
	f.effects[op.ID] = result
	return result, f.applyErr
}
func (f *fakeSystem) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.inspections), len(f.applications)
}

type fixture struct {
	repository *trace.Repository
	root       config.Root
	project    config.Project
	event      trace.Event
	clock      *fakeClock
	system     *fakeSystem
}

func setup(t *testing.T, boundary coreadapter.OperationBoundary) *fixture {
	t.Helper()
	base := t.TempDir()
	root, err := config.ResolveRoot(filepath.Join(base, "root"), "")
	must(t, err)
	project := config.Project{ID: projectID, Clone: filepath.Join(base, "target")}
	r, err := trace.Create(context.Background(), root, project, epoch, trace.Actor{Kind: "owner", ID: "local"})
	must(t, err)
	must(t, r.CreateWorkstream(context.Background(), streamID, epoch, trace.Actor{Kind: "owner", ID: "local"}))
	event := trace.Event{ID: trace.EventID("transition", "effect"), Kind: "local-effect", Body: "Perform authorized local work"}
	event.Operation = &coreadapter.Operation{ID: trace.OperationID(projectID, streamID, event.ID), Boundary: boundary, Action: "prepare", Input: json.RawMessage(`{"scope":{"Project":"test"}}`)}
	tx := trace.Transaction{Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: 1, ID: "transition", Revision: 1, Project: projectID, Workstream: streamID, At: epoch, Actor: trace.Actor{Kind: "service", ID: "scheduler"}, Cause: "owner-request", Depth: 2}, Subject: "local", To: "pending", Reason: "Authorized work"}, Events: []trace.Event{event}}
	_, err = r.Transact(context.Background(), tx)
	must(t, err)
	f := &fixture{r, root, project, event, &fakeClock{at: epoch}, &fakeSystem{boundary: boundary, effects: map[string]coreadapter.OperationResult{}}}
	t.Cleanup(func() { f.repository.Close() })
	return f
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func (f *fixture) controller(t *testing.T) *Controller {
	t.Helper()
	c, err := New(f.repository, Options{Worker: "test-controller", Now: f.clock.Now, Adapters: map[coreadapter.OperationBoundary]coreadapter.Reconciler{f.system.boundary: f.system}, RetryDelay: time.Minute})
	must(t, err)
	return c
}
func (f *fixture) reopen(t *testing.T) {
	t.Helper()
	must(t, f.repository.Close())
	r, err := trace.Open(f.root, f.project)
	must(t, err)
	f.repository = r
}
func (f *fixture) record(t *testing.T) trace.OperationRecord {
	t.Helper()
	records, err := f.repository.Operations(streamID)
	must(t, err)
	if len(records) != 1 {
		t.Fatalf("records: %+v", records)
	}
	return records[0]
}
func (f *fixture) completed(t *testing.T) {
	t.Helper()
	record := f.record(t)
	if !record.Acknowledged || record.Result == nil {
		t.Fatalf("not complete: %+v", record)
	}
	entries, err := f.repository.Outbox(streamID)
	must(t, err)
	if len(entries) != 1 || !entries[0].Acknowledged {
		t.Fatal(entries)
	}
	_, applies := f.system.counts()
	if applies != 1 {
		t.Fatalf("effect applied %d times", applies)
	}
	results, acknowledgements := 0, 0
	for _, a := range record.History {
		if a.Token == "" || a.Session == "" || a.Cause != f.event.Operation.ID || a.At.IsZero() || a.Actor.ID != "test-controller" || a.Depth != 2 {
			t.Fatalf("lost provenance: %+v", a)
		}
		if a.Kind == "result" {
			results++
		}
		if a.Kind == "acknowledge" {
			acknowledgements++
		}
	}
	if results != 1 || acknowledgements != 1 {
		t.Fatalf("results=%d acknowledgements=%d", results, acknowledgements)
	}
	f.system.mu.Lock()
	defer f.system.mu.Unlock()
	for _, id := range append(append([]string{}, f.system.inspections...), f.system.applications...) {
		if id != f.event.Operation.ID {
			t.Fatalf("identity changed: %s", id)
		}
	}
}

func TestRestartAtEveryBoundary(t *testing.T) {
	steps := []string{"before-claim", "after-claim", "before-inspection", "after-inspection", "after-observation", "before-effect", "after-effect", "before-result", "after-result", "before-acknowledgement", "after-acknowledgement"}
	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			f := setup(t, coreadapter.RepositoryBoundary)
			c := f.controller(t)
			c.boundary = func(name string) error {
				if name == step {
					return injected
				}
				return nil
			}
			if err := c.Pass(context.Background()); !errors.Is(err, injected) {
				t.Fatalf("boundary %s: %v", step, err)
			}
			before, _ := f.system.counts()
			f.reopen(t) // Fresh handle has no wakeup; old claim is discovered from disk.
			must(t, f.controller(t).Pass(context.Background()))
			must(t, f.controller(t).Pass(context.Background()))
			f.completed(t)
			after, _ := f.system.counts()
			if (step == "after-result" || step == "before-acknowledgement" || step == "after-acknowledgement") && after != before {
				t.Fatal("terminal result was inspected again")
			}
		})
	}
}

func TestLocalBoundariesRecoverEffectDespiteError(t *testing.T) {
	for _, boundary := range []coreadapter.OperationBoundary{coreadapter.RepositoryBoundary, coreadapter.RunnerBoundary, coreadapter.ContainerBoundary} {
		t.Run(string(boundary), func(t *testing.T) {
			f := setup(t, boundary)
			f.system.applyErr = injected
			must(t, f.controller(t).Pass(context.Background()))
			record := f.record(t)
			if record.Result != nil || record.Acknowledged || record.RetryAt.IsZero() {
				t.Fatal(record)
			}
			f.reopen(t)
			must(t, f.controller(t).Pass(context.Background()))
			inspected, _ := f.system.counts()
			if inspected != 1 {
				t.Fatal("retry ran early")
			}
			f.clock.Advance(time.Minute)
			must(t, f.controller(t).Pass(context.Background()))
			f.completed(t)
			record = f.record(t)
			if record.Observation.State != coreadapter.EffectCompleted {
				t.Fatal("effect was not reconciled")
			}
		})
	}
}

func TestAmbiguousInspectionNeverAppliesOrAcknowledges(t *testing.T) {
	for _, inspectionError := range []bool{false, true} {
		t.Run(fmt.Sprint(inspectionError), func(t *testing.T) {
			f := setup(t, coreadapter.RunnerBoundary)
			f.system.unknown = true
			if inspectionError {
				f.system.inspectErr = injected
			}
			for range 2 {
				must(t, f.controller(t).Pass(context.Background()))
				record := f.record(t)
				if record.Acknowledged || record.Result != nil || record.Observation.State != coreadapter.EffectUnknown {
					t.Fatal(record)
				}
				_, applies := f.system.counts()
				if applies != 0 {
					t.Fatal("ambiguous effect applied")
				}
				f.reopen(t)
				f.clock.Advance(time.Minute)
			}
			f.system.unknown, f.system.inspectErr = false, nil
			must(t, f.controller(t).Pass(context.Background()))
			f.completed(t)
		})
	}
}

func TestConcurrentControllersCannotOverlapEffect(t *testing.T) {
	f := setup(t, coreadapter.ContainerBoundary)
	f.system.entered, f.system.unblock = make(chan struct{}), make(chan struct{})
	first, second := f.controller(t), f.controller(t)
	errs := make(chan error, 2)
	go func() { errs <- first.Pass(context.Background()) }()
	<-f.system.entered
	// Advancing far beyond a delivery lease cannot permit a second in-flight effect.
	f.clock.Advance(24 * time.Hour)
	reached := make(chan struct{})
	second.boundary = func(step string) error {
		if step == "before-claim" {
			close(reached)
		}
		return nil
	}
	go func() { errs <- second.Pass(context.Background()) }()
	<-reached
	close(f.system.unblock)
	must(t, <-errs)
	must(t, <-errs)
	f.completed(t)
	inspected, _ := f.system.counts()
	if inspected != 1 {
		t.Fatal("concurrent worker inspected active effect")
	}
}

func TestRunScansOnStartupTicksAndDuplicateWakeups(t *testing.T) {
	f := setup(t, coreadapter.RepositoryBoundary)
	f.system.unknown = true
	f.reopen(t)
	ticks := make(chan time.Time)
	inspected := make(chan struct{}, 4)
	f.system.inspected = inspected
	c := f.controller(t)
	c.options.Ticks = ticks
	retried := make(chan struct{})
	c.boundary = func(step string) error {
		if step == "after-retry" {
			close(retried)
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	select {
	case <-inspected:
	case <-time.After(10 * time.Second):
		t.Fatal("startup did not scan")
	}
	// Synchronize with the first durable retry before changing the fake system.
	<-retried
	f.system.mu.Lock()
	f.system.unknown = false
	f.system.mu.Unlock()
	f.clock.Advance(time.Minute)
	ticks <- f.clock.Now() // No notify: periodic scanning must discover due work.
	select {
	case <-inspected:
	case <-time.After(10 * time.Second):
		t.Fatal("tick did not scan")
	}
	// Transactions emit duplicate latency hints, but contain no new effect.
	for i := range 2 {
		tx := trace.Transaction{Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: 1, ID: fmt.Sprintf("hint-%d", i), Revision: 1, Project: projectID, Workstream: streamID, At: epoch, Actor: trace.Actor{Kind: "service", ID: "test"}, Cause: "test", Depth: 0}, Subject: fmt.Sprintf("hint-%d", i), To: "done", Reason: "wake"}}
		_, err := f.repository.Transact(ctx, tx)
		must(t, err)
	}
	// A pass joins the in-flight operation and confirms duplicate wake safety.
	must(t, f.controller(t).Pass(ctx))
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	f.completed(t)
}

func TestAbsentEffectRetriesWithSameIdentity(t *testing.T) {
	f := setup(t, coreadapter.RepositoryBoundary)
	f.system.failBeforeEffect = true
	must(t, f.controller(t).Pass(context.Background()))
	f.reopen(t)
	f.clock.Advance(time.Minute)
	f.system.failBeforeEffect = false
	must(t, f.controller(t).Pass(context.Background()))
	record := f.record(t)
	if !record.Acknowledged || record.Result == nil {
		t.Fatal(record)
	}
	inspections, applications := f.system.counts()
	if inspections != 2 || applications != 2 {
		t.Fatalf("inspections=%d applications=%d", inspections, applications)
	}
	f.system.mu.Lock()
	defer f.system.mu.Unlock()
	for _, id := range append(f.system.inspections, f.system.applications...) {
		if id != f.event.Operation.ID {
			t.Fatal("retry changed operation identity")
		}
	}
}

func TestMissingAdapterRemainsRetryable(t *testing.T) {
	f := setup(t, coreadapter.RunnerBoundary)
	c := f.controller(t)
	c.options.Adapters = nil
	must(t, c.Pass(context.Background()))
	record := f.record(t)
	if record.Acknowledged || record.Result != nil || record.RetryAt.IsZero() || record.Observation.State != coreadapter.EffectUnknown {
		t.Fatal(record)
	}
	inspections, applications := f.system.counts()
	if inspections != 0 || applications != 0 {
		t.Fatal("used an unconfigured adapter")
	}
}

func TestScheduleRunsBeforeOperationsAreRead(t *testing.T) {
	ctx := context.Background()
	f := setup(t, coreadapter.RepositoryBoundary)
	c := f.controller(t)
	failure := errors.New("schedule failed")
	c.options.Schedule = func(context.Context) error { return failure }
	if err := c.Pass(ctx); !errors.Is(err, failure) {
		t.Fatalf("pass with failing schedule: %v", err)
	}
	if inspections, applies := f.system.counts(); inspections != 0 || applies != 0 {
		t.Fatalf("operations touched after schedule failure: %d inspections, %d applications", inspections, applies)
	}

	event := trace.Event{ID: trace.EventID("scheduled", "effect"), Kind: "local-effect", Body: "Scheduled local work"}
	event.Operation = &coreadapter.Operation{ID: trace.OperationID(projectID, streamID, event.ID), Boundary: coreadapter.RepositoryBoundary, Action: "prepare", Input: json.RawMessage(`{}`)}
	scheduled := 0
	c.options.Schedule = func(ctx context.Context) error {
		scheduled++
		tx := trace.Transaction{Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: 1, ID: "scheduled", Revision: 1, Project: projectID, Workstream: streamID, At: epoch, Actor: trace.Actor{Kind: "service", ID: "scheduler"}, Cause: "schedule", Depth: 2}, Subject: "scheduled", To: "pending", Reason: "Scheduled work"}, Events: []trace.Event{event}}
		_, err := f.repository.Transact(ctx, tx)
		return err
	}
	must(t, c.Pass(ctx))
	records, err := f.repository.Operations(streamID)
	must(t, err)
	if scheduled != 1 || len(records) != 2 {
		t.Fatalf("scheduled %d, records %+v", scheduled, records)
	}
	for _, record := range records {
		if !record.Acknowledged || record.Result == nil {
			t.Fatalf("operation not applied in the scheduling pass: %+v", record)
		}
	}
	f.system.mu.Lock()
	defer f.system.mu.Unlock()
	if len(f.system.applications) != 2 || !slices.Contains(f.system.applications, event.Operation.ID) {
		t.Fatalf("applications %v", f.system.applications)
	}
}
