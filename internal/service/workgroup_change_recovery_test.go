package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/amendment"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestCancelHoldsQueuedLandingAndPreservesApprovedCandidate(t *testing.T) {
	t.Parallel()
	f, stream, repo := newApprovedFixture(t, "cancel-queued-landing")
	defer repo.Close()
	ctx := context.Background()
	f.s.setSole(runtimeFor(repo))
	land := &foreman{masons: newMasonController(f.s, repo)}
	must(t, land.Pass(ctx))
	ops := landOperations(t, repo, stream)
	if len(ops) != 1 || ops[0].Result != nil || changeEffectStarted(ops[0]) {
		t.Fatal("fixture must have one queued, unstarted landing")
	}
	before := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", featureBranch(stream)))
	c := approvedCancellation(t, f, repo, stream, "workgroup", "draft")
	must(t, f.s.changeBoundary(ctx, repo, stream, c, false))
	if !f.s.holding(repo)(stream, ops[0].Operation) {
		t.Fatal("queued landing was admitted across the cancel boundary")
	}
	if _, err := land.Apply(ctx, ops[0].Operation); !errors.Is(err, errChangeBoundary) {
		t.Fatalf("direct landing crossed the cancel boundary: %v", err)
	}
	idle, err := changeIdle(repo, stream)
	must(t, err)
	if !idle {
		t.Fatal("an unstarted landing prevents cancellation from reaching its boundary")
	}
	wt, g, _, close, err := f.s.changeContext(ctx, string(stream))
	must(t, err)
	defer close()
	must(t, f.s.applyChange(ctx, wt, g, &c))
	if c.State != "applied" {
		t.Fatal("queued landing stranded cancellation")
	}
	// After release, the old operation can reconcile only a refusal.
	result, err := land.Apply(ctx, ops[0].Operation)
	must(t, err)
	if result.Outcome != "failed" {
		t.Fatal("cancelled candidate landed")
	}
	if got := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", featureBranch(stream))); got != before {
		t.Fatal("cancellation advanced the feature branch")
	}
	_, review := approvedReview(t, repo, stream, "resume")
	if got := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", unitBranch(stream, "resume"))); got != review.Identity.Candidate.Revision {
		t.Fatal("approved but unlanded candidate lost its unit branch")
	}
}

func TestAmendedWorkerWaitsForRevisedDependency(t *testing.T) {
	t.Parallel()
	f, stream, repo := newApprovedFixture(t, "amended-dependencies")
	defer repo.Close()
	ctx := context.Background()
	f.s.setSole(runtimeFor(repo))
	recordUnitMerged(t, repo, stream, "resume")
	spec, graph, err := memberSpecAndPlan(repo, stream)
	must(t, err)
	before := trace.ProjectSpec{Behavior: "Uploads", Slices: []trace.Slice{{Workstream: stream, Spec: spec, Plan: graph}}}
	after := before
	after.Slices = slices.Clone(before.Slices)
	after.Slices[0].Spec = amendedSpec
	p, err := plan.Parse([]byte(graph))
	must(t, err)
	for n := range p.Units {
		if p.Units[n].ID == "dedupe" {
			p.Units[n].DependsOn = []string{"resume"}
		}
	}
	data, err := plan.Encode(p)
	must(t, err)
	after.Slices[0].Plan = string(data)
	impact, err := amendment.ClassifyImpact("dependencies", before, after, []amendment.Member{{Workstream: stream, State: BuildingState, Units: map[string]string{"resume": UnitMerged, "dedupe": UnitApproved}}})
	must(t, err)
	c := WorkgroupChange{ID: "dependencies", Before: before, After: after, Result: &after, Impact: impact, Request: ChangeRequest{Change: "Dedupe follows revised checkpoints"}}
	must(t, f.s.reworkChangeMember(ctx, repo, impact.Streams[0], &c))
	th, err := repo.Thread(stream, masonAgent("dedupe"))
	must(t, err)
	var amended trace.QueuedTurn
	for _, q := range th.Turns {
		if q.Request.TurnID == amendmentMasonTurn("dedupe", "group-"+c.ID) {
			amended = q
		}
	}
	if amended.Request.TurnID == "" {
		t.Fatal("no durable amended worker turn")
	}
	candidate := scheduler.Candidate{Workstream: stream, Thread: th, Turn: amended}
	if held, err := f.s.holds(repo.Project(), librarianWorkstream(repo.Project()), repo, candidate); err != nil || !held {
		t.Fatalf("worker bypassed the revision dependency: %t %v", held, err)
	}
	recordUnitMerged(t, repo, stream, amendment.RevisionID(c.ID, "resume"))
	if held, err := f.s.holds(repo.Project(), librarianWorkstream(repo.Project()), repo, candidate); err != nil || held {
		t.Fatalf("worker did not resume after its dependency landed: %t %v", held, err)
	}
}

