package trace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

var ErrClaimed = errors.New("outbox entry has an active claim")
var ErrClaim = errors.New("outbox claim is stale or invalid")

// A transaction is identified by its transition's ID, stable across retries. ExpectedVersion
// is scoped to the transition's workstream and subject; zero means absent state.
// The complete request, including provenance and event order, is immutable.
type Transaction struct {
	ExpectedVersion uint64     `json:"expected_version"`
	Transition      Transition `json:"transition"`
	Events          []Event    `json:"events,omitempty"`
}

type Event struct {
	ID        string                 `json:"id"`
	Kind      string                 `json:"kind"`
	Body      string                 `json:"body"`
	Operation *coreadapter.Operation `json:"operation,omitempty"`
}

// EventID derives a stable identity from a logical transaction and event key.
// Identities are scoped to one workstream; callers retain the same keys on retry.
func EventID(transaction, event string) string {
	data, _ := json.Marshal([]string{transaction, event})
	return fmt.Sprintf("event_%x", sha256.Sum256(data))
}

type WorkflowState struct {
	Version uint64 `json:"version"`
	Value   string `json:"value"`
}

type DeliveryAction struct {
	EventID string    `json:"event_id"`
	Kind    string    `json:"kind"`
	Token   string    `json:"token"`
	Worker  string    `json:"worker,omitempty"`
	Session string    `json:"session,omitempty"`
	At      time.Time `json:"at"`
	Until   time.Time `json:"until,omitempty"`
}

// OutboxEntry is an event's delivery state. At is its transition's timestamp.
type OutboxEntry struct {
	Event        Event            `json:"event"`
	TransitionID string           `json:"transition_id"`
	At           time.Time        `json:"at"`
	Claim        *DeliveryAction  `json:"claim,omitempty"`
	Acknowledged bool             `json:"acknowledged"`
	History      []DeliveryAction `json:"history,omitempty"`
}

type workflowLog struct {
	Schema       string            `json:"schema"`
	Version      int               `json:"version"`
	Transactions []Transaction     `json:"transactions"`
	Deliveries   []DeliveryAction  `json:"deliveries"`
	Operations   []OperationAction `json:"operations,omitempty"`
	Threads      map[string]Thread `json:"threads,omitempty"`
}

type workflowView struct {
	states       map[string]WorkflowState
	transactions map[string]Transaction
	entries      map[string]*OutboxEntry
	operations   map[string]*OperationRecord
}

func equalJSON(a, b any) bool {
	x, ex := json.Marshal(a)
	y, ey := json.Marshal(b)
	return ex == nil && ey == nil && bytes.Equal(x, y)
}

func (v *workflowView) transition(tx Transaction, project config.ProjectID, stream config.WorkstreamID) (WorkflowState, error) {
	t := tx.Transition
	if err := validate(t); err != nil {
		return WorkflowState{}, err
	}
	if t.Project != project || t.Workstream != stream || t.Revision != 1 {
		return WorkflowState{}, fmt.Errorf("invalid transaction scope or transition revision")
	}
	if _, ok := v.transactions[t.ID]; ok {
		return WorkflowState{}, ErrConflict
	}
	state := v.states[t.Subject]
	if state.Version != tx.ExpectedVersion || state.Value != t.From || state.Version == math.MaxUint64 {
		return WorkflowState{}, ErrConflict
	}
	seen := map[string]bool{}
	for _, e := range tx.Events {
		if !key(e.ID) || !key(e.Kind) || !present(e.Body) {
			return WorkflowState{}, fmt.Errorf("invalid outbox event")
		}
		if seen[e.ID] || v.entries[e.ID] != nil {
			return WorkflowState{}, ErrConflict
		}
		if e.Operation != nil {
			if err := validateOperation(*e.Operation, project, stream, e.ID); err != nil {
				return WorkflowState{}, err
			}
		}
		seen[e.ID] = true
	}
	state = WorkflowState{Version: state.Version + 1, Value: t.To}
	v.states[t.Subject] = state
	v.transactions[t.ID] = tx
	for _, e := range tx.Events {
		v.entries[e.ID] = &OutboxEntry{Event: e, TransitionID: t.ID, At: t.At}
		if e.Operation != nil {
			v.operations[e.ID] = &OperationRecord{Operation: *e.Operation, EventID: e.ID, Transition: t.Header}
		}
	}
	return state, nil
}

func (v *workflowView) delivery(a DeliveryAction) error {
	e := v.entries[a.EventID]
	if e == nil || e.Event.Operation != nil || !key(a.Token) || a.At.IsZero() {
		return ErrClaim
	}
	switch a.Kind {
	case "claim":
		if !key(a.Worker) || !key(a.Session) || !a.Until.After(a.At) || e.Acknowledged {
			return ErrClaim
		}
		for _, old := range e.History {
			if old.Kind == "claim" && old.Token == a.Token {
				return ErrConflict
			}
		}
		if e.Claim != nil && e.Claim.Session == a.Session && e.Claim.Until.After(a.At) {
			return ErrClaimed
		}
		copy := a
		e.Claim = &copy
	case "acknowledge", "release":
		if a.Worker != "" || a.Session != "" || !a.Until.IsZero() || e.Acknowledged || e.Claim == nil || e.Claim.Token != a.Token || a.At.Before(e.Claim.At) || !a.At.Before(e.Claim.Until) {
			return ErrClaim
		}
		e.Acknowledged = a.Kind == "acknowledge"
		e.Claim = nil
	default:
		return fmt.Errorf("invalid delivery action %q", a.Kind)
	}
	e.History = append(e.History, a)
	return nil
}

