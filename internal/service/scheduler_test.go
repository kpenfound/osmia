package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/coreadapter/adaptertest"
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

func queueTurn(t *testing.T, repo *trace.Repository, turn string, at time.Time) {
	t.Helper()
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_" + turn, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: at, Actor: trace.Actor{Kind: "owner", ID: "local"}, Cause: "message_" + turn, Depth: 1},
		AgentID: fixturekit.DemoAgent, ThreadID: fixturekit.DemoThread, TurnID: turn, Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Owner message: " + turn}
	_, err := repo.EnqueueTurn(context.Background(), req)
	fixturekit.Must(t, err)
}

func turnOperations(t *testing.T, repo *trace.Repository) map[string]trace.OperationRecord {
	t.Helper()
	ops, err := repo.Operations(fixturekit.Stream)
	fixturekit.Must(t, err)
	byTurn := map[string]trace.OperationRecord{}
	for _, op := range ops {
		var in thread.TurnInput
		fixturekit.Must(t, json.Unmarshal(op.Operation.Input, &in))
		byTurn[in.Turn] = op
	}
	return byTurn
}

func TestServiceRunsQueuedTurnsAcrossRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "sc-")
	fixturekit.Must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := fixtureAt(t, home)
	cfg, err := projectstore.LoadConfig(opts.Config)
	fixturekit.Must(t, err)
	root := cfg.Root.String()

	clock := &fixturekit.DemoClock{At: fixturekit.DemoStart}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, clock.Now(), owner)
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo.CreateWorkstream(ctx, fixturekit.Stream, clock.Now(), owner))
	identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: fixturekit.DemoAgent, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: clock.Now(), Actor: owner, Cause: "workstream_created"}, Role: fixturekit.DemoRole, ThreadID: fixturekit.DemoThread}
	fixturekit.Must(t, repo.CreateThread(ctx, identity))
	queueTurn(t, repo, "first", clock.Now())
	fixturekit.Must(t, repo.Close())

	reply := func(turn string) adaptertest.Reply[coreadapter.SessionResult] {
		return adaptertest.Reply[coreadapter.SessionResult]{Value: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session"}, FinalResponse: "Answer " + turn}}
	}
	turns := &fixturekit.BlockingTurns{Fake: &adaptertest.Turns{Script: *adaptertest.NewScript[coreadapter.PreparedTurn](reply("first"), reply("second"))}, Hooks: map[string]func(context.Context){}}
	var bound sync.Mutex
	var live *trace.Repository
	opts.Threads = func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
		bound.Lock()
		live = r
		bound.Unlock()
		return thread.Dispatcher{Runner: thread.Runner{Jev: fixturekit.UnconfiguredJudge(nil), Store: r, Turns: turns, Now: clock.Now},
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				return coreadapter.PreparedTurn{SessionDirectory: filepath.Join(root, "sessions", in.Agent, in.Turn)}, nil
			}}, nil
	}
	ticks := make(chan time.Time)
	opts.Reconciliation.Now, opts.Reconciliation.Ticks = clock.Now, ticks

	entered := make(chan struct{})
	turns.Hooks["first"] = func(ctx context.Context) {
		close(entered)
		<-ctx.Done()
	}
	s, err := Start(ctx, opts)
	fixturekit.Must(t, err)
	select {
	case <-entered:
	case <-time.After(fixturekit.DemoTimeout):
		s.Close()
		t.Fatal("queued turn did not start")
	}
	bound.Lock()
	repo = live
	bound.Unlock()
	queueTurn(t, repo, "second", clock.Now())
	th, err := repo.Thread(fixturekit.Stream, fixturekit.DemoAgent)
	fixturekit.Must(t, err)
	if th.Active != "first" || len(th.Turns) != 2 || th.Turns[1].Claim != nil {
		t.Fatalf("mid-turn thread: %+v", th)
	}
	if ops := turnOperations(t, repo); len(ops) != 1 || ops["first"].Claim == nil {
		t.Fatalf("mid-turn operations: %+v", ops)
	}
	fixturekit.Must(t, s.Close())

	repo, err = trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	th, err = repo.Thread(fixturekit.Stream, fixturekit.DemoAgent)
	fixturekit.Must(t, err)
	if th.Active != "" || th.Turns[0].CompletedAt.IsZero() || th.Turns[1].Claim != nil {
		t.Fatalf("stopped thread: %+v", th)
	}
	ops := turnOperations(t, repo)
	if len(ops) != 1 || ops["first"].Result != nil || ops["first"].Acknowledged {
		t.Fatalf("stopped operations: %+v", ops)
	}
	fixturekit.Must(t, repo.Close())

	s, err = Start(ctx, opts)
	fixturekit.Must(t, err)
	fixturekit.AwaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord {
		bound.Lock()
		defer bound.Unlock()
		ops, err := live.Operations(fixturekit.Stream)
		fixturekit.Must(t, err)
		if len(ops) != 2 {
			return nil
		}
		return ops
	})
	fixturekit.Must(t, s.Close())

	repo, err = trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	defer repo.Close()
	calls := turns.Fake.Calls()
	var ran []string
	for _, call := range calls {
		ran = append(ran, call.Scope.Turn)
	}
	if !slices.Equal(ran, []string{"first", "second"}) {
		t.Fatalf("backend runs %v", ran)
	}
	th, err = repo.Thread(fixturekit.Stream, fixturekit.DemoAgent)
	fixturekit.Must(t, err)
	if th.Active != "" || th.Status != "idle" || len(th.Turns) != 2 {
		t.Fatalf("final thread: %+v", th)
	}
	for _, q := range th.Turns {
		if q.CompletedAt.IsZero() || len(q.Attempts) != 1 {
			t.Fatalf("turn %s: %+v", q.Request.TurnID, q)
		}
	}
	ops = turnOperations(t, repo)
	if len(ops) != 2 {
		t.Fatalf("operations %+v", ops)
	}
	for turn, op := range ops {
		if !op.Acknowledged || op.Result == nil || op.Result.Outcome != "idle" || op.Transition.Actor != scheduler.Actor || op.Transition.Cause != "request_"+turn {
			t.Fatalf("operation %s: %+v", turn, op)
		}
	}
	if op := ops["first"]; op.Observation == nil || op.Observation.State != coreadapter.EffectCompleted {
		t.Fatalf("first operation was not finished by inspection: %+v", op)
	}
}

