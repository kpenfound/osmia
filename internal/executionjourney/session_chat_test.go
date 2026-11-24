package executionjourney

import (
	context "context"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/intake"
	os "os"
	filepath "path/filepath"
	regexp "regexp"
	slices "slices"
	strings "strings"
	sync "sync"
	testing "testing"
	time "time"

	agent "github.com/kpenfound/agentcore/agent"
	enforcertest "github.com/kpenfound/agentcore/agent/agenttest/enforcertest"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	isolation "github.com/kpenfound/osmia/internal/isolation"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/service"
	fixturekit "github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	thread "github.com/kpenfound/osmia/internal/thread"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func newSessionChatFixture(t *testing.T) (*Service, *Client, config.ProjectID, config.WorkstreamID, *fixturekit.SessionEngine, string) {
	t.Helper()
	opts, clone := projectFixture(t)
	home := filepath.Dir(clone)
	fixturekit.Must(t, os.WriteFile(filepath.Join(clone, "README.md"), []byte("hi\n"), 0600))
	fixturekit.DemoGit(t, home, "-C", clone, "add", "README.md")
	fixturekit.DemoGit(t, home, "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	fixturekit.DemoGit(t, home, "-C", clone, "branch", "-M", "main")
	bare := filepath.Join(home, "remotes", "dagger", "dagger.git")
	fixturekit.Must(t, os.MkdirAll(filepath.Dir(bare), 0700))
	fixturekit.DemoGit(t, home, "clone", "--quiet", "--bare", "--no-hardlinks", clone, bare)
	fixturekit.DemoGit(t, home, "-C", clone, "remote", "add", "upstream", bare)
	engine := &fixturekit.SessionEngine{Sessions: &fixturekit.DemoSessions{ByKey: map[string]*mcp.ClientSession{}}}
	enforced := Enforce(opts, service.Enforcement{Engine: engine, Hosts: sessionImageHosts(engine.Sessions)})
	opts.Threads, opts.controls = enforced.Threads, enforced.controls
	s, c := start(t, opts)
	added, err := c.AddProject(context.Background(), fixturekit.Request(clone))
	fixturekit.Must(t, err)
	s.cfg = s.current()
	writeCharter(t, c, added.Project.ID, "1. Keep changes small.\n")
	resp, err := c.OpenSession(context.Background(), api.SessionOpenRequest{Project: added.Project.ID, Profile: "default"})
	fixturekit.Must(t, err)
	return s, c, added.Project.ID, resp.Session.Workstream, engine, clone
}

func awaitConversationOn(t *testing.T, c *Client, id config.WorkstreamID, done func(api.ConversationResponse) bool) api.ConversationResponse {
	t.Helper()
	return intakefixture.AwaitView(t, func() (api.ConversationResponse, error) {
		return c.Conversation(context.Background(), id)
	}, done, "conversation did not settle: %+v")
}

func TestSessionMessageStartsOneTurnAndRecordsTheReply(t *testing.T) {
	t.Parallel()
	s, c, _, stream, engine, _ := newSessionChatFixture(t)
	engine.Turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {

		if !strings.HasSuffix(req.Prompt, "Hello there") {
			t.Errorf("prompt: %q", req.Prompt)
		}
		return &agent.Result{ClaudeID: "model-session-1", ResultText: "Hi! How can I help?"}, nil
	}
	entry, err := c.SessionMessage(context.Background(), stream, "Hello there")
	fixturekit.Must(t, err)
	if entry.Kind != "message" || entry.Text != "Hello there" {
		t.Fatalf("acknowledgement: %+v", entry)
	}
	fixturekit.AwaitSession(t, c, stream, func(v api.SessionView) bool { return v.State == trace.SessionIdle })
	if n := len(engine.Calls()); n != 1 {
		t.Fatalf("turns run: %d", n)
	}
	th, err := s.sole().repository.Thread(stream, service.TestSessionAgentID)
	fixturekit.Must(t, err)
	if len(th.Turns) != 1 {
		t.Fatalf("thread turns: %+v", th.Turns)
	}
	q := th.Turns[0]
	if q.Request.Prompt != "Hello there" || q.Response == nil || q.CompletedAt.IsZero() ||
		q.Response.Result.FinalResponse != "Hi! How can I help?" {
		t.Fatalf("recorded turn: %+v", q)
	}
}

func TestSessionTurnWorkspaceIsTheSessionWorktreeWithRestrictedSandbox(t *testing.T) {
	t.Parallel()
	s, c, project, stream, engine, _ := newSessionChatFixture(t)
	worktree := filepath.Join(s.cfg.Root.String(), service.TestSessionsDirectory, string(project), string(stream))
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

		if want := []string{"Read", "Glob", "Grep", "Write", "Edit", "Bash", "WebSearch", "WebFetch", "mcp__osmia_0"}; !slices.Equal(req.Grants.Tools, want) {
			t.Errorf("session native tools %v, want %v", req.Grants.Tools, want)
		}
		if !strings.Contains(req.SystemPrompt, isolation.WebGuidance) || !strings.HasPrefix(req.SystemPrompt, service.TestSessionRolePrompt) {
			t.Errorf("session system prompt lacks the web guidance after its role prompt: %q", req.SystemPrompt)
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
	fixturekit.Must(t, err)
	fixturekit.AwaitSession(t, c, stream, func(v api.SessionView) bool { return v.State == trace.SessionIdle })
	data, err := os.ReadFile(filepath.Join(worktree, "notes.txt"))
	fixturekit.Must(t, err)
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
	fixturekit.Must(t, err)
	fixturekit.AwaitSession(t, c, stream, func(v api.SessionView) bool { return v.State == trace.SessionIdle })
	if !sawFile {
		t.Fatal("the second turn's workspace did not have the first turn's file")
	}
}

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
	fixturekit.Must(t, err)
	fixturekit.AwaitSession(t, c, stream, func(v api.SessionView) bool { return v.State == trace.SessionIdle })

	repository := s.sole().repository
	fixturekit.Must(t, service.TestRecordSessionEntry(context.Background(), repository, project, stream, "commit-1", s.now(), "Committed as abc123."))
	fixturekit.Must(t, service.TestRecordSessionEntry(context.Background(), repository, project, stream, "check-1", s.now(), "Checks passed."))

	var secondPrompt, secondResume string
	engine.Turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		secondPrompt, secondResume = req.Prompt, req.ResumeID
		return &agent.Result{ClaudeID: "model-session-1", ResultText: "Second reply"}, nil
	}
	_, err = c.SessionMessage(context.Background(), stream, "Second message")
	fixturekit.Must(t, err)
	fixturekit.AwaitSession(t, c, stream, func(v api.SessionView) bool { return v.State == trace.SessionIdle })

	if secondResume != "model-session-1" {
		t.Fatalf("second turn did not resume the first turn's model session: %q", secondResume)
	}
	if !strings.Contains(secondPrompt, "Committed as abc123.") || !strings.Contains(secondPrompt, "Checks passed.") || !strings.Contains(secondPrompt, "Second message") {
		t.Fatalf("second turn prompt missing entries or message: %q", secondPrompt)
	}
	if strings.Contains(secondPrompt, "First message") || strings.Contains(secondPrompt, "First reply") {
		t.Fatalf("second turn prompt replayed earlier thread history: %q", secondPrompt)
	}

	var thirdPrompt string
	engine.Turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		thirdPrompt = req.Prompt
		return &agent.Result{ClaudeID: "model-session-1", ResultText: "Third reply"}, nil
	}
	_, err = c.SessionMessage(context.Background(), stream, "Third message")
	fixturekit.Must(t, err)
	fixturekit.AwaitSession(t, c, stream, func(v api.SessionView) bool { return v.State == trace.SessionIdle })
	if strings.Contains(thirdPrompt, "Committed as abc123.") || strings.Contains(thirdPrompt, "Checks passed.") {
		t.Fatalf("a service entry reached a second turn: %q", thirdPrompt)
	}
	if thirdPrompt != "Third message" {
		t.Fatalf("third turn prompt: %q", thirdPrompt)
	}
}

