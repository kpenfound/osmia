package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// toolNames lists the tools a fake agent's session offers, sorted.
func toolNames(ctx context.Context, tools *mcp.ClientSession) ([]string, error) {
	listed, err := tools.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	return names, nil
}

// questionAsker is one agent of a question fixture with its first queued turn.
type questionAsker struct {
	stream                    config.WorkstreamID
	agent, thread, role, turn string
}

// questionFixture is a project whose askers and chiefs of staff are fake
// agents holding the real question tools over MCP. The clock stands still, so
// the event window closes only when a test advances it. Fake turns lock mu
// around results and problems.
type questionFixture struct {
	opts     Options
	cfg      *config.Config
	clock    *fixedClock
	engine   *demoEngine
	lives    chan *trace.Repository
	ticks    chan time.Time
	mu       sync.Mutex
	results  map[string][]string
	problems []string
}

// newQuestionFixture creates the trace with the charter, each asker's
// workstream and thread, and one queued turn per asker. global is appended to
// the root's config.toml.
func newQuestionFixture(t *testing.T, prefix, global, charter string, askers []questionAsker) *questionFixture {
	t.Helper()
	ctx := context.Background()
	home, err := os.MkdirTemp("", prefix)
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	f := &questionFixture{opts: fixtureAt(t, home), clock: &fixedClock{now: demoStart}, lives: make(chan *trace.Repository, 1), ticks: make(chan time.Time), results: map[string][]string{}}
	path := filepath.Join(f.opts.Config.Root, "config.toml")
	data, err := os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(path, append(data, []byte(global)...), 0600))
	f.cfg, err = config.Load(f.opts.Config)
	must(t, err)
	cfg, clock := f.cfg, f.clock
	must(t, os.MkdirAll(cfg.Project.Clone, 0700))
	demoGit(t, home, "-C", cfg.Project.Clone, "init", "-q")

	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, clock.Now(), owner)
	must(t, err)
	traceDir, err := cfg.Root.ProjectTrace(project)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(traceDir, "charter.md"), []byte(charter), 0600))
	agents := map[string]string{trace.ChiefOfStaff: trace.ChiefOfStaff}
	created := map[config.WorkstreamID]bool{}
	for _, a := range askers {
		if !created[a.stream] {
			must(t, repo.CreateWorkstream(ctx, a.stream, clock.Now(), owner))
			created[a.stream] = true
		}
		agents[a.thread] = a.agent
		h := trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: a.agent, Project: project, Workstream: a.stream, At: clock.Now(), Actor: owner, Cause: "workstream_created"}
		must(t, repo.CreateThread(ctx, trace.Agent{Header: h, Role: a.role, ThreadID: a.thread}))
		h.Schema, h.ID, h.Cause, h.Depth = "osmia.trace.turn-request", "request_"+a.turn, "message_"+a.turn, 1
		_, err := repo.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: a.agent, ThreadID: a.thread, TurnID: a.turn,
			Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "You are the " + a.role + ".", Prompt: "Work: " + a.turn})
		must(t, err)
	}
	must(t, repo.Close())

	sessions := &demoSessions{byKey: map[string]*mcp.ClientSession{}}
	f.engine = &demoEngine{sessions: sessions, turns: map[string]demoTurn{}}
	f.engine.resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error {
		return coreadapter.ErrResumeUnavailable
	}
	views := filepath.Join(cfg.Root.String(), "views")
	must(t, os.Mkdir(views, 0700))
	// Every role is granted every question tool; the role decides what it sees.
	every := append([]string{"file_read", questions.AskTool}, questions.ChiefTools...)
	f.opts.Threads = func(r *trace.Repository) (coreadapter.Reconciler, error) {
		f.lives <- r
		turns := &isolation.Turns{Workspaces: &demoWorkspaces{directory: cfg.Project.Clone}, Views: isolation.Views{Directory: views}, Engine: f.engine,
			Grants: map[string]coreadapter.Capabilities{"mason": {Tools: every}, "reviewer": {Tools: every}, trace.ChiefOfStaff: {Tools: every}},
			Select: func(context.Context, coreadapter.Scope) (isolation.Selection, error) {
				return isolation.Selection{Execution: coreadapter.ExecutionSettings{Mode: "container", Image: "fixture-image"}}, nil
			},
			Scoped: func(_ context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
				return questions.Tools(r, agents[scope.Thread], scope, clock.Now)
			},
			Hosts: func(token string) coreadapter.MCPHosts {
				return &coreadapter.MCPHost{Transport: &demoTransport{token: token, sessions: sessions}}
			},
		}
		return thread.Dispatcher{Runner: thread.Runner{Store: r, Turns: &questions.Turns{Turns: turns, Repository: r}, Now: clock.Now},
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				directory := filepath.Join(cfg.Root.String(), "sessions", in.Agent, in.Turn)
				return coreadapter.PreparedTurn{SessionDirectory: directory}, os.MkdirAll(directory, 0700)
			}}, nil
	}
	f.opts.Reconciliation.Now, f.opts.Reconciliation.Ticks = clock.Now, f.ticks
	return f
}

