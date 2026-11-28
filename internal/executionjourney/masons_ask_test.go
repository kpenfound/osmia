package executionjourney

import (
	context "context"
	fmt "fmt"
	os "os"
	filepath "path/filepath"
	reflect "reflect"
	slices "slices"
	strings "strings"
	sync "sync"
	testing "testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	questions "github.com/kpenfound/osmia/internal/questions"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func newAskingMasonFixture(t *testing.T, masons int, drafted string, p *intakefixture.Faults) (*journey.ShedFixture, *intakefixture.FakeMasons, *intakefixture.Manager) {
	t.Helper()
	return newAskingMasonFixturePrepared(t, masons, drafted, p, nil)
}

func newAskingMasonFixturePrepared(t *testing.T, masons int, drafted string, p *intakefixture.Faults, prepare func(*journey.Options)) (*journey.ShedFixture, *intakefixture.FakeMasons, *intakefixture.Manager) {
	t.Helper()
	f, fake := journey.NewMasonFixturePrepared(t, masons, drafted, prepare)
	c := &intakefixture.Manager{P: p, Released: map[string]bool{}, Held: map[string]chan struct{}{}}
	f.Engine.Mu.Lock()
	defer f.Engine.Mu.Unlock()
	f.Engine.Turns["*"] = c.Turn
	return f, fake, c
}

func awaitMasonTransitions(f *journey.ShedFixture, t *testing.T, stream config.WorkstreamID, n int) {
	t.Helper()
	intakefixture.Soon(t, fmt.Sprintf("%d mason transitions of %s", n, stream), func() bool {
		return len(masonTransitions(t, f, stream)) >= n
	})
}

