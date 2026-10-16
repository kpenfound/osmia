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
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/followup"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

// newFinalFixture seeds a sealed validPlan with a committee runner and no
// masons. It returns the stopped service, workstream, open trace and assembly
// controller. Intake, debate and ratification are covered by lifecycle tests.
func newFinalFixture(t *testing.T, key string) (*shedFixture, config.WorkstreamID, *trace.Repository, *finalReviewer) {
	t.Helper()
	return newFinalFixtureWith(t, key, "")
}

// newFinalFixtureWith is newFinalFixture with extra configuration appended.
func newFinalFixtureWith(t *testing.T, key, extra string) (*shedFixture, config.WorkstreamID, *trace.Repository, *finalReviewer) {
	t.Helper()
	f := newDebateFixtureWith(t, 1, 1, extra, nil)
	base := f.upstream(t)
	stream, repository := seedBuild(t, f, key, validPlan, config.WorkspacesGit, base)
	return f, stream, repository, &finalReviewer{s: f.s, repository: repository}
}

// mergeDirectly commits files onto the feature branch as the unit's landing
// and records the unit merged, as a landing does, and returns the new tip.
func mergeDirectly(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, unit string, files map[string]string) string {
	t.Helper()
	commit := moveFeature(t, f, stream, files)
	state, err := repository.Workflow(stream, trace.UnitSubject(unit))
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: trace.UnitSubject(unit) + "-" + UnitMerged, Revision: 1, Project: repository.Project(), Workstream: stream, Unit: unit, At: time.Now().UTC(), Actor: foremanActor, Cause: "landing-" + unit}
	_, err = repository.Transact(context.Background(), trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: h, Subject: trace.UnitSubject(unit), From: state.Value, To: UnitMerged, Reason: fmt.Sprintf("unit %s landed as %s", unit, commit)}})
	must(t, err)
	return commit
}

// advanceUpstream commits files on upstream's main and returns the commit.
func advanceUpstream(t *testing.T, f *shedFixture, files map[string]string) string {
	t.Helper()
	home := filepath.Dir(f.clone)
	scratch := filepath.Join(home, "upstream-scratch")
	if _, err := os.Stat(scratch); err != nil {
		// --no-hardlinks: a local clone otherwise hardlinks the bare
		// repository's object files into scratch instead of copying them.
		demoGit(t, home, "clone", "--quiet", "--no-hardlinks", filepath.Join(home, "remotes", "dagger", "dagger.git"), scratch)
	}
	for name, content := range files {
		must(t, os.MkdirAll(filepath.Dir(filepath.Join(scratch, name)), 0700))
		must(t, os.WriteFile(filepath.Join(scratch, name), []byte(content), 0600))
	}
	demoGit(t, home, "-C", scratch, "add", "--all")
	demoGit(t, home, "-C", scratch, "-c", "user.name=Upstream", "-c", "user.email=upstream@example.invalid", "commit", "-qm", "Upstream moves")
	demoGit(t, home, "-C", scratch, "push", "--quiet", "origin", "HEAD:main")
	return strings.TrimSpace(demoGit(t, home, "-C", scratch, "rev-parse", "HEAD"))
}

// finalOperations returns the workstream's final review operations.
func finalOperations(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	ops, err := repository.Operations(stream)
	must(t, err)
	ops = slices.DeleteFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.Action != FinalReviewAction })
	slices.SortFunc(ops, func(a, b trace.OperationRecord) int { return a.Transition.At.Compare(b.Transition.At) })
	return ops
}

// streamDocuments returns the recorded revisions of one workstream document.
func streamDocuments(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, id string) []trace.Document {
	t.Helper()
	docs, err := trace.Read[trace.Document](repository, stream)
	must(t, err)
	return slices.DeleteFunc(docs, func(d trace.Document) bool { return d.ID != id })
}

// transitionByID returns the workstream's transition with the given ID.
func transitionByID(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, id string) trace.Transition {
	t.Helper()
	transitions, err := trace.Read[trace.Transition](repository, stream)
	must(t, err)
	i := slices.IndexFunc(transitions, func(tr trace.Transition) bool { return tr.ID == id })
	if i < 0 {
		t.Fatalf("workstream %s has no transition %s", stream, id)
	}
	return transitions[i]
}

// chiefEvent reports whether e is an event the chief of staff receives as
// information: a notice or routine progress.
func chiefEvent(e trace.Event) bool {
	return e.Kind == trace.NoticeKind || e.Kind == trace.ProgressKind
}

// noticeOf returns the notice the transition raised for the chief of staff.
func noticeOf(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, transition string) string {
	t.Helper()
	outbox, err := repository.Outbox(stream)
	must(t, err)
	for _, entry := range outbox {
		if entry.TransitionID == transition && chiefEvent(entry.Event) {
			return entry.Event.Body
		}
	}
	t.Fatalf("transition %s told the chief of staff nothing", transition)
	return ""
}

// assembleBoth merges both units of validPlan, checking that the workstream
// stays building while one is unmerged, and asks for the first final review.
// It returns the feature branch tip and the review's operation.
func assembleBoth(t *testing.T, f *shedFixture, repository *trace.Repository, a *finalReviewer, stream config.WorkstreamID, resume, dedupe map[string]string) (string, coreadapter.Operation) {
	t.Helper()
	ctx := context.Background()
	building := func() {
		t.Helper()
		must(t, a.Pass(ctx))
		if feature, err := repository.Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != BuildingState {
			t.Fatalf("the workstream is %+v with a unit unmerged: %v", feature, err)
		}
	}
	building()
	mergeDirectly(t, f, repository, stream, "resume", resume)
	building()
	head := mergeDirectly(t, f, repository, stream, "dedupe", dedupe)
	must(t, a.Pass(ctx))
	if feature, err := repository.Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != AssembledState {
		t.Fatalf("the workstream is %+v with every unit merged: %v", feature, err)
	}
	if ops := finalOperations(t, repository, stream); len(ops) != 0 {
		t.Fatalf("final reviews asked for in the pass that assembled: %+v", ops)
	}
	must(t, a.Pass(ctx))
	must(t, a.Pass(ctx))
	ops := finalOperations(t, repository, stream)
	if len(ops) != 1 {
		t.Fatalf("final review operations %+v", ops)
	}
	return head, ops[0].Operation
}

