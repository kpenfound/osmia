package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestStartupRecoversClaimedAndUnclaimedSessionsForEveryRole(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts := fixtureAt(t, t.TempDir())
	cfg, err := projectstore.LoadConfig(opts.Config)
	intakefixture.Must(t, err)
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, fixturekit.DemoStart, ownerActor)
	intakefixture.Must(t, err)
	intakefixture.Must(t, repo.CreateWorkstream(ctx, intakefixture.Stream, fixturekit.DemoStart, ownerActor))
	roles := []struct{ agent, role string }{
		{trace.ChiefOfStaff, trace.ChiefOfStaff}, {"recovery_mason", masonRole}, {"recovery_reviewer", reviewerRole},
		{"recovery_architect", architectRole}, {"recovery_committee", committeeRole},
		{driftMasonAgent, masonRole}, {driftReviewerAgent, reviewerRole}, {"undispatched_reviewer", reviewerRole},
		{"final_committee", committeeRole},
	}
	for i, item := range roles {
		agent, role := item.agent, item.role
		if role != trace.ChiefOfStaff {
			id := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, Revision: 1, ID: agent, Project: cfg.Project.ID, Workstream: intakefixture.Stream, At: fixturekit.DemoStart, Actor: ownerActor, Cause: "workstream_created"}, Role: role, ThreadID: agent}
			intakefixture.Must(t, repo.CreateThread(ctx, id))
		}
		turn := "turn_" + agent
		_, err := repo.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, Revision: 1, ID: "request_" + agent, Project: cfg.Project.ID, Workstream: intakefixture.Stream, At: fixturekit.DemoStart, Actor: ownerActor, Cause: "workstream_created"}, AgentID: agent, ThreadID: agent, TurnID: turn, Profile: coreadapter.Profile{Name: "default", Backend: "codex", Model: "test"}, Prompt: "Finish durable work"})
		intakefixture.Must(t, err)
		dirs := sessionDirectories(cfg, cfg.Project.ID, intakefixture.Stream, trace.Agent{Role: role, Header: trace.Header{ID: agent}}, turn)
		dir := dirs[0]
		if agent == "final_committee" {
			dir = dirs[1]
		}
		if agent != "undispatched_reviewer" && (i%2 == 0 || role == masonRole) {
			_, err = repo.ClaimTurn(ctx, intakefixture.Stream, agent, "claim_"+agent, dir, fixturekit.DemoStart)
			intakefixture.Must(t, err)
		} else if agent != "undispatched_reviewer" {
			intakefixture.Must(t, os.MkdirAll(dir, 0700))
		}
	}
	intakefixture.Must(t, repo.Close())
	repo, err = trace.Open(cfg.Root, cfg.Project)
	intakefixture.Must(t, err)
	defer repo.Close()
	intakefixture.Must(t, repo.AbandonTurn(ctx, intakefixture.Stream, trace.ChiefOfStaff, "turn_"+trace.ChiefOfStaff, fixturekit.DemoStart.Add(1)))
	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{cfg.Project.ID: []config.WorkstreamID{intakefixture.Stream}}})
	intakefixture.Must(t, err)
	defer store.Close()
	s := &Service{cfg: cfg, store: store, options: opts}
	s.options.Threads = func(*trace.Repository, *config.Config) (coreadapter.Reconciler, error) { return nil, nil }
	intakefixture.Must(t, s.recoverSessions(ctx, cfg, repo))
	for _, item := range roles {
		agent, role := item.agent, item.role
		th, err := repo.Thread(intakefixture.Stream, agent)
		intakefixture.Must(t, err)
		if agent == "undispatched_reviewer" {
			if len(th.Turns) != 1 || th.Turns[0].Claim != nil || th.Turns[0].Response != nil {
				t.Fatalf("queued turn without a session was recovered: %+v", th)
			}
			continue
		}
		if role == masonRole && th.Active != "" {
			if th.Status != "interrupted" || th.Turns[0].Claim == nil {
				t.Fatalf("mason view recovery claim: %+v", th)
			}
			continue
		}
		if th.Turns[0].Status() != "interrupted" || th.Turns[0].Response == nil || th.Turns[0].Response.Failure == "" {
			t.Fatalf("%s interruption: %+v", role, th)
		}
		if role == trace.ChiefOfStaff {
			if len(th.Turns) != 2 || !strings.Contains(th.Turns[1].Request.Prompt, "service stopped") || th.Turns[1].Request.Cause != th.Turns[0].Response.ID {
				t.Fatalf("chief continuation: %+v", th)
			}
		} else if len(th.Turns) != 1 {
			t.Fatalf("%s is re-derived by its role pass: %+v", role, th)
		}
	}
	intakefixture.Must(t, s.recoverSessions(ctx, cfg, repo))
	chief, err := repo.ChiefOfStaffThread(intakefixture.Stream)
	intakefixture.Must(t, err)
	if len(chief.Turns) != 2 {
		t.Fatalf("repeated startup queued another chief turn: %+v", chief.Turns)
	}
}

