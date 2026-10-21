package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

func conversationAwait(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.After(demoTimeout)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !condition() {
		select {
		case <-deadline:
			t.Fatal("conversation path did not settle")
		case <-tick.C:
		}
	}
}

func TestConversationQueueSkipsUnreadableWorkstream(t *testing.T) {
	t.Parallel()
	opts, cfg := conversationFixture(t, "conversation-scan-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	t.Cleanup(func() { repo.Close() })
	// The unreadable trace sorts before the live thread. One scan failure
	// must not keep the remaining workstreams from publishing their turns.
	broken := config.WorkstreamID("w_00000000000000000000000000000000")
	dir, err := cfg.Root.WorkstreamTrace(cfg.Project.ID, broken)
	must(t, err)
	must(t, os.MkdirAll(dir, 0700))
	profile, err := cfg.NamedProfile("default")
	must(t, err)
	ctx := context.Background()
	q, err := repo.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_scan", Project: repo.Project(), Workstream: stream, At: opts.Reconciliation.Now(), Actor: ownerActor, Cause: "owner-message"}, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "message_scan", Profile: profile, Prompt: "Hello"})
	must(t, err)
	p := &conversationPath{s: &Service{options: opts}, repository: repo}
	must(t, p.queue(ctx, repo))
	records, err := repo.Operations(stream)
	must(t, err)
	if len(records) != 1 {
		t.Fatalf("published operations: %+v", records)
	}
	in, err := thread.DecodeTurn(records[0].Operation)
	must(t, err)
	if in.Turn != q.Request.TurnID {
		t.Fatalf("published turn: %+v", in)
	}
}

func TestConversationRecoveryOrdersPendingHooksByThreadSequence(t *testing.T) {
	t.Parallel()
	th := trace.Thread{Identity: trace.Agent{Header: trace.Header{ID: trace.ChiefOfStaff}}, Turns: []trace.QueuedTurn{
		{Request: trace.TurnRequest{TurnID: "earlier"}, CompletedAt: demoStart},
		{Request: trace.TurnRequest{TurnID: "later"}},
	}}
	operation := func(turn string) trace.OperationRecord {
		op, err := thread.TurnOperation(project, turn, thread.TurnInput{Workstream: stream, Agent: trace.ChiefOfStaff, Turn: turn})
		must(t, err)
		return trace.OperationRecord{Operation: op}
	}
	// Durable operation scans sort by hashed event ID. A later turn can be
	// encountered first even when an earlier completed turn's hook is pending.
	records := []trace.OperationRecord{operation("later"), operation("earlier")}
	if !pendingConversation(records, th, "later") {
		t.Fatal("later turn passed the earlier turn's unsettled hook")
	}
	if pendingConversation(records, th, "earlier") {
		t.Fatal("later intent prevented the earlier hook from settling")
	}
	records[1].Acknowledged = true
	if pendingConversation(records, th, "later") {
		t.Fatal("settled earlier hook held the later turn")
	}
}

// A scheduling hook and a workflow attempt on the chief's own workstream
// remain held while all three message APIs finish their turns. No scheduler
// tick is supplied, and capacity and a factory pause cannot exclude the chief.
func TestConversationPathBypassesSchedulingAndOwnWorkflow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, cfg := conversationFixture(t, "conversation-path-")
	turns := &chiefTurns{}
	runChief(&opts, cfg, turns)
	opts.Reconciliation.Ticks = make(chan time.Time)
	opts.conversationTicks = make(chan time.Time)
	s, client := start(t, opts)
	repo := s.sole().repository
	design, err := client.OpenDesign(ctx, cfg.Project.ID)
	must(t, err)
	session := config.WorkstreamID("w_00000000000000000000000000000111")
	must(t, repo.CreateSessionWorkstream(ctx, session, "default", "base", "session-test", "Session", s.now(), ownerActor))
	_, err = ensureSessionThread(ctx, repo, cfg.Project.ID, session, s.now())
	must(t, err)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	stage := *s.sole().pipeline.current.Load()
	stage.hooks = []scheduleHook{{"held", func(ctx context.Context) error {
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
	s.sole().pipeline.next.Store(&stage)
	repo.WakeWorkflow()
	select {
	case <-entered:
	case <-time.After(demoTimeout):
		t.Fatal("schedule was not held")
	}
	// A durable repository operation holds the workflow class directly; the
	// fake has no Git effect and cannot be started by any other controller.
	event := "held-conversation-workflow"
	op := coreadapter.Operation{ID: trace.OperationID(cfg.Project.ID, stream, event), Boundary: coreadapter.RepositoryBoundary, Action: "held-conversation-workflow", Input: json.RawMessage(`{}`)}
	_, err = repo.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: 1, ID: event, Revision: 1, Project: cfg.Project.ID, Workstream: stream, At: s.now(), Actor: serviceActor, Cause: "test"}, Subject: "fixture", To: "held", Reason: "Hold the workflow"}, Events: []trace.Event{{ID: event, Kind: "fixture", Body: "Hold the workflow", Operation: &op}}})
	must(t, err)
	attempt, _, err := repo.BeginOperation(ctx, stream, event, trace.WorkflowOperation, serviceActor, s.now)
	must(t, err)
	if attempt == nil {
		t.Fatal("fixture did not reserve workflow")
	}
	t.Cleanup(func() { attempt.Close() })

	chief, err := client.Send(ctx, stream, "Chief message")
	must(t, err)
	designed, err := client.DesignMessage(ctx, design.Design.Workstream, "Design message")
	must(t, err)
	sessionMessage, err := client.SessionMessage(ctx, session, "Session message")
	must(t, err)
	for _, test := range []struct {
		stream      config.WorkstreamID
		agent, turn string
	}{{stream, trace.ChiefOfStaff, chief.Turn}, {design.Design.Workstream, trace.ChiefOfStaff, designed.Turn}, {session, sessionAgentID, sessionMessage.Turn}} {
		conversationAwait(t, func() bool {
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
		must(t, err)
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
	for role, used := range s.pool.Slots([]config.ProjectID{cfg.Project.ID}).Used {
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
	opts, cfg := conversationFixture(t, "conversation-order-")
	turns := &chiefTurns{Hold: make(chan struct{}), Entered: make(chan struct{})}
	runChief(&opts, cfg, turns)
	opts.Reconciliation.Ticks = make(chan time.Time)
	opts.conversationTicks = make(chan time.Time)
	s, client := start(t, opts)
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(turns.Hold) }) })
	repo := s.sole().repository
	profile, err := cfg.NamedProfile("default")
	must(t, err)
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_background", Project: cfg.Project.ID, Workstream: stream, At: s.now(), Actor: serviceActor, Cause: "event"}, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "events_background", Profile: profile, Prompt: "Background"})
	must(t, err)
	select {
	case <-turns.Entered:
	case <-time.After(demoTimeout):
		t.Fatal("enqueue did not wake path")
	}
	sent, err := client.Send(ctx, stream, "Owner")
	must(t, err)
	if sent.QueuedReason != "waiting for an event turn on this thread" {
		t.Fatalf("queued response: %+v", sent)
	}
	list, err := client.Conversation(ctx, stream)
	must(t, err)
	if len(list.Entries) != 1 || list.Entries[0].QueuedReason != sent.QueuedReason {
		t.Fatalf("queued view: %+v", list)
	}
	if calls := turns.Calls(); len(calls) != 1 || calls[0].Prompt != "Background" {
		t.Fatalf("owner jumped queue: %+v", calls)
	}
	release.Do(func() { close(turns.Hold) })
	awaitConversation(t, client, func(r ConversationResponse) bool { return len(r.Entries) == 2 && r.Entries[0].State == TurnDone })
	if calls := turns.Calls(); len(calls) != 2 || calls[1].Prompt != "Owner" {
		t.Fatalf("turn order: %+v", calls)
	}
}

