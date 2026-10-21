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
	time "time"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

var resumeReport = "resume from the last chunk"

var exampleCard = intakefixture.ExampleCard

func newMasonController(s *Service, repository *trace.Repository) *masons {
	return &masons{s: s, cfg: s.cfg, repository: repository}
}

var done = intakefixture.Done

func reportDone(outcome string) func(context.Context, agent.Request, *mcp.ClientSession) error {
	return intakefixture.ReportDone(outcome)
}

func (f *shedFixture) awaitUnit(t *testing.T, stream config.WorkstreamID, unit, state string) {
	t.Helper()
	f.sharedTurns().AwaitUnit(t, stream, unit, state)
}

func (f *shedFixture) reports(t *testing.T, stream config.WorkstreamID, unit string) []trace.Document {
	t.Helper()
	docs, err := trace.Read[trace.Document](f.repository(), stream)
	must(t, err)
	return slices.DeleteFunc(docs, func(d trace.Document) bool { return d.ID != reportDocument(unit) })
}

func (f *shedFixture) checkCandidate(t *testing.T, stream config.WorkstreamID, report UnitReport) {
	t.Helper()
	git := func(args ...string) string {
		return strings.TrimSpace(demoGit(t, filepath.Dir(f.clone), append([]string{"-C", f.clone}, args...)...))
	}
	if tip := git("rev-parse", unitBranch(stream, report.Unit)); report.Candidate != tip {
		t.Fatalf("candidate %s, the unit's branch is at %s", report.Candidate, tip)
	}
	if feature := git("rev-parse", featureBranch(stream)); report.Base != feature || git("rev-parse", report.Candidate+"^") != feature {
		t.Fatalf("candidate %s on base %s, feature branch at %s", report.Candidate, report.Base, feature)
	}
	if got := git("show", report.Candidate+":"+masonWrote); got != "package trace" {
		t.Fatalf("the candidate holds %q", got)
	}
}