func (r *Repository) loadWorkflow(stream config.WorkstreamID) (workflowLog, *workflowView, error) {
	var log workflowLog
	if err := config.CheckWorkstreamIDs(stream); err != nil {
		return log, nil, err
	}
	if err := r.checkHistory(context.Background()); err != nil {
		return log, nil, err
	}
	records, _, err := r.scan()
	if err != nil {
		return log, nil, err
	}
	prefix := "workstreams/" + string(stream) + "/"
	if err := r.manifest(prefix+"workstream.json", "osmia.trace.workstream", stream); err != nil {
		return log, nil, err
	}
	data, err := r.readFile(prefix + "workflow.json")
	if os.IsNotExist(err) {
		log = workflowLog{Schema: "osmia.workflow", Version: 1}
	} else if err != nil {
		return log, nil, err
	} else if err = decode(data, &log); err != nil {
		return log, nil, err
	}
	if log.Schema != "osmia.workflow" || log.Version != 1 {
		return log, nil, fmt.Errorf("unsupported workflow schema/version")
	}
	v := &workflowView{states: map[string]WorkflowState{}, transactions: map[string]Transaction{}, entries: map[string]*OutboxEntry{}, operations: map[string]*OperationRecord{}}
	for _, tx := range log.Transactions {
		if _, err := v.transition(tx, r.project, stream); err != nil {
			return log, nil, fmt.Errorf("workflow transaction %s: %w", tx.Transition.ID, err)
		}
	}
	for _, a := range log.Deliveries {
		if err := v.delivery(a); err != nil {
			return log, nil, fmt.Errorf("workflow delivery %s: %w", a.EventID, err)
		}
	}
	for _, a := range log.Operations {
		if err := v.operation(a); err != nil {
			return log, nil, fmt.Errorf("workflow operation %s: %w", a.EventID, err)
		}
	}
	// Every managed transition has one identical record in the inspectable trace.
	found := map[string]bool{}
	for _, record := range records {
		t, ok := record.(Transition)
		if !ok || t.Workstream != stream {
			continue
		}
		if tx, ok := v.transactions[t.ID]; ok {
			if found[t.ID] || !equalJSON(tx.Transition, t) {
				return log, nil, ErrConflict
			}
			found[t.ID] = true
		}
	}
	if len(found) != len(v.transactions) {
		return log, nil, fmt.Errorf("workflow transition missing from trace")
	}
	if err := validateThreads(log.Threads, records, r.project, stream); err != nil {
		return log, nil, err
	}
	return log, v, nil
}

func (r *Repository) checkWorkflows(streams []config.WorkstreamID) error {
	for _, stream := range streams {
		if _, _, err := r.loadWorkflow(stream); err != nil {
			return fmt.Errorf("workstream %s: %w", stream, err)
		}
	}
	return nil
}

// Transact atomically publishes state, transition provenance and delivery intent.
// An error after publication may still mean committed: retry the identical request.
func (r *Repository) Transact(ctx context.Context, tx Transaction) (WorkflowState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.transact(ctx, tx)
}

func (r *Repository) transact(ctx context.Context, tx Transaction) (WorkflowState, error) {
	if err := ctx.Err(); err != nil {
		return WorkflowState{}, err
	}
	stream := tx.Transition.Workstream
	log, v, err := r.loadWorkflow(stream)
	if err != nil {
		return WorkflowState{}, err
	}
	if old, ok := v.transactions[tx.Transition.ID]; ok {
		if !equalJSON(old, tx) {
			return WorkflowState{}, ErrConflict
		}
		return WorkflowState{Version: old.ExpectedVersion + 1, Value: old.Transition.To}, nil
	}
	state, err := v.transition(tx, r.project, stream)
	if err != nil {
		return WorkflowState{}, err
	}
	prefix := "workstreams/" + string(stream) + "/"
	events, err := r.readFile(prefix + "events.jsonl")
	if err != nil {
		return WorkflowState{}, err
	}
	for _, line := range bytes.Split(events, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		record, err := decodeRecord(line)
		if err != nil {
			return WorkflowState{}, err
		}
		if record.header().ID == tx.Transition.ID {
			return WorkflowState{}, ErrConflict
		}
	}
	line, err := json.Marshal(tx.Transition)
	if err != nil {
		return WorkflowState{}, err
	}
	log.Transactions = append(log.Transactions, tx)
	data, err := json.MarshalIndent(log, "", "  ")
	if err != nil {
		return WorkflowState{}, err
	}
	if err := r.publish(ctx, map[string][]byte{prefix + "workflow.json": append(data, '\n'), prefix + "events.jsonl": append(events, append(line, '\n')...)}); err != nil {
		return WorkflowState{}, err
	}
	_ = r.wake.Notify(context.Background())
	return state, nil
}