// settle sends several ticks. A tick is received only between passes, so
// afterwards every pass an earlier one caused has finished.
func (f *questionFixture) settle(t *testing.T, s *Service) {
	t.Helper()
	for range 5 {
		select {
		case f.ticks <- f.clock.Now():
		case <-time.After(demoTimeout):
			s.Close()
			t.Fatal("service did not finish its pass")
		}
	}
}

// problem notes what a fake agent saw wrong; the test fails on them at its end.
func (f *questionFixture) problem(format string, args ...any) {
	f.problems = append(f.problems, fmt.Sprintf(format, args...))
}

// use calls a tool and keeps its result under step.
func (f *questionFixture) use(ctx context.Context, tools *mcp.ClientSession, step, name string, args map[string]any) error {
	out, err := callTool(ctx, tools, name, args)
	if err != nil {
		return err
	}
	f.results[step] = append(f.results[step], out)
	return nil
}

func questionResult(req agent.Request, id, text string) *agent.Result {
	return &agent.Result{ClaudeID: id, ResultText: text, SessionDir: req.SessionDir, NumTurns: 1}
}

// A fake mason and a fake reviewer ask, a fake chief of staff answers one
// question with a citation and escalates the others as a batch, and the
// service restarts with open, answered-but-undelivered and escalated
// questions. Nothing is lost and nothing runs twice.
func TestQuestionsAreAnsweredOrEscalatedAcrossRestarts(t *testing.T) {
	ctx := context.Background()
	f := newQuestionFixture(t, "qa-", "[capacity]\nmasons = 1\n", "# Charter\n\n1. Keep state in files under the root.\n", []questionAsker{
		{stream, demoAgent, demoThread, "mason", "build"}, {stream, "agent_mason2", "thread_mason2", "mason", "build2"}, {stream, "agent_reviewer", "thread_reviewer", "reviewer", "review"},
	})
	opts, clock, engine, lives, ticks := f.opts, f.clock, f.engine, f.lives, f.ticks
	settle := func(s *Service) { t.Helper(); f.settle(t, s) }
	mu, results, problem, use, result := &f.mu, f.results, f.problem, f.use, questionResult
	var repo *trace.Repository
	asker := func(session, question string, want ...string) demoTurn {
		return func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
			mu.Lock()
			defer mu.Unlock()
			names, err := toolNames(ctx, tools)
			if err != nil {
				return nil, err
			}
			if !slices.Equal(names, []string{"ask", "file_read"}) {
				problem("%s tools %v", req.Name, names)
			}
			for _, part := range want {
				if !strings.Contains(req.Prompt, part) {
					problem("%s prompt lacks %q:\n%s", req.Name, part, req.Prompt)
				}
			}
			if question == "" {
				return result(req, session, "Built"), nil
			}
			if err := use(ctx, tools, req.Name, "ask", map[string]any{"question": question}); err != nil {
				return nil, err
			}
			return result(req, session, "Asked"), nil
		}
	}
	engine.turns["build"] = asker("session-mason", "Where does state live?")
	engine.turns["build2"] = asker("session-mason2", "")
	engine.turns["review"] = asker("session-reviewer", "Is the log format fixed?")
	answerPrompt := "Answer to your question 1.\n\nYou asked:\nWhere does state live?\n\nAnswer:\nIn files under the root.\n\nCitations:\n- charter#1\n"
	// The answer arrives on the thread that asked, after its earlier turn.
	engine.turns["answer_1"] = asker("session-mason", "Which file holds the index?", answerPrompt, "Work: build")

	answered, escalated := make(chan struct{}), make(chan struct{})
	engine.turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		mu.Lock()
		names, err := toolNames(ctx, tools)
		if err != nil {
			mu.Unlock()
			return nil, err
		}
		if !slices.Equal(names, []string{"answer", "escalate", "file_read", "propose_charter", "relay_ruling", "route_amendment"}) {
			problem("chief of staff tools %v", names)
		}
		for _, part := range []string{"You are the chief of staff for workstream " + string(stream), questions.Guidance, "- charter#1 [Charter]: Keep state in files under the root."} {
			if !strings.Contains(req.SystemPrompt, part) {
				problem("event turn system prompt lacks %q:\n%s", part, req.SystemPrompt)
			}
		}
		switch {
		case strings.Contains(req.Prompt, "Question 1 is open, asked by the mason: Where does state live?"):
			if !strings.Contains(req.Prompt, "Question 2 is open, asked by the reviewer: Is the log format fixed?") {
				problem("questions were not delivered together:\n%s", req.Prompt)
			}
			for _, call := range []struct {
				tool string
				args map[string]any
			}{
				{"answer", map[string]any{"question": "1", "text": "In files under the root.", "citations": []string{}}},
				{"answer", map[string]any{"question": "1", "text": "In files under the root.", "citations": []string{"charter#7"}}},
				{"route_amendment", map[string]any{"question": "1"}},
				{"propose_charter", map[string]any{"question": "1"}},
				{"answer", map[string]any{"question": "1", "text": "In files under the root.", "citations": []string{"charter#1"}}},
				{"answer", map[string]any{"question": "1", "text": "In a database.", "citations": []string{"charter#1"}}},
			} {
				if err := use(ctx, tools, "chief-answers", call.tool, call.args); err != nil {
					mu.Unlock()
					return nil, err
				}
			}
			mu.Unlock()
			// The service stops before any pass can deliver the answer.
			close(answered)
			<-ctx.Done()
			return nil, ctx.Err()
		case strings.Contains(req.Prompt, "Question 3 is open, asked by the mason: Which file holds the index?"):
			defer mu.Unlock()
			for _, call := range []struct {
				tool string
				args map[string]any
			}{
				{"escalate", map[string]any{"questions": []string{"2", "3"}, "rephrasing": "Are the log format and the index file part of the contract?", "blocked": "The upload unit and its review.", "options": []string{"Both fixed", "Both free"}, "recommendation": "Both fixed."}},
				{"answer", map[string]any{"question": "2", "text": "Fixed.", "citations": []string{"charter#1"}}},
				{"escalate", map[string]any{"questions": []string{"3"}, "rephrasing": "Again?", "blocked": "The same.", "options": []string{}, "recommendation": "No."}},
			} {
				if err := use(ctx, tools, "chief-escalates", call.tool, call.args); err != nil {
					return nil, err
				}
			}
			close(escalated)
			return result(req, "session-chief", "Escalated"), nil
		}
		mu.Unlock()
		problem("unexpected chief-of-staff turn:\n%s", req.Prompt)
		return result(req, "session-chief", "Nothing to do"), nil
	}

	states := func(r *trace.Repository) map[string]string {
		t.Helper()
		list, err := r.Questions(stream)
		must(t, err)
		out := map[string]string{}
		for _, q := range list {
			out[q.Asked.ID] = q.State
		}
		return out
	}
	turnsOf := func(r *trace.Repository, agent string) []string {
		t.Helper()
		th, err := r.Thread(stream, agent)
		must(t, err)
		var out []string
		for _, q := range th.Turns {
			out = append(out, q.Request.TurnID)
		}
		return out
	}
	parked := func(r *trace.Repository, agent string) bool {
		t.Helper()
		th, err := r.Thread(stream, agent)
		must(t, err)
		return th.Parked()
	}
	questionEvents := func(r *trace.Repository) (total, unacknowledged int) {
		t.Helper()
		entries, err := r.Outbox(stream)
		must(t, err)
		for _, e := range entries {
			if strings.HasPrefix(e.Event.Body, "Question ") {
				total++
				if !e.Acknowledged {
					unacknowledged++
				}
			}
		}
		return total, unacknowledged
	}
	runs := func() map[string]int {
		engine.mu.Lock()
		defer engine.mu.Unlock()
		out := map[string]int{}
		for _, name := range engine.runs {
			if strings.HasPrefix(name, "events_") {
				name = "events"
			}
			out[name]++
		}
		return out
	}

	// First lifetime: both askers ask and park, and the parked mason's slot
	// goes to the second mason. The clock stands still, so the event window
	// stays open and the chief of staff hears nothing.
	s, err := Start(ctx, opts)
	must(t, err)
	repo = <-lives
	settle(s)
	if got := states(repo); !reflect.DeepEqual(got, map[string]string{"1": trace.QuestionOpen, "2": trace.QuestionOpen}) {
		t.Fatalf("questions after asking: %v", got)
	}
	if !parked(repo, demoAgent) || !parked(repo, "agent_reviewer") || parked(repo, "agent_mason2") {
		t.Fatalf("parked: mason %v, reviewer %v, second mason %v", parked(repo, demoAgent), parked(repo, "agent_reviewer"), parked(repo, "agent_mason2"))
	}
	th, err := repo.Thread(stream, demoAgent)
	must(t, err)
	if o := th.Turns[0].Response.Result.Outcome; o == nil || *o != (coreadapter.Outcome{Status: "waiting", Report: "Asked question 1"}) {
		t.Fatalf("asking turn outcome: %+v", o)
	}
	if got := runs(); !reflect.DeepEqual(got, map[string]int{"build": 1, "build2": 1, "review": 1}) {
		t.Fatalf("first lifetime runs: %v", got)
	}
	if total, open := questionEvents(repo); total != 2 || open != 2 {
		t.Fatalf("question events: %d, %d undelivered", total, open)
	}
	must(t, s.Close())

	// Second lifetime, with open questions: nothing is asked again. Once the
	// window closes the chief of staff answers question 1, and the service
	// stops before the answer is delivered.
	s, err = Start(ctx, opts)
	must(t, err)
	repo = <-lives
	settle(s)
	if got := runs(); !reflect.DeepEqual(got, map[string]int{"build": 1, "build2": 1, "review": 1}) {
		t.Fatalf("restart with open questions ran: %v", got)
	}
	clock.Advance(time.Minute)
	// The pass that sees the closed window blocks inside the chief-of-staff
	// turn, so it may never take another tick.
	for waiting := true; waiting; {
		select {
		case ticks <- clock.Now():
		case <-answered:
			waiting = false
		case <-time.After(demoTimeout):
			s.Close()
			t.Fatal("chief of staff did not answer")
		}
	}
	if got := turnsOf(repo, demoAgent); !slices.Equal(got, []string{"build"}) {
		t.Fatalf("answer delivered inside the chief-of-staff turn: %v", got)
	}
	must(t, s.Close())

	// Third lifetime, with an answered but undelivered question: the answer
	// reaches the mason's thread once, the mason asks again, and the chief of
	// staff escalates the two open questions together.
	s, err = Start(ctx, opts)
	must(t, err)
	repo = <-lives
	settle(s)
	if got := turnsOf(repo, demoAgent); !slices.Equal(got, []string{"build", "answer_1"}) {
		t.Fatalf("mason turns: %v", got)
	}
	th, err = repo.Thread(stream, demoAgent)
	must(t, err)
	if req := th.Turns[1].Request; req.Prompt != answerPrompt || req.ThreadID != demoThread || req.SystemPrompt != "You are the mason." || req.Profile.Name != "default" {
		t.Fatalf("answer turn: %+v", req)
	}
	if got := states(repo); !reflect.DeepEqual(got, map[string]string{"1": trace.QuestionAnswered, "2": trace.QuestionOpen, "3": trace.QuestionOpen}) || !parked(repo, demoAgent) {
		t.Fatalf("questions after the answer: %v, mason parked %v", got, parked(repo, demoAgent))
	}
	clock.Advance(time.Minute)
	settle(s)
	select {
	case <-escalated:
	default:
		t.Fatal("chief of staff did not escalate")
	}
	must(t, s.Close())

	// Fourth lifetime, with escalated questions: they wait for the owner.
	// Nothing runs, however much time passes.
	s, c := start(t, opts)
	repo = <-lives
	clock.Advance(24 * time.Hour)
	settle(s)
	if got := states(repo); !reflect.DeepEqual(got, map[string]string{"1": trace.QuestionAnswered, "2": trace.QuestionEscalated, "3": trace.QuestionEscalated}) {
		t.Fatalf("final questions: %v", got)
	}
	if got := runs(); !reflect.DeepEqual(got, map[string]int{"build": 1, "build2": 1, "review": 1, "answer_1": 1, "events": 2}) {
		t.Fatalf("runs: %v", got)
	}
	if !parked(repo, demoAgent) || !parked(repo, "agent_reviewer") {
		t.Fatal("an asker with an escalated question is not parked")
	}
	if got := turnsOf(repo, "agent_reviewer"); !slices.Equal(got, []string{"review"}) {
		t.Fatalf("reviewer turns: %v", got)
	}
	if total, open := questionEvents(repo); total != 3 || open != 0 {
		t.Fatalf("question events: %d, %d undelivered", total, open)
	}
	list, err := repo.Questions(stream)
	must(t, err)
	if r := list[0].Ruling; r == nil || r.ReturnedAnswer != "In files under the root." || !slices.Equal(r.Citations, []string{"charter#1"}) || r.Actor != (trace.Actor{Kind: "agent", ID: trace.ChiefOfStaff}) {
		t.Fatalf("ruling: %+v", r)
	}
	for _, q := range list[1:] {
		if e := q.Latest.Escalation; e == nil || e.Batch != "escalation_2" || !slices.Equal(e.Questions, []string{"2", "3"}) || q.Ruling != nil {
			t.Fatalf("escalation of question %s: %+v", q.Asked.ID, e)
		}
	}
	status, err := c.Status(ctx, stream)
	must(t, err)
	if status.OpenQuestions != 2 {
		t.Fatalf("open questions in status: %d", status.OpenQuestions)
	}

	mu.Lock()
	defer mu.Unlock()
	reserved := `{"recorded":false,"reason":"reserved until amendments and standing rulings (M4)"}`
	want := map[string][]string{
		"build":    {`{"recorded":true,"question":"1","next":"End your turn now. The answer arrives as your next turn on this thread."}`},
		"review":   {`{"recorded":true,"question":"2","next":"End your turn now. The answer arrives as your next turn on this thread."}`},
		"answer_1": {`{"recorded":true,"question":"3","next":"End your turn now. The answer arrives as your next turn on this thread."}`},
		"chief-answers": {
			`{"recorded":false,"reason":"an answer needs at least one citation; escalate a question the record does not settle"}`,
			`{"recorded":false,"reason":"citation \"charter#7\" does not resolve: the charter has no rule numbered 7 exactly once"}`,
			reserved, reserved,
			`{"recorded":true,"question":"1","next":"The answer is delivered to the asker as its next turn."}`,
			`{"recorded":false,"reason":"question 1 is already answered"}`,
		},
		"chief-escalates": {
			`{"recorded":true,"batch":"escalation_2","questions":["2","3"]}`,
			`{"recorded":false,"reason":"question 2 is escalated to the owner; only the owner's ruling answers it"}`,
			`{"recorded":false,"reason":"question 3 is escalated to the owner; only the owner's ruling answers it"}`,
		},
	}
	if !reflect.DeepEqual(results, want) {
		t.Fatalf("tool results:\n%v\nwant:\n%v", results, want)
	}
	if len(f.problems) != 0 {
		t.Fatalf("fake agents saw:\n%s", strings.Join(f.problems, "\n"))
	}
}

