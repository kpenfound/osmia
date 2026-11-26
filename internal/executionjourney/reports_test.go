package executionjourney

import (
	context "context"
	json "encoding/json"
	errors "errors"
	fmt "fmt"
	os "os"
	exec "os/exec"
	filepath "path/filepath"
	reflect "reflect"
	slices "slices"
	strings "strings"
	atomic "sync/atomic"
	testing "testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func reports(f *journey.ShedFixture, t *testing.T, stream config.WorkstreamID, unit string) []trace.Document {
	t.Helper()
	docs, err := trace.Read[trace.Document](f.Repository(), stream)
	fixturekit.Must(t, err)
	return slices.DeleteFunc(docs, func(d trace.Document) bool { return d.ID != execution.ReportDocument(unit) })
}

func checkCandidate(f *journey.ShedFixture, t *testing.T, stream config.WorkstreamID, report execution.UnitReport) {
	t.Helper()
	git := func(args ...string) string {
		return strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(f.Clone), append([]string{"-C", f.Clone}, args...)...))
	}
	if tip := git("rev-parse", execution.UnitBranch(stream, report.Unit)); report.Candidate != tip {
		t.Fatalf("candidate %s, the unit's branch is at %s", report.Candidate, tip)
	}
	if feature := git("rev-parse", intake.FeatureBranch(stream)); report.Base != feature || git("rev-parse", report.Candidate+"^") != feature {
		t.Fatalf("candidate %s on base %s, feature branch at %s", report.Candidate, report.Base, feature)
	}
	if got := git("show", report.Candidate+":"+fixturekit.MasonWrote); got != "package trace" {
		t.Fatalf("the candidate holds %q", got)
	}
}

