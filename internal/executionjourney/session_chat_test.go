package executionjourney

import (
	context "context"
	agent "github.com/kpenfound/agentcore/agent"
	enforcertest "github.com/kpenfound/agentcore/agent/agenttest/enforcertest"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/service"
	fixturekit "github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	thread "github.com/kpenfound/osmia/internal/thread"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
	os "os"
	filepath "path/filepath"
	regexp "regexp"
	strings "strings"
	sync "sync"
	testing "testing"
	time "time"
)

// newSessionChatFixture registers a project with a real local upstream
// remote and opens one session on it, with the production enforcement
// wiring (internal/service/enforcement.go) bound to a fake engine, so a
// session turn exercises the real sandbox, grant, capture and
// resume-confinement machinery.
func newSessionChatFixture(t *testing.T) (*Service, *Client, config.ProjectID, config.WorkstreamID, *sessionEngine, string) {
	t.Helper()
	opts, clone := projectFixture(t)
	home := filepath.Dir(clone)
	must(t, os.WriteFile(filepath.Join(clone, "README.md"), []byte("hi\n"), 0600))
	demoGit(t, home, "-C", clone, "add", "README.md")
	demoGit(t, home, "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	demoGit(t, home, "-C", clone, "branch", "-M", "main")
	bare := filepath.Join(home, "remotes", "dagger", "dagger.git")
	must(t, os.MkdirAll(filepath.Dir(bare), 0700))
	demoGit(t, home, "clone", "--quiet", "--bare", "--no-hardlinks", clone, bare)
	demoGit(t, home, "-C", clone, "remote", "add", "upstream", bare)
	engine := &sessionEngine{Sessions: &demoSessions{ByKey: map[string]*mcp.ClientSession{}}}
	enforced := Enforce(opts, Enforcement{Engine: engine, Hosts: sessionImageHosts(engine.Sessions)})
	opts.Threads, opts.controls = enforced.Threads, enforced.controls
	s, c := start(t, opts)
	added, err := c.AddProject(context.Background(), request(clone))
	must(t, err)
	s.cfg = s.current()
	writeCharter(t, c, added.Project.ID, "1. Keep changes small.\n")
	resp, err := c.OpenSession(context.Background(), SessionOpenRequest{Project: added.Project.ID, Profile: "default"})
	must(t, err)
	return s, c, added.Project.ID, resp.Session.Workstream, engine, clone
}

// awaitSession polls a session's view until done accepts it.
var awaitSession = fixturekit.AwaitSession

// awaitConversationOn polls a named workstream's conversation until done
// accepts it, the way awaitConversation does for the package's fixed stream.
func awaitConversationOn(t *testing.T, c *Client, id config.WorkstreamID, done func(ConversationResponse) bool) ConversationResponse {
	t.Helper()
	return intakefixture.AwaitView(t, func() (ConversationResponse, error) {
		return c.Conversation(context.Background(), id)
	}, done, "conversation did not settle: %+v")
}

// TestSessionMessageStartsOneTurnAndRecordsTheReply covers spec#6: an owner
// message to an idle session is recorded on its thread and starts exactly
// one agent turn on the session's profile, whose reply is recorded on the
// same thread.
func TestSessionMessageStartsOneTurnAndRecordsTheReply(t *testing.T) {
	t.Parallel()
	s, c, _, stream, engine, _ := newSessionChatFixture(t)
	engine.Turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		// The backend prompt carries the (here, empty) owned-log replay
		// context ahead of the owner's message; the durable record below
		// keeps the message by itself.
		if !strings.HasSuffix(req.Prompt, "Hello there") {
			t.Errorf("prompt: %q", req.Prompt)
		}
		return &agent.Result{ClaudeID: "model-session-1", ResultText: "Hi! How can I help?"}, nil
	}
	entry, err := c.SessionMessage(context.Background(), stream, "Hello there")
	must(t, err)
	if entry.Kind != "message" || entry.Text != "Hello there" {
		t.Fatalf("acknowledgement: %+v", entry)
	}
	awaitSession(t, c, stream, func(v SessionView) bool { return v.State == trace.SessionIdle })
	if n := len(engine.Calls()); n != 1 {
		t.Fatalf("turns run: %d", n)
	}
	th, err := s.sole().repository.Thread(stream, sessionAgentID)
	must(t, err)
	if len(th.Turns) != 1 {
		t.Fatalf("thread turns: %+v", th.Turns)
	}
	q := th.Turns[0]
	if q.Request.Prompt != "Hello there" || q.Response == nil || q.CompletedAt.IsZero() ||
		q.Response.Result.FinalResponse != "Hi! How can I help?" {
		t.Fatalf("recorded turn: %+v", q)
	}
}

