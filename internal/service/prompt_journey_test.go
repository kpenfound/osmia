package service

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/climb"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	kindreg "github.com/kpenfound/osmia/internal/kinds"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// planningAdvice is operating-policy text about units, plans and unit sizing.
// A kind without units never reads it.
var planningAdvice = []string{"### Plans and units", "size each unit so one mason turn can complete it", "One large unit is valid", "connecting the feature's parts", "### Acceptance and the check gate"}

// policyRevisions makes the operating policy of every turn end with a section
// that names the revision now in force, so a turn shows which policy it was
// given. It stands for a deploy that changed the policy while sessions were
// open, and is restored when the test ends.
func policyRevisions(t *testing.T) *atomic.Int32 {
	t.Helper()
	revision := &atomic.Int32{}
	revision.Store(1)
	previous := factoryInstructions
	factoryInstructions = func(scope coreadapter.Scope, kind kindreg.Kind) string {
		return previous(scope, kind) + fmt.Sprintf("\n\n### Policy revision %d\nThis section changes whenever the operating policy does.\n", revision.Load())
	}
	t.Cleanup(func() { factoryInstructions = previous })
	return revision
}

func journeyTurn(t *testing.T, log *fixturekit.PromptLog, name string) fixturekit.LoggedTurn {
	t.Helper()
	turn, ok := log.Turn(name)
	if !ok {
		t.Fatalf("the engine never ran turn %q; it ran %v", name, turnNames(log))
	}
	return turn
}

func firstReview(t *testing.T, log *fixturekit.PromptLog, prefix string) string {
	t.Helper()
	reviews := log.Named(prefix)
	if len(reviews) == 0 {
		t.Fatalf("the unit was never reviewed: %v", turnNames(log))
	}
	return reviews[0].Name
}

func turnNames(log *fixturekit.PromptLog) []string {
	var names []string
	for _, turn := range log.Turns() {
		names = append(names, turn.Name)
	}
	return names
}

// holdsPolicy checks that a turn read the operating policy at revision in its
// newest turn and never in its system prompt, which holds no revision marker.
func holdsPolicy(t *testing.T, turn fixturekit.LoggedTurn, revision int32) {
	t.Helper()
	if !strings.Contains(turn.Prompt, "## Operating policy") || !strings.Contains(turn.Prompt, "### Authority") {
		t.Errorf("%s: the newest turn carries no operating policy:\n%s", turn.Name, turn.Prompt)
	}
	for r := int32(1); r <= 5; r++ {
		marker := fmt.Sprintf("### Policy revision %d\n", r)
		if got := strings.Contains(turn.Prompt, marker); got != (r == revision) {
			t.Errorf("%s: the newest turn holds %q: %v, want %v", turn.Name, marker, got, r == revision)
		}
	}
	if strings.Contains(turn.SystemPrompt, "Operating policy") || strings.Contains(turn.SystemPrompt, "Policy revision") {
		t.Errorf("%s: the system prompt carries the operating policy", turn.Name)
	}
}

// resumesWith checks that resumed picked up the backend session first began and
// was sent the system prompt first began with, while its newest turn carries
// the later policy revision.
func resumesWith(t *testing.T, first, resumed fixturekit.LoggedTurn, revision int32) {
	t.Helper()
	if first.ResumeID != "" || resumed.ResumeID != "session-"+first.Name {
		t.Errorf("%s resumed %q after %s resumed %q, want the session %s began", resumed.Name, resumed.ResumeID, first.Name, first.ResumeID, "session-"+first.Name)
	}
	if resumed.SystemPrompt != first.SystemPrompt {
		t.Errorf("%s was sent a different system prompt than the session it resumed began with:\n%s\n--- began with\n%s", resumed.Name, resumed.SystemPrompt, first.SystemPrompt)
	}
	holdsPolicy(t, resumed, revision)
}

func absent(t *testing.T, turn fixturekit.LoggedTurn, texts ...string) {
	t.Helper()
	for _, text := range texts {
		if strings.Contains(turn.Prompt+"\n"+turn.SystemPrompt, text) {
			t.Errorf("%s carries %q:\n%s", turn.Name, text, turn.Prompt)
		}
	}
}

func present(t *testing.T, turn fixturekit.LoggedTurn, texts ...string) {
	t.Helper()
	for _, text := range texts {
		if !strings.Contains(turn.Prompt+"\n"+turn.SystemPrompt, text) {
			t.Errorf("%s lacks %q:\n%s\n%s", turn.Name, text, turn.SystemPrompt, turn.Prompt)
		}
	}
}

