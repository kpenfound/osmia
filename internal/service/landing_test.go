package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// dedupeReport is a complete report on the one criterion unit dedupe of
// independentPlan and validPlan addresses.
var dedupeReport = CriterionReport{Criterion: "spec#2", Done: "skip acknowledged chunks", Evidence: "no chunk is sent twice", Proof: "reviewer judgement"}

// newLandingFixture is a mason fixture over drafted whose resume mason reports
// done and whose reviewer approves every candidate of resume it reviews.
func newLandingFixture(t *testing.T, drafted string) (*shedFixture, *fakeMasons) {
	t.Helper()
	f, masons := newMasonFixture(t, 1, drafted)
	masons.play[masonTurnID("resume")] = reportDone("Built")
	chief := &chief{p: &faults{}, released: map[string]bool{}, held: map[string]chan struct{}{}}
	f.engine.mu.Lock()
	defer f.engine.mu.Unlock()
	f.engine.turns["*"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if strings.HasPrefix(req.Name, reviewerAgent("resume")+"-review-") {
			body, err := callTool(ctx, tools, verdictTool, map[string]any{"decision": "satisfactory", "evidence": reviewEvidence(), "findings": []ReviewFinding{}})
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				return nil, fmt.Errorf("verdict %s: %v", body, err)
			}
			return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
		}
		return chief.turn(ctx, req, verified, tools)
	}
	return f, masons
}

// landOperations returns the workstream's landing operations.
func landOperations(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	ops, err := repository.Operations(stream)
	must(t, err)
	return slices.DeleteFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.Action != LandAction })
}

// approvedReview returns the latest recorded review result of the unit.
func approvedReview(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string) (trace.Document, UnitReviewResult) {
	t.Helper()
	f := &foreman{masons: &masons{repository: repository}}
	review, result, ok, err := f.approval(stream, unit, 0)
	must(t, err)
	if !ok {
		t.Fatalf("unit %s has no satisfactory review: %+v", unit, review)
	}
	return review, result
}

