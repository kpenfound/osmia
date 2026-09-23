package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
var exampleCard = coreadapter.Card{Headline: "Uploads resume", Happened: "The storage client resumes uploads from the last chunk.", NeedsYou: "Review the candidate."}

// newMasonController returns the mason controller of the service's
// configuration over repository.
func newMasonController(s *Service, repository *trace.Repository) *masons {
	return &masons{s: s, cfg: s.cfg, repository: repository}
}

func criterionArgs(c CriterionReport) map[string]any {
	return map[string]any{"criterion": c.Criterion, "done": c.Done, "evidence": c.Evidence, "proof": c.Proof}
}

// done calls the done tool and returns whether the service accepted the
// report, and its reason when it did not.
func done(ctx context.Context, tools *mcp.ClientSession, args map[string]any) (bool, string, error) {
	for key, value := range map[string]string{"headline": exampleCard.Headline, "happened": exampleCard.Happened, "needs_you": exampleCard.NeedsYou} {
		if _, ok := args[key]; !ok {
			args[key] = value
		}
	}
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

func TestFinishNoticeWithOptionalOwnerAction(t *testing.T) {
	base := "Unit parser is reviewing: its mason reported done on turn first; its report is units/parser/report.json revision 2."
	for _, tc := range []struct {
		card *coreadapter.Card
		want string
	}{
		{nil, base},
		{&coreadapter.Card{Headline: "Parser is ready"}, base + " Headline: Parser is ready"},
		{&coreadapter.Card{Headline: "Parser is ready", NeedsYou: "Review the candidate."}, base + " Headline: Parser is ready; Needs you: Review the candidate."},
	} {
		if got := finishNotice("parser", "first", "units/parser/report.json", 2, tc.card); got != tc.want {
			t.Fatalf("notice %q, want %q", got, tc.want)
		}
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
// evidence or the proof; input the tool's schema refuses is a tool error.
// Either way the unit does not move and the turn does not fail. A complete report is
// accepted once per turn. Once the turn ends, the service snapshots the
// unit's workspace as its candidate, records the report with the candidate
// as units/<unit>/report.json and moves the unit to reviewing, whatever
// outcome the mason gave. That frees the workstream to start its next ready
// unit. A turn that fails after its report was accepted leaves its unit
// implementing.
func TestMasonDoneMovesTheUnitToReviewing(t *testing.T) {
	t.Parallel()
	f, masons := newMasonFixture(t, 4, independentPlan)
	defer func() { f.stop(t) }()
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
		missing, err := callTool(ctx, tools, doneTool, map[string]any{"outcome": "approved", "criteria": complete})
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
			args := map[string]any{"outcome": "approved", "criteria": complete, bad.field: bad.value}
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
		// Input the schema refuses is a tool error, not a recorded refusal.
		noProof := criterionArgs(resumeReport)
		delete(noProof, "proof")
		for _, args := range []map[string]any{
			{"outcome": "approved", "criteria": complete, "state": UnitApproved},
			{"outcome": "Built", "criteria": []any{noProof}},
			{"outcome": 1, "criteria": complete},
		} {
			if text, err := callTool(ctx, tools, doneTool, args); err == nil {
				return fmt.Errorf("done %v is no tool error: %s", args, text)
			}
		}
		if recorded, reason, err := done(ctx, tools, map[string]any{"outcome": "approved", "criteria": complete, "headline": " Uploads   resume ", "happened": " The storage client resumes uploads from the last chunk. ", "needs_you": " Review the candidate. "}); err != nil || !recorded {
			return fmt.Errorf("complete report: reason %q (%v)", reason, err)
		}
		const again = "this turn already reported its unit done; end the turn"
		if recorded, reason, err := done(ctx, tools, map[string]any{"outcome": "again", "criteria": complete}); err != nil || recorded || reason != again {
			return fmt.Errorf("second done: recorded %t, reason %q (%v)", recorded, reason, err)
		}
		return nil
	}
	dedupeReport := CriterionReport{Criterion: "spec#2", Done: "skip acknowledged chunks", Evidence: "no chunk is sent twice", Proof: "internal/trace/built.go"}
	masons.play[masonTurnID("dedupe")] = func(ctx context.Context, _ agent.Request, tools *mcp.ClientSession) error {
		if recorded, reason, err := done(ctx, tools, map[string]any{"outcome": "Built", "criteria": []any{}}); err != nil || recorded || reason != "the report misses spec#2: report on every criterion of unit dedupe" {
			return fmt.Errorf("incomplete report: recorded %t, reason %q (%v)", recorded, reason, err)
		}
		if recorded, reason, err := done(ctx, tools, map[string]any{"outcome": "Built", "criteria": []any{criterionArgs(dedupeReport)}}); err != nil || !recorded {
			return fmt.Errorf("complete report: reason %q (%v)", reason, err)
		}
		return errFailTurn
	}
	stream, _ := f.builtAs(t, "design")
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
	settle()
	masons.check(t)
	f.checkUnits(t, stream, []UnitStatus{{Unit: "resume", State: UnitReviewing, Card: &exampleCard}, {Unit: "dedupe", State: UnitImplementing}})

	th, err := f.repository().Thread(stream, masonAgent("resume"))
	must(t, err)
	if len(th.Turns) != 1 {
		t.Fatalf("mason turns %+v", th.Turns)
	}
	turn := th.Turns[0]
	wantReport := MasonReport{Outcome: "approved", Criteria: []CriterionReport{resumeReport}}
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
	want := UnitReport{Unit: "resume", Turn: masonTurnID("resume"), Seal: 1, Outcome: "approved", Criteria: []CriterionReport{resumeReport}, Card: &exampleCard, Branch: unitBranch(stream, "resume"), Base: report.Base, Candidate: report.Candidate}
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
	if data, err := os.ReadFile(filepath.Join(f.trace, "workstreams", string(stream), "units", "resume", "report.json")); err != nil || string(data) != doc.Content {
		t.Fatalf("report file %q %v", data, err)
	}

	reason := fmt.Sprintf("the mason of unit resume reported done on turn %s; its candidate is %s on %s, from %s at %s, and its report is units/resume/report.json revision 1", masonTurnID("resume"), report.Candidate, unitBranch(stream, "resume"), featureBranch(stream), report.Base)
	reviewing := transitionMove{reviewingTransitionID("resume", 1), trace.UnitSubject("resume"), UnitImplementing, UnitReviewing, turn.Response.ID, reason}
	if got, want := masonTransitions(t, f, stream), []transitionMove{started("resume", f.startedReason(t, stream, "resume")), reviewing, started("dedupe", f.startedReason(t, stream, "dedupe"))}; !reflect.DeepEqual(got, want) {
		t.Fatalf("mason transitions %+v, want %+v", got, want)
	}

	// The finish and the next start carry one notice each for the chief of
	// staff, delivered as event turns and acknowledged once they have
	// completed successfully.
	reviewNotice := fmt.Sprintf("Unit resume is reviewing: its mason reported done on turn %s; its report is units/resume/report.json revision 1. Headline: Uploads resume; Needs you: Review the candidate.", masonTurnID("resume"))
	if body := f.notice(t, stream, reviewingTransitionID("resume", 1)); body != reviewNotice {
		t.Fatalf("the reviewing notice %q, want %q", body, reviewNotice)
	}
	f.awaitEventTurns(t, stream, reviewNotice, "Unit dedupe is implementing: its mason works on it in its unit workspace on "+unitBranch(stream, "dedupe")+".")
	f.awaitAcknowledgedNotices(t, stream)

	// A turn that failed after its report was accepted does not finish its
	// unit.
	if got := f.reports(t, stream, "dedupe"); len(got) != 0 {
		t.Fatalf("the failed turn's report was recorded: %+v", got)
	}
	th, err = f.repository().Thread(stream, masonAgent("dedupe"))
	must(t, err)
	if turn := th.Turns[0]; turn.Status() != "failed" || turn.Response.Result.Outcome == nil || turn.Response.Result.Outcome.Status != masonDone {
		t.Fatalf("the failing mason's turn ended %s with %+v", turn.Status(), turn.Response.Result.Outcome)
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

	// A unit whose state moved since the pass read it is left as it is.
	f.stop(t)
	repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	controller := newMasonController(f.s, repo)
	b, found, err := controller.read(stream)
	if err != nil || !found {
		t.Fatalf("read %v %v", found, err)
	}
	subject := trace.UnitSubject("resume")
	b.states[subject] = trace.WorkflowState{Version: b.states[subject].Version - 1, Value: UnitImplementing}
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

// A unit whose mason reported done but whose workspace cannot be
// snapshotted, here because the feature branch moved on past the commit its
// workspace is at, stays implementing with nothing recorded but why it is
// blocked: its workstream starts nothing else, and its mason slot goes to the
// next workstream. Once the feature branch is back, the next pass moves it
// to reviewing with its candidate.
func TestUnitCandidateFailureKeepsItImplementing(t *testing.T) {
	t.Parallel()
	f, masons := newMasonFixture(t, 1, independentPlan)
	defer f.stop(t)
	factory := runtime.Target{Scope: "factory"}
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: factory, Mode: "soft", Source: "operator"})
	a, _ := f.builtAs(t, "first")
	b, _ := f.builtAs(t, "second")
	blocked, other := lowHigh(a, b)
	feature := "refs/heads/" + featureBranch(blocked)
	git := func(args ...string) (string, error) {
		out, err := exec.Command("git", append([]string{"-C", f.clone, "-c", "user.name=Owner", "-c", "user.email=owner@localhost"}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	var tip string
	masons.play[masonTurnID("resume")] = func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) error {
		if sessionStream(req) != string(blocked) {
			return nil
		}
		var err error
		if tip, err = git("rev-parse", feature); err != nil {
			return err
		}
		moved, err := git("commit-tree", "-p", tip, "-m", "moved on", tip+"^{tree}")
		if err != nil {
			return fmt.Errorf("%s: %w", moved, err)
		}
		if out, err := git("update-ref", feature, moved, tip); err != nil {
			return fmt.Errorf("%s: %w", out, err)
		}
		return reportDone("Built")(ctx, req, tools)
	}
	mutation(t, f.c, "DELETE", "pause", factory)
	f.awaitMasonRan(t, other, "resume")
	settle()
	masons.check(t)
	const prefix = "unit resume stays implementing: its mason reported done, and its candidate cannot be made: workspace "
	if got := f.blocks(t, blocked, "resume"); len(got) != 1 || !strings.HasPrefix(got[0], prefix) || !strings.HasSuffix(got[0], ", which does not descend from "+featureBranch(blocked)) {
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
	out, err := git("update-ref", feature, tip)
	masons.mu.Unlock()
	if err != nil {
		t.Fatalf("%s: %v", out, err)
	}
	f.awaitUnit(t, blocked, "resume", UnitReviewing)
	docs := f.reports(t, blocked, "resume")
	if len(docs) != 1 {
		t.Fatalf("reports %+v", docs)
	}
	var report UnitReport
	must(t, json.Unmarshal([]byte(docs[0].Content), &report))
	f.checkCandidate(t, blocked, report)
}