func TestMasonDoneMovesTheUnitToReviewing(t *testing.T) {
	t.Parallel()
	f, masons := journey.NewMasonFixture(t, 4, intakefixture.IndependentPlan)
	defer func() { f.Stop(t) }()
	refused := []struct {
		args   map[string]any
		reason string
	}{
		{map[string]any{"outcome": " "}, "outcome is required: say what the unit's work now does and how you checked its acceptance"},
	}
	masons.Play[execution.MasonTurnID("resume")] = func(ctx context.Context, _ agent.Request, tools *mcp.ClientSession) error {
		for _, r := range refused {
			recorded, reason, err := intakefixture.Done(ctx, tools, r.args)
			if err != nil || recorded || reason != r.reason {
				return fmt.Errorf("done %v: recorded %t, reason %q, want %q (%v)", r.args, recorded, reason, r.reason, err)
			}
		}

		for _, args := range []map[string]any{
			{"outcome": "approved", "state": intake.UnitApproved},
			{"outcome": "Built", "criteria": []any{map[string]any{"criterion": "spec#1"}}},
			{"outcome": 1},
			{"outcome": "Built", "learnings": []string{"Uses chunks."}},
		} {
			if text, err := fixturekit.CallTool(ctx, tools, execution.DoneTool, args); err == nil {
				return fmt.Errorf("done %v is no tool error: %s", args, text)
			}
		}
		if recorded, reason, err := intakefixture.Done(ctx, tools, map[string]any{"outcome": "approved", "headline": " Uploads   resume ", "happened": " The storage client resumes uploads from the last chunk. ", "needs_you": " Review the candidate. "}); err != nil || !recorded {
			return fmt.Errorf("complete report: reason %q (%v)", reason, err)
		}
		const again = "this turn already reported its unit done; end the turn"
		if recorded, reason, err := intakefixture.Done(ctx, tools, map[string]any{"outcome": "again"}); err != nil || recorded || reason != again {
			return fmt.Errorf("second done: recorded %t, reason %q (%v)", recorded, reason, err)
		}
		return nil
	}
	masons.Play[execution.MasonTurnID("dedupe")] = func(ctx context.Context, _ agent.Request, tools *mcp.ClientSession) error {
		if recorded, reason, err := intakefixture.Done(ctx, tools, map[string]any{"outcome": "Built"}); err != nil || !recorded {
			return fmt.Errorf("complete report: reason %q (%v)", reason, err)
		}
		return intakefixture.ErrFailTurn
	}
	stream := seedBuilding(f, t, "design", intakefixture.IndependentPlan)
	f.AwaitUnit(t, stream, "resume", intake.UnitReviewing)

	f.AwaitUnit(t, stream, "dedupe", intake.UnitReviewing)
	masons.Check(t)
	f.CheckUnits(t, stream, []api.UnitStatus{{Unit: "resume", State: intake.UnitReviewing, Card: &intakefixture.ExampleCard}, {Unit: "dedupe", State: intake.UnitReviewing, Card: &intakefixture.ExampleCard}})

	th, err := f.Repository().Thread(stream, intake.MasonAgent("resume"))
	fixturekit.Must(t, err)
	if len(th.Turns) != 1 {
		t.Fatalf("mason turns %+v", th.Turns)
	}
	turn := th.Turns[0]
	wantReport := service.MasonReport{Outcome: "approved"}
	var reported service.MasonReport
	if outcome := turn.Response.Result.Outcome; turn.Status() != "idle" || outcome == nil || outcome.Status != execution.MasonDone || outcome.Card == nil || *outcome.Card != intakefixture.ExampleCard || json.Unmarshal([]byte(outcome.Report), &reported) != nil || !reflect.DeepEqual(reported, wantReport) {
		t.Fatalf("the mason's turn ended %s with %+v", turn.Status(), turn.Response.Result.Outcome)
	}

	docs := reports(f, t, stream, "resume")
	if len(docs) != 1 {
		t.Fatalf("reports %+v", docs)
	}
	doc := docs[0]
	if doc.Revision != 1 || doc.Path != "units/resume/report.json" || doc.Unit != "resume" || doc.Actor != intake.MasonActor || doc.Cause != turn.Response.ID {
		t.Fatalf("report record %+v", doc.Header)
	}
	var report execution.UnitReport
	fixturekit.Must(t, json.Unmarshal([]byte(doc.Content), &report))
	checkCandidate(f, t, stream, report)
	want := execution.UnitReport{Unit: "resume", Turn: execution.MasonTurnID("resume"), Seal: 1, Outcome: "approved", Card: &intakefixture.ExampleCard, Branch: execution.UnitBranch(stream, "resume"), Base: report.Base, Candidate: report.Candidate}
	if !reflect.DeepEqual(report, want) {
		t.Fatalf("report %+v, want %+v", report, want)
	}
	status, err := f.C.Status(context.Background(), stream)
	fixturekit.Must(t, err)
	if len(status.Units) != 2 || status.Units[0].Card == nil || *status.Units[0].Card != intakefixture.ExampleCard || status.Units[1].Card == nil || *status.Units[1].Card != intakefixture.ExampleCard {
		t.Fatalf("unit cards in status: %+v", status.Units)
	}
	var apiErr api.WorkstreamStatus
	fixturekit.Must(t, f.C.Do(context.Background(), "GET", service.Prefix+"/status/"+string(stream), nil, &apiErr))
	if !reflect.DeepEqual(apiErr.Units, status.Units) {
		t.Fatalf("unit cards through API: %+v", apiErr.Units)
	}
	if data, err := fixturekit.WorkstreamDocument(t, f.S.Cfg.Root, f.Project, stream, "units/resume/report.json"); err != nil || string(data) != doc.Content {
		t.Fatalf("report file %q %v", data, err)
	}

	reason := fmt.Sprintf("the mason of unit resume reported done on turn %s; its candidate is %s on %s, from %s at %s, and its report is units/resume/report.json revision 1", execution.MasonTurnID("resume"), report.Candidate, execution.UnitBranch(stream, "resume"), intake.FeatureBranch(stream), report.Base)
	checking := intakefixture.TransitionMove{ID: execution.CheckingTransitionID("resume", 1), Subject: trace.UnitSubject("resume"), From: intake.UnitImplementing, To: execution.UnitChecking, Cause: turn.Response.ID, Reason: reason}
	f.AwaitUnit(t, stream, "dedupe", intake.UnitReviewing)
	dedupeChecking := masonTransitions(t, f, stream)[3]
	if dedupeChecking.ID != execution.CheckingTransitionID("dedupe", 1) || dedupeChecking.To != execution.UnitChecking {
		t.Fatalf("dedupe's move to checking %+v", dedupeChecking)
	}
	if got, want := masonTransitions(t, f, stream), []intakefixture.TransitionMove{started("resume", startedReason(f, t, stream, "resume")), checking, started("dedupe", startedReason(f, t, stream, "dedupe")), dedupeChecking}; !reflect.DeepEqual(got, want) {
		t.Fatalf("mason transitions %+v, want %+v", got, want)
	}

	reviewNotice := fmt.Sprintf("Unit resume is checking: its mason reported done on turn %s; its report is units/resume/report.json revision 1. Headline: Uploads resume; Needs you: Review the candidate.", execution.MasonTurnID("resume"))
	if body := f.Notice(t, stream, execution.CheckingTransitionID("resume", 1)); body != reviewNotice {
		t.Fatalf("the checking notice %q, want %q", body, reviewNotice)
	}
	f.AwaitEventTurns(t, stream, reviewNotice, "Unit dedupe is implementing: its mason works on it in its unit workspace on "+execution.UnitBranch(stream, "dedupe")+".")
	awaitAcknowledgedNotices(f, t, stream)

	th, err = f.Repository().Thread(stream, intake.MasonAgent("dedupe"))
	fixturekit.Must(t, err)
	failed := th.Turns[0]
	if len(th.Turns) != 1 || failed.Status() != "failed" || failed.Response.Result.Outcome == nil || failed.Response.Result.Outcome.Status != execution.MasonDone {
		t.Fatalf("the failing mason's turns %+v", th.Turns)
	}
	if got := reports(f, t, stream, "dedupe"); len(got) != 1 || got[0].Cause != failed.Response.ID {
		t.Fatalf("the failed turn's report %+v", got)
	}

	scope := coreadapter.Scope{Project: string(f.Project), Workstream: string(stream), Unit: "resume", Thread: intake.MasonAgent("resume"), Turn: "late", Role: intake.MasonRole}
	raw, err := json.Marshal(map[string]any{"outcome": "Built"})
	fixturekit.Must(t, err)
	out, err := (&execution.MasonReports{}).Tool(f.Repository(), scope).Handle(context.Background(), raw)
	fixturekit.Must(t, err)
	if string(out) != `{"recorded":false,"reason":"unit resume is reviewing, not implementing: done is accepted only while the unit is implementing"}` {
		t.Fatalf("late done %s", out)
	}

	f.Stop(t)
	repo, err := trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
	fixturekit.Must(t, err)
	controller := journey.NewMasonController(f.S, repo)
	b, found, err := controller.Read(stream)
	if err != nil || !found {
		t.Fatalf("read %v %v", found, err)
	}
	subject := trace.UnitSubject("resume")
	b.States()[subject] = trace.WorkflowState{Version: b.States()[subject].Version - 1, Value: intake.UnitImplementing}
	moved, blocked, err := controller.Finish(context.Background(), b, "resume")
	if moved || blocked || err != nil {
		t.Fatalf("finish on a stale read: moved %t, blocked %t, %v", moved, blocked, err)
	}
	docs, err = trace.Read[trace.Document](repo, stream)
	fixturekit.Must(t, errors.Join(err, repo.Close()))
	if n := len(slices.DeleteFunc(docs, func(d trace.Document) bool { return d.ID != execution.ReportDocument("resume") })); n != 1 {
		t.Fatalf("a stale finish recorded a report: %d revisions", n)
	}
	f.Start(t)
}