func TestMasonQuestionParksTheUnitUntilTheAnswerArrives(t *testing.T) {
	t.Parallel()
	p := &intakefixture.Faults{}
	passed, schedule := journey.CountingSchedule()

	prepare := func(opts *journey.Options) {
		schedule(opts)
		journey.AppendConfig(t, opts, "\n[mason]\nmax_clean_turns = 20\n")
	}
	f, masons, _ := newAskingMasonFixturePrepared(t, 1, intakefixture.IndependentPlan, p, prepare)
	defer func() { f.Stop(t) }()
	factory := runtime.Target{Scope: "factory"}
	built := seedBuildingPaused(f, t, factory, intakefixture.IndependentPlan, "first", "second", "third")
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
	f.Engine.Mu.Lock()
	for i := 1; i <= 6; i++ {
		f.Engine.Turns[fmt.Sprintf("%s-continue-%d", intake.MasonAgent("resume"), i)] = continued
		f.Engine.Turns[fmt.Sprintf("%s-recover-%d", intake.MasonAgent("resume"), i)] = continued
		if i > 3 {
			f.Engine.Turns[fmt.Sprintf("%s-clarify-%d", intake.MasonAgent("resume"), i)] = masons.Turn
		}
	}
	f.Engine.Mu.Unlock()

	var mu sync.Mutex
	var answered []agent.Request
	var during []string
	f.Engine.Mu.Lock()
	f.Engine.Turns[execution.MasonTurnID("resume")] = masons.Asking(p, asking, "1")
	f.Engine.Mu.Unlock()
	f.Answer("1", func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		state, err := f.UnitState(asking, "resume")
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
	intakefixture.Mutation(t, f.C, "DELETE", "pause", factory)

	intakefixture.Soon(t, "question 1 to be escalated", func() bool {
		asked, err := f.Repository().Questions(asking)
		fixturekit.Must(t, err)
		return len(asked) == 1 && asked[0].State == trace.QuestionEscalated
	})
	awaitMasonTransitions(f, t, asking, 2)
	f.AwaitMasonRan(t, other, "resume")

	intakefixture.AwaitPasses(t, passed, 3)
	p.Check(t)
	masons.Check(t)
	want := []intakefixture.TransitionMove{started("resume", startedReason(f, t, asking, "resume")), intakefixture.Parked("resume", "1")}
	if got := masonTransitions(t, f, asking); !reflect.DeepEqual(got, want) {
		t.Fatalf("mason transitions %+v, want %+v", got, want)
	}

	opened := "Question 1 is open, asked by the mason: " + intakefixture.AskedQuestion
	if body := f.Notice(t, asking, "question_1_open"); body != opened {
		t.Fatalf("the question notice %q, want %q", body, opened)
	}
	f.CheckUnits(t, asking, []api.UnitStatus{{Unit: "resume", State: intake.UnitWaiting}, deferred(f, t, asking, "dedupe", overlapping("resume"))})
	f.CheckUnits(t, other, []api.UnitStatus{{Unit: "resume", State: intake.UnitImplementing}, deferred(f, t, other, "dedupe", overlapping("resume"))})
	f.CheckUnits(t, third, []api.UnitStatus{deferred(f, t, third, "resume", slotless(1)), deferred(f, t, third, "dedupe", slotless(1))})
	q := f.Question(t, asking, "1")
	if q.Asked.AskedBy.ID != intake.MasonAgent("resume") || q.Asked.Thread != intake.MasonAgent("resume") || q.Asked.Turn != execution.MasonTurnID("resume") || q.Asked.Unit != "resume" || q.Asked.Question != intakefixture.AskedQuestion {
		t.Fatalf("question %+v", q.Asked)
	}
	workspace := filepath.Join(f.Opts.Config.Root, intake.UnitsDirectory, string(f.Project), string(asking), "resume")
	if _, err := os.Stat(filepath.Join(workspace, fixturekit.MasonWrote)); err != nil {
		t.Fatalf("the parked unit's workspace lost the mason's work: %v", err)
	}

	f.Stop(t)
	f.Start(t)

	intakefixture.AwaitPasses(t, passed, 3)
	f.CheckUnits(t, asking, []api.UnitStatus{{Unit: "resume", State: intake.UnitWaiting}, deferred(f, t, asking, "dedupe", overlapping("resume"))})
	if th := f.Thread(t, asking, intake.MasonAgent("resume")); !th.Parked() || len(th.Turns) != 1 {
		t.Fatalf("the mason's thread after a restart: %+v", th)
	}
	if got := f.Question(t, asking, "1").State; got != trace.QuestionEscalated {
		t.Fatalf("question 1 is %s after a restart", got)
	}

	f.Rule(t, "1")
	f.AwaitTurn(t, asking, intake.MasonAgent("resume"), questions.TurnID("1"))

	close(answerQueued)
	awaitCompleted(f, t, asking, intake.MasonAgent("resume"), questions.TurnID("1"))

	intakefixture.AwaitPasses(t, passed, 3)
	p.Check(t)
	want = append(want, intakefixture.Resumed("resume", "1"))
	if got := masonTransitions(t, f, asking); !reflect.DeepEqual(got, want) {
		t.Fatalf("mason transitions %+v, want %+v", got, want)
	}
	f.CheckUnits(t, asking, []api.UnitStatus{{Unit: "resume", State: intake.UnitImplementing}, deferred(f, t, asking, "dedupe", overlapping("resume"))})
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
	if th := f.Thread(t, asking, intake.MasonAgent("resume")); th.Turns[1].Request.Unit != "resume" || th.Turns[1].Status() != "idle" {
		t.Fatalf("answer turn %+v", th.Turns[1])
	}

	f.CheckUnits(t, other, []api.UnitStatus{{Unit: "resume", State: intake.UnitImplementing}, deferred(f, t, other, "dedupe", overlapping("resume"))})
	f.CheckUnits(t, third, []api.UnitStatus{deferred(f, t, third, "resume", slotless(1)), deferred(f, t, third, "dedupe", slotless(1))})
	intakefixture.Mutation(t, f.C, "PUT", "pause", api.PauseRequest{Target: runtime.Target{Scope: "workstream", Project: f.Project, Workstream: other}, Mode: "soft", Source: "owner"})

	intakefixture.AwaitPasses(t, passed, 3)
	if got := masonTransitions(t, f, third); len(got) != 0 {
		t.Fatalf("%s started a unit while one was implementing with one mason slot: %+v", third, got)
	}
	intakefixture.Mutation(t, f.C, "PUT", "pause", api.PauseRequest{Target: runtime.Target{Scope: "workstream", Project: f.Project, Workstream: asking}, Mode: "soft", Source: "owner"})

	close(finished)
	f.AwaitMasonRan(t, third, "resume")
	masons.Check(t)
}

func TestMasonAsksAgainInItsAnswerTurn(t *testing.T) {
	t.Parallel()
	p := &intakefixture.Faults{}
	passed, prepare := journey.CountingSchedule()
	f, fakes, c := newAskingMasonFixturePrepared(t, 4, intakefixture.IndependentPlan, p, prepare)
	defer f.Stop(t)
	c.Release("1")
	c.Release("2")
	f.Engine.Mu.Lock()
	f.Engine.Turns[execution.MasonTurnID("resume")] = fakes.Asking(p, "", "1")
	f.Engine.Mu.Unlock()
	f.Answer("1", intakefixture.Asks(p, "2"))
	f.Answer("2", func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) error { return nil })
	stream := seedBuilding(f, t, "design", intakefixture.IndependentPlan)
	awaitMasonTransitions(f, t, stream, 5)

	intakefixture.AwaitPasses(t, passed, 3)
	p.Check(t)
	fakes.Check(t)
	want := []intakefixture.TransitionMove{started("resume", startedReason(f, t, stream, "resume")), intakefixture.Parked("resume", "1"), intakefixture.Resumed("resume", "1"), intakefixture.Parked("resume", "2"), intakefixture.Resumed("resume", "2")}
	if got := masonTransitions(t, f, stream); !reflect.DeepEqual(got, want) {
		t.Fatalf("mason transitions %+v, want %+v", got, want)
	}
	f.CheckUnits(t, stream, []api.UnitStatus{{Unit: "resume", State: intake.UnitImplementing}, deferred(f, t, stream, "dedupe", overlapping("resume"))})
	if runs := implementationRuns(fakes, stream); len(runs) != 1 {
		t.Fatalf("implementation turns %d", len(runs))
	}

	b, found, err := (&journey.Masons{S: f.S, Repository: f.Repository()}).Read(stream)
	fixturekit.Must(t, err)
	for _, tr := range intakefixture.AllTransitions(t, f.S.Cfg.Root, f.Project, stream) {
		if tr.ID == execution.MasonTransitionID("resume") && (!found || !b.Started().Equal(tr.At)) {
			t.Fatalf("the workstream last started a unit at %s, want %s", b.Started(), tr.At)
		}
	}
}

