package executionjourney

import (
	bytes "bytes"
	context "context"
	errors "errors"
	fmt "fmt"
	maps "maps"
	os "os"
	filepath "path/filepath"
	reflect "reflect"
	slices "slices"
	strings "strings"
	sync "sync"
	atomic "sync/atomic"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	envelope "github.com/kpenfound/osmia/internal/envelope"
	isolation "github.com/kpenfound/osmia/internal/isolation"
	plan "github.com/kpenfound/osmia/internal/plan"
	questions "github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	thread "github.com/kpenfound/osmia/internal/thread"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const managerDemoRoles = `[capacity]
masons = 1
[roles.manager]
sandbox = "container"
image = "fixture-image"
[roles.mason]
sandbox = "container"
image = "fixture-image"
[roles.reviewer]
sandbox = "container"
image = "fixture-image"
`

const (
	managerDemoCharter  = "# Charter\n\n1. Keep state in files under the root.\n"
	managerDemoSpec     = "# Resumable uploads\n"
	managerDemoReply    = "Two workers are building the upload work. I will tell you when they need a decision."
	managerDemoAnswer   = "In files under the root."
	managerDemoRuling   = "Keep the upload API as it is and add a new endpoint."
	managerDemoRelayed  = "The upload API stays unchanged; add a new endpoint for resumable uploads."
	managerDemoQuestion = "May I change the upload API's response?"
)

var managerDemoStatus = api.StatusView{
	Goal:      "Ship resumable uploads.",
	Attention: "",
	Note:      "Two masons and a reviewer are at work on the upload unit.",
	Agents:    []string{"The first mason is building the upload unit.", "The second mason is building the index unit.", "The reviewer is reviewing the upload unit."},
}

var managerDemoAgents = map[string]string{fixturekit.DemoThread: fixturekit.DemoAgent, "thread_mason2": "agent_mason2", "thread_reviewer": "agent_reviewer"}

type managerFixture struct {
	t      *testing.T
	ctx    context.Context
	opts   journey.Options
	engine *fixturekit.DemoEngine
	clock  *fixturekit.FixedClock
	cfg    *config.Config
	id     config.ProjectID
	ticks  chan time.Time
	passes atomic.Uint64
	lives  chan *trace.Repository

	s    *journey.Service
	c    *journey.Client
	repo *trace.Repository

	mu         sync.Mutex
	problems   []string
	results    map[string][]string
	prompts    map[string]agent.Request
	sent       []api.ConversationEntry
	events     map[string]int
	current    *trace.Repository
	masonAsked chan struct{}
	asked      sync.Once
}

func newManagerFixture(t *testing.T) *managerFixture {
	t.Helper()
	ctx := context.Background()
	lf := newCloneFixture(t)
	opts, engine, sessions := lf.opts, lf.engine, lf.sessions
	configFile, err := os.OpenFile(filepath.Join(opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	fixturekit.Must(t, err)
	_, err = configFile.WriteString(managerDemoRoles)
	fixturekit.Must(t, errors.Join(err, configFile.Close()))

	engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error { return nil }

	clock := &fixturekit.FixedClock{At: fixturekit.DemoStart}
	opts.Reconciliation.Now = clock.Now
	f := &managerFixture{t: t, ctx: ctx, engine: engine, clock: clock, lives: make(chan *trace.Repository, 1),
		results: map[string][]string{}, prompts: map[string]agent.Request{}, events: map[string]int{}, masonAsked: make(chan struct{})}
	t.Cleanup(f.releaseReviewer)
	opts = journey.Enforce(opts, service.Enforcement{Engine: engine, Hosts: &coreadapter.MCPHost{Transport: &fixturekit.DemoTransport{Sessions: sessions}}})
	production := opts.Threads
	opts.Threads = func(r *trace.Repository, cfg *config.Config) (coreadapter.Reconciler, error) {
		bound, err := production(r, cfg)
		if err != nil {
			return nil, err
		}
		select {
		case <-f.lives:
		default:
		}
		f.lives <- r

		turns := bound.(thread.Dispatcher).Runner.Turns.(*questions.Turns).Turns.(*execution.VerdictTurns).Turns.(*execution.ReportingTurns).Turns.(*execution.RetriedTurns).Turns.(*isolation.Turns)
		turns.Grants = maps.Clone(turns.Grants)
		for _, role := range []string{"mason", "reviewer"} {
			turns.Grants[role] = coreadapter.Capabilities{Tools: []string{questions.AskTool}}
		}

		turns.Workspaces, turns.Capture = intake.StagedWorkspaces{}, nil
		selectManager := turns.Select
		turns.Select = func(ctx context.Context, scope coreadapter.Scope) (isolation.Selection, error) {
			if scope.Role == trace.Manager {
				return selectManager(ctx, scope)
			}
			workspace := filepath.Join(cfg.Root.String(), "workers", scope.Thread)
			role := cfg.Roles[config.ManagerRole]
			return isolation.Selection{Workspace: coreadapter.WorkspaceRequest{SourceDirectory: workspace, Directory: workspace},
				Execution: coreadapter.ExecutionSettings{Mode: role.Sandbox, Image: role.Image}}, os.MkdirAll(workspace, 0700)
		}
		manager := turns.Scoped
		turns.Scoped = func(ctx context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
			if scope.Role == trace.Manager {
				return manager(ctx, scope)
			}
			return questions.Tools(r, managerDemoAgents[scope.Thread], scope, clock.Now)
		}
		return bound, nil
	}

	s, c := journey.StartT(t, opts)
	added, err := c.AddProject(ctx, fixturekit.Request(lf.clone))
	fixturekit.Must(t, err)
	f.id = added.Project.ID
	intakefixture.WriteCharter(t, c, added.Project.ID, managerDemoCharter)
	onboarded, err := c.Configuration(ctx)
	fixturekit.Must(t, err)
	if cs := onboarded.Project.CharterState; cs == nil || cs.Rules != 1 {
		t.Fatalf("charter: %+v", cs)
	}
	f.cfg = s.Current()
	c.Close()
	fixturekit.Must(t, s.Close())

	repo, err := trace.Open(f.cfg.Root, f.cfg.For(f.id).Project)
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo.CreateWorkstream(ctx, fixturekit.Stream, clock.Now(), intake.OwnerActor))
	fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: plan.SpecDocument, Revision: 1, Project: f.id, Workstream: fixturekit.Stream, At: clock.Now(), Actor: intake.ArchitectActor, Cause: "fixture", Depth: 1},
		Path: plan.SpecPath, Content: managerDemoSpec}}))
	for _, w := range []struct{ agent, thread, role, turn string }{
		{fixturekit.DemoAgent, fixturekit.DemoThread, "mason", "build"}, {"agent_mason2", "thread_mason2", "mason", "build2"}, {"agent_reviewer", "thread_reviewer", "reviewer", "review"},
	} {
		h := trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: w.agent, Project: f.id, Workstream: fixturekit.Stream, At: clock.Now(), Actor: intake.OwnerActor, Cause: "workstream_created"}
		fixturekit.Must(t, repo.CreateThread(ctx, trace.Agent{Header: h, Role: w.role, ThreadID: w.thread}))
		h.Schema, h.ID, h.Cause, h.Depth = "osmia.trace.turn-request", "request_"+w.turn, "message_"+w.turn, 1
		_, err := repo.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: w.agent, ThreadID: w.thread, TurnID: w.turn,
			Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "You are the " + w.role + ".", Prompt: "Work: " + w.turn})
		fixturekit.Must(t, err)
	}
	fixturekit.Must(t, repo.Close())
	select {
	case <-f.lives:
	default:
	}

	f.ticks = make(chan time.Time)
	opts.Reconciliation.Ticks = f.ticks
	opts.SchedulePassed = func() { f.passes.Add(1) }
	f.opts = opts

	engine.Turns["build"] = f.worker("Where does state live?")
	engine.Turns["build2"] = f.worker("")
	reviewerTurn := f.worker(managerDemoQuestion)
	engine.Turns["review"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, session *mcp.ClientSession) (*agent.Result, error) {
		select {
		case <-f.masonAsked:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return reviewerTurn(ctx, req, turn, session)
	}
	resumed := func(ctx context.Context, req agent.Request, _ *agent.Turn, session *mcp.ClientSession) (*agent.Result, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.prompts[req.Name] = req
		f.checkTools(ctx, req.Name, session, questions.AskTool)
		return intakefixture.QuestionResult(req, "session-"+req.Name, "Continued"), nil
	}
	engine.Turns["*"] = f.managerTurn
	engine.Turns["answer_1"], engine.Turns["answer_2"] = resumed, resumed
	return f
}

func (f *managerFixture) releaseReviewer() { f.asked.Do(func() { close(f.masonAsked) }) }

func (f *managerFixture) problem(args ...any) { f.problems = append(f.problems, fmt.Sprintln(args...)) }

func (f *managerFixture) use(ctx context.Context, tools *mcp.ClientSession, step, name string, args map[string]any) {
	out, err := fixturekit.CallTool(ctx, tools, name, args)
	if err != nil {
		f.problem(step, err)
	}
	f.results[step] = append(f.results[step], out)
}

func (f *managerFixture) checkTools(ctx context.Context, name string, session *mcp.ClientSession, want ...string) {
	if got, err := intakefixture.ToolNames(ctx, session); err != nil || !slices.Equal(got, want) {
		f.problem("tools of", name, got, err)
	}
}

func (f *managerFixture) worker(question string) fixturekit.DemoTurn {
	return func(ctx context.Context, req agent.Request, _ *agent.Turn, session *mcp.ClientSession) (*agent.Result, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.prompts[req.Name] = req
		f.checkTools(ctx, req.Name, session, questions.AskTool)
		if question != "" {
			f.use(ctx, session, req.Name, questions.AskTool, map[string]any{"question": question})
			return intakefixture.QuestionResult(req, "session-"+req.Name, "Asked"), nil
		}

		th, err := f.current.Thread(fixturekit.Stream, fixturekit.DemoAgent)
		if err != nil || !th.Parked() {
			f.problem("second mason ran before the first parked", err)
		}
		return intakefixture.QuestionResult(req, "session-"+req.Name, "Built"), nil
	}
}

func (f *managerFixture) managerTurn(ctx context.Context, req agent.Request, _ *agent.Turn, session *mcp.ClientSession) (*agent.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompts[req.Name] = req
	f.checkTools(ctx, req.Name, session, "answer", "ask_owner_about_checks", "capacity", "decide", "decide_amendment", "decide_change", "decide_charter", "drop_dependency", "escalate", "factory_context", "file_read", "flake_fix", "follow_up", "hand_back_drift", "inspect_code", "log_assumption", "move_unit", "move_workstream", "notify", "pause", "preview", "prioritise", "project_context", "propose_charter", "pull_request_checks", "read_cloud_trace", "read_remote_file", "record_delivery_brief", "record_discovery", "relay_ruling", "request_change", "resolve_contested", "resolve_objection", "resume", "retry_change", "route_amendment", "set_status", "triage_finding")
	switch {
	case len(f.sent) > 0 && req.Name == f.sent[0].Turn:

		if listed, err := intakefixture.ReadTool(ctx, session, "."); err != nil || listed != plan.SpecPath {
			f.problem("manager view lists", listed, err)
		}
		if spec, err := intakefixture.ReadTool(ctx, session, plan.SpecPath); err != nil || spec != managerDemoSpec {
			f.problem("manager reads spec", spec, err)
		}

		refused := map[string]any{"goal": managerDemoStatus.Goal, "note": managerDemoStatus.Note, "agents": []string{fixturekit.DemoAgent + " is building."}}
		f.use(ctx, session, "status", "set_status", refused)
		f.use(ctx, session, "status", "set_status", map[string]any{"goal": managerDemoStatus.Goal, "attention": managerDemoStatus.Attention, "note": managerDemoStatus.Note, "agents": managerDemoStatus.Agents})
		return intakefixture.QuestionResult(req, "session-chief", managerDemoReply), nil
	case len(f.sent) > 1 && req.Name != f.sent[0].Turn && !strings.HasPrefix(req.Name, "events_"):
		return intakefixture.QuestionResult(req, "session-chief", "The upload API stays as it is."), nil
	case !strings.HasPrefix(req.Name, "events_"):

	case strings.Contains(req.Prompt, "asked by the mason: Where does state live?"):
		f.events["questions"]++
		if !strings.Contains(req.Prompt, "asked by the reviewer: "+managerDemoQuestion) {
			f.problem("the questions were not delivered together", req.Prompt)
		}
		for _, id := range []string{"1", "2"} {
			f.use(ctx, session, "decide", "decide", map[string]any{"kind": "question-routing", "subject": id, "evidence": "The manager investigated the question."})
		}
		f.use(ctx, session, "chief", "answer", map[string]any{"question": "1", "text": managerDemoAnswer, "citations": []string{"charter#1"}})
		f.use(ctx, session, "chief", "escalate", map[string]any{"questions": []string{"2"}, "rephrasing": "May the upload API's response change?",
			"blocked": "The reviewer's review of the upload unit.", "options": []string{"Change it", "Keep it and add an endpoint"}, "recommendation": "Keep it and add an endpoint."})
		return intakefixture.QuestionResult(req, "session-chief", "Answered one, escalated one"), nil
	case strings.Contains(req.Prompt, ", escalation_2 (questions 2): "+managerDemoRuling):
		f.events["ruling"]++
		f.use(ctx, session, "chief", "relay_ruling", map[string]any{"question": "2", "text": managerDemoRelayed, "scope": "notify"})
		return intakefixture.QuestionResult(req, "session-chief", "Relayed"), nil
	}
	f.problem("unexpected manager turn", req.Name, req.Prompt)
	return intakefixture.QuestionResult(req, "session-chief", "Nothing to do"), nil
}

func (f *managerFixture) start() {
	f.t.Helper()
	s, err := journey.Start(f.ctx, f.opts)
	fixturekit.Must(f.t, err)
	c := journey.NewClient(s.Socket())
	f.t.Cleanup(func() {
		c.Close()
		if err := s.Close(); err != nil {
			f.t.Errorf("service cleanup: %v", err)
		}
	})
	select {
	case r := <-f.lives:
		f.s, f.c, f.repo = s, c, r
		f.mu.Lock()
		f.current = r
		f.mu.Unlock()
	case <-s.Done:
		f.t.Fatalf("service stopped before opening the trace: %v", s.Wait())
	case <-time.After(fixturekit.DemoTimeout):
		f.t.Fatal("service did not open the trace")
	}
}

func (f *managerFixture) stop() {
	f.t.Helper()
	f.c.Close()
	fixturekit.Must(f.t, f.s.Close())
}

func (f *managerFixture) runs() map[string]int {
	f.engine.Mu.Lock()
	defer f.engine.Mu.Unlock()
	out := map[string]int{}
	for _, name := range f.engine.Runs {
		switch {
		case strings.HasPrefix(name, "events_"):
			name = "events"
		case strings.HasPrefix(name, "message_"):
			name = "message"
		}
		out[name]++
	}
	return out
}

func (f *managerFixture) question(id string) trace.QuestionState {
	f.t.Helper()
	return questionIn(f.t, f.repo, id)
}

func questionIn(t *testing.T, r *trace.Repository, id string) trace.QuestionState {
	t.Helper()
	list, err := r.Questions(fixturekit.Stream)
	fixturekit.Must(t, err)
	for _, q := range list {
		if q.Asked.ID == id {
			return q
		}
	}
	t.Fatalf("no question %s in %+v", id, list)
	return trace.QuestionState{}
}

func (f *managerFixture) threadOf(agent string) trace.Thread {
	f.t.Helper()
	th, err := f.repo.Thread(fixturekit.Stream, agent)
	fixturekit.Must(f.t, err)
	return th
}

func (f *managerFixture) turnDone(agent, turn, status string) bool {
	th := f.threadOf(agent)
	for _, q := range th.Turns {
		if q.Request.TurnID == turn {
			if q.Response == nil || q.CompletedAt.IsZero() {
				return false
			}
			if q.Response.RequestID != q.Request.ID || q.Response.RequestRevision != q.Request.Revision ||
				q.Response.ThreadID != q.Request.ThreadID || q.Response.TurnID != q.Request.TurnID {
				f.t.Fatalf("turn %s response lost its accepted request link: request=%+v response=%+v", turn, q.Request, q.Response)
			}
			return q.Status() == status
		}
	}
	return false
}

func (f *managerFixture) ownerDone(entry api.ConversationEntry) bool {
	manager, err := f.repo.ManagerThread(fixturekit.Stream)
	fixturekit.Must(f.t, err)
	return f.turnDone(manager.Identity.ID, entry.Turn, "idle")
}

func (f *managerFixture) quiescent() bool {
	threads, err := f.repo.Threads(fixturekit.Stream)
	fixturekit.Must(f.t, err)
	for _, th := range threads {
		if th.Active != "" {
			return false
		}
		for _, q := range th.Turns {
			if q.Response == nil || q.CompletedAt.IsZero() {
				return false
			}
		}
	}
	operations, err := f.repo.Operations(fixturekit.Stream)
	fixturekit.Must(f.t, err)
	for _, op := range operations {
		if op.Operation.Action == thread.TurnAction && !op.Acknowledged {
			return false
		}
	}
	return true
}

func (f *managerFixture) await(phase string, ready func() bool) {
	f.t.Helper()
	t, s, r := f.t, f.s, f.repo
	sub, ok := service.InspectForTest(s.Core()).Hub().Subscribe()
	if !ok {
		t.Fatalf("%s: cannot subscribe to stopped service: %v", phase, s.Wait())
	}
	defer service.InspectForTest(s.Core()).Hub().Unsubscribe(sub)
	deadline := time.NewTimer(fixturekit.DemoTimeout)
	defer deadline.Stop()
	poll := time.NewTicker(time.Second)
	defer poll.Stop()
	diagnose := func(reason string) {
		t.Helper()
		threads, err := r.Threads(fixturekit.Stream)
		t.Logf("PHASE %s project=%s workstream=%s reason=%s now=%s passes=%d threads_error=%v", phase, f.id, fixturekit.Stream, reason, f.clock.Now(), f.passes.Load(), err)
		for _, th := range threads {
			t.Logf("worker=%s thread=%s active=%s status=%s parked=%v", th.Identity.ID, th.Identity.ThreadID, th.Active, th.Status, th.Parked())
			for _, q := range th.Turns {
				response, failure := "", ""
				if q.Response != nil {
					response, failure = q.Response.ID, q.Response.Failure
				}
				t.Logf("worker=%s turn=%s request=%s response=%s claim=%+v completed=%s outcome=%s attempts=%d failure=%q", th.Identity.ID, q.Request.TurnID, q.Request.ID, response, q.Claim, q.CompletedAt, q.Status(), len(q.Attempts), failure)
				for _, attempt := range q.Attempts {
					t.Logf("turn=%s attempt=%d captured=%v failure=%q class=%s", q.Request.TurnID, attempt.Number, attempt.Result != nil, attempt.Failure, attempt.FailureClass)
				}
			}
		}
		operations, err := r.Operations(fixturekit.Stream)
		t.Logf("operations_error=%v", err)
		for _, op := range operations {
			if op.Operation.Action != thread.TurnAction {
				continue
			}
			in, decodeErr := thread.DecodeTurn(op.Operation)
			observed, inspectErr := (thread.Dispatcher{Runner: thread.Runner{Jev: fixturekit.UnconfiguredJudge(nil), Store: r}}).Inspect(f.ctx, op.Operation)
			outcome, failure := "", ""
			if op.Result != nil {
				outcome = op.Result.Outcome
			}
			for _, action := range op.History {
				if action.Failure != "" {
					failure = action.Failure
				}
			}
			t.Logf("operation=%s worker=%s turn=%s acknowledged=%v outcome=%s observation=%s evidence=%q retry=%s failure=%q decode_error=%v inspect_error=%v", op.Operation.ID, in.Agent, in.Turn, op.Acknowledged, outcome, observed.State, observed.Evidence, op.RetryAt, failure, decodeErr, inspectErr)
		}
		select {
		case <-s.Done:
			t.Logf("service stopped: %v", s.Wait())
		default:
			t.Logf("service active lifetime_error=%v", s.Lifetime.Err())
		}
		t.Fatalf("chief demonstration did not complete %s: %s", phase, reason)
	}
	for {
		select {
		case <-s.Done:
			diagnose("service stopped")
		default:
		}
		if ready() {
			return
		}
		select {
		case f.ticks <- f.clock.Now():
		case <-sub.Wake():
			sub.Take()
		case <-poll.C:
		case <-s.Done:
			diagnose("service stopped")
		case <-deadline.C:
			diagnose("deadline reached")
		}
	}
}

func (f *managerFixture) send(text string) api.ConversationEntry {
	f.t.Helper()
	f.mu.Lock()
	message, err := f.c.Send(f.ctx, fixturekit.Stream, text)
	f.sent = append(f.sent, message)
	f.mu.Unlock()
	fixturekit.Must(f.t, err)
	if message.State != api.TurnQueued {
		f.t.Fatalf("sent: %+v", message)
	}
	return message
}

func (f *managerFixture) checkProblems() {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.problems) != 0 {
		f.t.Fatalf("fake agents saw:\n%s", strings.Join(f.problems, "\n"))
	}
}

func (f *managerFixture) askQuestions() api.ConversationEntry {
	f.t.Helper()
	message := f.send("Where do we stand?")
	f.await("owner reply and both questions with the second mason completed", func() bool {
		asked, err := f.repo.Questions(fixturekit.Stream)
		fixturekit.Must(f.t, err)
		for _, q := range asked {
			if q.Asked.ID == "1" && q.Asked.Thread == fixturekit.DemoThread && q.Asked.Question == "Where does state live?" {
				f.releaseReviewer()
			}
		}
		if !f.ownerDone(message) || !f.turnDone(fixturekit.DemoAgent, "build", "waiting") ||
			!f.turnDone("agent_reviewer", "review", "waiting") || !f.turnDone("agent_mason2", "build2", "idle") {
			return false
		}
		return f.threadOf(fixturekit.DemoAgent).Parked() && f.threadOf("agent_reviewer").Parked() &&
			f.question("1").State == trace.QuestionOpen && f.question("2").State == trace.QuestionOpen && f.quiescent()
	})
	return message
}

func (f *managerFixture) answerAndEscalate() {
	f.t.Helper()
	f.clock.Advance(time.Minute)
	f.await("cited answer and escalation", func() bool {
		return f.turnDone(fixturekit.DemoAgent, "answer_1", "idle") && f.question("1").State == trace.QuestionAnswered &&
			f.question("2").State == trace.QuestionEscalated && f.threadOf("agent_reviewer").Parked() && f.quiescent()
	})
}

func (f *managerFixture) wantEscalation() (api.InboxEntry, api.ConversationEntry) {
	want := api.InboxEntry{Kind: api.InboxEscalation, Project: f.repo.Project(), Number: 1, Workstream: fixturekit.Stream, Batch: "escalation_2", Question: "May the upload API's response change?", Blocked: "The reviewer's review of the upload unit.",
		Options: []string{"Change it", "Keep it and add an endpoint"}, Recommendation: "Keep it and add an endpoint.", QuickReply: "Keep it and add an endpoint.", OpenedAt: f.clock.Now(), Answer: api.InboxAnswer{Method: "POST", Path: "/v1/inbox/1", Body: map[string]any{"project": string(f.repo.Project())}},
		Asked: []api.InboxQuestion{{ID: "2", AskedBy: "agent_reviewer", Question: managerDemoQuestion}}}
	action := api.ConversationEntry{Turn: want.Batch, Kind: "action", State: api.TurnDone, At: want.OpenedAt,
		Text: "Question for you: " + want.Question + "\nBlocked: " + want.Blocked +
			"\nOptions: " + strings.Join(want.Options, "; ") + "\nRecommendation: " + want.Recommendation}
	return want, action
}

func (f *managerFixture) reopenedRepository() *trace.Repository {
	f.t.Helper()
	repo, err := trace.Open(f.cfg.Root, f.cfg.For(f.id).Project)
	fixturekit.Must(f.t, err)
	f.t.Cleanup(func() { repo.Close() })
	return repo
}

func (f *managerFixture) committed(path string, wants ...string) {
	f.t.Helper()
	committed := string(bytes.Join(fixturekit.WorkstreamRecords(f.t, f.cfg.Root, f.id, fixturekit.Stream, path), []byte("\n")))
	for _, w := range wants {
		if !strings.Contains(committed, w) {
			f.t.Fatalf("committed %s lacks %q:\n%s", path, w, committed)
		}
	}
}

func checkEventTurns(t *testing.T, repo *trace.Repository, want int) {
	t.Helper()
	threads, err := repo.Threads(fixturekit.Stream)
	fixturekit.Must(t, err)
	eventTurns := 0
	for _, th := range threads {
		for _, q := range th.Turns {
			if strings.HasPrefix(q.Request.TurnID, "events_") {
				if th.Identity.Role != trace.Manager {
					t.Fatalf("%s received event turn %s", th.Identity.ID, q.Request.TurnID)
				}
				eventTurns++
			}
		}
	}
	if eventTurns != want {
		t.Fatalf("event turns: %d, want %d", eventTurns, want)
	}
}

func TestManagerRepliesAndWritesTheStatusTheOwnerReads(t *testing.T) {
	t.Parallel()
	f := newManagerFixture(t)
	f.start()
	f.askQuestions()
	c, id := f.c, f.id
	list, err := c.Conversation(f.ctx, fixturekit.Stream)
	fixturekit.Must(t, err)
	if got := intakefixture.States(list); !slices.Equal(got, []string{"message:done", "response:done"}) || list.Entries[0].Text != "Where do we stand?" || list.Entries[1].Text != managerDemoReply {
		t.Fatalf("conversation: %+v", list)
	}
	st, err := c.Status(f.ctx, fixturekit.Stream)
	fixturekit.Must(t, err)
	if st.Project != id || st.Status == nil {
		t.Fatalf("status: %+v", st)
	}
	written := *st.Status
	written.Revision, written.UpdatedAt = 0, time.Time{}
	if !reflect.DeepEqual(written, managerDemoStatus) || st.Status.Revision != 1 {
		t.Fatalf("status %+v, want %+v", st.Status, managerDemoStatus)
	}
	for _, text := range append([]string{written.Goal, written.Attention, written.Note}, written.Agents...) {
		for _, identifier := range []string{string(id), string(fixturekit.Stream), "agent_", "thread_", "session-", "message_", "events_", "fixture-image"} {
			if strings.Contains(text, identifier) {
				t.Fatalf("status field %q carries %q", text, identifier)
			}
		}
	}
	f.stop()

	f.mu.Lock()
	defer f.mu.Unlock()
	if st := f.results["status"]; len(st) != 2 || st[0] != `{"stored":false,"reason":"agents[0] contains an Osmia or backend identifier (\"`+fixturekit.DemoAgent+`\"); refer to the work or the agent in words"}` || st[1] != `{"stored":true,"revision":1}` {
		t.Fatalf("set_status results: %v", st)
	}
	if len(f.problems) != 0 {
		t.Fatalf("fake agents saw:\n%s", strings.Join(f.problems, "\n"))
	}
}

func TestAgentsThatAskParkTheirThreadsWhileTheOtherWorkContinues(t *testing.T) {
	t.Parallel()
	f := newManagerFixture(t)
	f.start()
	f.askQuestions()
	for _, agent := range []string{fixturekit.DemoAgent, "agent_reviewer"} {
		th := f.threadOf(agent)
		if !th.Parked() || len(th.Turns) != 1 {
			t.Fatalf("%s: parked %v, turns %+v", agent, th.Parked(), th.Turns)
		}
		if o := th.Turns[0].Response.Result.Outcome; o == nil || o.Status != "waiting" {
			t.Fatalf("%s asking turn outcome: %+v", agent, o)
		}
	}
	if q := f.question("1"); q.State != trace.QuestionOpen || q.Asked.Thread != fixturekit.DemoThread || q.Asked.Question != "Where does state live?" {
		t.Fatalf("question 1: %+v", q)
	}
	if q := f.question("2"); q.State != trace.QuestionOpen || q.Asked.Thread != "thread_reviewer" {
		t.Fatalf("question 2: %+v", q)
	}
	if th := f.threadOf("agent_mason2"); th.Parked() || len(th.Turns) != 1 || th.Turns[0].Status() != "idle" {
		t.Fatalf("second mason: %+v", th.Turns)
	}
	if got := f.runs(); !reflect.DeepEqual(got, map[string]int{"message": 1, "build": 1, "build2": 1, "review": 1}) {
		t.Fatalf("runs after asking: %v", got)
	}
	f.stop()
	f.mu.Lock()
	defer f.mu.Unlock()
	wantResults := map[string][]string{
		"build":  {`{"recorded":true,"question":"1","next":"End your turn now. The answer arrives as your next turn on this thread."}`},
		"review": {`{"recorded":true,"question":"2","next":"End your turn now. The answer arrives as your next turn on this thread."}`},
	}
	got := map[string][]string{"build": f.results["build"], "review": f.results["review"]}
	if !reflect.DeepEqual(got, wantResults) {
		t.Fatalf("ask results:\n%v\nwant\n%v", got, wantResults)
	}
	if len(f.problems) != 0 {
		t.Fatalf("fake agents saw:\n%s", strings.Join(f.problems, "\n"))
	}
}

func TestManagerAnswersOneQuestionAndEscalatesAnother(t *testing.T) {
	t.Parallel()
	f := newManagerFixture(t)
	f.start()
	f.askQuestions()
	f.answerAndEscalate()
	mason := f.threadOf(fixturekit.DemoAgent)
	if mason.Parked() || len(mason.Turns) != 2 || mason.Turns[1].Request.TurnID != "answer_1" || mason.Turns[1].Status() != "idle" {
		t.Fatalf("mason after the answer: parked %v, turns %+v", mason.Parked(), mason.Turns)
	}
	if q := f.question("1"); q.State != trace.QuestionAnswered || q.Ruling == nil || q.Ruling.Decision != trace.DecisionAnswer || !slices.Equal(q.Ruling.Citations, []string{"charter#1"}) {
		t.Fatalf("answered question: %+v", q)
	}
	if q := f.question("2"); q.State != trace.QuestionEscalated || !f.threadOf("agent_reviewer").Parked() {
		t.Fatalf("escalated question: %+v", q)
	}
	f.stop()

	repo := f.reopenedRepository()
	checkEventTurns(t, repo, 1)
	escalated := questionIn(t, repo, "2")
	want, _ := f.wantEscalation()
	if e := escalated.Latest.Escalation; e == nil || e.Batch != "escalation_2" || e.Recommendation != want.Recommendation || escalated.Latest.SentToOwner != want.Question {
		t.Fatalf("escalation: %+v", escalated.Latest)
	}
	f.committed("questions/2/question.jsonl", managerDemoQuestion, `"batch":"escalation_2"`)
	f.committed("questions/1/question.jsonl", "Where does state live?")
	f.committed("questions/1/rulings.jsonl", `"returned_answer":"`+managerDemoAnswer, `"citations":["charter#1"]`)

	f.mu.Lock()
	defer f.mu.Unlock()
	req := f.prompts["answer_1"]
	if prompt := questions.Prompt(trace.Question{Header: trace.Header{ID: "1"}, Question: "Where does state live?"}, trace.Ruling{Decision: trace.DecisionAnswer, ReturnedAnswer: managerDemoAnswer, Citations: []string{"charter#1"}}); req.ResumeID != "session-build" || !strings.Contains(req.Prompt, prompt) {
		t.Fatalf("answer_1 resumed %q with:\n%s", req.ResumeID, req.Prompt)
	}
	if d := f.results["decide"]; len(d) != 2 || !strings.Contains(d[0], `"decision"`) || !strings.Contains(d[1], `"decision"`) {
		t.Fatalf("decide results: %v", d)
	}
	manager := slices.Clone(f.results["chief"])
	if len(manager) > 0 {
		manager[0] = strings.Replace(manager[0], `"what":"queued mason turn"`, `"what":"the answer to question 1 is delivered as the asker's next turn"`, 1)
	}
	wantManager := []string{
		`{"next":"The answer is delivered to the asker as its next turn.","question":"1","queued":[{"role":"mason","what":"the answer to question 1 is delivered as the asker's next turn"}],"recorded":true}`,
		`{"recorded":true,"batch":"escalation_2","questions":["2"]}`,
	}
	if !reflect.DeepEqual(manager, wantManager) {
		t.Fatalf("chief tool results:\n%v\nwant\n%v", manager, wantManager)
	}
	if f.events["questions"] != 1 || f.events["ruling"] != 0 {
		t.Fatalf("manager event turns: %v", f.events)
	}
	if len(f.problems) != 0 {
		t.Fatalf("fake agents saw:\n%s", strings.Join(f.problems, "\n"))
	}
}

