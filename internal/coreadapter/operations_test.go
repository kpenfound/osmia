package coreadapter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
		{"recorded state", SessionResult{ExitCode: 1, Records: 1}, nil, Infrastructure, false},
		{"invalid outcome", SessionResult{IsError: true, ErrorSubtype: invalidOutcome}, errors.New("status \"x\" is not valid"), Behavioural, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := RetryRequest{Result: tt.result, Err: tt.err, Attempt: 1, MaxRetries: 2, Delay: 24 * time.Hour, FallbackProfile: "other-backend"}
			got, err := (RetryAdapter{}).Decide(context.Background(), req)
			if err != nil || got.Kind != tt.kind || got.Retry != tt.retry {
				t.Fatalf("%+v %v", got, err)
			}
			if tt.retry && (got.Delay != req.Delay || got.FallbackProfile != "") {
				t.Fatalf("lost caller policy: %+v", got)
			}
			req.Attempt = 3
			got, err = (RetryAdapter{}).Decide(context.Background(), req)
			if err != nil || got.Retry != tt.retry || tt.retry && (got.FallbackProfile != req.FallbackProfile || got.Delay != req.Delay) || !tt.retry && (got.FallbackProfile != "" || got.Delay != 0) {
				t.Fatalf("fallback after same-profile retries: %+v %v", got, err)
			}
			req.FallbackProfile = ""
			got, err = (RetryAdapter{}).Decide(context.Background(), req)
			if err != nil || got.Retry || got.FallbackProfile != "" || got.Delay != 0 {
				t.Fatalf("exhausted: %+v %v", got, err)
			}
		})
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

func TestRetryAdviceHonoursPreCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (RetryAdapter{}).Decide(ctx, RetryRequest{Attempt: 1}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