// finalTurn installs the fake committee member's turn of final review k.
func (f *shedFixture) finalTurn(k, attempt int, run func(ctx context.Context, tools *mcp.ClientSession) error) string {
	turn := finalTurnID(k, committeeAgent(1), attempt)
	f.engine.mu.Lock()
	defer f.engine.mu.Unlock()
	f.engine.turns[turn] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if err := run(ctx, tools); err != nil {
			return nil, err
		}
		return &agent.Result{ClaudeID: "session-" + turn, ResultText: "Read the branch", SessionDir: req.SessionDir, NumTurns: 1}, nil
	}
	return turn
}

// A workstream moves to assembled only once every planned unit has merged.
// Its first final review rebases the feature branch onto upstream's moved
// main, replaying each landed commit, and one committee member reads the
// exact rebased commit, its diff, the sealed spec and plan and the charter
// in a read-only view, with every criterion and the merged units serving it.
// The report it records holds that coverage and the reader's findings on the
// branch as a whole; a finding without a known severity or citing what is not
// a sealed criterion is refused. The report records the reviewed commit and
// every governing revision, survives a restart, and authorises later steps
// until the branch, the spec or the charter changes, when a new review is
// asked for.
func TestFinalReviewReadsTheRebasedBranchAsAWhole(t *testing.T) {
	t.Parallel()
	f, stream, repository, a := newFinalFixture(t, "final")
	ctx := context.Background()
	head, op := assembleBoth(t, f, repository, a, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n\n// Resume resumes.\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n\n// Dedupe skips.\n"})
	assembled := transitionByID(t, repository, stream, AssembledState)
	wantAssembled := fmt.Sprintf("every unit of the sealed plan has merged onto %s: resume, dedupe; the feature branch is rebased onto upstream and read against the sealed spec and the charter next", featureBranch(stream))
	if assembled.From != BuildingState || assembled.To != AssembledState || assembled.Actor != foremanActor || assembled.Cause != "unit-dedupe-merged" || assembled.Reason != wantAssembled {
		t.Fatalf("the move to assembled %+v", assembled)
	}
	if body := noticeOf(t, repository, stream, AssembledState); body != "Workstream state changed from building to assembled: "+wantAssembled {
		t.Fatalf("the notice %q", body)
	}
	charter, err := repository.Charter(ctx, time.Now())
	must(t, err)
	in, err := decodeFinalReview(op)
	must(t, err)
	if in != (finalReviewInput{Review: 1, Commit: head, Seal: 1, Spec: 1, Plan: 1, Charter: charter.Revision}) {
		t.Fatalf("final review input %+v", in)
	}
	landed := strings.Split(strings.TrimSpace(demoGit(t, filepath.Dir(f.clone), "-C", f.clone, "log", "--format=%H %s", "-2", head)), "\n")

	upstream := advanceUpstream(t, f, map[string]string{"UPSTREAM.md": "upstream moved\n"})
	var problems []error
	checked := 0
	f.s.options.reviewChecks = checkFunc(func(_ context.Context, dir string) (CheckResult, error) {
		checked++
		data, err := os.ReadFile(filepath.Join(dir, "UPSTREAM.md"))
		if err != nil || string(data) != "upstream moved\n" {
			return CheckResult{}, fmt.Errorf("checks missed rebased candidate: %s %v", data, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "UPSTREAM.md"), []byte("check generated output"), 0600); err != nil {
			return CheckResult{}, err
		}
		return CheckResult{Output: "all proofs passed"}, nil
	})
	turn := f.finalTurn(1, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		if checked != 1 {
			problems = append(problems, fmt.Errorf("the checks ran %d times before the read", checked))
		}
		for path, want := range map[string]string{plan.SpecPath: validSpec, plan.PlanPath: validPlan, "charter.md": shedCharter,
			"branch/internal/trace/dedupe.go": "package trace\n\n// Dedupe skips.\n", "branch/UPSTREAM.md": "upstream moved\n"} {
			if got, err := readTool(ctx, tools, path); err != nil || got != want {
				problems = append(problems, fmt.Errorf("%s holds %q: %v", path, got, err))
			}
		}
		diff, err := callTool(ctx, tools, workstreamDiffTool, map[string]any{})
		if err != nil || !strings.Contains(diff, "internal/trace/resume.go") || !strings.Contains(diff, "internal/trace/dedupe.go") || strings.Contains(diff, "UPSTREAM.md") {
			problems = append(problems, fmt.Errorf("branch diff %q: %v", diff, err))
		}
		if listed, err := callTool(ctx, tools, workstreamDiffTool, map[string]any{"files_only": true, "paths": []string{"internal/trace/dedupe.go"}}); err != nil || !strings.Contains(listed, `"path":"internal/trace/dedupe.go"`) || strings.Contains(listed, "resume.go") {
			problems = append(problems, fmt.Errorf("branch files %q: %v", listed, err))
		}
		if report, err := readTool(ctx, tools, "units/resume/report.json"); err == nil {
			problems = append(problems, fmt.Errorf("a unit that never reported shows a report %q", report))
		}
		if got, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "x", "findings": []any{map[string]any{"severity": "minor", "finding": "x"}}}); err == nil || strings.Contains(got, `"recorded":true`) {
			problems = append(problems, fmt.Errorf("a finding of unknown severity was not refused: %s %v", got, err))
		}
		if got, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "x", "findings": []any{map[string]any{"severity": "advisory", "criteria": []any{"spec#9"}, "finding": "x"}}}); err != nil || !strings.Contains(got, `\"spec#9\" is not a criterion of the sealed spec`) {
			problems = append(problems, fmt.Errorf("a finding citing an unknown criterion was not refused: %s %v", got, err))
		}
		got, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Uploads resume and skip acknowledged chunks.", "findings": []any{
			map[string]any{"severity": "advisory", "criteria": []any{"spec#2"}, "finding": "Dedupe keeps acknowledged chunk IDs in memory only.", "action": "Persist them if restarts matter."}}})
		if err != nil || !strings.Contains(got, `"recorded":true`) {
			problems = append(problems, fmt.Errorf("the report was not recorded: %s %v", got, err))
		}
		return nil
	})
	result, err := a.Apply(ctx, op)
	must(t, err)
	if err := errors.Join(problems...); err != nil {
		t.Fatal(err)
	}
	if result.Outcome != "succeeded" {
		t.Fatalf("final review result %+v", result)
	}

	g := workspaces(f.s.cfg, branchesDirectory, config.WorkspacesGit)
	tip, _, err := g.Branch(ctx, featureBranch(stream))
	must(t, err)
	var run FinalCheckRun
	runs := streamDocuments(t, repository, stream, finalChecksDocument(1))
	if len(runs) != 1 || runs[0].Path != "final/checks-1.json" || json.Unmarshal([]byte(runs[0].Content), &run) != nil ||
		run.Commit != tip || run.Status != ChecksPassed || run.Output != "all proofs passed" || !slices.Equal(run.Command, []string{"dagger", "check", "--progress=report", "--failfast"}) {
		t.Fatalf("the final check run %+v", runs)
	}
	reader, err := repository.Thread(stream, committeeAgent(1))
	must(t, err)
	if prompt := reader.Turns[len(reader.Turns)-1].Request.Prompt; !strings.Contains(prompt, "recorded in final/checks-1.json; you do not run them. The run passed.") || !strings.Contains(prompt, "all proofs passed") ||
		!strings.Contains(prompt, "- spec#1: served by merged units resume, each reviewed against its acceptance\n- spec#2: served by merged units dedupe") || strings.Contains(prompt, "change since") {
		t.Fatalf("the reader's prompt lacks the check run:\n%s", prompt)
	}
	if on, err := g.Ancestor(ctx, upstream, tip); err != nil || !on || tip == head {
		t.Fatalf("the feature branch at %s is not rebased onto upstream %s: %v", tip, upstream, err)
	}
	replayed := strings.Split(strings.TrimSpace(demoGit(t, filepath.Dir(f.clone), "-C", f.clone, "log", "--format=%H %s", upstream+".."+tip)), "\n")
	if len(replayed) != len(landed) || slices.ContainsFunc(replayed, func(r string) bool { return slices.Contains(landed, r) }) {
		t.Fatalf("the rebased branch holds %v, landed %v", replayed, landed)
	}
	for i := range replayed {
		_, got, _ := strings.Cut(replayed[i], " ")
		_, was, _ := strings.Cut(landed[i], " ")
		if got != was {
			t.Fatalf("replayed %q, landed %q", replayed[i], landed[i])
		}
	}
	worktree := filepath.Join(f.opts.Config.Root, branchesDirectory, string(f.project), string(stream))
	if at := strings.TrimSpace(demoGit(t, filepath.Dir(f.clone), "-C", worktree, "rev-parse", "HEAD")); at != tip {
		t.Fatalf("the feature worktree is at %s, not %s", at, tip)
	}
	if status := strings.TrimSpace(demoGit(t, filepath.Dir(f.clone), "-C", worktree, "status", "--porcelain")); status != "" {
		t.Fatalf("the feature worktree after the rebase:\n%s", status)
	}

	want := FinalReport{Review: 1, Operation: op.ID, Outcome: finalReviewed, Branch: featureBranch(stream), Before: head, Commit: tip,
		Upstream: &seal.Base{Remote: "upstream", Branch: "main", Commit: upstream}, Seal: 1, SpecHash: seal.SpecHash(validSpec), Spec: 1, Plan: 1, Charter: charter.Revision,
		Reader: committeeAgent(1), Turn: turn, Summary: "Uploads resume and skip acknowledged chunks.", Criteria: []FinalCriterion{
			{Criterion: "spec#1", Text: "An interrupted upload resumes from the last acknowledged chunk.", Evidence: "served by merged units resume, each reviewed against its acceptance"},
			{Criterion: "spec#2", Text: "Acknowledged chunks are never sent again.", Evidence: "served by merged units dedupe, each reviewed against its acceptance"}},
		Findings: []FinalFinding{{Severity: FindingAdvisory, Criteria: []string{"spec#2"}, Finding: "Dedupe keeps acknowledged chunk IDs in memory only.", Action: "Persist them if restarts matter."}}}
	report, found, err := latestFinalReport(repository, stream)
	if err != nil || !found || !reflect.DeepEqual(report, want) {
		t.Fatalf("final report %+v, %v, %v; want %+v", report, found, err, want)
	}
	reports := streamDocuments(t, repository, stream, finalReportDocument)
	if len(reports) != 1 || reports[0].Path != finalReportPath || reports[0].Cause != op.ID || reports[0].Actor != finalReviewActor {
		t.Fatalf("final report documents %+v", reports)
	}
	var rebase FinalRebase
	rebases := streamDocuments(t, repository, stream, finalRebaseDocument)
	if len(rebases) != 1 || rebases[0].Path != finalRebasePath || json.Unmarshal([]byte(rebases[0].Content), &rebase) != nil ||
		rebase != (FinalRebase{Review: 1, Operation: op.ID, Branch: featureBranch(stream), Upstream: *want.Upstream, Before: head, Commit: tip}) {
		t.Fatalf("final rebase documents %+v", rebases)
	}
	reviewed := transitionByID(t, repository, stream, "final-review-1-reviewed")
	wantReason := fmt.Sprintf("final review 1 read %s at %s against seal 1 (spec revision 1, plan revision 1, charter revision %d): 0 blocking and 1 advisory; report final/report.json revision 1", featureBranch(stream), tip, charter.Revision)
	if reviewed.Subject != finalReviewSubject || reviewed.From != "requested-1" || reviewed.To != "reviewed-1" || reviewed.Cause != op.ID || reviewed.Reason != wantReason || result.Evidence != wantReason {
		t.Fatalf("the reviewed transition %+v, result %+v", reviewed, result)
	}
	if body := noticeOf(t, repository, stream, reviewed.ID); body != "The final review of the assembled branch is recorded: 0 blocking, 1 advisory.\nAdvisory: spec#2: Dedupe keeps acknowledged chunk IDs in memory only. Action: Persist them if restarts matter.\nNothing blocks delivery; the owner decides it with the advisory findings in view." {
		t.Fatalf("the notice %q", body)
	}
	if _, reason, err := a.finalGate(ctx, stream); err != nil || reason != "" {
		t.Fatalf("the current report does not authorise: %q %v", reason, err)
	}

	// The review is at rest: applied again it records nothing, and a pass
	// asks for no other.
	again, err := a.Apply(ctx, op)
	if err != nil || !reflect.DeepEqual(again, result) {
		t.Fatalf("applied again %+v: %v", again, err)
	}
	must(t, a.Pass(ctx))
	if ops := finalOperations(t, repository, stream); len(ops) != 1 || len(streamDocuments(t, repository, stream, finalReportDocument)) != 1 {
		t.Fatalf("final review operations after the review %+v", ops)
	}

	// A restart keeps the report readable and current.
	repository.Close()
	f.start(t)
	live := &finalReviewer{s: f.s, repository: f.repository()}
	if after, _, err := latestFinalReport(f.repository(), stream); err != nil || !reflect.DeepEqual(after, want) {
		t.Fatalf("the report after a restart %+v: %v", after, err)
	}
	if _, reason, err := live.finalGate(ctx, stream); err != nil || reason != "" {
		t.Fatalf("the report after a restart does not authorise: %q %v", reason, err)
	}
	// The restarted service completes the operation from its record.
	if ops := awaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return finalOperations(t, f.repository(), stream) }); len(ops) != 1 || !reflect.DeepEqual(ops[0].Result, &result) {
		t.Fatalf("final review operations after a restart %+v", ops)
	}
	f.stop(t)
	repository, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	defer repository.Close()
	a = &finalReviewer{s: f.s, repository: repository}
	if ops := finalOperations(t, repository, stream); len(ops) != 1 {
		t.Fatalf("the restarted service asked for another final review: %+v", ops)
	}

	// A branch that moves makes the report stale, and a new review of the
	// moved branch is asked for.
	moved := moveFeature(t, f, stream, map[string]string{"internal/trace/late.go": "package trace\n"})
	if _, reason, err := a.finalGate(ctx, stream); err != nil || reason != fmt.Sprintf("final review 1 is stale: it read feature branch commit %s, and %s is current; a new final review is required", tip, moved) {
		t.Fatalf("a moved branch: %q %v", reason, err)
	}
	must(t, a.Pass(ctx))
	ops := finalOperations(t, repository, stream)
	if len(ops) != 2 {
		t.Fatalf("final review operations after the branch moved %+v", ops)
	}
	if in, err := decodeFinalReview(ops[1].Operation); err != nil || in != (finalReviewInput{Review: 2, Commit: moved, Seal: 1, Spec: 1, Plan: 1, Charter: charter.Revision}) {
		t.Fatalf("the second review %+v: %v", in, err)
	}
	demoGit(t, filepath.Dir(f.clone), "-C", worktree, "reset", "--hard", "--quiet", tip)
	if _, reason, err := a.finalGate(ctx, stream); err != nil || reason != "" {
		t.Fatalf("the branch back at the reviewed commit: %q %v", reason, err)
	}

	// So does a changed charter or spec.
	must(t, os.WriteFile(filepath.Join(f.trace, "charter.md"), []byte(shedCharter+"3. Document every change.\n"), 0600))
	if _, reason, err := a.finalGate(ctx, stream); err != nil || reason != fmt.Sprintf("final review 1 is stale: it read charter revision %d, and %d is current; a new final review is required", charter.Revision, charter.Revision+1) {
		t.Fatalf("a changed charter: %q %v", reason, err)
	}
	specs := streamDocuments(t, repository, stream, plan.SpecDocument)
	amended := specs[len(specs)-1]
	amended.Revision++
	amended.At = time.Now().UTC()
	amended.Content = strings.Replace(validSpec, "never sent again", "never sent twice", 1)
	must(t, repository.RecordDocuments(ctx, []trace.Document{amended}))
	if _, reason, err := a.finalGate(ctx, stream); err != nil || reason != "final review 1 is stale: it read spec revision 1, and 2 is current; a new final review is required" {
		t.Fatalf("a changed spec: %q %v", reason, err)
	}
}

