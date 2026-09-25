package thread

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// chain is a fallback chain default -> backup -> last, where last crosses to
// another backend.
var chain = map[string]coreadapter.Profile{
	"default": {Name: "backup", Backend: "fake", Model: "smaller"},
	"backup":  {Name: "last", Backend: "other", Model: "other"},
}

// crashed is a started session that failed with an infrastructure failure.
func crashed(p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
	return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: p.Profile.Backend, ID: "crashed"}, FinalResponse: "partial", Usage: coreadapter.Usage{Turns: 1, CostUSD: .1, CostKnown: true}, ExitCode: 1}, nil
}

func healthy(p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
	return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: p.Profile.Backend, ID: "healthy"}, FinalResponse: "done"}, nil
}

func profiles(q trace.QueuedTurn) []string {
	var names []string
	for _, a := range q.Attempts {
		names = append(names, a.Profile.Name)
	}
	return names
}

func TestInfrastructureFailuresRetryThenFollowFallbacks(t *testing.T) {
	ctx := context.Background()
	repo, root, p := setup(t)
	queue(t, repo, "first")
	var ran []string
	runner := Runner{Store: repo, Now: func() time.Time { return timestamp.Add(time.Second) }, MaxRetries: 2, Fallbacks: chain, Turns: fakeTurns(func(_ context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		ran = append(ran, p.Profile.Name)
		if p.Profile.Name == "last" {
			return healthy(p)
		}
		return crashed(p)
	})}
	q, err := runner.RunNext(ctx, stream, "agent", coreadapter.PreparedTurn{SessionDirectory: "/owned/first"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"default", "default", "default", "backup", "backup", "backup", "last"}
	if !reflect.DeepEqual(ran, want) || !reflect.DeepEqual(profiles(q), want) {
		t.Fatalf("ran %v, attempts %v; want %v", ran, profiles(q), want)
	}
	for i, a := range q.Attempts[:6] {
		if a.FailureClass != coreadapter.Infrastructure || a.Failure == "" || a.Result == nil || a.Result.ExitCode != 1 {
			t.Fatalf("attempt %d: %+v", i+1, a)
		}
	}
	for i, reason := range []string{"retry 1 of 2 on profile default", "retry 2 of 2 on profile default", "fallback from profile default to backup after 3 infrastructure failures", "retry 1 of 2 on profile backup", "retry 2 of 2 on profile backup", "fallback from profile backup to last after 3 infrastructure failures"} {
		if a := q.Attempts[i+1]; !strings.HasPrefix(a.Reason, reason) {
			t.Fatalf("attempt %d reason %q, want %q", a.Number, a.Reason, reason)
		}
	}
	if last := q.Attempts[6]; last.Failure != "" || last.FailureClass != "" || last.Path != "replay" || q.Response.Failure != "" || q.Status() != "idle" {
		t.Fatalf("final attempt %+v, response %+v", last, q.Response)
	}
	repo.Close()
	repo, err = trace.Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	th, err := repo.Thread(stream, "agent")
	if err != nil || !reflect.DeepEqual(th.Turns[0].Attempts, q.Attempts) {
		t.Fatalf("durable attempts: %+v %v", th, err)
	}
}

func TestProviderLimitSkipsSameBackendWithoutRetry(t *testing.T) {
	repo, _, _ := setup(t)
	queue(t, repo, "first")
	var ran, recorded []string
	runner := Runner{Store: repo, Now: func() time.Time { return timestamp.Add(time.Second) }, MaxRetries: 2, Fallbacks: chain,
		OnProviderLimit: func(p coreadapter.Profile, limit coreadapter.ProviderLimit) error {
			recorded = append(recorded, p.Name+":"+limit.Status)
			return nil
		},
		Turns: fakeTurns(func(_ context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
			ran = append(ran, p.Profile.Name)
			if p.Profile.Name == "last" {
				return healthy(p)
			}
			return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: p.Profile.Backend, ID: "limited"}, Limit: &coreadapter.ProviderLimit{Status: "blocked"}}, nil
		}),
	}
	q, err := runner.RunNext(context.Background(), stream, "agent", coreadapter.PreparedTurn{SessionDirectory: "/owned"})
	if err != nil || !reflect.DeepEqual(ran, []string{"default", "last"}) || !reflect.DeepEqual(recorded, []string{"default:blocked"}) || q.Response.Failure != "" {
		t.Fatalf("runs %v, recorded %v, response %+v: %v", ran, recorded, q.Response, err)
	}
}