// TestSessionTurnWorkspaceIsTheSessionWorktreeWithRestrictedSandbox covers
// spec#7: the agent's workspace reflects the session's worktree, a file it
// writes persists into the next turn, and the sandbox has no VCS metadata,
// no VCS or delivery credentials, and only the show_image MCP tool.
func TestSessionTurnWorkspaceIsTheSessionWorktreeWithRestrictedSandbox(t *testing.T) {
	t.Parallel()
	s, c, project, stream, engine, _ := newSessionChatFixture(t)
	worktree := filepath.Join(s.cfg.Root.String(), sessionsDirectory, string(project), string(stream))
	engine.Turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		if _, err := os.Lstat(filepath.Join(req.Workspace.Directory(), ".git")); err == nil {
			t.Error("the turn's view exposes VCS metadata")
		}
		if req.Grants == nil || req.Grants.VCS {
			t.Errorf("session turn granted VCS: %+v", req.Grants)
		}
		for _, name := range req.Grants.Tools {
			if strings.HasPrefix(name, "mcp__") && !strings.HasPrefix(name, "mcp__osmia_") {
				t.Errorf("session turn has an unexpected MCP tool: %s", name)
			}
		}
		if _, ok := req.Env[coreadapter.TokenEnvironment]; !ok {
			t.Error("session attachment tool has no scoped MCP token")
		}
		engine.Sessions.Mu.Lock()
		tools := engine.Sessions.ByKey[req.Env[coreadapter.TokenEnvironment]]
		engine.Sessions.Mu.Unlock()
		if tools == nil {
			t.Error("missing session tools")
		} else {
			list, err := tools.ListTools(context.Background(), nil)
			if err != nil || len(list.Tools) != 1 || list.Tools[0].Name != "show_image" {
				t.Errorf("session tool registry: %+v %v", list, err)
			}
		}
		if req.VCSEnv != nil || len(req.VCSContainerEnv) != 0 {
			t.Error("a session turn carries VCS credentials")
		}
		if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), "notes.txt"), []byte("left by turn one\n"), 0600); err != nil {
			t.Error(err)
		}
		return &agent.Result{ClaudeID: "model-session-1", ResultText: "wrote a note"}, nil
	}
	_, err := c.SessionMessage(context.Background(), stream, "write a note")
	must(t, err)
	awaitSession(t, c, stream, func(v SessionView) bool { return v.State == trace.SessionIdle })
	data, err := os.ReadFile(filepath.Join(worktree, "notes.txt"))
	must(t, err)
	if string(data) != "left by turn one\n" {
		t.Fatalf("worktree file: %q", data)
	}
	sawFile := false
	engine.Turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		notes, readErr := os.ReadFile(filepath.Join(req.Workspace.Directory(), "notes.txt"))
		sawFile = readErr == nil && string(notes) == "left by turn one\n"
		return &agent.Result{ClaudeID: "model-session-2", ResultText: "still there"}, nil
	}
	_, err = c.SessionMessage(context.Background(), stream, "check the note")
	must(t, err)
	awaitSession(t, c, stream, func(v SessionView) bool { return v.State == trace.SessionIdle })
	if !sawFile {
		t.Fatal("the second turn's workspace did not have the first turn's file")
	}
}

// TestSessionSecondTurnResumesWithOnlyNewEntriesSincePreviousTurn covers
// spec#8: a resumed second turn receives only the new owner message plus
// service entries recorded since the previous turn, with none of the
// thread's earlier history, and a service entry reaches the next turn
// exactly once.
func TestSessionSecondTurnResumesWithOnlyNewEntriesSincePreviousTurn(t *testing.T) {
	t.Parallel()
	s, c, project, stream, engine, _ := newSessionChatFixture(t)
	engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error { return nil }
	engine.Turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		if req.ResumeID != "" {
			t.Errorf("first turn asked to resume %q", req.ResumeID)
		}
		return &agent.Result{ClaudeID: "model-session-1", ResultText: "First reply"}, nil
	}
	_, err := c.SessionMessage(context.Background(), stream, "First message")
	must(t, err)
	awaitSession(t, c, stream, func(v SessionView) bool { return v.State == trace.SessionIdle })

	// Two service entries, like sess-controls' commit and check entries,
	// recorded directly on the thread between turns.
	repository := s.sole().repository
	must(t, recordSessionEntry(context.Background(), repository, project, stream, "commit-1", s.now(), "Committed as abc123."))
	must(t, recordSessionEntry(context.Background(), repository, project, stream, "check-1", s.now(), "Checks passed."))

	var secondPrompt, secondResume string
	engine.Turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		secondPrompt, secondResume = req.Prompt, req.ResumeID
		return &agent.Result{ClaudeID: "model-session-1", ResultText: "Second reply"}, nil
	}
	_, err = c.SessionMessage(context.Background(), stream, "Second message")
	must(t, err)
	awaitSession(t, c, stream, func(v SessionView) bool { return v.State == trace.SessionIdle })

	if secondResume != "model-session-1" {
		t.Fatalf("second turn did not resume the first turn's model session: %q", secondResume)
	}
	if !strings.Contains(secondPrompt, "Committed as abc123.") || !strings.Contains(secondPrompt, "Checks passed.") || !strings.Contains(secondPrompt, "Second message") {
		t.Fatalf("second turn prompt missing entries or message: %q", secondPrompt)
	}
	if strings.Contains(secondPrompt, "First message") || strings.Contains(secondPrompt, "First reply") {
		t.Fatalf("second turn prompt replayed earlier thread history: %q", secondPrompt)
	}

	// A third turn must not see the first and second turns' entries again.
	var thirdPrompt string
	engine.Turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		thirdPrompt = req.Prompt
		return &agent.Result{ClaudeID: "model-session-1", ResultText: "Third reply"}, nil
	}
	_, err = c.SessionMessage(context.Background(), stream, "Third message")
	must(t, err)
	awaitSession(t, c, stream, func(v SessionView) bool { return v.State == trace.SessionIdle })
	if strings.Contains(thirdPrompt, "Committed as abc123.") || strings.Contains(thirdPrompt, "Checks passed.") {
		t.Fatalf("a service entry reached a second turn: %q", thirdPrompt)
	}
	if thirdPrompt != "Third message" {
		t.Fatalf("third turn prompt: %q", thirdPrompt)
	}
}