// A final review's blocking finding builds nothing on its own: the plan and
// its units stay as they are, the chief of staff hears the finding with how
// to triage it, and the report is presented for delivery. Approving it
// without accepting the finding is refused; approving with acceptance records
// what was accepted, and delivery holds the approval to it. A new review of
// a changed branch starts from what the previous one found and reads what
// changed since.
func TestBlockingFinalFindingsWaitForTriageOrOwnerAcceptance(t *testing.T) {
	t.Parallel()
	f, stream, repository, a := newFinalFixture(t, "blocking")
	ctx := context.Background()
	_, op := assembleBoth(t, f, repository, a, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	f.finalTurn(1, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Dedupe is not wired in.", "findings": []any{
			map[string]any{"severity": "blocking", "criteria": []any{"spec#2"}, "finding": "The upload path never calls Dedupe.", "action": "Call Dedupe before sending a chunk."}}})
		return err
	})
	if result := settleOperation(t, f.s, repository, stream, op, a); result.Outcome != "succeeded" {
		t.Fatalf("the first review %+v", result)
	}
	blocking := []string{"spec#2: The upload path never calls Dedupe. Action: Call Dedupe before sending a chunk."}
	if added, err := followup.Read(repository, stream); err != nil || len(added) != 0 {
		t.Fatalf("a blocking finding built follow-up units %+v: %v", added, err)
	}
	if plans := streamDocuments(t, repository, stream, plan.PlanDocument); len(plans) != 1 || plans[0].Content != validPlan {
		t.Fatalf("a blocking finding changed the ratified plan: %+v", plans)
	}
	if feature, err := repository.Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != AssembledState {
		t.Fatalf("feature after a blocking finding %+v: %v", feature, err)
	}
	if body := noticeOf(t, repository, stream, "final-review-1-reviewed"); !strings.Contains(body, "1 blocking, 0 advisory.\nBlocking: "+blocking[0]) || !strings.Contains(body, "through route_amendment") {
		t.Fatalf("the notice %q", body)
	}
	must(t, a.Pass(ctx))
	if ops := finalOperations(t, repository, stream); len(ops) != 1 {
		t.Fatalf("the review was asked for again with nothing changed: %+v", ops)
	}

	moveFeature(t, f, stream, map[string]string{"internal/trace/upload.go": "package trace\n"})
	must(t, a.Pass(ctx))
	ops := finalOperations(t, repository, stream)
	if len(ops) != 2 {
		t.Fatalf("no review of the changed branch: %+v", ops)
	}
	var problems []error
	f.finalTurn(2, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		if since, err := callTool(ctx, tools, workstreamDiffTool, map[string]any{"change": "since", "files_only": true}); err != nil || !strings.Contains(since, "internal/trace/upload.go") || strings.Contains(since, "dedupe.go") {
			problems = append(problems, fmt.Errorf("the change since review 1 %q: %v", since, err))
		}
		_, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Dedupe is still not wired in.", "findings": []any{
			map[string]any{"severity": "blocking", "criteria": []any{"spec#2"}, "finding": "The upload path never calls Dedupe.", "action": "Call Dedupe before sending a chunk."}}})
		return err
	})
	result := settleOperation(t, f.s, repository, stream, ops[1].Operation, a)
	if err := errors.Join(problems...); err != nil {
		t.Fatal(err)
	}
	reader, err := repository.Thread(stream, committeeAgent(1))
	must(t, err)
	if prompt := reader.Turns[len(reader.Turns)-1].Request.Prompt; !strings.Contains(prompt, "Final review 1 read ") || !strings.Contains(prompt, "- blocking: "+blocking[0]) || !strings.Contains(prompt, "change since for what changed after final review 1") {
		t.Fatalf("the second reader's prompt lacks the first review:\n%s", prompt)
	}
	if report, reason, err := a.finalGate(ctx, stream); err != nil || reason != "" || result.Outcome != "succeeded" || report.Review != 2 || !slices.Equal(report.blocking(), blocking) {
		t.Fatalf("the second review %+v, reason %q: %v", report, reason, err)
	}

	f.s.setSole(&activeProject{repository: repository})
	p, api := f.s.deliveryPresentation(ctx, string(stream))
	if api != nil || !slices.Equal(p.Report.blocking(), blocking) {
		t.Fatalf("presentation %+v: %v", p.Report, api)
	}
	decision := DeliveryDecision{Review: p.Report.Review, ReviewRevision: p.ReviewRevision, Commit: p.Report.Commit, DraftHash: p.DraftHash}
	if _, api := f.s.approveDelivery(ctx, string(stream), decision); api == nil || api.Code != Conflict || !strings.Contains(api.Message, "approve with accept_blocking") {
		t.Fatalf("approved without accepting the blocking finding: %v", api)
	}
	decision.AcceptBlocking = true
	approval, api := f.s.approveDelivery(ctx, string(stream), decision)
	if api != nil || !slices.Equal(approval.AcceptedBlocking, blocking) {
		t.Fatalf("the accepting approval %+v: %v", approval, api)
	}
	if _, reason, err := f.s.deliveryGate(ctx, repository, stream, approval.Description); err != nil || reason != "" {
		t.Fatalf("the accepting approval does not authorise delivery: %q %v", reason, err)
	}
}