func TestEscalationStaysOneInboxEntryAndOneConversationActionAcrossARestart(t *testing.T) {
	t.Parallel()
	f := newManagerFixture(t)
	f.start()
	f.askQuestions()
	f.answerAndEscalate()
	want, wantAction := f.wantEscalation()
	inbox, err := f.c.Inbox(f.ctx)
	fixturekit.Must(t, err)
	if len(inbox.Entries) != 1 || !reflect.DeepEqual(inbox.Entries[0], want) {
		t.Fatalf("inbox:\n%+v\nwant\n%+v", inbox.Entries, want)
	}
	escalatedConversation, err := f.c.Conversation(f.ctx, fixturekit.Stream)
	fixturekit.Must(t, err)
	if got := intakefixture.States(escalatedConversation); !slices.Equal(got, []string{"message:done", "response:done", "action:done"}) ||
		!reflect.DeepEqual(escalatedConversation.Entries[2], wantAction) {
		t.Fatalf("conversation after escalation: %+v", escalatedConversation)
	}
	before := f.runs()
	f.stop()
	beforePass := f.passes.Load()
	f.start()
	f.await("escalation restart reconciliation", func() bool { return f.passes.Load() > beforePass && f.quiescent() })
	again, err := f.c.Inbox(f.ctx)
	fixturekit.Must(t, err)
	if !reflect.DeepEqual(again, inbox) {
		t.Fatalf("inbox after the restart:\n%+v\nwant\n%+v", again, inbox)
	}
	if got := f.runs(); !reflect.DeepEqual(got, before) {
		t.Fatalf("the restart ran %v, had run %v", got, before)
	}
	afterRestart, err := f.c.Conversation(f.ctx, fixturekit.Stream)
	fixturekit.Must(t, err)
	if !reflect.DeepEqual(afterRestart, escalatedConversation) {
		t.Fatalf("conversation duplicated or changed the escalation after restart: %+v", afterRestart)
	}
	f.checkProblems()
}

