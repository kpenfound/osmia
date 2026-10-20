package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

const otherStream = config.WorkstreamID("w_fedcba9876543210fedcba9876543210")
const thirdStream = config.WorkstreamID("w_00000000000000000000000000000003")

// controlledOperations can hold either boundary call independently. Channels
// expose entry without relying on goroutine scheduling or elapsed time.
type controlledOperations struct {
	entered     map[string]chan struct{}
	inspect     map[string]<-chan struct{}
	apply       map[string]<-chan struct{}
	result      map[string]error
	beforeApply func(context.Context, coreadapter.Operation) error
}

func waitGate(ctx context.Context, gate <-chan struct{}) error {
	if gate == nil {
		return nil
	}
	select {
	case <-gate:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (a *controlledOperations) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	if err := waitGate(ctx, a.inspect[op.Action]); err != nil {
		return coreadapter.Observation{}, err
	}
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "no effect"}, nil
}
func (a *controlledOperations) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	if entered := a.entered[op.Action]; entered != nil {
		close(entered)
	}
	if err := waitGate(ctx, a.apply[op.Action]); err != nil {
		return coreadapter.OperationResult{}, err
	}
	if a.beforeApply != nil {
		if err := a.beforeApply(ctx, op); err != nil {
			return coreadapter.OperationResult{}, err
		}
	}
	return coreadapter.OperationResult{Outcome: "done", Evidence: "effect settled"}, a.result[op.Action]
}

func (f *fixture) intent(t *testing.T, stream config.WorkstreamID, actions ...string) {
	t.Helper()
	if stream != streamID {
		must(t, f.repository.CreateWorkstream(context.Background(), stream, epoch, trace.Actor{Kind: "owner", ID: "local"}))
	}
	id := "intent-" + actions[0]
	var events []trace.Event
	for _, action := range actions {
		event := trace.Event{ID: trace.EventID(id, action), Kind: "local-effect", Body: action}
		event.Operation = &coreadapter.Operation{ID: trace.OperationID(projectID, stream, event.ID), Boundary: coreadapter.RunnerBoundary, Action: action, Input: json.RawMessage(`{}`)}
		events = append(events, event)
	}
	_, err := f.repository.Transact(context.Background(), trace.Transaction{Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: 1, ID: id, Revision: 1, Project: projectID, Workstream: stream, At: epoch, Actor: trace.Actor{Kind: "service", ID: "scheduler"}, Cause: "owner-request"}, Subject: id, To: "pending", Reason: "authorized work"}, Events: events})
	must(t, err)
}

func (f *fixture) orderedController(t *testing.T, a *controlledOperations, order ...string) *Controller {
	t.Helper()
	priority := map[string]int{}
	for i, action := range order {
		priority[action] = i
	}
	c, err := New(f.repository, Options{Worker: "test-controller", Now: f.clock.Now,
		Adapters:   map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RunnerBoundary: a},
		Hold:       func(_ config.WorkstreamID, op coreadapter.Operation) bool { return op.Action == "prepare" },
		Concurrent: func(coreadapter.Operation) bool { return true },
		Class:      func(coreadapter.Operation) trace.OperationClass { return trace.WorkflowOperation },
		Priority:   func(op coreadapter.Operation) int { return priority[op.Action] },
	})
	must(t, err)
	return c
}

func await(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(overlapTimeout):
		t.Fatal("operation did not reach boundary")
	}
}
func passPromptly(t *testing.T, c *Controller) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- c.Pass(context.Background()) }()
	select {
	case err := <-done:
		must(t, err)
	case <-time.After(overlapTimeout):
		t.Fatal("pass waited for execution capacity or an adapter")
	}
}
func recordFor(t *testing.T, f *fixture, stream config.WorkstreamID, action string) trace.OperationRecord {
	t.Helper()
	records, err := f.repository.Operations(stream)
	must(t, err)
	for _, record := range records {
		if record.Operation.Action == action {
			return record
		}
	}
	t.Fatalf("no operation %s", action)
	return trace.OperationRecord{}
}
func awaitAcknowledged(t *testing.T, f *fixture, stream config.WorkstreamID, action string) {
	t.Helper()
	deadline := time.After(overlapTimeout)
	for {
		record := recordFor(t, f, stream, action)
		if record.Acknowledged && record.Result != nil {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("%s not acknowledged: %+v", action, record)
		case <-time.After(time.Millisecond):
		}
	}
}

