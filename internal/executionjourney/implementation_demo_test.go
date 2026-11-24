package executionjourney

import (
	context "context"
	json "encoding/json"
	errors "errors"
	os "os"
	filepath "path/filepath"
	reflect "reflect"
	strings "strings"
	testing "testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"

	agent "github.com/kpenfound/agentcore/agent"
	questions "github.com/kpenfound/osmia/internal/questions"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSequentialImplementation(t *testing.T) {
	t.Parallel()
	p := &intakefixture.Faults{}
	f, fake, _ := newAskingMasonFixture(t, 1, fixturekit.ValidPlan, p)
	defer f.Stop(t)
	factory := runtime.Target{Scope: "factory"}
	f.Engine.Mu.Lock()
	f.Engine.Turns[execution.MasonTurnID("resume")] = fake.Asking(p, "", "1")
	f.Engine.Mu.Unlock()
	stream := builtPaused(f, t, factory, "sequential")[0]
	awaitDispatches(f, t, stream, "resume", 1)
	f.CheckUnits(t, stream, []api.UnitStatus{deferred(f, t, stream, "resume", factoryPaused), {Unit: "dedupe", State: intake.UnitPlanned}})
	if got := masonTransitions(t, f, stream); len(got) != 0 {
		t.Fatalf("mason started while the factory was paused: %+v", got)
	}
	intakefixture.Mutation(t, f.C, "DELETE", "pause", factory)

	awaitMasonTransitions(f, t, stream, 2)
	f.AwaitQuestion(t, stream, "1", trace.QuestionEscalated)
	p.Check(t)
	fake.Check(t)
	f.CheckUnits(t, stream, []api.UnitStatus{{Unit: "resume", State: intake.UnitWaiting}, {Unit: "dedupe", State: intake.UnitPlanned}})
	if got, want := masonTransitions(t, f, stream), []intakefixture.TransitionMove{started("resume", startedReason(f, t, stream, "resume")), intakefixture.Parked("resume", "1")}; !reflect.DeepEqual(got, want) {
		t.Fatalf("transitions before answer: %+v, want %+v", got, want)
	}
	first := fake.Requests(stream)
	if len(first) != 1 || !strings.Contains(first[0].Prompt, "# Unit resume\n") || !strings.Contains(first[0].Prompt, "seal: 1\n") || !strings.Contains(first[0].Prompt, "## Acceptance\n- spec#1 holds, shown by TestResume (new-test)") || strings.Contains(first[0].Prompt, "# Unit dedupe") {
		t.Fatalf("mason bundle or dispatch: %+v", first)
	}
	workspace := filepath.Join(f.Opts.Config.Root, intake.UnitsDirectory, string(f.Project), string(stream), "resume")
	if len(first) == 1 && first[0].Workspace.Directory() == workspace {
		t.Fatal("the mason ran in the worktree, not an isolated file view")
	}
	if branch := strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(f.Clone), "-C", workspace, "branch", "--show-current")); branch != execution.UnitBranch(stream, "resume") {
		t.Fatalf("unit branch %q", branch)
	}
	if data, err := os.ReadFile(filepath.Join(workspace, fixturekit.MasonWrote)); err != nil || string(data) != "package trace\n" {
		t.Fatalf("parked workspace lost the mason's work: %q %v", data, err)
	}
	startNotice := "Unit resume is implementing: its mason works on it in its unit workspace on " + execution.UnitBranch(stream, "resume") + "."
	f.AwaitEventTurns(t, stream, startNotice, "Question 1 is open, asked by the mason: "+intakefixture.AskedQuestion)
	awaitAcknowledgedNotices(f, t, stream)

	f.Answer("1", func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		if !strings.Contains(req.Prompt, intakefixture.RelayedRuling) {
			return errors.New("the answer turn lacks the ruling")
		}
		if req.Workspace.Directory() == workspace {
			return errors.New("the answer turn ran in the worktree")
		}
		if _, err := os.Stat(filepath.Join(req.Workspace.Directory(), fixturekit.MasonWrote)); err != nil {
			return err
		}
		recorded, reason, err := intakefixture.Done(ctx, tools, map[string]any{"outcome": "Uploads resume at the last acknowledged chunk"})
		if err != nil || !recorded {
			return errors.New("done refused: " + reason)
		}
		return nil
	})
	f.Rule(t, "1")
	f.AwaitTurn(t, stream, intake.MasonAgent("resume"), questions.TurnID("1"))
	f.AwaitUnit(t, stream, "resume", intake.UnitReviewing)
	p.Check(t)
	fake.Check(t)
	f.CheckUnits(t, stream, []api.UnitStatus{{Unit: "resume", State: intake.UnitReviewing, Card: &intakefixture.ExampleCard}, {Unit: "dedupe", State: intake.UnitPlanned}})
	if got := f.Question(t, stream, "1").State; got != trace.QuestionAnswered {
		t.Fatalf("question state %s", got)
	}
	thread := f.Thread(t, stream, intake.MasonAgent("resume"))
	if len(thread.Turns) != 2 || thread.Turns[1].Status() != "idle" || thread.Turns[1].Response.Result.Outcome == nil || thread.Turns[1].Response.Result.Outcome.Status != execution.MasonDone || thread.Turns[1].Response.Result.Outcome.Card == nil || *thread.Turns[1].Response.Result.Outcome.Card != intakefixture.ExampleCard {
		t.Fatalf("mason thread %+v", thread.Turns)
	}
	docs := reports(f, t, stream, "resume")
	if len(docs) != 1 || docs[0].Revision != 1 || docs[0].Path != "units/resume/report.json" || docs[0].Cause != thread.Turns[1].Response.ID {
		t.Fatalf("report records %+v", docs)
	}
	var report execution.UnitReport
	if len(docs) == 1 {
		fixturekit.Must(t, json.Unmarshal([]byte(docs[0].Content), &report))
	}
	checkCandidate(f, t, stream, report)
	wantReport := execution.UnitReport{Unit: "resume", Turn: questions.TurnID("1"), Seal: 1, Outcome: "Uploads resume at the last acknowledged chunk", Card: &intakefixture.ExampleCard, Branch: execution.UnitBranch(stream, "resume"), Base: report.Base, Candidate: report.Candidate}
	if !reflect.DeepEqual(report, wantReport) {
		t.Fatalf("report %+v, want %+v", report, wantReport)
	}
	transitions := masonTransitions(t, f, stream)
	if len(transitions) != 4 || !reflect.DeepEqual(transitions[:3], []intakefixture.TransitionMove{started("resume", startedReason(f, t, stream, "resume")), intakefixture.Parked("resume", "1"), intakefixture.Resumed("resume", "1")}) || transitions[3].ID != execution.CheckingTransitionID("resume", 1) || transitions[3].From != intake.UnitImplementing || transitions[3].To != execution.UnitChecking || transitions[3].Cause != thread.Turns[1].Response.ID {
		t.Fatalf("mason transitions %+v", transitions)
	}
	finishNotice := "Unit resume is checking: its mason reported done on turn " + questions.TurnID("1") + "; its report is units/resume/report.json revision 1. Headline: Uploads resume; Needs you: Review the candidate."
	f.AwaitEventTurns(t, stream, finishNotice)
	awaitAcknowledgedNotices(f, t, stream)
	if got := reports(f, t, stream, "dedupe"); len(got) != 0 {
		t.Fatalf("dependent unit has a report: %+v", got)
	}
	if _, err := f.Repository().Thread(stream, intake.MasonAgent("dedupe")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dependent unit has a mason thread: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.Opts.Config.Root, intake.UnitsDirectory, string(f.Project), string(stream), "dedupe")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dependent unit has a workspace: %v", err)
	}
}