// landedCommits returns the commits of the feature branch since base.
func (f *shedFixture) landedCommits(t *testing.T, stream config.WorkstreamID, base string) []string {
	t.Helper()
	out := strings.TrimSpace(demoGit(t, filepath.Dir(f.clone), "-C", f.clone, "rev-list", base+".."+featureBranch(stream)))
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// awaitMerged waits until the unit merged, reporting the landing operations'
// history when it never does.
func (f *shedFixture) awaitMerged(t *testing.T, stream config.WorkstreamID, unit string) {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for {
		got, err := f.repository().Workflow(stream, trace.UnitSubject(unit))
		must(t, err)
		if got.Value == UnitMerged {
			return
		}
		if time.Now().After(deadline) {
			data, _ := json.MarshalIndent(landOperations(t, f.repository(), stream), "", "  ")
			t.Fatalf("unit %s of %s is %q, never merged; landings %s", unit, stream, got.Value, data)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// An approved unit lands as one commit of its candidate's tree on the
// approved base, with a message derived from its criterion and trailers that
// attribute it. units/<unit>/landing.json records the approval, candidate,
// base, criteria and commit; the unit's move to merged is caused by the
// landing operation, and only then does the dependent unit become ready and
// start from the landed commit. The chief of staff hears of it.
func TestApprovedUnitLandsAndReadiesItsDependent(t *testing.T) {
	t.Parallel()
	f, masons := newLandingFixture(t, validPlan)
	defer f.stop(t)
	stream, _ := f.builtAs(t, "landing")
	f.awaitMerged(t, stream, "resume")
	f.awaitUnit(t, stream, "dedupe", UnitImplementing)
	masons.check(t)

	review, result := approvedReview(t, f.repository(), stream, "resume")
	candidate, base := result.Identity.Candidate.Revision, result.Identity.Candidate.BaseRevision
	commits := f.landedCommits(t, stream, base)
	if len(commits) != 1 {
		t.Fatalf("the feature branch gained %d commits: %v", len(commits), commits)
	}
	commit := commits[0]
	git := featureWorkspaces(f.s.cfg)
	landed, err := git.Commit(context.Background(), commit)
	must(t, err)
	tree, err := git.Commit(context.Background(), candidate)
	must(t, err)
	if landed.Tree != tree.Tree || !slices.Equal(landed.Parents, []string{base}) {
		t.Fatalf("the landed commit %+v is not candidate %s's tree on %s", landed, candidate, base)
	}
	ops := landOperations(t, f.repository(), stream)
	if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" {
		t.Fatalf("landing operations %+v", ops)
	}
	op := ops[0].Operation.ID
	approval := fmt.Sprintf("units/resume/review.json revision %d", review.Revision)
	wantMessage := fmt.Sprintf("An interrupted upload resumes from the last acknowledged chunk\n\nUnit resume (Resume from the last chunk) of workstream %s meets:\n- spec#1: An interrupted upload resumes from the last acknowledged chunk.\n\nOsmia-Workstream: %s\nOsmia-Unit: resume\nOsmia-Candidate: %s\nOsmia-Base: %s\nOsmia-Approval: %s\nOsmia-Operation: %s", stream, stream, candidate, base, approval, op)
	if landed.Message != wantMessage {
		t.Fatalf("the landing message:\n%s\nwant\n%s", landed.Message, wantMessage)
	}
	if who := strings.TrimSpace(demoGit(t, filepath.Dir(f.clone), "-C", f.clone, "log", "-1", "--format=%an <%ae>", commit)); who != "Osmia <osmia@localhost>" {
		t.Fatalf("the landing's author: %s", who)
	}

	docs, err := trace.Read[trace.Document](f.repository(), stream)
	must(t, err)
	var landings []trace.Document
	for _, d := range docs {
		if d.ID == landingDocument("resume") {
			landings = append(landings, d)
		}
	}
	if len(landings) != 1 || landings[0].Path != "units/resume/landing.json" || landings[0].Cause != op || landings[0].Actor != foremanActor {
		t.Fatalf("landing documents %+v", landings)
	}
	var landing UnitLanding
	must(t, json.Unmarshal([]byte(landings[0].Content), &landing))
	want := UnitLanding{Unit: "resume", Operation: op, Approval: approval, Turn: result.Turn, Candidate: candidate, Base: base, Spec: result.Identity.Candidate.SpecRevision, Plan: result.Identity.Candidate.PlanRevision, Seal: 1, Criteria: []string{"spec#1"}, Branch: featureBranch(stream), Commit: commit, Message: wantMessage}
	if !reflect.DeepEqual(landing, want) {
		t.Fatalf("landing %+v, want %+v", landing, want)
	}

	transitions := allTransitions(t, f.trace, stream)
	merged := slices.IndexFunc(transitions, func(tr trace.Transition) bool { return tr.ID == trace.UnitSubject("resume")+"-"+UnitMerged })
	ready := slices.IndexFunc(transitions, func(tr trace.Transition) bool { return tr.ID == trace.UnitSubject("dedupe")+"-"+UnitReady })
	if merged < 0 || ready < merged {
		t.Fatalf("merged at %d, dependent ready at %d", merged, ready)
	}
	m, r := transitions[merged], transitions[ready]
	wantReason := fmt.Sprintf("unit resume landed as %s on %s: %s approved candidate %s from %s, spec %s, plan %s, seal 1; criteria spec#1", commit, featureBranch(stream), approval, candidate, base, want.Spec, want.Plan)
	if m.From != UnitApproved || m.To != UnitMerged || m.Actor != foremanActor || m.Cause != op || m.Reason != wantReason {
		t.Fatalf("merged transition %+v", m)
	}
	if r.From != UnitPlanned || r.To != UnitReady || r.Cause != op || r.Reason != "unit dedupe is ready: every unit it depends on has merged: resume" {
		t.Fatalf("dependent ready transition %+v", r)
	}
	if got := strings.TrimSpace(demoGit(t, filepath.Dir(f.clone), "-C", f.clone, "merge-base", featureBranch(stream), unitBranch(stream, "dedupe"))); got != commit {
		t.Fatalf("the dependent unit started from %s, not the landed %s", got, commit)
	}
	f.awaitEventTurns(t, stream, fmt.Sprintf("Unit resume merged: it landed as %s on %s.", commit, featureBranch(stream)))
}

// A landing interrupted before its commit, after its commit, after the
// feature branch moved or after its record is reconciled on retry: the
// retry's inspection observes what the branch holds, the feature branch
// holds exactly one landing commit, and the unit merged once.
func TestInterruptedLandingIsReconciled(t *testing.T) {
	t.Parallel()
	observed := map[string]string{
		"land-committing": "holds no commit of this landing",
		"land-committed":  "holds no commit of this landing",
		"land-advanced":   "this operation's landing commit on",
		"land-recorded":   "landing succeeded",
	}
	for _, step := range []string{"land-committing", "land-committed", "land-advanced", "land-recorded"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			f, masons := newLandingFixture(t, independentPlan)
			defer f.stop(t)
			crashed := make(chan struct{}, 1)
			f.s.boundary = func(name string) error {
				if name == step && len(crashed) == 0 {
					crashed <- struct{}{}
					return errors.New("crash")
				}
				return nil
			}
			stream, _ := f.builtAs(t, "interrupted")
			f.awaitMerged(t, stream, "resume")
			masons.check(t)
			if len(crashed) != 1 {
				t.Fatal("the landing never reached " + step)
			}
			deadline := time.Now().Add(demoTimeout)
			var ops []trace.OperationRecord
			for {
				ops = landOperations(t, f.repository(), stream)
				if len(ops) == 1 && ops[0].Result != nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("landing operations %+v", ops)
				}
				time.Sleep(50 * time.Millisecond)
			}
			retries := 0
			var observations []coreadapter.Observation
			for _, a := range ops[0].History {
				if a.Kind == "observe" && a.Observation != nil {
					observations = append(observations, *a.Observation)
				}
				if a.Kind == "retry" {
					retries++
					if !strings.Contains(a.Failure, "crash") {
						t.Fatalf("retry %+v", a)
					}
				}
			}
			if retries != 1 || ops[0].Result.Outcome != "succeeded" {
				t.Fatalf("%d retries, result %+v", retries, ops[0].Result)
			}
			if len(observations) != 2 || (observations[1].State == coreadapter.EffectCompleted) != (step == "land-recorded") || !strings.Contains(observations[1].Evidence, observed[step]) {
				t.Fatalf("the retry observed %+v", observations)
			}
			_, result := approvedReview(t, f.repository(), stream, "resume")
			commits := f.landedCommits(t, stream, result.Identity.Candidate.BaseRevision)
			if len(commits) != 1 {
				t.Fatalf("the feature branch gained %d commits: %v", len(commits), commits)
			}
			var merged, landed []string
			for _, tr := range allTransitions(t, f.trace, stream) {
				if tr.Subject == trace.UnitSubject("resume") && tr.To == UnitMerged {
					merged = append(merged, tr.ID)
				}
				if tr.Subject == landingSubject("resume") && strings.HasPrefix(tr.To, "landed-") {
					landed = append(landed, tr.ID)
				}
			}
			if len(merged) != 1 || len(landed) != 1 {
				t.Fatalf("merged %v, landed %v", merged, landed)
			}
			docs, err := trace.Read[trace.Document](f.repository(), stream)
			must(t, err)
			var landing UnitLanding
			for _, d := range docs {
				if d.ID == landingDocument("resume") {
					must(t, json.Unmarshal([]byte(d.Content), &landing))
				}
			}
			if landing.Commit != commits[0] {
				t.Fatalf("landing.json records %s, the branch holds %s", landing.Commit, commits[0])
			}
			if status := strings.TrimSpace(demoGit(t, filepath.Dir(f.clone), "-C", filepath.Join(f.opts.Config.Root, branchesDirectory, string(f.project), string(stream)), "status", "--porcelain")); status != "" {
				t.Fatalf("the feature workspace after the landing:\n%s", status)
			}
		})
	}
}

