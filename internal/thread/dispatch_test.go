package thread

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

func turnOp(t *testing.T, turn string) coreadapter.Operation {
	t.Helper()
	op, err := TurnOperation(project, "event_"+turn, TurnInput{Workstream: stream, Agent: "agent", Turn: turn})
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func observe(t *testing.T, d Dispatcher, op coreadapter.Operation, want coreadapter.ObservationState) coreadapter.Observation {
	t.Helper()
	o, err := d.Inspect(context.Background(), op)
	if err != nil || o.State != want {
		t.Fatalf("inspection %+v %v, want %s", o, err, want)
	}
	return o
}

func TestDispatcherFollowsDurableQueue(t *testing.T) {
	ctx := context.Background()
	repo, _, _ := setup(t)
	queue(t, repo, "first")
	queue(t, repo, "second")
	calls := 0
	d := Dispatcher{
		Runner: Runner{Store: repo, Now: func() time.Time { return timestamp.Add(time.Second) }, Turns: fakeTurns(func(_ context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
			calls++
			if p.SessionDirectory != "/owned/"+p.Scope.Turn {
				t.Errorf("preparation not applied: %+v", p)
			}
			if p.Scope.Turn == "second" {
				return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "s"}, Usage: coreadapter.Usage{CostUSD: 1, CostKnown: true}}, errors.New("backend failed")
			}
			return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "s"}, FinalResponse: "done"}, nil
		})},
		Prepare: func(_ context.Context, in TurnInput) (coreadapter.PreparedTurn, error) {
			return coreadapter.PreparedTurn{SessionDirectory: "/owned/" + in.Turn}, nil
		},
	}
	first, second := turnOp(t, "first"), turnOp(t, "second")
	// A runner that fails before claiming leaves the intent pending.
	broken := d
	broken.Runner.MaxRetries = -1
	if _, err := broken.Apply(ctx, first); err == nil || calls != 0 {
		t.Fatalf("unclaimed turn reported: %v", err)
	}
	observe(t, d, second, coreadapter.EffectUnknown)
	if _, err := d.Apply(ctx, second); err == nil || calls != 0 {
		t.Fatalf("successor ran before its predecessor: %v", err)
	}
	observe(t, d, first, coreadapter.EffectAbsent)
	result, err := d.Apply(ctx, first)
	if err != nil || result.Outcome != "idle" || calls != 1 {
		t.Fatalf("first turn: %+v %v", result, err)
	}
	var data TurnResult
	if err := json.Unmarshal(result.Data, &data); err != nil || data.Sequence != 1 || data.RequestID != "request_first" || data.ResponseID != trace.EventID("request_first", "response") || data.Attempts != 1 {
		t.Fatalf("result data: %s %v", result.Data, err)
	}
	o := observe(t, d, first, coreadapter.EffectCompleted)
	if o.Result == nil || string(o.Result.Data) != string(result.Data) || o.Result.Outcome != result.Outcome {
		t.Fatalf("inspection result differs: %+v", o.Result)
	}
	if again, err := d.Apply(ctx, first); err != nil || calls != 1 || string(again.Data) != string(result.Data) {
		t.Fatalf("completed turn ran again: %v", err)
	}
	// A captured backend failure is a terminal domain result, not a retry.
	observe(t, d, second, coreadapter.EffectAbsent)
	result, err = d.Apply(ctx, second)
	if err != nil || result.Outcome != "failed" || calls != 2 {
		t.Fatalf("failed turn: %+v %v", result, err)
	}
	costs, err := trace.Read[trace.Cost](repo, stream)
	if err != nil || len(costs) != 2 || costs[0].Entry.AttemptID != AttemptID("request_first", 1) || costs[1].Entry.Usage.CostUSD != 1 || costs[1].Entry.Scope.Role != "mason" || costs[1].Cause != "owner-message" {
		t.Fatalf("costs: %+v %v", costs, err)
	}
}