func TestUnitCandidateFailureKeepsItImplementing(t *testing.T) {
	t.Parallel()
	var passed atomic.Int64
	f, masons := journey.NewMasonFixtureWith(t, config.WorkspacesGit, "masons = 1\n", intakefixture.IndependentPlan, func(opts *journey.Options) {
		opts.SchedulePassed = func() { passed.Add(1) }
	})
	defer f.Stop(t)
	factory := runtime.Target{Scope: "factory"}
	built := seedBuildingPaused(f, t, factory, intakefixture.IndependentPlan, "first", "second")
	blocked, other := intakefixture.LowHigh(built[0], built[1])
	var lock string
	masons.Play[execution.MasonTurnID("resume")] = func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) error {
		if intakefixture.SessionStream(req) != string(blocked) {
			return nil
		}
		w, found, err := intake.Workspaces(f.S.Cfg, intake.UnitsDirectory, config.WorkspacesGit).Workspace(ctx, execution.UnitName(blocked, "resume"))
		if err != nil || !found {
			return fmt.Errorf("the unit's workspace: %t %v", found, err)
		}
		out, err := exec.Command("git", "-C", w.Path, "rev-parse", "--absolute-git-dir").CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %w", out, err)
		}
		lock = filepath.Join(strings.TrimSpace(string(out)), "index.lock")
		if err := os.WriteFile(lock, nil, 0600); err != nil {
			return err
		}
		return intakefixture.ReportDone("Built")(ctx, req, tools)
	}

	intakefixture.AwaitPasses(t, &passed, 3)
	intakefixture.Mutation(t, f.C, "DELETE", "pause", factory)
	f.AwaitMasonRan(t, other, "resume")
	intakefixture.AwaitPasses(t, &passed, 3)
	intakefixture.Settled(t, func() int { return len(blocks(f, t, blocked, "resume")) })
	masons.Check(t)
	const prefix = "unit resume stays implementing: its mason reported done, and its candidate cannot be made: git add: "
	if got := blocks(f, t, blocked, "resume"); len(got) != 1 || !strings.HasPrefix(got[0], prefix) || !strings.Contains(got[0], "index.lock") {
		t.Fatalf("blocked %q", got)
	}
	f.CheckUnits(t, blocked, []api.UnitStatus{{Unit: "resume", State: intake.UnitImplementing}, deferred(f, t, blocked, "dedupe", api.UnitDispatch{Reason: execution.DeferBlocked, Blocked: "resume", Message: "Waits while the mason controller is blocked on unit resume of this workstream."})})
	if got := reports(f, t, blocked, "resume"); len(got) != 0 {
		t.Fatalf("reports %+v", got)
	}
	if _, err := f.Repository().Thread(blocked, intake.MasonAgent("dedupe")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the workstream started another unit: %v", err)
	}

	masons.Mu.Lock()
	err := os.Remove(lock)
	masons.Mu.Unlock()
	fixturekit.Must(t, err)
	f.AwaitUnit(t, blocked, "resume", intake.UnitReviewing)
	docs := reports(f, t, blocked, "resume")
	if len(docs) != 1 {
		t.Fatalf("reports %+v", docs)
	}
	var report execution.UnitReport
	fixturekit.Must(t, json.Unmarshal([]byte(docs[0].Content), &report))
	checkCandidate(f, t, blocked, report)
}
