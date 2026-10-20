package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/ghlog"
)

func operationTransaction() Transaction {
	tx := transaction("operation", 0, "", "pending", 1)
	event := &tx.Events[0]
	event.Operation = &coreadapter.Operation{ID: OperationID(projectID, streamID, event.ID), Boundary: coreadapter.RepositoryBoundary, Action: "acquire", Input: json.RawMessage(`{}`)}
	return tx
}
func TestOperationIdentityAndDeliveryFencing(t *testing.T) {
	r, _, _ := create(t)
	ctx := context.Background()
	tx := operationTransaction()
	bad := operationTransaction()
	bad.Events[0].Operation.ID = "arbitrary"
	if _, err := r.Transact(ctx, bad); err == nil {
		t.Fatal("accepted arbitrary operation ID")
	}
	transact(t, r, tx)
	transact(t, r, tx)
	event := tx.Events[0].ID
	if _, err := r.Claim(ctx, streamID, event, "token", "worker", at, time.Minute); !errors.Is(err, ErrClaim) {
		t.Fatalf("ordinary delivery claimed an operation: %v", err)
	}
	if len(ready(t, r, at)) != 0 {
		t.Fatal("operation offered as a notification")
	}
	var stale *OperationAttempt
	err := r.WithOperation(ctx, streamID, event, Actor{Kind: "service", ID: "test"}, func() time.Time { return at }, func(a *OperationAttempt, _ OperationRecord) error {
		stale = a
		for _, kind := range []string{"effect", "result", "acknowledge"} {
			action := a.Action(kind, at)
			if kind == "result" {
				action.Result = &coreadapter.OperationResult{Outcome: "done", Evidence: "unproven"}
			}
			if err := a.Record(ctx, action); err == nil {
				t.Fatalf("accepted %s before inspection", kind)
			}
		}
		action := a.Action("observe", at)
		action.Observation = &coreadapter.Observation{State: coreadapter.EffectUnknown, Evidence: "cannot locate resource"}
		if err := a.Record(ctx, action); err != nil {
			return err
		}
		action = a.Action("effect", at)
		if err := a.Record(ctx, action); err == nil {
			t.Fatal("ambiguous observation allowed effect")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := stale.Record(ctx, stale.Action("effect", at)); !errors.Is(err, ErrClaim) {
		t.Fatalf("escaped attempt remained valid: %v", err)
	}
}

func TestOperationPublicationRecovery(t *testing.T) {
	for _, kind := range []string{"claim", "observe", "effect", "result", "acknowledge", "retry"} {
		for _, boundary := range []string{boundaryBeforeCommit, boundaryCommitted} {
			t.Run(kind+"/"+boundary, func(t *testing.T) {
				r, root, project := create(t)
				s := mustScoped(t, r, streamID)
				tx := operationTransaction()
				transact(t, r, tx)
				injected := errors.New("publication interrupted")
				hit := false
				fail := func(step string) error {
					if step == boundary {
						hit = true
						return injected
					}
					return nil
				}
				if kind == "claim" {
					s.failPublication = fail
				}
				err := r.WithOperation(context.Background(), streamID, tx.Events[0].ID, Actor{Kind: "service", ID: "test"}, func() time.Time { return at }, func(a *OperationAttempt, _ OperationRecord) error {
					steps := []string{"observe", "effect", "result", "acknowledge"}
					if kind == "retry" {
						steps = []string{"observe", "retry"}
					}
					for _, step := range steps {
						if step == kind {
							s.failPublication = fail
						}
						action := a.Action(step, at)
						switch step {
						case "observe":
							action.Observation = &coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "verified absent"}
						case "result":
							action.Result = &coreadapter.OperationResult{Outcome: "complete", Evidence: "local resource"}
						case "retry":
							action.Failure, action.RetryAt = "inspection unavailable", at.Add(time.Minute)
						}
						if err := a.Record(context.Background(), action); err != nil {
							return err
						}
					}
					return nil
				})
				if !errors.Is(err, injected) || !hit {
					t.Fatalf("did not inject: %v", err)
				}
				s.failPublication = nil
				if err := r.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := Open(root, project)
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close()
				records, err := reopened.Operations(streamID)
				if err != nil {
					t.Fatal(err)
				}
				found := 0
				for _, action := range records[0].History {
					if action.Kind == kind {
						found++
					}
				}
				want := 0
				if boundary == boundaryCommitted {
					want = 1
				}
				if found != want {
					t.Fatalf("%s records=%d want=%d", kind, found, want)
				}
				entries, err := reopened.Outbox(streamID)
				if err != nil {
					t.Fatal(err)
				}
				if entries[0].Acknowledged != (kind == "acknowledge" && want == 1) {
					t.Fatal("acknowledgement not atomic")
				}
			})
		}
	}
}

// Worker attempts keep their operation claim while Unlocked and Relock allow
// independent operations of the same workstream to keep making progress.
func TestUnlockedWorkLetsOtherOperationsRun(t *testing.T) {
	r, _, _ := create(t)
	ctx := context.Background()
	tx := transaction("operations", 0, "", "pending", 3)
	for i := range tx.Events {
		e := &tx.Events[i]
		e.Operation = &coreadapter.Operation{ID: OperationID(projectID, streamID, e.ID), Boundary: coreadapter.RunnerBoundary, Action: "run", Input: json.RawMessage(`{}`)}
	}
	transact(t, r, tx)
	first, second, third := tx.Events[0].ID, tx.Events[1].ID, tx.Events[2].ID
	actor := Actor{Kind: "service", ID: "test"}
	now := func() time.Time { return at }
	claim := func(event string) (bool, error) {
		claimed := false
		err := withWorkerOperation(r, ctx, event, actor, now, func(*OperationAttempt, OperationRecord) error {
			claimed = true
			return nil
		})
		return claimed, err
	}
	claimed := make(chan struct{})
	err := withWorkerOperation(r, ctx, first, actor, now, func(a *OperationAttempt, _ OperationRecord) error {
		if err := Relock(ctx, func() error { return nil }); err != nil {
			return err
		}
		a.Unlocked(ctx, func(ctx context.Context) {
			done := make(chan struct{})
			go func() {
				defer close(done)
				if claimed, err := claim(first); err != nil || claimed {
					t.Errorf("the open attempt's operation was claimed again: %v %v", claimed, err)
				}
				if claimed, err := claim(second); err != nil || !claimed {
					t.Errorf("another operation was not claimed: %v %v", claimed, err)
				}
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("Unlocked kept the operation lock")
				return
			}
			err := Relock(ctx, func() error {
				go func() {
					defer close(claimed)
					if ok, err := claim(third); err != nil || !ok {
						t.Errorf("an operation was not claimed after the attempt: %v %v", ok, err)
					}
				}()
				return nil
			})
			if err == nil {
				err = Relock(ctx, func() error { return nil })
			}
			if err != nil {
				t.Error(err)
			}
			select {
			case <-claimed:
			case <-time.After(10 * time.Second):
				t.Error("Relock blocked an independent worker claim")
			}
		})
		action := a.Action("observe", at)
		action.Observation = &coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "nothing ran"}
		return a.Record(ctx, action)
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-claimed:
	case <-time.After(10 * time.Second):
		t.Fatal("the operation was never claimed after the attempt")
	}
	if claimed, err := claim(first); err != nil || !claimed {
		t.Fatalf("a closed attempt's operation was not claimed: %v %v", claimed, err)
	}
	records, err := r.Operations(streamID)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		var kinds []string
		for _, a := range record.History {
			kinds = append(kinds, a.Kind)
		}
		want := "[claim]"
		if record.EventID == first {
			want = "[claim observe claim]"
		}
		if fmt.Sprint(kinds) != want {
			t.Fatalf("operation %s history %v, want %s", record.EventID, kinds, want)
		}
	}
}

// Relock and Beside both leave independent claims free to proceed.
func TestBesideWorkLeavesTheLockReleased(t *testing.T) {
	r, _, _ := create(t)
	ctx := context.Background()
	tx := transaction("operations", 0, "", "pending", 3)
	for i := range tx.Events {
		e := &tx.Events[i]
		e.Operation = &coreadapter.Operation{ID: OperationID(projectID, streamID, e.ID), Boundary: coreadapter.RunnerBoundary, Action: "run", Input: json.RawMessage(`{}`)}
	}
	transact(t, r, tx)
	first, second, third := tx.Events[0].ID, tx.Events[1].ID, tx.Events[2].ID
	actor := Actor{Kind: "service", ID: "test"}
	now := func() time.Time { return at }
	claim := func(event string) <-chan bool {
		out := make(chan bool, 1)
		go func() {
			claimed := false
			err := withWorkerOperation(r, ctx, event, actor, now, func(*OperationAttempt, OperationRecord) error {
				claimed = true
				return nil
			})
			if err != nil {
				t.Error(err)
			}
			out <- claimed
		}()
		return out
	}
	err := withWorkerOperation(r, ctx, first, actor, now, func(a *OperationAttempt, _ OperationRecord) error {
		a.Unlocked(ctx, func(ctx context.Context) {
			if err := Relock(Beside(ctx), func() error { return nil }); err != nil {
				t.Error(err)
			}
			select {
			case claimed := <-claim(second):
				if !claimed {
					t.Error("another operation was not claimed")
				}
			case <-time.After(10 * time.Second):
				t.Error("Relock with a context from Beside took the lock back")
				return
			}
			if err := Relock(ctx, func() error { return nil }); err != nil {
				t.Error(err)
			}
			held := claim(third)
			select {
			case claimed := <-held:
				if !claimed {
					t.Error("independent worker was not claimed")
				}
			case <-time.After(10 * time.Second):
				t.Error("Relock blocked an independent worker claim")
			}
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func withWorkerOperation(r *Repository, ctx context.Context, event string, actor Actor, now func() time.Time, fn func(*OperationAttempt, OperationRecord) error) error {
	a, record, err := r.BeginOperation(ctx, streamID, event, WorkerOperation, actor, now)
	if err != nil || a == nil {
		return err
	}
	defer a.Close()
	return fn(a, record)
}

func TestOperationBookkeepingLogsLockWaits(t *testing.T) {
	for _, phase := range []string{"claim", "record"} {
		t.Run(phase, func(t *testing.T) {
			r, _, _ := create(t)
			tx := operationTransaction()
			transact(t, r, tx)
			var logs bytes.Buffer
			ctx := WithSlowThreshold(ghlog.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&logs, nil))), time.Millisecond)
			now := func() time.Time { return at }
			var a *OperationAttempt
			if phase == "record" {
				var err error
				a, _, err = r.BeginOperation(ctx, streamID, tx.Events[0].ID, WorkflowOperation, Actor{Kind: "service", ID: "test"}, now)
				if err != nil {
					t.Fatal(err)
				}
			}
			s := mustScoped(t, r, streamID)
			s.mu.Lock()
			entered := make(chan struct{})
			finished := make(chan error, 1)
			go func() {
				close(entered)
				if phase == "claim" {
					var err error
					a, _, err = r.BeginOperation(ctx, streamID, tx.Events[0].ID, WorkflowOperation, Actor{Kind: "service", ID: "test"}, now)
					finished <- err
				} else {
					action := a.Action("observe", at)
					action.Observation = &coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "absent"}
					finished <- a.Record(ctx, action)
				}
			}()
			<-entered
			select {
			case err := <-finished:
				s.mu.Unlock()
				t.Fatalf("bookkeeping did not wait for lock: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			s.mu.Unlock()
			if err := <-finished; err != nil {
				t.Fatal(err)
			}
			a.Close()
			found := false
			for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				var record map[string]any
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatal(err)
				}
				if record["msg"] != "lock wait exceeded threshold" {
					continue
				}
				duration, err := time.ParseDuration(record["duration"].(string))
				if err != nil || duration < time.Millisecond || record["project"] != string(projectID) || record["workstream"] != string(streamID) || record["operation"] == "" || record["lock"] != "trace bookkeeping" {
					t.Fatalf("lock wait log: %+v", record)
				}
				found = true
			}
			if !found {
				t.Fatalf("no slow lock log: %s", logs.String())
			}
		})
	}
}

func TestCloseJoinsEveryOperationClassBeforeClosingFiles(t *testing.T) {
	r, _, _ := create(t)
	tx := transaction("active", 0, "", "pending", 3)
	for i := range tx.Events {
		e := &tx.Events[i]
		e.Operation = &coreadapter.Operation{ID: OperationID(projectID, streamID, e.ID), Boundary: coreadapter.RunnerBoundary, Action: "run", Input: json.RawMessage(`{}`)}
	}
	transact(t, r, tx)
	var attempts []*OperationAttempt
	for i, class := range []OperationClass{WorkflowOperation, WorkerOperation, ConversationOperation} {
		a, _, err := r.BeginOperation(context.Background(), streamID, tx.Events[i].ID, class, Actor{Kind: "service", ID: "test"}, func() time.Time { return at })
		if err != nil || a == nil {
			t.Fatalf("admission %s: %v %v", class, a, err)
		}
		attempts = append(attempts, a)
		defer a.Close()
	}
	closed := make(chan error, 1)
	go func() { closed <- r.Close() }()
	// Each class pins the repository until its own attempt finishes.
	for _, a := range attempts {
		select {
		case err := <-closed:
			t.Fatalf("trace closed before attempt returned: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
		action := a.Action("observe", at)
		action.Observation = &coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "effect not applied"}
		if err := a.Record(context.Background(), action); err != nil {
			t.Fatalf("record before close joined: %v", err)
		}
		a.Close()
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("close did not join attempts")
	}
	for _, a := range attempts {
		if err := a.Record(context.Background(), a.Action("effect", at)); !errors.Is(err, ErrClaim) {
			t.Fatalf("write on closed trace: %v", err)
		}
	}
}
