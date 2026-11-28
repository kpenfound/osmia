package executionjourney

import (
	context "context"
	json "encoding/json"
	fmt "fmt"
	filepath "path/filepath"
	reflect "reflect"
	slices "slices"
	strings "strings"
	atomic "sync/atomic"
	testing "testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
)

var dedupeReport = "skip acknowledged chunks"

func newLandingFixture(t *testing.T, drafted string) (*journey.ShedFixture, *intakefixture.FakeMasons) {
	t.Helper()
	f, masons := journey.NewMasonFixture(t, 1, drafted)
	manager := &intakefixture.Manager{P: &intakefixture.Faults{}, Released: map[string]bool{}, Held: map[string]chan struct{}{}}
	intakefixture.ScriptLanding(f.Engine, masons, execution.MasonTurnID, manager.Turn)
	return f, masons
}

func landedCommits(f *journey.ShedFixture, t *testing.T, stream config.WorkstreamID, base string) []string {
	t.Helper()
	return intakefixture.LandedCommits(t, filepath.Dir(f.Clone), f.Clone, stream, base)
}

func TestApprovedUnitLandsAndReadiesItsDependent(t *testing.T) {
	t.Parallel()
	f, masons := newLandingFixture(t, fixturekit.ValidPlan)
	defer f.Stop(t)
	stream, _ := f.BuiltAs(t, "landing")
	f.AwaitMerged(t, stream, "resume")
	f.AwaitUnit(t, stream, "dedupe", intake.UnitImplementing)
	masons.Check(t)

	review, result := journey.ApprovedReview(t, f.Repository(), stream, "resume")
	candidate, base := result.Identity.Candidate.Revision, result.Identity.Candidate.BaseRevision
	commits := landedCommits(f, t, stream, base)
	if len(commits) != 1 {
		t.Fatalf("the feature branch gained %d commits: %v", len(commits), commits)
	}
	commit := commits[0]
	git := intake.Workspaces(f.S.Cfg, intake.BranchesDirectory, config.WorkspacesGit)
	landed, err := git.Commit(context.Background(), commit)
	fixturekit.Must(t, err)
	tree, err := git.Commit(context.Background(), candidate)
	fixturekit.Must(t, err)
	if landed.Tree != tree.Tree || !slices.Equal(landed.Parents, []string{base}) {
		t.Fatalf("the landed commit %+v is not candidate %s's tree on %s", landed, candidate, base)
	}
	ops := journey.LandOperations(t, f.Repository(), stream)
	if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" {
		t.Fatalf("landing operations %+v", ops)
	}
	op := ops[0].Operation.ID
	approval := fmt.Sprintf("units/resume/review.json revision %d", review.Revision)
	wantMessage := fmt.Sprintf("Resume from the last chunk\n\nUnit resume (Resume from the last chunk) of workstream %s serves:\n- spec#1: An interrupted upload resumes from the last acknowledged chunk.\n\nOsmia-Workstream: %s\nOsmia-Unit: resume\nOsmia-Candidate: %s\nOsmia-Base: %s\nOsmia-Approval: %s\nOsmia-Operation: %s", stream, stream, candidate, base, approval, op)
	if landed.Message != wantMessage {
		t.Fatalf("the landing message:\n%s\nwant\n%s", landed.Message, wantMessage)
	}
	if who := strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(f.Clone), "-C", f.Clone, "log", "-1", "--format=%an <%ae>", commit)); who != "Osmia <osmia@localhost>" {
		t.Fatalf("the landing's author: %s", who)
	}

	docs, err := trace.Read[trace.Document](f.Repository(), stream)
	fixturekit.Must(t, err)
	var landings []trace.Document
	for _, d := range docs {
		if d.ID == execution.LandingDocument("resume") {
			landings = append(landings, d)
		}
	}
	if len(landings) != 1 || landings[0].Path != "units/resume/landing.json" || landings[0].Cause != op || landings[0].Actor != execution.ForemanActor {
		t.Fatalf("landing documents %+v", landings)
	}
	var landing api.UnitLanding
	fixturekit.Must(t, json.Unmarshal([]byte(landings[0].Content), &landing))
	want := api.UnitLanding{Unit: "resume", Operation: op, Approval: approval, Turn: result.Turn, Candidate: candidate, Base: base, Spec: result.Identity.Candidate.SpecRevision, Plan: result.Identity.Candidate.PlanRevision, Seal: 1, Criteria: []string{"spec#1"}, Branch: intake.FeatureBranch(stream), Commit: commit, Message: wantMessage}
	if !reflect.DeepEqual(landing, want) {
		t.Fatalf("landing %+v, want %+v", landing, want)
	}

	transitions := intakefixture.AllTransitions(t, f.S.Cfg.Root, f.Project, stream)
	merged := slices.IndexFunc(transitions, func(tr trace.Transition) bool { return tr.ID == trace.UnitSubject("resume")+"-"+intake.UnitMerged })
	ready := slices.IndexFunc(transitions, func(tr trace.Transition) bool { return tr.ID == trace.UnitSubject("dedupe")+"-"+intake.UnitReady })
	if merged < 0 || ready < merged {
		t.Fatalf("merged at %d, dependent ready at %d", merged, ready)
	}
	m, r := transitions[merged], transitions[ready]
	wantReason := fmt.Sprintf("unit resume landed as %s on %s: %s approved candidate %s from %s, spec %s, plan %s, seal 1; criteria spec#1", commit, intake.FeatureBranch(stream), approval, candidate, base, want.Spec, want.Plan)
	if m.From != intake.UnitApproved || m.To != intake.UnitMerged || m.Actor != execution.ForemanActor || m.Cause != op || m.Reason != wantReason {
		t.Fatalf("merged transition %+v", m)
	}
	if r.From != intake.UnitPlanned || r.To != intake.UnitReady || r.Cause != op || r.Reason != "unit dedupe is ready: every unit it depends on has merged: resume" {
		t.Fatalf("dependent ready transition %+v", r)
	}
	if got := strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(f.Clone), "-C", f.Clone, "merge-base", intake.FeatureBranch(stream), execution.UnitBranch(stream, "dedupe"))); got != commit {
		t.Fatalf("the dependent unit started from %s, not the landed %s", got, commit)
	}
	f.AwaitEventTurns(t, stream, fmt.Sprintf("Unit resume merged: it landed as %s on %s.", commit, intake.FeatureBranch(stream)))
}

