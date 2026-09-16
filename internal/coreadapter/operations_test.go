package coreadapter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryAdvice(t *testing.T) {
	tests := []struct {
		name   string
		result SessionResult
		err    error
		kind   FailureKind
		retry  bool
	}{
		{"reported despite crash", SessionResult{Outcome: &Outcome{Status: "waiting"}, ExitCode: 1}, errors.New("transport"), Behavioural, false},
		{"clean no outcome", SessionResult{}, nil, Behavioural, false},
		{"timeout", SessionResult{TimedOut: true}, nil, Infrastructure, true},
		{"crash", SessionResult{ExitCode: 1}, nil, Infrastructure, true},
		{"signal", SessionResult{Signal: 9}, nil, Infrastructure, true},
		{"provider", SessionResult{Limit: &ProviderLimit{Kind: "daily"}}, nil, Infrastructure, true},
		{"text limit", SessionResult{FinalResponse: "rate limit"}, nil, Infrastructure, true},
		{"transport", SessionResult{}, errors.New("transport"), Infrastructure, true},
		{"cancel", SessionResult{}, context.Canceled, Infrastructure, false},
		{"unsupported", SessionResult{}, unsupported("backend", "missing"), Infrastructure, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := RetryRequest{Result: tt.result, Err: tt.err, Attempt: 1, MaxRetries: 2, Delay: 24 * time.Hour, FallbackProfile: "other-backend"}
			got, err := (RetryAdapter{}).Decide(context.Background(), req)
			if err != nil || got.Kind != tt.kind || got.Retry != tt.retry {
				t.Fatalf("%+v %v", got, err)
			}
			if tt.retry && (got.Delay != req.Delay || got.FallbackProfile != req.FallbackProfile) {
				t.Fatalf("lost caller policy: %+v", got)
			}
			req.Attempt = 3
			got, err = (RetryAdapter{}).Decide(context.Background(), req)
			if err != nil || got.Retry || got.FallbackProfile != "" || got.Delay != 0 {
				t.Fatalf("exhausted: %+v %v", got, err)
			}
		})
	}
}