func TestServiceStartRunsChiefInterruptionTurn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts := fixtureAt(t, t.TempDir())
	cfg, err := projectstore.LoadConfig(opts.Config)
	intakefixture.Must(t, err)
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, fixturekit.DemoStart, ownerActor)
	intakefixture.Must(t, err)
	intakefixture.Must(t, repo.CreateWorkstream(ctx, intakefixture.Stream, fixturekit.DemoStart, ownerActor))
	const turn = "owner_message"
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, Revision: 1, ID: "request_owner_message", Project: cfg.Project.ID, Workstream: intakefixture.Stream, At: fixturekit.DemoStart, Actor: ownerActor, Cause: "owner-message"}, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: turn, Profile: coreadapter.Profile{Name: "default", Backend: "codex", Model: "test"}, Prompt: "Remember this request"})
	intakefixture.Must(t, err)
	dir := filepath.Join(cfg.Root.String(), "threads", string(cfg.Project.ID), string(intakefixture.Stream), trace.ChiefOfStaff, turn)
	_, err = repo.ClaimTurn(ctx, intakefixture.Stream, trace.ChiefOfStaff, "old_claim", dir, fixturekit.DemoStart)
	intakefixture.Must(t, err)
	intakefixture.Must(t, repo.Close())
	clock := &fixturekit.FixedClock{At: fixturekit.DemoStart.Add(time.Minute)}
	calls := make(chan coreadapter.PreparedTurn, 1)
	opts.Reconciliation.Now = clock.Now
	opts.Threads = func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
		return thread.Dispatcher{Runner: thread.Runner{Jev: fixturekit.UnconfiguredJudge(nil), Store: r, Now: clock.Now, Turns: fixturekit.TurnsFunc(func(_ context.Context, input coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
			calls <- input
			return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "codex", ID: "recovered"}, FinalResponse: "Continued"}, nil
		})}, Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
			path := filepath.Join(cfg.Root.String(), "threads", string(cfg.Project.ID), string(in.Workstream), in.Agent, in.Turn)
			return coreadapter.PreparedTurn{SessionDirectory: path}, os.MkdirAll(path, 0700)
		}}, nil
	}
	s, err := Start(ctx, opts)
	intakefixture.Must(t, err)
	defer s.Close()
	select {
	case call := <-calls:
		if call.Scope.Role != trace.ChiefOfStaff || !strings.Contains(call.Prompt, "service stopped") || !strings.Contains(call.Prompt, "Remember this request") {
			t.Fatalf("chief recovery call: %+v", call)
		}
	case <-time.After(intakefixture.DemoTimeout):
		t.Fatal("chief recovery turn was not dispatched")
	}
}

func TestStartupPreservesHardPausedTurn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts := fixtureAt(t, t.TempDir())
	cfg, err := projectstore.LoadConfig(opts.Config)
	intakefixture.Must(t, err)
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, fixturekit.DemoStart, ownerActor)
	intakefixture.Must(t, err)
	intakefixture.Must(t, repo.CreateWorkstream(ctx, intakefixture.Stream, fixturekit.DemoStart, ownerActor))
	const agent = "paused_mason"
	intakefixture.Must(t, repo.CreateThread(ctx, trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, Revision: 1, ID: agent, Project: cfg.Project.ID, Workstream: intakefixture.Stream, At: fixturekit.DemoStart, Actor: ownerActor, Cause: "workstream_created"}, Role: masonRole, ThreadID: agent}))
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, Revision: 1, ID: "paused_request", Project: cfg.Project.ID, Workstream: intakefixture.Stream, At: fixturekit.DemoStart, Actor: ownerActor, Cause: "workstream_created"}, AgentID: agent, ThreadID: agent, TurnID: "paused_turn", Profile: coreadapter.Profile{Name: "default", Backend: "codex", Model: "test"}, Prompt: "Build"})
	intakefixture.Must(t, err)
	dir := filepath.Join(cfg.Root.String(), "threads", string(cfg.Project.ID), string(intakefixture.Stream), agent, "paused_turn")
	q, err := repo.ClaimTurn(ctx, intakefixture.Stream, agent, "pause_claim", dir, fixturekit.DemoStart)
	intakefixture.Must(t, err)
	h := q.Request.Header
	h.Schema, h.ID, h.At, h.Actor = "osmia.trace.turn-response", trace.EventID(q.Request.ID, "response"), fixturekit.DemoStart, recoveryActor
	res := trace.TurnResponse{Header: h, AgentID: agent, ThreadID: agent, TurnID: q.Request.TurnID, RequestID: q.Request.ID, RequestRevision: 1,
		Result: coreadapter.SessionResult{SessionDirectory: dir, StartedAt: fixturekit.DemoStart, Cancelled: true},
		Stop:   &trace.TurnStop{Cause: trace.TurnStopHardPause, Scope: "workstream", Source: runtime.PauseOwner, Reason: "rest"}}
	intakefixture.Must(t, repo.CaptureTurn(ctx, q.Claim.Token, res))
	intakefixture.Must(t, repo.CompleteTurn(ctx, intakefixture.Stream, agent, q.Request.TurnID, q.Claim.Token, fixturekit.DemoStart))
	intakefixture.Must(t, repo.Close())
	repo, err = trace.Open(cfg.Root, cfg.Project)
	intakefixture.Must(t, err)
	defer repo.Close()
	s := &Service{cfg: cfg, options: opts}
	intakefixture.Must(t, s.recoverSessions(ctx, cfg, repo))
	th, err := repo.Thread(intakefixture.Stream, agent)
	intakefixture.Must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].Response.Stop == nil || th.Turns[0].Response.Failure != "" || th.Turns[0].Status() != "interrupted" || interruption(th.Turns[0]) != "A hard pause stopped your last turn." {
		t.Fatalf("paused work changed on restart: %+v", th)
	}
}
