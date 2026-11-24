package executionjourney

import (
	context "context"
	json "encoding/json"
	fmt "fmt"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	filepath "path/filepath"
	reflect "reflect"
	slices "slices"
	strings "strings"
	atomic "sync/atomic"
	testing "testing"

	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
)

var dedupeReport = "skip acknowledged chunks"

func newLandingFixture(t *testing.T, drafted string) (*shedFixture, *intakefixture.FakeMasons) {
	t.Helper()
	f, masons := newMasonFixture(t, 1, drafted)
	chief := &intakefixture.Chief{P: &intakefixture.Faults{}, Released: map[string]bool{}, Held: map[string]chan struct{}{}}
	intakefixture.ScriptLanding(f.engine, masons, execution.MasonTurnID, chief.Turn)
	return f, masons
}

func landOperations(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	return intakefixture.LandOperations(t, repository, stream, service.LandAction)
}

func approvedReview(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string) (trace.Document, service.UnitReviewResult) {
	t.Helper()
	f := &foreman{masons: &masons{repository: repository}}
	return intakefixture.ApprovedReview(t, stream, unit, f.approval)
}

func (f *shedFixture) landedCommits(t *testing.T, stream config.WorkstreamID, base string) []string {
	t.Helper()
	return intakefixture.LandedCommits(t, filepath.Dir(f.clone), f.clone, stream, base)
}

func (f *shedFixture) awaitMerged(t *testing.T, stream config.WorkstreamID, unit string) {
	t.Helper()
	intakefixture.AwaitMerged(t, f.sharedTurns(), stream, unit, func(t *testing.T) []trace.OperationRecord { return landOperations(t, f.repository(), stream) })
}

func TestApprovedUnitLandsAndReadiesItsDependent(t *testing.T) {
	t.Parallel()
	f, masons := newLandingFixture(t, fixturekit.ValidPlan)
	defer f.stop(t)
	stream, _ := f.builtAs(t, "landing")
	f.awaitMerged(t, stream, "resume")
	f.awaitUnit(t, stream, "dedupe", intake.UnitImplementing)
	masons.Check(t)

	review, result := approvedReview(t, f.repository(), stream, "resume")
	candidate, base := result.Identity.Candidate.Revision, result.Identity.Candidate.BaseRevision
	commits := f.landedCommits(t, stream, base)
	if len(commits) != 1 {
		t.Fatalf("the feature branch gained %d commits: %v", len(commits), commits)
	}
	commit := commits[0]
	git := intake.Workspaces(f.s.cfg, intake.BranchesDirectory, config.WorkspacesGit)
	landed, err := git.Commit(context.Background(), commit)
	fixturekit.Must(t, err)
	tree, err := git.Commit(context.Background(), candidate)
	fixturekit.Must(t, err)
	if landed.Tree != tree.Tree || !slices.Equal(landed.Parents, []string{base}) {
		t.Fatalf("the landed commit %+v is not candidate %s's tree on %s", landed, candidate, base)
	}
	ops := landOperations(t, f.repository(), stream)
	if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" {
		t.Fatalf("landing operations %+v", ops)
	}
	op := ops[0].Operation.ID
	approval := fmt.Sprintf("units/resume/review.json revision %d", review.Revision)
	wantMessage := fmt.Sprintf("Resume from the last chunk\n\nUnit resume (Resume from the last chunk) of workstream %s serves:\n- spec#1: An interrupted upload resumes from the last acknowledged chunk.\n\nOsmia-Workstream: %s\nOsmia-Unit: resume\nOsmia-Candidate: %s\nOsmia-Base: %s\nOsmia-Approval: %s\nOsmia-Operation: %s", stream, stream, candidate, base, approval, op)
	if landed.Message != wantMessage {
		t.Fatalf("the landing message:\n%s\nwant\n%s", landed.Message, wantMessage)
	}
	if who := strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(f.clone), "-C", f.clone, "log", "-1", "--format=%an <%ae>", commit)); who != "Osmia <osmia@localhost>" {
		t.Fatalf("the landing's author: %s", who)
	}

	docs, err := trace.Read[trace.Document](f.repository(), stream)
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

	transitions := intakefixture.AllTransitions(t, f.s.cfg.Root, f.project, stream)
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
	if got := strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(f.clone), "-C", f.clone, "merge-base", intake.FeatureBranch(stream), execution.UnitBranch(stream, "dedupe"))); got != commit {
		t.Fatalf("the dependent unit started from %s, not the landed %s", got, commit)
	}
	f.awaitEventTurns(t, stream, fmt.Sprintf("Unit resume merged: it landed as %s on %s.", commit, intake.FeatureBranch(stream)))
}

