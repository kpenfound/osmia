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
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// newAskingMasonFixture is a mason fixture whose chief of staff is returned,
// so a test decides whether it answers a mason's question or escalates it.
func newAskingMasonFixture(t *testing.T, masons int, drafted string, p *faults) (*shedFixture, *fakeMasons, *chief) {
	t.Helper()
	return newAskingMasonFixturePrepared(t, masons, drafted, p, nil)
}

// newAskingMasonFixturePrepared is newAskingMasonFixture with prepare, run
// before the service starts, so a test can set opts.schedulePassed and wait
// for passes to demonstrably complete instead of a fixed sleep.
func newAskingMasonFixturePrepared(t *testing.T, masons int, drafted string, p *faults, prepare func(*Options)) (*shedFixture, *fakeMasons, *chief) {
	t.Helper()
	f, fake := newMasonFixturePrepared(t, masons, drafted, prepare)
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
// slot goes to another workstream and its own workstream does not start
// dedupe, which shares its footprint. The parked unit and its question survive a restart. The owner's
// ruling, relayed by the chief of staff, is the mason's next turn on the same
// thread, in the same workspace, and the unit is implementing again before
// that turn runs, though no mason slot is free. Every move is recorded by the
// mason controller. A third workstream starts no unit until fewer units than
// capacity.masons are implementing.
func TestMasonQuestionParksTheUnitUntilTheAnswerArrives(t *testing.T) {
	t.Parallel()
	p := &faults{}
	passed, prepare := countingSchedule()
	f, masons, _ := newAskingMasonFixturePrepared(t, 1, independentPlan, p, prepare)
	defer func() { f.stop(t) }()
	factory := runtime.Target{Scope: "factory"}
	built := f.seedBuildingPaused(t, factory, independentPlan, "first", "second", "third")
	a, b, c := built[0], built[1], built[2]
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
	// Further passes find nothing more to do for either workstream.
	awaitPasses(t, passed, 3)
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
	f.checkUnits(t, asking, []UnitStatus{{Unit: "resume", State: UnitWaiting}, f.deferred(t, asking, "dedupe", overlapping("resume"))})
	f.checkUnits(t, other, []UnitStatus{{Unit: "resume", State: UnitImplementing}, f.deferred(t, other, "dedupe", overlapping("resume"))})
	f.checkUnits(t, third, []UnitStatus{f.deferred(t, third, "resume", slotless(1)), f.deferred(t, third, "dedupe", slotless(1))})
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
	// Further passes after the restart find the parked unit still waiting.
	awaitPasses(t, passed, 3)
	f.checkUnits(t, asking, []UnitStatus{{Unit: "resume", State: UnitWaiting}, f.deferred(t, asking, "dedupe", overlapping("resume"))})
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
	// Further passes find no more work beyond resuming the unit.
	awaitPasses(t, passed, 3)
	p.check(t)
	want = append(want, resumed("resume", "1"))
	if got := masonTransitions(t, f, asking); !reflect.DeepEqual(got, want) {
		t.Fatalf("mason transitions %+v, want %+v", got, want)
	}
	f.checkUnits(t, asking, []UnitStatus{{Unit: "resume", State: UnitImplementing}, f.deferred(t, asking, "dedupe", overlapping("resume"))})
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
	f.checkUnits(t, other, []UnitStatus{{Unit: "resume", State: UnitImplementing}, f.deferred(t, other, "dedupe", overlapping("resume"))})
	f.checkUnits(t, third, []UnitStatus{f.deferred(t, third, "resume", slotless(1)), f.deferred(t, third, "dedupe", slotless(1))})
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "workstream", Project: f.project, Workstream: other}, Mode: "soft", Source: "owner"})
	// Further passes still find the third workstream without a free slot,
	// since the asking workstream still holds one.
	awaitPasses(t, passed, 3)
	if got := masonTransitions(t, f, third); len(got) != 0 {
		t.Fatalf("%s started a unit while one was implementing with one mason slot: %+v", third, got)
	}
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "workstream", Project: f.project, Workstream: asking}, Mode: "soft", Source: "owner"})
	f.awaitMasonRan(t, third, "resume")
	masons.check(t)
}