func TestSessionModelSessionDirectoryIsPerSessionUnderRootAndSurvivesRestart(t *testing.T) {
	t.Parallel()
	s, c, project, stream, engine, clone := newSessionChatFixture(t)
	root := s.cfg.Root.String()
	worktree := filepath.Join(root, service.TestSessionsDirectory, string(project), string(stream))
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
	fixturekit.Must(t, err)
	fixturekit.AwaitSession(t, c, stream, func(v api.SessionView) bool { return v.State == trace.SessionIdle })
	if sessionDir == "" || !strings.HasPrefix(sessionDir, dir) {
		t.Fatalf("turn's session directory %s is not under %s", sessionDir, dir)
	}
	if _, err := os.Stat(sessionDir); err != nil {
		t.Fatalf("session directory missing: %v", err)
	}
	fixturekit.Must(t, s.Close())
	s2, c2 := start(t, Options{Options: service.Options{
		Config: config.Options{Root: root}, Build: api.Identity{Version: "test", Commit: "abc"}, ShutdownTimeout: 100 * time.Millisecond,
		Threads: s.options.Threads,
	}, controls: s.options.controls})
	_ = s2
	resp, err := c2.Session(context.Background(), stream)
	fixturekit.Must(t, err)
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
	fixturekit.Must(t, err)
	select {
	case <-entered:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("first turn did not start")
	}
	before, err := c.Session(context.Background(), stream)
	fixturekit.Must(t, err)
	if before.Session.State != trace.SessionWorking {
		t.Fatalf("session is not reported working: %+v", before.Session)
	}
	_, err = c.SessionMessage(context.Background(), stream, "second")
	fixturekit.AssertCode(t, err, api.Conflict)
	after, err := c.Session(context.Background(), stream)
	fixturekit.Must(t, err)
	if after.Session != before.Session {
		t.Fatalf("a refused message changed the session: %+v, want %+v", after.Session, before.Session)
	}
	close(hold)
}