// TestSessionModelSessionDirectoryIsPerSessionUnderRootAndSurvivesRestart
// covers the model-session state directory part of spec#8: it is kept per
// session under the service root, outside the worktree and the target
// repository, and still resumable (the directory and its contents survive)
// after a restart.
func TestSessionModelSessionDirectoryIsPerSessionUnderRootAndSurvivesRestart(t *testing.T) {
	t.Parallel()
	s, c, project, stream, engine, clone := newSessionChatFixture(t)
	root := s.cfg.Root.String()
	worktree := filepath.Join(root, sessionsDirectory, string(project), string(stream))
	dir := runtime.ModelSessionDirectory(root, project, stream)
	if !strings.HasPrefix(dir, root) {
		t.Fatalf("model-session directory %s is not under the root %s", dir, root)
	}
	for _, outside := range []string{worktree, clone} {
		if rel, err := filepath.Rel(outside, dir); err == nil && !strings.HasPrefix(rel, "..") {
			t.Fatalf("model-session directory %s is inside %s", dir, outside)
		}
	}
	var sessionDir string
	engine.Turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		sessionDir = req.SessionDir
		return &agent.Result{ClaudeID: "model-session-1", ResultText: "First reply"}, nil
	}
	_, err := c.SessionMessage(context.Background(), stream, "first")
	must(t, err)
	awaitSession(t, c, stream, func(v SessionView) bool { return v.State == trace.SessionIdle })
	if sessionDir == "" || !strings.HasPrefix(sessionDir, dir) {
		t.Fatalf("turn's session directory %s is not under %s", sessionDir, dir)
	}
	if _, err := os.Stat(sessionDir); err != nil {
		t.Fatalf("session directory missing: %v", err)
	}
	must(t, s.Close())
	s2, c2 := start(t, Options{Options: service.Options{
		Config: config.Options{Root: root}, Build: Identity{Version: "test", Commit: "abc"}, ShutdownTimeout: 100 * time.Millisecond,
		Threads: s.options.Threads,
	}, controls: s.options.controls})
	_ = s2
	resp, err := c2.Session(context.Background(), stream)
	must(t, err)
	if resp.Session.Workstream != stream {
		t.Fatalf("session did not survive a restart: %+v", resp.Session)
	}
	if _, err := os.Stat(sessionDir); err != nil {
		t.Fatalf("model-session state did not survive the restart: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("model-session directory did not survive the restart: %v", err)
	}
}

// TestSessionMessageWhileBusyIsRefused covers spec#9: a message sent while
// a session turn is running is refused with a busy error and nothing is
// recorded.
func TestSessionMessageWhileBusyIsRefused(t *testing.T) {
	t.Parallel()
	_, c, _, stream, engine, _ := newSessionChatFixture(t)
	hold, entered := make(chan struct{}), make(chan struct{})
	engine.Turn = func(ctx context.Context, req agent.Request) (*agent.Result, error) {
		close(entered)
		select {
		case <-hold:
		case <-ctx.Done():
		}
		return &agent.Result{ClaudeID: "model-session-1", ResultText: "Done"}, nil
	}
	_, err := c.SessionMessage(context.Background(), stream, "first")
	must(t, err)
	select {
	case <-entered:
	case <-time.After(demoTimeout):
		t.Fatal("first turn did not start")
	}
	before, err := c.Session(context.Background(), stream)
	must(t, err)
	if before.Session.State != trace.SessionWorking {
		t.Fatalf("session is not reported working: %+v", before.Session)
	}
	_, err = c.SessionMessage(context.Background(), stream, "second")
	assertCode(t, err, Conflict)
	after, err := c.Session(context.Background(), stream)
	must(t, err)
	if after.Session != before.Session {
		t.Fatalf("a refused message changed the session: %+v, want %+v", after.Session, before.Session)
	}
	close(hold)
}

// TestSessionStopKeepsWorktreeChangesAndReturnsIdle covers spec#10: stopping
// a running turn records it as stopped, keeps the worktree changes it
// already made, returns the session to idle, and the next message resumes
// the same model session.
func TestSessionStopKeepsWorktreeChangesAndReturnsIdle(t *testing.T) {
	t.Parallel()
	s, c, project, stream, engine, _ := newSessionChatFixture(t)
	engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error { return nil }
	worktree := filepath.Join(s.cfg.Root.String(), sessionsDirectory, string(project), string(stream))
	entered := make(chan struct{})
	engine.Turn = func(ctx context.Context, req agent.Request) (*agent.Result, error) {
		if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), "progress.txt"), []byte("partial work\n"), 0600); err != nil {
			t.Error(err)
		}
		close(entered)
		<-ctx.Done()
		// A stopped turn's result carries no error: the stop is conveyed
		// through the Stoppable context, not through the agent's return
		// value, matching the production enforcer's own convention (see
		// interrupted_mason_test.go and reliability_demo_test.go) under
		// which a cancelled run's session reference still survives.
		return &agent.Result{ClaudeID: "model-session-1", ResultText: "Half done", IsError: true, Signal: 15}, nil
	}
	_, err := c.SessionMessage(context.Background(), stream, "first")
	must(t, err)
	select {
	case <-entered:
	case <-time.After(demoTimeout):
		t.Fatal("turn did not start")
	}
	_, err = c.SessionStop(context.Background(), stream)
	must(t, err)
	// Stopping only signals the cancellation; the turn's own goroutine
	// records its stopped response and returns the session to idle.
	awaitSession(t, c, stream, func(v SessionView) bool { return v.State == trace.SessionIdle })
	data, err := os.ReadFile(filepath.Join(worktree, "progress.txt"))
	must(t, err)
	if string(data) != "partial work\n" {
		t.Fatalf("worktree changes were not kept: %q", data)
	}
	th, err := s.sole().repository.Thread(stream, sessionAgentID)
	must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].Response == nil || th.Turns[0].Response.Stop == nil {
		t.Fatalf("turn is not recorded as stopped: %+v", th.Turns)
	}

	var resumeID string
	engine.Turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		resumeID = req.ResumeID
		return &agent.Result{ClaudeID: "model-session-1", ResultText: "Resumed"}, nil
	}
	_, err = c.SessionMessage(context.Background(), stream, "continue")
	must(t, err)
	awaitSession(t, c, stream, func(v SessionView) bool { return v.State == trace.SessionIdle })
	if resumeID != "model-session-1" {
		t.Fatalf("the next message did not resume the stopped turn's model session: %q", resumeID)
	}
}

