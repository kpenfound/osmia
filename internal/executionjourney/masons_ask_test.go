package executionjourney

import (
	context "context"
	fmt "fmt"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	os "os"
	filepath "path/filepath"
	reflect "reflect"
	slices "slices"
	strings "strings"
	sync "sync"
	testing "testing"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	questions "github.com/kpenfound/osmia/internal/questions"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func newAskingMasonFixture(t *testing.T, masons int, drafted string, p *intakefixture.Faults) (*shedFixture, *intakefixture.FakeMasons, *intakefixture.Chief) {
	t.Helper()
	return newAskingMasonFixturePrepared(t, masons, drafted, p, nil)
}

func newAskingMasonFixturePrepared(t *testing.T, masons int, drafted string, p *intakefixture.Faults, prepare func(*Options)) (*shedFixture, *intakefixture.FakeMasons, *intakefixture.Chief) {
	t.Helper()
	f, fake := newMasonFixturePrepared(t, masons, drafted, prepare)
	c := &intakefixture.Chief{P: p, Released: map[string]bool{}, Held: map[string]chan struct{}{}}
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
	intakefixture.Soon(t, fmt.Sprintf("%d mason transitions of %s", n, stream), func() bool {
		return len(masonTransitions(t, f, stream)) >= n
	})
}

func TestMasonQuestionParksTheUnitUntilTheAnswerArrives(t *testing.T) {
	t.Parallel()
	p := &intakefixture.Faults{}
	passed, schedule := countingSchedule()

	prepare := func(opts *Options) {
		schedule(opts)
		appendConfig(t, opts, "\n[mason]\nmax_clean_turns = 20\n")
	}
	f, masons, _ := newAskingMasonFixturePrepared(t, 1, independentPlan, p, prepare)
	defer func() { f.stop(t) }()
	factory := runtime.Target{Scope: "factory"}
	built := f.seedBuildingPaused(t, factory, independentPlan, "first", "second", "third")
	a, b, c := built[0], built[1], built[2]

	ids := []config.WorkstreamID{a, b, c}
	slices.Sort(ids)
	asking, other, third := ids[0], ids[1], ids[2]

	answerQueued, finished := make(chan struct{}), make(chan struct{})
	continued := func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		select {
		case <-answerQueued:
			select {
			case <-finished:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		default:
			select {
			case <-answerQueued:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return masons.Turn(ctx, req, turn, tools)
	}
	masons.Mu.Lock()
	for i := 4; i <= 6; i++ {
		masons.Play[fmt.Sprintf("%s-clarify-%d", intake.MasonAgent("resume"), i)] = intakefixture.FailMasonTurn
	}
	masons.Mu.Unlock()
	f.engine.Mu.Lock()
	for i := 1; i <= 6; i++ {
		f.engine.Turns[fmt.Sprintf("%s-continue-%d", intake.MasonAgent("resume"), i)] = continued
		f.engine.Turns[fmt.Sprintf("%s-recover-%d", intake.MasonAgent("resume"), i)] = continued
		if i > 3 {
			f.engine.Turns[fmt.Sprintf("%s-clarify-%d", intake.MasonAgent("resume"), i)] = masons.Turn
		}
	}
	f.engine.Mu.Unlock()

	var mu sync.Mutex
	var answered []agent.Request
	var during []string
	f.engine.Mu.Lock()
	f.engine.Turns[execution.MasonTurnID("resume")] = masons.Asking(p, asking, "1")
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

	intakefixture.Soon(t, "question 1 to be escalated", func() bool {
		asked, err := f.repository().Questions(asking)
		fixturekit.Must(t, err)
		return len(asked) == 1 && asked[0].State == trace.QuestionEscalated
	})
	f.awaitMasonTransitions(t, asking, 2)
	f.awaitMasonRan(t, other, "resume")

	intakefixture.AwaitPasses(t, passed, 3)
	p.Check(t)
	masons.Check(t)
	want := []intakefixture.TransitionMove{started("resume", f.startedReason(t, asking, "resume")), intakefixture.Parked("resume", "1")}
	if got := masonTransitions(t, f, asking); !reflect.DeepEqual(got, want) {
		t.Fatalf("mason transitions %+v, want %+v", got, want)
	}

	opened := "Question 1 is open, asked by the mason: " + intakefixture.AskedQuestion
	if body := f.notice(t, asking, "question_1_open"); body != opened {
		t.Fatalf("the question notice %q, want %q", body, opened)
	}
	f.checkUnits(t, asking, []api.UnitStatus{{Unit: "resume", State: intake.UnitWaiting}, f.deferred(t, asking, "dedupe", overlapping("resume"))})
	f.checkUnits(t, other, []api.UnitStatus{{Unit: "resume", State: intake.UnitImplementing}, f.deferred(t, other, "dedupe", overlapping("resume"))})
	f.checkUnits(t, third, []api.UnitStatus{f.deferred(t, third, "resume", slotless(1)), f.deferred(t, third, "dedupe", slotless(1))})
	q := f.question(t, asking, "1")
	if q.Asked.AskedBy.ID != intake.MasonAgent("resume") || q.Asked.Thread != intake.MasonAgent("resume") || q.Asked.Turn != execution.MasonTurnID("resume") || q.Asked.Unit != "resume" || q.Asked.Question != intakefixture.AskedQuestion {
		t.Fatalf("question %+v", q.Asked)
	}
	workspace := filepath.Join(f.opts.Config.Root, intake.UnitsDirectory, string(f.project), string(asking), "resume")
	if _, err := os.Stat(filepath.Join(workspace, fixturekit.MasonWrote)); err != nil {
		t.Fatalf("the parked unit's workspace lost the mason's work: %v", err)
	}

	f.stop(t)
	f.start(t)

	intakefixture.AwaitPasses(t, passed, 3)
	f.checkUnits(t, asking, []api.UnitStatus{{Unit: "resume", State: intake.UnitWaiting}, f.deferred(t, asking, "dedupe", overlapping("resume"))})
	if th := f.thread(t, asking, intake.MasonAgent("resume")); !th.Parked() || len(th.Turns) != 1 {
		t.Fatalf("the mason's thread after a restart: %+v", th)
	}
	if got := f.question(t, asking, "1").State; got != trace.QuestionEscalated {
		t.Fatalf("question 1 is %s after a restart", got)
	}

	f.rule(t, "1")
	f.awaitTurn(t, asking, intake.MasonAgent("resume"), questions.TurnID("1"))

	close(answerQueued)
	f.awaitCompleted(t, asking, intake.MasonAgent("resume"), questions.TurnID("1"))

	intakefixture.AwaitPasses(t, passed, 3)
	p.Check(t)
	want = append(want, intakefixture.Resumed("resume", "1"))
	if got := masonTransitions(t, f, asking); !reflect.DeepEqual(got, want) {
		t.Fatalf("mason transitions %+v, want %+v", got, want)
	}
	f.checkUnits(t, asking, []api.UnitStatus{{Unit: "resume", State: intake.UnitImplementing}, f.deferred(t, asking, "dedupe", overlapping("resume"))})
	mu.Lock()
	defer mu.Unlock()
	if len(answered) != 1 || !reflect.DeepEqual(during, []string{intake.UnitImplementing}) {
		t.Fatalf("answer turns %d, the unit was %q while they ran", len(answered), during)
	}
	if !strings.Contains(answered[0].Prompt, intakefixture.RelayedRuling) || !strings.Contains(answered[0].SystemPrompt, "You are a mason of the") {
		t.Fatalf("answer turn prompt %q, system prompt %q", answered[0].Prompt, answered[0].SystemPrompt)
	}
	for _, name := range []string{fixturekit.MasonWrote, "answered.go"} {
		if _, err := os.Stat(filepath.Join(workspace, name)); err != nil {
			t.Fatalf("the unit's workspace lacks %s: %v", name, err)
		}
	}
	if th := f.thread(t, asking, intake.MasonAgent("resume")); th.Turns[1].Request.Unit != "resume" || th.Turns[1].Status() != "idle" {
		t.Fatalf("answer turn %+v", th.Turns[1])
	}

	f.checkUnits(t, other, []api.UnitStatus{{Unit: "resume", State: intake.UnitImplementing}, f.deferred(t, other, "dedupe", overlapping("resume"))})
	f.checkUnits(t, third, []api.UnitStatus{f.deferred(t, third, "resume", slotless(1)), f.deferred(t, third, "dedupe", slotless(1))})
	mutation(t, f.c, "PUT", "pause", api.PauseRequest{Target: runtime.Target{Scope: "workstream", Project: f.project, Workstream: other}, Mode: "soft", Source: "owner"})

	intakefixture.AwaitPasses(t, passed, 3)
	if got := masonTransitions(t, f, third); len(got) != 0 {
		t.Fatalf("%s started a unit while one was implementing with one mason slot: %+v", third, got)
	}
	mutation(t, f.c, "PUT", "pause", api.PauseRequest{Target: runtime.Target{Scope: "workstream", Project: f.project, Workstream: asking}, Mode: "soft", Source: "owner"})

	close(finished)
	f.awaitMasonRan(t, third, "resume")
	masons.Check(t)
}

func TestMasonAsksAgainInItsAnswerTurn(t *testing.T) {
	t.Parallel()
	p := &intakefixture.Faults{}
	passed, prepare := countingSchedule()
	f, fakes, c := newAskingMasonFixturePrepared(t, 4, independentPlan, p, prepare)
	defer f.stop(t)
	c.Release("1")
	c.Release("2")
	f.engine.Mu.Lock()
	f.engine.Turns[execution.MasonTurnID("resume")] = fakes.Asking(p, "", "1")
	f.engine.Mu.Unlock()
	f.answer("1", intakefixture.Asks(p, "2"))
	f.answer("2", func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) error { return nil })
	stream := f.seedBuilding(t, "design", independentPlan)
	f.awaitMasonTransitions(t, stream, 5)

	intakefixture.AwaitPasses(t, passed, 3)
	p.Check(t)
	fakes.Check(t)
	want := []intakefixture.TransitionMove{started("resume", f.startedReason(t, stream, "resume")), intakefixture.Parked("resume", "1"), intakefixture.Resumed("resume", "1"), intakefixture.Parked("resume", "2"), intakefixture.Resumed("resume", "2")}
	if got := masonTransitions(t, f, stream); !reflect.DeepEqual(got, want) {
		t.Fatalf("mason transitions %+v, want %+v", got, want)
	}
	f.checkUnits(t, stream, []api.UnitStatus{{Unit: "resume", State: intake.UnitImplementing}, f.deferred(t, stream, "dedupe", overlapping("resume"))})
	if runs := implementationRuns(fakes, stream); len(runs) != 1 {
		t.Fatalf("implementation turns %d", len(runs))
	}

	b, found, err := (&masons{s: f.s, repository: f.repository()}).read(stream)
	fixturekit.Must(t, err)
	for _, tr := range intakefixture.AllTransitions(t, f.s.cfg.Root, f.project, stream) {
		if tr.ID == execution.MasonTransitionID("resume") && (!found || !b.Started().Equal(tr.At)) {
			t.Fatalf("the workstream last started a unit at %s, want %s", b.Started(), tr.At)
		}
	}
}

func askOnLatestTurn(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, agent, role, unit, text string) string {
	t.Helper()
	ctx := context.Background()
	th, err := repository.Thread(stream, agent)
	fixturekit.Must(t, err)
	turn := th.Turns[len(th.Turns)-1].Request.TurnID
	token := "asked-" + turn
	directory := filepath.Join(f.s.cfg.Root.String(), "threads", string(f.project), string(stream), agent, token)
	q, err := repository.ClaimTurn(ctx, stream, agent, token, directory, f.clock.Now())
	fixturekit.Must(t, err)
	scope := coreadapter.Scope{Project: string(f.project), Workstream: string(stream), Unit: unit, Thread: agent, Turn: turn, Role: role}
	asked, err := repository.Ask(ctx, agent, scope, text, f.clock.Now())
	fixturekit.Must(t, err)
	h := q.Request.Header
	h.Schema, h.ID, h.At = "osmia.trace.turn-response", trace.EventID(q.Request.ID, "response"), f.clock.Now()
	response := trace.TurnResponse{Header: h, AgentID: agent, ThreadID: agent, TurnID: turn, RequestID: q.Request.ID, RequestRevision: q.Request.Revision,
		Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: q.Request.Profile.Backend, ID: token}, SessionDirectory: directory, StartedAt: q.Claim.At,
			Outcome: &coreadapter.Outcome{Status: "waiting", Report: "Asked question " + asked.ID}}}
	fixturekit.Must(t, repository.CaptureTurn(ctx, q.Claim.Token, response))
	fixturekit.Must(t, repository.CompleteTurn(ctx, stream, agent, turn, q.Claim.Token, f.clock.Now()))
	return asked.ID
}