// Once maxFinalRounds final reviews of an assembled feature have found
// something blocking, the chief of staff no longer decides plan revisions of
// it: they wait for the owner, and the chief hears so with the findings.
func TestRepeatedBlockingFinalReviewsLeaveRevisionsToTheOwner(t *testing.T) {
	t.Parallel()
	f, stream, repository, a := newFinalFixture(t, "rounds")
	ctx := context.Background()
	assembleBoth(t, f, repository, a, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	record := func(review int, severity string) {
		content, err := json.Marshal(FinalReport{Review: review, Outcome: finalReviewed, Findings: []FinalFinding{{Severity: severity, Finding: "The upload path never calls Dedupe."}}})
		must(t, err)
		must(t, repository.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: finalReportDocument, Revision: review, Project: repository.Project(), Workstream: stream, At: time.Now().UTC(), Actor: finalReviewActor, Cause: "final-review"}, Path: finalReportPath, Content: string(content)}}))
	}
	chief := func() *APIError {
		return engineeringRevision(repository, stream, amendmentCase{}, seal.Seal{}, nil, "Groups the findings into one unit.")
	}
	const owners = "final reviews have found blocking problems 3 times; further revisions of the assembled plan require an owner decision"
	record(1, FindingBlocking)
	record(2, FindingAdvisory)
	record(3, FindingBlocking)
	if api := chief(); api != nil && api.Message == owners {
		t.Fatalf("two blocking reviews left the revision to the owner: %v", api)
	}
	record(4, FindingBlocking)
	if api := chief(); api == nil || api.Message != owners {
		t.Fatalf("three blocking reviews left the revision to the chief of staff: %v", api)
	}
	if body := finalReviewNotice(FinalReport{}, []string{"x"}, maxFinalRounds, false); !strings.Contains(body, "Further revisions of the assembled plan are the owner's decision") {
		t.Fatalf("the notice %q", body)
	}
}

