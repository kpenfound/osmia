package trace

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/ghlog"
)

// OperationID binds external identity to exactly one project, stream and intent.
func OperationID(project config.ProjectID, stream config.WorkstreamID, event string) string {
	data, _ := json.Marshal([]string{string(project), string(stream), event})
	return fmt.Sprintf("operation_%x", sha256.Sum256(data))
}

// OperationAction retains the actor, cause, session and attempt for each boundary.
type OperationAction struct {
	EventID     string                       `json:"event_id"`
	Token       string                       `json:"token"`
	Session     string                       `json:"session"`
	Kind        string                       `json:"kind"`
	At          time.Time                    `json:"at"`
	Actor       Actor                        `json:"actor"`
	Cause       string                       `json:"cause"`
	Depth       int                          `json:"depth"`
	Observation *coreadapter.Observation     `json:"observation,omitempty"`
	Result      *coreadapter.OperationResult `json:"result,omitempty"`
	Failure     string                       `json:"failure,omitempty"`
	RetryAt     time.Time                    `json:"retry_at,omitempty"`
}

type OperationRecord struct {
	Operation     coreadapter.Operation        `json:"operation"`
	EventID       string                       `json:"event_id"`
	Transition    Header                       `json:"transition"`
	History       []OperationAction            `json:"history"`
	Claim         *OperationAction             `json:"claim,omitempty"`
	Observation   *coreadapter.Observation     `json:"observation,omitempty"`
	EffectStarted bool                         `json:"effect_started"`
	Result        *coreadapter.OperationResult `json:"result,omitempty"`
	RetryAt       time.Time                    `json:"retry_at,omitempty"`
	Acknowledged  bool                         `json:"acknowledged"`
}

func validateOperation(op coreadapter.Operation, project config.ProjectID, stream config.WorkstreamID, event string) error {
	if op.ID != OperationID(project, stream, event) || !key(op.Action) || !json.Valid(op.Input) {
		return fmt.Errorf("invalid operation identity or input")
	}
	switch op.Boundary {
	case coreadapter.RepositoryBoundary, coreadapter.RunnerBoundary, coreadapter.ContainerBoundary:
		return nil
	default:
		return fmt.Errorf("unsupported local operation boundary %q", op.Boundary)
	}
}
func validResult(r *coreadapter.OperationResult) bool {
	return r != nil && present(r.Outcome) && present(r.Evidence) && (len(r.Data) == 0 || json.Valid(r.Data))
}
func validObservation(o *coreadapter.Observation) bool {
	if o == nil || !present(o.Evidence) {
		return false
	}
	switch o.State {
	case coreadapter.EffectAbsent, coreadapter.EffectUnknown:
		return o.Result == nil
	case coreadapter.EffectCompleted:
		return validResult(o.Result)
	default:
		return false
	}
}

func (v *workflowView) operation(a OperationAction) error {
	o := v.operations[a.EventID]
	if o == nil || o.Acknowledged || !key(a.Token) || !key(a.Session) || a.At.IsZero() || a.Actor.Kind != "service" || !validActor(a.Actor) || a.Cause != o.Operation.ID || a.Depth != o.Transition.Depth {
		return ErrClaim
	}
	if len(o.History) > 0 && a.At.Before(o.History[len(o.History)-1].At) {
		return ErrClaim
	}
	base := a
	base.Observation, base.Result, base.Failure, base.RetryAt = nil, nil, "", time.Time{}
	if a.Kind != "claim" && (o.Claim == nil || o.Claim.Token != a.Token || o.Claim.Session != a.Session || o.Claim.Actor != a.Actor) {
		return ErrClaim
	}
	switch a.Kind {
	case "claim":
		if a.At.Before(o.RetryAt) {
			return ErrClaim
		}
		for _, old := range o.History {
			if old.Kind == "claim" && old.Token == a.Token {
				return ErrConflict
			}
		}
	case "observe":
		if o.Result != nil || o.Observation != nil || !validObservation(a.Observation) {
			return ErrConflict
		}
		base.Observation = a.Observation
	case "effect":
		if o.Result != nil || o.EffectStarted || o.Observation == nil || o.Observation.State != coreadapter.EffectAbsent {
			return ErrConflict
		}
	case "result":
		if o.Result != nil || !validResult(a.Result) || o.Observation == nil {
			return ErrConflict
		}
		if o.Observation.State == coreadapter.EffectCompleted {
			if !equalJSON(a.Result, o.Observation.Result) {
				return ErrConflict
			}
		} else if o.Observation.State != coreadapter.EffectAbsent || !o.EffectStarted {
			return ErrConflict
		}
		base.Result = a.Result
	case "retry":
		if o.Result != nil || !present(a.Failure) || !a.RetryAt.After(a.At) {
			return ErrConflict
		}
		base.Failure, base.RetryAt = a.Failure, a.RetryAt
	case "acknowledge":
		if o.Result == nil {
			return ErrConflict
		}
	default:
		return fmt.Errorf("invalid operation action %q", a.Kind)
	}
	if !equalJSON(base, a) {
		return fmt.Errorf("unexpected operation action fields")
	}
	switch a.Kind {
	case "claim":
		copy := a
		o.Claim, o.Observation, o.EffectStarted, o.RetryAt = &copy, nil, false, time.Time{}
	case "observe":
		o.Observation = a.Observation
	case "effect":
		o.EffectStarted = true
	case "result":
		o.Result = a.Result
	case "retry":
		o.Claim, o.RetryAt = nil, a.RetryAt
	case "acknowledge":
		o.Claim, o.Acknowledged = nil, true
		v.entries[a.EventID].Acknowledged = true
	}
	o.History = append(o.History, a)
	return nil
}

