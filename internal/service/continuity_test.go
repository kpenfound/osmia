package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/busybees/core/agent/agenttest/enforcertest"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// The M1 demonstration: one mason thread receives two messages, the service
// restarts between them, and the second turn continues either by resuming the
// fake backend session or by replaying the owned log. Set OSMIA_M1_DEMO_DIR to
// keep each case's root for inspection; see docs/m1-demonstration.md.

const (
	demoAgent  = "agent_mason"
	demoThread = "thread_mason"
	demoRole   = "mason"
	// Delivery credentials exist in the service process but never reach a turn.
	demoSecret = "ghp_demo_delivery_credential_must_not_leak"
)

var demoStart = time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)

// demoTimeout only guards against a hang; trace publication runs Git and is
// slow under the race detector on a loaded machine.
const demoTimeout = 5 * time.Minute

type demoClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *demoClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(time.Second)
	return c.now
}

type demoLease func(context.Context) error

func (f demoLease) Release(ctx context.Context) error { return f(ctx) }

// demoWorkspaces is the fake repository provider: it lends the target clone,
// which keeps its service-owned .git directory, to the turn preparation path.
type demoWorkspaces struct {
	mu                 sync.Mutex
	directory          string
	acquired, released int
}

func (w *demoWorkspaces) Acquire(_ context.Context, req coreadapter.WorkspaceRequest) (coreadapter.WorkspaceLease, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.acquired++
	release := demoLease(func(context.Context) error { w.mu.Lock(); w.released++; w.mu.Unlock(); return nil })
	return coreadapter.WorkspaceLease{Workspace: coreadapter.Workspace{ID: "clone", Directory: w.directory, Access: req.Access}, Lease: release}, nil
}

// demoTransport serves the scoped MCP server in memory and lends the client
// side to the fake container engine under the turn's service token.
type demoTransport struct {
	sessions *demoSessions
}
type demoSessions struct {
	mu    sync.Mutex
	byKey map[string]*mcp.ClientSession
}

func (d *demoTransport) Start(ctx context.Context, server *mcp.Server) (coreadapter.Endpoint, coreadapter.Lease, error) {
	serverSide, clientSide := mcp.NewInMemoryTransports()
	served, err := server.Connect(ctx, serverSide, nil)
	if err != nil {
		return coreadapter.Endpoint{}, nil, err
	}
	client, err := mcp.NewClient(&mcp.Implementation{Name: "fake-agent", Version: "1"}, nil).Connect(ctx, clientSide, nil)
	if err != nil {
		served.Close()
		return coreadapter.Endpoint{}, nil, err
	}
	token := rand.Text()
	d.sessions.mu.Lock()
	d.sessions.byKey[token] = client
	d.sessions.mu.Unlock()
	release := demoLease(func(context.Context) error {
		d.sessions.mu.Lock()
		delete(d.sessions.byKey, token)
		d.sessions.mu.Unlock()
		return errors.Join(client.Close(), served.Wait())
	})
	return coreadapter.Endpoint{URL: "http://osmia-mcp.invalid/turn", BearerTokenEnvironment: "OSMIA_MCP_TOKEN", Token: token}, release, nil
}

// demoTurn is what the fake backend does inside the boundary core verified.
type demoTurn func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error)

// demoEngine is the fake execution engine. Core's fake enforcer admits each
// request the way a real session does, and Run verifies it with core's own
// container boundary, which is evidence of the grants, not of OS enforcement.
type demoEngine struct {
	mu       sync.Mutex
	sessions *demoSessions
	turns    map[string]demoTurn
	resume   func(previous, next coreadapter.Profile, session coreadapter.BackendSession) error
	runs     []string
	checks   int
}