// A post-merge dependency update's final review with blocking problems
// becomes one follow-up unit that resolves all of them, scoped to the sealed
// footprints of the units serving the criteria they concern. Advisory
// findings build nothing.
func TestPostMergeRepairCollectsBlockingProblemsInOneUnit(t *testing.T) {
	t.Parallel()
	_, stream, _, a := newFinalFixture(t, "repair")
	report := FinalReport{Review: 1, Spec: 1, Plan: 1,
		Criteria: []FinalCriterion{{Criterion: "spec#1", Gap: "no merged unit serves it"}, {Criterion: "spec#2", Evidence: "served by merged units dedupe"}},
		Findings: []FinalFinding{{Severity: FindingBlocking, Criteria: []string{"spec#2"}, Finding: "Dedupe breaks with the merged dependency."}, {Severity: FindingAdvisory, Finding: "Rename the helper."}}}
	added, found, err := a.followup(stream, report, 1)
	must(t, err)
	if !found || added.Unit.ID != "final-1" || !slices.Equal(added.Unit.Criteria, []string{"spec#1", "spec#2"}) || !slices.Equal(added.Unit.Footprint, []string{"internal.trace"}) ||
		!slices.Equal(added.Unit.Acceptance, []string{"Resolved: spec#1: no merged unit serves it", "Resolved: spec#2: Dedupe breaks with the merged dependency."}) || strings.Contains(added.Gap, "Rename") {
		t.Fatalf("the repair %+v", added)
	}
	report.Criteria[0] = FinalCriterion{Criterion: "spec#1", Evidence: "served by merged units resume"}
	report.Findings = report.Findings[1:]
	if added, found, err := a.followup(stream, report, 1); err != nil || found {
		t.Fatalf("advisory findings built a repair %+v: %v", added, err)
	}
}

