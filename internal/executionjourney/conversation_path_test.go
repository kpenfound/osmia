package executionjourney

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestConversationPathBypassesSchedulingAndOwnWorkflow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, cfg := journey.ConversationFixture(t, "conversation-path-")
	turns := &fixturekit.ManagerTurns{}
	journey.RunManager(&opts, cfg, turns)
	opts.Reconciliation.Ticks = make(chan time.Time)
	opts.ConversationTicks = make(chan time.Time)
	s, client := journey.StartT(t, opts)
	repo := s.Sole().Repository
	design, err := client.OpenDesign(ctx, cfg.Project.ID)
	fixturekit.Must(t, err)
	session := config.WorkstreamID("w_00000000000000000000000000000111")
	fixturekit.Must(t, repo.CreateSessionWorkstream(ctx, session, "default", "base", "session-test", "Session", s.Now(), intake.OwnerActor))
	_, err = service.TestEnsureSessionThread(ctx, repo, cfg.Project.ID, session, s.Now())
	fixturekit.Must(t, err)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	pipeline := journey.InspectForTest(s).Pipeline(s.Sole().Id)
	stage := *pipeline.Current.Load()
	stage.Hooks = []service.TestScheduleHook{{Name: "held", Pass: func(ctx context.Context) error {
		select {
		case <-entered:
		default:
			close(entered)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return nil
		}
	}}}
	pipeline.Next.Store(&stage)
	repo.WakeWorkflow()
	select {
	case <-entered:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("schedule was not held")
	}

	event := "held-conversation-workflow"
	op := coreadapter.Operation{ID: trace.OperationID(cfg.Project.ID, fixturekit.Stream, event), Boundary: coreadapter.RepositoryBoundary, Action: "held-conversation-workflow", Input: json.RawMessage(`{}`)}
	_, err = repo.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: 1, ID: event, Revision: 1, Project: cfg.Project.ID, Workstream: fixturekit.Stream, At: s.Now(), Actor: intake.ServiceActor, Cause: "test"}, Subject: "fixture", To: "held", Reason: "Hold the workflow"}, Events: []trace.Event{{ID: event, Kind: "fixture", Body: "Hold the workflow", Operation: &op}}})
	fixturekit.Must(t, err)
	attempt, _, err := repo.BeginOperation(ctx, fixturekit.Stream, event, trace.WorkflowOperation, intake.ServiceActor, s.Now)
	fixturekit.Must(t, err)
	if attempt == nil {
		t.Fatal("fixture did not reserve workflow")
	}
	t.Cleanup(func() { attempt.Close() })

	manager, err := client.Send(ctx, fixturekit.Stream, "Chief message")
	fixturekit.Must(t, err)
	designed, err := client.DesignMessage(ctx, design.Design.Workstream, "Design message")
	fixturekit.Must(t, err)
	sessionMessage, err := client.SessionMessage(ctx, session, "Session message")
	fixturekit.Must(t, err)
	for _, test := range []struct {
		stream      config.WorkstreamID
		agent, turn string
	}{{fixturekit.Stream, trace.Manager, manager.Turn}, {design.Design.Workstream, trace.Designer, designed.Turn}, {session, service.SessionAgentID, sessionMessage.Turn}} {
		fixturekit.ConversationAwait(t, func() bool {
			th, err := repo.Thread(test.stream, test.agent)
			if err != nil {
				return false
			}
			for _, q := range th.Turns {
				if q.Request.TurnID == test.turn {
					return q.Response != nil && q.Response.Result.FinalResponse != "" && !q.CompletedAt.IsZero()
				}
			}
			return false
		})
		records, err := repo.Operations(test.stream)
		fixturekit.Must(t, err)
		found := 0
		for _, record := range records {
			in, err := thread.DecodeTurn(record.Operation)
			if err == nil && in.Turn == test.turn {
				found++
			}
		}
		if found != 1 {
			t.Fatalf("turn has %d operations", found)
		}
	}
	for role, used := range journey.InspectForTest(s).Pool().Slots([]config.ProjectID{cfg.Project.ID}).Used {
		if used != 0 {
			t.Fatalf("conversation used %s capacity: %d", role, used)
		}
	}
	if len(turns.Calls()) != 3 {
		t.Fatalf("backend calls: %d", len(turns.Calls()))
	}
	select {
	case <-release:
		t.Fatal("schedule released early")
	default:
	}
	once.Do(func() { close(release) })
}

