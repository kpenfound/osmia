package coreadapter

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/busybees/core/ops"
)

type RetryAdapter struct{}

var _ Retries = RetryAdapter{}

func (RetryAdapter) Decide(ctx context.Context, req RetryRequest) (RetryDecision, error) {
	if err := ctx.Err(); err != nil {
		return RetryDecision{}, err
	}
	if req.Attempt < 1 || req.MaxRetries < 0 || req.Delay < 0 {
		return RetryDecision{}, errors.New("invalid retry counts or delay")
	}
	r := req.Result
	raw := &agent.Result{TimedOut: r.TimedOut, IsError: r.IsError || req.Err != nil || r.Cancelled || r.Signal != 0 || r.Limit != nil, ExitCode: r.ExitCode, ErrorSubtype: r.ErrorSubtype, ResultText: r.FinalResponse}
	if r.Outcome != nil {
		raw.HasOutcome = true
		raw.Outcome = agent.Outcome{Status: r.Outcome.Status, Note: r.Outcome.Report}
	}
	kind := ops.ClassifyFailure(raw)
	decision := RetryDecision{Kind: Behavioural, Reason: "reported outcome or clean exit without outcome"}
	switch {
	case r.ErrorSubtype == invalidOutcome:
		// The session ran and reported; the role may not report that status.
		kind, decision.Reason = ops.FailureBehavioural, "reported an outcome the role does not allow"
	case kind == ops.FailureInfra:
		decision.Kind = Infrastructure
		decision.Reason = ops.InfraReason(raw)
	}
	// Cancellation and unsupported execution need caller intervention, not retries.
	retryable := kind == ops.FailureInfra && !r.Cancelled && !errors.Is(req.Err, context.Canceled) && !errors.Is(req.Err, ErrUnsupported)
	if next := (ops.RetryPolicy{Retries: req.MaxRetries, Delay: req.Delay}).Decide(req.Attempt, retryable); next.Retry {
		decision.Retry, decision.Delay = true, next.Delay
	} else if retryable && req.FallbackProfile != "" {
		decision.Retry, decision.Delay, decision.FallbackProfile = true, req.Delay, req.FallbackProfile
	}
	return decision, nil
}

type BudgetAdapter struct{}

var _ Budgets = BudgetAdapter{}

func (BudgetAdapter) Evaluate(ctx context.Context, req BudgetRequest) (BudgetResult, error) {
	if err := ctx.Err(); err != nil {
		return BudgetResult{}, err
	}
	if !nonnegative(req.Spend.CostUSD) || !nonnegative(req.LimitUSD) || !nonnegative(req.ResumePercent) || req.ResumePercent > 100 || req.Spend.UnknownCosts < 0 {
		return BudgetResult{}, errors.New("invalid budget values")
	}
	// Spend is already selected by the caller, so a zero-width window includes it.
	at := time.Time{}
	signal := ops.EvaluateWindow([]ops.LedgerEntry{{Time: at, CostUSD: req.Spend.CostUSD}}, at, 0, req.LimitUSD, req.ResumePercent, req.PreviouslyReached)
	return BudgetResult{signal.Reached, signal.Crossed, signal.Released, req.Spend.UnknownCosts}, nil
}

func nonnegative(v float64) bool { return v >= 0 && !math.IsInf(v, 0) && !math.IsNaN(v) }

// CapacityAdapter owns private pools; callers supply every limit and claim order.
type CapacityAdapter struct {
	mu    sync.Mutex
	pools map[string]*ops.SharedPool
}

var _ Capacity = (*CapacityAdapter)(nil)

func NewCapacity(limits map[string]int) (*CapacityAdapter, error) {
	a := &CapacityAdapter{pools: make(map[string]*ops.SharedPool)}
	for name, size := range limits {
		if name == "" || size < 0 {
			return nil, errors.New("invalid capacity limit")
		}
		if size == 0 {
			a.pools[name] = nil
		} else {
			a.pools[name] = ops.NewSharedPool(size)
		}
	}
	return a, nil
}

func (a *CapacityAdapter) TryClaim(ctx context.Context, req ClaimRequest) (Claim, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Claim{}, err
	}
	if len(req.Slots) == 0 {
		return Claim{}, errors.New("empty capacity claim")
	}
	for name, n := range req.Slots {
		if _, ok := a.pools[name]; !ok {
			return Claim{}, unsupported("capacity pool", name)
		}
		if n < 1 {
			return Claim{}, fmt.Errorf("invalid claim for %s", name)
		}
	}
	releases := []func(){}
	rollback := func() {
		for _, release := range releases {
			release()
		}
	}
	for name, n := range req.Slots {
		pool := a.pools[name]
		if pool == nil || n > pool.Size() {
			rollback()
			return Claim{}, nil
		}
		member := pool.Join(req.Scope.Workstream, func() {})
		acquired := member.Acquire(n)
		member.Leave() // A refusal must not reserve ordering for a future caller pass.
		if !acquired {
			rollback()
			return Claim{}, nil
		}
		releases = append(releases, func() { member.Release(n) })
	}
	if err := ctx.Err(); err != nil {
		rollback()
		return Claim{}, err
	}
	return Claim{Acquired: true, Lease: &releaseLease{release: func(ctx context.Context) error {
		a.mu.Lock()
		defer a.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		rollback()
		return nil
	}}}, nil
}

// WakeAdapter coalesces hints for one controller; Wait consumes at most one hint.
type WakeAdapter struct{ wake ops.Wake }

var _ Wakeups = (*WakeAdapter)(nil)

func NewWakeups() *WakeAdapter { return &WakeAdapter{wake: ops.NewWake()} }
func (w *WakeAdapter) Notify(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w.wake.Signal()
	return nil
}
func (w *WakeAdapter) Wait(ctx context.Context, tick <-chan time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-tick:
		w.wake.Drain()
	case <-w.wake:
	}
	return ctx.Err()
}