func TestLandingIsSerialAndARebasedApprovalReturnsToReview(t *testing.T) {
	t.Parallel()
	f, stream, repository := journey.NewApprovedFixture(t, "serial")
	if change, err := execution.ReadUnitChange(repository, stream, "dedupe"); err != nil || change != "" {
		t.Fatalf("a unit on Git worktrees records change %q: %v", change, err)
	}
	lands := &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}
	ctx := context.Background()
	for range 2 {
		fixturekit.Must(t, lands.Pass(ctx))
	}
	ops := journey.LandOperations(t, repository, stream)
	if len(ops) != 1 {
		t.Fatalf("%d landings asked for while one has no result: %+v", len(ops), ops)
	}
	first, err := execution.DecodeLand(ops[0].Operation)
	fixturekit.Must(t, err)
	if first.Unit != "resume" {
		t.Fatalf("the first landing is of %s, not the first unit of the plan", first.Unit)
	}
	_, approved := journey.ApprovedReview(t, repository, stream, "dedupe")
	fixturekit.Must(t, repository.Close())
	var passed atomic.Int64
	f.Opts.SchedulePassed = func() { passed.Add(1) }
	f.Start(t)
	defer f.Stop(t)

	f.AwaitMerged(t, stream, "resume")
	f.AwaitUnit(t, stream, "dedupe", intake.UnitReviewing)
	var rebases []trace.OperationRecord
	intakefixture.Soon(t, "dedupe's rebase to finish", func() bool {
		rebases = journey.RebaseOperations(t, f.Repository(), stream, "dedupe")
		return len(rebases) == 1 && rebases[0].Result != nil
	})
	intakefixture.AwaitFurtherPasses(t, &passed, 2)
	if got := journey.LandOperations(t, f.Repository(), stream); len(got) != 1 {
		t.Fatalf("a landing was asked for after the first: %+v", got)
	}
	_, result := journey.ApprovedReview(t, f.Repository(), stream, "resume")
	commits := landedCommits(f, t, stream, result.Identity.Candidate.BaseRevision)
	if len(commits) != 1 {
		t.Fatalf("the feature branch holds %v", commits)
	}
	landed := commits[0]
	recorded := journey.UnitRebases(t, f.Repository(), stream, "dedupe")
	if len(recorded) != 1 {
		t.Fatalf("rebase records %+v", recorded)
	}
	rebase := recorded[0]
	if rebase.State != intake.UnitApproved || rebase.Onto != landed || rebase.Snapshot != approved.Identity.Candidate.Revision || rebase.Base != approved.Identity.Candidate.BaseRevision || len(rebase.Conflicts) != 0 {
		t.Fatalf("rebase.json %+v", rebase)
	}
	if parent := strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(f.Clone), "-C", f.Clone, "log", "-1", "--format=%P", execution.UnitBranch(stream, "dedupe"))); parent != landed || rebase.Commit != strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(f.Clone), "-C", f.Clone, "rev-parse", execution.UnitBranch(stream, "dedupe"))) {
		t.Fatalf("the unit branch is on %s, rebase.json names %s", parent, rebase.Commit)
	}
	doc, report := latestReport(t, f.Repository(), stream, "dedupe")
	if report.Base != landed || report.Candidate != rebase.Commit || doc.Revision != rebase.Report || doc.Cause != rebases[0].Operation.ID || doc.Actor != execution.ForemanActor {
		t.Fatalf("the rebased report %+v %+v", doc.Header, report)
	}
	id := trace.UnitSubject("dedupe") + "-checking-rebase-1"
	i := slices.IndexFunc(intakefixture.AllTransitions(t, f.S.Cfg.Root, f.Project, stream), func(tr trace.Transition) bool { return tr.ID == id })
	if i < 0 {
		t.Fatal("the return to review is not recorded")
	}
	back := intakefixture.AllTransitions(t, f.S.Cfg.Root, f.Project, stream)[i]
	if back.From != intake.UnitApproved || back.To != execution.UnitChecking || back.Cause != rebases[0].Operation.ID || !strings.HasPrefix(back.Reason, "the approval no longer holds: ") {
		t.Fatalf("the return to review %+v", back)
	}
	if body := f.Notice(t, stream, id); body != fmt.Sprintf("Unit dedupe returns to checking and review: its approved candidate was rebased onto %s.", landed) {
		t.Fatalf("notice %q", body)
	}
}