func TestSessionStopKeepsWorktreeChangesAndReturnsIdle(t *testing.T) {
	t.Parallel()
	s, c, project, stream, engine, _ := newSessionChatFixture(t)
	engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error { return nil }
	worktree := filepath.Join(s.cfg.Root.String(), service.TestSessionsDirectory, string(project), string(stream))
	entered := make(chan struct{})
	engine.Turn = func(ctx context.Context, req agent.Request) (*agent.Result, error) {
		if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), "progress.txt"), []byte("partial work\n"), 0600); err != nil {
			t.Error(err)
		}
		close(entered)
		<-ctx.Done()

		return &agent.Result{ClaudeID: "model-session-1", ResultText: "Half done", IsError: true, Signal: 15}, nil
	}
	_, err := c.SessionMessage(context.Background(), stream, "first")
	fixturekit.Must(t, err)
	select {
	case <-entered:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("turn did not start")
	}
	_, err = c.SessionStop(context.Background(), stream)
	fixturekit.Must(t, err)

	fixturekit.AwaitSession(t, c, stream, func(v api.SessionView) bool { return v.State == trace.SessionIdle })
	data, err := os.ReadFile(filepath.Join(worktree, "progress.txt"))
	fixturekit.Must(t, err)
	if string(data) != "partial work\n" {
		t.Fatalf("worktree changes were not kept: %q", data)
	}
	th, err := s.sole().repository.Thread(stream, service.TestSessionAgentID)
	fixturekit.Must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].Response == nil || th.Turns[0].Response.Stop == nil {
		t.Fatalf("turn is not recorded as stopped: %+v", th.Turns)
	}

	var resumeID string
	engine.Turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		resumeID = req.ResumeID
		return &agent.Result{ClaudeID: "model-session-1", ResultText: "Resumed"}, nil
	}
	_, err = c.SessionMessage(context.Background(), stream, "continue")
	fixturekit.Must(t, err)
	fixturekit.AwaitSession(t, c, stream, func(v api.SessionView) bool { return v.State == trace.SessionIdle })
	if resumeID != "model-session-1" {
		t.Fatalf("the next message did not resume the stopped turn's model session: %q", resumeID)
	}
}