func approveDirectly(t *testing.T, s *Service, repository *trace.Repository, stream config.WorkstreamID, unit, criterion string) {
	t.Helper()
	r := &reviewers{masons: newMasonController(s, repository)}
	intakefixture.ApproveDirectly(t, repository, stream, unit, r.prepareUnitReview, func(identity service.UnitReviewIdentity, turn string) ([]byte, error) {
		return json.Marshal(service.UnitReviewResult{Identity: identity, Turn: turn, Verdict: service.UnitVerdict{Decision: "satisfactory", Summary: reviewSummary}})
	}, execution.ReviewTurnID, execution.ReviewDocument, r.one, intake.UnitApproved)
}

func newApprovedFixture(t *testing.T, key string) (*shedFixture, config.WorkstreamID, *trace.Repository) {
	t.Helper()
	return newApprovedFixtureOn(t, key, config.WorkspacesGit)
}

func newApprovedFixtureOn(t *testing.T, key, backend string) (*shedFixture, config.WorkstreamID, *trace.Repository) {
	t.Helper()
	f, _ := newMasonFixtureOn(t, backend, "masons = 1\n", independentPlan)
	base := strings.TrimSpace(fixturekit.DemoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	stream, repository := seedBuild(t, f, key, independentPlan, backend, base)
	intakefixture.SeedApprovedUnits(t, func(u, o string) { seedReview(t, f, repository, stream, u, o) }, func(u, c string) { approveDirectly(t, f.s, repository, stream, u, c) })
	return f, stream, repository
}

func TestLandingIsSerialAndARebasedApprovalReturnsToReview(t *testing.T) {
	t.Parallel()
	f, stream, repository := newApprovedFixture(t, "serial")
	if change, err := execution.ReadUnitChange(repository, stream, "dedupe"); err != nil || change != "" {
		t.Fatalf("a unit on Git worktrees records change %q: %v", change, err)
	}
	lands := &foreman{masons: newMasonController(f.s, repository)}
	ctx := context.Background()
	for range 2 {
		fixturekit.Must(t, lands.Pass(ctx))
	}
	ops := landOperations(t, repository, stream)
	if len(ops) != 1 {
		t.Fatalf("%d landings asked for while one has no result: %+v", len(ops), ops)
	}
	first, err := execution.DecodeLand(ops[0].Operation)
	fixturekit.Must(t, err)
	if first.Unit != "resume" {
		t.Fatalf("the first landing is of %s, not the first unit of the plan", first.Unit)
	}
	_, approved := approvedReview(t, repository, stream, "dedupe")
	fixturekit.Must(t, repository.Close())
	var passed atomic.Int64
	f.opts.schedulePassed = func() { passed.Add(1) }
	f.start(t)
	defer f.stop(t)

	f.awaitMerged(t, stream, "resume")
	f.awaitUnit(t, stream, "dedupe", intake.UnitReviewing)
	var rebases []trace.OperationRecord
	intakefixture.Soon(t, "dedupe's rebase to finish", func() bool {
		rebases = rebaseOperations(t, f.repository(), stream, "dedupe")
		return len(rebases) == 1 && rebases[0].Result != nil
	})
	intakefixture.AwaitFurtherPasses(t, &passed, 2)
	if got := landOperations(t, f.repository(), stream); len(got) != 1 {
		t.Fatalf("a landing was asked for after the first: %+v", got)
	}
	_, result := approvedReview(t, f.repository(), stream, "resume")
	commits := f.landedCommits(t, stream, result.Identity.Candidate.BaseRevision)
	if len(commits) != 1 {
		t.Fatalf("the feature branch holds %v", commits)
	}
	landed := commits[0]
	recorded := unitRebases(t, f.repository(), stream, "dedupe")
	if len(recorded) != 1 {
		t.Fatalf("rebase records %+v", recorded)
	}
	rebase := recorded[0]
	if rebase.State != intake.UnitApproved || rebase.Onto != landed || rebase.Snapshot != approved.Identity.Candidate.Revision || rebase.Base != approved.Identity.Candidate.BaseRevision || len(rebase.Conflicts) != 0 {
		t.Fatalf("rebase.json %+v", rebase)
	}
	if parent := strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(f.clone), "-C", f.clone, "log", "-1", "--format=%P", execution.UnitBranch(stream, "dedupe"))); parent != landed || rebase.Commit != strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(f.clone), "-C", f.clone, "rev-parse", execution.UnitBranch(stream, "dedupe"))) {
		t.Fatalf("the unit branch is on %s, rebase.json names %s", parent, rebase.Commit)
	}
	doc, report := latestReport(t, f.repository(), stream, "dedupe")
	if report.Base != landed || report.Candidate != rebase.Commit || doc.Revision != rebase.Report || doc.Cause != rebases[0].Operation.ID || doc.Actor != execution.ForemanActor {
		t.Fatalf("the rebased report %+v %+v", doc.Header, report)
	}
	id := trace.UnitSubject("dedupe") + "-checking-rebase-1"
	i := slices.IndexFunc(intakefixture.AllTransitions(t, f.s.cfg.Root, f.project, stream), func(tr trace.Transition) bool { return tr.ID == id })
	if i < 0 {
		t.Fatal("the return to review is not recorded")
	}
	back := intakefixture.AllTransitions(t, f.s.cfg.Root, f.project, stream)[i]
	if back.From != intake.UnitApproved || back.To != execution.UnitChecking || back.Cause != rebases[0].Operation.ID || !strings.HasPrefix(back.Reason, "the approval no longer holds: ") {
		t.Fatalf("the return to review %+v", back)
	}
	if body := f.notice(t, stream, id); body != fmt.Sprintf("Unit dedupe returns to checking and review: its approved candidate was rebased onto %s.", landed) {
		t.Fatalf("notice %q", body)
	}
}