func TestStaleApprovalCannotLand(t *testing.T) {
	t.Parallel()
	f, stream, repository := journey.NewApprovedFixture(t, "stale")
	defer repository.Close()
	lands := &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}
	ctx := context.Background()
	review, result := journey.ApprovedReview(t, repository, stream, "resume")
	in := execution.LandInput{Unit: "resume", Review: review.Revision, Candidate: result.Identity.Candidate.Revision, Base: result.Identity.Candidate.BaseRevision}
	check := func(want string) {
		t.Helper()
		got, err := lands.Current(ctx, stream, in, result)
		fixturekit.Must(t, err)
		if got != want {
			t.Fatalf("reason %q, want %q", got, want)
		}
	}
	check("")
	home := filepath.Dir(f.Clone)
	move := func(branch, to, back string, want string) {
		t.Helper()
		fixturekit.DemoGit(t, home, "-C", f.Clone, "update-ref", "refs/heads/"+branch, to)
		check(want)
		fixturekit.DemoGit(t, home, "-C", f.Clone, "update-ref", "refs/heads/"+branch, back)
		check("")
	}
	move(execution.UnitBranch(stream, "resume"), in.Base, in.Candidate, "stale approval: stale candidate revision; review the current candidate again")
	move(intake.FeatureBranch(stream), in.Candidate, in.Base, "stale approval: stale base revision; review the current candidate again")
	stale := execution.LandInput{Unit: "resume", Review: review.Revision - 1, Candidate: in.Candidate, Base: in.Base}
	if got, err := lands.Current(ctx, stream, stale, result); err != nil || got != fmt.Sprintf("units/resume/review.json revision %d replaced the approval", review.Revision) {
		t.Fatalf("an earlier review revision: %q %v", got, err)
	}
	intakefixture.RecordSpec(t, repository, stream, staleSpec)
	got, err := lands.Current(ctx, stream, in, result)
	fixturekit.Must(t, err)
	if !strings.HasPrefix(got, "stale approval: stale review inputs: ") || !strings.Contains(got, "spec does not match its seal") {
		t.Fatalf("an edited spec: %q", got)
	}

	fixturekit.Must(t, lands.Pass(ctx))
	ops := journey.LandOperations(t, repository, stream)
	if len(ops) != 1 {
		t.Fatalf("landing operations %+v", ops)
	}
	observed, err := lands.Inspect(ctx, ops[0].Operation)
	fixturekit.Must(t, err)
	if observed.State != coreadapter.EffectAbsent {
		t.Fatalf("inspection %+v", observed)
	}
	outcome, err := lands.Apply(ctx, ops[0].Operation)
	fixturekit.Must(t, err)
	if outcome.Outcome != "failed" || !strings.Contains(outcome.Evidence, "unit resume did not land: stale approval: stale review inputs: ") {
		t.Fatalf("the stale landing %+v", outcome)
	}
	if commits := landedCommits(f, t, stream, in.Base); len(commits) != 0 {
		t.Fatalf("a stale approval landed %v", commits)
	}
	if state, err := repository.Workflow(stream, trace.UnitSubject("resume")); err != nil || state.Value != intake.UnitApproved {
		t.Fatalf("the refused unit is %+v: %v", state, err)
	}
	again, err := lands.Apply(ctx, ops[0].Operation)
	if err != nil || !reflect.DeepEqual(again, outcome) {
		t.Fatalf("a refused landing applied again: %+v %v", again, err)
	}
	observed, err = lands.Inspect(ctx, ops[0].Operation)
	if err != nil || observed.State != coreadapter.EffectCompleted || !reflect.DeepEqual(*observed.Result, outcome) {
		t.Fatalf("inspection after the refusal %+v %v", observed, err)
	}
}
