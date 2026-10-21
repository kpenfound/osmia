package executionjourney

import (
	context "context"
	os "os"
	filepath "path/filepath"
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
	runtime "github.com/kpenfound/osmia/internal/runtime"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func newAskingMasonFixture(t *testing.T, masons int, drafted string, p *faults) (*shedFixture, *fakeMasons, *chief) {
	t.Helper()
	return newAskingMasonFixturePrepared(t, masons, drafted, p, nil)
}

func newAskingMasonFixturePrepared(t *testing.T, masons int, drafted string, p *faults, prepare func(*Options)) (*shedFixture, *fakeMasons, *chief) {
	t.Helper()
	f, fake := newMasonFixturePrepared(t, masons, drafted, prepare)
	c := &chief{P: p, Released: map[string]bool{}, Held: map[string]chan struct{}{}}
	f.engine.Mu.Lock()
	defer f.engine.Mu.Unlock()
	f.engine.Turns["*"] = c.Turn
	return f, fake, c
}

func (f *shedFixture) unitState(stream config.WorkstreamID, unit string) (string, error) {
	states, err := f.repository().WorkflowStates(stream)
	return states[trace.UnitSubject(unit)].Value, err
}

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

var parked = intakefixture.Parked

var resumed = intakefixture.Resumed

func TestMasonQuestionParksTheUnitUntilTheAnswerArrives(t *testing.T) {
	t.Parallel()
	p := &faults{}
	passed, prepare := countingSchedule()
	f, masons, _ := newAskingMasonFixturePrepared(t, 1, independentPlan, p, prepare)
	defer func() { f.stop(t) }()
	factory := runtime.Target{Scope: "factory"}
	built := f.seedBuildingPaused(t, factory, independentPlan, "first", "second", "third")
	a, b, c := built[0], built[1], built[2]

	ids := []config.WorkstreamID{a, b, c}
	slices.Sort(ids)
	asking, other, third := ids[0], ids[1], ids[2]

	var mu sync.Mutex
	var answered []agent.Request
	var during []string
	f.engine.Mu.Lock()
	f.engine.Turns[masonTurnID("resume")] = masons.Asking(p, asking, "1")
	f.engine.Mu.Unlock()
	f.answer("1", func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		state, err := f.unitState(asking, "resume")
		if err != nil {
			p.Report("unit state: %v", err)
		}
		if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), "answered.go"), []byte("package trace\n"), 0644); err != nil {
			p.Report("answer turn: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		answered, during = append(answered, req), append(during, state)
		return nil
	})
	mutation(t, f.c, "DELETE", "pause", factory)

	deadline := time.Now().Add(demoTimeout)
	for {
		asked, err := f.repository().Questions(asking)
		must(t, err)
		if len(asked) == 1 && asked[0].State == trace.QuestionEscalated {
			break
		}
		if time.Now().After(deadline) {
			t.Logf("workstream %s question records: %+v", asking, asked)
			chief, chiefErr := f.repository().ChiefOfStaffThread(asking)
			t.Logf("chief_of_staff thread: %+v (error: %v)", chief, chiefErr)
			for _, turn := range chief.Turns {
				state := turn.Status()
				if turn.Claim == nil && turn.Response == nil && turn.CompletedAt.IsZero() {
					state = "queued"
				}
				t.Logf("chief_of_staff turn %s state=%s: %+v", turn.Request.TurnID, state, turn)
			}
			outbox, outboxErr := f.repository().Outbox(asking)
			pending := slices.DeleteFunc(outbox, func(e trace.OutboxEntry) bool { return e.Acknowledged })
			t.Logf("workstream %s pending notices and events: %+v (error: %v)", asking, pending, outboxErr)
			p.Mu.Lock()
			t.Logf("fake faults: %v", p.Errs)
			p.Mu.Unlock()
			t.Fatalf("question 1 was never escalated: %+v", asked)
		}
		time.Sleep(50 * time.Millisecond)
	}
	f.awaitMasonTransitions(t, asking, 2)
	f.awaitMasonRan(t, other, "resume")

	awaitPasses(t, passed, 3)
	p.Check(t)
	masons.Check(t)
	want := []transitionMove{started("resume", f.startedReason(t, asking, "resume")), parked("resume", "1")}
	if got := masonTransitions(t, f, asking); !reflect.DeepEqual(got, want) {
		t.Fatalf("mason transitions %+v, want %+v", got, want)
	}

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
	f.awaitCompleted(t, asking, masonAgent("resume"), questions.TurnID("1"))

	awaitPasses(t, passed, 3)
	p.Check(t)
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

	f.checkUnits(t, other, []UnitStatus{{Unit: "resume", State: UnitImplementing}, f.deferred(t, other, "dedupe", overlapping("resume"))})
	f.checkUnits(t, third, []UnitStatus{f.deferred(t, third, "resume", slotless(1)), f.deferred(t, third, "dedupe", slotless(1))})
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "workstream", Project: f.project, Workstream: other}, Mode: "soft", Source: "owner"})

	awaitPasses(t, passed, 3)
	if got := masonTransitions(t, f, third); len(got) != 0 {
		t.Fatalf("%s started a unit while one was implementing with one mason slot: %+v", third, got)
	}
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "workstream", Project: f.project, Workstream: asking}, Mode: "soft", Source: "owner"})
	f.awaitMasonRan(t, third, "resume")
	masons.Check(t)
}

func TestMasonAsksAgainInItsAnswerTurn(t *testing.T) {
	t.Parallel()
	p := &faults{}
	passed, prepare := countingSchedule()
	f, fakes, c := newAskingMasonFixturePrepared(t, 4, independentPlan, p, prepare)
	defer f.stop(t)
	c.Release("1")
	c.Release("2")
	f.engine.Mu.Lock()
	f.engine.Turns[masonTurnID("resume")] = fakes.Asking(p, "", "1")
	f.engine.Mu.Unlock()
	f.answer("1", asks(p, "2"))
	f.answer("2", func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) error { return nil })
	stream := f.seedBuilding(t, "design", independentPlan)
	f.awaitMasonTransitions(t, stream, 5)

	awaitPasses(t, passed, 3)
	p.Check(t)
	fakes.Check(t)
	want := []transitionMove{started("resume", f.startedReason(t, stream, "resume")), parked("resume", "1"), resumed("resume", "1"), parked("resume", "2"), resumed("resume", "2")}
	if got := masonTransitions(t, f, stream); !reflect.DeepEqual(got, want) {
		t.Fatalf("mason transitions %+v, want %+v", got, want)
	}
	f.checkUnits(t, stream, []UnitStatus{{Unit: "resume", State: UnitImplementing}, f.deferred(t, stream, "dedupe", overlapping("resume"))})
	if runs := implementationRuns(fakes, stream); len(runs) != 1 {
		t.Fatalf("implementation turns %d", len(runs))
	}

	b, found, err := (&masons{s: f.s, repository: f.repository()}).read(stream)
	must(t, err)
	for _, tr := range allTransitions(t, f.s.cfg.Root, f.project, stream) {
		if tr.ID == masonTransitionID("resume") && (!found || !b.Started().Equal(tr.At)) {
			t.Fatalf("the workstream last started a unit at %s, want %s", b.Started(), tr.At)
		}
	}
}

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

func queueFollowingTurn(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, agent, unit, turn string) {
	t.Helper()
	intakefixture.QueueFollowingTurn(t, f.project, f.clock.Now(), repository, stream, agent, unit, turn)
}

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
