package lifecycle_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestWorkgroupSharesOneChiefOfStaffAcrossMembersAndARestart builds a
// two-workstream workgroup with a test fixture (EnsureWorkgroups) rather
// than the architect flow, as the plan allows. Both workstreams' roles ask
// the shared chief of staff at once; neither workstream has a
// chief-of-staff thread of its own. The chief answers one directly and
// escalates the other's question, qualified with its own workstream, to
// the owner. The service then restarts, and the owner's ruling, relayed by
// the chief, resumes the asking role and turn in its own workstream: no
// question is dropped, and the first workstream's answer, already
// delivered before the restart, never waited on the second's escalation.
func TestWorkgroupSharesOneChiefOfStaffAcrossMembersAndARestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const second config.WorkstreamID = "w_00000000000000000000000000000f02"
	const question1, question2 = "Where does state live?", "Should the index rebuild on start?"
	const ownersRuling, relayedAnswer = "Keep the index fixed at start.", "The index does not rebuild on start."

	f := newQuestionFixture(t, "wg-", "[capacity]\nmasons = 2\n", "# Charter\n\n1. Keep state in files under the root.\n", []questionAsker{
		{stream, demoAgent, demoThread, "mason", "build"},
		{second, "agent_second", "thread_second", "mason", "build2"},
	})
	opts, clock, engine, lives, ticks := f.opts, f.clock, f.engine, f.lives, f.ticks

	// A test fixture groups the two workstreams under one chief of staff,
	// standing in for the architect's own graph (spec: "built by a test
	// fixture or service helper").
	grouping, err := trace.Open(f.cfg.Root, f.cfg.Project)
	must(t, err)
	_, err = trace.CreateWorkgroup(ctx, grouping, []config.WorkstreamID{stream, second}, clock.Now(), ownerActor, "test-workgroup")
	must(t, err)
	must(t, grouping.Close())

	var mu sync.Mutex
	var problems []string
	problem := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	ask := func(question string) demoTurn {
		return func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
			if out, err := callTool(ctx, tools, questions.AskTool, map[string]any{"question": question}); err != nil || !strings.Contains(out, `"recorded":true`) {
				problem("%s ask: %s %v", req.Name, out, err)
			}
			return questionResult(req, "session-"+req.Name, "Asked"), nil
		}
	}
	engine.Turns["build"] = ask(question1)
	engine.Turns["build2"] = ask(question2)

	// The chief's prompt replays the thread's earlier turns (as
	// TestOwnerRulingResumesTheAskersAcrossRestarts notes), so a turn
	// delivering the newest event still contains the older ones' trigger
	// text too, and both masons' notices can land together in one turn. A
	// real chief can make several tool calls in a turn, so this fake
	// performs every action whose trigger text is present and whose flag
	// isn't set yet, in order relay, escalate, answer, instead of stopping
	// after the first: flags make each action idempotent, so a replayed or
	// combined prompt never repeats an action regardless of how many
	// trigger texts it still carries.
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
			out, err := callTool(ctx, tools, questions.RelayRulingTool, map[string]any{"question": string(second) + ":1", "text": relayedAnswer, "scope": "local"})
			if err != nil || !strings.Contains(out, `"recorded":true`) {
				problem("relay: %s %v", out, err)
			}
			flagMu.Lock()
			relayed = true
			flagMu.Unlock()
			acted = true
		}
		if needEscalate {
			out, err := callTool(ctx, tools, questions.EscalateTool, map[string]any{
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
			out, err := callTool(ctx, tools, questions.AnswerTool, map[string]any{"question": "1", "text": "In files under the root.", "citations": []string{"charter#1"}})
			if err != nil || !strings.Contains(out, `"recorded":true`) {
				problem("answer: %s %v", out, err)
			}
			flagMu.Lock()
			answered1 = true
			flagMu.Unlock()
			acted = true
		}
		if acted {
			return questionResult(req, "session-chief", "Handled"), nil
		}

		// Nothing new to do: a replayed or redelivered prompt is harmless as
		// long as it still only carries trigger text already acted on.
		if strings.Contains(req.Prompt, "asked by the mason: "+question1) ||
			strings.Contains(req.Prompt, "asked by the mason: "+question2) ||
			strings.Contains(req.Prompt, ownersRuling) {
			return questionResult(req, "session-chief", "Nothing further"), nil
		}
		problem("unexpected chief-of-staff turn:\n%s", req.Prompt)
		return questionResult(req, "session-chief", "Nothing to do"), nil
	}
	engine.Turns[questions.TurnID("1")] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		return questionResult(req, "session-"+req.Name, "Continued"), nil
	}

	// current is the live repository of whichever service lifetime is
	// running; pump's timeout dump reads it fresh so it reports the state of
	// the lifetime that actually stalled.
	var current *trace.Repository

	// dump renders both workstreams' questions and states, the shared
	// chief-of-staff thread's turns and outcomes, the two askers' own
	// threads, and any problems callTool already recorded, so a timeout
	// names what it actually observed instead of only "not reached in time".
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
		describe("first workstream", stream)
		describe("second workstream", second)
		describeThread("shared chief of staff", stream, trace.ChiefOfStaff)
		describeThread("first asker", stream, demoAgent)
		describeThread("second asker", second, "agent_second")
		a1, e2, r := flags()
		fmt.Fprintf(&b, "flags: answered1=%v escalated2=%v relayed=%v\n", a1, e2, r)
		mu.Lock()
		fmt.Fprintf(&b, "problems: %v\n", problems)
		mu.Unlock()
		return b.String()
	}

	// pump sends ticks, driving reconciliation passes, until check is true.
	pump := func(s *Service, check func() bool) {
		t.Helper()
		deadline := time.Now().Add(demoTimeout)
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

	s, err := Start(ctx, opts)
	must(t, err)
	closed := false
	defer func() {
		if !closed {
			s.Close()
		}
	}()
	repo := <-lives
	current = repo
	// A tool's durable effect can become visible before its MCP response
	// reaches the chief. Restart only after the chief has finished the turn.
	chiefSettled := func(repository *trace.Repository) bool {
		chief, err := repository.ChiefOfStaffThread(stream)
		return err == nil && len(chief.Turns) > 0 && !slices.ContainsFunc(chief.Turns, func(q trace.QueuedTurn) bool {
			return q.CompletedAt.IsZero()
		})
	}
	// Let both masons ask their questions, then close the event window so
	// the chief-of-staff turns that deliver the two notices are dispatched.
	f.settle(t, s)
	clock.Advance(time.Minute)
	escalatedAndAnswered := func() bool {
		q1, err := repo.Questions(stream)
		if err != nil || len(q1) != 1 || q1[0].State != trace.QuestionAnswered {
			return false
		}
		q2, err := repo.Questions(second)
		if err != nil || len(q2) != 1 || q2[0].State != trace.QuestionEscalated {
			return false
		}
		th, err := repo.Thread(stream, demoAgent)
		return err == nil && slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == questions.TurnID("1") }) && chiefSettled(repo)
	}
	pump(s, escalatedAndAnswered)

	// Neither workstream has a chief of staff of its own: both resolve to
	// the same shared thread, which lives on the workgroup's host (the
	// first member). second's own identity, created with it before it
	// joined the workgroup, never gains a turn: every chief-of-staff turn
	// addressed to it is routed to the host instead.
	host1, err := repo.ChiefOfStaffHost(stream)
	must(t, err)
	host2, err := repo.ChiefOfStaffHost(second)
	must(t, err)
	if host1 != stream || host2 != stream {
		t.Fatalf("hosts: %s, %s, want %s for both", host1, host2, stream)
	}
	if own, err := repo.Thread(second, trace.ChiefOfStaff); err == nil && len(own.Turns) != 0 {
		t.Fatalf("workstream %s's own chief-of-staff identity gained a turn: %+v", second, own)
	}
	chief1, err := repo.ChiefOfStaffThread(stream)
	must(t, err)
	chief2, err := repo.ChiefOfStaffThread(second)
	must(t, err)
	if chief1.Identity.ID != trace.ChiefOfStaff || chief2.Identity.ID != trace.ChiefOfStaff || len(chief1.Turns) != len(chief2.Turns) {
		t.Fatalf("workstreams do not share one chief-of-staff thread: %+v vs %+v", chief1, chief2)
	}

	// Question 1 (first workstream) was answered without the owner, while
	// question 1 (second workstream, escalated) still waits: the first
	// workstream's work was never held up by the second's open escalation.
	ruling1, err := repo.Questions(stream)
	must(t, err)
	if ruling1[0].Ruling == nil || ruling1[0].Ruling.Actor != (trace.Actor{Kind: "agent", ID: trace.ChiefOfStaff}) {
		t.Fatalf("question 1 of %s should be answered by the chief of staff: %+v", stream, ruling1[0])
	}

	// The service restarts: the escalation and the answered-but-not-yet-
	// owner-ruled state both survive, and nothing is lost or duplicated.
	must(t, s.Close())
	closed = true
	s2, c := start(t, opts)
	repo2 := <-lives
	current = repo2

	// Snapshot the first asker's thread before the owner answers the
	// second's escalation: question 1 of the first workstream was already
	// answered by the chief directly in the first lifetime, so its thread
	// already carries a turn named TurnID("1") ("answer_1") before this
	// point. Checking for the mere presence of that TurnID after the second
	// pump would be true regardless of whether the owner's ruling (meant for
	// the second workstream) ever reached the first, so the snapshot lets
	// the real check below compare against what the first asker's thread
	// held before the second pump ran, instead.
	before, err := repo2.Thread(stream, demoAgent)
	must(t, err)
	beforeTurns := slices.Clone(before.Turns)

	q2, err := repo2.Questions(second)
	must(t, err)
	if len(q2) != 1 || q2[0].State != trace.QuestionEscalated {
		t.Fatalf("escalated question lost across restart: %+v", q2)
	}

	inbox, err := c.Inbox(ctx)
	must(t, err)
	var number int
	for _, e := range inbox.Entries {
		if e.Workstream == second && slices.ContainsFunc(e.Asked, func(q InboxQuestion) bool { return q.ID == "1" }) {
			number = e.Number
		}
	}
	if number == 0 {
		t.Fatalf("escalated question of %s is not in the inbox: %+v", second, inbox.Entries)
	}
	if _, err := c.Answer(ctx, number, ownersRuling, ""); err != nil {
		t.Fatalf("owner's ruling: %v", err)
	}

	// Close the event window again so the chief-of-staff turn that relays
	// the owner's ruling is dispatched.
	clock.Advance(time.Minute)
	pump(s2, func() bool {
		th, err := repo2.Thread(second, "agent_second")
		return err == nil && slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == questions.TurnID("1") }) && chiefSettled(repo2)
	})

	// The owner's answer resumes the original asking role and turn in its
	// own workstream: the second workstream's asker, not the first's.
	resumed, err := repo2.Thread(second, "agent_second")
	must(t, err)
	i := slices.IndexFunc(resumed.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == questions.TurnID("1") })
	if i < 0 || resumed.Turns[i].Request.ThreadID != "thread_second" || !strings.Contains(resumed.Turns[i].Request.Prompt, relayedAnswer) {
		t.Fatalf("the owner's ruling did not resume the second workstream's own thread and turn: %+v", resumed.Turns)
	}
	first, err := repo2.Thread(stream, demoAgent)
	must(t, err)
	gainedTurn := len(first.Turns) != len(beforeTurns)
	leaked := slices.ContainsFunc(first.Turns, func(q trace.QueuedTurn) bool {
		return strings.Contains(q.Request.Prompt, relayedAnswer) || strings.Contains(q.Request.Prompt, ownersRuling)
	})
	if gainedTurn || leaked {
		t.Fatalf("the owner's ruling on the second workstream's question was delivered to the first workstream too: before=%+v after=%+v", beforeTurns, first.Turns)
	}
	q2After, err := repo2.Questions(second)
	must(t, err)
	if q2After[0].State != trace.QuestionAnswered || q2After[0].Ruling == nil || q2After[0].Ruling.ReturnedAnswer != relayedAnswer {
		t.Fatalf("question 1 of %s was not answered with the owner's relayed ruling: %+v", second, q2After[0])
	}

	mu.Lock()
	defer mu.Unlock()
	if len(problems) != 0 {
		t.Fatal(strings.Join(problems, "\n"))
	}
}
