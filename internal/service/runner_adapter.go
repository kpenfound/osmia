package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

type runnerAdapter struct {
	deltas      deltaRunner
	turns       coreadapter.Reconciler
	draft       *drafter
	amend       *amendmentDrafter
	amendRounds amendmentDebate
	rounds      *debate
	finals      coreadapter.Reconciler
	checks      *checkers
}

func (a runnerAdapter) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	if op.Action == deltaTurnAction {
		return a.deltas.Inspect(ctx, op)
	}
	if op.Action == DraftAction {
		return a.draft.Inspect(ctx, op)
	}
	if op.Action == AmendmentDraftAction {
		return a.amend.Inspect(ctx, op)
	}
	if op.Action == AmendmentRoundAction || op.Action == AmendmentReplyAction {
		return a.amendRounds.Inspect(ctx, op)
	}
	if op.Action == RoundAction {
		return a.rounds.Inspect(ctx, op)
	}
	if op.Action == ReplyAction || op.Action == RedraftAction {
		return replier{Debate: a.rounds}.Inspect(ctx, op)
	}
	if op.Action == FinalReviewAction || op.Action == deliveryReviewAction {
		return a.finals.Inspect(ctx, op)
	}
	if op.Action == CheckAction {
		return a.checks.Inspect(ctx, op)
	}
	if a.turns == nil {
		return coreadapter.Observation{State: coreadapter.EffectUnknown, Evidence: "No reconciliation adapter configured"}, nil
	}
	return a.turns.Inspect(ctx, op)
}

func (a runnerAdapter) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	if op.Action == deltaTurnAction {
		return a.deltas.Apply(ctx, op)
	}
	if op.Action == DraftAction {
		return a.draft.Apply(ctx, op)
	}
	if op.Action == AmendmentDraftAction {
		return a.amend.Apply(ctx, op)
	}
	if op.Action == AmendmentRoundAction || op.Action == AmendmentReplyAction {
		return a.amendRounds.Apply(ctx, op)
	}
	if op.Action == RoundAction {
		return a.rounds.Apply(ctx, op)
	}
	if op.Action == ReplyAction || op.Action == RedraftAction {
		return replier{Debate: a.rounds}.Apply(ctx, op)
	}
	if op.Action == FinalReviewAction || op.Action == deliveryReviewAction {
		return a.finals.Apply(ctx, op)
	}
	if op.Action == CheckAction {
		return a.checks.Apply(ctx, op)
	}
	if a.turns == nil {
		return coreadapter.OperationResult{}, errors.New("no runner adapter is configured")
	}
	return a.turns.Apply(ctx, op)
}

func (s *Service) roleExecution(cfg *config.Config, role string) (coreadapter.Profile, coreadapter.ExecutionSettings, error) {
	state, _ := s.effective()
	name := state.Profiles[role]
	if name == "" {
		name = cfg.Roles[role].Profile
	}
	p, ok := cfg.Profiles[name]
	if !ok {
		return coreadapter.Profile{}, coreadapter.ExecutionSettings{}, fmt.Errorf("unknown profile %q", name)
	}
	timeout, err := time.ParseDuration(p.Timeout)
	if err != nil {
		return coreadapter.Profile{}, coreadapter.ExecutionSettings{}, err
	}
	r := cfg.Roles[role]
	return coreadapter.Profile{Name: name, Backend: p.Agent, Model: p.Model, Effort: p.Effort, Timeout: timeout, MaxTurns: p.MaxTurns, CostLimitUSD: cfg.Budget.SessionLimitUSD()}, coreadapter.ExecutionSettings{Mode: r.Sandbox, Image: r.Image, CPUs: r.CPUs, Memory: r.Memory, Skills: slices.Clone(r.Skills)}, nil
}