func (r *Repository) Workflow(stream config.WorkstreamID, subject string) (WorkflowState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, v, err := r.loadWorkflow(stream)
	if err != nil {
		return WorkflowState{}, err
	}
	if !key(subject) {
		return WorkflowState{}, fmt.Errorf("invalid workflow subject")
	}
	return v.states[subject], nil
}

// Outbox returns all entries and their retained delivery history, ordered by ID.
func (r *Repository) Outbox(stream config.WorkstreamID) ([]OutboxEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.outbox(stream, time.Time{})
}

// Ready scans durable notification intent; operations use Operations instead.
// Claims from another repository session are abandoned.
// A newly opened handle needs no wakeup signal to discover pending work.
func (r *Repository) Ready(stream config.WorkstreamID, now time.Time) ([]OutboxEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now.IsZero() {
		return nil, fmt.Errorf("ready timestamp required")
	}
	return r.outbox(stream, now)
}

func (r *Repository) outbox(stream config.WorkstreamID, now time.Time) ([]OutboxEntry, error) {
	_, v, err := r.loadWorkflow(stream)
	if err != nil {
		return nil, err
	}
	var entries []OutboxEntry
	for _, e := range v.entries {
		if !now.IsZero() && (e.Event.Operation != nil || e.Acknowledged || (e.Claim != nil && e.Claim.Session == r.session && e.Claim.Until.After(now))) {
			continue
		}
		entries = append(entries, *e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Event.ID < entries[j].Event.ID })
	return entries, nil
}

// Claim uses a caller-retained attempt token. Reusing a token with different
// claim parameters fails; retrying it never extends or resurrects its lease.
func (r *Repository) Claim(ctx context.Context, stream config.WorkstreamID, event, token, worker string, now time.Time, lease time.Duration) (DeliveryAction, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return DeliveryAction{}, err
	}
	log, v, err := r.loadWorkflow(stream)
	if err != nil {
		return DeliveryAction{}, err
	}
	if lease <= 0 {
		return DeliveryAction{}, ErrClaim
	}
	a := DeliveryAction{EventID: event, Kind: "claim", Token: token, Worker: worker, Session: r.session, At: now, Until: now.Add(lease)}
	e := v.entries[event]
	if e == nil {
		return DeliveryAction{}, ErrClaim
	}
	for _, old := range e.History {
		if old.Kind == "claim" && old.Token == token {
			a.Session = old.Session
			if !equalJSON(a, old) {
				return DeliveryAction{}, ErrConflict
			}
			if e.Claim == nil || e.Claim.Token != token || old.Session != r.session {
				return old, ErrClaim
			}
			return old, nil
		}
	}
	if err := v.delivery(a); err != nil {
		return DeliveryAction{}, err
	}
	if err := r.saveDelivery(ctx, stream, log, a); err != nil {
		return DeliveryAction{}, err
	}
	return a, nil
}

func (r *Repository) Acknowledge(ctx context.Context, stream config.WorkstreamID, event, token string, at time.Time) error {
	return r.finishDelivery(ctx, stream, event, token, at, "acknowledge")
}
func (r *Repository) Release(ctx context.Context, stream config.WorkstreamID, event, token string, at time.Time) error {
	return r.finishDelivery(ctx, stream, event, token, at, "release")
}
func (r *Repository) finishDelivery(ctx context.Context, stream config.WorkstreamID, event, token string, at time.Time, kind string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	log, v, err := r.loadWorkflow(stream)
	if err != nil {
		return err
	}
	e := v.entries[event]
	if e == nil || at.IsZero() {
		return ErrClaim
	}
	for _, old := range e.History {
		if old.Kind == kind && old.Token == token {
			return nil
		}
	}
	if e.Claim == nil || e.Claim.Session != r.session {
		return ErrClaim
	}
	a := DeliveryAction{EventID: event, Kind: kind, Token: token, At: at}
	if err := v.delivery(a); err != nil {
		return err
	}
	return r.saveDelivery(ctx, stream, log, a)
}
func (r *Repository) saveDelivery(ctx context.Context, stream config.WorkstreamID, log workflowLog, a DeliveryAction) error {
	log.Deliveries = append(log.Deliveries, a)
	data, err := json.MarshalIndent(log, "", "  ")
	if err != nil {
		return err
	}
	if err := r.publish(ctx, map[string][]byte{"workstreams/" + string(stream) + "/workflow.json": append(data, '\n')}); err != nil {
		return err
	}
	if a.Kind == "release" {
		_ = r.wake.Notify(context.Background())
	}
	return nil
}

// WaitWorkflow returns on a coalesced core wakeup, a caller tick, or cancellation.
// Callers scan state on startup and after every return; a wake is not delivery.
func (r *Repository) WaitWorkflow(ctx context.Context, ticks <-chan time.Time) error {
	return r.wake.Wait(ctx, ticks)
}
