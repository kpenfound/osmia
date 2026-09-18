package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// resumeReport is a complete report on the one criterion unit resume of
// independentPlan and validPlan addresses.
var resumeReport = CriterionReport{Criterion: "spec#1", Done: "resume from the last chunk", Evidence: "TestResume passes", Proof: "internal/trace/built_test.go TestResume"}

func criterionArgs(c CriterionReport) map[string]any {
	return map[string]any{"criterion": c.Criterion, "done": c.Done, "evidence": c.Evidence, "proof": c.Proof}
}

// done calls the done tool and returns whether the service accepted the
// report, and its reason when it did not.
func done(ctx context.Context, tools *mcp.ClientSession, args map[string]any) (bool, string, error) {
	text, err := callTool(ctx, tools, doneTool, args)
	if err != nil {
		return false, err.Error(), nil
	}
	var out struct {
		Recorded bool   `json:"recorded"`
		Reason   string `json:"reason"`
		Next     string `json:"next"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return false, "", fmt.Errorf("done returned %q: %w", text, err)
	}
	return out.Recorded, out.Reason, nil
}

// reportDone plays a mason turn that calls done once with a complete report
// whose outcome is outcome.
func reportDone(outcome string) func(context.Context, agent.Request, *mcp.ClientSession) error {
	return func(ctx context.Context, _ agent.Request, tools *mcp.ClientSession) error {
		recorded, reason, err := done(ctx, tools, map[string]any{"outcome": outcome, "criteria": []any{criterionArgs(resumeReport)}})
		if err != nil || !recorded {
			return fmt.Errorf("done refused: %q %v", reason, err)
		}
		return nil
	}
}

// awaitUnit waits until the unit of the workstream is in state.
func (f *shedFixture) awaitUnit(t *testing.T, stream config.WorkstreamID, unit, state string) {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for {
		got, err := f.repository().Workflow(stream, trace.UnitSubject(unit))
		must(t, err)
		if got.Value == state {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("unit %s of %s is %q, never %s", unit, stream, got.Value, state)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// reports returns the recorded revisions of the unit's report.
func (f *shedFixture) reports(t *testing.T, stream config.WorkstreamID, unit string) []trace.Document {
	t.Helper()
	docs, err := trace.Read[trace.Document](f.repository(), stream)
	must(t, err)
	return slices.DeleteFunc(docs, func(d trace.Document) bool { return d.ID != reportDocument(unit) })
}

// checkCandidate checks that the report's candidate is the tip of the
// unit's branch, holds what the fake mason wrote and sits on the feature
// branch commit the report names as its base, which is the feature branch's
// tip.
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

// A mason's done is refused, with the reason, for a report with no
// outcome, one that misses a criterion of the unit, names one the unit does
// not address or names one twice, or leaves out what was done, the
// evidence or the proof, and for an argument the tool does not take; the
// unit does not move and the turn does not fail. A complete report is
// accepted once per turn. Once the turn ends, the service snapshots the
// unit's workspace as its candidate, records the report with the candidate
// as units/<unit>/report.json and moves the unit to reviewing, whatever
// outcome the mason gave. That frees the workstream to start its next ready
// unit. A mason whose reports were all refused leaves its unit
// implementing.
func TestMasonDoneMovesTheUnitToReviewing(t *testing.T) {
	t.Parallel()
	f, masons := newMasonFixture(t, 4, independentPlan)
	defer f.stop(t)
	blank := resumeReport
	blank.Evidence = " "
	other := resumeReport
	other.Criterion = "spec#2"
	complete := []any{criterionArgs(resumeReport)}
	refused := []struct {
		args   map[string]any
		reason string
	}{
		{map[string]any{"outcome": " ", "criteria": complete}, "outcome is required: say what the unit's work now does"},
		{map[string]any{"outcome": "Built", "criteria": []any{}}, "the report misses spec#1: report on every criterion of unit resume"},
		{map[string]any{"outcome": "Built", "criteria": []any{criterionArgs(other)}}, `unit resume does not address criterion "spec#2"; report on spec#1 alone`},
		{map[string]any{"outcome": "Built", "criteria": []any{criterionArgs(resumeReport), criterionArgs(resumeReport)}}, "criterion spec#1 is reported twice"},
		{map[string]any{"outcome": "Built", "criteria": []any{criterionArgs(blank)}}, "criterion spec#1 has no evidence"},
	}
	masons.play[masonTurnID("resume")] = func(ctx context.Context, _ agent.Request, tools *mcp.ClientSession) error {
		for _, r := range refused {
			recorded, reason, err := done(ctx, tools, r.args)
			if err != nil || recorded || reason != r.reason {
				return fmt.Errorf("done %v: recorded %t, reason %q, want %q (%v)", r.args, recorded, reason, r.reason, err)
			}
		}
		if recorded, reason, err := done(ctx, tools, map[string]any{"outcome": "approved", "criteria": complete, "state": UnitApproved}); err != nil || recorded {
			return fmt.Errorf("done with a state: recorded %t, reason %q (%v)", recorded, reason, err)
		}
		if recorded, reason, err := done(ctx, tools, map[string]any{"outcome": "approved", "criteria": complete}); err != nil || !recorded {
			return fmt.Errorf("complete report: reason %q (%v)", reason, err)
		}
		const again = "this turn already reported its unit done; end the turn"
		if recorded, reason, err := done(ctx, tools, map[string]any{"outcome": "again", "criteria": complete}); err != nil || recorded || reason != again {
			return fmt.Errorf("second done: recorded %t, reason %q (%v)", recorded, reason, err)
		}
		return nil
	}
	masons.play[masonTurnID("dedupe")] = func(ctx context.Context, _ agent.Request, tools *mcp.ClientSession) error {
		if recorded, reason, err := done(ctx, tools, map[string]any{"outcome": "Built", "criteria": []any{}}); err != nil || recorded || reason != "the report misses spec#2: report on every criterion of unit dedupe" {
			return fmt.Errorf("incomplete report: recorded %t, reason %q (%v)", recorded, reason, err)
		}
		return nil
	}
	stream, _ := f.builtAs(t, "design")
	f.awaitUnit(t, stream, "resume", UnitReviewing)
	f.awaitMasonRan(t, stream, "dedupe")
	settle()
	masons.check(t)
	f.checkUnits(t, stream, []UnitStatus{{Unit: "resume", State: UnitReviewing}, {Unit: "dedupe", State: UnitImplementing}})

	th, err := f.repository().Thread(stream, masonAgent("resume"))
	must(t, err)
	if len(th.Turns) != 1 {
		t.Fatalf("mason turns %+v", th.Turns)
	}
	turn := th.Turns[0]
	wantReport := MasonReport{Outcome: "approved", Criteria: []CriterionReport{resumeReport}}
	var reported MasonReport
	if outcome := turn.Response.Result.Outcome; turn.Status() != "idle" || outcome == nil || outcome.Status != masonDone || json.Unmarshal([]byte(outcome.Report), &reported) != nil || !reflect.DeepEqual(reported, wantReport) {
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
	want := UnitReport{Unit: "resume", Turn: masonTurnID("resume"), Seal: 1, Outcome: "approved", Criteria: []CriterionReport{resumeReport}, Branch: unitBranch(stream, "resume"), Base: report.Base, Candidate: report.Candidate}
	if !reflect.DeepEqual(report, want) {
		t.Fatalf("report %+v, want %+v", report, want)
	}
	if data, err := os.ReadFile(filepath.Join(f.trace, "workstreams", string(stream), "units", "resume", "report.json")); err != nil || string(data) != doc.Content {
		t.Fatalf("report file %q %v", data, err)
	}

	reason := fmt.Sprintf("the mason of unit resume reported done on turn %s; its candidate is %s on %s, from %s at %s, and its report is units/resume/report.json revision 1", masonTurnID("resume"), report.Candidate, unitBranch(stream, "resume"), featureBranch(stream), report.Base)
	reviewing := transitionMove{reviewingTransitionID("resume", 1), trace.UnitSubject("resume"), UnitImplementing, UnitReviewing, turn.Response.ID, reason}
	if got, want := masonTransitions(t, f, stream), []transitionMove{started("resume", f.startedReason(t, stream, "resume")), reviewing, started("dedupe", f.startedReason(t, stream, "dedupe"))}; !reflect.DeepEqual(got, want) {
		t.Fatalf("mason transitions %+v, want %+v", got, want)
	}

	if got := f.reports(t, stream, "dedupe"); len(got) != 0 {
		t.Fatalf("the refused report was recorded: %+v", got)
	}
	th, err = f.repository().Thread(stream, masonAgent("dedupe"))
	must(t, err)
	if turn := th.Turns[0]; turn.Status() != "idle" || turn.Response.Result.Outcome != nil {
		t.Fatalf("the refused mason's turn ended %s with %+v", turn.Status(), turn.Response.Result.Outcome)
	}

	// A unit that is no longer implementing takes no report.
	scope := coreadapter.Scope{Project: string(f.project), Workstream: string(stream), Unit: "resume", Thread: masonAgent("resume"), Turn: "late", Role: masonRole}
	raw, err := json.Marshal(map[string]any{"outcome": "Built", "criteria": complete})
	must(t, err)
	out, err := (&masonReports{}).tool(f.repository(), scope).Handle(context.Background(), raw)
	must(t, err)
	if string(out) != `{"recorded":false,"reason":"unit resume is reviewing, not implementing"}` {
		t.Fatalf("late done %s", out)
	}
}

// A unit whose mason reported done but whose workspace cannot be
// snapshotted, here because the worktree's index is locked, stays
// implementing with nothing recorded but why it is blocked: its workstream
// starts nothing else, and its mason slot goes to the next workstream. Once
// the lock is gone, the next pass moves it to reviewing with its candidate.
func TestUnitCandidateFailureKeepsItImplementing(t *testing.T) {
	t.Parallel()
	f, masons := newMasonFixture(t, 1, independentPlan)
	defer f.stop(t)
	factory := runtime.Target{Scope: "factory"}
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: factory, Mode: "soft", Source: "operator"})
	a, _ := f.builtAs(t, "first")
	b, _ := f.builtAs(t, "second")
	blocked, other := lowHigh(a, b)
	var lock string
	masons.play[masonTurnID("resume")] = func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) error {
		if sessionStream(req) != string(blocked) {
			return nil
		}
		workspace := filepath.Join(f.opts.Config.Root, unitsDirectory, string(f.project), string(blocked), "resume")
		gitfile, err := os.ReadFile(filepath.Join(workspace, ".git"))
		if err != nil {
			return err
		}
		lock = filepath.Join(strings.TrimSpace(strings.TrimPrefix(string(gitfile), "gitdir:")), "index.lock")
		if err := os.WriteFile(lock, nil, 0600); err != nil {
			return err
		}
		return reportDone("Built")(ctx, req, tools)
	}
	mutation(t, f.c, "DELETE", "pause", factory)
	f.awaitMasonRan(t, other, "resume")
	settle()
	masons.check(t)
	const prefix = "unit resume stays implementing: its mason reported done, and its candidate cannot be made: "
	if got := f.blocks(t, blocked, "resume"); len(got) != 1 || !strings.HasPrefix(got[0], prefix) || !strings.Contains(got[0], "index.lock") {
		t.Fatalf("blocked %q", got)
	}
	f.checkUnits(t, blocked, []UnitStatus{{Unit: "resume", State: UnitImplementing}, {Unit: "dedupe", State: UnitReady}})
	if got := f.reports(t, blocked, "resume"); len(got) != 0 {
		t.Fatalf("reports %+v", got)
	}
	if _, err := f.repository().Thread(blocked, masonAgent("dedupe")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the workstream started another unit: %v", err)
	}

	masons.mu.Lock()
	must(t, os.Remove(lock))
	masons.mu.Unlock()
	f.awaitUnit(t, blocked, "resume", UnitReviewing)
	docs := f.reports(t, blocked, "resume")
	if len(docs) != 1 {
		t.Fatalf("reports %+v", docs)
	}
	var report UnitReport
	must(t, json.Unmarshal([]byte(docs[0].Content), &report))
	f.checkCandidate(t, blocked, report)
}
