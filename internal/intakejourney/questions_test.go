package intakejourney

import (
	context "context"
	fmt "fmt"
	reflect "reflect"
	slices "slices"
	strings "strings"
	sync "sync"
	testing "testing"
	time "time"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	questions "github.com/kpenfound/osmia/internal/questions"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

var toolNames = intakefixture.ToolNames

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
	f := &questionFixture{results: map[string][]string{}}
	var inputs []intakefixture.QuestionAsker
	for _, a := range askers {
		inputs = append(inputs, intakefixture.QuestionAsker{Stream: a.stream, Agent: a.agent, Thread: a.thread, Role: a.role, Turn: a.turn})
	}
	data := intakefixture.NewQuestionSetup(t, prefix, global, charter, inputs, func(t *testing.T, home string) config.Options { f.opts = fixtureAt(t, home); return f.opts.Config })
	f.cfg, f.clock, f.engine, f.lives, f.ticks = data.Config, data.Clock, data.Engine, data.Lives, data.Ticks
	f.opts.Threads, f.opts.Reconciliation.Now, f.opts.Reconciliation.Ticks = data.Threads, data.Clock.Now, data.Ticks
	return f
}

// settle sends several ticks. A tick is received only between passes, so
// afterwards every pass an earlier one caused has finished.
func (f *questionFixture) settle(t *testing.T, s *Service) {
	t.Helper()
	intakefixture.SettleTicks(t, f.ticks, f.clock.Now, s.Close)
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

var questionResult = intakefixture.QuestionResult

// A fake mason and a fake reviewer ask, a fake chief of staff answers one
// question with a citation and escalates the others as a batch, and the
// service restarts with open, answered-but-undelivered and escalated
// questions. Nothing is lost and nothing runs twice.
func TestQuestionsAreAnsweredOrEscalatedAcrossRestarts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newQuestionFixture(t, "qa-", "[capacity]\nmasons = 1\n", "# Charter\n\n1. Keep state in files under the root.\n", []questionAsker{
		{stream, demoAgent, demoThread, "mason", "build"}, {stream, "agent_mason2", "thread_mason2", "mason", "build2"}, {stream, "agent_reviewer", "thread_reviewer", "reviewer", "review"},
	})
	opts, clock, engine, lives, ticks := f.opts, f.clock, f.engine, f.lives, f.ticks

	// Keep worker operations in the pass so settle waits for the askers.
	opts.Reconciliation.Concurrent = func(coreadapter.Operation) bool { return false }
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
	engine.Turns["build"] = asker("session-mason", "Where does state live?")
	engine.Turns["build2"] = asker("session-mason2", "")
	engine.Turns["review"] = asker("session-reviewer", "Is the log format fixed?")
	answerPrompt := questions.Prompt(trace.Question{Header: trace.Header{ID: "1"}, Question: "Where does state live?"}, trace.Ruling{Decision: trace.DecisionAnswer, ReturnedAnswer: "In files under the root.", Citations: []string{"charter#1"}})

	engine.Turns["answer_1"] = asker("session-mason", "Which file holds the index?", answerPrompt, "Work: build")

	answered, escalated := make(chan struct{}), make(chan struct{})

	firstDeliveries := 0
	engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		mu.Lock()
		names, err := toolNames(ctx, tools)
		if err != nil {
			mu.Unlock()
			return nil, err
		}

		if !slices.Equal(names, []string{"answer", "escalate", "file_read", "log_assumption", "propose_charter", "relay_ruling", "route_amendment"}) {
			problem("chief of staff tools %v", names)
		}
		for _, part := range []string{"You are the chief of staff for workstream " + string(stream), questions.Guidance, "- charter#1 [Charter]: Keep state in files under the root.", "## Latest status", "## Open inbox escalations"} {
			if !strings.Contains(req.SystemPrompt, part) {
				problem("event turn system prompt lacks %q:\n%s", part, req.SystemPrompt)
			}
		}
		switch {

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
		case strings.Contains(req.Prompt, "Question 1 is open, asked by the mason: Where does state live?"):
			if !strings.Contains(req.Prompt, "Question 2 is open, asked by the reviewer: Is the log format fixed?") {
				problem("questions were not delivered together:\n%s", req.Prompt)
			}
			firstDeliveries++
			if firstDeliveries > 1 {

				defer mu.Unlock()
				if err := use(ctx, tools, "chief-redelivered", "answer", map[string]any{"question": "1", "text": "In files under the root.", "citations": []string{"charter#1"}}); err != nil {
					return nil, err
				}
				return result(req, "session-chief", "Already answered"), nil
			}
			for _, call := range []struct {
				tool string
				args map[string]any
			}{
				{"answer", map[string]any{"question": "1", "text": "In files under the root.", "citations": []string{}}},
				{"answer", map[string]any{"question": "1", "text": "In files under the root.", "citations": []string{"charter#7"}}},
				{"route_amendment", map[string]any{"question": "1", "citations": []string{}, "change": "Clarify the criterion", "reason": "The answer changes it"}},
				{"propose_charter", map[string]any{"question": "1", "rule": "State lives in files under the root."}},
				{"answer", map[string]any{"question": "1", "text": "In files under the root.", "citations": []string{"charter#1"}}},
				{"answer", map[string]any{"question": "1", "text": "In a database.", "citations": []string{"charter#1"}}},
			} {
				if err := use(ctx, tools, "chief-answers", call.tool, call.args); err != nil {
					mu.Unlock()
					return nil, err
				}
			}
			mu.Unlock()

			close(answered)
			<-ctx.Done()
			return nil, ctx.Err()
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
		engine.Mu.Lock()
		defer engine.Mu.Unlock()
		out := map[string]int{}
		for _, name := range engine.Runs {
			if strings.HasPrefix(name, "events_") {
				name = "events"
			}
			out[name]++
		}
		return out
	}

	s, err := Start(ctx, opts)
	must(t, err)
	defer func() {
		if s != nil {
			s.Close()
		}
	}()
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

	s, err = Start(ctx, opts)
	must(t, err)
	repo = <-lives
	settle(s)
	if got := runs(); !reflect.DeepEqual(got, map[string]int{"build": 1, "build2": 1, "review": 1}) {
		t.Fatalf("restart with open questions ran: %v", got)
	}
	// Hold delivery during this lifetime so the chief's durable answer stays
	// undelivered until restart, even as scheduling passes continue.
	InspectForTest(s).HoldAnswerDelivery()
	settle(s)
	clock.Advance(time.Minute)

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
	mu.Lock()
	again := firstDeliveries
	mu.Unlock()
	if again != 2 {
		t.Fatalf("events of the interrupted turn delivered %d times in all", again)
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

	s, c := start(t, opts)
	repo = <-lives
	clock.Advance(24 * time.Hour)
	settle(s)
	if got := states(repo); !reflect.DeepEqual(got, map[string]string{"1": trace.QuestionAnswered, "2": trace.QuestionEscalated, "3": trace.QuestionEscalated}) {
		t.Fatalf("final questions: %v", got)
	}
	if got := runs(); !reflect.DeepEqual(got, map[string]int{"build": 1, "build2": 1, "review": 1, "answer_1": 1, "events": 3}) {
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
	want := map[string][]string{
		"build":    {`{"recorded":true,"question":"1","next":"End your turn now. The answer arrives as your next turn on this thread."}`},
		"review":   {`{"recorded":true,"question":"2","next":"End your turn now. The answer arrives as your next turn on this thread."}`},
		"answer_1": {`{"recorded":true,"question":"3","next":"End your turn now. The answer arrives as your next turn on this thread."}`},
		"chief-answers": {
			`{"recorded":false,"reason":"an answer needs at least one citation; escalate a question the record does not settle"}`,
			`{"recorded":false,"reason":"citation \"charter#7\" does not resolve: the charter has no rule numbered 7 exactly once"}`,
			`{"recorded":false,"reason":"at least one spec#<n> or plan#<unit> citation is required"}`,
			`{"recorded":false,"reason":"question 1 has no ruling from the owner; only a ruling the owner gave can become a charter rule"}`,
			`{"recorded":true,"question":"1","next":"The answer is delivered to the asker as its next turn."}`,
			`{"recorded":false,"reason":"question 1 is already answered"}`,
		},
		"chief-redelivered": {`{"recorded":false,"reason":"question 1 is already answered"}`},
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

// An answer recorded in an abandoned workstream stays undelivered, while
// another workstream's answer is queued with its asker's role profile.

// An answer that arrives after its asker's unit merged stays undelivered: the
// unit's workspace is gone, so the turn could not run. The answer to the same
// question from a unit still in flight is queued on its asker's thread.