// TestSessionTurnInterruptedByRestartIsRecoveredLikeAChiefOfStaffTurn covers
// spec#11: a session turn a restart interrupts is recovered the same way as
// an interrupted chief-of-staff turn (TestServiceStartRunsChiefInterruptionTurn):
// the turn is left claimed and unanswered by a service session that never
// returns, not by one that runs its fake agent to completion, since the
// production shutdown path joins every in-flight turn before it returns. The
// recovered continuation resumes the session's recorded model session, and
// the owner's message is answered exactly once.
func TestSessionTurnInterruptedByRestartIsRecoveredLikeAChiefOfStaffTurn(t *testing.T) {
	t.Parallel()
	s, c, project, stream, engine, _ := newSessionChatFixture(t)
	root := s.cfg.Root.String()
	engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error { return nil }
	engine.Turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		return &agent.Result{ClaudeID: "model-session-1", ResultText: "First reply"}, nil
	}
	_, err := c.SessionMessage(context.Background(), stream, "first")
	must(t, err)
	awaitSession(t, c, stream, func(v SessionView) bool { return v.State == trace.SessionIdle })
	must(t, s.Close())

	// A restart, rather than a stop: a second owner message's turn is left
	// claimed by a prior service session that never captured a result,
	// exactly as TestServiceStartRunsChiefInterruptionTurn leaves a
	// chief-of-staff turn claimed.
	repository, err := trace.Open(s.cfg.Root, s.cfg.Project)
	must(t, err)
	cfg := s.current().For(project)
	profile, _, err := cfg.Execution(masonRole, "default")
	must(t, err)
	at := s.now()
	treq := trace.TurnRequest{
		Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_crashed", Revision: 1,
			Project: project, Workstream: stream, At: at, Actor: ownerActor, Cause: "owner-message"},
		AgentID: sessionAgentID, ThreadID: sessionAgentID, TurnID: "message_crashed",
		Profile: profile, SystemPrompt: sessionRolePrompt, Prompt: "second",
	}
	_, err = repository.EnqueueTurn(context.Background(), treq)
	must(t, err)
	must(t, setSessionState(context.Background(), repository, project, stream, at, "session-working-message_crashed", "session-turn-started", trace.SessionWorking))
	dir := filepath.Join(runtime.ModelSessionDirectory(root, project, stream), "message_crashed")
	must(t, os.MkdirAll(dir, 0700))
	_, err = repository.ClaimTurn(context.Background(), stream, sessionAgentID, "crashed_claim", dir, at)
	must(t, err)
	must(t, repository.Close())

	engine.Turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		if req.ResumeID != "model-session-1" {
			t.Errorf("recovered turn did not resume the interrupted turn's model session: %q", req.ResumeID)
		}
		if !strings.Contains(req.Prompt, "stopped") {
			t.Errorf("recovered turn prompt does not say the service stopped: %q", req.Prompt)
		}
		return &agent.Result{ClaudeID: "model-session-2", ResultText: "Recovered reply"}, nil
	}
	enforced := Enforce(Options{Options: service.Options{Config: config.Options{Root: root}}}, Enforcement{Engine: engine, Hosts: sessionImageHosts(engine.Sessions)})
	s2, c2 := start(t, Options{Options: service.Options{
		Config: config.Options{Root: root}, Build: Identity{Version: "test", Commit: "abc"}, ShutdownTimeout: 100 * time.Millisecond,
		Threads: enforced.Threads,
	}, controls: enforced.controls})
	awaitSession(t, c2, stream, func(v SessionView) bool { return v.State == trace.SessionIdle })
	th, err := s2.sole().repository.Thread(stream, sessionAgentID)
	must(t, err)
	if len(th.Turns) != 3 || th.Turns[1].Status() != "interrupted" {
		t.Fatalf("crashed turn was not recovered as interrupted: %+v", th.Turns)
	}
	var answered int
	for _, q := range th.Turns {
		if q.Response != nil && !q.CompletedAt.IsZero() && q.Response.Result.FinalResponse == "Recovered reply" {
			answered++
		}
	}
	if answered != 1 {
		t.Fatalf("owner message was not answered exactly once: %+v", th.Turns)
	}
}

// TestSessionFallsBackToHistoryWhenModelSessionCannotBeResumed covers
// spec#27: when the recorded model session cannot be resumed, the turn
// starts a new model session on the same profile seeded with the thread's
// history, records the new model session for later turns, and adds a
// thread entry saying the context was rebuilt.
func TestSessionFallsBackToHistoryWhenModelSessionCannotBeResumed(t *testing.T) {
	t.Parallel()
	s, c, _, stream, engine, _ := newSessionChatFixture(t)
	engine.Turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		if req.ResumeID != "" {
			t.Errorf("first turn asked to resume %q", req.ResumeID)
		}
		return &agent.Result{ClaudeID: "model-session-1", ResultText: "First reply"}, nil
	}
	_, err := c.SessionMessage(context.Background(), stream, "first")
	must(t, err)
	awaitSession(t, c, stream, func(v SessionView) bool { return v.State == trace.SessionIdle })

	// The saved model session cannot be resumed.
	engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error {
		return coreadapter.ErrResumeUnavailable
	}
	var secondResume, secondHistory string
	engine.Turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		secondResume, secondHistory = req.ResumeID, req.Prompt
		return &agent.Result{ClaudeID: "model-session-2", ResultText: "Second reply"}, nil
	}
	_, err = c.SessionMessage(context.Background(), stream, "second")
	must(t, err)
	awaitSession(t, c, stream, func(v SessionView) bool { return v.State == trace.SessionIdle })
	if secondResume != "" {
		t.Fatalf("the unresumable turn still resumed: %q", secondResume)
	}
	if !strings.Contains(secondHistory, "first") || !strings.Contains(secondHistory, "First reply") {
		t.Fatalf("fallback turn was not seeded with the thread's history: %q", secondHistory)
	}

	th, err := s.sole().repository.Thread(stream, sessionAgentID)
	must(t, err)
	if len(th.Turns) != 2 || th.Turns[1].Response == nil || th.Turns[1].Response.Result.Session.ID != "model-session-2" {
		t.Fatalf("the new model session was not recorded: %+v", th.Turns)
	}

	entries, err := sessionEntriesSince(s.sole().repository, stream, time.Time{})
	must(t, err)
	var rebuilt bool
	for _, e := range entries {
		if strings.Contains(e.Text, "rebuilt") {
			rebuilt = true
		}
	}
	if !rebuilt {
		t.Fatalf("no thread entry said the context was rebuilt: %+v", entries)
	}

	// The next turn resumes the newly recorded model session.
	var thirdResume string
	engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error { return nil }
	engine.Turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		thirdResume = req.ResumeID
		return &agent.Result{ClaudeID: "model-session-2", ResultText: "Third reply"}, nil
	}
	_, err = c.SessionMessage(context.Background(), stream, "third")
	must(t, err)
	awaitSession(t, c, stream, func(v SessionView) bool { return v.State == trace.SessionIdle })
	if thirdResume != "model-session-2" {
		t.Fatalf("later turn did not resume the rebuilt model session: %q", thirdResume)
	}
}

