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
	spend, err := sumCosts(costs)
	if err != nil {
		return err
	}
	if spend.known.Cmp(limit) <= 0 {
		return nil
	}
	current, doc, found, err := seal.Latest(b.repository, stream)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("building workstream %s has no seal", stream)
	}
	reason := fmt.Sprintf("Workstream %s has known spend of at least USD %s across recorded attempts, exceeding its USD %s per-unit budget", stream, spend, limitText)
	if spend.unknown > 0 {
		reason += fmt.Sprintf("; %d attempt(s) have unknown cost, so actual spend may be higher", spend.unknown)
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

// spendTotal is the exact sum of known costs and the number of costs that are
// unknown, which make the sum a lower bound.
type spendTotal struct {
	known     *big.Rat
	precision int
	unknown   int
}

// sumCosts adds the known costs exactly, as the decimals they print as.
func sumCosts(costs []trace.Cost) (spendTotal, error) {
	t := spendTotal{known: new(big.Rat)}
	for _, cost := range costs {
		if !cost.Entry.Usage.CostKnown {
			t.unknown++
			continue
		}
		amountText := strconv.FormatFloat(cost.Entry.Usage.CostUSD, 'f', -1, 64)
		if dot := strings.IndexByte(amountText, '.'); dot >= 0 {
			t.precision = max(t.precision, len(amountText)-dot-1)
		}
		amount, ok := new(big.Rat).SetString(amountText)
		if !ok {
			return spendTotal{}, fmt.Errorf("invalid recorded cost %s", cost.ID)
		}
		t.known.Add(t.known, amount)
	}
	return t, nil
}

// String prints the known sum as a decimal without trailing zeros.
func (t spendTotal) String() string {
	spend := t.known.FloatString(t.precision)
	if t.precision > 0 {
		spend = strings.TrimRight(strings.TrimRight(spend, "0"), ".")
	}
	if spend == "" {
		spend = "0"
	}
	return spend
}