// approveDirectly records a satisfactory review of the unit's current
// candidate and applies it, as a reviewer turn and the reviewer controller do.
func approveDirectly(t *testing.T, s *Service, repository *trace.Repository, stream config.WorkstreamID, unit, criterion string) {
	t.Helper()
	ctx := context.Background()
	r := &reviewers{masons: newMasonController(s, repository)}
	state, err := repository.Workflow(stream, trace.UnitSubject(unit))
	must(t, err)
	_, identity, err := r.prepareUnitReview(ctx, stream, unit)
	must(t, err)
	turn := reviewTurnID(unit, state.Version)
	data, err := json.Marshal(UnitReviewResult{Identity: identity, Turn: turn, Verdict: UnitVerdict{Decision: "satisfactory", Evidence: []ReviewEvidence{{Criterion: criterion, Evidence: "The planned proof holds"}}}})
	must(t, err)
	docs, err := trace.Read[trace.Document](repository, stream)
	must(t, err)
	var latest trace.Document
	for _, d := range docs {
		if d.ID == reviewDocument(unit) {
			latest = d
		}
	}
	latest.Revision++
	latest.Content = string(data) + "\n"
	latest.At = time.Now()
	latest.Cause = turn
	must(t, repository.RecordDocuments(ctx, []trace.Document{latest}))
	must(t, r.one(ctx, stream, unit, state, false))
	if approved, err := repository.Workflow(stream, trace.UnitSubject(unit)); err != nil || approved.Value != UnitApproved {
		t.Fatalf("unit %s is %+v: %v", unit, approved, err)
	}
}

