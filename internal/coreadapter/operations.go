package coreadapter

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/agentcore/ops"
)

// FailureReason preserves the backend diagnostic for a failed execution,
// including failures that occur before a session ID exists.
func FailureReason(r SessionResult) string {
	reason := ops.InfraReason(&agent.Result{TimedOut: r.TimedOut, IsError: r.IsError, ExitCode: r.ExitCode, ErrorSubtype: r.ErrorSubtype, ResultText: r.FinalResponse})
	if detail := strings.TrimSpace(r.FinalResponse); detail != "" {
		return reason + ": " + detail
	}
	return reason
}

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
		kind, decision.Reason = ops.FailureBehavioural, "reported an outcome the role does not allow"
	case kind == ops.FailureInfra:
		decision.Kind = Infrastructure
		decision.Reason = ops.InfraReason(raw)
	}
	retryable := kind == ops.FailureInfra && !r.Cancelled && !errors.Is(req.Err, context.Canceled) && !errors.Is(req.Err, ErrUnsupported) && r.Records == 0
	if next := (ops.RetryPolicy{Retries: req.MaxRetries, Delay: req.Delay}).Decide(req.Attempt, retryable); next.Retry {
		decision.Retry, decision.Delay = true, next.Delay
	} else if retryable && req.FallbackProfile != "" {
		decision.Retry, decision.Delay, decision.FallbackProfile = true, req.Delay, req.FallbackProfile
	}
	return decision, nil
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