func TestSessionTurnInterruptedByRestartIsRecoveredLikeAChiefOfStaffTurn(t *testing.T) {
	t.Parallel()
	s, c, project, stream, engine, _ := newSessionChatFixture(t)
	root := s.cfg.Root.String()
	engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error { return nil }
	engine.Turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		return &agent.Result{ClaudeID: "model-session-1", ResultText: "First reply"}, nil
	}
	_, err := c.SessionMessage(context.Background(), stream, "first")
	fixturekit.Must(t, err)
	fixturekit.AwaitSession(t, c, stream, func(v api.SessionView) bool { return v.State == trace.SessionIdle })
	fixturekit.Must(t, s.Close())

	repository, err := trace.Open(s.cfg.Root, s.cfg.Project)
	fixturekit.Must(t, err)
	cfg := s.current().For(project)
	profile, _, err := cfg.Execution(intake.MasonRole, "default")
	fixturekit.Must(t, err)
	at := s.now()
	treq := trace.TurnRequest{
		Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_crashed", Revision: 1,
			Project: project, Workstream: stream, At: at, Actor: intake.OwnerActor, Cause: "owner-message"},
		AgentID: service.TestSessionAgentID, ThreadID: service.TestSessionAgentID, TurnID: "message_crashed",
		Profile: profile, SystemPrompt: service.TestSessionRolePrompt, Prompt: "second",
	}
	_, err = repository.EnqueueTurn(context.Background(), treq)
	fixturekit.Must(t, err)
	fixturekit.Must(t, service.TestSetSessionState(context.Background(), repository, project, stream, at, "session-working-message_crashed", "session-turn-started", trace.SessionWorking))
	dir := filepath.Join(runtime.ModelSessionDirectory(root, project, stream), "message_crashed")
	fixturekit.Must(t, os.MkdirAll(dir, 0700))
	_, err = repository.ClaimTurn(context.Background(), stream, service.TestSessionAgentID, "crashed_claim", dir, at)
	fixturekit.Must(t, err)
	fixturekit.Must(t, repository.Close())

	engine.Turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		if req.ResumeID != "model-session-1" {
			t.Errorf("recovered turn did not resume the interrupted turn's model session: %q", req.ResumeID)
		}
		if !strings.Contains(req.Prompt, "stopped") {
			t.Errorf("recovered turn prompt does not say the service stopped: %q", req.Prompt)
		}
		return &agent.Result{ClaudeID: "model-session-2", ResultText: "Recovered reply"}, nil
	}
	enforced := Enforce(Options{Options: service.Options{Config: config.Options{Root: root}}}, service.Enforcement{Engine: engine, Hosts: sessionImageHosts(engine.Sessions)})
	s2, c2 := start(t, Options{Options: service.Options{
		Config: config.Options{Root: root}, Build: api.Identity{Version: "test", Commit: "abc"}, ShutdownTimeout: 100 * time.Millisecond,
		Threads: enforced.Threads,
	}, controls: enforced.controls})
	fixturekit.AwaitSession(t, c2, stream, func(v api.SessionView) bool { return v.State == trace.SessionIdle })
	th, err := s2.sole().repository.Thread(stream, service.TestSessionAgentID)
	fixturekit.Must(t, err)
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
	fixturekit.Must(t, err)
	fixturekit.AwaitSession(t, c, stream, func(v api.SessionView) bool { return v.State == trace.SessionIdle })

	engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error {
		return coreadapter.ErrResumeUnavailable
	}
	var secondResume, secondHistory string
	engine.Turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		secondResume, secondHistory = req.ResumeID, req.Prompt
		return &agent.Result{ClaudeID: "model-session-2", ResultText: "Second reply"}, nil
	}
	_, err = c.SessionMessage(context.Background(), stream, "second")
	fixturekit.Must(t, err)
	fixturekit.AwaitSession(t, c, stream, func(v api.SessionView) bool { return v.State == trace.SessionIdle })
	if secondResume != "" {
		t.Fatalf("the unresumable turn still resumed: %q", secondResume)
	}
	if !strings.Contains(secondHistory, "first") || !strings.Contains(secondHistory, "First reply") {
		t.Fatalf("fallback turn was not seeded with the thread's history: %q", secondHistory)
	}

	th, err := s.sole().repository.Thread(stream, service.TestSessionAgentID)
	fixturekit.Must(t, err)
	if len(th.Turns) != 2 || th.Turns[1].Response == nil || th.Turns[1].Response.Result.Session.ID != "model-session-2" {
		t.Fatalf("the new model session was not recorded: %+v", th.Turns)
	}

	entries, err := service.TestSessionEntriesSince(s.sole().repository, stream, time.Time{})
	fixturekit.Must(t, err)
	var rebuilt bool
	for _, e := range entries {
		if strings.Contains(e.Text, "rebuilt") {
			rebuilt = true
		}
	}
	if !rebuilt {
		t.Fatalf("no thread entry said the context was rebuilt: %+v", entries)
	}

	var thirdResume string
	engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error { return nil }
	engine.Turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		thirdResume = req.ResumeID
		return &agent.Result{ClaudeID: "model-session-2", ResultText: "Third reply"}, nil
	}
	_, err = c.SessionMessage(context.Background(), stream, "third")
	fixturekit.Must(t, err)
	fixturekit.AwaitSession(t, c, stream, func(v api.SessionView) bool { return v.State == trace.SessionIdle })
	if thirdResume != "model-session-2" {
		t.Fatalf("later turn did not resume the rebuilt model session: %q", thirdResume)
	}
}

