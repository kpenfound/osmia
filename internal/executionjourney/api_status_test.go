package executionjourney

import (
	context "context"
	json "encoding/json"
	errors "errors"
	fmt "fmt"
	agent "github.com/kpenfound/agentcore/agent"
	bundle "github.com/kpenfound/osmia/internal/bundle"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	isolation "github.com/kpenfound/osmia/internal/isolation"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/service"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	status "github.com/kpenfound/osmia/internal/status"
	thread "github.com/kpenfound/osmia/internal/thread"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
	os "os"
	filepath "path/filepath"
	reflect "reflect"
	slices "slices"
	strings "strings"
	sync "sync"
	testing "testing"
	time "time"
)

const chiefAgent = intakefixture.ChiefAgent

const chiefThread = intakefixture.ChiefThread

// queueChiefTurn accepts an owner message for the chief of staff and
// publishes the intent to run it.
func queueChiefTurn(t *testing.T, repo *trace.Repository, turn string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	owner := trace.Actor{Kind: "owner", ID: "local"}
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_" + turn, Project: project, Workstream: stream, At: at, Actor: owner, Cause: "message_" + turn, Depth: 1},
		AgentID: chiefAgent, ThreadID: chiefThread, TurnID: turn, Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"},
		SystemPrompt: "You are the chief of staff.", Prompt: "Owner message: " + turn}
	_, err := repo.EnqueueTurn(ctx, req)
	must(t, err)
	event := trace.EventID("deliver_"+turn, "turn")
	op, err := thread.TurnOperation(project, event, thread.TurnInput{Workstream: stream, Agent: chiefAgent, Turn: turn})
	must(t, err)
	tx := trace.Transaction{Transition: trace.Transition{
		Header:  trace.Header{Schema: "osmia.trace.transition", Version: 1, Revision: 1, ID: "deliver_" + turn, Project: project, Workstream: stream, At: at, Actor: trace.Actor{Kind: "service", ID: "inbox"}, Cause: req.ID, Depth: 1},
		Subject: "chief_inbox", To: "delivering", Reason: "Owner message accepted for the chief of staff"},
		Events: []trace.Event{{ID: event, Kind: "turn", Body: "Deliver " + turn, Operation: &op}}}
	_, err = repo.Transact(ctx, tx)
	must(t, err)
}

func TestChiefOfStaffStatusAcrossRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "cs-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := fixtureAt(t, home)
	cfg, err := config.Load(opts.Config)
	must(t, err)
	must(t, os.MkdirAll(cfg.Project.Clone, 0700))
	demoGit(t, home, "-C", cfg.Project.Clone, "init", "-q")

	clock := &demoClock{At: demoStart}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, clock.Now(), owner)
	must(t, err)
	must(t, repo.CreateWorkstream(ctx, stream, clock.Now(), owner))
	must(t, repo.CreateWorkstream(ctx, quiet, clock.Now(), owner))
	identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: chiefAgent, Project: project, Workstream: stream, At: clock.Now(), Actor: owner, Cause: "workstream_created"}, Role: "chief_of_staff", ThreadID: chiefThread}
	must(t, repo.CreateThread(ctx, identity))
	queueChiefTurn(t, repo, "status", clock.Now())
	must(t, repo.Close())

	sessions := &demoSessions{ByKey: map[string]*mcp.ClientSession{}}
	engine := &demoEngine{Sessions: sessions, Turns: map[string]demoTurn{}}
	engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error {
		return coreadapter.ErrResumeUnavailable
	}
	startTurn := make(chan struct{})
	views := filepath.Join(cfg.Root.String(), "views")
	must(t, os.Mkdir(views, 0700))
	opts.Threads = func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
		turns := &isolation.Turns{Workspaces: &demoWorkspaces{Directory: cfg.Project.Clone}, Views: isolation.Views{Directory: views}, Engine: engine,
			Grants: map[string]coreadapter.Capabilities{"chief_of_staff": {Tools: []string{"notes_read", status.ToolName}}},
			Select: func(context.Context, coreadapter.Scope) (isolation.Selection, error) {
				return isolation.Selection{Execution: coreadapter.ExecutionSettings{Mode: "container", Image: "fixture-image"}}, nil
			},
			Scoped: func(_ context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
				tools, err := r.NotesTools(chiefAgent, scope)
				if err != nil {
					return nil, err
				}
				set, err := status.Tool(r, chiefAgent, scope, clock.Now)
				return append(tools, set), err
			},
			Hosts: &coreadapter.MCPHost{Transport: &demoTransport{Sessions: sessions}},
		}
		return thread.Dispatcher{Runner: thread.Runner{Store: r, Turns: turns, Now: clock.Now},
			Prepare: func(ctx context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				select {
				case <-startTurn:
				case <-ctx.Done():
					return coreadapter.PreparedTurn{}, ctx.Err()
				}
				directory := filepath.Join(cfg.Root.String(), "sessions", in.Agent, in.Turn)
				return coreadapter.PreparedTurn{SessionDirectory: directory}, os.MkdirAll(directory, 0700)
			}}, nil
	}
	opts.Reconciliation.Now, opts.Reconciliation.Ticks = clock.Now, make(chan time.Time)

	// The fake chief of staff waits until the test has read the empty status,
	// then writes a rejected, a first and a replacing status.
	release, finished := make(chan struct{}), make(chan struct{})
	var results []string
	var mu sync.Mutex
	engine.Turns["status"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		defer close(finished)
		<-release
		listed, err := tools.ListTools(ctx, nil)
		if err != nil {
			return nil, err
		}
		var names []string
		for _, tool := range listed.Tools {
			names = append(names, tool.Name)
		}
		if want := []string{"notes_read", "set_status"}; !slices.Equal(names, want) {
			t.Errorf("chief of staff tools %v, want %v", names, want)
		}
		mu.Lock()
		defer mu.Unlock()
		for _, args := range []map[string]any{
			{"goal": "Finish the importer on " + chiefThread + ".", "note": "Started.", "agents": []string{}},
			{"goal": "Ship resumable uploads.", "note": "The plan is drafted.", "agents": []string{"The architect is drafting the plan."}},
			{"goal": "Ship resumable uploads.", "note": "The plan is ready for review. Work is starting.", "agents": []string{"The architect is idle."}},
		} {
			out, err := callTool(ctx, tools, status.ToolName, args)
			if err != nil {
				return nil, err
			}
			results = append(results, out)
		}
		return &agent.Result{ClaudeID: "session-chief", ResultText: "Status written", SessionDir: req.SessionDir, NumTurns: 1}, nil
	}

	s, c := start(t, opts)
	list, err := c.Statuses(ctx)
	must(t, err)
	groups, _, err := trace.Workgroups(s.sole().repository)
	must(t, err)
	empty := func(w config.WorkstreamID) WorkstreamStatus {
		group, ok := trace.WorkgroupOf(groups, w)
		if !ok {
			t.Fatalf("workstream %s has no recorded workgroup", w)
		}
		return WorkstreamStatus{DisplayState: "planning", Workgroup: &WorkgroupView{ID: group.ID, Members: []config.WorkstreamID{w}, Edges: []trace.Edge{}}, Workstream: w, Project: project, Units: []UnitStatus{}, Advisories: []OverlapAdvisory{}, Gates: []trace.OwnerGate{}, Agents: []AgentStatus{}, ContextMode: "file", Workspaces: config.WorkspacesGit}
	}
	if want := []WorkstreamStatus{empty(stream), empty(quiet)}; !sameStatuses(list.Workstreams, want) || len(list.Diagnostics) != 0 {
		t.Fatalf("before the chief of staff wrote: %+v", list)
	}
	close(startTurn)
	close(release)
	select {
	case <-finished:
	case <-time.After(demoTimeout):
		t.Fatal("chief-of-staff turn did not run")
	}
	mu.Lock()
	want := []string{
		`{"stored":false,"reason":"goal contains an Osmia or backend identifier (\"thread_chief\"); refer to the work or the agent in words"}`,
		`{"stored":true,"revision":1}`,
		`{"stored":true,"revision":2}`,
	}
	if !reflect.DeepEqual(results, want) {
		t.Fatalf("tool results:\n%s\nwant:\n%s", strings.Join(results, "\n"), strings.Join(want, "\n"))
	}
	mu.Unlock()
	stored, err := c.Status(ctx, stream)
	must(t, err)
	must(t, s.Close())

	check := func(got WorkstreamStatus) {
		t.Helper()
		st := got.Status
		if got.Workstream != stream || got.Project != project || got.State != nil || got.OpenQuestions != 0 || got.ContextMode != "file" || st == nil ||
			st.Goal != "Ship resumable uploads." || st.Attention != "" || st.Note != "The plan is ready for review. Work is starting." ||
			!slices.Equal(st.Agents, []string{"The architect is idle."}) || st.Revision != 2 || st.UpdatedAt.IsZero() {
			t.Fatalf("stored status: %+v %+v", got, st)
		}
	}
	check(stored)

	// A new service lifetime reads the same status without rerunning the turn.
	_, c = start(t, opts)
	after, err := c.Status(ctx, stream)
	must(t, err)
	check(after)
	if len(after.Agents) != 0 {
		t.Fatalf("completed turn still appears after restart: %+v", after.Agents)
	}
	stored.Agents = nil
	after.Agents = nil
	if !reflect.DeepEqual(after, stored) {
		t.Fatalf("stored status changed across restart: %+v, want %+v", after, stored)
	}
	none, err := c.Status(ctx, quiet)
	must(t, err)
	if !reflect.DeepEqual(none, empty(quiet)) {
		t.Fatalf("workstream without status: %+v", none)
	}
	raw := map[string]json.RawMessage{}
	must(t, c.Do(ctx, "GET", Prefix+"/status/"+string(quiet), nil, &raw))
	if string(raw["status"]) != "null" || string(raw["state"]) != "null" || string(raw["units"]) != "[]" || string(raw["gates"]) != "[]" || string(raw["open_questions"]) != "0" || string(raw["context_mode"]) != `"file"` {
		t.Fatalf("no-status JSON: %v", raw)
	}
	engine.Mu.Lock()
	defer engine.Mu.Unlock()
	if len(engine.Runs) != 1 {
		t.Fatalf("turn runs: %v", engine.Runs)
	}
}

