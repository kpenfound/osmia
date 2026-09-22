package service

import (
	"context"
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
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// newAskingMasonFixture is a mason fixture whose chief of staff is returned,
// so a test decides whether it answers a mason's question or escalates it.
func newAskingMasonFixture(t *testing.T, masons int, drafted string, p *faults) (*shedFixture, *fakeMasons, *chief) {
	t.Helper()
	f, fake := newMasonFixture(t, masons, drafted)
	c := &chief{p: p, released: map[string]bool{}, held: map[string]chan struct{}{}}
	f.engine.mu.Lock()
	defer f.engine.mu.Unlock()
	f.engine.turns["*"] = c.turn
	return f, fake, c
}

// asking wraps the fake masons' turn: in the workstream stream, or in every
// workstream when stream is empty, the turn asks the question that gets
// number id before it writes its file.
func (m *fakeMasons) asking(p *faults, stream config.WorkstreamID, id string) func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) (*agent.Result, error) {
	return func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if stream == "" || strings.Contains(filepath.ToSlash(req.SessionDir), "/"+string(stream)+"/") {
			if err := asks(p, id)(ctx, req, verified, tools); err != nil {
				return nil, err
			}
		}
		return m.turn(ctx, req, verified, tools)
	}
}

// unitState reads the unit's state from the running service's trace.
func (f *shedFixture) unitState(stream config.WorkstreamID, unit string) (string, error) {
	states, err := f.repository().WorkflowStates(stream)
	return states[trace.UnitSubject(unit)].Value, err
}

