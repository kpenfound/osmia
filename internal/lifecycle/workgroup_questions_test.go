package lifecycle_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestWorkgroupSharesOneManagerAcrossMembersAndARestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const second config.WorkstreamID = "w_00000000000000000000000000000f02"
	const question1, question2 = "Where does state live?", "Should the index rebuild on start?"
	const ownersRuling, relayedAnswer = "Keep the index fixed at start.", "The index does not rebuild on start."

	f := journey.NewQuestionFixture(t, "wg-", "[capacity]\nmasons = 2\n", "# Charter\n\n1. Keep state in files under the root.\n", []intakefixture.QuestionAsker{
		{Stream: fixturekit.Stream, Agent: fixturekit.DemoAgent, Thread: fixturekit.DemoThread, Role: "mason", Turn: "build"},
		{Stream: second, Agent: "agent_second", Thread: "thread_second", Role: "mason", Turn: "build2"},
	})
	opts, clock, engine, lives, ticks := f.Opts, f.Clock, f.Engine, f.Lives, f.Ticks

	grouping, err := trace.Open(f.Cfg.Root, f.Cfg.Project)
	fixturekit.Must(t, err)
	_, err = trace.CreateWorkgroup(ctx, grouping, []config.WorkstreamID{fixturekit.Stream, second}, clock.Now(), intake.OwnerActor, "test-workgroup")
	fixturekit.Must(t, err)
	fixturekit.Must(t, grouping.Close())

	var mu sync.Mutex
	var problems []string
	problem := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	ask := func(question string) fixturekit.DemoTurn {
		return func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
			if out, err := fixturekit.CallTool(ctx, tools, questions.AskTool, map[string]any{"question": question}); err != nil || !strings.Contains(out, `"recorded":true`) {
				problem("%s ask: %s %v", req.Name, out, err)
			}
			return intakefixture.QuestionResult(req, "session-"+req.Name, "Asked"), nil
		}
	}
	engine.Turns["build"] = ask(question1)
	engine.Turns["build2"] = ask(question2)

	var flagMu sync.Mutex
	var answered1, escalated2, relayed bool
	flags := func() (bool, bool, bool) {
		flagMu.Lock()
		defer flagMu.Unlock()
		return answered1, escalated2, relayed
	}
	engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		flagMu.Lock()
		needRelay := !relayed && strings.Contains(req.Prompt, ownersRuling)
		needEscalate := !escalated2 && strings.Contains(req.Prompt, "asked by the mason: "+question2)
		needAnswer := !answered1 && strings.Contains(req.Prompt, "asked by the mason: "+question1)
		flagMu.Unlock()

		acted := false
		if needRelay {
			out, err := fixturekit.CallTool(ctx, tools, questions.RelayRulingTool, map[string]any{"question": string(second) + ":1", "text": relayedAnswer, "scope": "local"})
			if err != nil || !strings.Contains(out, `"recorded":true`) {
				problem("relay: %s %v", out, err)
			}
			flagMu.Lock()
			relayed = true
			flagMu.Unlock()
			acted = true
		}
		if needEscalate {
			out, err := fixturekit.CallTool(ctx, tools, questions.EscalateTool, map[string]any{
				"questions": []string{string(second) + ":1"}, "rephrasing": "Should the index rebuild on start?",
				"blocked": "The second workstream's build.", "options": []string{"Rebuild", "Keep"}, "recommendation": "Keep.",
			})
			if err != nil || !strings.Contains(out, `"recorded":true`) {
				problem("escalate: %s %v", out, err)
			}
			flagMu.Lock()
			escalated2 = true
			flagMu.Unlock()
			acted = true
		}
		if needAnswer {
			out, err := fixturekit.CallTool(ctx, tools, questions.AnswerTool, map[string]any{"question": "1", "text": "In files under the root.", "citations": []string{"charter#1"}})
			if err != nil || !strings.Contains(out, `"recorded":true`) {
				problem("answer: %s %v", out, err)
			}
			flagMu.Lock()
			answered1 = true
			flagMu.Unlock()
			acted = true
		}
		if acted {
			return intakefixture.QuestionResult(req, "session-manager", "Handled"), nil
		}

		if strings.Contains(req.Prompt, "asked by the mason: "+question1) ||
			strings.Contains(req.Prompt, "asked by the mason: "+question2) ||
			strings.Contains(req.Prompt, ownersRuling) {
			return intakefixture.QuestionResult(req, "session-manager", "Nothing further"), nil
		}
		problem("unexpected manager turn:\n%s", req.Prompt)
		return intakefixture.QuestionResult(req, "session-manager", "Nothing to do"), nil
	}
	engine.Turns[questions.TurnID("1")] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		return intakefixture.QuestionResult(req, "session-"+req.Name, "Continued"), nil
	}

	var current *trace.Repository

	dump := func() string {
		var b strings.Builder
		describe := func(label string, ws config.WorkstreamID) {
			qs, err := current.Questions(ws)
			fmt.Fprintf(&b, "%s questions: %v (err=%v)\n", label, qs, err)
		}
		describeThread := func(label string, ws config.WorkstreamID, agent string) {
			th, err := current.Thread(ws, agent)
			fmt.Fprintf(&b, "%s thread %s/%s: err=%v turns=%+v\n", label, ws, agent, err, th.Turns)
		}
		describe("first workstream", fixturekit.Stream)
		describe("second workstream", second)
		describeThread("shared manager", fixturekit.Stream, trace.Manager)
		describeThread("first asker", fixturekit.Stream, fixturekit.DemoAgent)
		describeThread("second asker", second, "agent_second")
		a1, e2, r := flags()
		fmt.Fprintf(&b, "flags: answered1=%v escalated2=%v relayed=%v\n", a1, e2, r)
		mu.Lock()
		fmt.Fprintf(&b, "problems: %v\n", problems)
		mu.Unlock()
		return b.String()
	}

	pump := func(s *journey.Service, check func() bool) {
		t.Helper()
		deadline := time.Now().Add(fixturekit.DemoTimeout)
		for !check() {
			select {
			case ticks <- clock.Now():
			case <-time.After(20 * time.Millisecond):
			}
			if time.Now().After(deadline) {
				state := dump()
				s.Close()
				t.Fatalf("condition not reached in time:\n%s", state)
			}
		}
	}

	s, err := journey.Start(ctx, opts)
	fixturekit.Must(t, err)
	closed := false
	defer func() {
		if !closed {
			s.Close()
		}
	}()
	repo := <-lives
	current = repo
	managerSettled := func(repository *trace.Repository) bool {
		manager, err := repository.ManagerThread(fixturekit.Stream)
		return err == nil && len(manager.Turns) > 0 && !slices.ContainsFunc(manager.Turns, func(q trace.QueuedTurn) bool {
			return q.CompletedAt.IsZero()
		})
	}
	f.Settle(t, s)
	clock.Advance(time.Minute)
	escalatedAndAnswered := func() bool {
		q1, err := repo.Questions(fixturekit.Stream)
		if err != nil || len(q1) != 1 || q1[0].State != trace.QuestionAnswered {
			return false
		}
		q2, err := repo.Questions(second)
		if err != nil || len(q2) != 1 || q2[0].State != trace.QuestionEscalated {
			return false
		}
		th, err := repo.Thread(fixturekit.Stream, fixturekit.DemoAgent)
		return err == nil && slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == questions.TurnID("1") }) && managerSettled(repo)
	}
	pump(s, escalatedAndAnswered)

	host1, err := repo.ManagerHost(fixturekit.Stream)
	fixturekit.Must(t, err)
	host2, err := repo.ManagerHost(second)
	fixturekit.Must(t, err)
	if host1 != fixturekit.Stream || host2 != fixturekit.Stream {
		t.Fatalf("hosts: %s, %s, want %s for both", host1, host2, fixturekit.Stream)
	}
	if own, err := repo.Thread(second, trace.Manager); err == nil && len(own.Turns) != 0 {
		t.Fatalf("workstream %s's own manager identity gained a turn: %+v", second, own)
	}
	manager1, err := repo.ManagerThread(fixturekit.Stream)
	fixturekit.Must(t, err)
	manager2, err := repo.ManagerThread(second)
	fixturekit.Must(t, err)
	if manager1.Identity.ID != trace.Manager || manager2.Identity.ID != trace.Manager || len(manager1.Turns) != len(manager2.Turns) {
		t.Fatalf("workstreams do not share one manager thread: %+v vs %+v", manager1, manager2)
	}

	ruling1, err := repo.Questions(fixturekit.Stream)
	fixturekit.Must(t, err)
	if ruling1[0].Ruling == nil || ruling1[0].Ruling.Actor != (trace.Actor{Kind: "agent", ID: trace.Manager}) {
		t.Fatalf("question 1 of %s should be answered by the manager: %+v", fixturekit.Stream, ruling1[0])
	}

	fixturekit.Must(t, s.Close())
	closed = true
	s2, c := journey.StartT(t, opts)
	repo2 := <-lives
	current = repo2

	before, err := repo2.Thread(fixturekit.Stream, fixturekit.DemoAgent)
	fixturekit.Must(t, err)
	beforeTurns := slices.Clone(before.Turns)

	q2, err := repo2.Questions(second)
	fixturekit.Must(t, err)
	if len(q2) != 1 || q2[0].State != trace.QuestionEscalated {
		t.Fatalf("escalated question lost across restart: %+v", q2)
	}

	inbox, err := c.Inbox(ctx)
	fixturekit.Must(t, err)
	var number int
	for _, e := range inbox.Entries {
		if e.Workstream == second && slices.ContainsFunc(e.Asked, func(q api.InboxQuestion) bool { return q.ID == "1" }) {
			number = e.Number
		}
	}
	if number == 0 {
		t.Fatalf("escalated question of %s is not in the inbox: %+v", second, inbox.Entries)
	}
	if _, err := c.Answer(ctx, number, ownersRuling, ""); err != nil {
		t.Fatalf("owner's ruling: %v", err)
	}

	clock.Advance(time.Minute)
	pump(s2, func() bool {
		th, err := repo2.Thread(second, "agent_second")
		return err == nil && slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == questions.TurnID("1") }) && managerSettled(repo2)
	})

	resumed, err := repo2.Thread(second, "agent_second")
	fixturekit.Must(t, err)
	i := slices.IndexFunc(resumed.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == questions.TurnID("1") })
	if i < 0 || resumed.Turns[i].Request.ThreadID != "thread_second" || !strings.Contains(resumed.Turns[i].Request.Prompt, relayedAnswer) {
		t.Fatalf("the owner's ruling did not resume the second workstream's own thread and turn: %+v", resumed.Turns)
	}
	first, err := repo2.Thread(fixturekit.Stream, fixturekit.DemoAgent)
	fixturekit.Must(t, err)
	gainedTurn := len(first.Turns) != len(beforeTurns)
	leaked := slices.ContainsFunc(first.Turns, func(q trace.QueuedTurn) bool {
		return strings.Contains(q.Request.Prompt, relayedAnswer) || strings.Contains(q.Request.Prompt, ownersRuling)
	})
	if gainedTurn || leaked {
		t.Fatalf("the owner's ruling on the second workstream's question was delivered to the first workstream too: before=%+v after=%+v", beforeTurns, first.Turns)
	}
	q2After, err := repo2.Questions(second)
	fixturekit.Must(t, err)
	if q2After[0].State != trace.QuestionAnswered || q2After[0].Ruling == nil || q2After[0].Ruling.ReturnedAnswer != relayedAnswer {
		t.Fatalf("question 1 of %s was not answered with the owner's relayed ruling: %+v", second, q2After[0])
	}

	mu.Lock()
	defer mu.Unlock()
	if len(problems) != 0 {
		t.Fatal(strings.Join(problems, "\n"))
	}
}