func TestSessionRelayRecordsContextRebuiltEntryBeforeTheIdleTransition(t *testing.T) {
	t.Parallel()
	s, c, project, _ := newSessionFixture(t)
	resp, err := c.OpenSession(context.Background(), api.SessionOpenRequest{Project: project, Profile: "default"})
	fixturekit.Must(t, err)
	stream := resp.Session.Workstream

	fixturekit.Must(t, s.halt(s.sole()))

	repo := s.sole().repository
	traceDir, err := s.cfg.Root.WorkstreamTrace(project, stream)
	fixturekit.Must(t, err)
	ctx := context.Background()

	fixturekit.Must(t, service.TestSetSessionState(ctx, repo, project, stream, time.Now(), "test-working", "test-setup", trace.SessionWorking))

	turn := trace.QueuedTurn{
		Request:  trace.TurnRequest{Header: trace.Header{Workstream: stream}, TurnID: "turn-1"},
		Attempts: []trace.TurnAttempt{{Path: "replay", SourceSession: coreadapter.BackendSession{ID: "model-session-x", Backend: "claude"}}},
	}

	before := fixturekit.TraceCommits(t, traceDir)
	fixturekit.Must(t, service.TestSessionRelay(ctx, repo, time.Now, trace.Thread{}, turn))
	after := fixturekit.TraceCommits(t, traceDir)

	commits := fixturekit.CommittedPaths(t, traceDir, before)
	if len(commits) != 2 {
		t.Fatalf("sessionRelay recorded %d commits, want exactly 2 (the entry and the idle transition): %q", len(commits), commits)
	}
	if !strings.Contains(commits[0], "session-entries/") {
		t.Fatalf("the first commit sessionRelay made was not the context-rebuilt entry: %q", commits[0])
	}
	if !strings.Contains(commits[1], "events.jsonl") {
		t.Fatalf("the second commit sessionRelay made was not the idle transition: %q", commits[1])
	}

	entries, err := service.TestSessionEntriesSince(repo, stream, time.Time{})
	fixturekit.Must(t, err)
	if len(entries) != 1 || !strings.Contains(entries[0].Text, "rebuilt") {
		t.Fatalf("the context-rebuilt entry was not recorded: %+v", entries)
	}
	state, err := repo.Workflow(stream, trace.SessionSubject)
	fixturekit.Must(t, err)
	if state.Value != trace.SessionIdle {
		t.Fatalf("the session did not read idle after the relay: %+v", state)
	}

	fixturekit.Must(t, service.TestSessionRelay(ctx, repo, time.Now, trace.Thread{}, turn))
	if retried := fixturekit.TraceCommits(t, traceDir); retried != after {
		t.Fatalf("retrying the relay recorded more trace commits: %d -> %d", after, retried)
	}
	entries, err = service.TestSessionEntriesSince(repo, stream, time.Time{})
	fixturekit.Must(t, err)
	if len(entries) != 1 {
		t.Fatalf("retrying the relay recorded a second context-rebuilt entry: %+v", entries)
	}
}