func TestServicePauseHoldsWorkerTurnsUntilCleared(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "sp-")
	fixturekit.Must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := fixtureAt(t, home)
	cfg, err := projectstore.LoadConfig(opts.Config)
	fixturekit.Must(t, err)
	root := cfg.Root.String()

	clock := &fixturekit.DemoClock{At: fixturekit.DemoStart}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, clock.Now(), owner)
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo.CreateWorkstream(ctx, fixturekit.Stream, clock.Now(), owner))
	identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: fixturekit.DemoAgent, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: clock.Now(), Actor: owner, Cause: "workstream_created"}, Role: fixturekit.DemoRole, ThreadID: fixturekit.DemoThread}
	fixturekit.Must(t, repo.CreateThread(ctx, identity))
	fixturekit.Must(t, repo.Close())

	reply := adaptertest.Reply[coreadapter.SessionResult]{Value: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session"}, FinalResponse: "Answer"}}
	turns := &adaptertest.Turns{Script: *adaptertest.NewScript[coreadapter.PreparedTurn](reply, reply)}
	lives := make(chan *trace.Repository, 1)
	opts.Threads = func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
		lives <- r
		return thread.Dispatcher{Runner: thread.Runner{Jev: fixturekit.UnconfiguredJudge(nil), Store: r, Turns: turns, Now: clock.Now},
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				return coreadapter.PreparedTurn{SessionDirectory: filepath.Join(root, "sessions", in.Agent, in.Turn)}, nil
			}}, nil
	}
	ticks := make(chan time.Time)
	opts.Reconciliation.Now, opts.Reconciliation.Ticks = clock.Now, ticks
	var passed atomic.Int64
	opts.schedulePassed = func() { passed.Add(1) }
	s, c := start(t, opts)
	repo = <-lives
	tick := func() {
		t.Helper()
		select {
		case ticks <- clock.Now():
		case <-time.After(fixturekit.DemoTimeout):
			t.Fatal("reconciliation pass did not finish")
		}
	}
	ran := func() []string {
		var out []string
		for _, call := range turns.Calls() {
			out = append(out, call.Scope.Turn)
		}
		return out
	}
	await := func(what string, want []string) {
		t.Helper()
		deadline := time.Now().Add(fixturekit.DemoTimeout)
		for !slices.Equal(ran(), want) {
			if time.Now().After(deadline) {
				t.Fatalf("%s runs %v, want %v", what, ran(), want)
			}
			tick()
		}
	}
	awaitPasses := func(n int64) {
		t.Helper()
		base := passed.Load()
		deadline := time.Now().Add(fixturekit.DemoTimeout)
		for passed.Load()-base < n {
			if time.Now().After(deadline) {
				t.Fatalf("only %d of %d further reconciliation passes completed; runs %v", passed.Load()-base, n, ran())
			}
			tick()
		}
	}

	target := runtime.Target{Scope: "workstream", Project: fixturekit.Project, Workstream: fixturekit.Stream}
	intakefixture.Mutation(t, c, "PUT", "pause", PauseRequest{Target: target, Mode: "soft", Source: "owner"})
	queueTurn(t, repo, "held", clock.Now())
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_chief", Project: fixturekit.Project, Workstream: fixturekit.Stream, At: clock.Now(), Actor: owner, Cause: "message_chief", Depth: 1},
		AgentID: trace.Manager, ThreadID: trace.Manager, TurnID: "chief", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Owner message: chief"}
	_, err = repo.EnqueueTurn(ctx, req)
	fixturekit.Must(t, err)
	await("paused", []string{"chief"})
	awaitPasses(2)
	if got := ran(); !slices.Equal(got, []string{"chief"}) {
		t.Fatalf("paused runs %v", got)
	}
	if _, ok := turnOperations(t, repo)["held"]; ok {
		t.Fatal("paused worker turn was dispatched")
	}

	intakefixture.Mutation(t, c, "DELETE", "pause", ClearPauseRequest(target))
	await("resumed", []string{"chief", "held"})
	fixturekit.Must(t, s.Close())
}

func TestServiceParksWaitingThreadAcrossRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "sw-")
	fixturekit.Must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := fixtureAt(t, home)
	cfg, err := projectstore.LoadConfig(opts.Config)
	fixturekit.Must(t, err)
	root := cfg.Root.String()

	clock := &fixturekit.DemoClock{At: fixturekit.DemoStart}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, clock.Now(), owner)
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo.CreateWorkstream(ctx, fixturekit.Stream, clock.Now(), owner))
	identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: fixturekit.DemoAgent, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: clock.Now(), Actor: owner, Cause: "workstream_created"}, Role: fixturekit.DemoRole, ThreadID: fixturekit.DemoThread}
	fixturekit.Must(t, repo.CreateThread(ctx, identity))
	queueTurn(t, repo, "ask", clock.Now())
	fixturekit.Must(t, repo.Close())

	waiting := adaptertest.Reply[coreadapter.SessionResult]{Value: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session"}, FinalResponse: "Question", Outcome: &coreadapter.Outcome{Status: "waiting", Report: "Owner answer needed"}}}
	answered := adaptertest.Reply[coreadapter.SessionResult]{Value: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session"}, FinalResponse: "Thanks"}}
	turns := &adaptertest.Turns{Script: *adaptertest.NewScript[coreadapter.PreparedTurn](waiting, answered)}
	lives := make(chan *trace.Repository, 1)
	opts.Threads = func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
		lives <- r
		return thread.Dispatcher{Runner: thread.Runner{Jev: fixturekit.UnconfiguredJudge(nil), Store: r, Turns: turns, Now: clock.Now},
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				return coreadapter.PreparedTurn{SessionDirectory: filepath.Join(root, "sessions", in.Agent, in.Turn)}, nil
			}}, nil
	}
	ticks := make(chan time.Time)
	opts.Reconciliation.Now, opts.Reconciliation.Ticks = clock.Now, ticks
	tick := func() {
		t.Helper()
		select {
		case ticks <- clock.Now():
		case <-time.After(fixturekit.DemoTimeout):
			t.Fatal("reconciliation pass did not finish")
		}
	}
	ran := func() []string {
		var out []string
		for _, call := range turns.Calls() {
			out = append(out, call.Scope.Turn)
		}
		return out
	}
	parked := func(r *trace.Repository) bool {
		t.Helper()
		th, err := r.Thread(fixturekit.Stream, fixturekit.DemoAgent)
		fixturekit.Must(t, err)
		return th.Parked()
	}

	s, err := Start(ctx, opts)
	fixturekit.Must(t, err)
	repo = <-lives
	tick()
	tick()
	intakefixture.Soon(t, "the first lifetime to park the thread", func() bool { return parked(repo) })
	if got := ran(); !slices.Equal(got, []string{"ask"}) {
		t.Fatalf("first lifetime ran %v, parked %v", got, parked(repo))
	}
	fixturekit.Must(t, s.Close())

	s, err = Start(ctx, opts)
	fixturekit.Must(t, err)
	repo = <-lives
	tick()
	tick()
	if got := ran(); !slices.Equal(got, []string{"ask"}) || !parked(repo) {
		t.Fatalf("after restart ran %v, parked %v", got, parked(repo))
	}

	queueTurn(t, repo, "answer", clock.Now())
	if parked(repo) {
		t.Fatal("queued turn left the thread parked")
	}
	tick()
	tick()
	intakefixture.Soon(t, "the answer turn to run and unpark the thread", func() bool {
		return slices.Equal(ran(), []string{"ask", "answer"}) && !parked(repo)
	})
	if got := ran(); !slices.Equal(got, []string{"ask", "answer"}) || parked(repo) {
		t.Fatalf("unparked runs %v, parked %v", got, parked(repo))
	}
	fixturekit.Must(t, s.Close())
}