func askOnLatestTurn(t *testing.T, f *journey.ShedFixture, repository *trace.Repository, stream config.WorkstreamID, agent, role, unit, text string) string {
	t.Helper()
	ctx := context.Background()
	th, err := repository.Thread(stream, agent)
	fixturekit.Must(t, err)
	turn := th.Turns[len(th.Turns)-1].Request.TurnID
	token := "asked-" + turn
	directory := filepath.Join(f.S.Cfg.Root.String(), "threads", string(f.Project), string(stream), agent, token)
	q, err := repository.ClaimTurn(ctx, stream, agent, token, directory, f.Clock.Now())
	fixturekit.Must(t, err)
	scope := coreadapter.Scope{Project: string(f.Project), Workstream: string(stream), Unit: unit, Thread: agent, Turn: turn, Role: role}
	asked, err := repository.Ask(ctx, agent, scope, text, f.Clock.Now())
	fixturekit.Must(t, err)
	h := q.Request.Header
	h.Schema, h.ID, h.At = "osmia.trace.turn-response", trace.EventID(q.Request.ID, "response"), f.Clock.Now()
	response := trace.TurnResponse{Header: h, AgentID: agent, ThreadID: agent, TurnID: turn, RequestID: q.Request.ID, RequestRevision: q.Request.Revision,
		Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: q.Request.Profile.Backend, ID: token}, SessionDirectory: directory, StartedAt: q.Claim.At,
			Outcome: &coreadapter.Outcome{Status: "waiting", Report: "Asked question " + asked.ID}}}
	fixturekit.Must(t, repository.CaptureTurn(ctx, q.Claim.Token, response))
	fixturekit.Must(t, repository.CompleteTurn(ctx, stream, agent, turn, q.Claim.Token, f.Clock.Now()))
	return asked.ID
}