// Operations scans all operation histories, including completed results awaiting acknowledgement.
func (r *Repository) Operations(stream config.WorkstreamID) ([]OperationRecord, error) {
	r, err := r.scoped(stream)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, v, err := r.loadWorkflow(stream)
	if err != nil {
		return nil, err
	}
	var records []OperationRecord
	for _, o := range v.operations {
		records = append(records, *o)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].EventID < records[j].EventID })
	return records, nil
}

// OperationClass determines execution ordering, independently of whether the
// controller runs an operation inline or beside its pass.
type OperationClass string

const (
	WorkflowOperation     OperationClass = "workflow"
	WorkerOperation       OperationClass = "worker"
	ConversationOperation OperationClass = "conversation"
)

// SlowThreshold is the default duration after which operations and bookkeeping
// lock waits are logged.
const SlowThreshold = time.Second

type slowThresholdKey struct{}

// WithSlowThreshold sets the threshold for operation and lock-wait diagnostics.
func WithSlowThreshold(ctx context.Context, threshold time.Duration) context.Context {
	return context.WithValue(ctx, slowThresholdKey{}, threshold)
}

func slowThreshold(ctx context.Context) time.Duration {
	if d, ok := ctx.Value(slowThresholdKey{}).(time.Duration); ok && d > 0 {
		return d
	}
	return SlowThreshold
}

func (r *Repository) operationLock(ctx context.Context, stream config.WorkstreamID, operation string) func() {
	start := time.Now()
	r.mu.Lock()
	duration := time.Since(start)
	return func() {
		r.mu.Unlock()
		if duration >= slowThreshold(ctx) {
			ghlog.Logger(ctx).Warn("lock wait exceeded threshold", "project", string(r.project), "workstream", string(stream), "operation", operation, "lock", "trace bookkeeping", "duration", duration.String())
		}
	}
}

// OperationAttempt owns an admitted operation until Close. Its writes are fenced
// by the current session and attempt token. The owner must join all its work
// before closing it; Repository.Close joins admitted attempts before closing files.
type OperationAttempt struct {
	repository *Repository
	stream     config.WorkstreamID
	claim      OperationAction
	class      OperationClass
	closed     bool
}

