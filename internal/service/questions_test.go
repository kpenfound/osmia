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

// A fake mason and a fake reviewer ask, a fake chief of staff answers one
// question with a citation and escalates the others as a batch, and the
// service restarts with open, answered-but-undelivered and escalated
// questions. Nothing is lost and nothing runs twice.
func TestQuestionsAreAnsweredOrEscalatedAcrossRestarts(t *testing.T) {
	ctx := context.Background()
	home, err := os.MkdirTemp("", "qa-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := fixtureAt(t, home)
	// One mason slot: the second mason runs only once the first has parked.
	global := filepath.Join(opts.Config.Root, "config.toml")
	data, err := os.ReadFile(global)
	must(t, err)
	must(t, os.WriteFile(global, append(data, []byte("[capacity]\nmasons = 1\n")...), 0600))
	cfg, err := config.Load(opts.Config)
	must(t, err)
	must(t, os.MkdirAll(cfg.Project.Clone, 0700))
	demoGit(t, home, "-C", cfg.Project.Clone, "init", "-q")

	clock := &fixedClock{now: demoStart}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, clock.Now(), owner)
	must(t, err)
	must(t, repo.CreateWorkstream(ctx, stream, clock.Now(), owner))
	traceDir, err := cfg.Root.ProjectTrace(project)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(traceDir, "charter.md"), []byte("# Charter\n\n1. Keep state in files under the root.\n"), 0600))
	agents := map[string]string{demoThread: demoAgent, "thread_mason2": "agent_mason2", "thread_reviewer": "agent_reviewer", trace.ChiefOfStaff: trace.ChiefOfStaff}
	for _, a := range []struct{ agent, thread, role, turn string }{
		{demoAgent, demoThread, "mason", "build"}, {"agent_mason2", "thread_mason2", "mason", "build2"}, {"agent_reviewer", "thread_reviewer", "reviewer", "review"},
	} {
		h := trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: a.agent, Project: project, Workstream: stream, At: clock.Now(), Actor: owner, Cause: "workstream_created"}
		must(t, repo.CreateThread(ctx, trace.Agent{Header: h, Role: a.role, ThreadID: a.thread}))
		h.Schema, h.ID, h.Cause, h.Depth = "osmia.trace.turn-request", "request_"+a.turn, "message_"+a.turn, 1
		_, err := repo.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: a.agent, ThreadID: a.thread, TurnID: a.turn,
			Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "You are the " + a.role + ".", Prompt: "Work: " + a.turn})
		must(t, err)
	}
	must(t, repo.Close())

	sessions := &demoSessions{byKey: map[string]*mcp.ClientSession{}}
	engine := &demoEngine{sessions: sessions, turns: map[string]demoTurn{}}
	engine.resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error {
		return coreadapter.ErrResumeUnavailable
	}
	views := filepath.Join(cfg.Root.String(), "views")
	must(t, os.Mkdir(views, 0700))
	lives := make(chan *trace.Repository, 1)
	// Every role is granted every question tool; the role decides what it sees.
	every := append([]string{"file_read", questions.AskTool}, questions.ChiefTools...)
	opts.Threads = func(r *trace.Repository) (coreadapter.Reconciler, error) {
		lives <- r
		turns := &isolation.Turns{Workspaces: &demoWorkspaces{directory: cfg.Project.Clone}, Views: isolation.Views{Directory: views}, Engine: engine,
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
	ticks := make(chan time.Time)
	opts.Reconciliation.Now, opts.Reconciliation.Ticks = clock.Now, ticks
	// A tick is received only between passes, so after several of them every
	// pass an earlier one caused has finished.
	settle := func(s *Service) {
		t.Helper()
		for range 5 {
			select {
			case ticks <- clock.Now():
			case <-time.After(demoTimeout):
				s.Close()
				t.Fatal("service did not finish its pass")
			}
		}
	}

	var mu sync.Mutex
	results := map[string][]string{}
	var problems []string
	problem := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	use := func(ctx context.Context, tools *mcp.ClientSession, step, name string, args map[string]any) error {
		out, err := callTool(ctx, tools, name, args)
		if err != nil {
			return err
		}
		results[step] = append(results[step], out)
		return nil
	}
	result := func(req agent.Request, id, text string) *agent.Result {
		return &agent.Result{ClaudeID: id, ResultText: text, SessionDir: req.SessionDir, NumTurns: 1}
	}
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
		if !slices.Equal(names, []string{"answer", "escalate", "file_read", "propose_charter", "route_amendment"}) {
			problem("chief of staff tools %v", names)
		}
		for _, part := range []string{"You are the chief of staff for workstream " + string(stream), questions.Guidance, "- charter#1: Keep state in files under the root."} {
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
	select {
	case ticks <- clock.Now():
	case <-time.After(demoTimeout):
		t.Fatal("service did not finish its pass")
	}
	select {
	case <-answered:
	case <-time.After(demoTimeout):
		t.Fatal("chief of staff did not answer")
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
	if len(problems) != 0 {
		t.Fatalf("fake agents saw:\n%s", strings.Join(problems, "\n"))
	}
}