// TestFeatureAndHillClimbTurnsReceiveTheirRolesPromptsWithPolicyOfTheirKind
// runs a feature workstream through the architect, the committee, a mason
// and a reviewer, and a Hill Climb workstream through its architect and
// committee, in a running service. The operating policy is revised while the
// sessions of the feature workstream are open.
func TestFeatureAndHillClimbTurnsReceiveTheirRolesPromptsWithPolicyOfTheirKind(t *testing.T) {
	revision := policyRevisions(t)
	ctx := context.Background()

	f := newDebateFixtureWith(t, 1, 2, intakefixture.MasonRoles, func(opts *Options) {
		opts.WriteTimeout = time.Minute
		opts.reviewChecks = fixturekit.PassingChecks
		enforced := Enforce(*opts, Enforcement{Engine: opts.Committee.Engine, Hosts: opts.Committee.Hosts})
		opts.Threads, opts.controls = enforced.Threads, enforced.controls
		opts.Threads = intakefixture.ConfigureMasonThreads(t, opts.Config.Root, config.WorkspacesGit, intakefixture.MasonCapacity(1), opts.Threads)
	})
	defer f.stop(t)
	f.engine.Mu.Lock()
	f.engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error { return nil }
	f.engine.Mu.Unlock()
	log := fixturekit.NewPromptLog(f.engine)
	f.script("draft-1-1", map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: intakefixture.IndependentPlan}, nil)
	f.upstream(t)
	masons := intakefixture.ScriptMasons(f.engine, masonTurnID)

	faults := &intakefixture.Faults{}
	objection := shed.ObjectionID(1, intake.CommitteeAgent(1), 1)
	f.member(1, 1, 1, func(ctx context.Context, req agent.Request, v *agent.Turn, tools *mcp.ClientSession) error {
		defer revision.Store(2)
		return intakefixture.Objects(faults, shed.Fit, "plan", "spec#1")(ctx, req, v, tools)
	})
	f.script(intake.ReplyTurnID(1, 1), map[string]string{plan.PlanPath: fixturekit.ValidPlan}, func(ctx context.Context, req agent.Request, v *agent.Turn, tools *mcp.ClientSession) error {
		return intakefixture.Answers(faults, "The plan holds.", objection)(ctx, req, v, tools)
	})
	f.member(2, 1, 1, intakefixture.Concedes(faults, objection))

	unit := "resume"
	implement, clarify := masonTurnID(unit), intake.MasonAgent(unit)+"-clarify-1"
	masons.Response = map[string]string{implement: "Work is ongoing."}
	masons.Play[implement] = func(context.Context, agent.Request, *mcp.ClientSession) error {
		revision.Store(3)
		return nil
	}
	masons.Play[clarify] = intakefixture.ReportDone("Uploads resume at the last acknowledged chunk")

	reviewing := intake.ReviewerAgent(unit) + "-review-"
	f.engine.Mu.Lock()
	manager := f.engine.Turns["*"]
	f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if !strings.HasPrefix(req.Name, reviewing) {
			return manager(ctx, req, verified, tools)
		}
		revision.Store(4)
		if err := intakefixture.Asks(faults, "1")(ctx, req, verified, tools); err != nil {
			return nil, err
		}
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Asked", SessionDir: req.SessionDir, NumTurns: 1}, nil
	}
	f.engine.Mu.Unlock()
	answering := intakefixture.Answer(f.engine, "1", func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		body, err := fixturekit.CallTool(ctx, tools, "verdict", map[string]any{"decision": "satisfactory", "summary": fixturekit.ReviewSummary, "findings": []any{}})
		if err != nil || !strings.Contains(body, `"recorded":true`) {
			return fmt.Errorf("verdict %s: %v", body, err)
		}
		return nil
	})

	stream := f.handIn(t, "journey", fixturekit.HandedDesign)
	f.awaitShed(t, stream, "concluded-2")
	faults.Check(t)
	if _, err := f.c.Ratify(ctx, stream, 1, 2); err != nil {
		t.Fatal(err)
	}
	intakefixture.Rule(t, f.c, "1")
	intakefixture.Soon(t, "the reviewer's answer turn to approve the candidate", func() bool {
		state, err := f.repository().Workflow(stream, trace.UnitSubject(unit))
		fixturekit.Must(t, err)
		return state.Value == intake.UnitApproved
	})
	masons.Check(t)

	draft, reply := journeyTurn(t, log, "draft-1-1"), journeyTurn(t, log, intake.ReplyTurnID(1, 1))
	round1, round2 := journeyTurn(t, log, intake.RoundTurnID(1, intake.CommitteeAgent(1), 1)), journeyTurn(t, log, intake.RoundTurnID(2, intake.CommitteeAgent(1), 1))
	implementing, clarifying := journeyTurn(t, log, implement), journeyTurn(t, log, clarify)
	review, answer := journeyTurn(t, log, firstReview(t, log, reviewing)), journeyTurn(t, log, answering)

	holdsPolicy(t, draft, 1)
	holdsPolicy(t, round1, 1)
	holdsPolicy(t, implementing, 2)
	holdsPolicy(t, review, 3)
	resumesWith(t, draft, reply, 2)
	resumesWith(t, round1, round2, 2)
	resumesWith(t, implementing, clarifying, 3)
	resumesWith(t, review, answer, 4)
	present(t, draft, "architect of the", "### Plans and units", "size each unit so one mason turn can complete it")
	present(t, round1, "member of the committee", "### Plans and units", "### Acceptance and the check gate")
	present(t, implementing, "mason of the", "Consult neighboring assignments")
	present(t, review, "unit reviewer", "rebase of the candidate onto a moved feature branch")
	for _, turn := range []fixturekit.LoggedTurn{draft, reply, round1, round2, implementing, clarifying, review, answer} {
		if turn.Profile == "" || len(turn.Tools) == 0 {
			t.Errorf("%s ran with profile %q and tools %v", turn.Name, turn.Profile, turn.Tools)
		}
	}

	hill := hillClimbPromptTurns(t, revision)
	architect, committee, mason, reviewer := hill.turns[0], hill.turns[1], hill.turns[2], hill.turns[3]
	for _, turn := range hill.turns {
		holdsPolicy(t, turn, 4)
		absent(t, turn, planningAdvice...)
	}
	absent(t, mason, "Consult neighboring assignments")
	absent(t, reviewer, "rebase of the candidate onto a moved feature branch")
	present(t, architect, "architect of the", kindreg.ClimbSpecPath)
	present(t, committee, "climb#metric")
	present(t, mason, "mason of the")
	present(t, reviewer, "unit reviewer")

	all := append(log.Texts(), hill.log.Texts()...)
	results := fixturekit.ToolResults(t, f.repository(), stream)
	if len(results) == 0 || len(hill.results) == 0 {
		t.Fatalf("the trace holds %d tool results of the feature workstream and %d of the climb", len(results), len(hill.results))
	}
	all = append(all, results...)
	all = append(all, hill.results...)
	if len(all) < 40 {
		t.Fatalf("the journey's agents received only %d texts", len(all))
	}
	fixturekit.AssertNamesNoSkill(t, all)
	fixturekit.AssertMasonsAreNotToldTheyOwnTheCode(t, append(log.Turns(), hill.log.Turns()...), "mason")
}

