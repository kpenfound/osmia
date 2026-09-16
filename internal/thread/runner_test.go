package thread

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

const project config.ProjectID = "p_00000000000000000000000000000001"
const stream config.WorkstreamID = "w_00000000000000000000000000000001"

var timestamp = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

type fakeTurns func(context.Context, coreadapter.PreparedTurn) (coreadapter.SessionResult, error)

func (f fakeTurns) Run(ctx context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
	return f(ctx, p)
}
func setup(t *testing.T) (*trace.Repository, config.Root, config.Project) {
	t.Helper()
	base := t.TempDir()
	root, err := config.ResolveRoot(filepath.Join(base, "osmia"), "")
	if err != nil {
		t.Fatal(err)
	}
	p := config.Project{ID: project, Clone: filepath.Join(base, "target")}
	if err := os.Mkdir(p.Clone, 0700); err != nil {
		t.Fatal(err)
	}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(context.Background(), root, p, timestamp, owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.CreateWorkstream(context.Background(), stream, timestamp, owner); err != nil {
		t.Fatal(err)
	}
	h := trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: "agent", Project: project, Workstream: stream, At: timestamp, Actor: owner, Cause: "message", Depth: 3}
	if err := repo.CreateThread(context.Background(), trace.Agent{Header: h, Role: "mason", ThreadID: "thread"}); err != nil {
		t.Fatal(err)
	}
	return repo, root, p
}
func queue(t *testing.T, r *trace.Repository, id string) trace.TurnRequest {
	t.Helper()
	return queueBackend(t, r, id, "fake")
}

func queueBackend(t *testing.T, r *trace.Repository, id, backend string) trace.TurnRequest {
	t.Helper()
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_" + id, Project: project, Workstream: stream, At: timestamp, Actor: trace.Actor{Kind: "owner", ID: "local"}, Cause: "owner-message", Depth: 3}, AgentID: "agent", ThreadID: "thread", TurnID: id, Profile: coreadapter.Profile{Name: "default", Backend: backend, Model: "test"}, SystemPrompt: "Role instructions", Prompt: "Message " + id, History: "Owned history"}
	if _, err := r.EnqueueTurn(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	return req
}
func TestRunnerSerialDeliveryAndProvenance(t *testing.T) {
	repo, root, p := setup(t)
	first := queue(t, repo, "first")
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	clock := func() time.Time { return timestamp.Add(time.Second) }
	runner := Runner{Store: repo, Now: clock, Turns: fakeTurns(func(ctx context.Context, got coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		calls.Add(1)
		if got.Prompt != first.Prompt || got.Profile != first.Profile || got.SystemPrompt != first.SystemPrompt || got.History == first.History || got.Scope.Role != "mason" || got.Scope.Turn != "first" || got.Resume != nil {
			t.Errorf("prepared request: %#v", got)
		}
		close(entered)
		<-release
		return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session"}, FinalResponse: "First response", Outcome: &coreadapter.Outcome{Status: "complete", Report: "Done"}}, nil
	})}
	prepared := coreadapter.PreparedTurn{SessionDirectory: filepath.Join(t.TempDir(), "session"), Prompt: "must be replaced", Profile: coreadapter.Profile{Name: "wrong"}}
	finished := make(chan error, 1)
	go func() { _, err := runner.RunNext(context.Background(), stream, "agent", prepared); finished <- err }()
	<-entered
	second := queue(t, repo, "second")
	if _, err := runner.RunNext(context.Background(), stream, "agent", prepared); !errors.Is(err, trace.ErrClaimed) {
		t.Fatalf("competing dispatcher: %v", err)
	}
	th, err := repo.Thread(stream, "agent")
	if err != nil || th.Active != "first" || len(th.Turns) != 2 || th.Turns[0].Request.Prompt != first.Prompt {
		t.Fatalf("midturn queue: %#v %v", th, err)
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("runner calls %d", calls.Load())
	}
	repo.Close()
	repo, err = trace.Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	runner.Store = repo
	runner.Turns = fakeTurns(func(_ context.Context, got coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		if got.Prompt != second.Prompt || got.Scope.Turn != "second" {
			t.Errorf("queued successor: %#v", got)
		}
		return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "next"}, FinalResponse: "Second response", Outcome: &coreadapter.Outcome{Status: "waiting", Report: "Question"}}, nil
	})
	if _, err := runner.RunNext(context.Background(), stream, "agent", prepared); err != nil {
		t.Fatal(err)
	}
	responses, err := trace.Read[trace.TurnResponse](repo, stream)
	if err != nil || len(responses) != 2 {
		t.Fatalf("responses: %#v %v", responses, err)
	}
	for i, res := range responses {
		req := []trace.TurnRequest{first, second}[i]
		if res.Result.FinalResponse != []string{"First response", "Second response"}[i] {
			t.Fatalf("lost final response: %#v", res)
		}
		if res.RequestID != req.ID || res.Cause != req.Cause || res.Depth != req.Depth || res.Result.Outcome == nil || res.Result.StartedAt != clock() || res.Result.SessionDirectory != prepared.SessionDirectory || res.At != clock() {
			t.Fatalf("provenance: %#v", res)
		}
	}
	th, err = repo.Thread(stream, "agent")
	if err != nil || th.Status != "waiting" || th.Active != "" || th.Session.ID != "next" {
		t.Fatalf("final thread: %#v %v", th, err)
	}
	if _, err := runner.RunNext(context.Background(), stream, "agent", prepared); !errors.Is(err, trace.ErrNoTurn) {
		t.Fatalf("empty queue: %v", err)
	}
}

