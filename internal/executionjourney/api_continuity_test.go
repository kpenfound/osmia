package executionjourney

import (
	context "context"
	json "encoding/json"
	errors "errors"
	fmt "fmt"
	fs "io/fs"
	os "os"
	filepath "path/filepath"
	reflect "reflect"
	slices "slices"
	strings "strings"
	sync "sync"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	isolation "github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/projectstore"
	fixturekit "github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	thread "github.com/kpenfound/osmia/internal/thread"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

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
	if want := []string{"file_read", "file_write"}; !slices.Equal(names, want) {
		fail("role tools %v, want %v", names, want)
	}
	if want := coreadapter.AllowedTools([]string{"osmia_0"}, names); !slices.Equal(req.Profile.AllowedTools, want) {
		fail("backend allow list %v, want %v", req.Profile.AllowedTools, want)
	}

	for _, name := range []string{"shell", "git_push"} {
		if _, err := fixturekit.CallTool(ctx, tools, name, map[string]any{}); err == nil {
			fail("runtime called %s", name)
		}
	}
	for _, path := range []string{".git/config", "../escape", filepath.Join(clone, "escape"), "src/../../escape"} {
		if _, err := fixturekit.CallTool(ctx, tools, "file_write", map[string]any{"path": path, "content": "tampered"}); err == nil {
			fail("runtime wrote %s", path)
		}
	}
	for _, path := range []string{".git/HEAD", "../config.toml", filepath.Join(clone, "src", "app.txt")} {
		if _, err := fixturekit.CallTool(ctx, tools, "file_read", map[string]any{"path": path}); err == nil {
			fail("runtime read %s", path)
		}
	}

	fixturekit.CheckGrants(req, verified, clone, fail)
	if strings.Contains(req.SystemPrompt, isolation.WebGuidance) || strings.Contains(req.Prompt, isolation.WebGuidance) {
		fail("a mason turn carries the web guidance")
	}
	if _, err := os.Lstat(filepath.Join(req.Workspace.Directory(), ".git")); !errors.Is(err, fs.ErrNotExist) {
		fail("view contains VCS metadata")
	}
	if req.Profile.VCSAccess || req.Workspace == nil || req.Workspace.VCS() != nil || len(req.VCSEnv) != 0 || len(req.ContainerEnv) != 0 {
		fail("request carries VCS access")
	}
	for key, value := range req.Env {
		if key != "OSMIA_MCP_TOKEN" || strings.Contains(value, fixturekit.DemoSecret) {
			fail("runtime environment exposes %s", key)
		}
	}
	for _, text := range []string{req.Prompt, req.SystemPrompt} {
		if strings.Contains(text, fixturekit.DemoSecret) {
			fail("prompt exposes a delivery credential")
		}
	}
	return errors.Join(problems...)
}

func TestThreadContinuityAcrossRestart(t *testing.T) {

	for _, mode := range []string{"resume", "replay"} {
		t.Run(mode, func(t *testing.T) { demonstrate(t, mode) })
	}
}