func TestMasonDoneMovesTheUnitToReviewing(t *testing.T) {
	t.Parallel()
	f, masons := newMasonFixture(t, 4, independentPlan)
	defer func() { f.stop(t) }()
	refused := []struct {
		args   map[string]any
		reason string
	}{
		{map[string]any{"outcome": " "}, "outcome is required: say what the unit's work now does and how you checked its acceptance"},
		{map[string]any{"outcome": "Built", "learnings": []string{" "}}, "learning 1 is empty"},
	}
	masons.Play[masonTurnID("resume")] = func(ctx context.Context, _ agent.Request, tools *mcp.ClientSession) error {
		missing, err := callTool(ctx, tools, doneTool, map[string]any{"outcome": "approved"})
		if err != nil || !strings.Contains(missing, `"recorded":false`) || !strings.Contains(missing, `headline is required`) {
			return fmt.Errorf("missing card: %q %v", missing, err)
		}
		for _, bad := range []struct{ field, value, reason string }{
			{"headline", " ", "headline is required"},
			{"happened", " ", "happened is required"},
			{"headline", strings.Repeat("x", 65), "headline must be at most 64 characters"},
			{"happened", "One\nTwo", "happened must be a single line"},
			{"happened", "Completed " + masonTurnID("resume"), "happened contains an Osmia or backend identifier"},
			{"needs_you", "See parser.go", "needs_you contains a file name"},
		} {
			args := map[string]any{"outcome": "approved", bad.field: bad.value}
			recorded, reason, err := done(ctx, tools, args)
			if err != nil || recorded || !strings.Contains(reason, bad.reason) {
				return fmt.Errorf("bad %s: recorded %t reason %q err %v", bad.field, recorded, reason, err)
			}
		}
		for _, r := range refused {
			recorded, reason, err := done(ctx, tools, r.args)
			if err != nil || recorded || reason != r.reason {
				return fmt.Errorf("done %v: recorded %t, reason %q, want %q (%v)", r.args, recorded, reason, r.reason, err)
			}
		}

		for _, args := range []map[string]any{
			{"outcome": "approved", "state": UnitApproved},
			{"outcome": "Built", "criteria": []any{map[string]any{"criterion": "spec#1"}}},
			{"outcome": 1},
		} {
			if text, err := callTool(ctx, tools, doneTool, args); err == nil {
				return fmt.Errorf("done %v is no tool error: %s", args, text)
			}
		}
		if recorded, reason, err := done(ctx, tools, map[string]any{"outcome": "approved", "headline": " Uploads   resume ", "happened": " The storage client resumes uploads from the last chunk. ", "needs_you": " Review the candidate. "}); err != nil || !recorded {
			return fmt.Errorf("complete report: reason %q (%v)", reason, err)
		}
		const again = "this turn already reported its unit done; end the turn"
		if recorded, reason, err := done(ctx, tools, map[string]any{"outcome": "again"}); err != nil || recorded || reason != again {
			return fmt.Errorf("second done: recorded %t, reason %q (%v)", recorded, reason, err)
		}
		return nil
	}
	masons.Play[masonTurnID("dedupe")] = func(ctx context.Context, _ agent.Request, tools *mcp.ClientSession) error {
		if recorded, reason, err := done(ctx, tools, map[string]any{"outcome": "Built"}); err != nil || !recorded {
			return fmt.Errorf("complete report: reason %q (%v)", reason, err)
		}
		return errFailTurn
	}
	stream := f.seedBuilding(t, "design", independentPlan)
	f.awaitUnit(t, stream, "resume", UnitReviewing)
	deadline := time.Now().Add(demoTimeout)
	for {
		th, err := f.repository().Thread(stream, masonAgent("dedupe"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if err == nil && len(th.Turns) > 0 && !th.Turns[0].CompletedAt.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the mason of unit dedupe never ran")
		}
		time.Sleep(50 * time.Millisecond)
	}
	f.awaitUnit(t, stream, "dedupe", UnitImplementing)
	masons.Check(t)
	f.checkUnits(t, stream, []UnitStatus{{Unit: "resume", State: UnitReviewing, Card: &exampleCard}, {Unit: "dedupe", State: UnitImplementing}})

	th, err := f.repository().Thread(stream, masonAgent("resume"))
	must(t, err)
	if len(th.Turns) != 1 {
		t.Fatalf("mason turns %+v", th.Turns)
	}
	turn := th.Turns[0]
	wantReport := MasonReport{Outcome: "approved"}
	var reported MasonReport
	if outcome := turn.Response.Result.Outcome; turn.Status() != "idle" || outcome == nil || outcome.Status != masonDone || outcome.Card == nil || *outcome.Card != exampleCard || json.Unmarshal([]byte(outcome.Report), &reported) != nil || !reflect.DeepEqual(reported, wantReport) {
		t.Fatalf("the mason's turn ended %s with %+v", turn.Status(), turn.Response.Result.Outcome)
	}

	docs := f.reports(t, stream, "resume")
	if len(docs) != 1 {
		t.Fatalf("reports %+v", docs)
	}
	doc := docs[0]
	if doc.Revision != 1 || doc.Path != "units/resume/report.json" || doc.Unit != "resume" || doc.Actor != masonActor || doc.Cause != turn.Response.ID {
		t.Fatalf("report record %+v", doc.Header)
	}
	var report UnitReport
	must(t, json.Unmarshal([]byte(doc.Content), &report))
	f.checkCandidate(t, stream, report)
	want := UnitReport{Unit: "resume", Turn: masonTurnID("resume"), Seal: 1, Outcome: "approved", Card: &exampleCard, Branch: unitBranch(stream, "resume"), Base: report.Base, Candidate: report.Candidate}
	if !reflect.DeepEqual(report, want) {
		t.Fatalf("report %+v, want %+v", report, want)
	}
	status, err := f.c.Status(context.Background(), stream)
	must(t, err)
	if len(status.Units) != 2 || status.Units[0].Card == nil || *status.Units[0].Card != exampleCard || status.Units[1].Card != nil {
		t.Fatalf("unit cards in status: %+v", status.Units)
	}
	var api WorkstreamStatus
	must(t, f.c.Do(context.Background(), "GET", Prefix+"/status/"+string(stream), nil, &api))
	if !reflect.DeepEqual(api.Units, status.Units) {
		t.Fatalf("unit cards through API: %+v", api.Units)
	}
	if data, err := workstreamDocument(t, f.s.cfg.Root, f.project, stream, "units/resume/report.json"); err != nil || string(data) != doc.Content {
		t.Fatalf("report file %q %v", data, err)
	}

	reason := fmt.Sprintf("the mason of unit resume reported done on turn %s; its candidate is %s on %s, from %s at %s, and its report is units/resume/report.json revision 1", masonTurnID("resume"), report.Candidate, unitBranch(stream, "resume"), featureBranch(stream), report.Base)
	checking := transitionMove{ID: checkingTransitionID("resume", 1), Subject: trace.UnitSubject("resume"), From: UnitImplementing, To: UnitChecking, Cause: turn.Response.ID, Reason: reason}
	if got, want := masonTransitions(t, f, stream), []transitionMove{started("resume", f.startedReason(t, stream, "resume")), checking, started("dedupe", f.startedReason(t, stream, "dedupe"))}; !reflect.DeepEqual(got, want) {
		t.Fatalf("mason transitions %+v, want %+v", got, want)
	}

	reviewNotice := fmt.Sprintf("Unit resume is checking: its mason reported done on turn %s; its report is units/resume/report.json revision 1. Headline: Uploads resume; Needs you: Review the candidate.", masonTurnID("resume"))
	if body := f.notice(t, stream, checkingTransitionID("resume", 1)); body != reviewNotice {
		t.Fatalf("the checking notice %q, want %q", body, reviewNotice)
	}
	f.awaitEventTurns(t, stream, reviewNotice, "Unit dedupe is implementing: its mason works on it in its unit workspace on "+unitBranch(stream, "dedupe")+".")
	f.awaitAcknowledgedNotices(t, stream)

	if got := f.reports(t, stream, "dedupe"); len(got) != 0 {
		t.Fatalf("the failed turn's report was recorded: %+v", got)
	}
	th, err = f.repository().Thread(stream, masonAgent("dedupe"))
	must(t, err)
	if turn := th.Turns[0]; turn.Status() != "failed" || turn.Response.Result.Outcome == nil || turn.Response.Result.Outcome.Status != masonDone {
		t.Fatalf("the failing mason's turn ended %s with %+v", turn.Status(), turn.Response.Result.Outcome)
	}

	scope := coreadapter.Scope{Project: string(f.project), Workstream: string(stream), Unit: "resume", Thread: masonAgent("resume"), Turn: "late", Role: masonRole}
	raw, err := json.Marshal(map[string]any{"outcome": "Built"})
	must(t, err)
	out, err := (&masonReports{}).Tool(f.repository(), scope).Handle(context.Background(), raw)
	must(t, err)
	if string(out) != `{"recorded":false,"reason":"unit resume is reviewing, not implementing"}` {
		t.Fatalf("late done %s", out)
	}

	f.stop(t)
	repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	controller := newMasonController(f.s, repo)
	b, found, err := controller.read(stream)
	if err != nil || !found {
		t.Fatalf("read %v %v", found, err)
	}
	subject := trace.UnitSubject("resume")
	b.States()[subject] = trace.WorkflowState{Version: b.States()[subject].Version - 1, Value: UnitImplementing}
	moved, blocked, err := controller.finish(context.Background(), b, "resume")
	if moved || blocked || err != nil {
		t.Fatalf("finish on a stale read: moved %t, blocked %t, %v", moved, blocked, err)
	}
	docs, err = trace.Read[trace.Document](repo, stream)
	must(t, errors.Join(err, repo.Close()))
	if n := len(slices.DeleteFunc(docs, func(d trace.Document) bool { return d.ID != reportDocument("resume") })); n != 1 {
		t.Fatalf("a stale finish recorded a report: %d revisions", n)
	}
	f.start(t)
}

func TestUnitCandidateFailureKeepsItImplementing(t *testing.T) {
	t.Parallel()
	var passed atomic.Int64
	f, masons := newMasonFixtureWith(t, config.WorkspacesGit, "masons = 1\n", independentPlan, "", func(opts *Options) {
		opts.schedulePassed = func() { passed.Add(1) }
	})
	defer f.stop(t)
	factory := runtime.Target{Scope: "factory"}
	built := f.seedBuildingPaused(t, factory, independentPlan, "first", "second")
	blocked, other := lowHigh(built[0], built[1])
	var lock string
	masons.Play[masonTurnID("resume")] = func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) error {
		if sessionStream(req) != string(blocked) {
			return nil
		}
		w, found, err := workspaces(f.s.cfg, unitsDirectory, config.WorkspacesGit).Workspace(ctx, unitName(blocked, "resume"))
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
		return reportDone("Built")(ctx, req, tools)
	}
	// Let a paused pass observe both builds before either can take the slot.
	awaitPasses(t, &passed, 3)
	mutation(t, f.c, "DELETE", "pause", factory)
	f.awaitMasonRan(t, other, "resume")
	awaitPasses(t, &passed, 3)
	settled(t, func() int { return len(f.blocks(t, blocked, "resume")) })
	masons.Check(t)
	const prefix = "unit resume stays implementing: its mason reported done, and its candidate cannot be made: git add: "
	if got := f.blocks(t, blocked, "resume"); len(got) != 1 || !strings.HasPrefix(got[0], prefix) || !strings.Contains(got[0], "index.lock") {
		t.Fatalf("blocked %q", got)
	}
	f.checkUnits(t, blocked, []UnitStatus{{Unit: "resume", State: UnitImplementing}, f.deferred(t, blocked, "dedupe", UnitDispatch{Reason: DeferBlocked, Blocked: "resume", Message: "Waits while the mason controller is blocked on unit resume of this workstream."})})
	if got := f.reports(t, blocked, "resume"); len(got) != 0 {
		t.Fatalf("reports %+v", got)
	}
	if _, err := f.repository().Thread(blocked, masonAgent("dedupe")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the workstream started another unit: %v", err)
	}

	masons.Mu.Lock()
	err := os.Remove(lock)
	masons.Mu.Unlock()
	must(t, err)
	f.awaitUnit(t, blocked, "resume", UnitReviewing)
	docs := f.reports(t, blocked, "resume")
	if len(docs) != 1 {
		t.Fatalf("reports %+v", docs)
	}
	var report UnitReport
	must(t, json.Unmarshal([]byte(docs[0].Content), &report))
	f.checkCandidate(t, blocked, report)
}