func TestChangeBoundaryReconcilesStartedRepositoryEffects(t *testing.T) {
	t.Parallel()
	f, stream, repo := newApprovedFixture(t, "boundary-effect")
	defer repo.Close()
	ctx := context.Background()
	f.s.setSole(runtimeFor(repo))
	land := &foreman{masons: newMasonController(f.s, repo)}
	must(t, land.Pass(ctx))
	op := landOperations(t, repo, stream)[0]
	// A crash can occur after the external effect but before its result. The
	// durable effect record must permit reconciliation of the whole unit.
	must(t, repo.WithOperation(ctx, stream, op.EventID, foremanActor, f.s.now, func(attempt *trace.OperationAttempt, _ trace.OperationRecord) error {
		observe := attempt.Action("observe", f.s.now())
		observe.Observation = &coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "unit is approved and not landed"}
		if err := attempt.Record(ctx, observe); err != nil {
			return err
		}
		return attempt.Record(ctx, attempt.Action("effect", f.s.now()))
	}))
	c := approvedCancellation(t, f, repo, stream, "workstream", "draft")
	must(t, f.s.changeBoundary(ctx, repo, stream, c, false))
	if f.s.holding(repo)(stream, op.Operation) {
		t.Fatal("started repository effect was stranded")
	}
	idle, err := changeIdle(repo, stream)
	must(t, err)
	if idle {
		t.Fatal("cancellation skipped unresolved repository evidence")
	}
	result, err := land.Apply(ctx, op.Operation)
	must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("whole-unit reconciliation: %+v", result)
	}
	must(t, repo.WithOperation(ctx, stream, op.EventID, foremanActor, f.s.now, func(attempt *trace.OperationAttempt, _ trace.OperationRecord) error {
		observe := attempt.Action("observe", f.s.now())
		observe.Observation = &coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: result.Evidence, Result: &result}
		if err := attempt.Record(ctx, observe); err != nil {
			return err
		}
		completed := attempt.Action("result", f.s.now())
		completed.Result = &result
		return attempt.Record(ctx, completed)
	}))
	_, review := approvedReview(t, repo, stream, "resume")
	tree := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", featureBranch(stream)+"^{tree}"))
	if tree != strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", review.Identity.Candidate.Revision+"^{tree}")) {
		t.Fatal("feature branch contains only part of the landed candidate")
	}
	tip := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", featureBranch(stream)))
	wt, g, _, close, err := f.s.changeContext(ctx, string(stream))
	must(t, err)
	defer close()
	must(t, f.s.applyChange(ctx, wt, g, &c))
	if c.State != "applied" || strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", featureBranch(stream))) != tip {
		t.Fatal("cancellation lost the reconciled whole-unit boundary")
	}
}

func TestCancelDuringPlanningCreatesNoFeatureBranch(t *testing.T) {
	t.Parallel()
	for _, scope := range []string{"workstream", "workgroup"} {
		t.Run(scope, func(t *testing.T) {
			f := newShedFixture(t, 1)
			f.stop(t)
			repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
			must(t, err)
			defer repo.Close()
			id := HandInWorkstream(f.project, "planning-cancel")
			must(t, repo.CreateWorkstreamOn(context.Background(), id, config.WorkspacesGit, f.s.now(), ownerActor))
			before := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
			c := approvedCancellation(t, f, repo, id, scope, "close")
			wt, g, _, close, err := f.s.changeContext(context.Background(), string(id))
			must(t, err)
			defer close()
			must(t, f.s.applyChange(context.Background(), wt, g, &c))
			st, err := repo.Workflow(id, trace.FeatureSubject)
			must(t, err)
			if st.Value != AbandonedState {
				t.Fatal("planning cancellation did not stop work")
			}
			if branch := demoGit(t, f.clone, "-C", f.clone, "for-each-ref", "--format=%(refname)", "refs/heads/"+featureBranch(id)); strings.TrimSpace(branch) != "" {
				t.Fatal("cancel created a feature branch")
			}
			if after := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD")); after != before {
				t.Fatal("cancel changed target repository")
			}
		})
	}
}