func TestLedgerIdentityWindowAndUnknown(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.FixedZone("test", 3600))
	ledger := NewLedger(t.TempDir(), func() time.Time { return now })
	scope := Scope{Project: "../project", Workstream: "流/a:b", Unit: "unit", Thread: "thread", Turn: "turn", Role: "committee"}
	entries := []LedgerEntry{
		{Scope: scope, AttemptID: "first", At: now.Add(-25 * time.Hour), Usage: Usage{9, true, 1}},
		{Scope: scope, AttemptID: "second", Usage: Usage{2, true, 3}},
		{Scope: scope, AttemptID: "unknown", Usage: Usage{7, false, 1}},
		{Scope: Scope{Project: "other", Workstream: "else"}, AttemptID: "other", Usage: Usage{4, true, 2}},
	}
	for _, e := range entries {
		if err := ledger.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := ledger.Read(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 || rows[1].Scope != scope || rows[1].AttemptID != "second" || !rows[1].At.Equal(now) || rows[1].At.Location() != time.UTC {
		t.Fatalf("rows: %+v", rows)
	}
	got, err := ledger.Spend(ctx, SpendQuery{Since: now.Add(-24 * time.Hour), Workstreams: []string{scope.Workstream, scope.Workstream}})
	if err != nil || got != (Spend{2, 1}) {
		t.Fatalf("%+v %v", got, err)
	}
	all, err := NewLedger(ledger.core.Dir, nil).Spend(ctx, SpendQuery{})
	if err != nil || all != (Spend{15, 1}) {
		t.Fatalf("reopen: %+v %v", all, err)
	}
}

func TestLedgerFailsClosed(t *testing.T) {
	for name, corruption := range map[string]string{"malformed": "{broken\n", "null": "null\n", "missing fields": "{}\n", "oversized": strings.Repeat("x", (1<<20)+1) + "\n"} {
		t.Run(name, func(t *testing.T) {
			ledger := NewLedger(t.TempDir(), func() time.Time { return time.Unix(123, 0) })
			if err := ledger.Append(context.Background(), LedgerEntry{AttemptID: "valid", Usage: Usage{2, true, 1}}); err != nil {
				t.Fatal(err)
			}
			file, err := os.OpenFile(ledger.core.LedgerPath(), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = file.WriteString(corruption); err != nil {
				t.Fatal(err)
			}
			if err = file.Close(); err != nil {
				t.Fatal(err)
			}
			got, err := ledger.Spend(context.Background(), SpendQuery{})
			if err == nil || got != (Spend{}) {
				t.Fatalf("misleading partial total: %+v %v", got, err)
			}
		})
	}
	ledger := NewLedger(t.TempDir(), nil)
	if err := os.Mkdir(ledger.core.LedgerPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	if got, err := ledger.Spend(context.Background(), SpendQuery{}); err == nil || got != (Spend{}) {
		t.Fatalf("read failure: %+v %v", got, err)
	}
	if err := ledger.Append(context.Background(), LedgerEntry{AttemptID: "x"}); err == nil {
		t.Fatal("append failure hidden")
	}
}

func TestBudgetHysteresis(t *testing.T) {
	cases := []struct {
		cost     float64
		previous bool
		want     BudgetResult
	}{
		{10, false, BudgetResult{Reached: true, Crossed: true, UnknownCosts: 2}},
		{7, true, BudgetResult{Reached: true, UnknownCosts: 2}},
		{6.99, true, BudgetResult{Released: true, UnknownCosts: 2}},
		{9, false, BudgetResult{UnknownCosts: 2}},
	}
	for _, tt := range cases {
		got, err := (BudgetAdapter{}).Evaluate(context.Background(), BudgetRequest{Spend: Spend{tt.cost, 2}, LimitUSD: 10, ResumePercent: 70, PreviouslyReached: tt.previous})
		if err != nil || got != tt.want {
			t.Fatalf("%+v != %+v: %v", got, tt.want, err)
		}
	}
}

func TestCapacityAtomicReleaseAndCallerOrder(t *testing.T) {
	ctx := context.Background()
	capacity, err := NewCapacity(map[string]int{"a": 2, "b": 1, "disabled": 0})
	if err != nil {
		t.Fatal(err)
	}
	claim := func(slots map[string]int) Claim {
		t.Helper()
		got, err := capacity.TryClaim(ctx, ClaimRequest{Slots: slots})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	first := claim(map[string]int{"b": 1})
	if !first.Acquired {
		t.Fatal("first")
	}
	for range 20 {
		if claim(map[string]int{"a": 2, "b": 1}).Acquired {
			t.Fatal("partial claim acquired")
		}
	}
	second := claim(map[string]int{"a": 2})
	if !second.Acquired {
		t.Fatal("failed claims leaked slots")
	}
	if claim(map[string]int{"a": 1}).Acquired {
		t.Fatal("overallocated")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := second.Lease.Release(canceled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if claim(map[string]int{"a": 1}).Acquired {
		t.Fatal("cancelled release lost lease")
	}
	for range 2 {
		if err := second.Lease.Release(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// Refused callers leave no FIFO membership ahead of this new claim.
	third := claim(map[string]int{"a": 2})
	if !third.Acquired {
		t.Fatal("refusal reserved ordering")
	}
	if claim(map[string]int{"disabled": 1}).Acquired {
		t.Fatal("zero limit became one")
	}
	if _, err := capacity.TryClaim(canceled, ClaimRequest{Slots: map[string]int{"b": 1}}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := capacity.TryClaim(ctx, ClaimRequest{Slots: map[string]int{"missing": 1}}); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	if err := first.Lease.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if err := third.Lease.Release(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentCapacity(t *testing.T) {
	capacity, _ := NewCapacity(map[string]int{"slot": 1})
	var acquired atomic.Int32
	var wg sync.WaitGroup
	leases := make(chan Lease, 50)
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := capacity.TryClaim(context.Background(), ClaimRequest{Slots: map[string]int{"slot": 1}})
			if err != nil {
				t.Error(err)
			}
			if c.Acquired {
				acquired.Add(1)
				leases <- c.Lease
			}
		}()
	}
	wg.Wait()
	close(leases)
	if acquired.Load() != 1 {
		t.Fatal(acquired.Load())
	}
	for l := range leases {
		if err := l.Release(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWakeCoalescingReconcilesPersistedState(t *testing.T) {
	ctx := context.Background()
	wake := NewWakeups()
	path := filepath.Join(t.TempDir(), "state")
	for _, state := range []string{"first", "second", "latest"} {
		if err := os.WriteFile(path, []byte(state), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := wake.Notify(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(wake.wake) != 1 {
		t.Fatal("wake did not coalesce")
	}
	if err := wake.Wait(ctx, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "latest" {
		t.Fatalf("%s %v", data, err)
	}
	// No event is required: the periodic tick observes an unnotified update.
	if err := os.WriteFile(path, []byte("unnotified"), 0o600); err != nil {
		t.Fatal(err)
	}
	ticks := make(chan time.Time, 1)
	ticks <- time.Now()
	if err := wake.Wait(ctx, ticks); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if string(data) != "unnotified" {
		t.Fatal("reconcile lost state")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if !errors.Is(wake.Wait(canceled, nil), context.Canceled) || !errors.Is(wake.Notify(canceled), context.Canceled) {
		t.Fatal("ignored cancellation")
	}
}

func TestOperationalPreCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ledger := NewLedger(t.TempDir(), nil)
	_, retryErr := (RetryAdapter{}).Decide(ctx, RetryRequest{Attempt: 1})
	_, budgetErr := (BudgetAdapter{}).Evaluate(ctx, BudgetRequest{})
	_, readErr := ledger.Read(ctx, time.Time{})
	appendErr := ledger.Append(ctx, LedgerEntry{AttemptID: "x"})
	for _, err := range []error{retryErr, budgetErr, readErr, appendErr} {
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(ledger.core.LedgerPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled append wrote")
	}
}
