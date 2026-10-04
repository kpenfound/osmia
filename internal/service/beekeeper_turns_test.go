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
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/service/beekeeper"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// Posting an owner message to the Beekeeper with no owner project
// registered anywhere in the service records it and runs a Beekeeper turn
// on a fake model session whose reply is recorded in the same thread.
func TestPostBeekeeperWithNoProjectRegistered(t *testing.T) {
	t.Parallel()
	opts, _ := projectFixture(t)
	calls := make(chan coreadapter.PreparedTurn, 1)
	opts.BeekeeperTurns = turnsFunc(func(_ context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		calls <- p
		return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "claude", ID: "session"}, FinalResponse: "Hello owner."}, nil
	})
	s, c := start(t, opts)
	defer c.Close()

	q, err := s.PostBeekeeper(context.Background(), "Which workstreams need me?")
	must(t, err)
	if q.Response == nil || q.Response.Result.FinalResponse != "Hello owner." {
		t.Fatalf("turn did not run: %+v", q)
	}
	select {
	case p := <-calls:
		if p.Prompt != "Which workstreams need me?" || p.SystemPrompt != beekeeper.RolePrompt {
			t.Fatalf("prepared turn: %+v", p)
		}
	default:
		t.Fatal("the beekeeper turn did not reach the model session")
	}
	busy, err := s.BeekeeperBusy()
	must(t, err)
	if busy {
		t.Fatal("beekeeper still busy after its turn finished")
	}
	msgs, _, err := beekeeper.Messages(s.Beekeeper(), 10)
	must(t, err)
	if len(msgs) != 2 || msgs[1].Text != "Hello owner." {
		t.Fatalf("beekeeper chat: %+v", msgs)
	}
}

// A registered project's capacity and runtime pauses do not delay or block
// a Beekeeper turn, and a registered project's runtime settings do not
// change the Beekeeper's: the Beekeeper's turn uses only the profile the
// service-level Beekeeper section names, even while the project is fully
// hard-paused and its chief of staff is bound to another profile.
func TestPostBeekeeperIgnoresRegisteredProjectCapacityAndSettings(t *testing.T) {
	t.Parallel()
	opts := fixtureAt(t, t.TempDir())
	file := filepath.Join(opts.Config.Root, "config.toml")
	data, err := os.ReadFile(file)
	must(t, err)
	addition := "\n[roles.chief_of_staff]\nprofile = \"other\"\n" +
		"[beekeeper]\nname = \"Hive\"\nprofile = \"default\"\nsandbox = \"none\"\n"
	must(t, os.WriteFile(file, append(data, []byte(addition)...), 0600))

	calls := make(chan coreadapter.PreparedTurn, 1)
	opts.BeekeeperTurns = turnsFunc(func(_ context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		calls <- p
		return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "claude", ID: "session"}, FinalResponse: "All clear."}, nil
	})
	s, c := start(t, opts)
	defer c.Close()

	mutation(t, c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "project", Project: project}, Mode: "hard", Reason: "Fully closed for this test", Source: "owner"})

	done := make(chan struct{})
	go func() {
		q, err := s.PostBeekeeper(context.Background(), "Status please")
		must(t, err)
		if q.Response == nil || q.Response.Result.FinalResponse != "All clear." {
			t.Errorf("turn did not run: %+v", q)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(demoTimeout):
		t.Fatal("the beekeeper turn did not run; a fully hard-paused registered project blocked it")
	}

	select {
	case p := <-calls:
		if p.Profile.Name != "default" || p.Profile.Backend != "claude" {
			t.Fatalf("beekeeper turn used the project's chief-of-staff profile instead of its own: %+v", p.Profile)
		}
	default:
		t.Fatal("the beekeeper turn did not reach the model session")
	}
}

// checkRecoveredContinuation checks that th's first turn was recovered as
// interrupted with a recorded failure and that its continuation turn, the
// thread's second, resumed from that failure and completed with want as its
// final response.
func checkRecoveredContinuation(t *testing.T, role string, th trace.Thread, want string) {
	t.Helper()
	if len(th.Turns) != 2 {
		t.Fatalf("%s: expected an interrupted turn and its continuation: %+v", role, th.Turns)
	}
	first, second := th.Turns[0], th.Turns[1]
	if first.Status() != "interrupted" || first.Response == nil || first.Response.Failure == "" {
		t.Fatalf("%s: first turn not recovered as interrupted: %+v", role, first)
	}
	if second.Request.Cause != first.Response.ID || !strings.Contains(second.Request.Prompt, "service stopped") {
		t.Fatalf("%s: continuation turn does not resume the interrupted one: %+v", role, second)
	}
	if second.Status() != "idle" || second.Response == nil || second.Response.Result.FinalResponse != want {
		t.Fatalf("%s: continuation turn did not complete: %+v", role, second)
	}
}