func TestRejectedContractDeltaResumesOnlyItsParkedRequester(t *testing.T) {
	t.Parallel()
	f, stream, repo := newApprovedFixture(t, "reject-contract")
	defer repo.Close()
	f.s.setSole(runtimeFor(repo))
	ctx := context.Background()
	groups, err := trace.EnsureWorkgroups(ctx, repo, []config.WorkstreamID{stream}, f.s.now(), serviceActor, "fixture")
	must(t, err)
	g, _ := trace.WorkgroupOf(groups, stream)
	before, err := repo.Workflow(stream, trace.UnitSubject("dedupe"))
	must(t, err)
	original, _, _, err := seal.Latest(repo, stream)
	must(t, err)
	st, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	must(t, err)
	paused := trace.UnitSubject("resume") + "_waiting_project_amendment_" + string(g.ID) + "_1"
	_, err = repo.Transact(ctx, trace.Transaction{ExpectedVersion: st.Version, Transition: trace.Transition{
		Header:  trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: paused, Revision: 1, Project: repo.Project(), Workstream: stream, Unit: "resume", At: f.s.now(), Actor: trace.Actor{Kind: "service", ID: masonRole}, Cause: "project-amendment-1"},
		Subject: trace.UnitSubject("resume"), From: st.Value, To: UnitWaiting, Reason: "the protected contract request parks only this origin",
	}})
	must(t, err)
	th, err := repo.Thread(stream, masonAgent("resume"))
	must(t, err)
	c := WorkgroupChange{ID: "contract-1", State: "rejected", Decision: &ChangeDecision{Decision: "reject"}, Requester: &trace.ProjectAmendment{ID: "1", Workgroup: g.ID, Origin: stream, Requester: trace.Actor{Kind: "agent", ID: masonAgent("resume")}, Citations: []string{"spec#1"}, Change: "Change the offset type", Reason: "The consumer requested another type"}}
	must(t, f.s.settleChangeRequester(ctx, c))
	must(t, f.s.settleChangeRequester(ctx, c))
	resumed, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	must(t, err)
	kept, err := repo.Workflow(stream, trace.UnitSubject("dedupe"))
	must(t, err)
	if resumed.Value != UnitImplementing || kept != before {
		t.Fatal("the decision did not resume only its parked origin")
	}
	now, err := repo.Thread(stream, masonAgent("resume"))
	must(t, err)
	if len(now.Turns) != len(th.Turns)+1 || !strings.Contains(now.Turns[len(now.Turns)-1].Request.Prompt, "rejected") {
		t.Fatal("the requester did not receive exactly one attributed decision")
	}
	latest, _, _, err := seal.Latest(repo, stream)
	must(t, err)
	if latest.Seal != original.Seal || latest.SpecHash != original.SpecHash {
		t.Fatal("rejecting the impact changed approved intent")
	}
}

func TestCancelUpstreamIncludesDependentsInSharedImpact(t *testing.T) {
	t.Parallel()
	f := newProductionDesignFixture(t)
	defer f.stop(t)
	f.startOwner(t)
	packet := f.awaitPacket(t)
	_, err := f.c.Ratify(context.Background(), f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan)
	must(t, err)
	f.awaitApproved(t)
	c, api := f.s.requestChange(context.Background(), string(f.origin), ChangeRequest{Key: "cancel-server", Kind: "cancel", Scope: "workstream", Workstream: f.stream1, Change: "Cancel the server and its dependent client"})
	if api != nil {
		t.Fatal(api)
	}
	f.wait(t, "cancel impact", func() bool { return currentChange(t, f.s, f.origin, c.ID).State == "presented" })
	c = currentChange(t, f.s, f.origin, c.ID)
	if len(c.Impact.Streams) != 2 || c.Impact.Streams[0].Action != amendment.Cancel || c.Impact.Streams[1].Action != amendment.Cancel || !c.Impact.Streams[1].Cascade {
		t.Fatalf("dependent missing: %+v", c.Impact)
	}
	conversation, err := f.c.Conversation(context.Background(), f.origin)
	must(t, err)
	count := 0
	for _, entry := range conversation.Entries {
		if entry.Kind == "action" && entry.Turn == "change-"+c.ID+"-impact" {
			count++
			if !strings.Contains(entry.Text, string(f.stream2)) || !strings.Contains(entry.Text, "dependent") {
				t.Fatal("same impact did not show dependent")
			}
		}
	}
	if count != 1 {
		t.Fatalf("cancel prompts=%d", count)
	}
	_, api = f.s.decideChange(context.Background(), string(f.origin), c.ID, ChangeDecision{Revision: c.Revision, Decision: "approve", PullRequests: "draft"})
	if api != nil {
		t.Fatal(api)
	}
	f.wait(t, "dependent cancellation", func() bool { return currentChange(t, f.s, f.origin, c.ID).State == "applied" })
	for i, id := range []config.WorkstreamID{f.stream1, f.stream2} {
		repo, err := f.s.repository(f.repos[i].id)
		must(t, err)
		st, err := repo.Workflow(id, trace.FeatureSubject)
		must(t, err)
		if st.Value != AbandonedState {
			t.Fatal("dependent proceeded after cancel")
		}
	}
	f.checkContinuation(t)
}