func TestConversationPathOwnerWaitsForBackgroundTurnAndShowsReason(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, cfg := journey.ConversationFixture(t, "conversation-order-")
	turns := &fixturekit.ManagerTurns{Hold: make(chan struct{}), Entered: make(chan struct{})}
	journey.RunManager(&opts, cfg, turns)
	opts.Reconciliation.Ticks = make(chan time.Time)
	opts.ConversationTicks = make(chan time.Time)
	s, client := journey.StartT(t, opts)
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(turns.Hold) }) })
	repo := s.Sole().Repository
	profile, err := cfg.NamedProfile("default")
	fixturekit.Must(t, err)
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_background", Project: cfg.Project.ID, Workstream: fixturekit.Stream, At: s.Now(), Actor: intake.ServiceActor, Cause: "event"}, AgentID: trace.Manager, ThreadID: trace.Manager, TurnID: "events_background", Profile: profile, Prompt: "Background"})
	fixturekit.Must(t, err)
	select {
	case <-turns.Entered:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("enqueue did not wake path")
	}
	sent, err := client.Send(ctx, fixturekit.Stream, "Owner")
	fixturekit.Must(t, err)
	if sent.QueuedReason != "waiting for an event turn on this thread" {
		t.Fatalf("queued response: %+v", sent)
	}
	list, err := client.Conversation(ctx, fixturekit.Stream)
	fixturekit.Must(t, err)
	if len(list.Entries) != 1 || list.Entries[0].QueuedReason != sent.QueuedReason {
		t.Fatalf("queued view: %+v", list)
	}
	if calls := turns.Calls(); len(calls) != 1 || calls[0].Prompt != "Background" {
		t.Fatalf("owner jumped queue: %+v", calls)
	}
	release.Do(func() { close(turns.Hold) })
	intakefixture.AwaitConversation(t, client, func(r api.ConversationResponse) bool {
		return len(r.Entries) == 2 && r.Entries[0].State == api.TurnDone
	})
	if calls := turns.Calls(); len(calls) != 2 || calls[1].Prompt != "Owner" {
		t.Fatalf("turn order: %+v", calls)
	}
}