func (e *demoEngine) CheckResume(_ context.Context, previous, next coreadapter.Profile, session coreadapter.BackendSession) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.checks++
	return e.resume(previous, next, session)
}
func (e *demoEngine) Enforcer(s coreadapter.ExecutionSettings) (agent.Enforcer, error) {
	return &enforcertest.Enforcer{Sandbox: s.Mode, Image: s.Image, Agent: func(ctx context.Context, turn *enforcertest.Turn) (*agent.Result, error) {
		return e.Run(ctx, turn.Request)
	}}, nil
}
func (e *demoEngine) Verify(req agent.Request) (*agent.Turn, error) {
	return (&agent.Runner{}).Verify(req)
}
func (e *demoEngine) Run(ctx context.Context, req agent.Request) (*agent.Result, error) {
	verified, err := e.Verify(req)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.runs = append(e.runs, req.Name)
	turn := e.turns[req.Name]
	if turn == nil {
		// Turns the service names itself, such as event turns, share a script.
		turn = e.turns["*"]
	}
	e.mu.Unlock()
	e.sessions.mu.Lock()
	tools := e.sessions.byKey[req.Env["OSMIA_MCP_TOKEN"]]
	e.sessions.mu.Unlock()
	if turn == nil || tools == nil {
		return nil, fmt.Errorf("unexpected turn %q", req.Name)
	}
	return turn(ctx, req, verified, tools)
}

// checkGrants reports a request whose grants or verified turn reach beyond a
// private writable view outside clone: another mount, VCS, a built-in tool, a
// VCS executable on PATH, or a variable beyond the request's own.
func checkGrants(req agent.Request, verified *agent.Turn, clone string, fail func(string, ...any)) {
	view := req.Workspace.Directory()
	g := req.Grants
	if g == nil || len(g.Mounts) != 2 || g.Mounts[0] != (agent.Mount{Path: view, Access: agent.ReadWrite}) || g.Mounts[1] != (agent.Mount{Path: req.SessionDir, Access: agent.ReadOnly}) || view == clone || strings.HasPrefix(view, clone+string(filepath.Separator)) {
		fail("grants %+v expose more than the private view", g)
		return
	}
	if g.VCS || verified.VCS || !slices.Equal(verified.DeniedExecutables, agent.VCSExecutables) {
		fail("VCS is not denied: %+v", verified)
	}
	if !slices.Equal(g.Tools, []string{"mcp__osmia_0"}) || verified.Tools == nil || len(verified.Tools) != 0 {
		fail("tools %v %v exceed the scoped MCP server", g.Tools, verified.Tools)
	}
	for _, kv := range verified.Env {
		key, value, _ := strings.Cut(kv, "=")
		if key != "HOME" && req.Env[key] != value {
			fail("verified environment exposes %s", key)
		}
	}
}

func callTool(ctx context.Context, tools *mcp.ClientSession, name string, args map[string]any) (string, error) {
	result, err := tools.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return "", err
	}
	var text strings.Builder
	for _, content := range result.Content {
		if c, ok := content.(*mcp.TextContent); ok {
			text.WriteString(c.Text)
		}
	}
	if result.IsError {
		return "", errors.New(text.String())
	}
	return text.String(), nil
}

// checkBoundary makes the negative assertions from inside a running turn. It
// returns an error because it runs on the service's controller goroutine.
func checkBoundary(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession, clone string) error {
	var problems []error
	fail := func(format string, args ...any) { problems = append(problems, fmt.Errorf(format, args...)) }
	listed, err := tools.ListTools(ctx, nil)
	if err != nil {
		return err
	}
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if want := []string{"file_read", "file_write", "notes_read", "notes_write"}; !slices.Equal(names, want) {
		fail("role tools %v, want %v", names, want)
	}
	if want := coreadapter.AllowedTools([]string{"osmia_0"}, names); !slices.Equal(req.Profile.AllowedTools, want) {
		fail("backend allow list %v, want %v", req.Profile.AllowedTools, want)
	}
	// Granted by name but beyond the role's execute and VCS ceiling.
	for _, name := range []string{"shell", "git_push"} {
		if _, err := callTool(ctx, tools, name, map[string]any{}); err == nil {
			fail("runtime called %s", name)
		}
	}
	for _, path := range []string{".git/config", "../escape", filepath.Join(clone, "escape"), "src/../../escape"} {
		if _, err := callTool(ctx, tools, "file_write", map[string]any{"path": path, "content": "tampered"}); err == nil {
			fail("runtime wrote %s", path)
		}
	}
	for _, path := range []string{".git/HEAD", "../config.toml", filepath.Join(clone, "src", "app.txt")} {
		if _, err := callTool(ctx, tools, "file_read", map[string]any{"path": path}); err == nil {
			fail("runtime read %s", path)
		}
	}
	checkGrants(req, verified, clone, fail)
	if _, err := os.Lstat(filepath.Join(req.Workspace.Directory(), ".git")); !errors.Is(err, fs.ErrNotExist) {
		fail("view contains VCS metadata")
	}
	if req.Profile.VCSAccess || req.Workspace == nil || req.Workspace.VCS() != nil || len(req.VCSEnv) != 0 || len(req.ContainerEnv) != 0 {
		fail("request carries VCS access")
	}
	for key, value := range req.Env {
		if key != "OSMIA_MCP_TOKEN" || strings.Contains(value, demoSecret) {
			fail("runtime environment exposes %s", key)
		}
	}
	for _, text := range []string{req.Prompt, req.SystemPrompt} {
		if strings.Contains(text, demoSecret) {
			fail("prompt exposes a delivery credential")
		}
	}
	return errors.Join(problems...)
}