// BeginOperation tries to claim due work. A nil attempt means it is already
// acknowledged, not due, active, or excluded by its workstream's workflow
// operation. It never waits for another operation's inspection or effect.
// Worker and conversation operations rely on their scheduler and thread order;
// neither reserves the workstream. Every admitted attempt must be closed.
func (r *Repository) BeginOperation(ctx context.Context, stream config.WorkstreamID, event string, class OperationClass, actor Actor, now func() time.Time) (*OperationAttempt, OperationRecord, error) {
	var empty OperationRecord
	if class != WorkflowOperation && class != WorkerOperation && class != ConversationOperation {
		return nil, empty, fmt.Errorf("invalid operation class %q", class)
	}
	r, err := r.scoped(stream)
	if err != nil {
		return nil, empty, err
	}
	unlock := r.operationLock(ctx, stream, event)
	defer unlock()
	if err := ctx.Err(); err != nil {
		return nil, empty, err
	}
	if r.lock == nil || r.closing {
		return nil, empty, ErrClaim
	}
	_, v, err := r.loadWorkflow(stream)
	if err != nil {
		return nil, empty, err
	}
	o := v.operations[event]
	if o == nil {
		return nil, empty, ErrClaim
	}
	at := now()
	key := string(stream) + "/" + event
	if o.Acknowledged || at.Before(o.RetryAt) || r.attempts[key] || (class == WorkflowOperation && r.workflow) {
		return nil, empty, nil
	}
	a := &OperationAttempt{repository: r, stream: stream, class: class, claim: OperationAction{EventID: event, Token: rand.Text(), Session: r.session, Kind: "claim", At: at, Actor: actor, Cause: o.Operation.ID, Depth: o.Transition.Depth}}
	// loadWorkflow returns a private view, so this snapshot stays immutable
	// while later bookkeeping publishes new views.
	record := *o
	if err := a.record(ctx, a.claim); err != nil {
		return nil, empty, err
	}
	if r.attempts == nil {
		r.attempts = map[string]bool{}
	}
	r.attempts[key] = true
	if class == WorkflowOperation {
		r.workflow = true
	}
	r.active.Add(1)
	return a, record, nil
}

// Close releases this attempt's execution ownership after all its work returns.
// It is idempotent and leaves durable records intact; an interrupted attempt
// remains available for restart inspection.
func (a *OperationAttempt) Close() {
	r := a.repository
	r.mu.Lock()
	defer r.mu.Unlock()
	if a.closed {
		return
	}
	a.closed = true
	delete(r.attempts, string(a.stream)+"/"+a.claim.EventID)
	if a.class == WorkflowOperation {
		r.workflow = false
	}
	r.active.Done()
	// Wake after releasing admission, so a scan cannot consume the result's
	// hint while the workstream is still reserved and strand the next intent.
	_ = r.wake.Notify(context.Background())
}

// WithOperation reconciles a workflow operation under its workstream's ordering.
// Worker and conversation callers use BeginOperation with their explicit class.
// The callback must join its work before returning and must not close the store.
func (r *Repository) WithOperation(ctx context.Context, stream config.WorkstreamID, event string, actor Actor, now func() time.Time, fn func(*OperationAttempt, OperationRecord) error) error {
	a, record, err := r.BeginOperation(ctx, stream, event, WorkflowOperation, actor, now)
	if err != nil || a == nil {
		return err
	}
	defer a.Close()
	return fn(a, record)
}

// Unlocked runs work within this attempt's lifetime. No bookkeeping lock is
// held by an attempt's callback, inspection, or effect.
func (a *OperationAttempt) Unlocked(ctx context.Context, fn func(context.Context)) { fn(ctx) }

// Relock runs bookkeeping work. Each repository write takes its own short lock;
// this callback never acquires a project-wide reconciliation lock.
func Relock(ctx context.Context, fn func() error) error { return fn() }

// Beside returns the context for work joined by the attempt's owner.
func Beside(ctx context.Context) context.Context { return ctx }

// Serialize runs a callback without a project-wide lock. Repository writes
// provide their own bookkeeping exclusion. Callers must join before Close.
// Deprecated: invoke the callback directly.
func (r *Repository) Serialize(fn func() error) error { return fn() }

// Action supplies the immutable provenance of this attempt for a boundary write.
func (a *OperationAttempt) Action(kind string, at time.Time) OperationAction {
	action := a.claim
	action.Kind, action.At = kind, at
	return action
}
func (a *OperationAttempt) Record(ctx context.Context, action OperationAction) error {
	r := a.repository
	unlock := r.operationLock(ctx, a.stream, a.claim.Cause)
	defer unlock()
	return a.record(ctx, action)
}

// record is called with the repository bookkeeping lock held.
func (a *OperationAttempt) record(ctx context.Context, action OperationAction) error {
	r := a.repository
	if err := ctx.Err(); err != nil {
		return err
	}
	if a.closed || r.lock == nil || action.EventID != a.claim.EventID || action.Token != a.claim.Token || action.Session != r.session || action.Actor != a.claim.Actor {
		return ErrClaim
	}
	log, v, err := r.loadWorkflow(a.stream)
	if err != nil {
		return err
	}
	if err := v.operation(action); err != nil {
		return err
	}
	log.Operations = append(log.Operations, action)
	return r.publish(ctx, change{stream: a.stream, workflow: &log, view: v})
}
