package service

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/envelope"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// The chief-of-staff demonstration: on an onboarded project, the owner
// talks to a workstream's chief of staff, fake workers ask questions, the
// chief of staff answers one and escalates another, and the owner's ruling
// reaches the asker across a service restart. See docs/chief-of-staff-demonstration.md.

// chiefDemoRoles runs every role of the demonstration in a container, the
// only sandbox the fake engine's boundary check accepts, with one mason slot.
const chiefDemoRoles = `[capacity]
masons = 1
[roles.chief_of_staff]
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
	chiefDemoCharter  = "# Charter\n\n1. Keep state in files under the root.\n"
	chiefDemoSpec     = "# Resumable uploads\n"
	chiefDemoReply    = "Two workers are building the upload work. I will tell you when they need a decision."
	chiefDemoAnswer   = "In files under the root."
	chiefDemoRuling   = "Keep the upload API as it is and add a new endpoint."
	chiefDemoRelayed  = "The upload API stays unchanged; add a new endpoint for resumable uploads."
	chiefDemoQuestion = "May I change the upload API's response?"
)

// chiefDemoStatus is the status the fake chief of staff writes, with no
// identifier in any field.
var chiefDemoStatus = StatusView{
	Goal:      "Ship resumable uploads.",
	Attention: "",
	Note:      "Two masons and a reviewer are at work on the upload unit.",
	Agents:    []string{"The first mason is building the upload unit.", "The second mason is building the index unit.", "The reviewer is reviewing the upload unit."},
}

// chiefDemoAgents maps each worker thread of the demonstration to its agent.
var chiefDemoAgents = map[string]string{demoThread: demoAgent, "thread_mason2": "agent_mason2", "thread_reviewer": "agent_reviewer"}

func TestChiefOfStaffQuestionsAndInbox(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// An onboarded project: added through the API, its knowledge base written
	// by a fake librarian and its charter written by the owner.
	lf := newLibrarianFixture(t)
	opts, engine, sessions := lf.opts, lf.engine, lf.sessions
	configFile, err := os.OpenFile(filepath.Join(opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = configFile.WriteString(chiefDemoRoles)
	must(t, errors.Join(err, configFile.Close()))
	// Every backend session can be resumed, so a thread's next turn names the
	// session of its previous one.
	engine.resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error { return nil }
	seed, err := kb.Seed(lf.clone)
	must(t, err)
	lf.script("extract-1-1", map[string]string{"output/kb/trace.md": "# trace\n\nState lives in files.\n", "output/kb/entities.json": entitiesWith(t, seed, "history")}, nil)

	// The clock stands still, so an event window closes only when the test
	// advances it.
	clock := &fixedClock{now: demoStart}
	opts.Reconciliation.Now = clock.Now
	lives := make(chan *trace.Repository, 1)
	opts = Enforce(opts, Enforcement{Engine: engine, Hosts: &coreadapter.MCPHost{Transport: &demoTransport{sessions: sessions}}})
	production := opts.Threads
	opts.Threads = func(r *trace.Repository, cfg *config.Config) (coreadapter.Reconciler, error) {
		bound, err := production(r, cfg)
		if err != nil {
			return nil, err
		}
		select {
		case <-lives:
		default:
		}
		lives <- r
		// The service grants thread turns to the chief of staff alone. The
		// fake workers are added here with their ask tool; the chief of staff
		// keeps the service's grant and tools.
		turns := bound.(thread.Dispatcher).Runner.Turns.(*questions.Turns).Turns.(*verdictTurns).Turns.(*reportingTurns).Turns.(*retriedTurns).Turns.(*isolation.Turns)
		turns.Grants = maps.Clone(turns.Grants)
		for _, role := range []string{"mason", "reviewer"} {
			turns.Grants[role] = coreadapter.Capabilities{Tools: []string{questions.AskTool}}
		}
		// The fake workers build no unit: each works in an empty directory of
		// its own, with no workspace to copy back to.
		turns.Workspaces, turns.Capture = stagedWorkspaces{}, nil
		selectChief := turns.Select
		turns.Select = func(ctx context.Context, scope coreadapter.Scope) (isolation.Selection, error) {
			if scope.Role == trace.ChiefOfStaff {
				return selectChief(ctx, scope)
			}
			workspace := filepath.Join(cfg.Root.String(), "workers", scope.Thread)
			role := cfg.Roles[trace.ChiefOfStaff]
			return isolation.Selection{Workspace: coreadapter.WorkspaceRequest{SourceDirectory: workspace, Directory: workspace},
				Execution: coreadapter.ExecutionSettings{Mode: role.Sandbox, Image: role.Image}}, os.MkdirAll(workspace, 0700)
		}
		chief := turns.Scoped
		turns.Scoped = func(ctx context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
			if scope.Role == trace.ChiefOfStaff {
				return chief(ctx, scope)
			}
			return questions.Tools(r, chiefDemoAgents[scope.Thread], scope, clock.Now)
		}
		return bound, nil
	}

	s, c := start(t, opts)
	added, err := c.AddProject(ctx, request(lf.clone))
	must(t, err)
	id := added.Project.ID
	if x := awaitExtraction(t, c); x.State != "succeeded" {
		t.Fatalf("extraction: %+v", x)
	}
	must(t, os.WriteFile(added.Project.Charter, []byte(chiefDemoCharter), 0600))
	onboarded, err := c.Configuration(ctx)
	must(t, err)
	if cs := onboarded.Project.CharterState; cs == nil || !cs.Ready || cs.Rules != 1 {
		t.Fatalf("charter: %+v", cs)
	}
	c.Close()
	must(t, s.Close())

	// The test fixture creates the workstream: two masons and a reviewer, each
	// with one queued turn.
	cfg, err := config.Load(opts.Config)
	must(t, err)
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	must(t, repo.CreateWorkstream(ctx, stream, clock.Now(), ownerActor))
	must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: plan.SpecDocument, Revision: 1, Project: id, Workstream: stream, At: clock.Now(), Actor: architectActor, Cause: "fixture", Depth: 1},
		Path: plan.SpecPath, Content: chiefDemoSpec}}))
	for _, w := range []struct{ agent, thread, role, turn string }{
		{demoAgent, demoThread, "mason", "build"}, {"agent_mason2", "thread_mason2", "mason", "build2"}, {"agent_reviewer", "thread_reviewer", "reviewer", "review"},
	} {
		h := trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: w.agent, Project: id, Workstream: stream, At: clock.Now(), Actor: ownerActor, Cause: "workstream_created"}
		must(t, repo.CreateThread(ctx, trace.Agent{Header: h, Role: w.role, ThreadID: w.thread}))
		h.Schema, h.ID, h.Cause, h.Depth = "osmia.trace.turn-request", "request_"+w.turn, "message_"+w.turn, 1
		_, err := repo.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: w.agent, ThreadID: w.thread, TurnID: w.turn,
			Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "You are the " + w.role + ".", Prompt: "Work: " + w.turn})
		must(t, err)
	}
	must(t, repo.Close())
	select {
	case <-lives:
	default:
	}

	// From here on a pass runs only when the test sends a tick.
	ticks := make(chan time.Time)
	opts.Reconciliation.Ticks = ticks
	var passes atomic.Uint64
	opts.schedulePassed = func() { passes.Add(1) }
	lifetime := func() (*Service, *Client, *trace.Repository) {
		t.Helper()
		s, err := Start(ctx, opts)
		must(t, err)
		c := NewClient(s.Socket())
		t.Cleanup(func() {
			c.Close()
			if err := s.Close(); err != nil {
				t.Errorf("service cleanup: %v", err)
			}
		})
		select {
		case r := <-lives:
			return s, c, r
		case <-s.done:
			t.Fatalf("service stopped before opening the trace: %v", s.Wait())
		case <-time.After(demoTimeout):
			t.Fatal("service did not open the trace")
		}
		return nil, nil, nil
	}
	stop := func(s *Service, c *Client) {
		t.Helper()
		c.Close()
		must(t, s.Close())
	}

	// The fake agents. Each records what it saw wrong under mu, and the test
	// fails on it at the end.
	var mu sync.Mutex
	var problems []string
	results := map[string][]string{}
	prompts := map[string]agent.Request{}
	problem := func(args ...any) { problems = append(problems, fmt.Sprintln(args...)) }
	use := func(ctx context.Context, tools *mcp.ClientSession, step, name string, args map[string]any) {
		out, err := callTool(ctx, tools, name, args)
		if err != nil {
			problem(step, err)
		}
		results[step] = append(results[step], out)
	}
	tools := func(ctx context.Context, name string, session *mcp.ClientSession, want ...string) {
		if got, err := toolNames(ctx, session); err != nil || !slices.Equal(got, want) {
			problem("tools of", name, got, err)
		}
	}
	var current *trace.Repository
	worker := func(question string) demoTurn {
		return func(ctx context.Context, req agent.Request, _ *agent.Turn, session *mcp.ClientSession) (*agent.Result, error) {
			mu.Lock()
			defer mu.Unlock()
			prompts[req.Name] = req
			tools(ctx, req.Name, session, questions.AskTool)
			if question != "" {
				use(ctx, session, req.Name, questions.AskTool, map[string]any{"question": question})
				return questionResult(req, "session-"+req.Name, "Asked"), nil
			}
			// The second mason runs while the first waits for its answer.
			th, err := current.Thread(stream, demoAgent)
			if err != nil || !th.Parked() {
				problem("second mason ran before the first parked", err)
			}
			return questionResult(req, "session-"+req.Name, "Built"), nil
		}
	}
	engine.turns["build"] = worker("Where does state live?")
	engine.turns["build2"] = worker("")
	masonAsked := make(chan struct{})
	var masonAskedOnce sync.Once
	releaseReviewer := func() { masonAskedOnce.Do(func() { close(masonAsked) }) }
	t.Cleanup(releaseReviewer)
	reviewerTurn := worker(chiefDemoQuestion)
	engine.turns["review"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, session *mcp.ClientSession) (*agent.Result, error) {
		select {
		case <-masonAsked:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return reviewerTurn(ctx, req, turn, session)
	}
	resumed := func(ctx context.Context, req agent.Request, _ *agent.Turn, session *mcp.ClientSession) (*agent.Result, error) {
		mu.Lock()
		defer mu.Unlock()
		prompts[req.Name] = req
		tools(ctx, req.Name, session, questions.AskTool)
		return questionResult(req, "session-"+req.Name, "Continued"), nil
	}
	var sent []ConversationEntry
	events := map[string]int{}
	engine.turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, session *mcp.ClientSession) (*agent.Result, error) {
		mu.Lock()
		defer mu.Unlock()
		prompts[req.Name] = req
		tools(ctx, req.Name, session, "answer", "capacity", "decide_amendment", "decide_change", "decide_charter", "escalate", "factory_context", "file_read", "hand_back_drift", "inspect_code", "log_assumption", "move_unit", "move_workstream", "notify", "pause", "prioritise", "project_context", "propose_charter", "read_remote_file", "record_discovery", "relay_ruling", "request_change", "resolve_contested", "resume", "route_amendment", "set_status")
		switch {
		case len(sent) > 0 && req.Name == sent[0].Turn:
			// The chief of staff reads the workstream's documents in its view.
			if listed, err := readTool(ctx, session, "."); err != nil || listed != plan.SpecPath {
				problem("chief of staff view lists", listed, err)
			}
			if spec, err := readTool(ctx, session, plan.SpecPath); err != nil || spec != chiefDemoSpec {
				problem("chief of staff reads spec", spec, err)
			}
			// A status naming an agent by its ID is refused; one in words is
			// stored.
			refused := map[string]any{"goal": chiefDemoStatus.Goal, "note": chiefDemoStatus.Note, "agents": []string{demoAgent + " is building."}}
			use(ctx, session, "status", "set_status", refused)
			use(ctx, session, "status", "set_status", map[string]any{"goal": chiefDemoStatus.Goal, "attention": chiefDemoStatus.Attention, "note": chiefDemoStatus.Note, "agents": chiefDemoStatus.Agents})
			return questionResult(req, "session-chief", chiefDemoReply), nil
		case len(sent) > 1 && req.Name != sent[0].Turn && !strings.HasPrefix(req.Name, "events_"):
			return questionResult(req, "session-chief", "The upload API stays as it is."), nil
		case !strings.HasPrefix(req.Name, "events_"):
		// Prompts replay nothing here: a resumed session receives only the new
		// events, so each case matches one event turn.
		case strings.Contains(req.Prompt, "asked by the mason: Where does state live?"):
			events["questions"]++
			if !strings.Contains(req.Prompt, "asked by the reviewer: "+chiefDemoQuestion) {
				problem("the questions were not delivered together", req.Prompt)
			}
			use(ctx, session, "chief", "answer", map[string]any{"question": "1", "text": chiefDemoAnswer, "citations": []string{"charter#1"}})
			use(ctx, session, "chief", "escalate", map[string]any{"questions": []string{"2"}, "rephrasing": "May the upload API's response change?",
				"blocked": "The reviewer's review of the upload unit.", "options": []string{"Change it", "Keep it and add an endpoint"}, "recommendation": "Keep it and add an endpoint."})
			return questionResult(req, "session-chief", "Answered one, escalated one"), nil
		case strings.Contains(req.Prompt, ", escalation_2 (questions 2): "+chiefDemoRuling):
			events["ruling"]++
			use(ctx, session, "chief", "relay_ruling", map[string]any{"question": "2", "text": chiefDemoRelayed, "scope": "notify"})
			return questionResult(req, "session-chief", "Relayed"), nil
		}
		problem("unexpected chief-of-staff turn", req.Name, req.Prompt)
		return questionResult(req, "session-chief", "Nothing to do"), nil
	}
	engine.turns["answer_1"], engine.turns["answer_2"] = resumed, resumed
	runs := func() map[string]int {
		engine.mu.Lock()
		defer engine.mu.Unlock()
		out := map[string]int{}
		for _, name := range engine.runs {
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
	question := func(r *trace.Repository, id string) trace.QuestionState {
		t.Helper()
		list, err := r.Questions(stream)
		must(t, err)
		for _, q := range list {
			if q.Asked.ID == id {
				return q
			}
		}
		t.Fatalf("no question %s in %+v", id, list)
		return trace.QuestionState{}
	}
	threadOf := func(r *trace.Repository, agent string) trace.Thread {
		t.Helper()
		th, err := r.Thread(stream, agent)
		must(t, err)
		return th
	}
	turnDone := func(r *trace.Repository, agent, turn, status string) bool {
		th := threadOf(r, agent)
		for _, q := range th.Turns {
			if q.Request.TurnID == turn {
				if q.Response == nil || q.CompletedAt.IsZero() {
					return false
				}
				if q.Response.RequestID != q.Request.ID || q.Response.RequestRevision != q.Request.Revision ||
					q.Response.ThreadID != q.Request.ThreadID || q.Response.TurnID != q.Request.TurnID {
					t.Fatalf("turn %s response lost its accepted request link: request=%+v response=%+v", turn, q.Request, q.Response)
				}
				return q.Status() == status
			}
		}
		return false
	}
	ownerDone := func(r *trace.Repository, entry ConversationEntry) bool {
		chief, err := r.ChiefOfStaffThread(stream)
		must(t, err)
		return turnDone(r, chief.Identity.ID, entry.Turn, "idle")
	}
	quiescent := func(r *trace.Repository) bool {
		threads, err := r.Threads(stream)
		must(t, err)
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
		operations, err := r.Operations(stream)
		must(t, err)
		for _, op := range operations {
			if op.Operation.Action == thread.TurnAction && !op.Acknowledged {
				return false
			}
		}
		return true
	}
	await := func(s *Service, r *trace.Repository, phase string, ready func() bool) {
		t.Helper()
		sub, ok := s.hub.subscribe()
		if !ok {
			t.Fatalf("%s: cannot subscribe to stopped service: %v", phase, s.Wait())
		}
		defer s.hub.unsubscribe(sub)
		deadline := time.NewTimer(demoTimeout)
		defer deadline.Stop()
		poll := time.NewTicker(time.Second)
		defer poll.Stop()
		diagnose := func(reason string) {
			t.Helper()
			threads, err := r.Threads(stream)
			t.Logf("PHASE %s project=%s workstream=%s reason=%s now=%s passes=%d threads_error=%v", phase, id, stream, reason, clock.Now(), passes.Load(), err)
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
			operations, err := r.Operations(stream)
			t.Logf("operations_error=%v", err)
			for _, op := range operations {
				if op.Operation.Action != thread.TurnAction {
					continue
				}
				in, decodeErr := thread.DecodeTurn(op.Operation)
				observed, inspectErr := (thread.Dispatcher{Runner: thread.Runner{Store: r}}).Inspect(ctx, op.Operation)
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
			case <-s.done:
				t.Logf("service stopped: %v", s.Wait())
			default:
				t.Logf("service active lifetime_error=%v", s.lifetime.Err())
			}
			t.Fatalf("chief demonstration did not complete %s: %s", phase, reason)
		}
		for {
			select {
			case <-s.done:
				diagnose("service stopped")
			default:
			}
			if ready() {
				return
			}
			select {
			case ticks <- clock.Now():
			case <-sub.wake:
				sub.take()
			case <-poll.C:
			case <-s.done:
				diagnose("service stopped")
			case <-deadline.C:
				diagnose("deadline reached")
			}
		}
	}

	// 1. The owner sends a message; the chief of staff replies and writes the
	// workstream's status.
	s, c, repo = lifetime()
	mu.Lock()
	current = repo
	message, err := c.Send(ctx, stream, "Where do we stand?")
	sent = append(sent, message)
	mu.Unlock()
	must(t, err)
	if message.State != TurnQueued {
		t.Fatalf("sent: %+v", message)
	}
	await(s, repo, "owner reply and both questions with the second mason completed", func() bool {
		// The reviewer asks only after the mason owns the original question.
		asked, err := repo.Questions(stream)
		must(t, err)
		for _, q := range asked {
			if q.Asked.ID == "1" && q.Asked.Thread == demoThread && q.Asked.Question == "Where does state live?" {
				releaseReviewer()
			}
		}
		if !ownerDone(repo, message) || !turnDone(repo, demoAgent, "build", "waiting") ||
			!turnDone(repo, "agent_reviewer", "review", "waiting") || !turnDone(repo, "agent_mason2", "build2", "idle") {
			return false
		}
		return threadOf(repo, demoAgent).Parked() && threadOf(repo, "agent_reviewer").Parked() &&
			question(repo, "1").State == trace.QuestionOpen && question(repo, "2").State == trace.QuestionOpen && quiescent(repo)
	})
	list, err := c.Conversation(ctx, stream)
	must(t, err)
	if got := states(list); !slices.Equal(got, []string{"message:done", "response:done"}) || list.Entries[0].Text != "Where do we stand?" || list.Entries[1].Text != chiefDemoReply {
		t.Fatalf("conversation: %+v", list)
	}
	st, err := c.Status(ctx, stream)
	must(t, err)
	if st.Project != id || st.Status == nil {
		t.Fatalf("status: %+v", st)
	}
	written := *st.Status
	written.Revision, written.UpdatedAt = 0, time.Time{}
	if !reflect.DeepEqual(written, chiefDemoStatus) || st.Status.Revision != 1 {
		t.Fatalf("status %+v, want %+v", st.Status, chiefDemoStatus)
	}
	for _, text := range append([]string{written.Goal, written.Attention, written.Note}, written.Agents...) {
		for _, identifier := range []string{string(id), string(stream), "agent_", "thread_", "session-", "message_", "events_", "fixture-image"} {
			if strings.Contains(text, identifier) {
				t.Fatalf("status field %q carries %q", text, identifier)
			}
		}
	}

	// 2. The first mason and the reviewer ask and park. The mason's slot goes
	// to the queued second mason in the meantime.
	for _, agent := range []string{demoAgent, "agent_reviewer"} {
		th := threadOf(repo, agent)
		if !th.Parked() || len(th.Turns) != 1 {
			t.Fatalf("%s: parked %v, turns %+v", agent, th.Parked(), th.Turns)
		}
		if o := th.Turns[0].Response.Result.Outcome; o == nil || o.Status != "waiting" {
			t.Fatalf("%s asking turn outcome: %+v", agent, o)
		}
	}
	if q := question(repo, "1"); q.State != trace.QuestionOpen || q.Asked.Thread != demoThread || q.Asked.Question != "Where does state live?" {
		t.Fatalf("question 1: %+v", q)
	}
	if q := question(repo, "2"); q.State != trace.QuestionOpen || q.Asked.Thread != "thread_reviewer" {
		t.Fatalf("question 2: %+v", q)
	}
	if th := threadOf(repo, "agent_mason2"); th.Parked() || len(th.Turns) != 1 || th.Turns[0].Status() != "idle" {
		t.Fatalf("second mason: %+v", th.Turns)
	}
	if got := runs(); !reflect.DeepEqual(got, map[string]int{"extract-1-1": 1, "message": 1, "build": 1, "build2": 1, "review": 1}) {
		t.Fatalf("runs after asking: %v", got)
	}

	// 3. Once the event window closes, the chief of staff answers the
	// mason's question with a citation and escalates the reviewer's. The
	// mason resumes on its own thread.
	clock.Advance(time.Minute)
	await(s, repo, "cited answer and escalation", func() bool {
		return turnDone(repo, demoAgent, "answer_1", "idle") && question(repo, "1").State == trace.QuestionAnswered &&
			question(repo, "2").State == trace.QuestionEscalated && threadOf(repo, "agent_reviewer").Parked() && quiescent(repo)
	})
	mason := threadOf(repo, demoAgent)
	if mason.Parked() || len(mason.Turns) != 2 || mason.Turns[1].Request.TurnID != "answer_1" || mason.Turns[1].Status() != "idle" {
		t.Fatalf("mason after the answer: parked %v, turns %+v", mason.Parked(), mason.Turns)
	}
	if q := question(repo, "1"); q.State != trace.QuestionAnswered || q.Ruling == nil || q.Ruling.Decision != trace.DecisionAnswer || !slices.Equal(q.Ruling.Citations, []string{"charter#1"}) {
		t.Fatalf("answered question: %+v", q)
	}
	if q := question(repo, "2"); q.State != trace.QuestionEscalated || !threadOf(repo, "agent_reviewer").Parked() {
		t.Fatalf("escalated question: %+v", q)
	}

	// 4. The escalation is in the inbox, and still there, once, after a
	// restart.
	inbox, err := c.Inbox(ctx)
	must(t, err)
	want := InboxEntry{Kind: InboxEscalation, Project: repo.Project(), Number: 1, Workstream: stream, Batch: "escalation_2", Question: "May the upload API's response change?", Blocked: "The reviewer's review of the upload unit.",
		Options: []string{"Change it", "Keep it and add an endpoint"}, Recommendation: "Keep it and add an endpoint.", QuickReply: "Keep it and add an endpoint.", OpenedAt: clock.Now(), Answer: InboxAnswer{Method: "POST", Path: "/v1/inbox/1", Body: map[string]any{"project": string(repo.Project())}},
		Asked: []InboxQuestion{{ID: "2", AskedBy: "agent_reviewer", Question: chiefDemoQuestion}}}
	if len(inbox.Entries) != 1 || !reflect.DeepEqual(inbox.Entries[0], want) {
		t.Fatalf("inbox:\n%+v\nwant\n%+v", inbox.Entries, want)
	}
	wantAction := ConversationEntry{Turn: want.Batch, Kind: "action", State: TurnDone, At: want.OpenedAt,
		Text: "Question for you: " + want.Question + "\nBlocked: " + want.Blocked +
			"\nOptions: " + strings.Join(want.Options, "; ") + "\nRecommendation: " + want.Recommendation}
	escalatedConversation, err := c.Conversation(ctx, stream)
	must(t, err)
	if got := states(escalatedConversation); !slices.Equal(got, []string{"message:done", "response:done", "action:done"}) ||
		!reflect.DeepEqual(escalatedConversation.Entries[2], wantAction) {
		t.Fatalf("conversation after escalation: %+v", escalatedConversation)
	}
	before := runs()
	stop(s, c)
	beforePass := passes.Load()
	s, c, repo = lifetime()
	mu.Lock()
	current = repo
	mu.Unlock()
	await(s, repo, "escalation restart reconciliation", func() bool { return passes.Load() > beforePass && quiescent(repo) })
	again, err := c.Inbox(ctx)
	must(t, err)
	if !reflect.DeepEqual(again, inbox) {
		t.Fatalf("inbox after the restart:\n%+v\nwant\n%+v", again, inbox)
	}
	if got := runs(); !reflect.DeepEqual(got, before) {
		t.Fatalf("the restart ran %v, had run %v", got, before)
	}
	afterRestart, err := c.Conversation(ctx, stream)
	must(t, err)
	if !reflect.DeepEqual(afterRestart, escalatedConversation) {
		t.Fatalf("conversation duplicated or changed the escalation after restart: %+v", afterRestart)
	}
	clock.Advance(time.Second)
	mu.Lock()
	openMessage, err := c.Send(ctx, stream, "What is waiting on me?")
	sent = append(sent, openMessage)
	mu.Unlock()
	must(t, err)
	await(s, repo, "owner reply with the open escalation", func() bool { return ownerDone(repo, openMessage) && quiescent(repo) })
	mu.Lock()
	openPrompt := prompts[openMessage.Turn].SystemPrompt
	mu.Unlock()
	for _, part := range []string{
		"Goal: " + chiefDemoStatus.Goal,
		"Attention: None",
		"Note: " + chiefDemoStatus.Note,
		"- " + chiefDemoStatus.Agents[0],
		"Revision: 1",
		"Inbox: 1",
		"Batch: escalation_2",
		"Question for the owner: May the upload API's response change?",
		"- 2: " + chiefDemoQuestion,
	} {
		if !strings.Contains(openPrompt, part) {
			t.Fatalf("owner conversation system prompt lacks %q:\n%s", part, openPrompt)
		}
	}

	// 5. The owner rules. The chief of staff rephrases the ruling as a
	// project notice, the reviewer resumes with it, and the next message's
	// context carries the notice.
	answered, err := c.Answer(ctx, want.Number, chiefDemoRuling, "")
	must(t, err)
	if !reflect.DeepEqual(answered, AnswerResponse{Number: 1, Workstream: stream, Batch: "escalation_2", Questions: []string{"2"}, Ruling: chiefDemoRuling, At: clock.Now()}) {
		t.Fatalf("answer: %+v", answered)
	}
	if inbox, err := c.Inbox(ctx); err != nil || len(inbox.Entries) != 0 {
		t.Fatalf("inbox after the ruling: %+v %v", inbox, err)
	}
	wantRuling := ConversationEntry{Turn: "ruling_" + want.Batch, Kind: "action", State: TurnDone,
		Text: "You ruled: " + chiefDemoRuling, At: answered.At}
	ruledConversation, err := c.Conversation(ctx, stream)
	must(t, err)
	if len(ruledConversation.Entries) != len(afterRestart.Entries)+3 ||
		!reflect.DeepEqual(ruledConversation.Entries[5], wantRuling) {
		t.Fatalf("conversation immediately after the owner answer: %+v; want ruling %+v", ruledConversation, wantRuling)
	}
	clock.Advance(time.Minute)
	await(s, repo, "owner ruling relayed to the reviewer", func() bool {
		return turnDone(repo, "agent_reviewer", "answer_2", "idle") && question(repo, "2").State == trace.QuestionAnswered && quiescent(repo)
	})
	reviewer := threadOf(repo, "agent_reviewer")
	if reviewer.Parked() || len(reviewer.Turns) != 2 || reviewer.Turns[1].Request.TurnID != "answer_2" || reviewer.Turns[1].Status() != "idle" {
		t.Fatalf("reviewer after the ruling: parked %v, turns %+v", reviewer.Parked(), reviewer.Turns)
	}
	mu.Lock()
	message, err = c.Send(ctx, stream, "Did the upload API change?")
	sent = append(sent, message)
	mu.Unlock()
	must(t, err)
	await(s, repo, "owner reply after the ruling", func() bool { return ownerDone(repo, message) && quiescent(repo) })
	list, err = c.Conversation(ctx, stream)
	must(t, err)
	if got := states(list); !slices.Equal(got, []string{"message:done", "response:done", "action:done", "message:done", "response:done", "action:done", "message:done", "response:done"}) ||
		!reflect.DeepEqual(list.Entries[2], wantAction) || !reflect.DeepEqual(list.Entries[5], wantRuling) ||
		!reflect.DeepEqual(list.Entries[:6], ruledConversation.Entries) {
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
	wantTexts := []string{"Where do we stand?", chiefDemoReply, "What is waiting on me?", "The upload API stays as it is.", "Did the upload API change?", "The upload API stays as it is."}
	var texts []string
	for _, entry := range list.Entries {
		if entry.Kind == "message" || entry.Kind == "response" {
			texts = append(texts, entry.Text)
		}
	}
	if !slices.Equal(texts, wantTexts) {
		t.Fatalf("conversation messages and responses: %v; want %v", texts, wantTexts)
	}
	stop(s, c)
	beforePass = passes.Load()
	s, c, repo = lifetime()
	await(s, repo, "ruling restart reconciliation", func() bool { return passes.Load() > beforePass && quiescent(repo) })
	afterRulingRestart, err := c.Conversation(ctx, stream)
	must(t, err)
	if !reflect.DeepEqual(afterRulingRestart, list) {
		t.Fatalf("conversation duplicated or changed the escalation after ruling and restart: %+v", afterRulingRestart)
	}
	stop(s, c)

	// 6. Only the chief of staff received event turns, and the trace holds
	// the question, the choice, the ruling and what was sent back.
	if got := runs(); !reflect.DeepEqual(got, map[string]int{"extract-1-1": 1, "message": 3, "build": 1, "build2": 1, "review": 1, "events": 2, "answer_1": 1, "answer_2": 1}) {
		t.Fatalf("runs: %v", got)
	}
	repo, err = trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	defer repo.Close()
	threads, err := repo.Threads(stream)
	must(t, err)
	eventTurns := 0
	for _, th := range threads {
		for _, q := range th.Turns {
			if strings.HasPrefix(q.Request.TurnID, "events_") {
				if th.Identity.Role != trace.ChiefOfStaff {
					t.Fatalf("%s received event turn %s", th.Identity.ID, q.Request.TurnID)
				}
				eventTurns++
			}
		}
	}
	if eventTurns != 2 {
		t.Fatalf("event turns: %d", eventTurns)
	}
	ruled := question(repo, "2")
	if ruled.State != trace.QuestionAnswered || ruled.Asked.Question != chiefDemoQuestion || ruled.Asked.AskedBy != (trace.Actor{Kind: "agent", ID: "agent_reviewer"}) {
		t.Fatalf("ruled question: %+v", ruled)
	}
	if e := ruled.Latest.Escalation; e == nil || e.Batch != "escalation_2" || e.Recommendation != want.Recommendation || ruled.Latest.SentToOwner != want.Question {
		t.Fatalf("escalation: %+v", ruled.Latest)
	}
	if r := ruled.Ruling; r == nil || r.Decision != trace.DecisionRuling || r.OwnerResponse != chiefDemoRuling || r.ReturnedAnswer != chiefDemoRelayed || r.Scope != "notify" || r.Revision != 2 {
		t.Fatalf("ruling: %+v", r)
	}
	streamTrace, err := cfg.Root.WorkstreamTrace(id, stream)
	must(t, err)
	for path, wants := range map[string][]string{
		"workstreams/" + string(stream) + "/questions/2/question.jsonl": {chiefDemoQuestion, `"batch":"escalation_2"`},
		"workstreams/" + string(stream) + "/questions/2/rulings.jsonl":  {`"owner_response":"` + chiefDemoRuling, `"returned_answer":"` + chiefDemoRelayed},
		"workstreams/" + string(stream) + "/questions/1/question.jsonl": {"Where does state live?"},
		"workstreams/" + string(stream) + "/questions/1/rulings.jsonl":  {`"returned_answer":"` + chiefDemoAnswer, `"citations":["charter#1"]`},
	} {
		committed := demoGit(t, "", "-C", streamTrace, "cat-file", "blob", "HEAD:"+path)
		for _, w := range wants {
			if !strings.Contains(committed, w) {
				t.Fatalf("committed %s lacks %q:\n%s", path, w, committed)
			}
		}
	}

	mu.Lock()
	defer mu.Unlock()
	// Each asker resumed its own backend session with the answer.
	for turn, w := range map[string]struct{ session, prompt string }{
		"answer_1": {"session-build", questions.Prompt(trace.Question{Header: trace.Header{ID: "1"}, Question: "Where does state live?"}, trace.Ruling{Decision: trace.DecisionAnswer, ReturnedAnswer: chiefDemoAnswer, Citations: []string{"charter#1"}})},
		"answer_2": {"session-review", questions.Prompt(trace.Question{Header: trace.Header{ID: "2"}, Question: chiefDemoQuestion}, trace.Ruling{Decision: trace.DecisionRuling, OwnerResponse: chiefDemoRuling, ReturnedAnswer: chiefDemoRelayed})},
	} {
		if req := prompts[turn]; req.ResumeID != w.session || !strings.Contains(req.Prompt, w.prompt) {
			t.Fatalf("%s resumed %q with:\n%s", turn, req.ResumeID, req.Prompt)
		}
	}
	section, err := envelope.Render(envelope.Section{Name: "owner_response", Text: chiefDemoRuling}, envelope.Section{Name: "returned_answer", Text: chiefDemoRelayed})
	if err != nil {
		t.Fatal(err)
	}
	notice := "## Notices\n- workstreams/" + string(stream) + "/questions/2/rulings.jsonl (record 2 revision 2, workstream " + string(stream) + ")\n" + section
	if first := prompts[sent[0].Turn].SystemPrompt; strings.Contains(first, "questions/2/rulings.jsonl") || !strings.Contains(first, "The owner edited charter.md") {
		t.Fatalf("a notice before the ruling:\n%s", first)
	}
	if later := prompts[sent[2].Turn].SystemPrompt; !strings.Contains(later, notice) {
		t.Fatalf("the later bundle lacks the notice:\n%s", later)
	}
	wantResults := map[string][]string{
		"build":  {`{"recorded":true,"question":"1","next":"End your turn now. The answer arrives as your next turn on this thread."}`},
		"review": {`{"recorded":true,"question":"2","next":"End your turn now. The answer arrives as your next turn on this thread."}`},
		"chief": {
			`{"recorded":true,"question":"1","next":"The answer is delivered to the asker as its next turn."}`,
			`{"recorded":true,"batch":"escalation_2","questions":["2"]}`,
			`{"recorded":true,"questions":["2"],"scope":"notify","next":"The ruling is delivered to each asker as its next turn."}`,
		},
	}
	if st := results["status"]; len(st) != 2 || st[0] != `{"stored":false,"reason":"agents[0] contains an Osmia or backend identifier (\"`+demoAgent+`\"); refer to the work or the agent in words"}` || st[1] != `{"stored":true,"revision":1}` {
		t.Fatalf("set_status results: %v", st)
	}
	delete(results, "status")
	if !reflect.DeepEqual(results, wantResults) {
		t.Fatalf("tool results:\n%v\nwant\n%v", results, wantResults)
	}
	if events["questions"] != 1 || events["ruling"] != 1 {
		t.Fatalf("chief-of-staff event turns: %v", events)
	}
	if len(problems) != 0 {
		t.Fatalf("fake agents saw:\n%s", strings.Join(problems, "\n"))
	}
}