func sameStatuses(got, want []WorkstreamStatus) bool {
	sort := func(list []WorkstreamStatus) {
		slices.SortFunc(list, func(a, b WorkstreamStatus) int { return strings.Compare(string(a.Workstream), string(b.Workstream)) })
	}
	got, want = slices.Clone(got), slices.Clone(want)
	sort(got)
	sort(want)
	return reflect.DeepEqual(got, want)
}

func TestStatusErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts := fixture(t)
	_, c := start(t, opts)
	// A configured project without a trace has no workstream status.
	list, err := c.Statuses(ctx)
	must(t, err)
	if len(list.Workstreams) != 0 || len(list.Diagnostics) != 0 {
		t.Fatalf("no trace: %+v", list)
	}
	for id, code := range map[string]Code{"w_bad": Validation, string(stream): NotFound, "": Validation} {
		var api *APIError
		_, err := c.Status(ctx, config.WorkstreamID(id))
		if !errors.As(err, &api) || api.Code != code || api.Message == "" {
			t.Errorf("%q: %v, want %s", id, err, code)
		}
	}

	// Without a project, the list is empty and one workstream is no_project.
	root := opts.Config.Root
	idle := filepath.Join(filepath.Dir(root), "idle")
	must(t, os.MkdirAll(idle, 0700))
	must(t, os.WriteFile(filepath.Join(idle, "config.toml"), []byte("version = 1\n[profiles.default]\nagent = \"claude\"\nmodel = \"test\"\n"), 0600))
	_, c = start(t, Options{Options: service.Options{Config: config.Options{Root: idle}, ShutdownTimeout: 100 * time.Millisecond}})
	list, err = c.Statuses(ctx)
	must(t, err)
	if len(list.Workstreams) != 0 {
		t.Fatalf("no project: %+v", list)
	}
	var api *APIError
	if _, err := c.Status(ctx, stream); !errors.As(err, &api) || api.Code != NoProject {
		t.Fatalf("no project: %v", err)
	}
}