// A feature branch that does not replay cleanly onto upstream fails its
// final review: the branch stays where it was, no committee member runs, and
// the failed report names the conflicted paths. A failed report authorises
// nothing, and the same branch and documents are not reviewed again.
func TestFinalReviewThatCannotRebaseFailsAndAuthorisesNothing(t *testing.T) {
	t.Parallel()
	f, stream, repository, a := newFinalFixture(t, "conflicted")
	ctx := context.Background()
	head, op := assembleBoth(t, f, repository, a, stream,
		map[string]string{"CODEOWNERS": "/internal/ @feature\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	upstream := advanceUpstream(t, f, map[string]string{"CODEOWNERS": "/internal/ @upstream\n"})
	runs := len(f.runs())
	result, err := a.Apply(ctx, op)
	must(t, err)
	if result.Outcome != "failed" || !strings.Contains(result.Evidence, "CODEOWNERS conflicted") {
		t.Fatalf("final review result %+v", result)
	}
	if tip, _, err := workspaces(f.s.cfg, branchesDirectory, config.WorkspacesGit).Branch(ctx, featureBranch(stream)); err != nil || tip != head {
		t.Fatalf("a conflicted rebase moved the feature branch to %s: %v", tip, err)
	}
	if ran := f.runs()[runs:]; len(ran) != 0 {
		t.Fatalf("a committee member ran %v on a branch that did not rebase", ran)
	}
	report, found, err := latestFinalReport(repository, stream)
	if err != nil || !found || report.Outcome != finalFailed || report.Commit != "" || report.Before != head || !slices.Equal(report.Conflicts, []string{"CODEOWNERS"}) ||
		report.Upstream == nil || report.Upstream.Commit != upstream || len(report.Criteria) != 0 ||
		report.Failure != fmt.Sprintf("feature branch %s does not rebase cleanly onto upstream/main at %s: CODEOWNERS conflicted; the branch is left at %s", featureBranch(stream), upstream, head) {
		t.Fatalf("the failed report %+v: %v", report, err)
	}
	if len(streamDocuments(t, repository, stream, finalRebaseDocument)) != 0 {
		t.Fatal("a conflicted rebase recorded a rebase")
	}
	failed := transitionByID(t, repository, stream, "final-review-1-failed")
	if failed.To != "failed-1" || failed.Reason != result.Evidence {
		t.Fatalf("the failed transition %+v", failed)
	}
	if body := noticeOf(t, repository, stream, failed.ID); !strings.HasPrefix(body, "The final review of the assembled branch failed: feature branch ") || !strings.HasSuffix(body, "The failed review authorises nothing; a new review runs once the branch or its governing documents change.") {
		t.Fatalf("the notice %q", body)
	}
	if _, reason, err := a.finalGate(ctx, stream); err != nil || reason != "final review 1 failed: "+report.Failure {
		t.Fatalf("a failed review authorises: %q %v", reason, err)
	}

	// Once a restarted service completed the operation from its record, the
	// failed review is not asked for again with nothing changed.
	repository.Close()
	f.start(t)
	if ops := awaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return finalOperations(t, f.repository(), stream) }); len(ops) != 1 || !reflect.DeepEqual(ops[0].Result, &result) {
		t.Fatalf("final review operations after a restart %+v", ops)
	}
	f.stop(t)
	repository, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	defer repository.Close()
	a = &finalReviewer{s: f.s, repository: repository}
	must(t, a.Pass(ctx))
	if ops := finalOperations(t, repository, stream); len(ops) != 1 {
		t.Fatalf("the failed review was asked for again with nothing changed: %+v", ops)
	}
	if after, _, err := latestFinalReport(repository, stream); err != nil || !reflect.DeepEqual(after, report) {
		t.Fatalf("the failed report after a restart %+v: %v", after, err)
	}
}