func TestConversationPathSettlesCompletionHookAfterFailureOrRestart(t *testing.T) {
	t.Parallel()
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "retry", true: "restart"}[restart], func(t *testing.T) {
			ctx := context.Background()
			opts, cfg := journey.ConversationFixture(t, "conversation-hook-")
			clock := &fixturekit.DemoClock{At: fixturekit.DemoStart}
			opts.Reconciliation.Now = clock.Now
			opts.Reconciliation.RetryDelay = time.Hour
			var calls, relays atomic.Int32
			opts.Threads = func(repo *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
				return thread.Dispatcher{Runner: thread.Runner{Jev: fixturekit.UnconfiguredJudge(nil), Store: repo, Now: clock.Now, Turns: fixturekit.TurnsFunc(func(_ context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
					calls.Add(1)
					return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: p.Profile.Backend, ID: "session"}, FinalResponse: "Reply"}, nil
				})}, Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
					return coreadapter.PreparedTurn{SessionDirectory: filepath.Join(cfg.Root.String(), "sessions", in.Turn)}, nil
				}, Relay: func(ctx context.Context, th trace.Thread, q trace.QueuedTurn) error {
					if th.Identity.Role != service.SessionAgentID {
						return nil
					}
					q.Attempts = []trace.TurnAttempt{{Path: "replay", SourceSession: coreadapter.BackendSession{ID: "old", Backend: "fake"}}}
					if relays.Add(1) == 1 {
						if err := service.RecordSessionEntry(ctx, repo, repo.Project(), q.Request.Workstream, "context-rebuilt-"+q.Request.TurnID, clock.Now(), "Your earlier model session could not be resumed, so this turn's context was rebuilt from the thread's history."); err != nil {
							return err
						}
						return errors.New("interrupted between relay effects")
					}
					return service.SessionRelay(ctx, repo, clock.Now, th, q)
				}}, nil
			}
			s, client := journey.StartT(t, opts)
			repo := s.Sole().Repository
			session := config.WorkstreamID("w_00000000000000000000000000000112")
			fixturekit.Must(t, repo.CreateSessionWorkstream(ctx, session, "default", "base", "session-hook", "Session", clock.Now(), intake.OwnerActor))
			sent, err := client.SessionMessage(ctx, session, "Hello")
			fixturekit.Must(t, err)
			fixturekit.ConversationAwait(t, func() bool {
				records, _ := repo.Operations(session)
				for _, r := range records {
					if len(r.History) > 0 && r.History[len(r.History)-1].Kind == "retry" {
						return true
					}
				}
				return false
			})
			th, err := repo.Thread(session, service.SessionAgentID)
			fixturekit.Must(t, err)
			if len(th.Turns) != 1 || th.Turns[0].CompletedAt.IsZero() {
				t.Fatal("hook failed before completion")
			}
			state, err := repo.Workflow(session, trace.SessionSubject)
			fixturekit.Must(t, err)
			if state.Value != trace.SessionWorking {
				t.Fatalf("session state: %s", state.Value)
			}
			if restart {
				fixturekit.Must(t, s.Close())
			}
			fixturekit.Jump(clock, clock.Now().Add(2*time.Hour))
			if restart {
				s, _ = journey.StartT(t, opts)
			} else {
				journey.InspectForTest(s).WakeConversations()
			}
			repo = s.Sole().Repository
			fixturekit.ConversationAwait(t, func() bool {
				records, _ := repo.Operations(session)
				return len(records) == 1 && records[0].Acknowledged
			})
			state, err = repo.Workflow(session, trace.SessionSubject)
			fixturekit.Must(t, err)
			if state.Value != trace.SessionIdle || calls.Load() != 1 || relays.Load() != 2 {
				t.Fatalf("recovery state=%s calls=%d relays=%d", state.Value, calls.Load(), relays.Load())
			}
			entries, err := service.SessionEntriesSince(repo, session, time.Time{})
			fixturekit.Must(t, err)
			if len(entries) != 1 || entries[0].ID != "context-rebuilt-"+sent.Turn {
				t.Fatalf("relay effects: %+v", entries)
			}
			transitions, err := trace.Read[trace.Transition](repo, session)
			fixturekit.Must(t, err)
			idle := 0
			for _, tr := range transitions {
				if strings.HasPrefix(tr.ID, "session-idle-") {
					idle++
				}
			}
			if idle != 1 {
				t.Fatalf("idle transitions: %d", idle)
			}
			fixturekit.Must(t, s.Close())
			s, _ = journey.StartT(t, opts)
			fixturekit.ConversationAwait(t, func() bool {
				records, _ := s.Sole().Repository.Operations(session)
				return len(records) == 1 && records[0].Acknowledged
			})
			if calls.Load() != 1 || relays.Load() != 2 {
				t.Fatal("acknowledged hook replayed")
			}

		})
	}
}