// snapshot reads every file below dir, including VCS metadata.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	must(t, filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		files[path] = string(data)
		return err
	}))
	return files
}

func demoGit(t *testing.T, home string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// deliver accepts an owner message and publishes the intent to run it.
func deliver(t *testing.T, repo *trace.Repository, turn string, version uint64, at time.Time) trace.TurnRequest {
	t.Helper()
	owner := trace.Actor{Kind: "owner", ID: "local"}
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_" + turn, Project: project, Workstream: stream, At: at, Actor: owner, Cause: "message_" + turn, Depth: 1},
		AgentID: demoAgent, ThreadID: demoThread, TurnID: turn, Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"},
		SystemPrompt: "You are the mason.", Prompt: "Owner message: " + turn}
	_, err := repo.EnqueueTurn(context.Background(), req)
	must(t, err)
	event := trace.EventID("deliver_"+turn, "turn")
	op, err := thread.TurnOperation(project, event, thread.TurnInput{Workstream: stream, Agent: demoAgent, Turn: turn})
	must(t, err)
	from := ""
	if version > 0 {
		from = "delivering"
	}
	tx := trace.Transaction{ExpectedVersion: version, Transition: trace.Transition{
		Header:  trace.Header{Schema: "osmia.trace.transition", Version: 1, Revision: 1, ID: "deliver_" + turn, Project: project, Workstream: stream, At: at, Actor: trace.Actor{Kind: "service", ID: "inbox"}, Cause: req.ID, Depth: 1},
		Subject: "mason_inbox", From: from, To: "delivering", Reason: "Owner message accepted for the mason thread"},
		Events: []trace.Event{{ID: event, Kind: "turn", Body: "Deliver " + turn, Operation: &op}}}
	_, err = repo.Transact(context.Background(), tx)
	must(t, err)
	return req
}

func TestM1ThreadContinuityAcrossRestart(t *testing.T) {
	for _, mode := range []string{"resume", "replay"} {
		t.Run(mode, func(t *testing.T) { demonstrate(t, mode) })
	}
}