// newApprovedFixture builds independentPlan with both units' masons
// reporting done, stops the service once both are reviewing and approves
// both with the service stopped. It returns the workstream and the trace,
// open.
func newApprovedFixture(t *testing.T, key string) (*shedFixture, config.WorkstreamID, *trace.Repository) {
	t.Helper()
	f, masons := newMasonFixture(t, 1, independentPlan)
	masons.play[masonTurnID("resume")] = reportDone("Built")
	masons.play[masonTurnID("dedupe")] = func(ctx context.Context, _ agent.Request, tools *mcp.ClientSession) error {
		recorded, reason, err := done(ctx, tools, map[string]any{"outcome": "Built", "criteria": []any{criterionArgs(dedupeReport)}})
		if err != nil || !recorded {
			return fmt.Errorf("done refused: %q %v", reason, err)
		}
		return nil
	}
	stream, _ := f.builtAs(t, key)
	f.awaitUnit(t, stream, "resume", UnitReviewing)
	f.awaitUnit(t, stream, "dedupe", UnitReviewing)
	masons.check(t)
	f.stop(t)
	repository, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	approveDirectly(t, f.s, repository, stream, "resume", "spec#1")
	approveDirectly(t, f.s, repository, stream, "dedupe", "spec#2")
	return f, stream, repository
}