func TestWorkflowAdmissionSkipsBusyWorkstream(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(fmt.Sprint("retry=", retry), func(t *testing.T) {
			f := setup(t, coreadapter.RepositoryBoundary)
			f.intent(t, streamID, "a1", "a2")
			f.intent(t, otherStream, "b")
			held := make(chan struct{})
			release := sync.OnceFunc(func() { close(held) })
			defer release()
			a := &controlledOperations{entered: map[string]chan struct{}{"a1": make(chan struct{})}, apply: map[string]<-chan struct{}{"a1": held}, result: map[string]error{}}
			if retry {
				a.result["a1"] = errInjected
			}
			a.beforeApply = func(_ context.Context, op coreadapter.Operation) error {
				if op.Action == "a2" {
					records, err := f.repository.Operations(streamID)
					if err != nil {
						return err
					}
					for _, r := range records {
						if r.Operation.Action == "a1" && r.Result == nil && r.RetryAt.IsZero() {
							return fmt.Errorf("a2 overlapped a1 before result or retry")
						}
					}
				}
				return nil
			}
			c := f.orderedController(t, a, "a1", "a2", "b")
			permits := make(chan struct{}, 1)
			c.options.Limit = func(op coreadapter.Operation) (func(), bool) {
				if op.Action == "a1" {
					return nil, true
				}
				select {
				case permits <- struct{}{}:
					return func() { <-permits }, true
				default:
					return nil, false
				}
			}
			passPromptly(t, c)
			await(t, a.entered["a1"])
			awaitAcknowledged(t, f, otherStream, "b")
			for range 3 {
				passPromptly(t, c)
			}
			if r := recordFor(t, f, streamID, "a2"); len(r.History) != 0 {
				t.Fatalf("busy workflow was claimed: %+v", r)
			}
			release()
			must(t, c.Wait())
			passPromptly(t, c)
			must(t, c.Wait())
			awaitAcknowledged(t, f, streamID, "a2")
			if r := recordFor(t, f, streamID, "a1"); retry && r.RetryAt.IsZero() {
				t.Fatal("failure did not record retry")
			}
		})
	}
}

func TestWorkerAndConversationAdmissionBypassesWorkflow(t *testing.T) {
	f := setup(t, coreadapter.RepositoryBoundary)
	f.intent(t, streamID, "workflow", "worker1", "worker2", turnAction)
	held, workers := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(held); close(workers) })
	defer release()
	a := &controlledOperations{entered: map[string]chan struct{}{"workflow": make(chan struct{}), "worker1": make(chan struct{}), "worker2": make(chan struct{})}, apply: map[string]<-chan struct{}{"workflow": held, "worker1": workers, "worker2": workers}}
	c := f.orderedController(t, a, "workflow", "worker1", "worker2", turnAction)
	c.options.Class = func(op coreadapter.Operation) trace.OperationClass {
		switch op.Action {
		case "workflow":
			return trace.WorkflowOperation
		case turnAction:
			return trace.ConversationOperation
		default:
			return trace.WorkerOperation
		}
	}
	passPromptly(t, c)
	for _, action := range []string{"workflow", "worker1", "worker2"} {
		await(t, a.entered[action])
	}
	awaitAcknowledged(t, f, streamID, turnAction)
	for _, action := range []string{"workflow", "worker1", "worker2"} {
		if r := recordFor(t, f, streamID, action); r.Claim == nil || r.Result != nil {
			t.Fatalf("operation not in flight: %+v", r)
		}
	}
	release()
	must(t, c.Wait())
	for _, action := range []string{"workflow", "worker1", "worker2"} {
		awaitAcknowledged(t, f, streamID, action)
	}
}

func TestKindLimitLeavesPassAndOtherKindsFree(t *testing.T) {
	f := setup(t, coreadapter.RepositoryBoundary)
	f.intent(t, streamID, "git1")
	f.intent(t, otherStream, "git2", "later")
	f.intent(t, thirdStream, "model")
	held := make(chan struct{})
	release := sync.OnceFunc(func() { close(held) })
	defer release()
	a := &controlledOperations{entered: map[string]chan struct{}{"git1": make(chan struct{})}, apply: map[string]<-chan struct{}{"git1": held}}
	c := f.orderedController(t, a, "git1", "git2", "later", "model")
	permits := make(chan struct{}, 1)
	c.options.Limit = func(op coreadapter.Operation) (func(), bool) {
		if op.Action != "git1" && op.Action != "git2" {
			return nil, true
		}
		select {
		case permits <- struct{}{}:
			return func() { <-permits }, true
		default:
			return nil, false
		}
	}
	passPromptly(t, c)
	await(t, a.entered["git1"])
	awaitAcknowledged(t, f, thirdStream, "model")
	for range 3 {
		passPromptly(t, c)
	}
	for _, action := range []string{"git2", "later"} {
		if r := recordFor(t, f, otherStream, action); len(r.History) != 0 {
			t.Fatalf("pending workflow overtaken or claimed: %+v", r)
		}
	}
	// No tick: releasing the permit wakes Run to finish the pending workflows.
	c.options.Ticks = make(chan time.Time)
	scheduled := make(chan struct{}, 1)
	c.options.Schedule = func(context.Context) error {
		select {
		case scheduled <- struct{}{}:
		default:
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	await(t, scheduled)
	release()
	awaitAcknowledged(t, f, otherStream, "git2")
	awaitAcknowledged(t, f, otherStream, "later")
	c.Drain()
	must(t, <-done)
	if len(permits) != 0 {
		t.Fatal("kind permit leaked")
	}
}

func TestResultRecordingBypassesScheduleAndOtherWorkstream(t *testing.T) {
	f := setup(t, coreadapter.RepositoryBoundary)
	f.intent(t, streamID, "long")
	f.intent(t, otherStream, "record")
	held, record := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(held) })
	releaseRecord := sync.OnceFunc(func() { close(record) })
	defer release()
	defer releaseRecord()
	a := &controlledOperations{entered: map[string]chan struct{}{"long": make(chan struct{}), "record": make(chan struct{})}, apply: map[string]<-chan struct{}{"long": held, "record": record}}
	relocked := make(chan struct{})
	a.beforeApply = func(ctx context.Context, op coreadapter.Operation) error {
		if op.Action == "record" {
			return trace.Relock(ctx, func() error { close(relocked); return nil })
		}
		return nil
	}
	c := f.orderedController(t, a, "long", "record")
	passPromptly(t, c)
	await(t, a.entered["long"])
	await(t, a.entered["record"])
	scheduleEntered, scheduleRelease := make(chan struct{}), make(chan struct{})
	finishSchedule := sync.OnceFunc(func() { close(scheduleRelease) })
	defer finishSchedule()
	c.options.Schedule = func(context.Context) error { close(scheduleEntered); <-scheduleRelease; return nil }
	passed := make(chan error, 1)
	go func() { passed <- c.Pass(context.Background()) }()
	await(t, scheduleEntered)
	releaseRecord()
	await(t, relocked)
	awaitAcknowledged(t, f, otherStream, "record")
	if r := recordFor(t, f, streamID, "long"); r.Result != nil {
		t.Fatal("long operation unexpectedly finished")
	}
	finishSchedule()
	must(t, <-passed)
	release()
	must(t, c.Wait())
}