func TestServiceBoundsTurnsByProjectCapacity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "sc-")
	fixturekit.Must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := fixtureAt(t, home)
	top := filepath.Join(opts.Config.Root, "config.toml")
	body, err := os.ReadFile(top)
	fixturekit.Must(t, err)
	fixturekit.Must(t, os.WriteFile(top, []byte(strings.Replace(string(body), "[profiles.default]", "[capacity]\nmasons = 4\nper_workstream = 3\n[profiles.default]", 1)), 0600))
	fixturekit.EditProjectSettings(t, opts.Config.Root, fixturekit.Project, func(text string) string { return text + "[capacity]\nper_workstream = 1\n" })
	cfg, err := projectstore.LoadConfig(opts.Config)
	fixturekit.Must(t, err)
	if cfg.Capacity.PerWorkstream != 3 || cfg.Project.Capacity.PerWorkstream != 1 {
		t.Fatalf("capacity %+v, project %+v", cfg.Capacity, cfg.Project.Capacity)
	}
	root := cfg.Root.String()

	clock := &fixturekit.DemoClock{At: fixturekit.DemoStart}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, clock.Now(), owner)
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo.CreateWorkstream(ctx, fixturekit.Stream, clock.Now(), owner))
	for _, id := range []string{"mason1", "mason2"} {
		identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: id, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: clock.Now(), Actor: owner, Cause: "workstream_created"}, Role: "mason", ThreadID: id}
		fixturekit.Must(t, repo.CreateThread(ctx, identity))
		req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_" + id, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: clock.Now(), Actor: owner, Cause: "message_" + id, Depth: 1},
			AgentID: id, ThreadID: id, TurnID: id, Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Owner message"}
		_, err = repo.EnqueueTurn(ctx, req)
		fixturekit.Must(t, err)
	}
	fixturekit.Must(t, repo.Close())

	reply := adaptertest.Reply[coreadapter.SessionResult]{Value: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session"}, FinalResponse: "Answer"}}
	started, release := make(chan string, 2), make(chan struct{})
	block := func(ctx context.Context) {
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	turns := &fixturekit.BlockingTurns{Fake: &adaptertest.Turns{Script: *adaptertest.NewScript[coreadapter.PreparedTurn](reply, reply)}, Hooks: map[string]func(context.Context){}}
	for _, id := range []string{"mason1", "mason2"} {
		turns.Hooks[id] = func(ctx context.Context) { started <- id; block(ctx) }
	}
	lives := make(chan *trace.Repository, 1)
	opts.Threads = func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
		lives <- r
		return thread.Dispatcher{Runner: thread.Runner{Jev: fixturekit.UnconfiguredJudge(nil), Store: r, Turns: turns, Now: clock.Now},
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				return coreadapter.PreparedTurn{SessionDirectory: filepath.Join(root, "sessions", in.Agent, in.Turn)}, nil
			}}, nil
	}
	var passed atomic.Int64
	opts.schedulePassed = func() { passed.Add(1) }
	s, _ := start(t, opts)
	repo = <-lives
	select {
	case id := <-started:
		if id != "mason1" {
			t.Fatalf("first turn %s", id)
		}
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("no turn started")
	}
	intakefixture.AwaitPasses(t, &passed, 3)
	select {
	case id := <-started:
		t.Fatalf("%s started beside mason1", id)
	default:
	}
	if _, ok := turnOperations(t, repo)["mason2"]; ok {
		t.Fatal("mason2 was dispatched beside mason1")
	}
	close(release)
	select {
	case id := <-started:
		if id != "mason2" {
			t.Fatalf("second turn %s", id)
		}
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("mason2 did not run after mason1 completed")
	}
	fixturekit.Must(t, s.Close())
}