// A final review interrupted after its rebase is recorded and before the
// branch moved moves the branch to the recorded commit on retry, even when
// upstream moved again, and the committee member reads that commit. A reader
// that records no report fails the review, which authorises nothing.
func TestInterruptedFinalReviewKeepsItsRecordedRebase(t *testing.T) {
	t.Parallel()
	f, stream, repository, a := newFinalFixture(t, "interrupted")
	ctx := context.Background()
	head, op := assembleBoth(t, f, repository, a, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	first := advanceUpstream(t, f, map[string]string{"FIRST.md": "first\n"})
	f.s.boundary = func(name string) error {
		if name == "final-rebase-recorded" {
			return errors.New("the service stopped")
		}
		return nil
	}
	if _, err := a.Apply(ctx, op); err == nil || !strings.Contains(err.Error(), "the service stopped") {
		t.Fatalf("the interrupted review: %v", err)
	}
	f.s.boundary = nil
	g := workspaces(f.s.cfg, branchesDirectory, config.WorkspacesGit)
	if tip, _, err := g.Branch(ctx, featureBranch(stream)); err != nil || tip != head {
		t.Fatalf("the branch moved to %s before the review resumed: %v", tip, err)
	}
	var rebase FinalRebase
	rebases := streamDocuments(t, repository, stream, finalRebaseDocument)
	if len(rebases) != 1 || json.Unmarshal([]byte(rebases[0].Content), &rebase) != nil || rebase.Before != head || rebase.Upstream.Commit != first {
		t.Fatalf("the recorded rebase %+v", rebases)
	}
	advanceUpstream(t, f, map[string]string{"SECOND.md": "second\n"})
	var problems []error
	f.finalTurn(1, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		if got, err := readTool(ctx, tools, "branch/FIRST.md"); err != nil || got != "first\n" {
			problems = append(problems, fmt.Errorf("FIRST.md holds %q: %v", got, err))
		}
		if got, err := readTool(ctx, tools, "branch/SECOND.md"); err == nil {
			problems = append(problems, fmt.Errorf("the reader sees upstream's later commit: %q", got))
		}
		return nil
	})
	result, err := a.Apply(ctx, op)
	must(t, err)
	if err := errors.Join(problems...); err != nil {
		t.Fatal(err)
	}
	if tip, _, err := g.Branch(ctx, featureBranch(stream)); err != nil || tip != rebase.Commit {
		t.Fatalf("the branch is at %s, not the recorded rebase %s: %v", tip, rebase.Commit, err)
	}
	report, _, err := latestFinalReport(repository, stream)
	must(t, err)
	if result.Outcome != "failed" || report.Outcome != finalFailed || report.Commit != rebase.Commit || report.Failure != "the committee member ended its turn without recording a report" {
		t.Fatalf("result %+v, report %+v", result, report)
	}
	if _, reason, err := a.finalGate(ctx, stream); err != nil || reason != "final review 1 failed: the committee member ended its turn without recording a report" {
		t.Fatalf("a failed review authorises: %q %v", reason, err)
	}
}

// The service runs the final review's checks before the read and records
// every run whatever its result. A review interrupted after a run is recorded
// reads it back on retry instead of running the checks again. A run that did
// not complete is never read: the review waits and runs the checks again,
// waiting longer after each run, and after maxIncompleteChecks such runs in a
// row the owner's inbox reports them until a run completes.
func TestFinalReviewWaitsForChecksThatComplete(t *testing.T) {
	t.Parallel()
	f, stream, repository, a := newFinalFixture(t, "final-checks")
	ctx := context.Background()
	_, op := assembleBoth(t, f, repository, a, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	// A check record without captured output does not reserve an output revision.
	prior, err := json.Marshal(FinalCheckRun{Review: 1, Commit: "another-candidate", Status: ChecksPassed})
	must(t, err)
	must(t, repository.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: finalChecksDocument(1), Revision: 1, Project: repository.Project(), Workstream: stream, At: f.clock.Now(), Actor: checksActor, Cause: "fixture"}, Path: finalChecksPath(1), Content: string(prior)}}))
	rawOutput := "engine diagnostic start\n" + strings.Repeat("transport detail\n", 6000) + "engine diagnostic end\n"
	runs, complete := 0, false
	f.s.options.reviewChecks = checkFunc(func(context.Context, string) (CheckResult, error) {
		runs++
		if complete {
			return CheckResult{Output: "all proofs passed"}, nil
		}
		return CheckResult{ExitCode: -1, Output: rawOutput}, errors.New("engine unreachable")
	})
	f.s.boundary = func(name string) error {
		if name == "final-checks-recorded" {
			return errors.New("the service stopped")
		}
		return nil
	}
	if _, err := a.Apply(ctx, op); err == nil || !strings.Contains(err.Error(), "the service stopped") {
		t.Fatalf("the interrupted review: %v", err)
	}
	f.s.boundary = nil
	read := 0
	f.finalTurn(1, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		read++
		_, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Uploads resume.", "findings": []any{}})
		return err
	})
	if _, err := a.Apply(ctx, op); err == nil || !strings.Contains(err.Error(), "final review 1 waits for its checks: 1 runs in a row on") || !strings.Contains(err.Error(), "engine unreachable") {
		t.Fatalf("the review read a commit whose checks did not complete: %v", err)
	}
	if runs != 1 || read != 0 {
		t.Fatalf("the checks ran %d times and the reader %d times before the wait passed", runs, read)
	}
	var run FinalCheckRun
	docs := streamDocuments(t, repository, stream, finalChecksDocument(1))
	if len(docs) != 2 || json.Unmarshal([]byte(docs[1].Content), &run) != nil || run.Status != ChecksIncomplete || run.Error != "engine unreachable" || run.Commit == "" || run.OutputRevision != 1 {
		t.Fatalf("the final check run %+v", docs)
	}
	outputDocs := streamDocuments(t, repository, stream, finalChecksDocument(1)+"-output")
	if len(outputDocs) != 1 || outputDocs[0].Content != rawOutput || outputDocs[0].Path != run.OutputPath || !run.Truncated || !strings.Contains(run.Output, "engine diagnostic start") || !strings.Contains(run.Output, "engine diagnostic end") {
		t.Fatal("final check recovery lost captured output or diagnostics")
	}
	entry := func() (InboxEntry, bool) {
		t.Helper()
		state, err := repository.Workflow(stream, finalReviewSubject)
		must(t, err)
		e, open, err := finalChecksEntry(repository, trace.WorkstreamStatus{Workstream: stream, Subjects: map[string]trace.WorkflowState{finalReviewSubject: state}})
		must(t, err)
		return e, open
	}
	for n, wait := range []time.Duration{checkRetryDelay, 2 * checkRetryDelay} {
		if _, open := entry(); open {
			t.Fatalf("the inbox reports %d incomplete runs", n+1)
		}
		f.clock.now = f.clock.now.Add(wait + time.Minute)
		if _, err := a.Apply(ctx, op); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%d runs in a row", n+2)) {
			t.Fatalf("run %d: %v", n+2, err)
		}
	}
	if runs != 3 || read != 0 {
		t.Fatalf("the checks ran %d times and the reader %d times", runs, read)
	}
	e, open := entry()
	if !open || e.Kind != InboxFinalChecks || e.Revision != 1 || !strings.Contains(e.Question, "did not complete 3 times in a row") || !strings.Contains(e.Question, "Last failure: engine unreachable") || !strings.Contains(e.Blocked, "waiting longer after each run") {
		t.Fatalf("the inbox entry %+v, open %v", e, open)
	}
	complete = true
	if _, err := a.Apply(ctx, op); err == nil {
		t.Fatal("the checks ran again before the longer wait passed")
	}
	f.clock.now = f.clock.now.Add(finalCheckDelay(3) + time.Minute)
	result, err := a.Apply(ctx, op)
	must(t, err)
	if result.Outcome != "succeeded" || runs != 4 || read != 1 {
		t.Fatalf("result %+v after %d check runs and %d reads", result, runs, read)
	}
	reader, err := repository.Thread(stream, committeeAgent(1))
	must(t, err)
	if prompt := reader.Turns[len(reader.Turns)-1].Request.Prompt; !strings.Contains(prompt, "The run passed.") || strings.Contains(prompt, "engine unreachable") {
		t.Fatalf("the reader's prompt:\n%s", prompt)
	}
	if _, open := entry(); open {
		t.Fatal("the inbox still reports the checks once a run completed")
	}
}

