package execution

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

// FailureNotice is the number of consecutive failed attempts of a landing or a
// rebase at which the manager is told. It matches the failures at
// which a publication opens its inbox entry.
const FailureNotice = 3

var _ reconcile.Bounded = (*foreman)(nil)

type recording struct {
	existing bool
	note     string
}

var errNothingToRecord = errors.New("the effect is not on its target")

type unreadable struct{ err error }

func (u *unreadable) Error() string { return u.err.Error() }
func (u *unreadable) Unwrap() error { return u.err }

func (f *foreman) Bound(op coreadapter.Operation) bool {
	return op.Boundary == coreadapter.RepositoryBoundary && (op.Action == LandAction || op.Action == RebaseAction)
}

func (f *foreman) Failing(ctx context.Context, op coreadapter.Operation, failures int, reason string) error {
	if failures != FailureNotice {
		return nil
	}
	var stream config.WorkstreamID
	var what string
	switch op.Action {
	case LandAction:
		in, err := DecodeLand(op)
		if err != nil {
			return err
		}
		if stream, err = f.stream(op, in); err != nil {
			return err
		}
		what = fmt.Sprintf("The landing of unit %s", in.Unit)
	case RebaseAction:
		in, err := DecodeRebase(op)
		if err != nil {
			return err
		}
		if stream, err = (rebaser{f}).stream(op, in); err != nil {
			return err
		}
		what = fmt.Sprintf("The rebase of unit %s", in.Unit)
	default:
		return nil
	}
	return f.repository.RecordOperationNotice(ctx, stream, trace.OperationNotice{
		ID: fmt.Sprintf("%s-failing-%d", op.ID, failures), Operation: op.ID, Actor: foremanActor, At: f.s.Now(),
		Reason: fmt.Sprintf("%s has failed %d times in a row", what, failures),
		Body:   fmt.Sprintf("%s has failed %d times in a row. Last failure: %s. The service tries again, waiting longer after each failure, and records a result once it has failed %d times and its give-up window has passed.", what, failures, reason, reconcile.GiveUpFailures),
	})
}

func (f *foreman) GiveUp(ctx context.Context, op coreadapter.Operation, why reconcile.GiveUpReason) (coreadapter.OperationResult, error) {
	switch op.Action {
	case LandAction:
		return f.giveUpLanding(ctx, op, why)
	case RebaseAction:
		return (rebaser{f}).giveUp(ctx, op, why)
	}
	return coreadapter.OperationResult{}, fmt.Errorf("operation %q is not bounded", op.Action)
}

func giveUpNote(what string, why reconcile.GiveUpReason) string {
	return fmt.Sprintf("%s failed repeatedly (%s) but its effect is already on its target, so it is recorded as done and not repeated.", what, why.Summary())
}

func unreadableReason(err error, why reconcile.GiveUpReason) string {
	return fmt.Sprintf("%s, and %s: %v (%s); a mutation may still be running on the clone, so none was made", RefusedAfterRetries, RefusedUnknownOutcome, err, why.Summary())
}

func unreadableFailure(err error) bool {
	var read *unreadable
	return workspace.IsContention(err) || workspace.IsRecoveryFailure(err) || errors.As(err, &read)
}