func TestServiceOffersFreedSlotsInRuntimePriorityOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "sc-")
	fixturekit.Must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := fixtureAt(t, home)
	top := filepath.Join(opts.Config.Root, "config.toml")
	body, err := os.ReadFile(top)
	fixturekit.Must(t, err)
	fixturekit.Must(t, os.WriteFile(top, []byte(strings.Replace(string(body), "[profiles.default]", "[capacity]\nmasons = 1\n[profiles.default]", 1)), 0600))
	cfg, err := projectstore.LoadConfig(opts.Config)
	fixturekit.Must(t, err)
	root := cfg.Root.String()

	const second config.WorkstreamID = "w_fedcba9876543210fedcba9876543210"
	clock := &fixturekit.DemoClock{At: fixturekit.DemoStart}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, clock.Now(), owner)
	fixturekit.Must(t, err)
	for _, ws := range []config.WorkstreamID{fixturekit.Stream, second} {
		fixturekit.Must(t, repo.CreateWorkstream(ctx, ws, clock.Now(), owner))
		id := "mason_" + string(ws)
		identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: id, Project: fixturekit.Project, Workstream: ws, At: clock.Now(), Actor: owner, Cause: "workstream_created"}, Role: "mason", ThreadID: id}
		fixturekit.Must(t, repo.CreateThread(ctx, identity))
		req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_" + id, Project: fixturekit.Project, Workstream: ws, At: clock.Now(), Actor: owner, Cause: "message_" + id, Depth: 1},
			AgentID: id, ThreadID: id, TurnID: string(ws), Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Owner message"}
		_, err = repo.EnqueueTurn(ctx, req)
		fixturekit.Must(t, err)
	}
	fixturekit.Must(t, repo.Close())
	priority, err := json.Marshal(runtime.State{Version: runtime.Version, Priorities: []runtime.Priority{{Project: fixturekit.Project, Workstreams: []config.WorkstreamID{second}}}})
	fixturekit.Must(t, err)
	fixturekit.Must(t, os.WriteFile(filepath.Join(root, "runtime.json"), priority, 0600))

	reply := adaptertest.Reply[coreadapter.SessionResult]{Value: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session"}, FinalResponse: "Answer"}}
	started := make(chan string, 2)
	turns := &fixturekit.BlockingTurns{Fake: &adaptertest.Turns{Script: *adaptertest.NewScript[coreadapter.PreparedTurn](reply, reply)}, Hooks: map[string]func(context.Context){}}
	for _, ws := range []config.WorkstreamID{fixturekit.Stream, second} {
		turns.Hooks[string(ws)] = func(context.Context) { started <- string(ws) }
	}
	opts.Threads = func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
		return thread.Dispatcher{Runner: thread.Runner{Jev: fixturekit.UnconfiguredJudge(nil), Store: r, Turns: turns, Now: clock.Now},
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				return coreadapter.PreparedTurn{SessionDirectory: filepath.Join(root, "sessions", in.Agent, in.Turn)}, nil
			}}, nil
	}
	s, _ := start(t, opts)
	var order []string
	for range 2 {
		select {
		case ws := <-started:
			order = append(order, ws)
		case <-time.After(fixturekit.DemoTimeout):
			t.Fatalf("turns started %v", order)
		}
	}
	if want := []string{string(second), string(fixturekit.Stream)}; !slices.Equal(order, want) {
		t.Fatalf("turns started %v, want %v", order, want)
	}
	fixturekit.Must(t, s.Close())
}