func TestConversationPathShutdownJoinsBeforeTraceClose(t *testing.T) {
	t.Parallel()
	opts, cfg := journey.ConversationFixture(t, "conversation-drain-")
	entered, stopping, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	recorded := make(chan error, 1)
	opts.Threads = func(repo *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
		return thread.Dispatcher{Runner: thread.Runner{Jev: fixturekit.UnconfiguredJudge(nil), Store: repo, Now: opts.Reconciliation.Now, Turns: fixturekit.TurnsFunc(func(ctx context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
			close(entered)
			<-ctx.Done()
			close(stopping)
			<-release
			recorded <- service.RecordSessionEntry(context.Background(), repo, repo.Project(), config.WorkstreamID(p.Scope.Workstream), "drained", fixturekit.DemoStart, "Turn stopped before closing the trace.")
			return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: p.Profile.Backend, ID: "stopped"}, FinalResponse: "Stopped", Cancelled: true}, nil
		})}, Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
			return coreadapter.PreparedTurn{SessionDirectory: filepath.Join(cfg.Root.String(), "sessions", in.Turn)}, nil
		}}, nil
	}
	s, client := journey.StartT(t, opts)
	var released sync.Once
	t.Cleanup(func() { released.Do(func() { close(release) }) })
	_, err := client.Send(context.Background(), fixturekit.Stream, "Wait for shutdown")
	fixturekit.Must(t, err)
	select {
	case <-entered:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("turn did not start")
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case <-stopping:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("shutdown did not stop turn")
	}
	select {
	case <-closed:
		t.Fatal("shutdown returned before joining turn")
	default:
	}
	released.Do(func() { close(release) })
	fixturekit.Must(t, <-recorded)
	fixturekit.Must(t, <-closed)
	repo, err := trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	t.Cleanup(func() { repo.Close() })
	th, err := repo.ManagerThread(fixturekit.Stream)
	fixturekit.Must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].Response == nil || th.Turns[0].CompletedAt.IsZero() {
		t.Fatalf("shutdown lost reply: %+v", th)
	}
}

func TestConversationPathManagerPauseAndProviderHolds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, cfg := journey.ConversationFixture(t, "conversation-gates-")
	turns := &fixturekit.ManagerTurns{}
	journey.RunManager(&opts, cfg, turns)
	s, client := journey.StartT(t, opts)
	fixturekit.Must(t, journey.InspectForTest(s).SetPause(runtime.Pause{Target: runtime.Target{Scope: "factory"}, Mode: "soft", Reason: "Owner pause", Source: runtime.PauseOwner, SetAt: s.Now()}))
	sent, err := client.Send(ctx, fixturekit.Stream, "Reachable during pause")
	fixturekit.Must(t, err)
	intakefixture.AwaitConversation(t, client, func(r api.ConversationResponse) bool { return len(r.Entries) == 2 })
	if len(turns.Calls()) != 1 {
		t.Fatal("chief did not run while paused")
	}
	repo := s.Sole().Repository
	records, err := repo.Operations(fixturekit.Stream)
	fixturekit.Must(t, err)
	fixturekit.ConversationAwait(t, func() bool {
		records, _ = repo.Operations(fixturekit.Stream)
		for _, r := range records {
			in, _ := thread.DecodeTurn(r.Operation)
			if in.Turn == sent.Turn {
				return r.Acknowledged
			}
		}
		return false
	})
	fixturekit.Must(t, s.Store.SetProviderLimit(runtime.ProviderLimit{Backend: "claude", Status: "blocked", SetAt: s.Now()}))
	fixturekit.Must(t, s.Store.SetProviderLimit(runtime.ProviderLimit{Backend: "codex", Status: "blocked", SetAt: s.Now()}))
	next, err := client.Send(ctx, fixturekit.Stream, "Wait for provider")
	fixturekit.Must(t, err)
	holds := func(repo *trace.Repository, stream config.WorkstreamID, op coreadapter.Operation) bool {
		return journey.InspectForTest(s).ConversationHolds(repo.Project(), repo, stream, op)
	}
	th, err := repo.ManagerThread(fixturekit.Stream)
	fixturekit.Must(t, err)
	turn := th.Turns[len(th.Turns)-1]
	op, err := thread.TurnOperation(cfg.Project.ID, "provider-test", thread.TurnInput{Workstream: fixturekit.Stream, Agent: trace.Manager, Turn: next.Turn})
	fixturekit.Must(t, err)
	if !holds(repo, fixturekit.Stream, op) || turn.Claim != nil || len(turns.Calls()) != 1 {
		t.Fatal("provider hold did not keep chief queued")
	}
	fixturekit.Must(t, s.Store.ClearProviderLimit("claude"))
	fixturekit.Must(t, s.Store.ClearProviderLimit("codex"))
	journey.InspectForTest(s).WakeConversations()
	intakefixture.AwaitConversation(t, client, func(r api.ConversationResponse) bool { return len(r.Entries) == 4 })
	if len(turns.Calls()) != 2 {
		t.Fatal("held turn did not resume")
	}
}

