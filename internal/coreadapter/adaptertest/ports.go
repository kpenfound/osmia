package adaptertest

import (
	"context"
	"sync"
	"time"

	a "github.com/kpenfound/osmia/internal/coreadapter"
)

type Turns struct {
	Script[a.PreparedTurn, a.SessionResult]
}

func (f *Turns) Run(ctx context.Context, req a.PreparedTurn) (a.SessionResult, error) {
	return f.Call(ctx, req)
}

var _ a.Turns = (*Turns)(nil)

type Workspaces struct {
	Script[a.WorkspaceRequest, a.WorkspaceLease]
}

func (f *Workspaces) Acquire(ctx context.Context, req a.WorkspaceRequest) (a.WorkspaceLease, error) {
	return f.Call(ctx, req)
}

var _ a.Workspaces = (*Workspaces)(nil)

type Sandboxes struct {
	Script[a.SandboxRequest, a.SandboxLease]
}

func (f *Sandboxes) Prepare(ctx context.Context, req a.SandboxRequest) (a.SandboxLease, error) {
	return f.Call(ctx, req)
}

var _ a.Sandboxes = (*Sandboxes)(nil)

type MCPHosts struct {
	Script[a.HostRequest, a.HostedMCP]
}

func (f *MCPHosts) Host(ctx context.Context, req a.HostRequest) (a.HostedMCP, error) {
	return f.Call(ctx, req)
}

var _ a.MCPHosts = (*MCPHosts)(nil)

type Reviews struct {
	Script[a.ReviewRequest, a.ReviewResult]
}

func (f *Reviews) Review(ctx context.Context, req a.ReviewRequest) (a.ReviewResult, error) {
	return f.Call(ctx, req)
}

var _ a.Reviews = (*Reviews)(nil)

type Retries struct {
	Script[a.RetryRequest, a.RetryDecision]
}

func (f *Retries) Decide(ctx context.Context, req a.RetryRequest) (a.RetryDecision, error) {
	return f.Call(ctx, req)
}

var _ a.Retries = (*Retries)(nil)

type Budgets struct {
	Script[a.BudgetRequest, a.BudgetResult]
}

func (f *Budgets) Evaluate(ctx context.Context, req a.BudgetRequest) (a.BudgetResult, error) {
	return f.Call(ctx, req)
}

var _ a.Budgets = (*Budgets)(nil)

type Capacity struct {
	Script[a.ClaimRequest, a.Claim]
}

func (f *Capacity) TryClaim(ctx context.Context, req a.ClaimRequest) (a.Claim, error) {
	return f.Call(ctx, req)
}

var _ a.Capacity = (*Capacity)(nil)

type Ledger struct {
	Appends Script[a.LedgerEntry, struct{}]
	Totals  Script[a.SpendQuery, a.Spend]
}

func (f *Ledger) Append(ctx context.Context, req a.LedgerEntry) error {
	_, err := f.Appends.Call(ctx, req)
	return err
}
func (f *Ledger) Spend(ctx context.Context, req a.SpendQuery) (a.Spend, error) {
	return f.Totals.Call(ctx, req)
}

var _ a.Ledger = (*Ledger)(nil)

// Wakeups scripts wake operations without timers or goroutines; it records the
// supplied tick channel but deliberately does not implement a real wake bus.
type Wakeups struct {
	Notifications Script[struct{}, struct{}]
	Waits         Script[<-chan time.Time, struct{}]
}

func (f *Wakeups) Notify(ctx context.Context) error {
	_, err := f.Notifications.Call(ctx, struct{}{})
	return err
}
func (f *Wakeups) Wait(ctx context.Context, ticks <-chan time.Time) error {
	_, err := f.Waits.Call(ctx, ticks)
	return err
}

var _ a.Wakeups = (*Wakeups)(nil)

// Lease records cleanup attempts. After a successful release subsequent calls
// succeed without consuming a reply; failures can be retried with a fresh context.
type Lease struct {
	mu       sync.Mutex
	released bool
	Releases Script[struct{}, struct{}]
}

func (f *Lease) Release(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.released {
		return nil
	}
	_, err := f.Releases.Call(ctx, struct{}{})
	if err == nil {
		f.released = true
	}
	return err
}

var _ a.Lease = (*Lease)(nil)