// A mason that asks again in the turn that delivers its answer parks the unit
// again, and each answer resumes it: every park and resume is its own
// transition, named after its question. While the unit waits, its workstream
// does not start dedupe, which shares its footprint, though mason slots are
// free.
func TestMasonAsksAgainInItsAnswerTurn(t *testing.T) {
	t.Parallel()
	p := &faults{}
	passed, prepare := countingSchedule()
	f, fakes, c := newAskingMasonFixturePrepared(t, 4, independentPlan, p, prepare)
	defer f.stop(t)
	c.release("1")
	c.release("2")
	f.engine.mu.Lock()
	f.engine.turns[masonTurnID("resume")] = fakes.asking(p, "", "1")
	f.engine.mu.Unlock()
	f.answer("1", asks(p, "2"))
	f.answer("2", func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) error { return nil })
	stream := f.seedBuilding(t, "design", independentPlan)
	f.awaitMasonTransitions(t, stream, 5)
	// Further passes find no more parks or resumes beyond the five.
	awaitPasses(t, passed, 3)
	p.check(t)
	fakes.check(t)
	want := []transitionMove{started("resume", f.startedReason(t, stream, "resume")), parked("resume", "1"), resumed("resume", "1"), parked("resume", "2"), resumed("resume", "2")}
	if got := masonTransitions(t, f, stream); !reflect.DeepEqual(got, want) {
		t.Fatalf("mason transitions %+v, want %+v", got, want)
	}
	f.checkUnits(t, stream, []UnitStatus{{Unit: "resume", State: UnitImplementing}, f.deferred(t, stream, "dedupe", overlapping("resume"))})
	if runs := implementationRuns(fakes, stream); len(runs) != 1 {
		t.Fatalf("implementation turns %d", len(runs))
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

// askOnLatestTurn runs the agent's latest queued turn of the unit to its end,
// asking text through the question tool as the turn would, and returns the
// question's number.
func askOnLatestTurn(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, agent, role, unit, text string) string {
	t.Helper()
	ctx := context.Background()
	th, err := repository.Thread(stream, agent)
	must(t, err)
	turn := th.Turns[len(th.Turns)-1].Request.TurnID
	token := "asked-" + turn
	directory := filepath.Join(f.s.cfg.Root.String(), "threads", string(f.project), string(stream), agent, token)
	q, err := repository.ClaimTurn(ctx, stream, agent, token, directory, f.clock.Now())
	must(t, err)
	scope := coreadapter.Scope{Project: string(f.project), Workstream: string(stream), Unit: unit, Thread: agent, Turn: turn, Role: role}
	asked, err := repository.Ask(ctx, agent, scope, text, f.clock.Now())
	must(t, err)
	h := q.Request.Header
	h.Schema, h.ID, h.At = "osmia.trace.turn-response", trace.EventID(q.Request.ID, "response"), f.clock.Now()
	response := trace.TurnResponse{Header: h, AgentID: agent, ThreadID: agent, TurnID: turn, RequestID: q.Request.ID, RequestRevision: q.Request.Revision,
		Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: q.Request.Profile.Backend, ID: token}, SessionDirectory: directory, StartedAt: q.Claim.At,
			Outcome: &coreadapter.Outcome{Status: "waiting", Report: "Asked question " + asked.ID}}}
	must(t, repository.CaptureTurn(ctx, q.Claim.Token, response))
	must(t, repository.CompleteTurn(ctx, stream, agent, turn, q.Claim.Token, f.clock.Now()))
	return asked.ID
}

// queueFollowingTurn queues a turn of the unit on the agent's thread after the
// one that asked, as a move queues the turn that carries its note.
func queueFollowingTurn(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, agent, unit, turn string) {
	t.Helper()
	th, err := repository.Thread(stream, agent)
	must(t, err)
	last := th.Turns[len(th.Turns)-1].Request
	h := trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: f.project, Workstream: stream, Unit: unit,
		At: f.clock.Now(), Actor: trace.Actor{Kind: "owner", ID: "local"}, Cause: "move"}
	_, err = repository.EnqueueTurn(context.Background(), trace.TurnRequest{Header: h, AgentID: agent, ThreadID: agent, TurnID: turn, Profile: last.Profile, SystemPrompt: last.SystemPrompt, Prompt: "Carry on."})
	must(t, err)
}

// waitingTransitions returns the IDs of the unit's recorded moves to waiting.
func waitingTransitions(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string) []string {
	t.Helper()
	transitions, err := trace.Read[trace.Transition](repository, stream)
	must(t, err)
	var ids []string
	for _, tr := range transitions {
		if tr.Subject == trace.UnitSubject(unit) && tr.To == UnitWaiting {
			ids = append(ids, tr.ID)
		}
	}
	return ids
}