func TestDispatcherRejectsInvalidOperations(t *testing.T) {
	repo, _, _ := setup(t)
	queue(t, repo, "first")
	d := Dispatcher{Runner: Runner{Store: repo, Now: time.Now, Turns: fakeTurns(func(context.Context, coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		t.Fatal("invalid operation ran")
		return coreadapter.SessionResult{}, nil
	})}, Prepare: func(context.Context, TurnInput) (coreadapter.PreparedTurn, error) {
		return coreadapter.PreparedTurn{}, nil
	}}
	valid := turnOp(t, "first")
	for name, mutate := range map[string]func(*coreadapter.Operation){
		"action":   func(op *coreadapter.Operation) { op.Action = "run" },
		"boundary": func(op *coreadapter.Operation) { op.Boundary = coreadapter.ContainerBoundary },
		"unknown": func(op *coreadapter.Operation) {
			op.Input = json.RawMessage(`{"workstream":"` + string(stream) + `","agent":"agent","turn":"first","prompt":"x"}`)
		},
		"workstream": func(op *coreadapter.Operation) {
			op.Input = json.RawMessage(`{"workstream":"../w","agent":"agent","turn":"first"}`)
		},
		"missing turn": func(op *coreadapter.Operation) { *op = turnOp(t, "absent") },
	} {
		op := valid
		mutate(&op)
		if _, err := d.Inspect(context.Background(), op); err == nil {
			t.Errorf("%s: inspection accepted", name)
		}
		if _, err := d.Apply(context.Background(), op); err == nil {
			t.Errorf("%s: apply accepted", name)
		}
	}
}

func TestDispatcherRecoversCapturedAndReservedTurns(t *testing.T) {
	ctx := context.Background()
	repo, root, p := setup(t)
	req := queue(t, repo, "first")
	queue(t, repo, "second")
	at := timestamp.Add(time.Second)
	runner := Runner{Store: repo, Now: func() time.Time { return at }}
	q, err := repo.ClaimTurn(ctx, stream, "agent", "token", "/owned", at)
	if err != nil {
		t.Fatal(err)
	}
	result := coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "s"}, SessionDirectory: "/owned", StartedAt: at, FinalResponse: "done", Usage: coreadapter.Usage{CostUSD: 2, CostKnown: true}}
	a := trace.TurnAttempt{Number: 1, Profile: req.Profile, Path: "replay", Reason: "fresh", At: at}
	if err := repo.RecordAttempt(ctx, stream, "agent", "first", "token", a); err != nil {
		t.Fatal(err)
	}
	a.Result = &result
	if err := repo.RecordAttempt(ctx, stream, "agent", "first", "token", a); err != nil {
		t.Fatal(err)
	}
	h := req.Header
	h.Schema, h.ID, h.At, h.Actor = "osmia.trace.turn-response", trace.EventID(req.ID, "response"), at, trace.Actor{Kind: "service", ID: "thread-runner"}
	if err := repo.CaptureTurn(ctx, "token", trace.TurnResponse{Header: h, AgentID: "agent", ThreadID: req.ThreadID, TurnID: "first", RequestID: req.ID, RequestRevision: 1, Result: result}); err != nil {
		t.Fatal(err)
	}
	// The service stopped after recording cost but before completion.
	q.Attempts = []trace.TurnAttempt{a}
	if err := runner.costs(ctx, "mason", q); err != nil {
		t.Fatal(err)
	}
	repo.Close()
	repo, err = trace.Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	d := Dispatcher{Runner: Runner{Store: repo, Now: func() time.Time { return at }, Turns: fakeTurns(func(context.Context, coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		t.Fatal("captured turn relaunched")
		return coreadapter.SessionResult{}, nil
	})}}
	observe(t, d, turnOp(t, "first"), coreadapter.EffectAbsent)
	observe(t, d, turnOp(t, "second"), coreadapter.EffectUnknown)
	done, err := d.Apply(ctx, turnOp(t, "first"))
	if err != nil || done.Outcome != "idle" {
		t.Fatalf("captured completion: %+v %v", done, err)
	}
	costs, err := trace.Read[trace.Cost](repo, stream)
	if err != nil || len(costs) != 1 || costs[0].Entry.Usage.CostUSD != 2 {
		t.Fatalf("duplicate or missing cost: %+v %v", costs, err)
	}

	// A reservation without a captured result may still be running.
	if _, err := repo.ClaimTurn(ctx, stream, "agent", "token2", "/owned2", at); err != nil {
		t.Fatal(err)
	}
	a = trace.TurnAttempt{Number: 1, Profile: req.Profile, Path: "replay", Reason: "fresh", At: at}
	if err := repo.RecordAttempt(ctx, stream, "agent", "second", "token2", a); err != nil {
		t.Fatal(err)
	}
	observe(t, d, turnOp(t, "second"), coreadapter.EffectUnknown)
	if _, err := d.Apply(ctx, turnOp(t, "second")); err == nil {
		t.Fatal("reserved turn relaunched")
	}
}
