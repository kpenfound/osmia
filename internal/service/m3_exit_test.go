package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestM3SequentialImplementation demonstrates the M3 build through the local
// API with a fake mason. See docs/m3-exit.md.
func TestM3SequentialImplementation(t *testing.T) {
	t.Parallel()
	p := &faults{}
	f, fake, _ := newAskingMasonFixture(t, 1, validPlan, p)
	defer f.stop(t)
	factory := runtime.Target{Scope: "factory"}
	f.engine.mu.Lock()
	f.engine.turns[masonTurnID("resume")] = fake.asking(p, "", "1")
	f.engine.mu.Unlock()
	stream := f.builtPaused(t, factory, "sequential")[0]
	f.awaitDispatches(t, stream, "resume", 1)
	f.checkUnits(t, stream, []UnitStatus{f.deferred(t, stream, "resume", factoryPaused), {Unit: "dedupe", State: UnitPlanned}})
	if got := masonTransitions(t, f, stream); len(got) != 0 {
		t.Fatalf("mason started while the factory was paused: %+v", got)
	}
	mutation(t, f.c, "DELETE", "pause", factory)

	f.awaitMasonTransitions(t, stream, 2)
	f.awaitQuestion(t, stream, "1", trace.QuestionEscalated)
	p.check(t)
	fake.check(t)
	f.checkUnits(t, stream, []UnitStatus{{Unit: "resume", State: UnitWaiting}, {Unit: "dedupe", State: UnitPlanned}})
	if got, want := masonTransitions(t, f, stream), []transitionMove{started("resume", f.startedReason(t, stream, "resume")), parked("resume", "1")}; !reflect.DeepEqual(got, want) {
		t.Fatalf("transitions before answer: %+v, want %+v", got, want)
	}
	first := fake.requests(stream)
	if len(first) != 1 || !strings.Contains(first[0].Prompt, "# Unit resume\n") || !strings.Contains(first[0].Prompt, "seal: 1\n") || !strings.Contains(first[0].Prompt, "proof: new-test TestResume") || strings.Contains(first[0].Prompt, "# Unit dedupe") {
		t.Fatalf("mason bundle or dispatch: %+v", first)
	}
	workspace := filepath.Join(f.opts.Config.Root, unitsDirectory, string(f.project), string(stream), "resume")
	if len(first) == 1 && first[0].Workspace.Directory() == workspace {
		t.Fatal("the mason ran in the worktree, not an isolated file view")
	}
	if branch := strings.TrimSpace(demoGit(t, filepath.Dir(f.clone), "-C", workspace, "branch", "--show-current")); branch != unitBranch(stream, "resume") {
		t.Fatalf("unit branch %q", branch)
	}
	if data, err := os.ReadFile(filepath.Join(workspace, masonWrote)); err != nil || string(data) != "package trace\n" {
		t.Fatalf("parked workspace lost the mason's work: %q %v", data, err)
	}
	startNotice := "Unit resume is implementing: its mason works on it in its unit workspace on " + unitBranch(stream, "resume") + "."
	f.awaitEventTurns(t, stream, startNotice, "Question 1 is open, asked by the mason: "+askedQuestion)
	f.awaitAcknowledgedNotices(t, stream)

	f.answer("1", func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		if !strings.Contains(req.Prompt, relayedRuling) {
			return errors.New("the answer turn lacks the ruling")
		}
		if req.Workspace.Directory() == workspace {
			return errors.New("the answer turn ran in the worktree")
		}
		if _, err := os.Stat(filepath.Join(req.Workspace.Directory(), masonWrote)); err != nil {
			return err
		}
		recorded, reason, err := done(ctx, tools, map[string]any{"outcome": "Uploads resume at the last acknowledged chunk", "criteria": []any{criterionArgs(resumeReport)}})
		if err != nil || !recorded {
			return errors.New("done refused: " + reason)
		}
		return nil
	})
	f.rule(t, "1")
	f.awaitTurn(t, stream, masonAgent("resume"), questions.TurnID("1"))
	f.awaitUnit(t, stream, "resume", UnitReviewing)
	p.check(t)
	fake.check(t)
	f.checkUnits(t, stream, []UnitStatus{{Unit: "resume", State: UnitReviewing, Card: &exampleCard}, {Unit: "dedupe", State: UnitPlanned}})
	if got := f.question(t, stream, "1").State; got != trace.QuestionAnswered {
		t.Fatalf("question state %s", got)
	}
	thread := f.thread(t, stream, masonAgent("resume"))
	if len(thread.Turns) != 2 || thread.Turns[1].Status() != "idle" || thread.Turns[1].Response.Result.Outcome == nil || thread.Turns[1].Response.Result.Outcome.Status != masonDone || thread.Turns[1].Response.Result.Outcome.Card == nil || *thread.Turns[1].Response.Result.Outcome.Card != exampleCard {
		t.Fatalf("mason thread %+v", thread.Turns)
	}
	docs := f.reports(t, stream, "resume")
	if len(docs) != 1 || docs[0].Revision != 1 || docs[0].Path != "units/resume/report.json" || docs[0].Cause != thread.Turns[1].Response.ID {
		t.Fatalf("report records %+v", docs)
	}
	var report UnitReport
	if len(docs) == 1 {
		must(t, json.Unmarshal([]byte(docs[0].Content), &report))
	}
	f.checkCandidate(t, stream, report)
	wantReport := UnitReport{Unit: "resume", Turn: questions.TurnID("1"), Seal: 1, Outcome: "Uploads resume at the last acknowledged chunk", Criteria: []CriterionReport{resumeReport}, Card: &exampleCard, Branch: unitBranch(stream, "resume"), Base: report.Base, Candidate: report.Candidate}
	if !reflect.DeepEqual(report, wantReport) {
		t.Fatalf("report %+v, want %+v", report, wantReport)
	}
	transitions := masonTransitions(t, f, stream)
	if len(transitions) != 4 || !reflect.DeepEqual(transitions[:3], []transitionMove{started("resume", f.startedReason(t, stream, "resume")), parked("resume", "1"), resumed("resume", "1")}) || transitions[3].ID != reviewingTransitionID("resume", 1) || transitions[3].From != UnitImplementing || transitions[3].To != UnitReviewing || transitions[3].Cause != thread.Turns[1].Response.ID {
		t.Fatalf("mason transitions %+v", transitions)
	}
	finishNotice := "Unit resume is reviewing: its mason reported done on turn " + questions.TurnID("1") + "; its report is units/resume/report.json revision 1. Headline: Uploads resume; Needs you: Review the candidate."
	f.awaitEventTurns(t, stream, finishNotice)
	f.awaitAcknowledgedNotices(t, stream)
	if got := f.reports(t, stream, "dedupe"); len(got) != 0 {
		t.Fatalf("dependent unit has a report: %+v", got)
	}
	if _, err := f.repository().Thread(stream, masonAgent("dedupe")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dependent unit has a mason thread: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.opts.Config.Root, unitsDirectory, string(f.project), string(stream), "dedupe")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dependent unit has a workspace: %v", err)
	}
}