// awaitMasonTransitions waits until the mason controller recorded n
// transitions in the workstream.
func (f *shedFixture) awaitMasonTransitions(t *testing.T, stream config.WorkstreamID, n int) {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for len(masonTransitions(t, f, stream)) < n {
		if time.Now().After(deadline) {
			t.Fatalf("mason transitions %+v, want %d", masonTransitions(t, f, stream), n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func parked(unit, q string) transitionMove {
	return transitionMove{trace.UnitSubject(unit) + "-waiting-" + q, trace.UnitSubject(unit), UnitImplementing, UnitWaiting, trace.QuestionSubject(q) + "_" + trace.QuestionOpen,
		"unit " + unit + " is waiting: its mason asked question " + q + "; the unit's workspace is kept and it takes no mason slot until the answer arrives"}
}

func resumed(unit, q string) transitionMove {
	return transitionMove{trace.UnitSubject(unit) + "-implementing-" + q, trace.UnitSubject(unit), UnitWaiting, UnitImplementing, trace.QuestionSubject(q) + "_" + trace.QuestionAnswered,
		"unit " + unit + " resumes implementing: the answer to question " + q + " is its mason's next turn"}
}

// A mason that asks parks its unit in waiting: its workspace keeps what the
// mason wrote, the chief of staff receives the question, the unit's mason
// slot goes to another workstream and its own workstream starts no other
// unit. The parked unit and its question survive a restart. The owner's
// ruling, relayed by the chief of staff, is the mason's next turn on the same
// thread, in the same workspace, and the unit is implementing again before
// that turn runs, though no mason slot is free. Every move is recorded by the
// mason controller. A third workstream starts no unit until fewer units than
// capacity.masons are implementing.
func TestMasonQuestionParksTheUnitUntilTheAnswerArrives(t *testing.T) {
	t.Parallel()
	p := &faults{}
	f, masons, _ := newAskingMasonFixture(t, 1, independentPlan, p)
	defer func() { f.stop(t) }()
	factory := runtime.Target{Scope: "factory"}
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: factory, Mode: "soft", Source: "operator"})
	a, _ := f.builtAs(t, "first")
	b, _ := f.builtAs(t, "second")
	c, _ := f.builtAs(t, "third")
	// Among equal workstreams, the slot goes in ID order.
	ids := []config.WorkstreamID{a, b, c}
	slices.Sort(ids)
	asking, other, third := ids[0], ids[1], ids[2]

	var mu sync.Mutex
	var answered []agent.Request
	var during []string
	f.engine.mu.Lock()
	f.engine.turns[masonTurnID("resume")] = masons.asking(p, asking, "1")
	f.engine.mu.Unlock()
	f.answer("1", func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		state, err := f.unitState(asking, "resume")
		if err != nil {
			p.report("unit state: %v", err)
		}
		if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), "answered.go"), []byte("package trace\n"), 0644); err != nil {
			p.report("answer turn: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		answered, during = append(answered, req), append(during, state)
		return nil
	})
	mutation(t, f.c, "DELETE", "pause", factory)

	// The asking unit parks and its slot goes to the other workstream.
	deadline := time.Now().Add(demoTimeout)
	for {
		asked, err := f.repository().Questions(asking)
		must(t, err)
		if len(asked) == 1 && asked[0].State == trace.QuestionEscalated {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("question 1 was never escalated: %+v", asked)
		}
		time.Sleep(50 * time.Millisecond)
	}
	f.awaitMasonTransitions(t, asking, 2)
	f.awaitMasonRan(t, other, "resume")
	settle()
	p.check(t)
	masons.check(t)
	want := []transitionMove{started("resume", f.startedReason(t, asking, "resume")), parked("resume", "1")}
	if got := masonTransitions(t, f, asking); !reflect.DeepEqual(got, want) {
		t.Fatalf("mason transitions %+v, want %+v", got, want)
	}
	// The question the mason raised is one notice for the chief of staff,
	// which is how the fake chief saw it and escalated it.
	opened := "Question 1 is open, asked by the mason: " + askedQuestion
	if body := f.notice(t, asking, "question_1_open"); body != opened {
		t.Fatalf("the question notice %q, want %q", body, opened)
	}
	f.checkUnits(t, asking, []UnitStatus{{Unit: "resume", State: UnitWaiting}, {Unit: "dedupe", State: UnitReady}})
	f.checkUnits(t, other, []UnitStatus{{Unit: "resume", State: UnitImplementing}, {Unit: "dedupe", State: UnitReady}})
	f.checkUnits(t, third, []UnitStatus{{Unit: "resume", State: UnitReady}, {Unit: "dedupe", State: UnitReady}})
	q := f.question(t, asking, "1")
	if q.Asked.AskedBy.ID != masonAgent("resume") || q.Asked.Thread != masonAgent("resume") || q.Asked.Turn != masonTurnID("resume") || q.Asked.Unit != "resume" || q.Asked.Question != askedQuestion {
		t.Fatalf("question %+v", q.Asked)
	}
	workspace := filepath.Join(f.opts.Config.Root, unitsDirectory, string(f.project), string(asking), "resume")
	if _, err := os.Stat(filepath.Join(workspace, masonWrote)); err != nil {
		t.Fatalf("the parked unit's workspace lost the mason's work: %v", err)
	}

	f.stop(t)
	f.start(t)
	settle()
	f.checkUnits(t, asking, []UnitStatus{{Unit: "resume", State: UnitWaiting}, {Unit: "dedupe", State: UnitReady}})
	if th := f.thread(t, asking, masonAgent("resume")); !th.Parked() || len(th.Turns) != 1 {
		t.Fatalf("the mason's thread after a restart: %+v", th)
	}
	if got := f.question(t, asking, "1").State; got != trace.QuestionEscalated {
		t.Fatalf("question 1 is %s after a restart", got)
	}

	f.rule(t, "1")
	f.awaitTurn(t, asking, masonAgent("resume"), questions.TurnID("1"))
	deadline = time.Now().Add(demoTimeout)
	for th := f.thread(t, asking, masonAgent("resume")); len(th.Turns) != 2 || th.Turns[1].CompletedAt.IsZero(); th = f.thread(t, asking, masonAgent("resume")) {
		if time.Now().After(deadline) {
			t.Fatalf("the answer turn never completed: %+v", th.Turns)
		}
		time.Sleep(50 * time.Millisecond)
	}
	settle()
	p.check(t)
	want = append(want, resumed("resume", "1"))
	if got := masonTransitions(t, f, asking); !reflect.DeepEqual(got, want) {
		t.Fatalf("mason transitions %+v, want %+v", got, want)
	}
	f.checkUnits(t, asking, []UnitStatus{{Unit: "resume", State: UnitImplementing}, {Unit: "dedupe", State: UnitReady}})
	mu.Lock()
	defer mu.Unlock()
	if len(answered) != 1 || !reflect.DeepEqual(during, []string{UnitImplementing}) {
		t.Fatalf("answer turns %d, the unit was %q while they ran", len(answered), during)
	}
	if !strings.Contains(answered[0].Prompt, relayedRuling) || !strings.Contains(answered[0].SystemPrompt, "You are a mason of the") {
		t.Fatalf("answer turn prompt %q, system prompt %q", answered[0].Prompt, answered[0].SystemPrompt)
	}
	for _, name := range []string{masonWrote, "answered.go"} {
		if _, err := os.Stat(filepath.Join(workspace, name)); err != nil {
			t.Fatalf("the unit's workspace lacks %s: %v", name, err)
		}
	}
	if th := f.thread(t, asking, masonAgent("resume")); th.Turns[1].Request.Unit != "resume" || th.Turns[1].Status() != "idle" {
		t.Fatalf("answer turn %+v", th.Turns[1])
	}

	// Two units are implementing with one mason slot: the third workstream
	// waits until fewer than one are, here because pauses take theirs away.
	f.checkUnits(t, other, []UnitStatus{{Unit: "resume", State: UnitImplementing}, {Unit: "dedupe", State: UnitReady}})
	f.checkUnits(t, third, []UnitStatus{{Unit: "resume", State: UnitReady}, {Unit: "dedupe", State: UnitReady}})
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "workstream", Project: f.project, Workstream: other}, Mode: "soft", Source: "operator"})
	settle()
	if got := masonTransitions(t, f, third); len(got) != 0 {
		t.Fatalf("%s started a unit while one was implementing with one mason slot: %+v", third, got)
	}
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "workstream", Project: f.project, Workstream: asking}, Mode: "soft", Source: "operator"})
	f.awaitMasonRan(t, third, "resume")
	masons.check(t)
}