func TestOwnerConversationPromptCarriesTheStatusAndTheOpenEscalation(t *testing.T) {
	t.Parallel()
	f := newManagerFixture(t)
	f.start()
	f.askQuestions()
	f.answerAndEscalate()
	f.clock.Advance(time.Second)
	openMessage := f.send("What is waiting on me?")
	f.await("owner reply with the open escalation", func() bool { return f.ownerDone(openMessage) && f.quiescent() })
	f.mu.Lock()
	openPrompt := f.prompts[openMessage.Turn].Prompt
	f.mu.Unlock()
	for _, part := range []string{
		"Goal: " + managerDemoStatus.Goal,
		"Attention: None",
		"Note: " + managerDemoStatus.Note,
		"- " + managerDemoStatus.Agents[0],
		"Revision: 1",
		"Inbox: 1",
		"Batch: escalation_2",
		"Question for the owner: May the upload API's response change?",
		"- 2: " + managerDemoQuestion,
	} {
		if !strings.Contains(openPrompt, part) {
			t.Fatalf("owner conversation system prompt lacks %q:\n%s", part, openPrompt)
		}
	}
	f.checkProblems()
}

func (f *managerFixture) rule() (api.AnswerResponse, api.InboxEntry) {
	f.t.Helper()
	want, _ := f.wantEscalation()
	answered, err := f.c.Answer(f.ctx, want.Number, managerDemoRuling, "")
	fixturekit.Must(f.t, err)
	if !reflect.DeepEqual(answered, api.AnswerResponse{Number: 1, Workstream: fixturekit.Stream, Batch: "escalation_2", Questions: []string{"2"}, Ruling: managerDemoRuling, At: f.clock.Now()}) {
		f.t.Fatalf("answer: %+v", answered)
	}
	return answered, want
}