// A question parks its unit only while the turn that asked it is the mason's
// latest. When a later turn follows it before the unit waited, as when the
// owner moves a contested unit to implementing while its mason's question is
// open, the unit stays implementing for that turn, and the question's answer
// still reaches the mason when it arrives. The unit then waits on a question
// the later turn asks.
func TestAQuestionAnEarlierTurnAskedDoesNotParkTheUnit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, _ := newMasonFixture(t, 1, independentPlan)
	base := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	stream, repository := seedBuild(t, f, "overtaken", independentPlan, config.WorkspacesGit, base)
	m := newMasonController(f.s, repository)
	b, found, err := m.read(stream)
	must(t, err)
	if !found {
		t.Fatal("fixture workstream is not building")
	}
	if started, blocked, err := m.start(ctx, b, "resume"); err != nil || !started || blocked {
		t.Fatalf("unit resume did not start: started %v, blocked %v, %v", started, blocked, err)
	}
	agent := masonAgent("resume")
	askOnLatestTurn(t, f, repository, stream, agent, masonRole, "resume", "Should resume keep partial uploads?")
	queueFollowingTurn(t, f, repository, stream, agent, "resume", agent+"-move-1")

	state, err := repository.Workflow(stream, trace.UnitSubject("resume"))
	must(t, err)
	if value, err := m.follow(ctx, stream, "resume", state); err != nil || value != UnitImplementing {
		t.Fatalf("follow with the asking turn overtaken left the unit %q (%v), want implementing", value, err)
	}
	if waits := waitingTransitions(t, repository, stream, "resume"); len(waits) != 0 {
		t.Fatalf("an overtaken question parked the unit: %v", waits)
	}

	asked := askOnLatestTurn(t, f, repository, stream, agent, masonRole, "resume", "Which store holds the offsets?")
	state, err = repository.Workflow(stream, trace.UnitSubject("resume"))
	must(t, err)
	if value, err := m.follow(ctx, stream, "resume", state); err != nil || value != UnitWaiting {
		t.Fatalf("follow after the latest turn asked question %s left the unit %q (%v), want waiting", asked, value, err)
	}
	if waits, want := waitingTransitions(t, repository, stream, "resume"), []string{trace.UnitSubject("resume") + "-waiting-" + asked}; !reflect.DeepEqual(waits, want) {
		t.Fatalf("waiting transitions %v, want %v", waits, want)
	}
}

// A reviewer's question parks its unit only while the turn that asked it is
// the reviewer's latest; a review turn that follows it, as a move to reviewing
// queues, reviews without waiting on it.
func TestAQuestionAnEarlierReviewTurnAskedDoesNotParkTheUnit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repository := newReviewFixture(t, "overtaken-review")
	r := &reviewers{masons: newMasonController(f.s, repository)}
	state, err := repository.Workflow(stream, trace.UnitSubject("resume"))
	must(t, err)
	must(t, r.one(ctx, stream, "resume", state, false))
	agent := reviewerAgent("resume")
	askOnLatestTurn(t, f, repository, stream, agent, reviewerRole, "resume", "Is the retry bound part of the acceptance?")
	queueFollowingTurn(t, f, repository, stream, agent, "resume", agent+"-move-1")

	state, err = repository.Workflow(stream, trace.UnitSubject("resume"))
	must(t, err)
	if value, err := r.followQuestion(ctx, stream, "resume", state); err != nil || value != UnitReviewing {
		t.Fatalf("followQuestion with the asking turn overtaken left the unit %q (%v), want reviewing", value, err)
	}
	if waits := waitingTransitions(t, repository, stream, "resume"); len(waits) != 0 {
		t.Fatalf("an overtaken question parked the unit: %v", waits)
	}

	asked := askOnLatestTurn(t, f, repository, stream, agent, reviewerRole, "resume", "Does the bound cover restarts?")
	state, err = repository.Workflow(stream, trace.UnitSubject("resume"))
	must(t, err)
	if value, err := r.followQuestion(ctx, stream, "resume", state); err != nil || value != UnitWaiting {
		t.Fatalf("followQuestion after the latest turn asked question %s left the unit %q (%v), want waiting", asked, value, err)
	}
	if waits, want := waitingTransitions(t, repository, stream, "resume"), []string{trace.UnitSubject("resume") + "-reviewer-waiting-" + asked}; !reflect.DeepEqual(waits, want) {
		t.Fatalf("waiting transitions %v, want %v", waits, want)
	}
}