func TestSessionMessageAfterFallbackReceivesTheContextRebuiltEntryOnce(t *testing.T) {
	t.Parallel()
	_, c, _, stream, engine, _ := newSessionChatFixture(t)
	engine.Turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		return &agent.Result{ClaudeID: "model-session-1", ResultText: "First reply"}, nil
	}
	_, err := c.SessionMessage(context.Background(), stream, "first")
	fixturekit.Must(t, err)
	fixturekit.AwaitSession(t, c, stream, func(v api.SessionView) bool { return v.State == trace.SessionIdle })

	engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error {
		return coreadapter.ErrResumeUnavailable
	}
	engine.Turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		return &agent.Result{ClaudeID: "model-session-2", ResultText: "Second reply"}, nil
	}
	_, err = c.SessionMessage(context.Background(), stream, "second")
	fixturekit.Must(t, err)
	fixturekit.AwaitSession(t, c, stream, func(v api.SessionView) bool { return v.State == trace.SessionIdle })

	engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error { return nil }

	var thirdPrompt string
	engine.Turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		thirdPrompt = req.Prompt
		return &agent.Result{ClaudeID: "model-session-2", ResultText: "Third reply"}, nil
	}
	_, err = c.SessionMessage(context.Background(), stream, "third")
	fixturekit.Must(t, err)
	fixturekit.AwaitSession(t, c, stream, func(v api.SessionView) bool { return v.State == trace.SessionIdle })
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
	fixturekit.Must(t, err)
	fixturekit.AwaitSession(t, c, stream, func(v api.SessionView) bool { return v.State == trace.SessionIdle })
	if strings.Contains(fourthPrompt, "rebuilt") {
		t.Fatalf("the turn after the one that received the context-rebuilt entry received it again: %q", fourthPrompt)
	}
}

func TestSessionMessageRefusedWhenProfileNoLongerRunnable(t *testing.T) {
	t.Parallel()
	s, c, _, stream, engine, _ := newSessionChatFixture(t)
	cfg := *s.current()
	cfg.Profiles = map[string]config.Profile{}
	service.InspectForTest(s.core()).SetConfig(&cfg)
	s.cfg = &cfg
	_, err := c.SessionMessage(context.Background(), stream, "hello")
	var apiErr *api.APIError
	if err == nil {
		t.Fatal("message to a session with no usable profile was not refused")
	}
	if ok := func() bool {
		if a, is := asAPIError(err); is {
			apiErr = a
			return true
		}
		return false
	}(); !ok {
		t.Fatalf("not an API error: %v", err)
	}
	if apiErr.Code != api.Validation || !strings.Contains(apiErr.Message, "default") {
		t.Fatalf("refusal does not name the profile: %+v", apiErr)
	}
	th, err := s.sole().repository.Thread(stream, service.TestSessionAgentID)
	if err == nil && len(th.Turns) != 0 {
		t.Fatalf("a refused message was recorded: %+v", th)
	}
	if calls := len(engine.Calls()); calls != 0 {
		t.Fatalf("a turn ran on another profile: %d calls", calls)
	}
}

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