// claimTurn queues and claims a turn of the agent in an offline trace and
// returns the turn's scope, so a test can call the question writes directly.
func claimTurn(t *testing.T, repo *trace.Repository, home string, ws config.WorkstreamID, agent, thread, turn string, at time.Time) coreadapter.Scope {
	t.Helper()
	ctx := context.Background()
	h := trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_" + turn, Project: project, Workstream: ws, At: at, Actor: trace.Actor{Kind: "owner", ID: "local"}, Cause: "message_" + turn}
	_, err := repo.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: agent, ThreadID: thread, TurnID: turn, Profile: coreadapter.Profile{Name: "other", Backend: "codex", Model: "other"}, Prompt: "Work"})
	must(t, err)
	_, err = repo.ClaimTurn(ctx, ws, agent, "token_"+turn, filepath.Join(home, turn), at)
	must(t, err)
	th, err := repo.Thread(ws, agent)
	must(t, err)
	return coreadapter.Scope{Project: string(project), Workstream: string(ws), Thread: thread, Turn: turn, Role: th.Identity.Role}
}

// An answer recorded in an abandoned workstream stays undelivered, while
// another workstream's answer is queued with its asker's role profile.
func TestAnswersAreNotDeliveredToAbandonedWorkstreams(t *testing.T) {
	ctx := context.Background()
	home, err := os.MkdirTemp("", "qb-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := fixtureAt(t, home)
	cfg, err := config.Load(opts.Config)
	must(t, err)
	clock := &fixedClock{now: demoStart}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, clock.Now(), owner)
	must(t, err)
	defer repo.Close()
	traceDir, err := cfg.Root.ProjectTrace(project)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(traceDir, "charter.md"), []byte("# Charter\n\n1. Keep state in files.\n"), 0600))
	for _, ws := range []config.WorkstreamID{stream, quiet} {
		must(t, repo.CreateWorkstream(ctx, ws, clock.Now(), owner))
		identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: demoAgent, Project: project, Workstream: ws, At: clock.Now(), Actor: owner, Cause: "workstream_created"}, Role: demoRole, ThreadID: demoThread}
		must(t, repo.CreateThread(ctx, identity))
		_, err := repo.Ask(ctx, demoAgent, claimTurn(t, repo, home, ws, demoAgent, demoThread, "build", clock.Now()), "Where does state live?", clock.Now())
		must(t, err)
		_, err = repo.AnswerQuestion(ctx, trace.ChiefOfStaff, claimTurn(t, repo, home, ws, trace.ChiefOfStaff, trace.ChiefOfStaff, "events", clock.Now()), "1", "In files.", []string{"charter#1"}, clock.Now())
		must(t, err)
	}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: abandonTransition, Revision: 1, Project: project, Workstream: stream, At: clock.Now(), Actor: owner, Cause: abandonTransition}
	_, err = repo.SetFeatureState(ctx, h, AbandonedState, "gone")
	must(t, err)

	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: []config.WorkstreamID{stream, quiet}})
	must(t, err)
	defer store.Close()
	s := &Service{options: Options{Reconciliation: reconcile.Options{Now: clock.Now}}, store: store}
	must(t, s.answers(cfg, repo).Pass(ctx))
	abandonedThread, err := repo.Thread(stream, demoAgent)
	must(t, err)
	live, err := repo.Thread(quiet, demoAgent)
	must(t, err)
	if len(abandonedThread.Turns) != 1 || len(live.Turns) != 2 {
		t.Fatalf("abandoned workstream has %d turns, live one %d", len(abandonedThread.Turns), len(live.Turns))
	}
	// The answer runs on the mason's bound profile, not the asking turn's.
	if req := live.Turns[1].Request; req.TurnID != "answer_1" || req.Profile.Name != "default" || req.Profile.Backend != "claude" || req.Profile.Model != "test" || !req.At.Equal(clock.Now()) {
		t.Fatalf("answer turn: %+v", req)
	}
}