// A mason that asks again in the turn that delivers its answer parks the unit
// again, and each answer resumes it: every park and resume is its own
// transition, named after its question. While the unit waits, its workstream
// starts no other unit, though mason slots are free.
func TestMasonAsksAgainInItsAnswerTurn(t *testing.T) {
	t.Parallel()
	p := &faults{}
	f, fakes, c := newAskingMasonFixture(t, 4, independentPlan, p)
	defer f.stop(t)
	c.release("1")
	c.release("2")
	f.engine.mu.Lock()
	f.engine.turns[masonTurnID("resume")] = fakes.asking(p, "", "1")
	f.engine.mu.Unlock()
	f.answer("1", asks(p, "2"))
	f.answer("2", func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) error { return nil })
	stream, _ := f.builtAs(t, "design")
	f.awaitMasonTransitions(t, stream, 5)
	settle()
	p.check(t)
	fakes.check(t)
	want := []transitionMove{started("resume", f.startedReason(t, stream, "resume")), parked("resume", "1"), resumed("resume", "1"), parked("resume", "2"), resumed("resume", "2")}
	if got := masonTransitions(t, f, stream); !reflect.DeepEqual(got, want) {
		t.Fatalf("mason transitions %+v, want %+v", got, want)
	}
	f.checkUnits(t, stream, []UnitStatus{{Unit: "resume", State: UnitImplementing}, {Unit: "dedupe", State: UnitReady}})
	if runs := fakes.requests(stream); len(runs) != 1 {
		t.Fatalf("mason turns %d", len(runs))
	}
	// Resuming is no start: the workstream last started a unit when the unit
	// first moved to implementing, which orders it among equal workstreams.
	b, found, err := (&masons{s: f.s, repository: f.repository()}).read(stream)
	must(t, err)
	for _, tr := range allTransitions(t, f.trace, stream) {
		if tr.ID == masonTransitionID("resume") && (!found || !b.started.Equal(tr.At)) {
			t.Fatalf("the workstream last started a unit at %s, want %s", b.started, tr.At)
		}
	}
}