// TestSessionRelayRecordsContextRebuiltEntryBeforeTheIdleTransition drives
// sessionRelay directly, with no sleep and no concurrency, and reads the
// trace repository's own commit order: the context-rebuilt entry is
// recorded in a commit that precedes the commit that moves the session to
// idle, so a reader that waits for idle can never find the session idle
// without the entry already present. It also shows that retrying the relay
// for the same turn records no second entry and no second idle transition.
func TestSessionRelayRecordsContextRebuiltEntryBeforeTheIdleTransition(t *testing.T) {
	t.Parallel()
	s, c, project, _ := newSessionFixture(t)
	resp, err := c.OpenSession(context.Background(), SessionOpenRequest{Project: project, Profile: "default"})
	must(t, err)
	stream := resp.Session.Workstream

	// Sessions take no reconciliation pass, but the project's controller
	// still runs its own passes on other subjects in the background; halt
	// it so nothing else commits to the session's trace while this test
	// counts sessionRelay's own commits.
	must(t, s.halt(s.sole()))

	repo := s.sole().repository
	traceDir, err := s.cfg.Root.WorkstreamTrace(project, stream)
	must(t, err)
	ctx := context.Background()

	// A turn is in progress, so the relay's transition to idle is a real
	// effect rather than a no-op.
	must(t, setSessionState(ctx, repo, project, stream, time.Now(), "test-working", "test-setup", trace.SessionWorking))

	turn := trace.QueuedTurn{
		Request:  trace.TurnRequest{Header: trace.Header{Workstream: stream}, TurnID: "turn-1"},
		Attempts: []trace.TurnAttempt{{Path: "replay", SourceSession: coreadapter.BackendSession{ID: "model-session-x", Backend: "claude"}}},
	}

	before := traceCommits(t, traceDir)
	must(t, sessionRelay(ctx, repo, time.Now, trace.Thread{}, turn))
	after := traceCommits(t, traceDir)

	commits := committedPaths(t, traceDir, before)
	if len(commits) != 2 {
		t.Fatalf("sessionRelay recorded %d commits, want exactly 2 (the entry and the idle transition): %q", len(commits), commits)
	}
	if !strings.Contains(commits[0], "session-entries/") {
		t.Fatalf("the first commit sessionRelay made was not the context-rebuilt entry: %q", commits[0])
	}
	if !strings.Contains(commits[1], "events.jsonl") {
		t.Fatalf("the second commit sessionRelay made was not the idle transition: %q", commits[1])
	}

	entries, err := sessionEntriesSince(repo, stream, time.Time{})
	must(t, err)
	if len(entries) != 1 || !strings.Contains(entries[0].Text, "rebuilt") {
		t.Fatalf("the context-rebuilt entry was not recorded: %+v", entries)
	}
	state, err := repo.Workflow(stream, trace.SessionSubject)
	must(t, err)
	if state.Value != trace.SessionIdle {
		t.Fatalf("the session did not read idle after the relay: %+v", state)
	}

	// Retrying the relay for the same turn records nothing more.
	must(t, sessionRelay(ctx, repo, time.Now, trace.Thread{}, turn))
	if retried := traceCommits(t, traceDir); retried != after {
		t.Fatalf("retrying the relay recorded more trace commits: %d -> %d", after, retried)
	}
	entries, err = sessionEntriesSince(repo, stream, time.Time{})
	must(t, err)
	if len(entries) != 1 {
		t.Fatalf("retrying the relay recorded a second context-rebuilt entry: %+v", entries)
	}
}