func TestStatusReportsUnreadableTrace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "cu-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := fixtureAt(t, home)
	cfg, err := config.Load(opts.Config)
	must(t, err)
	must(t, os.MkdirAll(cfg.Project.Clone, 0700))
	demoGit(t, home, "-C", cfg.Project.Clone, "init", "-q")
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, demoStart, owner)
	must(t, err)
	t.Cleanup(func() { repo.Close() })
	must(t, repo.CreateWorkstream(ctx, stream, demoStart, owner))
	// A running controller stops the service on a damaged trace, so the
	// handlers are called on a service value without one.
	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{cfg.Project.ID: []config.WorkstreamID{stream}}})
	must(t, err)
	t.Cleanup(func() { store.Close() })
	s := &Service{cfg: cfg, projects: []*activeProject{runtimeFor(repo)}, store: store}
	traceExec(t, workstreamTraceDirectory(t, cfg.Root, project, stream), "INSERT INTO records (commit_seq, path, schema, id, revision, body) VALUES (1, ?, 'osmia.trace.status', 'status', 1, '{not json}')", "workstreams/"+string(stream)+"/status.jsonl")
	message := fmt.Sprintf("cannot read the workstream status of project %s; check the trace repository", project)
	list := s.statusList()
	usage := fmt.Sprintf("cannot read today's costs or turn attempts in project %s; check the trace repository", project)
	// The capacity reads no trace, and failure streaks read only the thread
	// records, so a damaged status record hides neither.
	if want := []Diagnostic{{Field: "provider_usage", Code: Internal, Message: usage}, {Field: "workstreams", Code: Internal, Message: message}}; len(list.Workstreams) != 0 || len(list.FailureStreaks) != 0 || list.Capacity == nil || list.ProviderUsage != nil || !reflect.DeepEqual(list.Diagnostics, want) {
		t.Fatalf("damaged trace: %+v", list)
	}
	if _, api := s.workstreamStatus(string(stream)); api == nil || *api != (APIError{Code: Internal, Message: message}) {
		t.Fatalf("damaged trace: %v", api)
	}
}

func TestStatusViewCarriesTraceFacts(t *testing.T) {
	t.Parallel()
	at := demoStart
	stored := &trace.Status{Header: trace.Header{Revision: 4, At: at}, StatusContent: trace.StatusContent{Goal: "g", Attention: "a", Note: "n", Agents: []string{"x"}}}
	state := "building"
	got := statusView(project, bundle.ModeFile, trace.WorkstreamStatus{Workstream: stream, Workspaces: config.WorkspacesJujutsu, State: state, OpenQuestions: 2, Status: stored}, false)
	want := WorkstreamStatus{Workstream: stream, Project: project, State: &state, Units: []UnitStatus{}, Advisories: []OverlapAdvisory{}, OpenQuestions: 2, Gates: []trace.OwnerGate{}, ContextMode: "file", Workspaces: config.WorkspacesJujutsu,
		Status: &StatusView{Goal: "g", Attention: "a", Note: "n", Agents: []string{"x"}, Revision: 4, UpdatedAt: at}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
