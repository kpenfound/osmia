package service

import (
	"context"
	"errors"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

type agentStatusRig struct {
	t       *testing.T
	ctx     context.Context
	cfg     *config.Config
	repo    *trace.Repository
	s       *Service
	started time.Time
	owner   trace.Actor
	actor   trace.Actor
	profile coreadapter.Profile
	header  trace.Header
	request trace.TurnRequest
	attempt trace.TurnAttempt
}

func newAgentStatusRig(t *testing.T) *agentStatusRig {
	t.Helper()
	ctx := context.Background()
	opts := fixture(t)
	cfg, err := projectstore.LoadConfig(opts.Config)
	fixturekit.Must(t, err)
	started := time.Now().UTC().Add(-5 * time.Second).Truncate(time.Second)
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, started.Add(-time.Second), owner)
	fixturekit.Must(t, err)
	t.Cleanup(func() { repo.Close() })
	fixturekit.Must(t, repo.CreateWorkstream(ctx, fixturekit.Stream, started.Add(-time.Second), owner))
	actor := trace.Actor{Kind: "agent", ID: "mason_one"}
	profile := coreadapter.Profile{Name: "mason-default", Backend: "fake", Model: "test"}
	h := trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: actor.ID, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: started.Add(-time.Second), Actor: owner, Cause: "created"}
	fixturekit.Must(t, repo.CreateThread(ctx, trace.Agent{Header: h, Role: "mason", ThreadID: "mason_thread"}))
	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{cfg.Project.ID: []config.WorkstreamID{fixturekit.Stream}}})
	fixturekit.Must(t, err)
	t.Cleanup(func() { store.Close() })
	return &agentStatusRig{t: t, ctx: ctx, cfg: cfg, repo: repo, s: &Service{cfg: cfg, projects: []*activeProject{runtimeFor(repo)}, store: store},
		started: started, owner: owner, actor: actor, profile: profile, header: h}
}

func (r *agentStatusRig) claimFirstTurn() {
	r.t.Helper()
	h := r.header
	h.Schema, h.ID, h.At, h.Cause, h.Unit = "osmia.trace.turn-request", "request_one", r.started.Add(-time.Second), "dispatch", "resume"
	r.header = h
	r.request = trace.TurnRequest{Header: h, AgentID: r.actor.ID, ThreadID: "mason_thread", TurnID: "one", Profile: r.profile, Prompt: "Build resume"}
	_, err := r.repo.EnqueueTurn(r.ctx, r.request)
	fixturekit.Must(r.t, err)
	_, err = r.repo.ClaimTurn(r.ctx, fixturekit.Stream, r.actor.ID, "token_one", "/owned/one", r.started)
	fixturekit.Must(r.t, err)
	r.attempt = trace.TurnAttempt{Number: 1, Profile: r.profile, Path: "replay", Reason: "new thread", At: r.started}
	fixturekit.Must(r.t, r.repo.RecordAttempt(r.ctx, fixturekit.Stream, r.actor.ID, "one", "token_one", r.attempt))
}

func (r *agentStatusRig) askAndCaptureFirstTurn() string {
	r.t.Helper()
	started, actor, repo := r.started, r.actor, r.repo
	ctx, t := r.ctx, r.t
	question, err := repo.Ask(ctx, actor.ID, coreadapter.Scope{Project: string(fixturekit.Project), Workstream: string(fixturekit.Stream), Unit: "resume", Thread: "mason_thread", Turn: "one", Role: "mason"}, "Which format?", started.Add(2*time.Second))
	fixturekit.Must(t, err)
	result := coreadapter.SessionResult{SessionDirectory: "/owned/one", StartedAt: started, Session: coreadapter.BackendSession{Backend: "fake", ID: "session_one"}, Outcome: &coreadapter.Outcome{Status: "waiting", Report: "Asked"}}
	r.attempt.Result = &result
	fixturekit.Must(t, repo.RecordAttempt(ctx, fixturekit.Stream, actor.ID, "one", "token_one", r.attempt))
	h := r.header
	h.Schema, h.ID, h.At, h.Actor, h.Cause = "osmia.trace.turn-response", "response_one", started.Add(3*time.Second), trace.Actor{Kind: "service", ID: "turn"}, r.request.Cause
	response := trace.TurnResponse{Header: h, AgentID: actor.ID, ThreadID: "mason_thread", TurnID: "one", RequestID: r.request.ID, RequestRevision: 1, Result: result}
	fixturekit.Must(t, repo.CaptureTurn(ctx, "token_one", response))
	return question.ID
}

func (r *agentStatusRig) completeFirstTurn() {
	r.t.Helper()
	fixturekit.Must(r.t, r.repo.CompleteTurn(r.ctx, fixturekit.Stream, r.actor.ID, "one", "token_one", r.started.Add(3*time.Second)))
}

func (r *agentStatusRig) claimSecondTurn() {
	r.t.Helper()
	h := r.header
	h.Schema, h.ID, h.At, h.Actor, h.Cause = "osmia.trace.turn-request", "request_two", r.started.Add(3*time.Second), r.owner, "answer"
	req := trace.TurnRequest{Header: h, AgentID: r.actor.ID, ThreadID: "mason_thread", TurnID: "two", Profile: r.profile, Prompt: "Answer"}
	_, err := r.repo.EnqueueTurn(r.ctx, req)
	fixturekit.Must(r.t, err)
	_, err = r.repo.ClaimTurn(r.ctx, fixturekit.Stream, r.actor.ID, "token_two", "/owned/two", r.started.Add(4*time.Second))
	fixturekit.Must(r.t, err)
}