func TestResultRecordingBypassesOperationDiscovery(t *testing.T) {
	f := setup(t, coreadapter.RepositoryBoundary)
	f.intent(t, streamID, "record")
	recordRelease := make(chan struct{})
	finishRecord := sync.OnceFunc(func() { close(recordRelease) })
	defer finishRecord()
	a := &controlledOperations{entered: map[string]chan struct{}{"record": make(chan struct{})}, apply: map[string]<-chan struct{}{"record": recordRelease}}
	c := f.orderedController(t, a, "record")
	passPromptly(t, c)
	await(t, a.entered["record"])
	discovering, discoveryRelease := make(chan struct{}), make(chan struct{})
	finishDiscovery := sync.OnceFunc(func() { close(discoveryRelease) })
	defer finishDiscovery()
	c.options.Hold = func(_ config.WorkstreamID, op coreadapter.Operation) bool {
		if op.Action == "prepare" {
			close(discovering)
			<-discoveryRelease
		}
		return true
	}
	passed := make(chan error, 1)
	go func() { passed <- c.Pass(context.Background()) }()
	await(t, discovering)
	finishRecord()
	awaitAcknowledged(t, f, streamID, "record")
	finishDiscovery()
	must(t, <-passed)
	must(t, c.Wait())
}

func TestBlockedInspectionDoesNotBlockAdmission(t *testing.T) {
	f := setup(t, coreadapter.RepositoryBoundary)
	f.intent(t, streamID, "inspect")
	f.intent(t, otherStream, "other")
	held := make(chan struct{})
	release := sync.OnceFunc(func() { close(held) })
	defer release()
	a := &controlledOperations{inspect: map[string]<-chan struct{}{"inspect": held}}
	c := f.orderedController(t, a, "inspect", "other")
	passPromptly(t, c)
	awaitAcknowledged(t, f, otherStream, "other")
	release()
	must(t, c.Wait())
}

func TestDrainJoinsWorkflowsAndLeavesPendingWorkUnclaimed(t *testing.T) {
	f := setup(t, coreadapter.RepositoryBoundary)
	f.intent(t, streamID, "a1", "a2")
	f.intent(t, otherStream, "b")
	heldA, heldB := make(chan struct{}), make(chan struct{})
	releaseA := sync.OnceFunc(func() { close(heldA) })
	releaseB := sync.OnceFunc(func() { close(heldB) })
	defer releaseA()
	defer releaseB()
	a := &controlledOperations{entered: map[string]chan struct{}{"a1": make(chan struct{}), "b": make(chan struct{})}, apply: map[string]<-chan struct{}{"a1": heldA, "b": heldB}}
	c := f.orderedController(t, a, "a1", "a2", "b")
	c.options.Ticks = make(chan time.Time)
	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background()) }()
	await(t, a.entered["a1"])
	await(t, a.entered["b"])
	c.Drain()
	releaseA()
	awaitAcknowledged(t, f, streamID, "a1")
	select {
	case err := <-done:
		t.Fatalf("drain returned while b was running: %v", err)
	default:
	}
	releaseB()
	select {
	case err := <-done:
		must(t, err)
	case <-time.After(overlapTimeout):
		t.Fatal("drain did not join workflows")
	}
	if r := recordFor(t, f, streamID, "a2"); len(r.History) != 0 {
		t.Fatalf("drain admitted pending workflow: %+v", r)
	}
	must(t, f.repository.Close())
}
