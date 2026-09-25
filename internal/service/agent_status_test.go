package service

import (
	"context"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestAgentStatusFromDurableTurns(t *testing.T) {
	ctx := context.Background()
	opts := fixture(t)
	cfg, err := config.Load(opts.Config)
	must(t, err)
	started := time.Now().UTC().Add(-5 * time.Second).Truncate(time.Second)
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, started.Add(-time.Second), owner)
	must(t, err)
	must(t, repo.CreateWorkstream(ctx, stream, started.Add(-time.Second), owner))
	actor := trace.Actor{Kind: "agent", ID: "mason_one"}
	profile := coreadapter.Profile{Name: "mason-default", Backend: "fake", Model: "test"}
	h := trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: actor.ID, Project: project, Workstream: stream, At: started.Add(-time.Second), Actor: owner, Cause: "created"}
	must(t, repo.CreateThread(ctx, trace.Agent{Header: h, Role: "mason", ThreadID: "mason_thread"}))
	if got, err := agentStatuses(repo, stream, started); err != nil || len(got) != 0 {
		t.Fatalf("idle thread: %+v %v", got, err)
	}
	h.Schema, h.ID, h.At, h.Cause, h.Unit = "osmia.trace.turn-request", "request_one", started.Add(-time.Second), "dispatch", "resume"
	req := trace.TurnRequest{Header: h, AgentID: actor.ID, ThreadID: "mason_thread", TurnID: "one", Profile: profile, Prompt: "Build resume"}
	_, err = repo.EnqueueTurn(ctx, req)
	must(t, err)
	_, err = repo.ClaimTurn(ctx, stream, actor.ID, "token_one", "/owned/one", started)
	must(t, err)
	attempt := trace.TurnAttempt{Number: 1, Profile: profile, Path: "replay", Reason: "new thread", At: started}
	must(t, repo.RecordAttempt(ctx, stream, actor.ID, "one", "token_one", attempt))
	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: []config.WorkstreamID{stream}})
	must(t, err)
	defer store.Close()
	s := &Service{cfg: cfg, active: &activeProject{repository: repo}, store: store}
	check := func(now time.Time, state string, elapsed int64, question string) {
		t.Helper()
		got, err := agentStatuses(repo, stream, now)
		must(t, err)
		if len(got) != 1 || got[0] != (AgentStatus{Role: "mason", Unit: "resume", State: state, StartedAt: started, Elapsed: elapsed, Profile: profile.Name, Attempt: 1, Path: "replay", QuestionID: question}) {
			t.Fatalf("agent status at %s: %+v", now, got)
		}
		view, api := s.workstreamStatus(string(stream))
		if api != nil || len(view.Agents) != 1 || view.Agents[0].State != state || view.Agents[0].QuestionID != question {
			t.Fatalf("API status at %s: %+v %v", now, view, api)
		}
	}
	check(started.Add(5*time.Second), "running", 5, "")
	check(started.Add(7*time.Second), "running", 7, "")
	question, err := repo.Ask(ctx, actor.ID, coreadapter.Scope{Project: string(project), Workstream: string(stream), Unit: "resume", Thread: "mason_thread", Turn: "one", Role: "mason"}, "Which format?", started.Add(2*time.Second))
	must(t, err)
	result := coreadapter.SessionResult{SessionDirectory: "/owned/one", StartedAt: started, Session: coreadapter.BackendSession{Backend: "fake", ID: "session_one"}, Outcome: &coreadapter.Outcome{Status: "waiting", Report: "Asked"}}
	attempt.Result = &result
	must(t, repo.RecordAttempt(ctx, stream, actor.ID, "one", "token_one", attempt))
	h.Schema, h.ID, h.At, h.Actor, h.Cause = "osmia.trace.turn-response", "response_one", started.Add(3*time.Second), trace.Actor{Kind: "service", ID: "turn"}, req.Cause
	response := trace.TurnResponse{Header: h, AgentID: actor.ID, ThreadID: "mason_thread", TurnID: "one", RequestID: req.ID, RequestRevision: 1, Result: result}
	must(t, repo.CaptureTurn(ctx, "token_one", response))
	check(started.Add(10*time.Second), "captured", 3, "")
	must(t, repo.CompleteTurn(ctx, stream, actor.ID, "one", "token_one", started.Add(3*time.Second)))
	check(started.Add(10*time.Second), "waiting", 3, question.ID)

	h.Schema, h.ID, h.At, h.Actor, h.Cause = "osmia.trace.turn-request", "request_two", started.Add(3*time.Second), owner, "answer"
	req = trace.TurnRequest{Header: h, AgentID: actor.ID, ThreadID: "mason_thread", TurnID: "two", Profile: profile, Prompt: "Answer"}
	_, err = repo.EnqueueTurn(ctx, req)
	must(t, err)
	_, err = repo.ClaimTurn(ctx, stream, actor.ID, "token_two", "/owned/two", started.Add(4*time.Second))
	must(t, err)
	must(t, repo.Close())
	repo, err = trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	defer repo.Close()
	s.active.repository = repo
	got, err := agentStatuses(repo, stream, started.Add(9*time.Second))
	must(t, err)
	if len(got) != 1 || got[0].State != "interrupted" || got[0].Elapsed != 5 || got[0].Profile != profile.Name || got[0].Attempt != 0 {
		t.Fatalf("restart status: %+v", got)
	}
	view, api := s.workstreamStatus(string(stream))
	if api != nil || len(view.Agents) != 1 || view.Agents[0].State != "interrupted" {
		t.Fatalf("restart API status: %+v %v", view, api)
	}
	must(t, repo.AbandonTurn(ctx, stream, actor.ID, "two", started.Add(9*time.Second)))
	if got, err := agentStatuses(repo, stream, started.Add(10*time.Second)); err != nil || len(got) != 0 {
		t.Fatalf("completed thread: %+v %v", got, err)
	}
}