func (r *agentStatusRig) reopen() {
	r.t.Helper()
	fixturekit.Must(r.t, r.repo.Close())
	repo, err := trace.Open(r.cfg.Root, r.cfg.Project)
	fixturekit.Must(r.t, err)
	r.t.Cleanup(func() { repo.Close() })
	r.repo = repo
	r.s.sole().repository = repo
}

func (r *agentStatusRig) requireStatus(now time.Time, state string, elapsed int64, question string) {
	r.t.Helper()
	t, started, profile := r.t, r.started, r.profile
	got, err := agentStatuses(r.repo, fixturekit.Stream, now)
	fixturekit.Must(t, err)
	if len(got) != 1 || got[0] != (AgentStatus{Role: "mason", Unit: "resume", State: state, StartedAt: started, Elapsed: elapsed, Profile: profile.Name, Attempt: 1, Path: "replay", QuestionID: question}) {
		t.Fatalf("agent status at %s: %+v", now, got)
	}
	view, api := r.s.workstreamStatus(string(fixturekit.Stream))
	if api != nil || len(view.Agents) != 1 || view.Agents[0].State != state || view.Agents[0].QuestionID != question {
		t.Fatalf("API status at %s: %+v %v", now, view, api)
	}
}

func TestAnIdleAgentThreadHasNoStatus(t *testing.T) {
	t.Parallel()
	r := newAgentStatusRig(t)
	if got, err := agentStatuses(r.repo, fixturekit.Stream, r.started); err != nil || len(got) != 0 {
		t.Fatalf("idle thread: %+v %v", got, err)
	}
}

func TestAClaimedTurnShowsTheAgentRunningForTheTimeSinceItStarted(t *testing.T) {
	t.Parallel()
	r := newAgentStatusRig(t)
	r.claimFirstTurn()
	r.requireStatus(r.started.Add(5*time.Second), "running", 5, "")
	r.requireStatus(r.started.Add(7*time.Second), "running", 7, "")
}

func TestACapturedTurnShowsTheAgentCaptured(t *testing.T) {
	t.Parallel()
	r := newAgentStatusRig(t)
	r.claimFirstTurn()
	r.askAndCaptureFirstTurn()
	r.requireStatus(r.started.Add(10*time.Second), "captured", 3, "")
}

func TestACompletedTurnWithAQuestionShowsTheAgentWaitingOnIt(t *testing.T) {
	t.Parallel()
	r := newAgentStatusRig(t)
	r.claimFirstTurn()
	question := r.askAndCaptureFirstTurn()
	r.completeFirstTurn()
	r.requireStatus(r.started.Add(10*time.Second), "waiting", 3, question)
}

func TestAClaimedTurnFoundAfterARestartShowsTheAgentInterrupted(t *testing.T) {
	t.Parallel()
	r := newAgentStatusRig(t)
	r.claimFirstTurn()
	r.askAndCaptureFirstTurn()
	r.completeFirstTurn()
	r.claimSecondTurn()
	r.reopen()
	got, err := agentStatuses(r.repo, fixturekit.Stream, r.started.Add(9*time.Second))
	fixturekit.Must(t, err)
	if len(got) != 1 || got[0].State != "interrupted" || got[0].Elapsed != 5 || got[0].Profile != r.profile.Name || got[0].Attempt != 0 {
		t.Fatalf("restart status: %+v", got)
	}
	view, api := r.s.workstreamStatus(string(fixturekit.Stream))
	if api != nil || len(view.Agents) != 1 || view.Agents[0].State != "interrupted" {
		t.Fatalf("restart API status: %+v %v", view, api)
	}
}

func TestUnreadableAgentTurnsAreReportedAsADiagnosticAndAnAPIError(t *testing.T) {
	t.Parallel()
	r := newAgentStatusRig(t)
	r.claimFirstTurn()
	s := r.s
	agentsMessage := "cannot read the agent turns of workstream " + string(fixturekit.Stream) + "; check the trace repository"
	for _, failing := range [][]string{{"status-agents"}, {"status-agents", "status-units"}} {
		s.setBoundary(func(name string) error {
			if slices.Contains(failing, name) {
				return errors.New("unreadable")
			}
			return nil
		})
		list, unreadable, api := s.statuses()
		if api != nil || len(list) != 1 || list[0].Agents != nil || unreadable[fixturekit.Stream] != (Diagnostic{Field: "agents", Code: Internal, Message: agentsMessage}) {
			t.Fatalf("failing %v: %+v %+v %v", failing, list, unreadable, api)
		}
		if _, api := s.workstreamStatus(string(fixturekit.Stream)); api == nil || *api != (APIError{Code: Internal, Message: agentsMessage}) {
			t.Fatalf("failing %v: workstream status %v", failing, api)
		}
	}
}

func TestAnAbandonedTurnLeavesNoAgentStatus(t *testing.T) {
	t.Parallel()
	r := newAgentStatusRig(t)
	r.claimFirstTurn()
	r.askAndCaptureFirstTurn()
	r.completeFirstTurn()
	r.claimSecondTurn()
	r.reopen()
	fixturekit.Must(t, r.repo.AbandonTurn(r.ctx, fixturekit.Stream, r.actor.ID, "two", r.started.Add(9*time.Second)))
	if got, err := agentStatuses(r.repo, fixturekit.Stream, r.started.Add(10*time.Second)); err != nil || len(got) != 0 {
		t.Fatalf("completed thread: %+v %v", got, err)
	}
}