func TestCancelInterruptedClaimAndMemberApplicationRecovers(t *testing.T) {
	t.Parallel()
	f, stream, repo := newCleanupFixture(t, config.WorkspacesGit)
	ctx := context.Background()
	c := approvedCancellation(t, f, repo, stream, "workgroup", "draft")
	th, err := repo.Thread(stream, masonAgent("upload"))
	must(t, err)
	_, err = repo.ClaimTurn(ctx, stream, masonAgent("upload"), "stopped", filepath.Join(f.opts.Config.Root, "stopped"), f.s.now())
	must(t, err)
	wt, g, _, close, err := f.s.changeContext(ctx, string(stream))
	must(t, err)
	must(t, f.s.applyChange(ctx, wt, g, &c))
	if c.State == "applied" {
		t.Fatal("crossed running claim")
	}
	close()
	repo.Close()
	repo, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	defer repo.Close()
	f.s.setSole(runtimeFor(repo))
	wt, g, _, close, err = f.s.changeContext(ctx, string(stream))
	must(t, err)
	defer close()
	c = currentChange(t, f.s, stream, c.ID)
	crash := errors.New("interrupted after member application")
	f.s.boundary = func(step string) error {
		if step == "change-member-applied" {
			return crash
		}
		return nil
	}
	if err := f.s.applyChange(ctx, wt, g, &c); !errors.Is(err, crash) {
		t.Fatalf("interruption: %v", err)
	}
	f.s.boundary = nil
	c = currentChange(t, f.s, stream, c.ID)
	must(t, f.s.applyChange(ctx, wt, g, &c))
	if c.State != "applied" {
		t.Fatal("restart could not settle stopped claim")
	}
	thread, err := repo.Thread(stream, masonAgent("upload"))
	must(t, err)
	if len(thread.Turns) != len(th.Turns) || thread.Active != "" || thread.Turns[0].Response == nil || !thread.Turns[0].Response.Result.Cancelled {
		t.Fatal("interrupted turn not retained as cancelled")
	}
	cleanup := newCleanup(f, repo)
	must(t, cleanup.Pass(ctx))
	if len(cleanup.failures) > 0 {
		t.Fatal(cleanup.failures)
	}
	if got := branchFile(t, f, repo, stream, unitBranch(stream, "upload"), "upload.txt"); got != "upload\n" {
		t.Fatal("unlanded work lost on recovery")
	}
}