// The first relay persists context-rebuilt, then fails. Restart must inspect
// the completed turn, invoke sessionRelay again and acknowledge only once the
// idle transition is durable. A second restart must repeat no backend effect.
func TestConversationPathSettlesCompletionHookAfterFailureOrRestart(t *testing.T) {
	t.Parallel()
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "retry", true: "restart"}[restart], func(t *testing.T) {
			ctx := context.Background()
			opts, cfg := conversationFixture(t, "conversation-hook-")
			clock := &demoClock{At: demoStart}
			opts.Reconciliation.Now = clock.Now
			opts.Reconciliation.RetryDelay = time.Hour
			var calls, relays atomic.Int32
			opts.Threads = func(repo *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
				return thread.Dispatcher{Runner: thread.Runner{Store: repo, Now: clock.Now, Turns: turnsFunc(func(_ context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
					calls.Add(1)
					return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: p.Profile.Backend, ID: "session"}, FinalResponse: "Reply"}, nil
				})}, Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
					return coreadapter.PreparedTurn{SessionDirectory: filepath.Join(cfg.Root.String(), "sessions", in.Turn)}, nil
				}, Relay: func(ctx context.Context, th trace.Thread, q trace.QueuedTurn) error {
					if th.Identity.Role != sessionRole {
						return nil
					}
					q.Attempts = []trace.TurnAttempt{{Path: "replay", SourceSession: coreadapter.BackendSession{ID: "old", Backend: "fake"}}}
					if relays.Add(1) == 1 {
						if err := recordSessionEntry(ctx, repo, repo.Project(), q.Request.Workstream, "context-rebuilt-"+q.Request.TurnID, clock.Now(), "Your earlier model session could not be resumed, so this turn's context was rebuilt from the thread's history."); err != nil {
							return err
						}
						return errors.New("interrupted between relay effects")
					}
					return sessionRelay(ctx, repo, clock.Now, th, q)
				}}, nil
			}
			s, client := start(t, opts)
			repo := s.sole().repository
			session := config.WorkstreamID("w_00000000000000000000000000000112")
			must(t, repo.CreateSessionWorkstream(ctx, session, "default", "base", "session-hook", "Session", clock.Now(), ownerActor))
			sent, err := client.SessionMessage(ctx, session, "Hello")
			must(t, err)
			conversationAwait(t, func() bool {
				records, _ := repo.Operations(session)
				for _, r := range records {
					if len(r.History) > 0 && r.History[len(r.History)-1].Kind == "retry" {
						return true
					}
				}
				return false
			})
			th, err := repo.Thread(session, sessionAgentID)
			must(t, err)
			if len(th.Turns) != 1 || th.Turns[0].CompletedAt.IsZero() {
				t.Fatal("hook failed before completion")
			}
			state, err := repo.Workflow(session, trace.SessionSubject)
			must(t, err)
			if state.Value != trace.SessionWorking {
				t.Fatalf("session state: %s", state.Value)
			}
			if restart {
				must(t, s.Close())
			}
			jump(clock, clock.Now().Add(2*time.Hour))
			if restart {
				s, _ = start(t, opts)
			} else {
				s.conversationSignal.wake()
			}
			repo = s.sole().repository
			conversationAwait(t, func() bool {
				records, _ := repo.Operations(session)
				return len(records) == 1 && records[0].Acknowledged
			})
			state, err = repo.Workflow(session, trace.SessionSubject)
			must(t, err)
			if state.Value != trace.SessionIdle || calls.Load() != 1 || relays.Load() != 2 {
				t.Fatalf("recovery state=%s calls=%d relays=%d", state.Value, calls.Load(), relays.Load())
			}
			entries, err := sessionEntriesSince(repo, session, time.Time{})
			must(t, err)
			if len(entries) != 1 || entries[0].ID != "context-rebuilt-"+sent.Turn {
				t.Fatalf("relay effects: %+v", entries)
			}
			transitions, err := trace.Read[trace.Transition](repo, session)
			must(t, err)
			idle := 0
			for _, tr := range transitions {
				if strings.HasPrefix(tr.ID, "session-idle-") {
					idle++
				}
			}
			if idle != 1 {
				t.Fatalf("idle transitions: %d", idle)
			}
			must(t, s.Close())
			s, _ = start(t, opts)
			conversationAwait(t, func() bool {
				records, _ := s.sole().repository.Operations(session)
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
	opts, cfg := conversationFixture(t, "conversation-drain-")
	entered, stopping, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	recorded := make(chan error, 1)
	opts.Threads = func(repo *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
		return thread.Dispatcher{Runner: thread.Runner{Store: repo, Now: opts.Reconciliation.Now, Turns: turnsFunc(func(ctx context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
			close(entered)
			<-ctx.Done()
			close(stopping)
			<-release
			recorded <- recordSessionEntry(context.Background(), repo, repo.Project(), config.WorkstreamID(p.Scope.Workstream), "drained", demoStart, "Turn stopped before closing the trace.")
			return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: p.Profile.Backend, ID: "stopped"}, FinalResponse: "Stopped", Cancelled: true}, nil
		})}, Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
			return coreadapter.PreparedTurn{SessionDirectory: filepath.Join(cfg.Root.String(), "sessions", in.Turn)}, nil
		}}, nil
	}
	s, client := start(t, opts)
	var released sync.Once
	t.Cleanup(func() { released.Do(func() { close(release) }) })
	_, err := client.Send(context.Background(), stream, "Wait for shutdown")
	must(t, err)
	select {
	case <-entered:
	case <-time.After(demoTimeout):
		t.Fatal("turn did not start")
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case <-stopping:
	case <-time.After(demoTimeout):
		t.Fatal("shutdown did not stop turn")
	}
	select {
	case <-closed:
		t.Fatal("shutdown returned before joining turn")
	default:
	}
	released.Do(func() { close(release) })
	must(t, <-recorded)
	must(t, <-closed)
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	t.Cleanup(func() { repo.Close() })
	th, err := repo.ChiefOfStaffThread(stream)
	must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].Response == nil || th.Turns[0].CompletedAt.IsZero() {
		t.Fatalf("shutdown lost reply: %+v", th)
	}
}