func (f *foreman) giveUpLanding(ctx context.Context, op coreadapter.Operation, why reconcile.GiveUpReason) (coreadapter.OperationResult, error) {
	in, err := DecodeLand(op)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	stream, err := f.stream(op, in)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if result, err := f.outcome(stream, in); err != nil || result != nil {
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		return *result, nil
	}
	requested, err := f.RequestedAt(stream, op.ID)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	g, err := featureWorkspaces(f.cfg, f.repository).Of(stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	var recorded coreadapter.OperationResult
	var absent string
	err = workspace.Quiesced(ctx, []workspace.Provider{g}, func(ctx context.Context) error {
		observed, err := f.observeLanding(ctx, g, featureBranch(stream), in, op.ID)
		if err != nil {
			return &unreadable{err}
		}
		recorded, err = checkpointed(ctx, string(stream)+"/"+op.ID, requested, []workspace.Provider{g}, func(ctx context.Context) (coreadapter.OperationResult, error) {
			return f.land(ctx, op.ID, stream, in, g, requested, recording{existing: true, note: giveUpNote(fmt.Sprintf("The landing of unit %s", in.Unit), why)})
		})
		if errors.Is(err, errNothingToRecord) {
			absent = observed.Evidence
			return nil
		}
		return err
	})
	if err == nil && absent == "" {
		return recorded, nil
	}
	if result, outcomeErr := f.outcome(stream, in); outcomeErr == nil && result != nil {
		return *result, nil
	}
	write, cancel := terminalContext(ctx)
	defer cancel()
	switch {
	case err == nil:
		reason := fmt.Sprintf("%s (%s); the landing is confirmed absent: %s", RefusedAfterRetries, why.Summary(), absent)
		return f.refuseWith(write, stream, in, reason, fmt.Sprintf("Unit %s did not land: %s. The foreman made no change to the feature branch; what happens to the unit next is for the router to decide.", in.Unit, reason))
	case unreadableFailure(err) || ctx.Err() != nil:
		reason := unreadableReason(err, why)
		return f.refuseWith(write, stream, in, reason, fmt.Sprintf("Unit %s may or may not have landed: %s. The foreman made no further change to the feature branch.", in.Unit, reason))
	}
	return coreadapter.OperationResult{}, err
}

func (r rebaser) giveUp(ctx context.Context, op coreadapter.Operation, why reconcile.GiveUpReason) (coreadapter.OperationResult, error) {
	in, err := DecodeRebase(op)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	stream, err := r.stream(op, in)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if result, err := r.outcome(stream, in); err != nil || result != nil {
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		return *result, nil
	}
	requested, err := r.RequestedAt(stream, op.ID)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	g, err := NewUnitWorkspaces(r.cfg, r.repository).Of(stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	var recorded coreadapter.OperationResult
	var absent string
	err = workspace.Quiesced(ctx, []workspace.Provider{g}, func(ctx context.Context) error {
		observed, err := r.observeRebase(ctx, g, UnitBranch(stream, in.Unit), in, op.ID)
		if err != nil {
			return &unreadable{err}
		}
		recorded, err = checkpointed(ctx, string(stream)+"/"+op.ID, requested, []workspace.Provider{g}, func(ctx context.Context) (coreadapter.OperationResult, error) {
			return r.rebase(ctx, op.ID, stream, in, g, requested, recording{existing: true, note: giveUpNote(fmt.Sprintf("The rebase of unit %s", in.Unit), why)})
		})
		if errors.Is(err, errNothingToRecord) {
			absent = observed.Evidence
			return nil
		}
		return err
	})
	if err == nil && absent == "" {
		return recorded, nil
	}
	if result, outcomeErr := r.outcome(stream, in); outcomeErr == nil && result != nil {
		return *result, nil
	}
	write, cancel := terminalContext(ctx)
	defer cancel()
	switch {
	case err == nil:
		reason := fmt.Sprintf("%s (%s); the rebase is confirmed absent: %s", RefusedAfterRetries, why.Summary(), absent)
		return r.refuseWith(write, stream, in, reason, fmt.Sprintf("Unit %s was not rebased onto %s: %s. The foreman made no change to the unit's workspace. It asks for the rebase again on a later pass; once %d rebases onto that commit are refused, the unit is contested.", in.Unit, in.Onto, reason, MaxRefusedRebases))
	case unreadableFailure(err) || ctx.Err() != nil:
		reason := unreadableReason(err, why)
		return r.refuseWith(write, stream, in, reason, fmt.Sprintf("Unit %s may or may not have been rebased onto %s: %s. The foreman made no further change to the unit's workspace. It asks for the rebase again on a later pass; once %d rebases onto that commit are refused, the unit is contested.", in.Unit, in.Onto, reason, MaxRefusedRebases))
	}
	return coreadapter.OperationResult{}, err
}

func terminalContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return ctx, func() {}
	}
	return context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
}