type hillClimbTurns struct {
	log     *fixturekit.PromptLog
	turns   []fixturekit.LoggedTurn
	results []fixturekit.LoggedText
}

// hillClimbPromptTurns hands a Hill Climb workstream in, has its climb spec
// drafted, debated and ratified, and waits for its evaluator unit to be built
// and reviewed. It returns the turns of the architect, the committee, the
// evaluator unit's mason and its reviewer. The operating policy stays at
// revision 4.
func hillClimbPromptTurns(t *testing.T, revision *atomic.Int32) hillClimbTurns {
	t.Helper()
	climbing := newClimbFixture(t, newEvaluatorEngine(100), false, nil)
	defer climbing.stop(t)
	log := fixturekit.NewPromptLog(climbing.shedFixture.engine)
	stream := climbing.ratified(t)
	intakefixture.Soon(t, "the evaluator unit to be reviewed", func() bool {
		_, ok := climbing.skills.of("evaluator-reviewer")
		return ok
	})
	reviewing := log.Named(intake.ReviewerAgent(climb.EvaluatorUnitID) + "-review-")
	if len(reviewing) == 0 {
		t.Fatalf("the evaluator unit was never reviewed: %v", turnNames(log))
	}
	if got := revision.Load(); got != 4 {
		t.Fatalf("the policy revision is %d", got)
	}
	var out hillClimbTurns
	out.log = log
	out.turns = []fixturekit.LoggedTurn{
		journeyTurn(t, log, "draft-1-1"),
		journeyTurn(t, log, intake.RoundTurnID(1, intake.CommitteeAgent(1), 1)),
		journeyTurn(t, log, masonTurnID(climb.EvaluatorUnitID)),
		reviewing[0],
	}
	out.results = fixturekit.ToolResults(t, climbing.repository(), stream)
	return out
}