func (f *managerFixture) awaitRelay() {
	f.t.Helper()
	f.clock.Advance(time.Minute)
	f.await("owner ruling relayed to the reviewer", func() bool {
		return f.turnDone("agent_reviewer", "answer_2", "idle") && f.question("2").State == trace.QuestionAnswered && f.quiescent()
	})
}

func TestManagerQuestionsAndInbox(t *testing.T) {
	t.Parallel()
	f := newManagerFixture(t)
	f.start()
	f.askQuestions()
	f.answerAndEscalate()
	answered, want := f.rule()
	_, wantAction := f.wantEscalation()
	if inbox, err := f.c.Inbox(f.ctx); err != nil || len(inbox.Entries) != 0 {
		t.Fatalf("inbox after the ruling: %+v %v", inbox, err)
	}
	wantRuling := api.ConversationEntry{Turn: "ruling_" + want.Batch, Kind: "action", State: api.TurnDone,
		Text: "You ruled: " + managerDemoRuling, At: answered.At}
	ruledConversation, err := f.c.Conversation(f.ctx, fixturekit.Stream)
	fixturekit.Must(t, err)
	if len(ruledConversation.Entries) != 4 || !reflect.DeepEqual(ruledConversation.Entries[3], wantRuling) {
		t.Fatalf("conversation immediately after the owner answer: %+v; want ruling %+v", ruledConversation, wantRuling)
	}
	f.awaitRelay()
	reviewer := f.threadOf("agent_reviewer")
	if reviewer.Parked() || len(reviewer.Turns) != 2 || reviewer.Turns[1].Request.TurnID != "answer_2" || reviewer.Turns[1].Status() != "idle" {
		t.Fatalf("reviewer after the ruling: parked %v, turns %+v", reviewer.Parked(), reviewer.Turns)
	}
	list, err := f.c.Conversation(f.ctx, fixturekit.Stream)
	fixturekit.Must(t, err)
	if got := intakefixture.States(list); !slices.Equal(got, []string{"message:done", "response:done", "action:done", "action:done"}) ||
		!reflect.DeepEqual(list.Entries[2], wantAction) || !reflect.DeepEqual(list.Entries[3], wantRuling) {
		t.Fatalf("conversation after the ruling: %+v", list)
	}
	n := 0
	for _, entry := range list.Entries {
		if entry.Turn == wantRuling.Turn {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("owner ruling occurs %d times; want one: %+v", n, list)
	}
	f.stop()
	beforePass := f.passes.Load()
	f.start()
	f.await("ruling restart reconciliation", func() bool { return f.passes.Load() > beforePass && f.quiescent() })
	afterRulingRestart, err := f.c.Conversation(f.ctx, fixturekit.Stream)
	fixturekit.Must(t, err)
	if !reflect.DeepEqual(afterRulingRestart, list) {
		t.Fatalf("conversation duplicated or changed the escalation after ruling and restart: %+v", afterRulingRestart)
	}
	f.stop()

	if got := f.runs(); !reflect.DeepEqual(got, map[string]int{"message": 1, "build": 1, "build2": 1, "review": 1, "events": 2, "answer_1": 1, "answer_2": 1}) {
		t.Fatalf("runs: %v", got)
	}
	repo := f.reopenedRepository()
	checkEventTurns(t, repo, 2)
	ruled := questionIn(t, repo, "2")
	if ruled.State != trace.QuestionAnswered || ruled.Asked.Question != managerDemoQuestion || ruled.Asked.AskedBy != (trace.Actor{Kind: "agent", ID: "agent_reviewer"}) {
		t.Fatalf("ruled question: %+v", ruled)
	}
	if r := ruled.Ruling; r == nil || r.Decision != trace.DecisionRuling || r.OwnerResponse != managerDemoRuling || r.ReturnedAnswer != managerDemoRelayed || r.Scope != "notify" || r.Revision != 2 {
		t.Fatalf("ruling: %+v", r)
	}
	f.committed("questions/2/rulings.jsonl", `"owner_response":"`+managerDemoRuling, `"returned_answer":"`+managerDemoRelayed)

	f.mu.Lock()
	defer f.mu.Unlock()
	req := f.prompts["answer_2"]
	if prompt := questions.Prompt(trace.Question{Header: trace.Header{ID: "2"}, Question: managerDemoQuestion}, trace.Ruling{Decision: trace.DecisionRuling, OwnerResponse: managerDemoRuling, ReturnedAnswer: managerDemoRelayed}); req.ResumeID != "session-review" || !strings.Contains(req.Prompt, prompt) {
		t.Fatalf("answer_2 resumed %q with:\n%s", req.ResumeID, req.Prompt)
	}
	if manager := f.results["chief"]; len(manager) != 3 || manager[2] != `{"next":"The ruling is delivered to each asker as its next turn.","questions":["2"],"queued":[{"role":"reviewer","what":"the answer to question 2 is delivered as the asker's next turn"}],"recorded":true,"scope":"notify"}` {
		t.Fatalf("relay_ruling results: %v", manager)
	}
	if f.events["questions"] != 1 || f.events["ruling"] != 1 {
		t.Fatalf("manager event turns: %v", f.events)
	}
	if len(f.problems) != 0 {
		t.Fatalf("fake agents saw:\n%s", strings.Join(f.problems, "\n"))
	}
}

func TestOwnerRulingReachesTheManagersNextConversationAsANotice(t *testing.T) {
	t.Parallel()
	f := newManagerFixture(t)
	f.start()
	f.askQuestions()
	f.answerAndEscalate()
	f.rule()
	f.awaitRelay()
	message := f.send("Did the upload API change?")
	f.await("owner reply after the ruling", func() bool { return f.ownerDone(message) && f.quiescent() })
	list, err := f.c.Conversation(f.ctx, fixturekit.Stream)
	fixturekit.Must(t, err)
	var texts []string
	for _, entry := range list.Entries {
		if entry.Kind == "message" || entry.Kind == "response" {
			texts = append(texts, entry.Text)
		}
	}
	if want := []string{"Where do we stand?", managerDemoReply, "Did the upload API change?", "The upload API stays as it is."}; !slices.Equal(texts, want) {
		t.Fatalf("conversation messages and responses: %v; want %v", texts, want)
	}
	section, err := envelope.Render(envelope.Section{Name: "owner_response", Text: managerDemoRuling}, envelope.Section{Name: "returned_answer", Text: managerDemoRelayed})
	fixturekit.Must(t, err)
	notice := "## Notices\n- workstreams/" + string(fixturekit.Stream) + "/questions/2/rulings.jsonl (record 2 revision 2, workstream " + string(fixturekit.Stream) + ")\n" + section
	f.mu.Lock()
	defer f.mu.Unlock()
	if first := f.prompts[f.sent[0].Turn].Prompt; strings.Contains(first, "questions/2/rulings.jsonl") || !strings.Contains(first, "The owner edited charter.md") {
		t.Fatalf("a notice before the ruling:\n%s", first)
	}
	if later := f.prompts[f.sent[1].Turn].Prompt; !strings.Contains(later, notice) {
		t.Fatalf("the later bundle lacks the notice:\n%s", later)
	}
	if len(f.problems) != 0 {
		t.Fatalf("fake agents saw:\n%s", strings.Join(f.problems, "\n"))
	}
}