// A final review interrupted before its replay, after its rebase is
// recorded, or after the branch moved to the rebased commit is reconciled on
// retry: the retry's inspection observes how far the interrupted attempt
// got, the branch is replayed once, final/rebase.json records one rebase,
// the branch holds the landed commits once on upstream, and the review reads
// and reports the rebased commit.
func TestInterruptedFinalRebaseIsReconciled(t *testing.T) {
	t.Parallel()
	evidence := map[string]string{
		"final-rebase-replaying": "has recorded no rebase",
		"final-rebase-recorded":  "the branch moves to it without replaying again",
		"final-rebase-moved":     "the review reads it without replaying again",
	}
	for _, point := range []string{"final-rebase-replaying", "final-rebase-recorded", "final-rebase-moved"} {
		t.Run(point, func(t *testing.T) {
			t.Parallel()
			f, stream, repository, a := newFinalFixture(t, point)
			ctx := context.Background()
			head, op := assembleBoth(t, f, repository, a, stream,
				map[string]string{"internal/trace/resume.go": "package trace\n"},
				map[string]string{"internal/trace/dedupe.go": "package trace\n"})
			upstream := advanceUpstream(t, f, map[string]string{"UPSTREAM.md": "upstream\n"})
			replays, stop := 0, true
			f.s.boundary = func(name string) error {
				if name == point && stop {
					stop = false
					return errors.New("the service stopped")
				}
				if name == "final-rebase-replaying" {
					replays++
				}
				return nil
			}
			if _, err := a.Apply(ctx, op); err == nil || !strings.Contains(err.Error(), "the service stopped") {
				t.Fatalf("the review never reached %s: %v", point, err)
			}
			if observed, err := a.Inspect(ctx, op); err != nil || observed.State != coreadapter.EffectAbsent || !strings.Contains(observed.Evidence, evidence[point]) {
				t.Fatalf("inspection after an interruption at %s: %+v %v", point, observed, err)
			}
			var problems []error
			f.finalTurn(1, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
				got, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Uploads resume and skip acknowledged chunks.", "findings": []any{}})
				if err != nil || !strings.Contains(got, `"recorded":true`) {
					problems = append(problems, fmt.Errorf("the report was not recorded: %s %v", got, err))
				}
				return nil
			})
			result := settleOperation(t, f.s, repository, stream, op, a)
			if err := errors.Join(problems...); err != nil {
				t.Fatal(err)
			}
			tip, _, err := workspaces(f.s.cfg, branchesDirectory, config.WorkspacesGit).Branch(ctx, featureBranch(stream))
			must(t, err)
			var rebase FinalRebase
			rebases := streamDocuments(t, repository, stream, finalRebaseDocument)
			if replays != 1 || len(rebases) != 1 || json.Unmarshal([]byte(rebases[0].Content), &rebase) != nil || rebase.Before != head || rebase.Upstream.Commit != upstream || rebase.Commit != tip {
				t.Fatalf("%d replays; the recorded rebases %+v; the branch is at %s", replays, rebases, tip)
			}
			home := filepath.Dir(f.clone)
			landed := strings.TrimSpace(demoGit(t, home, "-C", f.clone, "rev-list", "--count", strings.TrimSpace(demoGit(t, home, "-C", f.clone, "merge-base", head, upstream))+".."+head))
			if count := strings.TrimSpace(demoGit(t, home, "-C", f.clone, "rev-list", "--count", upstream+".."+tip)); count != landed {
				t.Fatalf("the rebased branch holds %s commits on upstream, %s landed", count, landed)
			}
			report, _, err := latestFinalReport(repository, stream)
			must(t, err)
			if result.Outcome != "succeeded" || report.Outcome != finalReviewed || report.Commit != tip {
				t.Fatalf("result %+v, report %+v", result, report)
			}
			if ops := finalOperations(t, repository, stream); len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" {
				t.Fatalf("final review operations %+v", ops)
			}
		})
	}
}
