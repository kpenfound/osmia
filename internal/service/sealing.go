package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/reconcile"
)

type repositoryAdapter struct {
	other     coreadapter.Reconciler
	seals     *sealer
	builds    *builder
	lands     *foreman
	publishes coreadapter.Reconciler
	landings  coreadapter.Reconciler
}

func (a repositoryAdapter) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	switch op.Action {
	case SealAction:
		return a.seals.Inspect(ctx, op)
	case BuildAction:
		return a.builds.Inspect(ctx, op)
	case LandAction:
		return a.lands.Inspect(ctx, op)
	case RebaseAction:
		return rebaser{a.lands}.Inspect(ctx, op)
	case DriftAction:
		return drifter{a.lands}.Inspect(ctx, op)
	case baseRefreshAction:
		return (&baseRefresher{S: a.lands.s.lifecycle(), Repository: a.seals.Repository}).Inspect(ctx, op)
	case PublishAction, publishUpstreamAction:
		return a.publishes.Inspect(ctx, op)
	case landDeliveryAction:
		return a.landings.Inspect(ctx, op)
	}
	if a.other == nil {
		return coreadapter.Observation{State: coreadapter.EffectUnknown, Evidence: "No reconciliation adapter configured"}, nil
	}
	return a.other.Inspect(ctx, op)
}

func (a repositoryAdapter) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	switch op.Action {
	case SealAction:
		return a.seals.Apply(ctx, op)
	case BuildAction:
		return a.builds.Apply(ctx, op)
	case LandAction:
		return a.lands.Apply(ctx, op)
	case RebaseAction:
		return rebaser{a.lands}.Apply(ctx, op)
	case DriftAction:
		return drifter{a.lands}.Apply(ctx, op)
	case baseRefreshAction:
		return (&baseRefresher{S: a.lands.s.lifecycle(), Repository: a.seals.Repository}).Apply(ctx, op)
	case PublishAction, publishUpstreamAction:
		return a.publishes.Apply(ctx, op)
	case landDeliveryAction:
		return a.landings.Apply(ctx, op)
	}
	if a.other == nil {
		return coreadapter.OperationResult{}, errors.New("no repository adapter is configured")
	}
	return a.other.Apply(ctx, op)
}

var _ reconcile.Bounded = repositoryAdapter{}

func (a repositoryAdapter) boundedBy(op coreadapter.Operation) (reconcile.Bounded, bool) {
	var b reconcile.Bounded
	switch op.Action {
	case LandAction, RebaseAction:
		b = a.lands
	case PublishAction, publishUpstreamAction:
		var ok bool
		if b, ok = a.publishes.(reconcile.Bounded); !ok {
			return nil, false
		}
	case landDeliveryAction:
		var ok bool
		if b, ok = a.landings.(reconcile.Bounded); !ok {
			return nil, false
		}
	default:
		return nil, false
	}
	return b, b.Bound(op)
}

func (a repositoryAdapter) Bound(op coreadapter.Operation) bool {
	_, ok := a.boundedBy(op)
	return ok
}

func (a repositoryAdapter) Failing(ctx context.Context, op coreadapter.Operation, failures int, reason string) error {
	if b, ok := a.boundedBy(op); ok {
		return b.Failing(ctx, op, failures, reason)
	}
	return nil
}

func (a repositoryAdapter) GiveUp(ctx context.Context, op coreadapter.Operation, why reconcile.GiveUpReason) (coreadapter.OperationResult, error) {
	if b, ok := a.boundedBy(op); ok {
		return b.GiveUp(ctx, op, why)
	}
	return coreadapter.OperationResult{}, fmt.Errorf("operation %q is not bounded", op.Action)
}