func TestStaleApprovalCannotLand(t *testing.T) {
	t.Parallel()
	f, stream, repository := newApprovedFixture(t, "stale")
	defer repository.Close()
	lands := &foreman{masons: newMasonController(f.s, repository)}
	ctx := context.Background()
	review, result := approvedReview(t, repository, stream, "resume")
	in := execution.LandInput{Unit: "resume", Review: review.Revision, Candidate: result.Identity.Candidate.Revision, Base: result.Identity.Candidate.BaseRevision}
	check := func(want string) {
		t.Helper()
		got, err := lands.current(ctx, stream, in, result)
		fixturekit.Must(t, err)
		if got != want {
			t.Fatalf("reason %q, want %q", got, want)
		}
	}
	check("")
	home := filepath.Dir(f.clone)
	move := func(branch, to, back string, want string) {
		t.Helper()
		fixturekit.DemoGit(t, home, "-C", f.clone, "update-ref", "refs/heads/"+branch, to)
		check(want)
		fixturekit.DemoGit(t, home, "-C", f.clone, "update-ref", "refs/heads/"+branch, back)
		check("")
	}
	move(execution.UnitBranch(stream, "resume"), in.Base, in.Candidate, "stale approval: stale candidate revision; review the current candidate again")
	move(intake.FeatureBranch(stream), in.Candidate, in.Base, "stale approval: stale base revision; review the current candidate again")
	stale := execution.LandInput{Unit: "resume", Review: review.Revision - 1, Candidate: in.Candidate, Base: in.Base}
	if got, err := lands.current(ctx, stream, stale, result); err != nil || got != fmt.Sprintf("units/resume/review.json revision %d replaced the approval", review.Revision) {
		t.Fatalf("an earlier review revision: %q %v", got, err)
	}
	intakefixture.RecordSpec(t, repository, stream, staleSpec)
	got, err := lands.current(ctx, stream, in, result)
	fixturekit.Must(t, err)
	if !strings.HasPrefix(got, "stale approval: stale review inputs: ") || !strings.Contains(got, "spec does not match its seal") {
		t.Fatalf("an edited spec: %q", got)
	}

	fixturekit.Must(t, lands.Pass(ctx))
	ops := landOperations(t, repository, stream)
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
	if commits := f.landedCommits(t, stream, in.Base); len(commits) != 0 {
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
