package intakejourney

import (
	context "context"
	reflect "reflect"
	slices "slices"
	strings "strings"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	agent "github.com/kpenfound/agentcore/agent"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	questions "github.com/kpenfound/osmia/internal/questions"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestQuestionsAreAnsweredOrEscalatedAcrossRestarts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := journey.NewQuestionFixture(t, "qa-", "[capacity]\nmasons = 1\n", "# Charter\n\n1. Keep state in files under the root.\n", []intakefixture.QuestionAsker{
		{Stream: fixturekit.Stream, Agent: fixturekit.DemoAgent, Thread: fixturekit.DemoThread, Role: "mason", Turn: "build"}, {Stream: fixturekit.Stream, Agent: "agent_mason2", Thread: "thread_mason2", Role: "mason", Turn: "build2"}, {Stream: fixturekit.Stream, Agent: "agent_reviewer", Thread: "thread_reviewer", Role: "reviewer", Turn: "review"},
	})
	opts, clock, engine, lives, ticks := f.Opts, f.Clock, f.Engine, f.Lives, f.Ticks

	opts.Reconciliation.Concurrent = func(coreadapter.Operation) bool { return false }
	settle := func(s *journey.Service) { t.Helper(); f.Settle(t, s) }
	mu, results, problem, use, result := &f.Mu, f.Results, f.Problem, f.Use, intakefixture.QuestionResult
	var repo *trace.Repository
	asker := func(session, question string, want ...string) fixturekit.DemoTurn {
		return func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
			mu.Lock()
			defer mu.Unlock()
			names, err := intakefixture.ToolNames(ctx, tools)
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
		names, err := intakefixture.ToolNames(ctx, tools)
		if err != nil {
			mu.Unlock()
			return nil, err
		}

		if !slices.Equal(names, []string{"answer", "escalate", "file_read", "log_assumption", "propose_charter", "relay_ruling", "route_amendment"}) {
			problem("chief of staff tools %v", names)
		}
		for _, part := range []string{"You are the chief of staff for workstream " + string(fixturekit.Stream), questions.Guidance} {
			if !strings.Contains(req.SystemPrompt, part) {
				problem("event turn system prompt lacks %q:\n%s", part, req.SystemPrompt)
			}
		}
		for _, part := range []string{"- charter#1 [Charter]: Keep state in files under the root.", "## Latest status", "## Open inbox escalations"} {
			if !strings.Contains(req.Prompt, part) || strings.Contains(req.SystemPrompt, part) {
				problem("event turn context lacks %q, or its system prompt carries it:\n%s", part, req.Prompt)
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
		list, err := r.Questions(fixturekit.Stream)
		fixturekit.Must(t, err)
		out := map[string]string{}
		for _, q := range list {
			out[q.Asked.ID] = q.State
		}
		return out
	}
	turnsOf := func(r *trace.Repository, agent string) []string {
		t.Helper()
		th, err := r.Thread(fixturekit.Stream, agent)
		fixturekit.Must(t, err)
		var out []string
		for _, q := range th.Turns {
			out = append(out, q.Request.TurnID)
		}
		return out
	}
	parked := func(r *trace.Repository, agent string) bool {
		t.Helper()
		th, err := r.Thread(fixturekit.Stream, agent)
		fixturekit.Must(t, err)
		return th.Parked()
	}
	questionEvents := func(r *trace.Repository) (total, unacknowledged int) {
		t.Helper()
		entries, err := r.Outbox(fixturekit.Stream)
		fixturekit.Must(t, err)
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

	s, err := journey.Start(ctx, opts)
	fixturekit.Must(t, err)
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
	if !parked(repo, fixturekit.DemoAgent) || !parked(repo, "agent_reviewer") || parked(repo, "agent_mason2") {
		t.Fatalf("parked: mason %v, reviewer %v, second mason %v", parked(repo, fixturekit.DemoAgent), parked(repo, "agent_reviewer"), parked(repo, "agent_mason2"))
	}
	th, err := repo.Thread(fixturekit.Stream, fixturekit.DemoAgent)
	fixturekit.Must(t, err)
	if o := th.Turns[0].Response.Result.Outcome; o == nil || *o != (coreadapter.Outcome{Status: "waiting", Report: "Asked question 1"}) {
		t.Fatalf("asking turn outcome: %+v", o)
	}
	if got := runs(); !reflect.DeepEqual(got, map[string]int{"build": 1, "build2": 1, "review": 1}) {
		t.Fatalf("first lifetime runs: %v", got)
	}
	if total, open := questionEvents(repo); total != 2 || open != 2 {
		t.Fatalf("question events: %d, %d undelivered", total, open)
	}
	fixturekit.Must(t, s.Close())

	s, err = journey.Start(ctx, opts)
	fixturekit.Must(t, err)
	repo = <-lives
	settle(s)
	if got := runs(); !reflect.DeepEqual(got, map[string]int{"build": 1, "build2": 1, "review": 1}) {
		t.Fatalf("restart with open questions ran: %v", got)
	}
	journey.InspectForTest(s).HoldAnswerDelivery()
	settle(s)
	clock.Advance(time.Minute)

	for waiting := true; waiting; {
		select {
		case ticks <- clock.Now():
		case <-answered:
			waiting = false
		case <-time.After(fixturekit.DemoTimeout):
			s.Close()
			t.Fatal("chief of staff did not answer")
		}
	}
	if got := turnsOf(repo, fixturekit.DemoAgent); !slices.Equal(got, []string{"build"}) {
		t.Fatalf("answer delivered inside the chief-of-staff turn: %v", got)
	}
	fixturekit.Must(t, s.Close())

	s, err = journey.Start(ctx, opts)
	fixturekit.Must(t, err)
	repo = <-lives
	settle(s)
	if got := turnsOf(repo, fixturekit.DemoAgent); !slices.Equal(got, []string{"build", "answer_1"}) {
		t.Fatalf("mason turns: %v", got)
	}
	th, err = repo.Thread(fixturekit.Stream, fixturekit.DemoAgent)
	fixturekit.Must(t, err)
	if req := th.Turns[1].Request; req.Prompt != answerPrompt || req.ThreadID != fixturekit.DemoThread || req.SystemPrompt != "You are the mason." || req.Profile.Name != "default" {
		t.Fatalf("answer turn: %+v", req)
	}
	mu.Lock()
	again := firstDeliveries
	mu.Unlock()
	if again != 2 {
		t.Fatalf("events of the interrupted turn delivered %d times in all", again)
	}
	if got := states(repo); !reflect.DeepEqual(got, map[string]string{"1": trace.QuestionAnswered, "2": trace.QuestionOpen, "3": trace.QuestionOpen}) || !parked(repo, fixturekit.DemoAgent) {
		t.Fatalf("questions after the answer: %v, mason parked %v", got, parked(repo, fixturekit.DemoAgent))
	}
	clock.Advance(time.Minute)
	settle(s)
	select {
	case <-escalated:
	default:
		t.Fatal("chief of staff did not escalate")
	}
	fixturekit.Must(t, s.Close())

	s, c := journey.StartT(t, opts)
	repo = <-lives
	clock.Advance(24 * time.Hour)
	settle(s)
	if got := states(repo); !reflect.DeepEqual(got, map[string]string{"1": trace.QuestionAnswered, "2": trace.QuestionEscalated, "3": trace.QuestionEscalated}) {
		t.Fatalf("final questions: %v", got)
	}
	if got := runs(); !reflect.DeepEqual(got, map[string]int{"build": 1, "build2": 1, "review": 1, "answer_1": 1, "events": 3}) {
		t.Fatalf("runs: %v", got)
	}
	if !parked(repo, fixturekit.DemoAgent) || !parked(repo, "agent_reviewer") {
		t.Fatal("an asker with an escalated question is not parked")
	}
	if got := turnsOf(repo, "agent_reviewer"); !slices.Equal(got, []string{"review"}) {
		t.Fatalf("reviewer turns: %v", got)
	}
	if total, open := questionEvents(repo); total != 3 || open != 0 {
		t.Fatalf("question events: %d, %d undelivered", total, open)
	}
	list, err := repo.Questions(fixturekit.Stream)
	fixturekit.Must(t, err)
	if r := list[0].Ruling; r == nil || r.ReturnedAnswer != "In files under the root." || !slices.Equal(r.Citations, []string{"charter#1"}) || r.Actor != (trace.Actor{Kind: "agent", ID: trace.ChiefOfStaff}) {
		t.Fatalf("ruling: %+v", r)
	}
	for _, q := range list[1:] {
		if e := q.Latest.Escalation; e == nil || e.Batch != "escalation_2" || !slices.Equal(e.Questions, []string{"2", "3"}) || q.Ruling != nil {
			t.Fatalf("escalation of question %s: %+v", q.Asked.ID, e)
		}
	}
	status, err := c.Status(ctx, fixturekit.Stream)
	fixturekit.Must(t, err)
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
	if len(f.Problems) != 0 {
		t.Fatalf("fake agents saw:\n%s", strings.Join(f.Problems, "\n"))
	}
}