func TestConversationPathChiefPauseAndProviderHolds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, cfg := conversationFixture(t, "conversation-gates-")
	turns := &chiefTurns{}
	runChief(&opts, cfg, turns)
	s, client := start(t, opts)
	must(t, s.setPause(runtime.Pause{Target: runtime.Target{Scope: "factory"}, Mode: "soft", Reason: "Owner pause", Source: runtime.PauseOwner, SetAt: s.now()}))
	sent, err := client.Send(ctx, stream, "Reachable during pause")
	must(t, err)
	awaitConversation(t, client, func(r ConversationResponse) bool { return len(r.Entries) == 2 })
	if len(turns.Calls()) != 1 {
		t.Fatal("chief did not run while paused")
	}
	repo := s.sole().repository
	records, err := repo.Operations(stream)
	must(t, err)
	conversationAwait(t, func() bool {
		records, _ = repo.Operations(stream)
		for _, r := range records {
			in, _ := thread.DecodeTurn(r.Operation)
			if in.Turn == sent.Turn {
				return r.Acknowledged
			}
		}
		return false
	})
	must(t, s.store.SetProviderLimit(runtime.ProviderLimit{Backend: "claude", Status: "blocked", SetAt: s.now()}))
	must(t, s.store.SetProviderLimit(runtime.ProviderLimit{Backend: "codex", Status: "blocked", SetAt: s.now()}))
	next, err := client.Send(ctx, stream, "Wait for provider")
	must(t, err)
	p := s.sole().conversations
	th, err := repo.ChiefOfStaffThread(stream)
	must(t, err)
	turn := th.Turns[len(th.Turns)-1]
	op, err := thread.TurnOperation(cfg.Project.ID, "provider-test", thread.TurnInput{Workstream: stream, Agent: trace.ChiefOfStaff, Turn: next.Turn})
	must(t, err)
	if !p.holds(repo, stream, op) || turn.Claim != nil || len(turns.Calls()) != 1 {
		t.Fatal("provider hold did not keep chief queued")
	}
	must(t, s.store.ClearProviderLimit("claude"))
	must(t, s.store.ClearProviderLimit("codex"))
	s.conversationSignal.wake()
	awaitConversation(t, client, func(r ConversationResponse) bool { return len(r.Entries) == 4 })
	if len(turns.Calls()) != 2 {
		t.Fatal("held turn did not resume")
	}
}

