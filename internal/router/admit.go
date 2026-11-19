package router

import (
	"context"
	"errors"
)

// Admission says whether a pinned exit would be applied now: its subject is
// where the exit left it and its target meets every hard prerequisite and
// limit. A hold is not a refusal: nothing is recorded for it, and the exit is
// admitted once the hold clears.
type Admission struct {
	Admitted bool
	// Hold is HoldPaused, HoldBusy or HoldStale when the exit is not admitted.
	Hold   string
	Reason string
}

// Admitter is a router port that can say whether a pinned exit would be applied
// now, before the caller does what the move depends on.
type Admitter interface {
	Admit(ctx context.Context, x Exit) (Admission, error)
}

// Admit checks a pinned exit against current state, as applying it would, and
// records nothing. A caller whose move rests on an effect it cannot undo asks
// first, so that a hold on the move stops the effect before it happens. A
// non-nil error is a failure to read.
func (r *Router) Admit(ctx context.Context, x Exit) (Admission, error) {
	if r == nil {
		return Admission{}, errors.New("the router is unavailable")
	}
	if err := x.validate(); err != nil {
		return Admission{}, err
	}
	if !x.Authority.pinned() {
		return Admission{}, errors.New("only a pinned exit is admitted ahead of its effect")
	}
	state, err := x.Repo.RawWorkflow(x.Workstream, x.Subject)
	if err != nil {
		return Admission{}, err
	}
	if state.Version != x.Version || state.Value != x.From {
		return Admission{Hold: HoldStale, Reason: "the subject moved on since the exit was raised"}, nil
	}
	target, ok := x.target(x.Authority.Target)
	if !ok {
		return Admission{}, errors.New("the authorised target is not among the targets")
	}
	if why, err := r.paused(ctx, x, target); err != nil {
		return Admission{}, err
	} else if why != "" {
		return Admission{Hold: HoldPaused, Reason: why}, nil
	}
	why, err := r.legality(ctx, x, target)
	if err != nil {
		return Admission{}, err
	}
	if why != "" {
		return Admission{Hold: HoldBusy, Reason: why}, nil
	}
	return Admission{Admitted: true}, nil
}

// router note 8e10f0e4
