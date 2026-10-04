package beekeeper

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// recordingTurns is a fake model session: it records every prepared turn and
// answers with its scripted result.
type recordingTurns struct {
	mu     sync.Mutex
	calls  []coreadapter.PreparedTurn
	result coreadapter.SessionResult
	err    error
}

func (f *recordingTurns) Run(_ context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, p)
	f.mu.Unlock()
	return f.result, f.err
}

func (f *recordingTurns) Calls() []coreadapter.PreparedTurn {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]coreadapter.PreparedTurn(nil), f.calls...)
}

// blockingTurns is a fake model session whose first call closes entered and
// then waits for hold to close, or for its context to end.
type blockingTurns struct {
	hold    chan struct{}
	entered chan struct{}
	once    sync.Once
	result  coreadapter.SessionResult
}

func (f *blockingTurns) Run(ctx context.Context, _ coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
	f.once.Do(func() { close(f.entered) })
	select {
	case <-f.hold:
	case <-ctx.Done():
		return coreadapter.SessionResult{}, ctx.Err()
	}
	return f.result, nil
}

func testPrepare(t *testing.T) Prepare {
	return func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
		return coreadapter.PreparedTurn{SessionDirectory: filepath.Join(t.TempDir(), string(in.Workstream), in.Agent, in.Turn)}, nil
	}
}

func testProfile() coreadapter.Profile {
	return coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}
}

func openShadow(t *testing.T) *trace.Repository {
	t.Helper()
	ctx := context.Background()
	root, err := config.ResolveRoot(filepath.Join(t.TempDir(), "osmia"), "")
	must(t, err)
	b := config.Beekeeper{Name: "Hive", Profile: "default", Sandbox: "none"}
	repo, err := Open(ctx, root, b, at)
	must(t, err)
	t.Cleanup(func() { repo.Close() })
	return repo
}

