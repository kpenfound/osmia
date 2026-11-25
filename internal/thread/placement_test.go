package thread

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

func placedOn(client string, released *int) func(context.Context, trace.Thread, trace.QueuedTurn) (Placed, error) {
	return func(context.Context, trace.Thread, trace.QueuedTurn) (Placed, error) {
		return Placed{Client: client, Release: func() { *released++ }}, nil
	}
}

func TestATurnResumesItsSessionOnTheClientThatHoldsItAndReplaysItsLogOnAnother(t *testing.T) {
	for name, tc := range map[string]struct {
		client string
		resume bool
	}{
		"same client":    {"a", true},
		"another client": {"b", false},
		"local client":   {"local", false},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			repo, _, _ := setup(t)
			defer repo.Close()
			queue(t, repo, "first")
			released := 0
			runner := Runner{Jev: unconfiguredJudge(), Store: repo, Now: func() time.Time { return timestamp.Add(time.Second) }, ResumableRoles: func(string) bool { return true },
				Place: placedOn("a", &released),
				Turns: fakeTurns(func(context.Context, coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
					return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "saved"}, FinalResponse: "first final", Client: "a"}, nil
				})}
			if _, err := runner.RunNext(ctx, stream, "agent", coreadapter.PreparedTurn{SessionDirectory: "/owned/first"}); err != nil {
				t.Fatal(err)
			}
			if released != 1 {
				t.Fatalf("the first turn released its client slot %d times", released)
			}
			th, err := repo.Thread(stream, "agent")
			if err != nil {
				t.Fatal(err)
			}
			if client, ok := th.SessionClient(); !ok || client != "a" {
				t.Fatalf("the session is on %q %v", client, ok)
			}
			queue(t, repo, "second")
			var got coreadapter.PreparedTurn
			runner.Place = placedOn(tc.client, &released)
			runner.Turns = resumableTurns{
				check: func(context.Context, coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error {
					return nil
				},
				run: func(_ context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
					got = p
					return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "next"}, FinalResponse: "second final", Client: tc.client}, nil
				}}
			q, err := runner.RunNext(ctx, stream, "agent", coreadapter.PreparedTurn{SessionDirectory: "/owned/second"})
			if err != nil {
				t.Fatal(err)
			}
			if (got.Resume != nil) != tc.resume || (got.History != "") == tc.resume {
				t.Fatalf("resume %v, history %q on client %s", got.Resume, got.History, tc.client)
			}
			path := q.Attempts[0].Path
			if tc.resume && path != "resume" || !tc.resume && (path != "replay" || !strings.Contains(q.Attempts[0].Reason, "session is on client a")) {
				t.Fatalf("attempt: %+v", q.Attempts[0])
			}
			if released != 2 {
				t.Fatalf("slots released %d times", released)
			}
		})
	}
}

func TestATurnThatCannotBePlacedEndsInterruptedWithoutRunning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	repo, _, _ := setup(t)
	defer repo.Close()
	queue(t, repo, "first")
	ran := false
	runner := Runner{Jev: unconfiguredJudge(), Store: repo, Now: func() time.Time { return timestamp.Add(time.Second) },
		Place: func(ctx context.Context, _ trace.Thread, _ trace.QueuedTurn) (Placed, error) {
			cancel()
			return Placed{}, ctx.Err()
		},
		Turns: fakeTurns(func(context.Context, coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
			ran = true
			return coreadapter.SessionResult{}, nil
		})}
	_, err := runner.RunNext(ctx, stream, "agent", coreadapter.PreparedTurn{SessionDirectory: "/owned/first"})
	if !errors.Is(err, context.Canceled) || ran {
		t.Fatalf("ran %v, error %v", ran, err)
	}
}

func TestFallbackProfilesTheClientCannotRunAreSkippedAndRecorded(t *testing.T) {
	ctx := context.Background()
	repo, _, _ := setup(t)
	defer repo.Close()
	queue(t, repo, "first")
	var ran []string
	released := 0
	runner := Runner{Jev: unconfiguredJudge(), Store: repo, Now: func() time.Time { return timestamp.Add(time.Second) }, MaxRetries: 1, Fallbacks: chain,
		Place: func(context.Context, trace.Thread, trace.QueuedTurn) (Placed, error) {
			return Placed{Client: "a", Runs: func(profile string) bool { return profile != "backup" }, Release: func() { released++ }}, nil
		},
		Turns: fakeTurns(func(_ context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
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
	if want := []string{"default", "default", "last"}; !reflect.DeepEqual(ran, want) || !reflect.DeepEqual(profiles(q), want) {
		t.Fatalf("ran %v, attempts %v; want %v", ran, profiles(q), want)
	}
	if !strings.Contains(q.Attempts[2].Reason, "fallback from profile default to last") {
		t.Fatalf("reason %q", q.Attempts[2].Reason)
	}
	events, err := repo.ClientEvents(stream, "first")
	if err != nil {
		t.Fatal(err)
	}
	var skipped []string
	for _, e := range events {
		if e.Kind == trace.ClientSkipped {
			if e.Client != "a" {
				t.Fatalf("skip recorded for client %q", e.Client)
			}
			skipped = append(skipped, e.What)
		}
	}
	if !reflect.DeepEqual(skipped, []string{"backup"}) {
		t.Fatalf("skipped profiles %v", skipped)
	}
	if released != 1 {
		t.Fatalf("slot released %d times", released)
	}
}

func TestEveryFallbackTheClientCannotRunLeavesTheTurnFailedNotRetriedElsewhere(t *testing.T) {
	ctx := context.Background()
	repo, _, _ := setup(t)
	defer repo.Close()
	queue(t, repo, "first")
	var ran []string
	runner := Runner{Jev: unconfiguredJudge(), Store: repo, Now: func() time.Time { return timestamp.Add(time.Second) }, MaxRetries: 1, Fallbacks: chain,
		Place: func(context.Context, trace.Thread, trace.QueuedTurn) (Placed, error) {
			return Placed{Client: "a", Runs: func(profile string) bool { return profile == "default" }}, nil
		},
		Turns: fakeTurns(func(_ context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
			ran = append(ran, p.Profile.Name)
			return crashed(p)
		})}
	q, _ := runner.RunNext(ctx, stream, "agent", coreadapter.PreparedTurn{SessionDirectory: "/owned/first"})
	if want := []string{"default", "default"}; !reflect.DeepEqual(ran, want) {
		t.Fatalf("ran %v, want %v", ran, want)
	}
	if q.Response == nil || q.Response.Failure == "" {
		t.Fatalf("response %+v", q.Response)
	}
}
