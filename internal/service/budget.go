package service

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

type budgetSignals struct {
	s          *Service
	repository *trace.Repository
}

func (b budgetSignals) Pass(ctx context.Context) error {
	limitText := b.s.current().Budget.PerUnit
	if limitText == "" {
		return nil
	}
	limit, ok := new(big.Rat).SetString(limitText)
	if !ok {
		return fmt.Errorf("invalid per-unit budget %q", limitText)
	}
	streams, err := b.repository.Workstreams()
	if err != nil {
		return err
	}
	for _, stream := range streams {
		if err := b.check(ctx, stream, limit, limitText); err != nil {
			return err
		}
	}
	return nil
}

func (b budgetSignals) check(ctx context.Context, stream config.WorkstreamID, limit *big.Rat, limitText string) error {
	state, err := b.repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return err
	}
	if state.Value != "building" && state.Value != "assembled" {
		return nil
	}
	costs, err := trace.Read[trace.Cost](b.repository, stream)
	if err != nil {
		return err
	}
	known := new(big.Rat)
	unknown := 0
	precision := 0
	for _, cost := range costs {
		if !cost.Entry.Usage.CostKnown {
			unknown++
			continue
		}
		amountText := strconv.FormatFloat(cost.Entry.Usage.CostUSD, 'f', -1, 64)
		if dot := strings.IndexByte(amountText, '.'); dot >= 0 {
			precision = max(precision, len(amountText)-dot-1)
		}
		amount, ok := new(big.Rat).SetString(amountText)
		if !ok {
			return fmt.Errorf("invalid recorded cost %s", cost.ID)
		}
		known.Add(known, amount)
	}
	if known.Cmp(limit) <= 0 {
		return nil
	}
	current, doc, found, err := seal.Latest(b.repository, stream)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("building workstream %s has no seal", stream)
	}
	spend := known.FloatString(precision)
	if precision > 0 {
		spend = strings.TrimRight(strings.TrimRight(spend, "0"), ".")
	}
	if spend == "" {
		spend = "0"
	}
	reason := fmt.Sprintf("Workstream %s has known spend of at least USD %s across recorded attempts, exceeding its USD %s per-unit budget", stream, spend, limitText)
	if unknown > 0 {
		reason += fmt.Sprintf("; %d attempt(s) have unknown cost, so actual spend may be higher", unknown)
	}
	_, err = b.repository.FileBudgetAmendment(ctx, stream, trace.AmendmentRequest{
		Citations: []string{"plan"}, Change: fmt.Sprintf("Reassess the plan and budget for workstream %s", stream), Reason: reason,
		Seal: current.Seal, SealRevision: doc.Revision, SpecHash: current.SpecHash,
	}, b.s.now())
	if errors.Is(err, trace.ErrConflict) {
		return nil
	}
	return err
}