// TestSessionMessageAfterFallbackReceivesTheContextRebuiltEntryOnce shows
// that an owner message posted once the session has read idle following a
// fallback turn receives the context-rebuilt entry in its turn input
// exactly once, and that the turn after that does not receive it again.
func TestSessionMessageAfterFallbackReceivesTheContextRebuiltEntryOnce(t *testing.T) {
	t.Parallel()
	_, c, _, stream, engine, _ := newSessionChatFixture(t)
	engine.Turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		return &agent.Result{ClaudeID: "model-session-1", ResultText: "First reply"}, nil
	}
	_, err := c.SessionMessage(context.Background(), stream, "first")
	must(t, err)
	awaitSession(t, c, stream, func(v SessionView) bool { return v.State == trace.SessionIdle })

	// The saved model session cannot be resumed, so this turn falls back
	// and the relay records a context-rebuilt entry before returning to idle.
	engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error {
		return coreadapter.ErrResumeUnavailable
	}
	engine.Turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		return &agent.Result{ClaudeID: "model-session-2", ResultText: "Second reply"}, nil
	}
	_, err = c.SessionMessage(context.Background(), stream, "second")
	must(t, err)
	awaitSession(t, c, stream, func(v SessionView) bool { return v.State == trace.SessionIdle })

	// The session can resume from here on, so no later turn falls back and
	// records an entry of its own.
	engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error { return nil }

	var thirdPrompt string
	engine.Turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		thirdPrompt = req.Prompt
		return &agent.Result{ClaudeID: "model-session-2", ResultText: "Third reply"}, nil
	}
	_, err = c.SessionMessage(context.Background(), stream, "third")
	must(t, err)
	awaitSession(t, c, stream, func(v SessionView) bool { return v.State == trace.SessionIdle })
	if n := strings.Count(thirdPrompt, "rebuilt"); n != 1 {
		t.Fatalf("the context-rebuilt entry reached the next turn's input %d times, want exactly 1: %q", n, thirdPrompt)
	}
	if !strings.Contains(thirdPrompt, "third") {
		t.Fatalf("the turn's own message was lost alongside the entry: %q", thirdPrompt)
	}

	var fourthPrompt string
	engine.Turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		fourthPrompt = req.Prompt
		return &agent.Result{ClaudeID: "model-session-2", ResultText: "Fourth reply"}, nil
	}
	_, err = c.SessionMessage(context.Background(), stream, "fourth")
	must(t, err)
	awaitSession(t, c, stream, func(v SessionView) bool { return v.State == trace.SessionIdle })
	if strings.Contains(fourthPrompt, "rebuilt") {
		t.Fatalf("the turn after the one that received the context-rebuilt entry received it again: %q", fourthPrompt)
	}
}

// TestSessionMessageRefusedWhenProfileNoLongerRunnable covers spec#26: when
// the session's profile is no longer one the service can run, a message is
// refused with an error naming the profile, nothing is recorded, and no
// other profile is used.
func TestSessionMessageRefusedWhenProfileNoLongerRunnable(t *testing.T) {
	t.Parallel()
	s, c, _, stream, engine, _ := newSessionChatFixture(t)
	cfg := *s.current()
	cfg.Profiles = map[string]config.Profile{}
	service.InspectForTest(s.core()).SetConfig(&cfg)
	s.cfg = &cfg
	_, err := c.SessionMessage(context.Background(), stream, "hello")
	var api *APIError
	if err == nil {
		t.Fatal("message to a session with no usable profile was not refused")
	}
	if ok := func() bool {
		if a, is := asAPIError(err); is {
			api = a
			return true
		}
		return false
	}(); !ok {
		t.Fatalf("not an API error: %v", err)
	}
	if api.Code != Validation || !strings.Contains(api.Message, "default") {
		t.Fatalf("refusal does not name the profile: %+v", api)
	}
	th, err := s.sole().repository.Thread(stream, sessionAgentID)
	if err == nil && len(th.Turns) != 0 {
		t.Fatalf("a refused message was recorded: %+v", th)
	}
	if calls := len(engine.Calls()); calls != 0 {
		t.Fatalf("a turn ran on another profile: %d calls", calls)
	}
}

// confinementEngine is a fake execution engine with no CheckResume of its
// own, unlike sessionEngine, which answers CheckResume directly and so never
// exercises resumableEngine's role-blind heuristic fallback at all (session_
// turns.go): every CheckResume call reaching this engine falls to that
// fallback, which thread.Runner.ResumableRoles confines to the session
// role. A test using it, rather than sessionEngine, shows the fallback
// itself stays confined, not just the generic CheckResume-answers-directly
// mechanism every role already shares on base.
type confinementEngine struct {
	mu   sync.Mutex
	runs []agent.Request
	turn func(ctx context.Context, req agent.Request) (*agent.Result, error)
}

func (e *confinementEngine) Enforcer(s coreadapter.ExecutionSettings) (agent.Enforcer, error) {
	return &enforcertest.Enforcer{Sandbox: s.Mode, Image: s.Image, Agent: func(ctx context.Context, turn *enforcertest.Turn) (*agent.Result, error) {
		e.mu.Lock()
		e.runs = append(e.runs, turn.Request)
		run := e.turn
		e.mu.Unlock()
		if run == nil {
			return &agent.Result{ClaudeID: "turn", ResultText: "ok"}, nil
		}
		return run(ctx, turn.Request)
	}}, nil
}

// newConfinementFixture registers a project the way newSessionChatFixture
// does, with an MCP host so a chief-of-staff turn's granted tools can be
// served: unlike a session turn, which is granted none, every other role
// needs one to run at all.
func newConfinementFixture(t *testing.T) (*Service, *Client, config.ProjectID, *confinementEngine) {
	t.Helper()
	opts, clone := projectFixture(t)
	home := filepath.Dir(clone)
	must(t, os.WriteFile(filepath.Join(clone, "README.md"), []byte("hi\n"), 0600))
	demoGit(t, home, "-C", clone, "add", "README.md")
	demoGit(t, home, "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	demoGit(t, home, "-C", clone, "branch", "-M", "main")
	bare := filepath.Join(home, "remotes", "dagger", "dagger.git")
	must(t, os.MkdirAll(filepath.Dir(bare), 0700))
	demoGit(t, home, "clone", "--quiet", "--bare", "--no-hardlinks", clone, bare)
	demoGit(t, home, "-C", clone, "remote", "add", "upstream", bare)
	sessions := &demoSessions{ByKey: map[string]*mcp.ClientSession{}}
	engine := &confinementEngine{}
	enforced := Enforce(opts, Enforcement{Engine: engine, Hosts: &coreadapter.MCPHost{Transport: &demoTransport{Sessions: sessions}}})
	opts.Threads, opts.controls = enforced.Threads, enforced.controls
	s, c := start(t, opts)
	added, err := c.AddProject(context.Background(), request(clone))
	must(t, err)
	writeCharter(t, c, added.Project.ID, "1. Keep changes small.\n")
	return s, c, added.Project.ID, engine
}

// chiefTurn runs a two-message chief-of-staff conversation on a fresh
// workstream of project through c, and reports the second turn's ResumeID
// and Prompt.
func confinementChiefTurn(t *testing.T, s *Service, c *Client, engine *confinementEngine) (resume, prompt string) {
	t.Helper()
	repository := s.sole().repository
	streams, err := repository.Workstreams()
	must(t, err)
	other, err := config.NewWorkstreamID(streams...)
	must(t, err)
	must(t, repository.CreateWorkstream(context.Background(), other, s.now(), ownerActor))
	engine.mu.Lock()
	engine.turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		if req.ResumeID != "" {
			t.Errorf("chief-of-staff first turn asked to resume %q", req.ResumeID)
		}
		return &agent.Result{ClaudeID: "chief-session-1", ResultText: "First chief reply"}, nil
	}
	engine.mu.Unlock()
	_, err = c.Send(context.Background(), other, "first chief message")
	must(t, err)
	awaitConversationOn(t, c, other, func(l ConversationResponse) bool { return len(l.Entries) == 2 })
	engine.mu.Lock()
	engine.turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		resume, prompt = req.ResumeID, req.Prompt
		return &agent.Result{ClaudeID: "chief-session-1", ResultText: "Second chief reply"}, nil
	}
	engine.mu.Unlock()
	_, err = c.Send(context.Background(), other, "second chief message")
	must(t, err)
	awaitConversationOn(t, c, other, func(l ConversationResponse) bool { return len(l.Entries) == 4 })
	return resume, prompt
}

