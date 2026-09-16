package thread

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// TurnAction is the runner-boundary operation action that delivers one queued turn.
const TurnAction = "thread-turn"

// TurnInput names an accepted request. The durable queue supplies its content.
type TurnInput struct {
	Workstream config.WorkstreamID `json:"workstream"`
	Agent      string              `json:"agent"`
	Turn       string              `json:"turn"`
}

// TurnOperation builds the outbox intent that asks the reconciler to deliver an
// accepted request. Publish it with the transition that authorizes the turn.
func TurnOperation(project config.ProjectID, event string, input TurnInput) (coreadapter.Operation, error) {
	data, err := json.Marshal(input)
	if err != nil {
		return coreadapter.Operation{}, err
	}
	return coreadapter.Operation{ID: trace.OperationID(project, input.Workstream, event), Boundary: coreadapter.RunnerBoundary, Action: TurnAction, Input: data}, nil
}

// TurnResult is the structured data of a terminal turn operation.
type TurnResult struct {
	Sequence   uint64 `json:"sequence"`
	RequestID  string `json:"request_id"`
	ResponseID string `json:"response_id"`
	Attempts   int    `json:"attempts"`
}

// Dispatcher reconciles turn operations against the durable thread queue, so a
// restarted controller finds unfinished turns by reading the store. Prepare
// supplies per-turn execution resources and outcome policy for Runner.RunNext.
type Dispatcher struct {
	Runner  Runner
	Prepare func(context.Context, TurnInput) (coreadapter.PreparedTurn, error)
}

var _ coreadapter.Reconciler = Dispatcher{}

// DecodeTurn returns the input of a turn operation, refusing other operations,
// unknown fields, invalid workstream IDs and missing agent or turn IDs.
func DecodeTurn(op coreadapter.Operation) (TurnInput, error) {
	var in TurnInput
	if op.Boundary != coreadapter.RunnerBoundary || op.Action != TurnAction {
		return in, fmt.Errorf("unsupported runner operation %q", op.Action)
	}
	d := json.NewDecoder(bytes.NewReader(op.Input))
	d.DisallowUnknownFields()
	if err := d.Decode(&in); err != nil {
		return in, fmt.Errorf("invalid turn operation input: %w", err)
	}
	if err := config.CheckWorkstreamIDs(in.Workstream); err != nil {
		return in, err
	}
	if in.Agent == "" || in.Turn == "" {
		return in, errors.New("turn operation requires agent and turn")
	}
	return in, nil
}

// find returns the requested turn and whether every earlier turn has completed.
func (d Dispatcher) find(in TurnInput) (trace.Thread, trace.QueuedTurn, bool, error) {
	if d.Runner.Store == nil {
		return trace.Thread{}, trace.QueuedTurn{}, false, errors.New("dispatcher requires a thread runner store")
	}
	t, err := d.Runner.Store.Thread(in.Workstream, in.Agent)
	if err != nil {
		return t, trace.QueuedTurn{}, false, err
	}
	ready := true
	for _, q := range t.Turns {
		if q.Request.TurnID == in.Turn {
			return t, q, ready, nil
		}
		ready = ready && !q.CompletedAt.IsZero()
	}
	return t, trace.QueuedTurn{}, false, fmt.Errorf("turn %q was not accepted", in.Turn)
}

func turnResult(q trace.QueuedTurn) coreadapter.OperationResult {
	data, _ := json.Marshal(TurnResult{Sequence: q.Sequence, RequestID: q.Request.ID, ResponseID: q.Response.ID, Attempts: len(q.Attempts)})
	return coreadapter.OperationResult{Outcome: q.Status(), Evidence: "owned response " + q.Response.ID, Data: data}
}

// Inspect reads only the durable queue. A reservation without a captured result
// may still be running or was interrupted, so it is never reported absent.
func (d Dispatcher) Inspect(_ context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	in, err := DecodeTurn(op)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	_, q, ready, err := d.find(in)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	switch {
	case !q.CompletedAt.IsZero():
		result := turnResult(q)
		return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: "turn completed", Result: &result}, nil
	case q.Response != nil:
		return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "captured result awaits completion"}, nil
	case q.Claim != nil:
		return coreadapter.Observation{State: coreadapter.EffectUnknown, Evidence: "turn is reserved without a captured result"}, nil
	case !ready:
		return coreadapter.Observation{State: coreadapter.EffectUnknown, Evidence: "an earlier turn has not completed"}, nil
	}
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "turn is queued and unclaimed"}, nil
}

// Apply completes a captured turn without a backend call, or runs the queued
// turn. A captured execution failure is a terminal result, not a retry.
func (d Dispatcher) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	in, err := DecodeTurn(op)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	t, q, ready, err := d.find(in)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	r := d.Runner
	if r.Now == nil {
		return coreadapter.OperationResult{}, errors.New("thread runner requires a clock")
	}
	var runErr error
	switch {
	case !q.CompletedAt.IsZero():
		return turnResult(q), nil
	case q.Response != nil:
		cleanup := context.WithoutCancel(ctx)
		if err := r.costs(cleanup, t.Identity.Role, q); err != nil {
			return coreadapter.OperationResult{}, err
		}
		if err := r.Store.CompleteTurn(cleanup, in.Workstream, in.Agent, in.Turn, q.Claim.Token, r.Now()); err != nil {
			return coreadapter.OperationResult{}, err
		}
	case q.Claim != nil || !ready:
		return coreadapter.OperationResult{}, errors.New("turn is not eligible to run")
	default:
		if d.Prepare == nil {
			return coreadapter.OperationResult{}, errors.New("dispatcher requires turn preparation")
		}
		prepared, err := d.Prepare(ctx, in)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		_, runErr = r.RunNext(ctx, in.Workstream, in.Agent, prepared)
	}
	_, q, _, err = d.find(in)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if q.CompletedAt.IsZero() {
		return coreadapter.OperationResult{}, errors.Join(runErr, errors.New("turn did not complete"))
	}
	return turnResult(q), nil
}