func demonstrate(t *testing.T, mode string) {
	for _, name := range []string{"GITHUB_TOKEN", "GH_TOKEN", "OSMIA_DELIVERY_TOKEN"} {
		t.Setenv(name, demoSecret)
	}
	ctx := context.Background()
	var home string
	if keep := os.Getenv("OSMIA_M1_DEMO_DIR"); keep != "" {
		home = filepath.Join(keep, mode)
		must(t, os.MkdirAll(keep, 0700))
		must(t, os.Mkdir(home, 0700))
	} else {
		var err error
		home, err = os.MkdirTemp("", "m1-")
		must(t, err)
		t.Cleanup(func() { os.RemoveAll(home) })
	}
	opts := fixtureAt(t, home)
	cfg, err := config.Load(opts.Config)
	must(t, err)
	root := cfg.Root.String()
	traceDir, err := cfg.Root.ProjectTrace(project)
	must(t, err)
	t.Logf("%s demonstration root: %s; trace: %s", mode, root, traceDir)

	// A local target repository whose metadata only the service may touch.
	clone := cfg.Project.Clone
	must(t, os.MkdirAll(filepath.Join(clone, "src"), 0700))
	must(t, os.WriteFile(filepath.Join(clone, "src", "app.txt"), []byte("original\n"), 0600))
	demoGit(t, home, "-C", clone, "init", "-q")
	demoGit(t, home, "-C", clone, "add", "src/app.txt")
	demoGit(t, home, "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	cloneBefore := snapshot(t, clone)

	clock := &demoClock{now: demoStart}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, clock.Now(), owner)
	must(t, err)
	must(t, repo.CreateWorkstream(ctx, stream, clock.Now(), owner))
	identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: demoAgent, Project: project, Workstream: stream, At: clock.Now(), Actor: owner, Cause: "workstream_created", Depth: 0}, Role: demoRole, ThreadID: demoThread}
	must(t, repo.CreateThread(ctx, identity))
	first := deliver(t, repo, "first", 0, clock.Now())
	must(t, repo.Close())

	sessions := &demoSessions{byKey: map[string]*mcp.ClientSession{}}
	engine := &demoEngine{sessions: sessions, turns: map[string]demoTurn{}}
	engine.resume = func(previous, next coreadapter.Profile, session coreadapter.BackendSession) error {
		if mode == "resume" && previous == next && session == (coreadapter.BackendSession{Backend: "claude", ID: "session-first"}) {
			return nil
		}
		return coreadapter.ErrResumeUnavailable
	}
	workspaces := &demoWorkspaces{directory: clone}
	views := filepath.Join(root, "views")
	must(t, os.Mkdir(views, 0700))
	var bound sync.Mutex
	var live *trace.Repository
	opts.Threads = func(r *trace.Repository) (coreadapter.Reconciler, error) {
		bound.Lock()
		live = r
		bound.Unlock()
		turns := &isolation.Turns{Workspaces: workspaces, Views: isolation.Views{Directory: views}, Engine: engine,
			Grants: map[string]coreadapter.Capabilities{demoRole: {Tools: []string{"file_read", "file_write", "notes_read", "notes_write", "shell", "git_push"}, WriteFiles: true}},
			Tools:  []coreadapter.Tool{{Name: "shell", Effect: coreadapter.ToolExecute}, {Name: "git_push", Effect: coreadapter.ToolVCS}},
			Select: func(context.Context, coreadapter.Scope) (isolation.Selection, error) {
				return isolation.Selection{Paths: []string{"src"}, Execution: coreadapter.ExecutionSettings{Mode: "container", Image: "fixture-image"}}, nil
			},
			Scoped: func(_ context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
				return r.NotesTools(demoAgent, scope)
			},
			Hosts: &coreadapter.MCPHost{Transport: &demoTransport{sessions: sessions}},
		}
		return thread.Dispatcher{Runner: thread.Runner{Store: r, Turns: turns, Now: clock.Now},
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				directory := filepath.Join(root, "sessions", in.Agent, in.Turn)
				return coreadapter.PreparedTurn{SessionDirectory: directory}, os.MkdirAll(directory, 0700)
			}}, nil
	}
	ticks := make(chan time.Time)
	opts.Reconciliation.Now, opts.Reconciliation.Ticks = clock.Now, ticks
	current := func() *trace.Repository { bound.Lock(); defer bound.Unlock(); return live }

	// First service lifetime: the first turn runs; a second message arrives
	// while it is active; the service stops as the backend returns.
	entered := make(chan struct{})
	engine.turns["first"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		err := checkBoundary(ctx, req, verified, tools, clone)
		if req.ResumeID != "" || !strings.HasSuffix(req.Prompt, first.Prompt) {
			err = errors.Join(err, fmt.Errorf("first turn context: %q %q", req.ResumeID, req.Prompt))
		}
		if notes, e := callTool(ctx, tools, "notes_read", map[string]any{}); e != nil || notes != `{"text":""}` {
			err = errors.Join(err, fmt.Errorf("fresh notes %q %v", notes, e))
		}
		_, e1 := callTool(ctx, tools, "notes_write", map[string]any{"text": "first turn: owner prefers small commits"})
		_, e2 := callTool(ctx, tools, "file_write", map[string]any{"path": "src/app.txt", "content": "edited in turn one\n"})
		if err = errors.Join(err, e1, e2); err != nil {
			t.Error(err)
		}
		close(entered)
		<-ctx.Done()
		return &agent.Result{ClaudeID: "session-first", ResultText: "First answer", SessionDir: req.SessionDir, NumTurns: 3, CostUSD: 0.25, CostKnown: true}, nil
	}
	s, err := Start(ctx, opts)
	must(t, err)
	select {
	case <-entered:
	case <-time.After(demoTimeout):
		s.Close()
		t.Fatal("first turn did not start from durable intent")
	}
	repo = current()
	second := deliver(t, repo, "second", 1, clock.Now())
	if _, err := repo.ClaimTurn(ctx, stream, demoAgent, "competing", filepath.Join(root, "competing"), clock.Now()); !errors.Is(err, trace.ErrClaimed) {
		t.Errorf("second turn claimable while first is active: %v", err)
	}
	th, err := repo.Thread(stream, demoAgent)
	must(t, err)
	if th.Active != "first" || len(th.Turns) != 2 || th.Turns[1].Claim != nil {
		t.Fatalf("mid-turn queue: %+v", th)
	}
	must(t, s.Close())

	repo, err = trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	th, err = repo.Thread(stream, demoAgent)
	must(t, err)
	if th.Active != "" || th.Status != "idle" || th.Session.ID != "session-first" || th.Turns[0].CompletedAt.IsZero() || th.Turns[1].Claim != nil || th.Turns[1].Request.Prompt != second.Prompt {
		t.Fatalf("stopped state: %+v", th)
	}
	firstSession := th.Turns[0].Claim.ServiceSession
	ops, err := repo.Operations(stream)
	must(t, err)
	for _, op := range ops {
		var in thread.TurnInput
		must(t, json.Unmarshal(op.Operation.Input, &in))
		switch {
		case in.Turn == "first" && (op.Claim == nil || op.Result != nil || op.Acknowledged):
			t.Fatalf("first operation was not left mid-reconciliation: %+v", op)
		case in.Turn == "second" && len(op.History) != 0:
			t.Fatalf("second operation was touched before the restart: %+v", op)
		}
	}
	if notes, err := os.ReadFile(filepath.Join(traceDir, "notes", demoRole+".md")); err != nil || string(notes) != "first turn: owner prefers small commits" {
		t.Fatalf("notes before restart: %q %v", notes, err)
	}
	must(t, repo.Close())

	// Second service lifetime: the controller finds both intents by scanning.
	finished := make(chan struct{})
	engine.turns["second"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		defer close(finished)
		err := checkBoundary(ctx, req, verified, tools, clone)
		switch mode {
		case "resume":
			if req.ResumeID != "session-first" || req.Prompt != second.Prompt {
				err = errors.Join(err, fmt.Errorf("resume context: %q %q", req.ResumeID, req.Prompt))
			}
		case "replay":
			history, prompt, _ := strings.Cut(req.Prompt, "\n\n")
			var replay struct {
				Source    string `json:"source"`
				Omitted   int    `json:"omitted_prefix_exchanges"`
				Exchanges []struct {
					Sequence      uint64       `json:"sequence"`
					Request       trace.Header `json:"request"`
					Response      trace.Header `json:"response"`
					TurnID        string       `json:"turn_id"`
					Prompt        string       `json:"prompt"`
					FinalResponse string       `json:"final_response"`
				} `json:"exchanges"`
			}
			if e := json.Unmarshal([]byte(history), &replay); e != nil || req.ResumeID != "" || prompt != second.Prompt || replay.Source != "osmia-owned-log" || replay.Omitted != 0 || len(replay.Exchanges) != 1 {
				err = errors.Join(err, fmt.Errorf("replay context: %q %q %v", req.ResumeID, req.Prompt, e))
			} else if x := replay.Exchanges[0]; x.Sequence != 1 || x.Request.ID != first.ID || x.TurnID != "first" || x.Prompt != first.Prompt || x.FinalResponse != "First answer" || strings.Contains(history, first.SystemPrompt) || strings.Contains(history, "small commits") {
				err = errors.Join(err, fmt.Errorf("replayed exchange: %s", history))
			}
		}
		notes, e1 := callTool(ctx, tools, "notes_read", map[string]any{})
		if e1 == nil && notes != `{"text":"first turn: owner prefers small commits"}` {
			e1 = fmt.Errorf("notes after restart: %s", notes)
		}
		_, e2 := callTool(ctx, tools, "notes_write", map[string]any{"text": "second turn: confirmed"})
		// Views are fresh copies; the first turn's edit was never copied back.
		content, e3 := callTool(ctx, tools, "file_read", map[string]any{"path": "src/app.txt"})
		if e3 == nil && content != `"original\n"` {
			e3 = fmt.Errorf("fresh view content %s", content)
		}
		if err = errors.Join(err, e1, e2, e3); err != nil {
			t.Error(err)
		}
		id := "session-second"
		if mode == "resume" {
			id = "session-first"
		}
		return &agent.Result{ClaudeID: id, ResultText: "Second answer", SessionDir: req.SessionDir, NumTurns: 2}, nil
	}
	s, err = Start(ctx, opts)
	must(t, err)
	// The loop reads its first tick only after the startup pass finishes.
	select {
	case ticks <- clock.Now():
	case <-time.After(demoTimeout):
		s.Close()
		t.Fatal("restarted controller did not finish its startup pass")
	}
	select {
	case <-finished:
	default:
		s.Close()
		t.Fatal("second turn did not run from durable state")
	}
	must(t, s.Close())

	// Inspect the history through the typed read API and ordinary files.
	repo, err = trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	defer repo.Close()
	if engine.runs[0] != "first" || engine.runs[1] != "second" || len(engine.runs) != 2 {
		t.Fatalf("backend runs %v", engine.runs)
	}
	if want := map[string]int{"resume": 1, "replay": 1}[mode]; engine.checks != want {
		t.Fatalf("resume checks %d", engine.checks)
	}
	th, err = repo.Thread(stream, demoAgent)
	must(t, err)
	if th.Identity.Role != demoRole || th.Identity.ThreadID != demoThread || th.Active != "" || th.Status != "idle" || len(th.Turns) != 2 {
		t.Fatalf("final thread: %+v", th)
	}
	requests, err := trace.Read[trace.TurnRequest](repo, stream)
	must(t, err)
	responses, err := trace.Read[trace.TurnResponse](repo, stream)
	must(t, err)
	costs, err := trace.Read[trace.Cost](repo, stream)
	must(t, err)
	transitions, err := trace.Read[trace.Transition](repo, stream)
	must(t, err)
	if len(requests) != 2 || len(responses) != 2 || len(costs) != 2 || len(transitions) != 2 {
		t.Fatalf("records: %d requests, %d responses, %d costs, %d transitions", len(requests), len(responses), len(costs), len(transitions))
	}
	finals := []string{"First answer", "Second answer"}
	for i, q := range th.Turns {
		req, res, cost := requests[i], responses[i], costs[i]
		if q.Sequence != uint64(i+1) || q.CompletedAt.IsZero() || q.Status() != "idle" || len(q.Attempts) != 1 || !reflect.DeepEqual(q.Request, req) || !reflect.DeepEqual(*q.Response, res) {
			t.Fatalf("turn %d: %+v", i, q)
		}
		if res.RequestID != req.ID || res.TurnID != req.TurnID || res.ThreadID != demoThread || res.Cause != req.Cause || res.Result.FinalResponse != finals[i] || res.Result.SessionDirectory != filepath.Join(root, "sessions", demoAgent, req.TurnID) {
			t.Fatalf("response provenance: %+v", res)
		}
		scope := coreadapter.Scope{Project: string(project), Workstream: string(stream), Thread: demoThread, Turn: req.TurnID, Role: demoRole}
		if cost.Entry.Scope != scope || cost.Entry.AttemptID != thread.AttemptID(req.ID, 1) || cost.Cause != req.Cause || cost.Entry.At != q.Attempts[0].At || cost.Entry.Usage != res.Result.Usage {
			t.Fatalf("cost provenance: %+v", cost)
		}
		if transitions[i].Cause != req.ID || transitions[i].ID != "deliver_"+req.TurnID {
			t.Fatalf("transition provenance: %+v", transitions[i])
		}
	}
	if costs[0].Entry.Usage != (coreadapter.Usage{CostUSD: 0.25, CostKnown: true, Turns: 3}) || costs[1].Entry.Usage != (coreadapter.Usage{Turns: 2}) {
		t.Fatalf("usage: %+v %+v", costs[0].Entry.Usage, costs[1].Entry.Usage)
	}
	if th.Turns[1].Claim.ServiceSession == firstSession {
		t.Fatal("second turn was not claimed by the restarted service")
	}
	a := th.Turns[1].Attempts[0]
	if a.Path != mode || a.SourceSequence != 1 || a.SourceSession != (coreadapter.BackendSession{Backend: "claude", ID: "session-first"}) {
		t.Fatalf("continuation attempt: %+v", a)
	}
	if mode == "replay" && (a.ReplayFrom != 1 || a.Omitted != 0) {
		t.Fatalf("replay bounds: %+v", a)
	}
	outbox, err := repo.Outbox(stream)
	must(t, err)
	ops, err = repo.Operations(stream)
	must(t, err)
	if len(outbox) != 2 || len(ops) != 2 {
		t.Fatalf("outbox %d, operations %d", len(outbox), len(ops))
	}
	for i, op := range ops {
		var in thread.TurnInput
		must(t, json.Unmarshal(op.Operation.Input, &in))
		var result thread.TurnResult
		if op.Result != nil {
			must(t, json.Unmarshal(op.Result.Data, &result))
		}
		q := th.Turns[slices.IndexFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == in.Turn })]
		if !op.Acknowledged || op.Result == nil || op.Result.Outcome != "idle" || result.ResponseID != q.Response.ID || result.RequestID != q.Request.ID || outbox[i].Event.Operation.ID != op.Operation.ID || op.Transition.ID != "deliver_"+in.Turn {
			t.Fatalf("operation %s: %+v", in.Turn, op)
		}
		kinds := map[string]int{}
		sessions := map[string]bool{}
		for _, action := range op.History {
			kinds[action.Kind]++
			sessions[action.Session] = true
			if action.Cause != op.Operation.ID || action.Actor.Kind != "service" {
				t.Fatalf("action provenance: %+v", action)
			}
		}
		if kinds["effect"] != 1 || kinds["result"] != 1 || kinds["acknowledge"] != 1 {
			t.Fatalf("operation %s history %v", in.Turn, kinds)
		}
		// The first intent spans both service sessions and was completed by inspection.
		if in.Turn == "first" && (kinds["claim"] != 2 || len(sessions) != 2 || op.Observation.State != coreadapter.EffectCompleted) {
			t.Fatalf("first operation after restart: %v %+v", kinds, op.Observation)
		}
		if in.Turn == "second" && (kinds["claim"] != 1 || len(sessions) != 1) {
			t.Fatalf("second operation: %v", kinds)
		}
	}

	// Ordinary files hold the same history, and nothing leaked or escaped.
	for _, name := range []string{"workflow.json", "events.jsonl", "ledger.jsonl", "agents/" + demoAgent + "/log.jsonl", "agents/" + demoAgent + "/identity.jsonl"} {
		path := "workstreams/" + string(stream) + "/" + name
		data, err := os.ReadFile(filepath.Join(traceDir, path))
		if err != nil || len(data) == 0 {
			t.Fatalf("trace file %s: %v", name, err)
		}
		if committed := demoGit(t, home, "-C", traceDir, "show", "HEAD:"+path); committed != string(data) {
			t.Fatalf("trace file %s differs from its committed version", name)
		}
	}
	log, err := os.ReadFile(filepath.Join(traceDir, "workstreams", string(stream), "agents", demoAgent, "log.jsonl"))
	must(t, err)
	if lines := bytes.Split(bytes.TrimSpace(log), []byte("\n")); len(lines) != 4 {
		t.Fatalf("owned log has %d records", len(lines))
	}
	if notes, err := os.ReadFile(filepath.Join(traceDir, "notes", demoRole+".md")); err != nil || string(notes) != "second turn: confirmed" {
		t.Fatalf("final notes: %q %v", notes, err)
	}
	if after := snapshot(t, clone); !reflect.DeepEqual(after, cloneBefore) {
		t.Fatal("runtime changed the target repository or its VCS metadata")
	}
	for path, content := range snapshot(t, root) {
		if strings.Contains(content, demoSecret) {
			t.Fatalf("delivery credential recorded in %s", path)
		}
	}
	if entries, err := os.ReadDir(views); err != nil || len(entries) != 0 {
		t.Fatalf("views leaked: %v %v", entries, err)
	}
	if workspaces.acquired != 2 || workspaces.released != 2 {
		t.Fatalf("workspace leases: %d acquired, %d released", workspaces.acquired, workspaces.released)
	}
}
