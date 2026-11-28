package executionjourney

import (
	context "context"
	json "encoding/json"
	errors "errors"
	fmt "fmt"
	os "os"
	filepath "path/filepath"
	reflect "reflect"
	slices "slices"
	strings "strings"
	sync "sync"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	agent "github.com/kpenfound/agentcore/agent"
	bundle "github.com/kpenfound/osmia/internal/bundle"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	isolation "github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/projectstore"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	status "github.com/kpenfound/osmia/internal/status"
	telemetry "github.com/kpenfound/osmia/internal/telemetry"
	thread "github.com/kpenfound/osmia/internal/thread"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func queueManagerTurn(t *testing.T, repo *trace.Repository, turn string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	owner := trace.Actor{Kind: "owner", ID: "local"}
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_" + turn, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: at, Actor: owner, Cause: "message_" + turn, Depth: 1},
		AgentID: intakefixture.ManagerAgent, ThreadID: intakefixture.ManagerThread, TurnID: turn, Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"},
		SystemPrompt: "You are the chief of staff.", Prompt: "Owner message: " + turn}
	_, err := repo.EnqueueTurn(ctx, req)
	fixturekit.Must(t, err)
	event := trace.EventID("deliver_"+turn, "turn")
	op, err := thread.TurnOperation(fixturekit.Project, event, thread.TurnInput{Workstream: fixturekit.Stream, Agent: intakefixture.ManagerAgent, Turn: turn})
	fixturekit.Must(t, err)
	tx := trace.Transaction{Transition: trace.Transition{
		Header:  trace.Header{Schema: "osmia.trace.transition", Version: 1, Revision: 1, ID: "deliver_" + turn, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: at, Actor: trace.Actor{Kind: "service", ID: "inbox"}, Cause: req.ID, Depth: 1},
		Subject: "chief_inbox", To: "delivering", Reason: "Owner message accepted for the chief of staff"},
		Events: []trace.Event{{ID: event, Kind: "turn", Body: "Deliver " + turn, Operation: &op}}}
	_, err = repo.Transact(ctx, tx)
	fixturekit.Must(t, err)
}

func TestManagerStatusAcrossRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "cs-")
	fixturekit.Must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := journey.FixtureAt(t, home)
	cfg, err := projectstore.LoadConfig(opts.Config)
	fixturekit.Must(t, err)
	fixturekit.Must(t, os.MkdirAll(cfg.Project.Clone, 0700))
	fixturekit.DemoGit(t, home, "-C", cfg.Project.Clone, "init", "-q")

	clock := &fixturekit.DemoClock{At: fixturekit.DemoStart}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, clock.Now(), owner)
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo.CreateWorkstream(ctx, fixturekit.Stream, clock.Now(), owner))
	fixturekit.Must(t, repo.CreateWorkstream(ctx, fixturekit.Quiet, clock.Now(), owner))
	identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: intakefixture.ManagerAgent, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: clock.Now(), Actor: owner, Cause: "workstream_created"}, Role: "chief_of_staff", ThreadID: intakefixture.ManagerThread}
	fixturekit.Must(t, repo.CreateThread(ctx, identity))
	queueManagerTurn(t, repo, "status", clock.Now())
	fixturekit.Must(t, repo.Close())

	sessions := &fixturekit.DemoSessions{ByKey: map[string]*mcp.ClientSession{}}
	engine := &fixturekit.DemoEngine{Sessions: sessions, Turns: map[string]fixturekit.DemoTurn{}}
	engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error {
		return coreadapter.ErrResumeUnavailable
	}
	startTurn := make(chan struct{})
	views := filepath.Join(cfg.Root.String(), "views")
	fixturekit.Must(t, os.Mkdir(views, 0700))
	opts.Threads = func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
		turns := &isolation.Turns{Workspaces: &fixturekit.DemoWorkspaces{Directory: cfg.Project.Clone}, Views: isolation.Views{Directory: views}, Engine: engine,
			Grants: map[string]coreadapter.Capabilities{"chief_of_staff": {Tools: []string{status.ToolName}}},
			Select: func(context.Context, coreadapter.Scope) (isolation.Selection, error) {
				return isolation.Selection{Execution: coreadapter.ExecutionSettings{Mode: "container", Image: "fixture-image"}}, nil
			},
			Scoped: func(_ context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
				set, err := status.Tool(r, intakefixture.ManagerAgent, scope, clock.Now)
				return []coreadapter.Tool{set}, err
			},
			Hosts: &coreadapter.MCPHost{Transport: &fixturekit.DemoTransport{Sessions: sessions}},
		}
		return thread.Dispatcher{Runner: thread.Runner{Jev: fixturekit.UnconfiguredJudge(nil), Store: r, Turns: turns, Now: clock.Now},
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
		if want := []string{"set_status"}; !slices.Equal(names, want) {
			t.Errorf("chief of staff tools %v, want %v", names, want)
		}
		mu.Lock()
		defer mu.Unlock()
		for _, args := range []map[string]any{
			{"goal": "Finish the importer on " + intakefixture.ManagerThread + ".", "note": "Started.", "agents": []string{}},
			{"goal": "Ship resumable uploads.", "note": "The plan is drafted.", "agents": []string{"The architect is drafting the plan."}},
			{"goal": "Ship resumable uploads.", "note": "The plan is ready for review. Work is starting.", "agents": []string{"The architect is idle."}},
		} {
			out, err := fixturekit.CallTool(ctx, tools, status.ToolName, args)
			if err != nil {
				return nil, err
			}
			results = append(results, out)
		}
		return &agent.Result{ClaudeID: "session-chief", ResultText: "Status written", SessionDir: req.SessionDir, NumTurns: 1}, nil
	}

	s, c := journey.StartT(t, opts)
	list, err := c.Statuses(ctx)
	fixturekit.Must(t, err)
	groups, _, err := trace.Workgroups(s.Sole().Repository)
	fixturekit.Must(t, err)
	empty := func(w config.WorkstreamID) api.WorkstreamStatus {
		group, ok := trace.WorkgroupOf(groups, w)
		if !ok {
			t.Fatalf("workstream %s has no recorded workgroup", w)
		}
		return api.WorkstreamStatus{DisplayState: "planning", Workgroup: &api.WorkgroupView{ID: group.ID, Members: []config.WorkstreamID{w}, Edges: []trace.Edge{}}, Workstream: w, Project: fixturekit.Project, Units: []api.UnitStatus{}, Advisories: []api.OverlapAdvisory{}, Gates: []trace.OwnerGate{}, Agents: []api.AgentStatus{}, ContextMode: "file", Workspaces: config.WorkspacesGit,
			TelemetryTraceID: telemetry.WorkstreamTrace(w).String(), TelemetryRootSpanID: telemetry.WorkstreamRoot(w).String()}
	}
	if want := []api.WorkstreamStatus{empty(fixturekit.Stream), empty(fixturekit.Quiet)}; !sameStatuses(list.Workstreams, want) || len(list.Diagnostics) != 0 {
		t.Fatalf("before the chief of staff wrote: %+v", list)
	}
	close(startTurn)
	close(release)
	select {
	case <-finished:
	case <-time.After(fixturekit.DemoTimeout):
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
	stored, err := c.Status(ctx, fixturekit.Stream)
	fixturekit.Must(t, err)
	fixturekit.Must(t, s.Close())

	check := func(got api.WorkstreamStatus) {
		t.Helper()
		st := got.Status
		if got.Workstream != fixturekit.Stream || got.Project != fixturekit.Project || got.State != nil || got.OpenQuestions != 0 || got.ContextMode != "file" || st == nil ||
			st.Goal != "Ship resumable uploads." || st.Attention != "" || st.Note != "The plan is ready for review. Work is starting." ||
			!slices.Equal(st.Agents, []string{"The architect is idle."}) || st.Revision != 2 || st.UpdatedAt.IsZero() {
			t.Fatalf("stored status: %+v %+v", got, st)
		}
	}
	check(stored)

	_, c = journey.StartT(t, opts)
	after, err := c.Status(ctx, fixturekit.Stream)
	fixturekit.Must(t, err)
	check(after)
	if len(after.Agents) != 0 {
		t.Fatalf("completed turn still appears after restart: %+v", after.Agents)
	}
	stored.Agents = nil
	after.Agents = nil
	if !reflect.DeepEqual(after, stored) {
		t.Fatalf("stored status changed across restart: %+v, want %+v", after, stored)
	}
	none, err := c.Status(ctx, fixturekit.Quiet)
	fixturekit.Must(t, err)
	if !reflect.DeepEqual(none, empty(fixturekit.Quiet)) {
		t.Fatalf("workstream without status: %+v", none)
	}
	raw := map[string]json.RawMessage{}
	fixturekit.Must(t, c.Do(ctx, "GET", service.Prefix+"/status/"+string(fixturekit.Quiet), nil, &raw))
	if string(raw["status"]) != "null" || string(raw["state"]) != "null" || string(raw["units"]) != "[]" || string(raw["gates"]) != "[]" || string(raw["open_questions"]) != "0" || string(raw["context_mode"]) != `"file"` {
		t.Fatalf("no-status JSON: %v", raw)
	}
	engine.Mu.Lock()
	defer engine.Mu.Unlock()
	if len(engine.Runs) != 1 {
		t.Fatalf("turn runs: %v", engine.Runs)
	}
}

func sameStatuses(got, want []api.WorkstreamStatus) bool {
	sort := func(list []api.WorkstreamStatus) {
		slices.SortFunc(list, func(a, b api.WorkstreamStatus) int {
			return strings.Compare(string(a.Workstream), string(b.Workstream))
		})
	}
	got, want = slices.Clone(got), slices.Clone(want)
	sort(got)
	sort(want)
	return reflect.DeepEqual(got, want)
}

func TestStatusErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts := journey.Fixture(t)
	_, c := journey.StartT(t, opts)

	list, err := c.Statuses(ctx)
	fixturekit.Must(t, err)
	if len(list.Workstreams) != 0 || len(list.Diagnostics) != 0 {
		t.Fatalf("no trace: %+v", list)
	}
	for id, code := range map[string]api.Code{"w_bad": api.Validation, string(fixturekit.Stream): api.NotFound, "": api.Validation} {
		var apiErr *api.APIError
		_, err := c.Status(ctx, config.WorkstreamID(id))
		if !errors.As(err, &apiErr) || apiErr.Code != code || apiErr.Message == "" {
			t.Errorf("%q: %v, want %s", id, err, code)
		}
	}

	root := opts.Config.Root
	idle := filepath.Join(filepath.Dir(root), "idle")
	fixturekit.Must(t, os.MkdirAll(idle, 0700))
	fixturekit.Must(t, os.WriteFile(filepath.Join(idle, "config.toml"), []byte("version = 1\n[profiles.default]\nagent = \"claude\"\nmodel = \"test\"\n"), 0600))
	_, c = journey.StartT(t, journey.Options{Options: service.Options{Config: config.Options{Root: idle}, ShutdownTimeout: 100 * time.Millisecond}})
	list, err = c.Statuses(ctx)
	fixturekit.Must(t, err)
	if len(list.Workstreams) != 0 {
		t.Fatalf("no project: %+v", list)
	}
	var apiErr *api.APIError
	if _, err := c.Status(ctx, fixturekit.Stream); !errors.As(err, &apiErr) || apiErr.Code != api.NoProject {
		t.Fatalf("no project: %v", err)
	}
}

func TestStatusReportsUnreadableTrace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "cu-")
	fixturekit.Must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := journey.FixtureAt(t, home)
	cfg, err := projectstore.LoadConfig(opts.Config)
	fixturekit.Must(t, err)
	fixturekit.Must(t, os.MkdirAll(cfg.Project.Clone, 0700))
	fixturekit.DemoGit(t, home, "-C", cfg.Project.Clone, "init", "-q")
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, fixturekit.DemoStart, owner)
	fixturekit.Must(t, err)
	t.Cleanup(func() { repo.Close() })
	fixturekit.Must(t, repo.CreateWorkstream(ctx, fixturekit.Stream, fixturekit.DemoStart, owner))

	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{cfg.Project.ID: []config.WorkstreamID{fixturekit.Stream}}})
	fixturekit.Must(t, err)
	t.Cleanup(func() { store.Close() })
	s := &journey.Service{Cfg: cfg, Projects: []*journey.ActiveProject{journey.RuntimeFor(repo)}, Store: store}
	fixturekit.TraceExec(t, fixturekit.WorkstreamTraceDirectory(t, cfg.Root, fixturekit.Project, fixturekit.Stream), "INSERT INTO records (commit_seq, path, schema, id, revision, body) VALUES (1, ?, 'osmia.trace.status', 'status', 1, '{not json}')", "workstreams/"+string(fixturekit.Stream)+"/status.jsonl")
	message := fmt.Sprintf("cannot read the workstream status of project %s; check the trace repository", fixturekit.Project)
	list := s.StatusList()
	usage := fmt.Sprintf("cannot read today's costs or turn attempts in project %s; check the trace repository", fixturekit.Project)

	if want := []api.Diagnostic{{Field: "provider_usage", Code: api.Internal, Message: usage}, {Field: "workstreams", Code: api.Internal, Message: message}}; len(list.Workstreams) != 0 || len(list.FailureStreaks) != 0 || list.Capacity == nil || list.ProviderUsage != nil || !reflect.DeepEqual(list.Diagnostics, want) {
		t.Fatalf("damaged trace: %+v", list)
	}
	if _, apiErr := s.WorkstreamStatus(string(fixturekit.Stream)); apiErr == nil || *apiErr != (api.APIError{Code: api.Internal, Message: message}) {
		t.Fatalf("damaged trace: %v", apiErr)
	}
}

func TestStatusViewCarriesTraceFacts(t *testing.T) {
	t.Parallel()
	at := fixturekit.DemoStart
	stored := &trace.Status{Header: trace.Header{Revision: 4, At: at}, StatusContent: trace.StatusContent{Goal: "g", Attention: "a", Note: "n", Agents: []string{"x"}}}
	state := "building"
	got := service.WorkstreamStatusView(fixturekit.Project, bundle.ModeFile, trace.WorkstreamStatus{Workstream: fixturekit.Stream, Workspaces: config.WorkspacesJujutsu, State: state, OpenQuestions: 2, Status: stored}, false)
	want := api.WorkstreamStatus{Workstream: fixturekit.Stream, Project: fixturekit.Project, State: &state, Units: []api.UnitStatus{}, Advisories: []api.OverlapAdvisory{}, OpenQuestions: 2, Gates: []trace.OwnerGate{}, ContextMode: "file", Workspaces: config.WorkspacesJujutsu,
		Status: &api.StatusView{Goal: "g", Attention: "a", Note: "n", Agents: []string{"x"}, Revision: 4, UpdatedAt: at}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