func queueFollowingTurn(t *testing.T, f *journey.ShedFixture, repository *trace.Repository, stream config.WorkstreamID, agent, unit, turn string) {
	t.Helper()
	intakefixture.QueueFollowingTurn(t, f.Project, f.Clock.Now(), repository, stream, agent, unit, turn)
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
	f, _ := journey.NewMasonFixture(t, 1, intakefixture.IndependentPlan)
	base := strings.TrimSpace(fixturekit.DemoGit(t, f.Clone, "-C", f.Clone, "rev-parse", "HEAD"))
	stream, repository := journey.SeedBuild(t, f, "overtaken", intakefixture.IndependentPlan, config.WorkspacesGit, base)
	m := journey.NewMasonController(f.S, repository)
	b, found, err := m.Read(stream)
	fixturekit.Must(t, err)
	if !found {
		t.Fatal("fixture workstream is not building")
	}
	if started, blocked, err := m.Start(ctx, b, "resume"); err != nil || !started || blocked {
		t.Fatalf("unit resume did not start: started %v, blocked %v, %v", started, blocked, err)
	}
	agent := intake.MasonAgent("resume")
	askOnLatestTurn(t, f, repository, stream, agent, intake.MasonRole, "resume", "Should resume keep partial uploads?")
	queueFollowingTurn(t, f, repository, stream, agent, "resume", agent+"-move-1")

	state, err := repository.Workflow(stream, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	if value, err := m.Driver().Follow(ctx, stream, "resume", state); err != nil || value != intake.UnitImplementing {
		t.Fatalf("follow with the asking turn overtaken left the unit %q (%v), want implementing", value, err)
	}
	if waits := waitingTransitions(t, repository, stream, "resume"); len(waits) != 0 {
		t.Fatalf("an overtaken question parked the unit: %v", waits)
	}

	asked := askOnLatestTurn(t, f, repository, stream, agent, intake.MasonRole, "resume", "Which store holds the offsets?")
	state, err = repository.Workflow(stream, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	if value, err := m.Driver().Follow(ctx, stream, "resume", state); err != nil || value != intake.UnitWaiting {
		t.Fatalf("follow after the latest turn asked question %s left the unit %q (%v), want waiting", asked, value, err)
	}
	if waits, want := waitingTransitions(t, repository, stream, "resume"), []string{trace.UnitSubject("resume") + "-waiting-" + asked}; !reflect.DeepEqual(waits, want) {
		t.Fatalf("waiting transitions %v, want %v", waits, want)
	}
}

func TestAQuestionAnEarlierReviewTurnAskedDoesNotParkTheUnit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repository := journey.NewReviewFixture(t, "overtaken-review")
	r := &journey.Reviewers{Masons: journey.NewMasonController(f.S, repository)}
	state, err := repository.Workflow(stream, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	fixturekit.Must(t, r.One(ctx, stream, "resume", state, false))
	agent := intake.ReviewerAgent("resume")
	askOnLatestTurn(t, f, repository, stream, agent, intake.ReviewerRole, "resume", "Is the retry bound part of the acceptance?")
	queueFollowingTurn(t, f, repository, stream, agent, "resume", agent+"-move-1")

	state, err = repository.Workflow(stream, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	if value, err := r.Driver().FollowQuestion(ctx, stream, "resume", state); err != nil || value != intake.UnitReviewing {
		t.Fatalf("followQuestion with the asking turn overtaken left the unit %q (%v), want reviewing", value, err)
	}
	if waits := waitingTransitions(t, repository, stream, "resume"); len(waits) != 0 {
		t.Fatalf("an overtaken question parked the unit: %v", waits)
	}

	asked := askOnLatestTurn(t, f, repository, stream, agent, intake.ReviewerRole, "resume", "Does the bound cover restarts?")
	state, err = repository.Workflow(stream, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	if value, err := r.Driver().FollowQuestion(ctx, stream, "resume", state); err != nil || value != intake.UnitWaiting {
		t.Fatalf("followQuestion after the latest turn asked question %s left the unit %q (%v), want waiting", asked, value, err)
	}
	if waits, want := waitingTransitions(t, repository, stream, "resume"), []string{trace.UnitSubject("resume") + "-reviewer-waiting-" + asked}; !reflect.DeepEqual(waits, want) {
		t.Fatalf("waiting transitions %v, want %v", waits, want)
	}
}