func newConfinementFixture(t *testing.T) (*Service, *Client, config.ProjectID, *confinementEngine) {
	t.Helper()
	opts, clone := projectFixture(t)
	home := filepath.Dir(clone)
	fixturekit.Must(t, os.WriteFile(filepath.Join(clone, "README.md"), []byte("hi\n"), 0600))
	fixturekit.DemoGit(t, home, "-C", clone, "add", "README.md")
	fixturekit.DemoGit(t, home, "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	fixturekit.DemoGit(t, home, "-C", clone, "branch", "-M", "main")
	bare := filepath.Join(home, "remotes", "dagger", "dagger.git")
	fixturekit.Must(t, os.MkdirAll(filepath.Dir(bare), 0700))
	fixturekit.DemoGit(t, home, "clone", "--quiet", "--bare", "--no-hardlinks", clone, bare)
	fixturekit.DemoGit(t, home, "-C", clone, "remote", "add", "upstream", bare)
	sessions := &fixturekit.DemoSessions{ByKey: map[string]*mcp.ClientSession{}}
	engine := &confinementEngine{}
	enforced := Enforce(opts, service.Enforcement{Engine: engine, Hosts: &coreadapter.MCPHost{Transport: &fixturekit.DemoTransport{Sessions: sessions}}})
	opts.Threads, opts.controls = enforced.Threads, enforced.controls
	s, c := start(t, opts)
	added, err := c.AddProject(context.Background(), fixturekit.Request(clone))
	fixturekit.Must(t, err)
	writeCharter(t, c, added.Project.ID, "1. Keep changes small.\n")
	return s, c, added.Project.ID, engine
}

func confinementChiefTurn(t *testing.T, s *Service, c *Client, engine *confinementEngine) (resume, prompt string) {
	t.Helper()
	repository := s.sole().repository
	streams, err := repository.Workstreams()
	fixturekit.Must(t, err)
	other, err := config.NewWorkstreamID(streams...)
	fixturekit.Must(t, err)
	fixturekit.Must(t, repository.CreateWorkstream(context.Background(), other, s.now(), intake.OwnerActor))
	engine.mu.Lock()
	engine.turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		if req.ResumeID != "" {
			t.Errorf("chief-of-staff first turn asked to resume %q", req.ResumeID)
		}
		return &agent.Result{ClaudeID: "chief-session-1", ResultText: "First chief reply"}, nil
	}
	engine.mu.Unlock()
	_, err = c.Send(context.Background(), other, "first chief message")
	fixturekit.Must(t, err)
	awaitConversationOn(t, c, other, func(l api.ConversationResponse) bool { return len(l.Entries) == 2 })
	engine.mu.Lock()
	engine.turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		resume, prompt = req.ResumeID, req.Prompt
		return &agent.Result{ClaudeID: "chief-session-1", ResultText: "Second chief reply"}, nil
	}
	engine.mu.Unlock()
	_, err = c.Send(context.Background(), other, "second chief message")
	fixturekit.Must(t, err)
	awaitConversationOn(t, c, other, func(l api.ConversationResponse) bool { return len(l.Entries) == 4 })
	return resume, prompt
}

func TestResumeConfinedToSessionTurns(t *testing.T) {
	t.Parallel()
	s, c, project, engine := newConfinementFixture(t)
	resp, err := c.OpenSession(context.Background(), api.SessionOpenRequest{Project: project, Profile: "default"})
	fixturekit.Must(t, err)
	stream := resp.Session.Workstream
	engine.mu.Lock()
	engine.turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		return &agent.Result{ClaudeID: "model-session-1", ResultText: "First session reply"}, nil
	}
	engine.mu.Unlock()
	_, err = c.SessionMessage(context.Background(), stream, "first")
	fixturekit.Must(t, err)
	fixturekit.AwaitSession(t, c, stream, func(v api.SessionView) bool { return v.State == trace.SessionIdle })
	var sessionResumed string
	engine.mu.Lock()
	engine.turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		sessionResumed = req.ResumeID
		return &agent.Result{ClaudeID: "model-session-1", ResultText: "Second session reply"}, nil
	}
	engine.mu.Unlock()
	_, err = c.SessionMessage(context.Background(), stream, "second")
	fixturekit.Must(t, err)
	fixturekit.AwaitSession(t, c, stream, func(v api.SessionView) bool { return v.State == trace.SessionIdle })
	if sessionResumed != "model-session-1" {
		t.Fatalf("the session's own second turn did not resume through the heuristic fallback: %q", sessionResumed)
	}

	chiefResume, chiefPrompt := confinementChiefTurn(t, s, c, engine)
	if chiefResume != "" {
		t.Fatalf("a chief-of-staff turn resumed although only the session role may: %q", chiefResume)
	}
	if !strings.Contains(chiefPrompt, "second chief message") {
		t.Fatalf("chief-of-staff turn input: %q", chiefPrompt)
	}

	s2, c2, _, engine2 := newConfinementFixture(t)
	_, basePrompt := confinementChiefTurn(t, s2, c2, engine2)
	if normalizeReplayPrompt(basePrompt) != normalizeReplayPrompt(chiefPrompt) {
		t.Fatalf("chief-of-staff turn input with a session present %q, without one %q", chiefPrompt, basePrompt)
	}
}

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