func TestConversationPathRestartReconcilesCapturedReplyAndSettledHook(t *testing.T) {
	t.Parallel()
	for _, completed := range []bool{false, true} {
		t.Run(map[bool]string{false: "captured", true: "completed-before-acknowledgement"}[completed], func(t *testing.T) {
			ctx := context.Background()
			opts, cfg := journey.ConversationFixture(t, "conversation-captured-")
			repo, err := trace.Open(cfg.Root, cfg.Project)
			fixturekit.Must(t, err)
			profile, err := cfg.NamedProfile("default")
			fixturekit.Must(t, err)
			at := fixturekit.DemoStart.Add(time.Second)
			q, err := repo.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_captured", Project: cfg.Project.ID, Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: intake.OwnerActor, Cause: "owner-message"}, AgentID: trace.Manager, ThreadID: trace.Manager, TurnID: "captured", Profile: profile, Prompt: "Captured message"})
			fixturekit.Must(t, err)
			th, err := repo.ManagerThread(fixturekit.Stream)
			fixturekit.Must(t, err)
			fixturekit.Must(t, scheduler.Dispatch(ctx, repo, scheduler.Candidate{Project: cfg.Project.ID, Workstream: fixturekit.Stream, Thread: th, Turn: q}, at))
			records, err := repo.Operations(fixturekit.Stream)
			fixturekit.Must(t, err)
			attempt, _, err := repo.BeginOperation(ctx, fixturekit.Stream, records[0].EventID, trace.ConversationOperation, intake.ServiceActor, func() time.Time { return at })
			fixturekit.Must(t, err)
			if attempt == nil {
				t.Fatal("operation not claimed")
			}
			q, err = repo.ClaimTurn(ctx, fixturekit.Stream, trace.Manager, "captured-token", "/owned", at)
			fixturekit.Must(t, err)
			result := coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: profile.Backend, ID: "session"}, FinalResponse: "Already captured", SessionDirectory: "/owned", StartedAt: at}
			a := trace.TurnAttempt{Number: 1, At: at, Profile: profile, Path: "replay", Reason: "fresh"}
			fixturekit.Must(t, repo.RecordAttempt(ctx, fixturekit.Stream, trace.Manager, q.Request.TurnID, q.Claim.Token, a))
			a.Result = &result
			fixturekit.Must(t, repo.RecordAttempt(ctx, fixturekit.Stream, trace.Manager, q.Request.TurnID, q.Claim.Token, a))
			h := q.Request.Header
			h.Schema, h.ID, h.At, h.Actor = "osmia.trace.turn-response", trace.EventID(q.Request.ID, "response"), at, intake.ServiceActor
			fixturekit.Must(t, repo.CaptureTurn(ctx, q.Claim.Token, trace.TurnResponse{Header: h, AgentID: trace.Manager, ThreadID: trace.Manager, TurnID: q.Request.TurnID, RequestID: q.Request.ID, RequestRevision: 1, Result: result}))
			var relays atomic.Int32
			relay := func(ctx context.Context, r *trace.Repository) error {
				relays.Add(1)
				return service.RecordSessionEntry(ctx, r, cfg.Project.ID, fixturekit.Stream, "hook-effect", fixturekit.DemoStart, "Hook settled")
			}
			if completed {
				fixturekit.Must(t, repo.CompleteTurn(ctx, fixturekit.Stream, trace.Manager, q.Request.TurnID, q.Claim.Token, at))
				fixturekit.Must(t, relay(ctx, repo))
			}
			attempt.Close()
			fixturekit.Must(t, repo.Close())
			opts.Threads = func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
				return thread.Dispatcher{Runner: thread.Runner{Jev: fixturekit.UnconfiguredJudge(nil), Store: r, Now: opts.Reconciliation.Now, Turns: fixturekit.TurnsFunc(func(context.Context, coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
					t.Error("captured reply executed again")
					return coreadapter.SessionResult{}, errors.New("captured reply executed again")
				})}, Relay: func(ctx context.Context, _ trace.Thread, _ trace.QueuedTurn) error { return relay(ctx, r) }}, nil
			}
			s, _ := journey.StartT(t, opts)
			repo = s.Sole().Repository
			fixturekit.ConversationAwait(t, func() bool {
				records, _ := repo.Operations(fixturekit.Stream)
				return len(records) == 1 && records[0].Acknowledged
			})
			th, err = repo.ManagerThread(fixturekit.Stream)
			fixturekit.Must(t, err)
			if len(th.Turns) != 1 || th.Turns[0].CompletedAt.IsZero() || th.Turns[0].Response.Result.FinalResponse != result.FinalResponse || len(th.Turns[0].Attempts) != 1 {
				t.Fatalf("recovered turn: %+v", th)
			}
			entries, err := service.SessionEntriesSince(repo, fixturekit.Stream, time.Time{})
			fixturekit.Must(t, err)
			expectedRelays := int32(1)
			if completed {
				expectedRelays++
			}
			if len(entries) != 1 || relays.Load() != expectedRelays {
				t.Fatalf("hook effects=%+v invocations=%d", entries, relays.Load())
			}
			if completed && !th.Turns[0].CompletedAt.Equal(at) {
				t.Fatal("completed turn completed twice")
			}
		})
	}
}