// Landing is serial per project: while one landing has no result no other is
// asked for. Two units approved on the same base both wait to land; once the
// first lands, the second's workspace is rebased onto the landed commit, its
// report names the rebased candidate on the new base, and its approval no
// longer holds: it returns to review, with a notice, and is not asked to land.
func TestLandingIsSerialAndARebasedApprovalReturnsToReview(t *testing.T) {
	t.Parallel()
	f, stream, repository := newApprovedFixture(t, "serial")
	lands := &foreman{masons: newMasonController(f.s, repository)}
	ctx := context.Background()
	for range 2 {
		must(t, lands.Pass(ctx))
	}
	ops := landOperations(t, repository, stream)
	if len(ops) != 1 {
		t.Fatalf("%d landings asked for while one has no result: %+v", len(ops), ops)
	}
	first, err := decodeLand(ops[0].Operation)
	must(t, err)
	if first.Unit != "resume" {
		t.Fatalf("the first landing is of %s, not the first unit of the plan", first.Unit)
	}
	_, approved := approvedReview(t, repository, stream, "dedupe")
	must(t, repository.Close())
	f.start(t)
	defer f.stop(t)

	f.awaitMerged(t, stream, "resume")
	f.awaitUnit(t, stream, "dedupe", UnitReviewing)
	deadline := time.Now().Add(demoTimeout)
	var rebases []trace.OperationRecord
	for {
		rebases = rebaseOperations(t, f.repository(), stream, "dedupe")
		if len(rebases) == 1 && rebases[0].Result != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("dedupe's rebases %+v", rebases)
		}
		time.Sleep(50 * time.Millisecond)
	}
	settle()
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
	if rebase.State != UnitApproved || rebase.Onto != landed || rebase.Snapshot != approved.Identity.Candidate.Revision || rebase.Base != approved.Identity.Candidate.BaseRevision || len(rebase.Conflicts) != 0 {
		t.Fatalf("rebase.json %+v", rebase)
	}
	if parent := strings.TrimSpace(demoGit(t, filepath.Dir(f.clone), "-C", f.clone, "log", "-1", "--format=%P", unitBranch(stream, "dedupe"))); parent != landed || rebase.Commit != strings.TrimSpace(demoGit(t, filepath.Dir(f.clone), "-C", f.clone, "rev-parse", unitBranch(stream, "dedupe"))) {
		t.Fatalf("the unit branch is on %s, rebase.json names %s", parent, rebase.Commit)
	}
	doc, report := latestReport(t, f.repository(), stream, "dedupe")
	if report.Base != landed || report.Candidate != rebase.Commit || doc.Revision != rebase.Report || doc.Cause != rebases[0].Operation.ID || doc.Actor != foremanActor {
		t.Fatalf("the rebased report %+v %+v", doc.Header, report)
	}
	id := trace.UnitSubject("dedupe") + "-reviewing-rebase-1"
	i := slices.IndexFunc(allTransitions(t, f.trace, stream), func(tr trace.Transition) bool { return tr.ID == id })
	if i < 0 {
		t.Fatal("the return to review is not recorded")
	}
	back := allTransitions(t, f.trace, stream)[i]
	if back.From != UnitApproved || back.To != UnitReviewing || back.Cause != rebases[0].Operation.ID || !strings.HasPrefix(back.Reason, "the approval no longer holds: ") {
		t.Fatalf("the return to review %+v", back)
	}
	if body := f.notice(t, stream, id); body != fmt.Sprintf("Unit dedupe returns to review: its approved candidate was rebased onto %s.", landed) {
		t.Fatalf("notice %q", body)
	}
}