func runLandingWithSession(t *testing.T, withSession bool) (masonResume, masonPrompt, reviewResume, reviewPrompt string) {
	t.Helper()
	f, masons := newLandingFixture(t, independentPlan)
	defer f.stop(t)
	f.engine.Mu.Lock()
	f.engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error { return nil }
	f.engine.Turns[service.TestMasonTurnID("resume")] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
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
		if strings.HasPrefix(req.Name, intake.ReviewerAgent("resume")+"-review-") {
			reviewResume, reviewPrompt = req.ResumeID, req.Prompt
		}
		return chiefFallback(ctx, req, verified, tools)
	}
	f.engine.Mu.Unlock()

	if withSession {
		resp, err := f.c.OpenSession(context.Background(), api.SessionOpenRequest{Project: f.project, Profile: "default"})
		fixturekit.Must(t, err)
		sstream := resp.Session.Workstream
		_, err = f.c.SessionMessage(context.Background(), sstream, "first")
		fixturekit.Must(t, err)
		fixturekit.AwaitSession(t, f.c, sstream, func(v api.SessionView) bool { return v.State == trace.SessionIdle })
		_, err = f.c.SessionMessage(context.Background(), sstream, "second")
		fixturekit.Must(t, err)
		fixturekit.AwaitSession(t, f.c, sstream, func(v api.SessionView) bool { return v.State == trace.SessionIdle })
		if len(sessionResumeIDs) != 2 || sessionResumeIDs[0] != "" || sessionResumeIDs[1] == "" {
			t.Fatalf("the session's own second turn did not resume the first: %v", sessionResumeIDs)
		}
	}

	stream := f.seedBuilding(t, "land", independentPlan)
	f.awaitMerged(t, stream, "resume")
	return masonResume, masonPrompt, reviewResume, reviewPrompt
}

func TestResumeRoleGateCoversEveryRole(t *testing.T) {
	t.Parallel()
	s, _, project, _, _, _ := newSessionChatFixture(t)
	repository := s.sole().repository
	cfg := s.current().For(project)
	reconciler, err := s.options.Threads(repository, cfg)
	fixturekit.Must(t, err)
	d, ok := reconciler.(thread.Dispatcher)
	if !ok {
		t.Fatalf("threads factory did not return a thread.Dispatcher: %T", reconciler)
	}
	if d.Runner.ResumableRoles == nil {
		t.Fatal("no resumable-roles gate is configured")
	}
	for role, want := range map[string]bool{
		service.TestSessionAgentID: true, trace.ChiefOfStaff: false, intake.MasonRole: false, intake.ReviewerRole: false,
	} {
		if got := d.Runner.ResumableRoles(role); got != want {
			t.Errorf("ResumableRoles(%q) = %v, want %v", role, got, want)
		}
	}
}

func asAPIError(err error) (*api.APIError, bool) {
	type unwrapper interface{ Unwrap() error }
	for e := err; e != nil; {
		if apiErr, ok := e.(*api.APIError); ok {
			return apiErr, true
		}
		u, ok := e.(unwrapper)
		if !ok {
			return nil, false
		}
		e = u.Unwrap()
	}
	return nil, false
}

func sessionImageHosts(input ...*fixturekit.DemoSessions) coreadapter.MCPHosts {
	sessions := &fixturekit.DemoSessions{ByKey: map[string]*mcp.ClientSession{}}
	if len(input) > 0 && input[0] != nil {
		sessions = input[0]
	}
	return &coreadapter.MCPHost{Transport: &fixturekit.DemoTransport{Sessions: sessions}}
}