func TestSessionRelayRetryPreservesLaterTurnState(t *testing.T) {
	t.Parallel()
	opts, cfg := journey.ConversationFixture(t, "session-relay-keys-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	t.Cleanup(func() { repo.Close() })
	ctx := context.Background()
	session := config.WorkstreamID("w_00000000000000000000000000000113")
	fixturekit.Must(t, repo.CreateSessionWorkstream(ctx, session, "default", "base", "session-relay", "Session", fixturekit.DemoStart, intake.OwnerActor))
	at := opts.Reconciliation.Now
	first := trace.QueuedTurn{Request: trace.TurnRequest{Header: trace.Header{Project: cfg.Project.ID, Workstream: session}, TurnID: "first"}}
	fixturekit.Must(t, service.SetSessionState(ctx, repo, cfg.Project.ID, session, at(), "session-working-first", "session-turn-started", trace.SessionWorking))
	fixturekit.Must(t, service.SessionRelay(ctx, repo, at, trace.Thread{}, first))
	fixturekit.Must(t, service.SetSessionState(ctx, repo, cfg.Project.ID, session, at(), "session-working-next", "session-turn-started", trace.SessionWorking))
	fixturekit.Must(t, service.SessionRelay(ctx, repo, at, trace.Thread{}, first))
	state, err := repo.Workflow(session, trace.SessionSubject)
	fixturekit.Must(t, err)
	if state.Value != trace.SessionWorking {
		t.Fatalf("old hook reset next turn: %s", state.Value)
	}
}