func demonstrate(t *testing.T, mode string) {
	for _, name := range []string{"GITHUB_TOKEN", "GH_TOKEN", "OSMIA_DELIVERY_TOKEN"} {
		t.Setenv(name, fixturekit.DemoSecret)
	}
	ctx := context.Background()
	var home string
	if keep := os.Getenv("OSMIA_CONTINUITY_DEMO_DIR"); keep != "" {
		home = filepath.Join(keep, mode)
		fixturekit.Must(t, os.MkdirAll(keep, 0700))
		fixturekit.Must(t, os.Mkdir(home, 0700))
	} else {
		var err error
		home, err = os.MkdirTemp("", "continuity-")
		fixturekit.Must(t, err)
		t.Cleanup(func() { os.RemoveAll(home) })
	}
	opts := journey.FixtureAt(t, home)
	cfg, err := projectstore.LoadConfig(opts.Config)
	fixturekit.Must(t, err)
	root := cfg.Root.String()
	traceDir, err := cfg.Root.ProjectTrace(fixturekit.Project)
	fixturekit.Must(t, err)
	t.Logf("%s demonstration root: %s; trace: %s", mode, root, traceDir)

	clone := cfg.Project.Clone
	fixturekit.Must(t, os.MkdirAll(filepath.Join(clone, "src"), 0700))
	fixturekit.Must(t, os.WriteFile(filepath.Join(clone, "src", "app.txt"), []byte("original\n"), 0600))
	fixturekit.DemoGit(t, home, "-C", clone, "init", "-q")
	fixturekit.DemoGit(t, home, "-C", clone, "add", "src/app.txt")
	fixturekit.DemoGit(t, home, "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	cloneBefore := intakefixture.Snapshot(t, clone)

	clock := &fixturekit.DemoClock{At: fixturekit.DemoStart}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, clock.Now(), owner)
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo.CreateWorkstream(ctx, fixturekit.Stream, clock.Now(), owner))
	identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: fixturekit.DemoAgent, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: clock.Now(), Actor: owner, Cause: "workstream_created", Depth: 0}, Role: fixturekit.DemoRole, ThreadID: fixturekit.DemoThread}
	fixturekit.Must(t, repo.CreateThread(ctx, identity))
	first := intakefixture.Deliver(t, repo, "first", 0, clock.Now())
	fixturekit.Must(t, repo.Close())

	sessions := &fixturekit.DemoSessions{ByKey: map[string]*mcp.ClientSession{}}
	engine := &fixturekit.DemoEngine{Sessions: sessions, Turns: map[string]fixturekit.DemoTurn{}}
	engine.Resume = func(previous, next coreadapter.Profile, session coreadapter.BackendSession) error {
		if mode == "resume" && previous == next && session == (coreadapter.BackendSession{Backend: "claude", ID: "session-first"}) {
			return nil
		}
		return coreadapter.ErrResumeUnavailable
	}
	workspaces := &fixturekit.DemoWorkspaces{Directory: clone}
	views := filepath.Join(root, "views")
	fixturekit.Must(t, os.Mkdir(views, 0700))
	var bound sync.Mutex
	var live *trace.Repository
	opts.Threads = func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
		bound.Lock()
		live = r
		bound.Unlock()
		turns := &isolation.Turns{Workspaces: workspaces, Views: isolation.Views{Directory: views}, Engine: engine,
			Grants: map[string]coreadapter.Capabilities{fixturekit.DemoRole: {Tools: []string{"file_read", "file_write", "shell", "git_push"}, WriteFiles: true}},
			Tools:  []coreadapter.Tool{{Name: "shell", Effect: coreadapter.ToolExecute}, {Name: "git_push", Effect: coreadapter.ToolVCS}},
			Select: func(context.Context, coreadapter.Scope) (isolation.Selection, error) {
				return isolation.Selection{Paths: []string{"src"}, Execution: coreadapter.ExecutionSettings{Mode: "container", Image: "fixture-image"}}, nil
			},
			Hosts: &coreadapter.MCPHost{Transport: &fixturekit.DemoTransport{Sessions: sessions}},
		}

		return thread.Dispatcher{Runner: thread.Runner{Jev: fixturekit.UnconfiguredJudge(nil), Store: r, Turns: turns, Now: clock.Now, ResumableRoles: func(string) bool { return true }},
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				directory := filepath.Join(root, "sessions", in.Agent, in.Turn)
				return coreadapter.PreparedTurn{SessionDirectory: directory}, os.MkdirAll(directory, 0700)
			}}, nil
	}
	ticks := make(chan time.Time)
	opts.Reconciliation.Now, opts.Reconciliation.Ticks = clock.Now, ticks
	current := func() *trace.Repository { bound.Lock(); defer bound.Unlock(); return live }

	entered := make(chan struct{})
	engine.Turns["first"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		err := checkBoundary(ctx, req, verified, tools, clone)
		if req.ResumeID != "" || !strings.HasSuffix(req.Prompt, first.Prompt) {
			err = errors.Join(err, fmt.Errorf("first turn context: %q %q", req.ResumeID, req.Prompt))
		}
		_, e2 := fixturekit.CallTool(ctx, tools, "file_write", map[string]any{"path": "src/app.txt", "content": "edited in turn one\n"})
		if err = errors.Join(err, e2); err != nil {
			t.Error(err)
		}
		close(entered)
		<-ctx.Done()
		return &agent.Result{ClaudeID: "session-first", ResultText: "First answer", SessionDir: req.SessionDir, NumTurns: 3, CostUSD: 0.25, CostKnown: true}, nil
	}
	s, err := journey.Start(ctx, opts)
	fixturekit.Must(t, err)
	select {
	case <-entered:
	case <-time.After(fixturekit.DemoTimeout):
		s.Close()
		t.Fatal("first turn did not start from durable intent")
	}
	repo = current()
	second := intakefixture.Deliver(t, repo, "second", 1, clock.Now())
	if _, err := repo.ClaimTurn(ctx, fixturekit.Stream, fixturekit.DemoAgent, "competing", filepath.Join(root, "competing"), clock.Now()); !errors.Is(err, trace.ErrClaimed) {
		t.Errorf("second turn claimable while first is active: %v", err)
	}
	th, err := repo.Thread(fixturekit.Stream, fixturekit.DemoAgent)
	fixturekit.Must(t, err)
	if th.Active != "first" || len(th.Turns) != 2 || th.Turns[1].Claim != nil {
		t.Fatalf("mid-turn queue: %+v", th)
	}
	fixturekit.Must(t, s.Close())

	repo, err = trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	th, err = repo.Thread(fixturekit.Stream, fixturekit.DemoAgent)
	fixturekit.Must(t, err)
	if th.Active != "" || th.Status != "idle" || th.Session.ID != "session-first" || th.Turns[0].CompletedAt.IsZero() || th.Turns[1].Claim != nil || th.Turns[1].Request.Prompt != second.Prompt {
		t.Fatalf("stopped state: %+v", th)
	}
	firstSession := th.Turns[0].Claim.ServiceSession
	ops, err := repo.Operations(fixturekit.Stream)
	fixturekit.Must(t, err)
	for _, op := range ops {
		var in thread.TurnInput
		fixturekit.Must(t, json.Unmarshal(op.Operation.Input, &in))
		switch {
		case in.Turn == "first" && (op.Claim == nil || op.Result != nil || op.Acknowledged):
			t.Fatalf("first operation was not left mid-reconciliation: %+v", op)

		case in.Turn == "second" && (op.EffectStarted || op.Result != nil || op.Acknowledged):
			t.Fatalf("second operation was applied before the restart: %+v", op)
		}
	}
	fixturekit.Must(t, repo.Close())

	finished := make(chan struct{})
	engine.Turns["second"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
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

		content, e3 := fixturekit.CallTool(ctx, tools, "file_read", map[string]any{"path": "src/app.txt"})
		if e3 == nil && content != `"original\n"` {
			e3 = fmt.Errorf("fresh view content %s", content)
		}
		if err = errors.Join(err, e3); err != nil {
			t.Error(err)
		}
		id := "session-second"
		if mode == "resume" {
			id = "session-first"
		}
		return &agent.Result{ClaudeID: id, ResultText: "Second answer", SessionDir: req.SessionDir, NumTurns: 2}, nil
	}
	s, err = journey.Start(ctx, opts)
	fixturekit.Must(t, err)
	select {
	case <-finished:
	case <-time.After(fixturekit.DemoTimeout):
		s.Close()
		t.Fatal("second turn did not run from durable state")
	}
	fixturekit.AwaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord {
		ops, err := current().Operations(fixturekit.Stream)
		fixturekit.Must(t, err)
		return ops
	})
	fixturekit.Must(t, s.Close())

	repo, err = trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	defer repo.Close()
	if engine.Runs[0] != "first" || engine.Runs[1] != "second" || len(engine.Runs) != 2 {
		t.Fatalf("backend runs %v", engine.Runs)
	}
	if want := map[string]int{"resume": 1, "replay": 1}[mode]; engine.Checks != want {
		t.Fatalf("resume checks %d", engine.Checks)
	}
	th, err = repo.Thread(fixturekit.Stream, fixturekit.DemoAgent)
	fixturekit.Must(t, err)
	if th.Identity.Role != fixturekit.DemoRole || th.Identity.ThreadID != fixturekit.DemoThread || th.Active != "" || th.Status != "idle" || len(th.Turns) != 2 {
		t.Fatalf("final thread: %+v", th)
	}
	requests, err := trace.Read[trace.TurnRequest](repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	responses, err := trace.Read[trace.TurnResponse](repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	costs, err := trace.Read[trace.Cost](repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	transitions, err := trace.Read[trace.Transition](repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if len(requests) != 2 || len(responses) != 2 || len(costs) != 2 || len(transitions) != 2 {
		t.Fatalf("records: %d requests, %d responses, %d costs, %d transitions", len(requests), len(responses), len(costs), len(transitions))
	}
	finals := []string{"First answer", "Second answer"}
	for i, q := range th.Turns {
		req, res, cost := requests[i], responses[i], costs[i]
		if q.Sequence != uint64(i+1) || q.CompletedAt.IsZero() || q.Status() != "idle" || len(q.Attempts) != 1 || !reflect.DeepEqual(q.Request, req) || !reflect.DeepEqual(*q.Response, res) {
			t.Fatalf("turn %d: %+v", i, q)
		}
		if res.RequestID != req.ID || res.TurnID != req.TurnID || res.ThreadID != fixturekit.DemoThread || res.Cause != req.Cause || res.Result.FinalResponse != finals[i] || res.Result.SessionDirectory != filepath.Join(root, "sessions", fixturekit.DemoAgent, req.TurnID) {
			t.Fatalf("response provenance: %+v", res)
		}
		scope := coreadapter.Scope{Project: string(fixturekit.Project), Workstream: string(fixturekit.Stream), Thread: fixturekit.DemoThread, Turn: req.TurnID, Role: fixturekit.DemoRole}
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
	outbox, err := repo.Outbox(fixturekit.Stream)
	fixturekit.Must(t, err)
	ops, err = repo.Operations(fixturekit.Stream)
	fixturekit.Must(t, err)
	if len(outbox) != 2 || len(ops) != 2 {
		t.Fatalf("outbox %d, operations %d", len(outbox), len(ops))
	}
	for i, op := range ops {
		var in thread.TurnInput
		fixturekit.Must(t, json.Unmarshal(op.Operation.Input, &in))
		var result thread.TurnResult
		if op.Result != nil {
			fixturekit.Must(t, json.Unmarshal(op.Result.Data, &result))
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

		if in.Turn == "first" && (kinds["claim"] != 2 || len(sessions) != 2 || op.Observation.State != coreadapter.EffectCompleted) {
			t.Fatalf("first operation after restart: %v %+v", kinds, op.Observation)
		}

		if in.Turn == "second" && (kinds["claim"] < 1 || op.Claim != nil || effectSession(op) != th.Turns[1].Claim.ServiceSession) {
			t.Fatalf("second operation: %v %+v", kinds, op.History)
		}
	}

	for _, name := range []string{"events.jsonl", "ledger.jsonl", "agents/" + fixturekit.DemoAgent + "/log.jsonl", "agents/" + fixturekit.DemoAgent + "/identity.jsonl"} {
		if len(fixturekit.WorkstreamRecords(t, cfg.Root, fixturekit.Project, fixturekit.Stream, name)) == 0 {
			t.Fatalf("trace records %s: none", name)
		}
	}
	if log := fixturekit.WorkstreamRecords(t, cfg.Root, fixturekit.Project, fixturekit.Stream, "agents/"+fixturekit.DemoAgent+"/log.jsonl"); len(log) != 4 {
		t.Fatalf("owned log has %d records", len(log))
	}
	if after := intakefixture.Snapshot(t, clone); !reflect.DeepEqual(after, cloneBefore) {
		t.Fatal("runtime changed the target repository or its VCS metadata")
	}
	for path, content := range intakefixture.Snapshot(t, root) {
		if strings.Contains(content, fixturekit.DemoSecret) {
			t.Fatalf("delivery credential recorded in %s", path)
		}
	}
	if entries, err := os.ReadDir(views); err != nil || len(entries) != 0 {
		t.Fatalf("views leaked: %v %v", entries, err)
	}
	if workspaces.Acquired != 2 || workspaces.Released != 2 {
		t.Fatalf("workspace leases: %d acquired, %d released", workspaces.Acquired, workspaces.Released)
	}
}

func effectSession(op trace.OperationRecord) string {
	for _, action := range op.History {
		if action.Kind == "effect" {
			return action.Session
		}
	}
	return ""
}