func TestRunnerCapturesFailureAndCancellation(t *testing.T) {
	for _, kind := range []string{"failure", "launch-failure", "cancelled", "timeout", "backend-error", "exit", "signal"} {
		t.Run(kind, func(t *testing.T) {
			repo, _, _ := setup(t)
			queue(t, repo, "first")
			queue(t, repo, "next")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failure := errors.New("backend failed")
			result := coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "partial"}, FinalResponse: "Partial final response"}
			var runErr error
			switch kind {
			case "failure":
				runErr = failure
			case "launch-failure":
				runErr = failure
				result.Session = coreadapter.BackendSession{}
			case "cancelled":
				runErr = context.Canceled
			case "timeout":
				result.TimedOut = true
			case "backend-error":
				result.IsError = true
			case "exit":
				result.ExitCode = 1
			case "signal":
				result.Signal = 9
			}
			runner := Runner{Store: repo, Now: func() time.Time { return timestamp.Add(time.Second) }, Turns: fakeTurns(func(_ context.Context, _ coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
				if kind == "cancelled" {
					cancel()
				}
				return result, runErr
			})}
			q, err := runner.RunNext(ctx, stream, "agent", coreadapter.PreparedTurn{SessionDirectory: "/owned/partial"})
			if !errors.Is(err, runErr) {
				t.Fatalf("error: %v", err)
			}
			responses, err := trace.Read[trace.TurnResponse](repo, stream)
			if err != nil || len(responses) != 1 || !reflect.DeepEqual(responses[0], *q.Response) || responses[0].Result.FinalResponse != result.FinalResponse {
				t.Fatalf("captured partial: %#v %v", responses, err)
			}
			th, err := repo.Thread(stream, "agent")
			want := "failed"
			if kind == "cancelled" {
				want = "interrupted"
			}
			if err != nil || th.Status != want || th.Active != "" || q.CompletedAt.IsZero() {
				t.Fatalf("failure state: %#v %v", th, err)
			}
			if _, err := repo.ClaimTurn(context.Background(), stream, "agent", "next", "/next", timestamp.Add(2*time.Second)); err != nil {
				t.Fatalf("successor after failure: %v", err)
			}
		})
	}
}
