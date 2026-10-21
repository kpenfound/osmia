package service

import (
	"context"
	"errors"

	"github.com/kpenfound/osmia/internal/coreadapter"
)

// repositoryAdapter routes repository-boundary operations: sealings to the
// service's sealer, builds to its builder, landings, rebases and drift
// rebases to its foreman, publications to its publisher, everything else to the configured
// reconciler.
type repositoryAdapter struct {
	other     coreadapter.Reconciler
	seals     *sealer
	builds    *builder
	lands     *foreman
	publishes coreadapter.Reconciler
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
		return (&baseRefresher{s: a.lands.s, repository: a.seals.Repository}).Inspect(ctx, op)
	case PublishAction, publishUpstreamAction:
		return a.publishes.Inspect(ctx, op)
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
		return (&baseRefresher{s: a.lands.s, repository: a.seals.Repository}).Apply(ctx, op)
	case PublishAction, publishUpstreamAction:
		return a.publishes.Apply(ctx, op)
	}
	if a.other == nil {
		return coreadapter.OperationResult{}, errors.New("no repository adapter is configured")
	}
	return a.other.Apply(ctx, op)
}