// Posting an owner message, with no owner project registered anywhere near
// this repository, records it in the Beekeeper thread and runs a Beekeeper
// turn on a fake model session whose response is recorded in the same
// thread; the Beekeeper is free again once it completes.
func TestPostRecordsOwnerMessageAndRunsBeekeeperTurn(t *testing.T) {
	ctx := context.Background()
	repo := openShadow(t)
	if busy, err := Busy(repo); err != nil || busy {
		t.Fatalf("fresh beekeeper busy: %v %v", busy, err)
	}
	turns := &recordingTurns{result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session"}, FinalResponse: "Every workstream is quiet."}}
	q, err := Post(ctx, repo, testProfile(), turns, testPrepare(t), func() time.Time { return at }, "Which workstreams need me?")
	must(t, err)
	if q.Response == nil || q.Response.Result.FinalResponse != "Every workstream is quiet." || q.Status() != "idle" {
		t.Fatalf("completed turn: %+v", q)
	}
	msgs, hasOlder, err := Messages(repo, 10)
	must(t, err)
	if hasOlder || len(msgs) != 2 {
		t.Fatalf("messages: %+v %v", msgs, hasOlder)
	}
	if msgs[0].Author.Kind != AuthorOwner || msgs[0].Text != "Which workstreams need me?" {
		t.Fatalf("owner message: %+v", msgs[0])
	}
	if msgs[1].Author.Kind != AuthorBeekeeper || msgs[1].Text != "Every workstream is quiet." {
		t.Fatalf("beekeeper reply: %+v", msgs[1])
	}
	if busy, err := Busy(repo); err != nil || busy {
		t.Fatalf("beekeeper still busy after its turn: %v %v", busy, err)
	}
	calls := turns.Calls()
	if len(calls) != 1 || calls[0].Prompt != "Which workstreams need me?" || calls[0].SystemPrompt != RolePrompt || calls[0].Profile != testProfile() {
		t.Fatalf("prepared turn: %+v", calls)
	}
}

// A turn the model session fails is recorded as a failure, the same way a
// failed chief-of-staff turn is, ends the busy state and leaves the
// Beekeeper able to take the next message.
func TestPostFailedTurnRecordsFailureEndsBusyAndAllowsNextMessage(t *testing.T) {
	ctx := context.Background()
	repo := openShadow(t)
	failing := &recordingTurns{result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session"}, FinalResponse: "partial", IsError: true}}
	now := func() time.Time { return at }
	q, err := Post(ctx, repo, testProfile(), failing, testPrepare(t), now, "Build the plan.")
	must(t, err)
	if q.Status() != "failed" || q.Response == nil || q.Response.Failure == "" {
		t.Fatalf("failed turn not recorded as a failure: %+v", q)
	}
	msgs, _, err := Messages(repo, 10)
	must(t, err)
	if len(msgs) != 2 || msgs[1].Author.Kind != AuthorFailure {
		t.Fatalf("failure message: %+v", msgs)
	}
	if busy, err := Busy(repo); err != nil || busy {
		t.Fatalf("beekeeper still busy after a failed turn: %v %v", busy, err)
	}

	succeeding := &recordingTurns{result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session-2"}, FinalResponse: "Back online."}}
	q2, err := Post(ctx, repo, testProfile(), succeeding, testPrepare(t), now, "Try again.")
	must(t, err)
	if q2.Status() != "idle" || q2.Response.Result.FinalResponse != "Back online." {
		t.Fatalf("next message after a failure: %+v", q2)
	}
}

// Posting while the Beekeeper's latest owner request has no finished turn
// returns the busy error and records nothing.
func TestPostWhileBusyReturnsErrorAndRecordsNothing(t *testing.T) {
	ctx := context.Background()
	repo := openShadow(t)
	if _, err := EnsureThread(ctx, repo, at); err != nil {
		t.Fatal(err)
	}
	pending := trace.TurnRequest{
		Header:  trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_pending", Revision: 1, Project: config.ShadowProjectID, Workstream: config.BeekeeperWorkstreamID, At: at, Actor: OwnerActor, Cause: "owner-message"},
		AgentID: AgentID, ThreadID: ThreadID, TurnID: "message_pending", Profile: testProfile(), Prompt: "Already asked",
	}
	if _, err := repo.EnqueueTurn(ctx, pending); err != nil {
		t.Fatal(err)
	}
	turns := &recordingTurns{result: coreadapter.SessionResult{FinalResponse: "should not run"}}
	_, err := Post(ctx, repo, testProfile(), turns, testPrepare(t), func() time.Time { return at }, "New message")
	if err != ErrBusy {
		t.Fatalf("expected ErrBusy, got %v", err)
	}
	if len(turns.Calls()) != 0 {
		t.Fatal("a turn ran while busy")
	}
	th, err := repo.Thread(config.BeekeeperWorkstreamID, AgentID)
	must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].Request.TurnID != "message_pending" {
		t.Fatalf("a message was recorded while busy: %+v", th.Turns)
	}
}

// Many simultaneous posts while a turn is in flight record exactly one
// owner message and run exactly one turn; every other post returns the
// busy error.
func TestPostConcurrentCallsRecordExactlyOneMessageAndRunOneTurn(t *testing.T) {
	ctx := context.Background()
	repo := openShadow(t)
	now := func() time.Time { return at }
	blocker := &blockingTurns{hold: make(chan struct{}), entered: make(chan struct{}), result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session"}, FinalResponse: "Done."}}

	first := make(chan error, 1)
	go func() {
		_, err := Post(ctx, repo, testProfile(), blocker, testPrepare(t), now, "First message")
		first <- err
	}()
	select {
	case <-blocker.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first turn never started")
	}

	const concurrent = 20
	results := make(chan error, concurrent)
	var wg sync.WaitGroup
	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			turns := &recordingTurns{result: coreadapter.SessionResult{FinalResponse: "should not run"}}
			_, err := Post(ctx, repo, testProfile(), turns, testPrepare(t), now, "Concurrent message")
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != ErrBusy {
			t.Fatalf("expected ErrBusy from a concurrent post, got %v", err)
		}
	}
	close(blocker.hold)
	if err := <-first; err != nil {
		t.Fatalf("the first post failed: %v", err)
	}

	th, err := repo.Thread(config.BeekeeperWorkstreamID, AgentID)
	must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].Request.Prompt != "First message" || th.Turns[0].Response.Result.FinalResponse != "Done." {
		t.Fatalf("exactly one turn should have run: %+v", th.Turns)
	}
}

// The Beekeeper's turns, including recording the owner's message and its
// response, work with Hearsay not configured.
func TestBeekeeperTurnWorksWithHearsayNotConfigured(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	root, err := config.ResolveRoot(filepath.Join(base, "osmia"), "")
	must(t, err)
	topPath, err := root.Config()
	must(t, err)
	must(t, os.MkdirAll(filepath.Dir(topPath), 0700))
	must(t, os.WriteFile(topPath, []byte("version = 1\nactive_projects = []\n[profiles.default]\nagent = \"claude\"\nmodel = \"test\"\n"), 0600))
	cfg, err := config.Load(config.Options{Root: root.String()})
	must(t, err)
	if cfg.Hearsay.URL != "" || len(cfg.Hearsay.Agents) != 0 {
		t.Fatalf("test assumes Hearsay is not configured: %+v", cfg.Hearsay)
	}

	repo, err := Open(ctx, root, cfg.Beekeeper, at)
	must(t, err)
	t.Cleanup(func() { repo.Close() })
	profile, err := cfg.NamedProfile(cfg.Beekeeper.Profile)
	must(t, err)
	turns := &recordingTurns{result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "claude", ID: "session"}, FinalResponse: "Still here."}}
	q, err := Post(ctx, repo, profile, turns, testPrepare(t), func() time.Time { return at }, "Are you there?")
	must(t, err)
	if q.Response == nil || q.Response.Result.FinalResponse != "Still here." {
		t.Fatalf("turn with hearsay unconfigured: %+v", q)
	}
}
