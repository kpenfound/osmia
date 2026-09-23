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

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/followup"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

// newFinalFixture builds validPlan with a committee runner and no masons,
// and stops the service. It returns the workstream, the trace, open, and the
// assembly controller over it.
func newFinalFixture(t *testing.T, key string) (*shedFixture, config.WorkstreamID, *trace.Repository, *finalReviewer) {
	t.Helper()
	f := newDebateFixture(t, 1, 1)
	f.upstream(t)
	stream, _ := f.builtAs(t, key)
	f.stop(t)
	repository, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	t.Cleanup(func() { repository.Close() })
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
		demoGit(t, home, "clone", "--quiet", filepath.Join(home, "remotes", "dagger", "dagger.git"), scratch)
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

// noticeOf returns the notice the transition raised for the chief of staff.
func noticeOf(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, transition string) string {
	t.Helper()
	outbox, err := repository.Outbox(stream)
	must(t, err)
	for _, entry := range outbox {
		if entry.TransitionID == transition && entry.Event.Kind == trace.NoticeKind {
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
// in a read-only view. The report it records accounts for every sealed
// criterion with evidence or a gap; one naming only some is refused. The
// report records the reviewed commit and every governing revision, survives
// a restart, and authorises later steps until the branch, the spec or the
// charter changes, when a new review is asked for.
func TestFinalReviewReadsTheRebasedBranchAgainstEverySealedCriterion(t *testing.T) {
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
	turn := f.finalTurn(1, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		for path, want := range map[string]string{plan.SpecPath: validSpec, plan.PlanPath: validPlan, "charter.md": shedCharter,
			"branch/internal/trace/dedupe.go": "package trace\n\n// Dedupe skips.\n", "branch/UPSTREAM.md": "upstream moved\n"} {
			if got, err := readTool(ctx, tools, path); err != nil || got != want {
				problems = append(problems, fmt.Errorf("%s holds %q: %v", path, got, err))
			}
		}
		diff, err := readTool(ctx, tools, "branch.diff")
		if err != nil || !strings.Contains(diff, "internal/trace/resume.go") || !strings.Contains(diff, "internal/trace/dedupe.go") || strings.Contains(diff, "UPSTREAM.md") {
			problems = append(problems, fmt.Errorf("branch.diff %q: %v", diff, err))
		}
		if report, err := readTool(ctx, tools, "units/resume/report.json"); err == nil {
			problems = append(problems, fmt.Errorf("a unit that never reported shows a report %q", report))
		}
		if got, err := callTool(ctx, tools, FinalReportTool, map[string]any{"criteria": []any{map[string]any{"criterion": "spec#1", "evidence": "TestResume"}}}); err != nil || !strings.Contains(got, "missing spec#2") {
			problems = append(problems, fmt.Errorf("a partial report was not refused: %s %v", got, err))
		}
		if got, err := callTool(ctx, tools, FinalReportTool, map[string]any{"criteria": []any{map[string]any{"criterion": "spec#1", "evidence": "x", "gap": "y"}, map[string]any{"criterion": "spec#2", "gap": "y"}}}); err != nil || !strings.Contains(got, "not both") {
			problems = append(problems, fmt.Errorf("a criterion with evidence and a gap was not refused: %s %v", got, err))
		}
		got, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Both criteria are shown.", "criteria": []any{
			map[string]any{"criterion": "spec#2", "evidence": "internal/trace/dedupe.go skips acknowledged chunks."},
			map[string]any{"criterion": "spec#1", "evidence": "internal/trace/resume.go resumes from the last chunk."}}})
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

	g := featureWorkspaces(f.s.cfg)
	tip, _, err := g.Branch(ctx, featureBranch(stream))
	must(t, err)
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
		Reader: committeeAgent(1), Turn: turn, Summary: "Both criteria are shown.", Criteria: []FinalCriterion{
			{Criterion: "spec#1", Text: "An interrupted upload resumes from the last acknowledged chunk.", Evidence: "internal/trace/resume.go resumes from the last chunk."},
			{Criterion: "spec#2", Text: "Acknowledged chunks are never sent again.", Evidence: "internal/trace/dedupe.go skips acknowledged chunks."}}}
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
	wantReason := fmt.Sprintf("final review 1 read %s at %s against seal 1 (spec revision 1, plan revision 1, charter revision %d): 2 of 2 criteria shown; gaps: none; report final/report.json revision 1", featureBranch(stream), tip, charter.Revision)
	if reviewed.Subject != finalReviewSubject || reviewed.From != "requested-1" || reviewed.To != "reviewed-1" || reviewed.Cause != op.ID || reviewed.Reason != wantReason || result.Evidence != wantReason {
		t.Fatalf("the reviewed transition %+v, result %+v", reviewed, result)
	}
	if body := noticeOf(t, repository, stream, reviewed.ID); body != "The final review of the assembled branch is recorded: 2 of 2 criteria are shown; not shown: none." {
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

func TestFinalReviewGapBecomesAnAssembledFollowupAndRequiresRereview(t *testing.T) {
	t.Parallel()
	f, stream, repository, a := newFinalFixture(t, "gaps")
	ctx := context.Background()
	_, op := assembleBoth(t, f, repository, a, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	f.finalTurn(1, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := callTool(ctx, tools, FinalReportTool, map[string]any{"criteria": []any{
			map[string]any{"criterion": "spec#1", "evidence": "resume.go"},
			map[string]any{"criterion": "spec#2", "gap": "The branch has no proof that acknowledged chunks are skipped."},
		}})
		return err
	})
	_, err := a.Apply(ctx, op)
	must(t, err)
	added, err := followup.Read(repository, stream)
	must(t, err)
	if len(added) != 1 || added[0].Review != 1 || added[0].Report != 1 || added[0].Criterion != "spec#2" || added[0].Gap == "" {
		t.Fatalf("follow-up trace %+v", added)
	}
	id := added[0].Unit.ID
	if len(added[0].Unit.Addresses) != 1 || added[0].Unit.Addresses[0].Criterion != "spec#2" || len(added[0].Unit.Footprint) == 0 {
		t.Fatalf("follow-up is not actionable: %+v", added[0])
	}
	if plans := streamDocuments(t, repository, stream, plan.PlanDocument); len(plans) != 1 || plans[0].Content != validPlan {
		t.Fatalf("follow-up changed the ratified plan: %+v", plans)
	}
	if docs := streamDocuments(t, repository, stream, followup.DocumentID); len(docs) != 1 || docs[0].Path != followup.Path || docs[0].Cause != "final-review-1-reviewed" {
		t.Fatalf("follow-up documents %+v", docs)
	}
	if feature, err := repository.Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != AssembledState {
		t.Fatalf("feature after gap %+v: %v", feature, err)
	}
	b, found, err := (&masons{s: f.s, cfg: f.s.cfg, repository: repository}).read(stream)
	must(t, err)
	if !found || b.states[trace.UnitSubject(id)].Value != UnitReady || !slices.ContainsFunc(b.plan.Units, func(u plan.Unit) bool { return u.ID == id }) {
		t.Fatalf("follow-up is not scheduled: %+v", b)
	}
	m := &masons{s: f.s, cfg: f.s.cfg, repository: repository}
	masonBundle, err := m.bundle(ctx, stream, id)
	must(t, err)
	if masonBundle.Followup == nil || masonBundle.Followup.Gap != added[0].Gap || !strings.Contains(masonBundle.Render(), added[0].Gap) {
		t.Fatalf("mason cannot see the gap: %+v", masonBundle.Followup)
	}
	sealed, _, _, err := seal.Latest(repository, stream)
	must(t, err)
	footprint, err := reviewFootprint(repository, stream, sealed, id)
	must(t, err)
	if len(footprint.Entities) == 0 || len(footprint.Paths) == 0 {
		t.Fatalf("reviewer lacks the follow-up footprint: %+v", footprint)
	}
	started, err := m.start(ctx, b, id)
	must(t, err)
	if !started {
		t.Fatal("the normal mason controller did not start the follow-up")
	}
	if state, err := repository.Workflow(stream, trace.UnitSubject(id)); err != nil || state.Value != UnitImplementing {
		t.Fatalf("follow-up did not enter implementing: %+v %v", state, err)
	}
	if _, reason, err := a.finalGate(ctx, stream); err != nil || !strings.Contains(reason, "unresolved gap for spec#2") {
		t.Fatalf("gap passed delivery gate: %q %v", reason, err)
	}
	must(t, a.Pass(ctx))
	if ops := finalOperations(t, repository, stream); len(ops) != 1 {
		t.Fatalf("review asked before landing: %+v", ops)
	}
	mergeDirectly(t, f, repository, stream, id, map[string]string{"internal/trace/proof_test.go": "package trace\n"})
	if feature, err := repository.Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != AssembledState {
		t.Fatalf("landing moved the feature: %+v %v", feature, err)
	}
	if _, reason, err := a.finalGate(ctx, stream); err != nil || !strings.Contains(reason, "stale") {
		t.Fatalf("old review after landing: %q %v", reason, err)
	}
	repository.Close()
	f.start(t)
	awaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return finalOperations(t, f.repository(), stream) })
	f.stop(t)
	repository, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	defer repository.Close()
	a = &finalReviewer{s: f.s, repository: repository}
	must(t, a.Pass(ctx))
	ops := finalOperations(t, repository, stream)
	if len(ops) != 2 {
		t.Fatalf("new review not asked: %+v", ops)
	}
	f.finalTurn(2, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := callTool(ctx, tools, FinalReportTool, map[string]any{"criteria": []any{
			map[string]any{"criterion": "spec#1", "evidence": "resume.go"},
			map[string]any{"criterion": "spec#2", "evidence": "proof_test.go shows acknowledged chunks are skipped"},
		}})
		return err
	})
	result, err := a.Apply(ctx, ops[1].Operation)
	must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("rerun failed: %+v", result)
	}
	if report, reason, err := a.finalGate(ctx, stream); err != nil || reason != "" || report.Review != 2 {
		t.Fatalf("rerun did not clear gap: review %+v, reason %q: %v", report, reason, err)
	}
	must(t, os.WriteFile(filepath.Join(f.trace, "workstreams", string(stream), plan.SpecPath), []byte(strings.Replace(validSpec, "never sent again", "sometimes resent", 1)), 0600))
	if _, err := (&masons{s: f.s, cfg: f.s.cfg, repository: repository}).bundle(ctx, stream, id); !errors.Is(err, bundle.ErrStaleSpec) {
		t.Fatalf("follow-up accepted unratified spec change: %v", err)
	}
}

func TestFinalReviewBehaviorAndEvidenceGapsStayExplicit(t *testing.T) {
	t.Parallel()
	_, stream, _, a := newFinalFixture(t, "both-gaps")
	added, err := a.followups(stream, FinalReport{Review: 1, Spec: 1, Plan: 1, Criteria: []FinalCriterion{
		{Criterion: "spec#1", Gap: "Resume ignores the checkpoint."},
		{Criterion: "spec#2", Gap: "No test shows acknowledged chunks are skipped."},
	}}, 1)
	must(t, err)
	if len(added) != 2 || added[0].Criterion != "spec#1" || added[0].Gap != "Resume ignores the checkpoint." ||
		added[1].Criterion != "spec#2" || added[1].Gap != "No test shows acknowledged chunks are skipped." ||
		added[0].Unit.ID == added[1].Unit.ID || len(added[0].Unit.Footprint) == 0 || len(added[1].Unit.Footprint) == 0 {
		t.Fatalf("gaps were collapsed or lost: %+v", added)
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
	if tip, _, err := featureWorkspaces(f.s.cfg).Branch(ctx, featureBranch(stream)); err != nil || tip != head {
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
	g := featureWorkspaces(f.s.cfg)
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
