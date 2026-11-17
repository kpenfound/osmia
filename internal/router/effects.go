package router

import (
	"context"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/trace"
)

// Plan is what an effect is built for: the exit, the target it enters, and the
// identity and routing of the transition that will enter it.
type Plan struct {
	Exit   Exit
	Target Target
	// TransitionID is the ID of the transition into the target; an effect
	// derives the IDs of its events and documents from it, so a retry of the
	// same move records the same ones.
	TransitionID string
	// Reason is the transition's reason.
	Reason  string
	At      time.Time
	Routing trace.Routing
}

// Effect is what entering a target records beyond the transition itself, in the
// same commit: documents (a move record, an approval record, a fresh review
// request), events for the outbox (notices to the chief of staff, the
// mason's fresh allowance) and transactions of other subjects of the
// workstream.
type Effect struct {
	Documents []trace.Document
	Events    []trace.Event
	With      []trace.Transaction
	// Reason, when set, replaces the transition's reason.
	Reason string
	// Cause, when set, replaces the cause the transition cites.
	Cause string
}

// EffectFunc builds the effect of entering a target. An error wrapping
// ErrCannotCarryOut says the target cannot be carried out; any other error is
// a failure to read and is retried.
type EffectFunc func(ctx context.Context, p Plan) (Effect, error)

// Effects is the registry of per-target side effects. Each target of each
// machine has its effect defined once, here, whoever raises the exit: a
// default, a Jev pick, a chief proposal, an owner decision and an owner card
// all enter the target through it.
type Effects struct {
	mu    sync.RWMutex
	funcs map[string]EffectFunc
}

func effectKey(machine, target string) string { return machine + "/" + target }

// Register defines the effect of entering target in machine. A second
// registration of the same pair replaces the first.
func (e *Effects) Register(machine, target string, f EffectFunc) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.funcs == nil {
		e.funcs = map[string]EffectFunc{}
	}
	e.funcs[effectKey(machine, target)] = f
}

// Registered reports whether an effect is defined for the pair.
func (e *Effects) Registered(machine, target string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	_, ok := e.funcs[effectKey(machine, target)]
	return ok
}

// build returns the effect of the plan's target. The exit's default target is
// entered as the trigger enters it when the exit says how; any other target is
// entered through the registered effect, or has none. The records of the
// trigger itself come with every target.
func (e *Effects) build(ctx context.Context, p Plan) (Effect, error) {
	var out Effect
	var err error
	switch x := p.Exit; {
	case x.Enter != nil && p.Target.Name == x.Default:
		out, err = x.Enter(ctx, p)
	case e != nil:
		e.mu.RLock()
		f := e.funcs[effectKey(x.Machine, p.Target.Name)]
		e.mu.RUnlock()
		if f != nil {
			out, err = f(ctx, p)
		}
	}
	if err != nil {
		return Effect{}, err
	}
	if p.Exit.Records != nil {
		records, err := p.Exit.Records(ctx, p)
		if err != nil {
			return Effect{}, err
		}
		out.Documents = append(append([]trace.Document{}, records.Documents...), out.Documents...)
		out.Events = append(out.Events, records.Events...)
		out.With = append(out.With, records.With...)
	}
	return out, nil
}
