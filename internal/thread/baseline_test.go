package thread

import (
	"context"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

func enqueueWith(t *testing.T, r *trace.Repository, id, system string) trace.TurnRequest {
	t.Helper()
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_" + id, Project: project, Workstream: stream, At: timestamp, Actor: trace.Actor{Kind: "owner", ID: "local"}, Cause: "owner-message", Depth: 3}, AgentID: "agent", ThreadID: "thread", TurnID: id, Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, SystemPrompt: system, Prompt: "Message " + id}
	if _, err := r.EnqueueTurn(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	return req
}

type sessions struct {
	seen    []coreadapter.PreparedTurn
	next    string
	refuses bool
}

func (s *sessions) Run(_ context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
	s.seen = append(s.seen, p)
	id := s.next
	if p.Resume != nil {
		id = p.Resume.ID
	}
	return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: id}, FinalResponse: "done"}, nil
}

func (s *sessions) CheckResume(context.Context, coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error {
	if s.refuses {
		return coreadapter.ErrResumeUnavailable
	}
	return nil
}

func runnerOn(repo *trace.Repository, backend *sessions) Runner {
	return Runner{Jev: unconfiguredJudge(), Store: repo, Now: func() time.Time { return timestamp.Add(time.Second) }, Turns: backend}
}

func run(t *testing.T, r Runner, id string) trace.QueuedTurn {
	t.Helper()
	q, err := r.RunNext(context.Background(), stream, "agent", coreadapter.PreparedTurn{SessionDirectory: "/owned/" + id})
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func TestResumedSessionKeepsTheSystemPromptItBeganWith(t *testing.T) {
	repo, root, cfg := setup(t)
	backend := &sessions{next: "s1"}
	enqueueWith(t, repo, "first", "base text v1")
	run(t, runnerOn(repo, backend), "first")

	repo.Close()
	repo, err := trace.Open(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	for i, id := range []string{"second", "third"} {
		enqueueWith(t, repo, id, "base text v2 after a deploy "+id)
		q := run(t, runnerOn(repo, backend), id)
		got := backend.seen[len(backend.seen)-1]
		if got.Resume == nil || got.Resume.ID != "s1" || got.SystemPrompt != "base text v1" {
			t.Fatalf("turn %d: resume=%v system prompt %q", i+2, got.Resume, got.SystemPrompt)
		}
		if a := q.Attempts[0]; a.Path != "resume" || a.SystemPrompt == nil || *a.SystemPrompt != "base text v1" {
			t.Fatalf("turn %d: attempt does not record the baseline: %+v", i+2, a)
		}
		if q.Request.SystemPrompt == got.SystemPrompt {
			t.Fatal("the durable request was rewritten")
		}
	}
}

func TestReplayedSessionStartsFromTheCurrentPromptAndKeepsIt(t *testing.T) {
	repo, root, cfg := setup(t)
	backend := &sessions{next: "s1"}
	enqueueWith(t, repo, "first", "base text v1")
	run(t, runnerOn(repo, backend), "first")

	backend.refuses, backend.next = true, "s2"
	enqueueWith(t, repo, "second", "base text v2")
	q := run(t, runnerOn(repo, backend), "second")
	got := backend.seen[len(backend.seen)-1]
	if got.Resume != nil || got.History == "" || got.SystemPrompt != "base text v2" || *q.Attempts[0].SystemPrompt != "base text v2" {
		t.Fatalf("replay did not start from the current prompt: resume=%v history=%q system=%q", got.Resume, got.History, got.SystemPrompt)
	}

	repo.Close()
	repo, err := trace.Open(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	backend.refuses = false
	enqueueWith(t, repo, "third", "base text v3")
	run(t, runnerOn(repo, backend), "third")
	got = backend.seen[len(backend.seen)-1]
	if got.Resume == nil || got.Resume.ID != "s2" || got.SystemPrompt != "base text v2" {
		t.Fatalf("the replacement session was not held to its own prompt: resume=%v system=%q", got.Resume, got.SystemPrompt)
	}
}

func TestBaselineBelongsToTheSessionNotTheThread(t *testing.T) {
	repo, _, _ := setup(t)
	backend := &sessions{next: "s1"}
	enqueueWith(t, repo, "first", "prompt of s1")
	run(t, runnerOn(repo, backend), "first")
	backend.refuses, backend.next = true, "s2"
	enqueueWith(t, repo, "second", "prompt of s2")
	run(t, runnerOn(repo, backend), "second")
	th, err := repo.Thread(stream, "agent")
	if err != nil {
		t.Fatal(err)
	}
	for session, want := range map[string]string{"s1": "prompt of s1", "s2": "prompt of s2"} {
		got, ok := sessionBaseline(th, 99, coreadapter.BackendSession{Backend: "fake", ID: session}, nil)
		if !ok || got != want {
			t.Fatalf("baseline of %s = %q %v, want %q", session, got, ok, want)
		}
	}
	if _, ok := sessionBaseline(th, 99, coreadapter.BackendSession{Backend: "fake", ID: "unknown"}, nil); ok {
		t.Fatal("a baseline was invented for a session the thread never ran")
	}
}

func legacyTurn(t *testing.T, r *trace.Repository, req trace.TurnRequest, path string, source, session coreadapter.BackendSession) {
	t.Helper()
	ctx := context.Background()
	q, err := r.ClaimTurn(ctx, stream, "agent", "token-"+req.TurnID, "/owned/"+req.TurnID, timestamp.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	a := trace.TurnAttempt{Number: 1, Profile: req.Profile, Path: path, Reason: "legacy", SourceSession: source, SourceSequence: q.Sequence - 1, At: timestamp.Add(2 * time.Second)}
	if path == "replay" {
		a.SourceSequence = 0
	}
	if err := r.RecordAttempt(ctx, stream, "agent", req.TurnID, q.Claim.Token, a); err != nil {
		t.Fatal(err)
	}
	result := coreadapter.SessionResult{Session: session, SessionDirectory: q.Claim.SessionDirectory, FinalResponse: "done", StartedAt: a.At}
	a.Result = &result
	if err := r.RecordAttempt(ctx, stream, "agent", req.TurnID, q.Claim.Token, a); err != nil {
		t.Fatal(err)
	}
	h := req.Header
	h.Schema, h.ID, h.At, h.Actor = "osmia.trace.turn-response", trace.EventID(req.ID, "response"), timestamp.Add(3*time.Second), trace.Actor{Kind: "service", ID: "thread-runner"}
	response := trace.TurnResponse{Header: h, AgentID: "agent", ThreadID: req.ThreadID, TurnID: req.TurnID, RequestID: req.ID, RequestRevision: req.Revision, Result: result}
	if err := r.CaptureTurn(ctx, q.Claim.Token, response); err != nil {
		t.Fatal(err)
	}
	if err := r.CompleteTurn(ctx, stream, "agent", req.TurnID, q.Claim.Token, timestamp.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestSavedSessionRecoversItsBaselineFromTheRequestThatOpenedIt(t *testing.T) {
	repo, _, _ := setup(t)
	backend := &sessions{}
	opened := enqueueWith(t, repo, "opened", "what the opening request recorded")
	s1 := coreadapter.BackendSession{Backend: "fake", ID: "s1"}
	legacyTurn(t, repo, opened, "replay", coreadapter.BackendSession{}, s1)
	resumed := enqueueWith(t, repo, "resumed", "text of a later deploy")
	legacyTurn(t, repo, resumed, "resume", s1, s1)

	enqueueWith(t, repo, "next", "text of an even later deploy")
	r := runnerOn(repo, backend)
	r.LegacyBaseline = func(role string, opening trace.QueuedTurn) (string, bool) {
		if role != "mason" || opening.Request.TurnID != "opened" {
			t.Fatalf("the baseline was read from %s of role %s, not from the turn that opened the session", opening.Request.TurnID, role)
		}
		return opening.Request.SystemPrompt, true
	}
	q := run(t, r, "next")
	got := backend.seen[0]
	if got.Resume == nil || got.Resume.ID != "s1" || got.SystemPrompt != "what the opening request recorded" {
		t.Fatalf("resume=%v system=%q", got.Resume, got.SystemPrompt)
	}
	if a := q.Attempts[0]; a.SystemPrompt == nil || *a.SystemPrompt != "what the opening request recorded" {
		t.Fatalf("the recovered baseline was not recorded: %+v", a)
	}
}

func TestSavedSessionWithNoEstablishedBaselineReplaysInsteadOfResuming(t *testing.T) {
	s1 := coreadapter.BackendSession{Backend: "fake", ID: "s1"}
	for name, legacy := range map[string]LegacyBaseline{
		"no recovery rule":      nil,
		"records fall short":    func(string, trace.QueuedTurn) (string, bool) { return "", false },
		"opening not on record": nil,
	} {
		t.Run(name, func(t *testing.T) {
			repo, _, _ := setup(t)
			backend := &sessions{next: "s2"}
			opened := enqueueWith(t, repo, "opened", "recorded")
			if name == "opening not on record" {
				legacyTurn(t, repo, opened, "resume", coreadapter.BackendSession{Backend: "fake", ID: "earlier"}, s1)
			} else {
				legacyTurn(t, repo, opened, "replay", coreadapter.BackendSession{}, s1)
			}
			enqueueWith(t, repo, "next", "current")
			r := runnerOn(repo, backend)
			r.LegacyBaseline = legacy
			q := run(t, r, "next")
			got := backend.seen[0]
			if got.Resume != nil || got.History == "" || got.SystemPrompt != "current" {
				t.Fatalf("resumed with a guessed prompt: resume=%v system=%q", got.Resume, got.SystemPrompt)
			}
			if a := q.Attempts[0]; a.Path != "replay" || *a.SystemPrompt != "current" {
				t.Fatalf("attempt %+v", a)
			}
		})
	}
}

func TestEmptyBaselineIsAnEstablishedBaseline(t *testing.T) {
	repo, _, _ := setup(t)
	backend := &sessions{next: "s1"}
	enqueueWith(t, repo, "first", "")
	run(t, runnerOn(repo, backend), "first")
	enqueueWith(t, repo, "second", "now there is text")
	run(t, runnerOn(repo, backend), "second")
	if got := backend.seen[1]; got.Resume == nil || got.SystemPrompt != "" {
		t.Fatalf("resume=%v system=%q", got.Resume, got.SystemPrompt)
	}
}
