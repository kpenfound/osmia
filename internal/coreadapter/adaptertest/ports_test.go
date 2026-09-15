package adaptertest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	a "github.com/kpenfound/osmia/internal/coreadapter"
)

func exercise[Q, R any](t *testing.T, s *Script[Q, R], call func(context.Context, Q) (R, error), req Q, result R) {
	t.Helper()
	failure := errors.New("provider unavailable")
	*s = *NewScript[Q, R](Reply[R]{Value: result}, Reply[R]{Value: result, Err: a.ErrUnsupported}, Reply[R]{Value: result, Err: failure})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := call(ctx, req); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if len(s.Calls()) != 0 {
		t.Fatal("cancelled call consumed script")
	}
	for _, wantErr := range []error{nil, a.ErrUnsupported, failure} {
		got, err := call(context.Background(), req)
		if !errors.Is(err, wantErr) || !reflect.DeepEqual(got, result) {
			t.Fatalf("got %#v, %v; want %#v, %v", got, err, result, wantErr)
		}
	}
	if got := s.Calls(); !reflect.DeepEqual(got, []Q{req, req, req}) {
		t.Fatalf("requests lost: %#v", got)
	}
	if _, err := call(context.Background(), req); !errors.Is(err, ErrExhausted) {
		t.Fatalf("exhaustion: %v", err)
	}
}
func TestPorts(t *testing.T) {
	t.Run("Turns", func(t *testing.T) {
		f := &Turns{}
		var port a.Turns = f
		exercise(t, &f.Script, port.Run, a.PreparedTurn{Prompt: "build", Scope: a.Scope{Thread: "thread", Turn: "turn"}, Resume: &a.BackendSession{Backend: "fake", ID: "resume"}}, a.SessionResult{FinalResponse: "done", Outcome: &a.Outcome{Status: "waiting"}, Usage: a.Usage{CostKnown: false}})
	})
	t.Run("Workspaces", func(t *testing.T) {
		f := &Workspaces{}
		var port a.Workspaces = f
		exercise(t, &f.Script, port.Acquire, a.WorkspaceRequest{Directory: "/files", Access: a.ReadOnly}, a.WorkspaceLease{Workspace: a.Workspace{Directory: "/files", Access: a.ReadOnly}})
	})
	t.Run("Sandboxes", func(t *testing.T) {
		f := &Sandboxes{}
		var port a.Sandboxes = f
		exercise(t, &f.Script, port.Prepare, a.SandboxRequest{Required: a.Isolation{DenyVCS: true, DenyInheritedEnvironment: true, DenyDeliveryCredentials: true}}, a.SandboxLease{ID: "verified", Verified: a.Isolation{DenyVCS: true}})
	})
	t.Run("MCPHosts", func(t *testing.T) {
		f := &MCPHosts{}
		var port a.MCPHosts = f
		exercise(t, &f.Script, port.Host, a.HostRequest{Scope: a.Scope{Role: "committee"}, Capabilities: a.Capabilities{Tools: []string{"verdict"}}}, a.HostedMCP{Endpoint: a.Endpoint{URL: "fake://mcp"}})
	})
	t.Run("Reviews", func(t *testing.T) {
		f := &Reviews{}
		var port a.Reviews = f
		exercise(t, &f.Script, port.Review, a.ReviewRequest{Candidate: a.Candidate{Revision: "candidate", BaseRevision: "base", SpecRevision: "spec", PlanRevision: "plan"}}, a.ReviewResult{Partial: true, Artifacts: []a.Artifact{{Kind: "brief", Path: "/artifacts/brief.json"}}})
	})
	t.Run("Retries", func(t *testing.T) {
		f := &Retries{}
		var port a.Retries = f
		exercise(t, &f.Script, port.Decide, a.RetryRequest{Attempt: 2, MaxRetries: 3, FallbackProfile: "backup"}, a.RetryDecision{Kind: a.Infrastructure, Retry: true, FallbackProfile: "backup"})
	})
	t.Run("Budgets", func(t *testing.T) {
		f := &Budgets{}
		var port a.Budgets = f
		exercise(t, &f.Script, port.Evaluate, a.BudgetRequest{LimitUSD: 5, Spend: a.Spend{UnknownCosts: 1}}, a.BudgetResult{Reached: true, UnknownCosts: 1})
	})
	t.Run("Capacity", func(t *testing.T) {
		f := &Capacity{}
		var port a.Capacity = f
		exercise(t, &f.Script, port.TryClaim, a.ClaimRequest{Slots: map[string]int{"mason": 1}}, a.Claim{Acquired: false})
	})
	t.Run("LedgerAppend", func(t *testing.T) {
		f := &Ledger{}
		var port a.Ledger = f
		exercise(t, &f.Appends, func(ctx context.Context, q a.LedgerEntry) (struct{}, error) { return struct{}{}, port.Append(ctx, q) }, a.LedgerEntry{AttemptID: "attempt"}, struct{}{})
	})
	t.Run("LedgerSpend", func(t *testing.T) {
		f := &Ledger{}
		var port a.Ledger = f
		exercise(t, &f.Totals, port.Spend, a.SpendQuery{Workstreams: []string{"stream"}}, a.Spend{CostUSD: 3, UnknownCosts: 1})
	})
	t.Run("Notify", func(t *testing.T) {
		f := &Wakeups{}
		var port a.Wakeups = f
		exercise(t, &f.Notifications, func(ctx context.Context, q struct{}) (struct{}, error) { return struct{}{}, port.Notify(ctx) }, struct{}{}, struct{}{})
	})
	t.Run("Wait", func(t *testing.T) {
		f := &Wakeups{}
		var port a.Wakeups = f
		var ticks <-chan time.Time = make(chan time.Time)
		exercise(t, &f.Waits, func(ctx context.Context, q <-chan time.Time) (struct{}, error) { return struct{}{}, port.Wait(ctx, q) }, ticks, struct{}{})
	})
}
func TestLeaseCleanup(t *testing.T) {
	failure := errors.New("cleanup unavailable")
	f := &Lease{Releases: *NewScript[struct{}, struct{}](Reply[struct{}]{Err: a.ErrUnsupported}, Reply[struct{}]{Err: failure}, Reply[struct{}]{})}
	var lease a.Lease = f
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := lease.Release(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, want := range []error{a.ErrUnsupported, failure, nil, nil} {
		if err := lease.Release(context.WithoutCancel(ctx)); !errors.Is(err, want) {
			t.Fatalf("release: %v, want %v", err, want)
		}
	}
	if len(f.Releases.Calls()) != 3 {
		t.Fatal("successful cleanup must be idempotent")
	}
}