func TestConversationPathRestartReconcilesCapturedReplyAndSettledHook(t *testing.T) {
	t.Parallel()
	for _, completed := range []bool{false, true} {
		t.Run(map[bool]string{false: "captured", true: "completed-before-acknowledgement"}[completed], func(t *testing.T) {
			ctx := context.Background()
			opts, cfg := conversationFixture(t, "conversation-captured-")
			repo, err := trace.Open(cfg.Root, cfg.Project)
			must(t, err)
			profile, err := cfg.NamedProfile("default")
			must(t, err)
			at := demoStart.Add(time.Second)
			q, err := repo.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_captured", Project: cfg.Project.ID, Workstream: stream, At: demoStart, Actor: ownerActor, Cause: "owner-message"}, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "captured", Profile: profile, Prompt: "Captured message"})
			must(t, err)
			th, err := repo.ChiefOfStaffThread(stream)
			must(t, err)
			must(t, scheduler.Dispatch(ctx, repo, scheduler.Candidate{Project: cfg.Project.ID, Workstream: stream, Thread: th, Turn: q}, at))
			records, err := repo.Operations(stream)
			must(t, err)
			attempt, _, err := repo.BeginOperation(ctx, stream, records[0].EventID, trace.ConversationOperation, serviceActor, func() time.Time { return at })
			must(t, err)
			if attempt == nil {
				t.Fatal("operation not claimed")
			}
			q, err = repo.ClaimTurn(ctx, stream, trace.ChiefOfStaff, "captured-token", "/owned", at)
			must(t, err)
			result := coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: profile.Backend, ID: "session"}, FinalResponse: "Already captured", SessionDirectory: "/owned", StartedAt: at}
			a := trace.TurnAttempt{Number: 1, At: at, Profile: profile, Path: "replay", Reason: "fresh"}
			must(t, repo.RecordAttempt(ctx, stream, trace.ChiefOfStaff, q.Request.TurnID, q.Claim.Token, a))
			a.Result = &result
			must(t, repo.RecordAttempt(ctx, stream, trace.ChiefOfStaff, q.Request.TurnID, q.Claim.Token, a))
			h := q.Request.Header
			h.Schema, h.ID, h.At, h.Actor = "osmia.trace.turn-response", trace.EventID(q.Request.ID, "response"), at, serviceActor
			must(t, repo.CaptureTurn(ctx, q.Claim.Token, trace.TurnResponse{Header: h, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: q.Request.TurnID, RequestID: q.Request.ID, RequestRevision: 1, Result: result}))
			var relays atomic.Int32
			relay := func(ctx context.Context, r *trace.Repository) error {
				relays.Add(1)
				return recordSessionEntry(ctx, r, cfg.Project.ID, stream, "hook-effect", demoStart, "Hook settled")
			}
			if completed {
				must(t, repo.CompleteTurn(ctx, stream, trace.ChiefOfStaff, q.Request.TurnID, q.Claim.Token, at))
				must(t, relay(ctx, repo))
			}
			attempt.Close()
			must(t, repo.Close())
			opts.Threads = func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
				return thread.Dispatcher{Runner: thread.Runner{Store: r, Now: opts.Reconciliation.Now, Turns: turnsFunc(func(context.Context, coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
					t.Error("captured reply executed again")
					return coreadapter.SessionResult{}, errors.New("captured reply executed again")
				})}, Relay: func(ctx context.Context, _ trace.Thread, _ trace.QueuedTurn) error { return relay(ctx, r) }}, nil
			}
			s, _ := start(t, opts)
			repo = s.sole().repository
			conversationAwait(t, func() bool {
				records, _ := repo.Operations(stream)
				return len(records) == 1 && records[0].Acknowledged
			})
			th, err = repo.ChiefOfStaffThread(stream)
			must(t, err)
			if len(th.Turns) != 1 || th.Turns[0].CompletedAt.IsZero() || th.Turns[0].Response.Result.FinalResponse != result.FinalResponse || len(th.Turns[0].Attempts) != 1 {
				t.Fatalf("recovered turn: %+v", th)
			}
			entries, err := sessionEntriesSince(repo, stream, time.Time{})
			must(t, err)
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
	opts, cfg := conversationFixture(t, "session-relay-keys-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	t.Cleanup(func() { repo.Close() })
	ctx := context.Background()
	session := config.WorkstreamID("w_00000000000000000000000000000113")
	must(t, repo.CreateSessionWorkstream(ctx, session, "default", "base", "session-relay", "Session", demoStart, ownerActor))
	at := opts.Reconciliation.Now
	first := trace.QueuedTurn{Request: trace.TurnRequest{Header: trace.Header{Project: cfg.Project.ID, Workstream: session}, TurnID: "first"}}
	must(t, setSessionState(ctx, repo, cfg.Project.ID, session, at(), "session-working-first", "session-turn-started", trace.SessionWorking))
	must(t, sessionRelay(ctx, repo, at, trace.Thread{}, first))
	must(t, setSessionState(ctx, repo, cfg.Project.ID, session, at(), "session-working-next", "session-turn-started", trace.SessionWorking))
	must(t, sessionRelay(ctx, repo, at, trace.Thread{}, first))
	state, err := repo.Workflow(session, trace.SessionSubject)
	must(t, err)
	if state.Value != trace.SessionWorking {
		t.Fatalf("old hook reset next turn: %s", state.Value)
	}
}

func TestConversationPathSharedChiefRestartReconcilesHostOperation(t *testing.T) {
	t.Parallel()
	for _, completed := range []bool{false, true} {
		t.Run(map[bool]string{false: "captured", true: "completed"}[completed], func(t *testing.T) {
			ctx := context.Background()
			f := newCrossRepoWiringFixture(t, map[string]string{trace.ChiefOfStaff: trace.ChiefOfStaff})
			s, err := Start(ctx, f.opts)
			must(t, err)
			t.Cleanup(func() { must(t, s.Close()) })
			_, api := s.DraftWorkgroup(ctx, []config.WorkstreamID{f.stream1, f.stream2})
			if api != nil {
				t.Fatal(api)
			}
			_, projects := s.runtimes()
			for _, project := range projects {
				project.conversations.Drain()
			}
			client := NewClient(s.Socket())
			t.Cleanup(func() { client.Close() })
			sent, err := client.Send(ctx, f.stream1, "Shared captured message")
			must(t, err)
			repo, err := s.repository(f.project1)
			must(t, err)
			wt, host, _, closeTarget, err := s.chiefRepository(ctx, repo, f.stream1)
			must(t, err)
			th, err := wt.Thread(host, trace.ChiefOfStaff)
			must(t, err)
			if len(th.Turns) != 1 {
				closeTarget()
				t.Fatalf("shared queue: %+v", th)
			}
			q := th.Turns[0]
			at := f.clock.Now()
			must(t, scheduler.Dispatch(ctx, wt, scheduler.Candidate{Project: wt.Project(), Workstream: host, Thread: th, Turn: q}, at))
			records, err := wt.Operations(host)
			must(t, err)
			attempt, _, err := wt.BeginOperation(ctx, host, records[0].EventID, trace.ConversationOperation, serviceActor, f.clock.Now)
			must(t, err)
			if attempt == nil {
				closeTarget()
				t.Fatal("host operation not claimed")
			}
			q, err = wt.ClaimTurn(ctx, host, trace.ChiefOfStaff, "shared-token", "/owned-shared", at)
			must(t, err)
			result := coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: q.Request.Profile.Backend, ID: "shared-session"}, FinalResponse: "Shared captured reply", SessionDirectory: "/owned-shared", StartedAt: at}
			a := trace.TurnAttempt{Number: 1, At: at, Profile: q.Request.Profile, Path: "replay", Reason: "fresh"}
			must(t, wt.RecordAttempt(ctx, host, trace.ChiefOfStaff, q.Request.TurnID, q.Claim.Token, a))
			a.Result = &result
			must(t, wt.RecordAttempt(ctx, host, trace.ChiefOfStaff, q.Request.TurnID, q.Claim.Token, a))
			h := q.Request.Header
			h.Schema, h.ID, h.Actor = "osmia.trace.turn-response", trace.EventID(q.Request.ID, "response"), serviceActor
			must(t, wt.CaptureTurn(ctx, q.Claim.Token, trace.TurnResponse{Header: h, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: q.Request.TurnID, RequestID: q.Request.ID, RequestRevision: 1, Result: result}))
			if completed {
				must(t, wt.CompleteTurn(ctx, host, trace.ChiefOfStaff, q.Request.TurnID, q.Claim.Token, at))
			}
			attempt.Close()
			closeTarget()
			must(t, s.Close())
			s, err = Start(ctx, f.opts)
			must(t, err)
			repo, err = s.repository(f.project1)
			must(t, err)
			wt, host, _, closeTarget, err = s.chiefRepository(ctx, repo, f.stream1)
			must(t, err)
			t.Cleanup(closeTarget)
			conversationAwait(t, func() bool { records, _ := wt.Operations(host); return len(records) == 1 && records[0].Acknowledged })
			th, err = wt.Thread(host, trace.ChiefOfStaff)
			must(t, err)
			if len(th.Turns) != 1 || th.Turns[0].Request.TurnID != sent.Turn || th.Turns[0].CompletedAt.IsZero() || th.Turns[0].Response.Result.FinalResponse != result.FinalResponse || len(th.Turns[0].Attempts) != 1 {
				t.Fatalf("shared recovery: %+v", th)
			}
			f.engine.Mu.Lock()
			calls := len(f.engine.Runs)
			f.engine.Mu.Unlock()
			if calls != 0 {
				t.Fatalf("captured shared reply made %d backend calls", calls)
			}
		})
	}
}