// An approval lands only while it names the unit's current candidate, base,
// spec and plan: a moved unit branch, a moved feature branch or an owner edit
// to the sealed spec each refuse it, and nothing is committed.
func TestStaleApprovalCannotLand(t *testing.T) {
	t.Parallel()
	f, stream, repository := newApprovedFixture(t, "stale")
	defer repository.Close()
	lands := &foreman{masons: newMasonController(f.s, repository)}
	ctx := context.Background()
	review, result := approvedReview(t, repository, stream, "resume")
	in := landInput{Unit: "resume", Review: review.Revision, Candidate: result.Identity.Candidate.Revision, Base: result.Identity.Candidate.BaseRevision}
	check := func(want string) {
		t.Helper()
		got, err := lands.current(ctx, stream, in, result)
		must(t, err)
		if got != want {
			t.Fatalf("reason %q, want %q", got, want)
		}
	}
	check("")
	home := filepath.Dir(f.clone)
	move := func(branch, to, back string, want string) {
		t.Helper()
		demoGit(t, home, "-C", f.clone, "update-ref", "refs/heads/"+branch, to)
		check(want)
		demoGit(t, home, "-C", f.clone, "update-ref", "refs/heads/"+branch, back)
		check("")
	}
	move(unitBranch(stream, "resume"), in.Base, in.Candidate, "stale approval: stale candidate revision; review the current candidate again")
	move(featureBranch(stream), in.Candidate, in.Base, "stale approval: stale base revision; review the current candidate again")
	stale := landInput{Unit: "resume", Review: review.Revision - 1, Candidate: in.Candidate, Base: in.Base}
	if got, err := lands.current(ctx, stream, stale, result); err != nil || got != fmt.Sprintf("units/resume/review.json revision %d replaced the approval", review.Revision) {
		t.Fatalf("an earlier review revision: %q %v", got, err)
	}
	f.editSpec(t, stream, staleSpec)
	got, err := lands.current(ctx, stream, in, result)
	must(t, err)
	if !strings.HasPrefix(got, "stale approval: stale review inputs: ") || !strings.Contains(got, "spec does not match its seal") {
		t.Fatalf("an edited spec: %q", got)
	}

	must(t, lands.Pass(ctx))
	ops := landOperations(t, repository, stream)
	if len(ops) != 1 {
		t.Fatalf("landing operations %+v", ops)
	}
	observed, err := lands.Inspect(ctx, ops[0].Operation)
	must(t, err)
	if observed.State != coreadapter.EffectAbsent {
		t.Fatalf("inspection %+v", observed)
	}
	outcome, err := lands.Apply(ctx, ops[0].Operation)
	must(t, err)
	if outcome.Outcome != "failed" || !strings.Contains(outcome.Evidence, "unit resume did not land: stale approval: stale review inputs: ") {
		t.Fatalf("the stale landing %+v", outcome)
	}
	if commits := f.landedCommits(t, stream, in.Base); len(commits) != 0 {
		t.Fatalf("a stale approval landed %v", commits)
	}
	if state, err := repository.Workflow(stream, trace.UnitSubject("resume")); err != nil || state.Value != UnitApproved {
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

// A planned unit becomes ready when the unit that merges is the last of its
// dependencies to merge, and not before.
func TestNewlyReadyWaitsForEveryDependency(t *testing.T) {
	t.Parallel()
	p := plan.Plan{Version: plan.Version, Units: []plan.Unit{
		{ID: "a"}, {ID: "b"},
		{ID: "both", DependsOn: []string{"a", "b"}},
		{ID: "only-a", DependsOn: []string{"a"}},
		{ID: "only-b", DependsOn: []string{"b"}},
	}}
	states := map[string]trace.WorkflowState{}
	for _, u := range p.Units {
		states[trace.UnitSubject(u.ID)] = trace.WorkflowState{Version: 1, Value: UnitPlanned}
	}
	states[trace.UnitSubject("a")] = trace.WorkflowState{Value: UnitApproved}
	states[trace.UnitSubject("b")] = trace.WorkflowState{Value: UnitApproved}
	ids := func(units []plan.Unit) []string {
		var out []string
		for _, u := range units {
			out = append(out, u.ID)
		}
		return out
	}
	if got := ids(newlyReady(p, states, "a")); !slices.Equal(got, []string{"only-a"}) {
		t.Fatalf("ready when a merges first: %v", got)
	}
	states[trace.UnitSubject("a")] = trace.WorkflowState{Value: UnitMerged}
	states[trace.UnitSubject("only-a")] = trace.WorkflowState{Value: UnitReady}
	if got := ids(newlyReady(p, states, "b")); !slices.Equal(got, []string{"both", "only-b"}) {
		t.Fatalf("ready when b merges after a: %v", got)
	}
}