func TestExhaustedFallbackChainFailsTheTurnOnce(t *testing.T) {
	ctx := context.Background()
	repo, root, p := setup(t)
	queue(t, repo, "first")
	calls := 0
	d := Dispatcher{
		Runner: Runner{Store: repo, Now: func() time.Time { return timestamp.Add(time.Second) }, MaxRetries: 1, Fallbacks: chain, Turns: fakeTurns(func(_ context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
			calls++
			return coreadapter.SessionResult{TimedOut: true}, context.DeadlineExceeded
		})},
		Prepare: func(context.Context, TurnInput) (coreadapter.PreparedTurn, error) {
			return coreadapter.PreparedTurn{SessionDirectory: "/owned/first"}, nil
		},
	}
	op := turnOp(t, "first")
	result, err := d.Apply(ctx, op)
	if err != nil || result.Outcome != "failed" {
		t.Fatalf("apply: %+v %v", result, err)
	}
	var data TurnResult
	if err := json.Unmarshal(result.Data, &data); err != nil || data.Attempts != 6 || calls != 6 {
		t.Fatalf("attempts %+v, calls %d: %v", data, calls, err)
	}
	th, err := repo.Thread(stream, "agent")
	if err != nil {
		t.Fatal(err)
	}
	q := th.Turns[0]
	if want := []string{"default", "default", "backup", "backup", "last", "last"}; !reflect.DeepEqual(profiles(q), want) {
		t.Fatalf("attempt profiles %v, want %v", profiles(q), want)
	}
	for _, a := range q.Attempts {
		if a.FailureClass != coreadapter.Infrastructure || !a.Result.TimedOut {
			t.Fatalf("attempt %+v", a)
		}
	}
	if q.Response.FailureClass != coreadapter.Infrastructure || q.Response.Failure != q.Attempts[5].Failure || q.Status() != "failed" {
		t.Fatalf("response %+v", q.Response)
	}

	// Reconciliation after a restart completes from the durable record and
	// never repeats an attempt.
	repo.Close()
	repo, err = trace.Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	d.Runner.Store = repo
	d.Runner.Turns = fakeTurns(func(context.Context, coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		t.Fatal("a completed attempt ran again")
		return coreadapter.SessionResult{}, nil
	})
	observe(t, d, op, coreadapter.EffectCompleted)
	if again, err := d.Apply(ctx, op); err != nil || !reflect.DeepEqual(again, result) {
		t.Fatalf("reapply: %+v %v", again, err)
	}
	th, err = repo.Thread(stream, "agent")
	if err != nil || !reflect.DeepEqual(th.Turns[0].Attempts, q.Attempts) {
		t.Fatalf("durable attempts: %+v %v", th, err)
	}
}

func TestBehaviouralFailureIsNotRetried(t *testing.T) {
	for _, mode := range []string{"invalid-outcome", "reported"} {
		t.Run(mode, func(t *testing.T) {
			repo, _, _ := setup(t)
			queue(t, repo, "first")
			calls := 0
			runner := Runner{Store: repo, Now: func() time.Time { return timestamp.Add(time.Second) }, MaxRetries: 2, Fallbacks: chain, Turns: fakeTurns(func(_ context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
				calls++
				result := coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "ran"}, IsError: true}
				if mode == "reported" {
					result.Outcome = &coreadapter.Outcome{Status: "done", Report: "{}"}
					return result, errors.New("transport closed after the report")
				}
				result.ErrorSubtype = "invalid_outcome"
				return result, errors.New(`status "shipped" is not valid for mason`)
			})}
			q, err := runner.RunNext(context.Background(), stream, "agent", coreadapter.PreparedTurn{SessionDirectory: "/owned"})
			if err == nil || calls != 1 || len(q.Attempts) != 1 {
				t.Fatalf("retried behavioural failure: %d %+v %v", calls, q.Attempts, err)
			}
			if a := q.Attempts[0]; a.FailureClass != coreadapter.Behavioural || q.Response.FailureClass != coreadapter.Behavioural || q.Status() != "failed" {
				t.Fatalf("attempt %+v, response %+v", a, q.Response)
			}
		})
	}
}

func TestFallbackNeverReturnsToATriedProfile(t *testing.T) {
	repo, _, _ := setup(t)
	queue(t, repo, "first")
	var ran []string
	cycle := map[string]coreadapter.Profile{"default": {Name: "backup", Backend: "fake", Model: "x"}, "backup": {Name: "default", Backend: "fake", Model: "test"}}
	runner := Runner{Store: repo, Now: func() time.Time { return timestamp.Add(time.Second) }, Fallbacks: cycle, Turns: fakeTurns(func(_ context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		ran = append(ran, p.Profile.Name)
		return crashed(p)
	})}
	q, err := runner.RunNext(context.Background(), stream, "agent", coreadapter.PreparedTurn{SessionDirectory: "/owned"})
	if err != nil || !reflect.DeepEqual(ran, []string{"default", "backup"}) || q.Response.FailureClass != coreadapter.Infrastructure {
		t.Fatalf("ran %v: %+v %v", ran, q.Response, err)
	}
}

func TestCancelledAttemptIsNotRetried(t *testing.T) {
	repo, _, _ := setup(t)
	queue(t, repo, "first")
	calls := 0
	runner := Runner{Store: repo, Now: func() time.Time { return timestamp.Add(time.Second) }, MaxRetries: 1, Fallbacks: chain}
	runner.Turns = fakeTurns(func(context.Context, coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		calls++
		return coreadapter.SessionResult{}, errors.Join(coreadapter.ErrNotStarted, context.Canceled)
	})
	q, err := runner.RunNext(context.Background(), stream, "agent", coreadapter.PreparedTurn{SessionDirectory: "/owned"})
	if err == nil || calls != 1 || len(q.Attempts) != 1 || q.CompletedAt.IsZero() || q.Status() != "interrupted" {
		t.Fatalf("retried cancelled attempt: %d %#v %v", calls, q, err)
	}
}
