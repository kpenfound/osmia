package trace

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
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

// OperationAttempt is valid only during WithOperation's synchronous callback.
// Its writes are fenced by the current session and attempt token.
type OperationAttempt struct {
	repository *Repository
	stream     config.WorkstreamID
	claim      OperationAction
	closed     bool
}

// WithOperation serializes reconciliation and Close for this repository,
// except for the work a callback runs through Unlocked. The exclusive
// repository process lock rules out other owners. Claims do not expire while
// an external call is in flight; a returned callback or process exit ends
// execution ownership. New attempts always inspect before applying an effect.
// The callback must join its work before returning and must not close the store
// or recursively call WithOperation. Acknowledged/not-yet-due work, and work an
// open attempt of this handle holds, is a no-op.
func (r *Repository) WithOperation(ctx context.Context, stream config.WorkstreamID, event string, actor Actor, now func() time.Time, fn func(*OperationAttempt, OperationRecord) error) error {
	r.operationMu.Lock()
	defer r.operationMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	if r.lock == nil {
		r.mu.Unlock()
		return ErrClaim
	}
	_, v, err := r.loadWorkflow(stream)
	if err != nil {
		r.mu.Unlock()
		return err
	}
	o := v.operations[event]
	if o == nil {
		r.mu.Unlock()
		return ErrClaim
	}
	at := now()
	key := string(stream) + "/" + event
	if o.Acknowledged || at.Before(o.RetryAt) || r.attempts[key] {
		r.mu.Unlock()
		return nil
	}
	if r.attempts == nil {
		r.attempts = map[string]bool{}
	}
	r.attempts[key] = true
	r.mu.Unlock()
	a := &OperationAttempt{repository: r, stream: stream, claim: OperationAction{EventID: event, Token: rand.Text(), Session: r.session, Kind: "claim", At: at, Actor: actor, Cause: o.Operation.ID, Depth: o.Transition.Depth}}
	defer func() { r.mu.Lock(); a.closed = true; delete(r.attempts, key); r.mu.Unlock() }()
	if err := a.Record(ctx, a.claim); err != nil {
		return err
	}
	return fn(a, *o)
}

// unlockedKey marks the context Unlocked passes to its work.
type unlockedKey struct{}

// unlocked is the lock work run through Unlocked takes back once.
type unlocked struct {
	r    *Repository
	back sync.Once
}

func (u *unlocked) relock() { u.back.Do(u.r.operationMu.Lock) }

// Unlocked runs fn without the lock WithOperation holds, so other operations
// are reconciled, and Close may run, while fn is in flight; the lock is taken
// back before it returns. fn receives ctx, with which Relock takes the lock
// back early. The attempt keeps its claim throughout, and fn must not use the
// attempt. A caller that closes the repository joins fn first.
func (a *OperationAttempt) Unlocked(ctx context.Context, fn func(context.Context)) {
	u := &unlocked{r: a.repository}
	u.r.operationMu.Unlock()
	defer u.relock()
	fn(context.WithValue(ctx, unlockedKey{}, u))
}

// Relock runs fn after taking back the lock Unlocked released, when ctx comes
// from Unlocked: the rest of that work, and the callback after it, run
// serialized with reconciliation. Otherwise, in a callback that holds the
// lock or outside any operation, or with a context from Beside, it runs fn
// as it is.
func Relock(ctx context.Context, fn func() error) error {
	if u, ok := ctx.Value(unlockedKey{}).(*unlocked); ok && u != nil {
		u.relock()
	}
	return fn()
}

// Beside returns ctx for one of several pieces of work that run at the same
// time within one Unlocked call: Relock given it runs fn as it is, so one
// piece ending does not take the lock back while the others are in flight.
// Their writes land without the lock; the caller relocks with the context
// Unlocked passed once they have all returned.
func Beside(ctx context.Context) context.Context {
	return context.WithValue(ctx, unlockedKey{}, (*unlocked)(nil))
}

// Serialize runs fn serialized with reconciliation and Close. fn must not call
// WithOperation, Serialize or Close.
func (r *Repository) Serialize(fn func() error) error {
	r.operationMu.Lock()
	defer r.operationMu.Unlock()
	return fn()
}

// Action supplies the immutable provenance of this attempt for a boundary write.
func (a *OperationAttempt) Action(kind string, at time.Time) OperationAction {
	action := a.claim
	action.Kind, action.At = kind, at
	return action
}
func (a *OperationAttempt) Record(ctx context.Context, action OperationAction) error {
	r := a.repository
	r.mu.Lock()
	defer r.mu.Unlock()
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
	data, err := json.MarshalIndent(log, "", "  ")
	if err != nil {
		return err
	}
	return r.publish(ctx, map[string][]byte{"workstreams/" + string(a.stream) + "/workflow.json": append(data, '\n')})
}