// TestResumeConfinedToSessionTurns covers that resume is confined to
// session turns (spec#8, spec#27 and the constraint that chief-of-staff,
// mason and reviewer turns must not start resuming): in a service where a
// session has taken a turn resumed through resumableEngine's role-blind
// heuristic fallback, a chief-of-staff conversation run through the exact
// same shared engine (which has no CheckResume of its own, so it could not
// resume through any other mechanism either) never resumes, and a second,
// completely session-free service's identical conversation receives the
// exact same second-turn prompt.
func TestResumeConfinedToSessionTurns(t *testing.T) {
	t.Parallel()
	s, c, project, engine := newConfinementFixture(t)
	resp, err := c.OpenSession(context.Background(), SessionOpenRequest{Project: project, Profile: "default"})
	must(t, err)
	stream := resp.Session.Workstream
	engine.mu.Lock()
	engine.turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		return &agent.Result{ClaudeID: "model-session-1", ResultText: "First session reply"}, nil
	}
	engine.mu.Unlock()
	_, err = c.SessionMessage(context.Background(), stream, "first")
	must(t, err)
	awaitSession(t, c, stream, func(v SessionView) bool { return v.State == trace.SessionIdle })
	var sessionResumed string
	engine.mu.Lock()
	engine.turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		sessionResumed = req.ResumeID
		return &agent.Result{ClaudeID: "model-session-1", ResultText: "Second session reply"}, nil
	}
	engine.mu.Unlock()
	_, err = c.SessionMessage(context.Background(), stream, "second")
	must(t, err)
	awaitSession(t, c, stream, func(v SessionView) bool { return v.State == trace.SessionIdle })
	if sessionResumed != "model-session-1" {
		t.Fatalf("the session's own second turn did not resume through the heuristic fallback: %q", sessionResumed)
	}

	// Now an ordinary chief-of-staff conversation on another workstream of
	// the same project, through the exact same engine the session just
	// resumed on.
	chiefResume, chiefPrompt := confinementChiefTurn(t, s, c, engine)
	if chiefResume != "" {
		t.Fatalf("a chief-of-staff turn resumed although only the session role may: %q", chiefResume)
	}
	if !strings.Contains(chiefPrompt, "second chief message") {
		t.Fatalf("chief-of-staff turn input: %q", chiefPrompt)
	}

	// A second, completely session-free service's identical chief-of-staff
	// conversation receives the exact same second-turn prompt: the session
	// elsewhere made no difference to it.
	s2, c2, _, engine2 := newConfinementFixture(t)
	_, basePrompt := confinementChiefTurn(t, s2, c2, engine2)
	if normalizeReplayPrompt(basePrompt) != normalizeReplayPrompt(chiefPrompt) {
		t.Fatalf("chief-of-staff turn input with a session present %q, without one %q", chiefPrompt, basePrompt)
	}
}

// normalizeReplayPrompt blanks the request and response IDs, project,
// workstream and timestamp fields a turn's replayed owned-log prefix
// carries, every literal project or workstream ID a mason or reviewer
// prompt's plain-text project context section names outside any JSON
// field, and every git commit it names, so two turns on different
// projects, workstreams and git histories, run at different times, can
// still be compared for the shape and text that matter: what was asked
// and what was answered.
func normalizeReplayPrompt(prompt string) string {
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`"id":"[^"]*"`),
		regexp.MustCompile(`"turn_id":"[^"]*"`),
		regexp.MustCompile(`"project":"[^"]*"`),
		regexp.MustCompile(`"workstream":"[^"]*"`),
		regexp.MustCompile(`"at":"[^"]*"`),
		regexp.MustCompile(`\bp_[0-9a-f]{32}\b`),
		regexp.MustCompile(`\bw_[0-9a-f]{32}\b`),
		regexp.MustCompile(`\b[0-9a-f]{40}\b`),
	} {
		prompt = re.ReplaceAllString(prompt, "")
	}
	return prompt
}