// After a restart, a turn a previous service session claimed without
// capturing a result ends interrupted with a recorded failure on both the
// Beekeeper's thread and a workstream's chief-of-staff thread, and both then
// get a continuation turn that resumes the request and completes, through
// the real reconciliation path each role's turns run on: Threads bound for
// the chief of staff and BeekeeperTurns for the Beekeeper, exactly as a
// running service binds them.
func TestBeekeeperTurnInterruptedByARestartContinuesLikeAnInterruptedChiefOfStaffTurn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts := fixtureAt(t, t.TempDir())
	cfg, err := config.Load(opts.Config)
	must(t, err)

	shadow, err := beekeeper.Open(ctx, cfg.Root, cfg.Beekeeper, demoStart)
	must(t, err)
	if _, err := beekeeper.EnsureThread(ctx, shadow, demoStart); err != nil {
		t.Fatal(err)
	}
	beeReq := trace.TurnRequest{
		Header:  trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, Revision: 1, ID: "request_bee_crash", Project: config.ShadowProjectID, Workstream: config.BeekeeperWorkstreamID, At: demoStart, Actor: trace.Actor{Kind: "owner", ID: "local"}, Cause: "owner-message"},
		AgentID: beekeeper.AgentID, ThreadID: beekeeper.ThreadID, TurnID: "message_bee_crash", Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, Prompt: "Were you listening?",
	}
	if _, err := shadow.EnqueueTurn(ctx, beeReq); err != nil {
		t.Fatal(err)
	}
	beeDir := filepath.Join(cfg.Root.String(), "threads", string(config.ShadowProjectID), string(config.BeekeeperWorkstreamID), beekeeper.AgentID, "message_bee_crash")
	if _, err := shadow.ClaimTurn(ctx, config.BeekeeperWorkstreamID, beekeeper.AgentID, "crashed_bee", beeDir, demoStart); err != nil {
		t.Fatal(err)
	}
	must(t, shadow.Close())

	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, demoStart, ownerActor)
	must(t, err)
	must(t, repo.CreateWorkstream(ctx, stream, demoStart, ownerActor))
	if _, err := repo.EnsureChiefOfStaff(ctx, stream, demoStart, ownerActor); err != nil {
		t.Fatal(err)
	}
	chiefReq := trace.TurnRequest{
		Header:  trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, Revision: 1, ID: "request_chief_crash", Project: cfg.Project.ID, Workstream: stream, At: demoStart, Actor: ownerActor, Cause: "owner-message"},
		AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "chief_crash", Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, Prompt: "Status?",
	}
	if _, err := repo.EnqueueTurn(ctx, chiefReq); err != nil {
		t.Fatal(err)
	}
	chiefDir := filepath.Join(cfg.Root.String(), "threads", string(cfg.Project.ID), string(stream), trace.ChiefOfStaff, "chief_crash")
	if _, err := repo.ClaimTurn(ctx, stream, trace.ChiefOfStaff, "crashed_chief", chiefDir, demoStart); err != nil {
		t.Fatal(err)
	}
	must(t, repo.Close())

	clock := &fixedClock{now: demoStart.Add(time.Hour)}
	opts.Reconciliation.Now = clock.Now
	chiefCalls := make(chan coreadapter.PreparedTurn, 1)
	opts.Threads = func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
		return thread.Dispatcher{Runner: thread.Runner{Store: r, Now: clock.Now, Turns: turnsFunc(func(_ context.Context, input coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
			chiefCalls <- input
			return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "claude", ID: "recovered-chief"}, FinalResponse: "Chief continued."}, nil
		})}, Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
			path := filepath.Join(cfg.Root.String(), "threads", string(cfg.Project.ID), string(in.Workstream), in.Agent, in.Turn)
			return coreadapter.PreparedTurn{SessionDirectory: path}, os.MkdirAll(path, 0700)
		}}, nil
	}
	beeCalls := make(chan coreadapter.PreparedTurn, 1)
	opts.BeekeeperTurns = turnsFunc(func(_ context.Context, input coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		beeCalls <- input
		return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "claude", ID: "recovered-bee"}, FinalResponse: "Beekeeper continued."}, nil
	})

	s, err := Start(ctx, opts)
	must(t, err)
	defer s.Close()

	for role, calls := range map[string]chan coreadapter.PreparedTurn{"chief": chiefCalls, "beekeeper": beeCalls} {
		select {
		case call := <-calls:
			if !strings.Contains(call.Prompt, "service stopped") {
				t.Fatalf("%s recovery call: %+v", role, call)
			}
		case <-time.After(demoTimeout):
			t.Fatalf("%s recovery turn was not dispatched", role)
		}
	}

	deadline := time.Now().Add(demoTimeout)
	for {
		chiefThread, err := s.sole().repository.Thread(stream, trace.ChiefOfStaff)
		must(t, err)
		beeThread, err := s.Beekeeper().Thread(config.BeekeeperWorkstreamID, beekeeper.AgentID)
		must(t, err)
		if !chiefThread.Turns[len(chiefThread.Turns)-1].CompletedAt.IsZero() && !beeThread.Turns[len(beeThread.Turns)-1].CompletedAt.IsZero() {
			checkRecoveredContinuation(t, "chief", chiefThread, "Chief continued.")
			checkRecoveredContinuation(t, "beekeeper", beeThread, "Beekeeper continued.")
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("continuation turns did not both complete: chief=%+v beekeeper=%+v", chiefThread.Turns, beeThread.Turns)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