func queueFollowingTurn(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, agent, unit, turn string) {
	t.Helper()
	intakefixture.QueueFollowingTurn(t, f.project, f.clock.Now(), repository, stream, agent, unit, turn)
}

func waitingTransitions(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string) []string {
	t.Helper()
	transitions, err := trace.Read[trace.Transition](repository, stream)
	fixturekit.Must(t, err)
	var ids []string
	for _, tr := range transitions {
		if tr.Subject == trace.UnitSubject(unit) && tr.To == intake.UnitWaiting {
			ids = append(ids, tr.ID)
		}
	}
	return ids
}

func TestAQuestionAnEarlierTurnAskedDoesNotParkTheUnit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, _ := newMasonFixture(t, 1, independentPlan)
	base := strings.TrimSpace(fixturekit.DemoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	stream, repository := seedBuild(t, f, "overtaken", independentPlan, config.WorkspacesGit, base)
	m := newMasonController(f.s, repository)
	b, found, err := m.read(stream)
	fixturekit.Must(t, err)
	if !found {
		t.Fatal("fixture workstream is not building")
	}
	if started, blocked, err := m.start(ctx, b, "resume"); err != nil || !started || blocked {
		t.Fatalf("unit resume did not start: started %v, blocked %v, %v", started, blocked, err)
	}
	agent := intake.MasonAgent("resume")
	askOnLatestTurn(t, f, repository, stream, agent, intake.MasonRole, "resume", "Should resume keep partial uploads?")
	queueFollowingTurn(t, f, repository, stream, agent, "resume", agent+"-move-1")

	state, err := repository.Workflow(stream, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	if value, err := m.follow(ctx, stream, "resume", state); err != nil || value != intake.UnitImplementing {
		t.Fatalf("follow with the asking turn overtaken left the unit %q (%v), want implementing", value, err)
	}
	if waits := waitingTransitions(t, repository, stream, "resume"); len(waits) != 0 {
		t.Fatalf("an overtaken question parked the unit: %v", waits)
	}

	asked := askOnLatestTurn(t, f, repository, stream, agent, intake.MasonRole, "resume", "Which store holds the offsets?")
	state, err = repository.Workflow(stream, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	if value, err := m.follow(ctx, stream, "resume", state); err != nil || value != intake.UnitWaiting {
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
	fixturekit.Must(t, err)
	fixturekit.Must(t, r.one(ctx, stream, "resume", state, false))
	agent := intake.ReviewerAgent("resume")
	askOnLatestTurn(t, f, repository, stream, agent, intake.ReviewerRole, "resume", "Is the retry bound part of the acceptance?")
	queueFollowingTurn(t, f, repository, stream, agent, "resume", agent+"-move-1")

	state, err = repository.Workflow(stream, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	if value, err := r.followQuestion(ctx, stream, "resume", state); err != nil || value != intake.UnitReviewing {
		t.Fatalf("followQuestion with the asking turn overtaken left the unit %q (%v), want reviewing", value, err)
	}
	if waits := waitingTransitions(t, repository, stream, "resume"); len(waits) != 0 {
		t.Fatalf("an overtaken question parked the unit: %v", waits)
	}

	asked := askOnLatestTurn(t, f, repository, stream, agent, intake.ReviewerRole, "resume", "Does the bound cover restarts?")
	state, err = repository.Workflow(stream, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	if value, err := r.followQuestion(ctx, stream, "resume", state); err != nil || value != intake.UnitWaiting {
		t.Fatalf("followQuestion after the latest turn asked question %s left the unit %q (%v), want waiting", asked, value, err)
	}
	if waits, want := waitingTransitions(t, repository, stream, "resume"), []string{trace.UnitSubject("resume") + "-reviewer-waiting-" + asked}; !reflect.DeepEqual(waits, want) {
		t.Fatalf("waiting transitions %v, want %v", waits, want)
	}
}