// TestResumeConfinedAcrossMasonAndReviewerTurns extends
// TestResumeConfinedToSessionTurns's chief-of-staff coverage (spec#8,
// spec#27, the constraint that chief-of-staff, mason and reviewer turns
// must not start resuming, and acceptance item 11) to a real
// mason turn and a real reviewer turn, run through the exact production
// pipeline every other mason and reviewer test uses (newLandingFixture's
// shared demoEngine, which every role's turn reaches, session included):
// in a service where a session has taken a second turn that resumed its
// first on that same engine, the mason's and the reviewer's own turns of a
// unit on the same project ask for no resume, and receive the exact same
// prompt an otherwise identical, completely session-free fixture's mason
// and reviewer turns get.
func TestResumeConfinedAcrossMasonAndReviewerTurns(t *testing.T) {
	t.Parallel()
	masonResume, masonPrompt, reviewResume, reviewPrompt := runLandingWithSession(t, true)
	baseMasonResume, basePrompt, baseReviewResume, baseReviewPrompt := runLandingWithSession(t, false)
	if masonResume != "" || baseMasonResume != "" {
		t.Fatalf("a mason turn resumed although only the session role may: with a session %q, without %q", masonResume, baseMasonResume)
	}
	if reviewResume != "" || baseReviewResume != "" {
		t.Fatalf("a reviewer turn resumed although only the session role may: with a session %q, without %q", reviewResume, baseReviewResume)
	}
	if normalizeReplayPrompt(masonPrompt) != normalizeReplayPrompt(basePrompt) {
		t.Fatalf("mason turn input with a session present %q, without one %q", masonPrompt, basePrompt)
	}
	if normalizeReplayPrompt(reviewPrompt) != normalizeReplayPrompt(baseReviewPrompt) {
		t.Fatalf("reviewer turn input with a session present %q, without one %q", reviewPrompt, baseReviewPrompt)
	}
}

// runLandingWithSession lands independentPlan's unit resume through
// newLandingFixture's real mason and reviewer turns and, when withSession
// is set, first opens a session on the same project and has it take a
// second turn that resumes its first on the fixture's own engine, so the
// mason's and the reviewer's own turns run with a session's resumed turn
// already recorded in the service. It returns each turn's ResumeID and
// Prompt.
func runLandingWithSession(t *testing.T, withSession bool) (masonResume, masonPrompt, reviewResume, reviewPrompt string) {
	t.Helper()
	f, masons := newLandingFixture(t, independentPlan)
	defer f.stop(t)
	f.engine.Mu.Lock()
	f.engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error { return nil }
	f.engine.Turns[masonTurnID("resume")] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		masonResume, masonPrompt = req.ResumeID, req.Prompt
		return masons.Turn(ctx, req, verified, tools)
	}
	chiefFallback := f.engine.Turns["*"]
	var sessionResumeIDs []string
	f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if strings.HasPrefix(req.Name, "message_") {
			sessionResumeIDs = append(sessionResumeIDs, req.ResumeID)
			return &agent.Result{ClaudeID: "fixture-session", ResultText: "OK", SessionDir: req.SessionDir, NumTurns: 1}, nil
		}
		if strings.HasPrefix(req.Name, reviewerAgent("resume")+"-review-") {
			reviewResume, reviewPrompt = req.ResumeID, req.Prompt
		}
		return chiefFallback(ctx, req, verified, tools)
	}
	f.engine.Mu.Unlock()

	if withSession {
		resp, err := f.c.OpenSession(context.Background(), SessionOpenRequest{Project: f.project, Profile: "default"})
		must(t, err)
		sstream := resp.Session.Workstream
		_, err = f.c.SessionMessage(context.Background(), sstream, "first")
		must(t, err)
		awaitSession(t, f.c, sstream, func(v SessionView) bool { return v.State == trace.SessionIdle })
		_, err = f.c.SessionMessage(context.Background(), sstream, "second")
		must(t, err)
		awaitSession(t, f.c, sstream, func(v SessionView) bool { return v.State == trace.SessionIdle })
		if len(sessionResumeIDs) != 2 || sessionResumeIDs[0] != "" || sessionResumeIDs[1] == "" {
			t.Fatalf("the session's own second turn did not resume the first: %v", sessionResumeIDs)
		}
	}

	stream := f.seedBuilding(t, "land", independentPlan)
	f.awaitMerged(t, stream, "resume")
	return masonResume, masonPrompt, reviewResume, reviewPrompt
}

// TestResumeRoleGateCoversEveryRole covers spec#8, spec#27 and the
// constraint that chief-of-staff, mason and reviewer turns must not start
// resuming: it calls the production Threads factory (internal/service's
// Enforce, exercised through the same fixture every other session test
// uses) and inspects the thread.Runner.ResumableRoles gate it configures,
// confirming it answers true for the session role alone and false for
// chief-of-staff, mason and reviewer.
func TestResumeRoleGateCoversEveryRole(t *testing.T) {
	t.Parallel()
	s, _, project, _, _, _ := newSessionChatFixture(t)
	repository := s.sole().repository
	cfg := s.current().For(project)
	reconciler, err := s.options.Threads(repository, cfg)
	must(t, err)
	d, ok := reconciler.(thread.Dispatcher)
	if !ok {
		t.Fatalf("threads factory did not return a thread.Dispatcher: %T", reconciler)
	}
	if d.Runner.ResumableRoles == nil {
		t.Fatal("no resumable-roles gate is configured")
	}
	for role, want := range map[string]bool{
		sessionRole: true, trace.ChiefOfStaff: false, masonRole: false, reviewerRole: false,
	} {
		if got := d.Runner.ResumableRoles(role); got != want {
			t.Errorf("ResumableRoles(%q) = %v, want %v", role, got, want)
		}
	}
}

// asAPIError extracts an *APIError from an http client error without
// importing errors into every call site above.
func asAPIError(err error) (*APIError, bool) {
	type unwrapper interface{ Unwrap() error }
	for e := err; e != nil; {
		if api, ok := e.(*APIError); ok {
			return api, true
		}
		u, ok := e.(unwrapper)
		if !ok {
			return nil, false
		}
		e = u.Unwrap()
	}
	return nil, false
}

func sessionImageHosts(input ...*demoSessions) coreadapter.MCPHosts {
	sessions := &demoSessions{ByKey: map[string]*mcp.ClientSession{}}
	if len(input) > 0 && input[0] != nil {
		sessions = input[0]
	}
	return &coreadapter.MCPHost{Transport: &demoTransport{Sessions: sessions}}
}