func TestRemovedSectionCreatesObsoleteUnitAndRemovalRevision(t *testing.T) {
	t.Parallel()
	f, stream, repo := newApprovedFixture(t, "remove-section")
	defer repo.Close()
	ctx := context.Background()
	recordUnitMerged(t, repo, stream, "resume")
	spec, graph, err := memberSpecAndPlan(repo, stream)
	must(t, err)
	p, err := plan.Parse([]byte(graph))
	must(t, err)
	p.Units = slices.DeleteFunc(p.Units, func(u plan.Unit) bool { return u.ID == "resume" })
	p.Units[0].DependsOn = nil
	data, err := plan.Encode(p)
	must(t, err)
	before := trace.ProjectSpec{Behavior: "Uploads", Slices: []trace.Slice{{Workstream: stream, Spec: spec, Plan: graph}}}
	after := trace.ProjectSpec{Behavior: "Uploads", Slices: []trace.Slice{{Workstream: stream, Spec: strings.Replace(spec, "1. An interrupted upload resumes from the last acknowledged chunk.\n", "", 1), Plan: string(data)}}}
	impact, err := amendment.ClassifyImpact("remove", before, after, []amendment.Member{{Workstream: stream, State: BuildingState, Units: map[string]string{"resume": UnitMerged, "dedupe": UnitApproved}}})
	must(t, err)
	c := WorkgroupChange{ID: "remove", Before: before, After: after, Result: &after, Impact: impact, Request: ChangeRequest{Change: "Remove resumption"}}
	must(t, f.s.reworkChangeMember(ctx, repo, impact.Streams[0], &c))
	st, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	must(t, err)
	if st.Value != "obsolete" {
		t.Fatal("removed section's unit remained active")
	}
	sealed, _, _, err := seal.Latest(repo, stream)
	must(t, err)
	doc, err := sealedPlan(repo, stream, sealed.Revision.Plan)
	must(t, err)
	p, err = plan.Parse([]byte(doc.Content))
	must(t, err)
	removal, ok := p.Unit(amendment.RevisionID("remove", "resume"))
	if !ok || !strings.Contains(removal.Task, "Remove the code") || len(removal.Footprint) == 0 {
		t.Fatal("removal work missing its code footprint")
	}
	if _, ok := p.Unit("resume"); ok {
		t.Fatal("obsolete unit remained in active plan")
	}
}

func TestAmendNotStartedReplansBeforeBuilderInitializesUnits(t *testing.T) {
	t.Parallel()
	f := newShedFixture(t, 1)
	base := f.upstream(t)
	stream, repo := seedBuild(t, f, "not-started", validPlan, config.WorkspacesGit, base)
	defer repo.Close()
	// A seal exists but no build has begun: retain its documents and branch,
	// and use a second fresh member to exercise ordinary builder admission.
	id := HandInWorkstream(f.project, "not-started-new")
	ctx := context.Background()
	must(t, repo.CreateWorkstreamOn(ctx, id, config.WorkspacesGit, f.s.now(), ownerActor))
	f.s.setSole(runtimeFor(repo))
	spec, graph, err := memberSpecAndPlan(repo, stream)
	must(t, err)
	c := WorkgroupChange{ID: "replan", Request: ChangeRequest{Change: "Use checkpoints"}}
	sl := trace.Slice{Workstream: id, Spec: amendedSpec, Plan: graph}
	must(t, f.s.planChangeMember(ctx, repo, id, sl, c.ID))
	ready, err := f.s.changeSealReady(ctx, repo, id, c.ID)
	must(t, err)
	if ready {
		t.Fatal("replan admitted before sealing")
	}
	z := &sealer{s: f.s, repository: repo}
	ops, err := repo.Operations(id)
	must(t, err)
	for _, op := range ops {
		if op.Operation.Action == SealAction {
			result, err := z.Apply(ctx, op.Operation)
			must(t, err)
			if result.Outcome != "succeeded" {
				t.Fatal(result)
			}
		}
	}
	before := trace.ProjectSpec{Behavior: "Uploads", Slices: []trace.Slice{{Workstream: id, Spec: spec, Plan: graph}}}
	after := trace.ProjectSpec{Behavior: "Uploads", Slices: []trace.Slice{sl}}
	impact, err := amendment.ClassifyImpact(c.ID, before, after, []amendment.Member{{Workstream: id, State: RatifiedState, Units: map[string]string{}}})
	must(t, err)
	if impact.Streams[0].Action != amendment.Replan {
		t.Fatal("not-started member not replanned")
	}
	c.Before, c.After, c.Result, c.Impact = before, after, &after, impact
	must(t, f.s.reworkChangeMember(ctx, repo, impact.Streams[0], &c))
	b := &builder{s: f.s, repository: repo}
	must(t, b.Pass(ctx))
	ops, err = repo.Operations(id)
	must(t, err)
	for _, op := range ops {
		if op.Operation.Action == BuildAction {
			result, err := b.Apply(ctx, op.Operation)
			must(t, err)
			if result.Outcome != "succeeded" {
				t.Fatal(result)
			}
		}
	}
	st, err := repo.Workflow(id, trace.UnitSubject("resume"))
	must(t, err)
	if st.Value != UnitReady {
		t.Fatal("replacement plan failed to initialize")
	}
	if _, err := os.Stat(filepath.Join(f.clone, "resume.go")); !os.IsNotExist(err) {
		t.Fatal("planning committed code")
	}
}
