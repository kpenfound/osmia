package lifecycle_test

import (
	"context"
	"errors"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/amendment"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestCancelHoldsQueuedLandingAndPreservesApprovedCandidate(t *testing.T) {
	t.Parallel()
	f, stream, repo := journey.NewApprovedFixture(t, "cancel-queued-landing")
	defer repo.Close()
	ctx := context.Background()
	f.S.SetSole(journey.RuntimeFor(repo))
	land := &journey.Foreman{Masons: journey.NewMasonController(f.S, repo)}
	fixturekit.Must(t, land.Pass(ctx))
	ops := journey.LandOperations(t, repo, stream)
	if len(ops) != 1 || ops[0].Result != nil || lifecycle.ChangeEffectStarted(ops[0]) {
		t.Fatal("fixture must have one queued, unstarted landing")
	}
	before := strings.TrimSpace(fixturekit.DemoGit(t, f.Clone, "-C", f.Clone, "rev-parse", intake.FeatureBranch(stream)))
	c := approvedCancellation(t, f, repo, stream, "workgroup", "draft")
	fixturekit.Must(t, f.S.Lifecycle().ChangeBoundary(ctx, repo, stream, c, false))
	if !f.S.Holding(repo)(stream, ops[0].Operation) {
		t.Fatal("queued landing was admitted across the cancel boundary")
	}
	if _, err := land.Apply(ctx, ops[0].Operation); !errors.Is(err, intake.ErrChangeBoundary) {
		t.Fatalf("direct landing crossed the cancel boundary: %v", err)
	}
	idle, err := lifecycle.ChangeIdle(repo, stream)
	fixturekit.Must(t, err)
	if !idle {
		t.Fatal("an unstarted landing prevents cancellation from reaching its boundary")
	}
	wt, g, _, close, err := f.S.Lifecycle().ChangeContext(ctx, string(stream))
	fixturekit.Must(t, err)
	defer close()
	fixturekit.Must(t, f.S.Lifecycle().ApplyChange(ctx, wt, g, &c))
	if c.State != "applied" {
		t.Fatal("queued landing stranded cancellation")
	}
	result, err := land.Apply(ctx, ops[0].Operation)
	fixturekit.Must(t, err)
	if result.Outcome != "failed" {
		t.Fatal("cancelled candidate landed")
	}
	if got := strings.TrimSpace(fixturekit.DemoGit(t, f.Clone, "-C", f.Clone, "rev-parse", intake.FeatureBranch(stream))); got != before {
		t.Fatal("cancellation advanced the feature branch")
	}
	_, review := journey.ApprovedReview(t, repo, stream, "resume")
	if got := strings.TrimSpace(fixturekit.DemoGit(t, f.Clone, "-C", f.Clone, "rev-parse", execution.UnitBranch(stream, "resume"))); got != review.Identity.Candidate.Revision {
		t.Fatal("approved but unlanded candidate lost its unit branch")
	}
}
func TestAmendedWorkerWaitsForRevisedDependency(t *testing.T) {
	t.Parallel()
	f, stream, repo := journey.NewApprovedFixture(t, "amended-dependencies")
	defer repo.Close()
	ctx := context.Background()
	f.S.SetSole(journey.RuntimeFor(repo))
	recordUnitMerged(t, repo, stream, "resume")
	spec, graph, err := lifecycle.MemberSpecAndPlan(repo, stream)
	fixturekit.Must(t, err)
	before := trace.ProjectSpec{Behavior: "Uploads", Slices: []trace.Slice{{Workstream: stream, Spec: spec, Plan: graph}}}
	after := before
	after.Slices = slices.Clone(before.Slices)
	after.Slices[0].Spec = intakefixture.AmendedSpec
	p, err := plan.Parse([]byte(graph))
	fixturekit.Must(t, err)
	for n := range p.Units {
		if p.Units[n].ID == "dedupe" {
			p.Units[n].DependsOn = []string{"resume"}
		}
	}
	data, err := plan.Encode(p)
	fixturekit.Must(t, err)
	after.Slices[0].Plan = string(data)
	impact, err := amendment.ClassifyImpact("dependencies", before, after, []amendment.Member{{Workstream: stream, State: intake.BuildingState, Units: map[string]string{"resume": intake.UnitMerged, "dedupe": intake.UnitApproved}}})
	fixturekit.Must(t, err)
	c := lifecycle.WorkgroupChange{ID: "dependencies", Before: before, After: after, Result: &after, Impact: impact, Request: lifecycle.ChangeRequest{Change: "Dedupe follows revised checkpoints"}}
	fixturekit.Must(t, f.S.Lifecycle().ReworkChangeMember(ctx, repo, impact.Streams[0], &c))
	th, err := repo.Thread(stream, intake.MasonAgent("dedupe"))
	fixturekit.Must(t, err)
	var amended trace.QueuedTurn
	for _, q := range th.Turns {
		if q.Request.TurnID == intake.AmendmentMasonTurn("dedupe", "group-"+c.ID) {
			amended = q
		}
	}
	if amended.Request.TurnID == "" {
		t.Fatal("no durable amended worker turn")
	}
	candidate := scheduler.Candidate{Workstream: stream, Thread: th, Turn: amended}
	if held, err := f.S.Holds(repo.Project(), repo, candidate); err != nil || !held {
		t.Fatalf("worker bypassed the revision dependency: %t %v", held, err)
	}
	recordUnitMerged(t, repo, stream, amendment.RevisionID(c.ID, "resume"))
	if held, err := f.S.Holds(repo.Project(), repo, candidate); err != nil || held {
		t.Fatalf("worker did not resume after its dependency landed: %t %v", held, err)
	}
}
func TestChangeBoundaryReconcilesStartedRepositoryEffects(t *testing.T) {
	t.Parallel()
	f, stream, repo := journey.NewApprovedFixture(t, "boundary-effect")
	defer repo.Close()
	ctx := context.Background()
	f.S.SetSole(journey.RuntimeFor(repo))
	land := &journey.Foreman{Masons: journey.NewMasonController(f.S, repo)}
	fixturekit.Must(t, land.Pass(ctx))
	op := journey.LandOperations(t, repo, stream)[0]
	fixturekit.Must(t, repo.WithOperation(ctx, stream, op.EventID, execution.ForemanActor, f.S.Now, func(attempt *trace.OperationAttempt, _ trace.OperationRecord) error {
		observe := attempt.Action("observe", f.S.Now())
		observe.Observation = &coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "unit is approved and not landed"}
		if err := attempt.Record(ctx, observe); err != nil {
			return err
		}
		return attempt.Record(ctx, attempt.Action("effect", f.S.Now()))
	}))
	c := approvedCancellation(t, f, repo, stream, "workstream", "draft")
	fixturekit.Must(t, f.S.Lifecycle().ChangeBoundary(ctx, repo, stream, c, false))
	if f.S.Holding(repo)(stream, op.Operation) {
		t.Fatal("started repository effect was stranded")
	}
	idle, err := lifecycle.ChangeIdle(repo, stream)
	fixturekit.Must(t, err)
	if idle {
		t.Fatal("cancellation skipped unresolved repository evidence")
	}
	result, err := land.Apply(ctx, op.Operation)
	fixturekit.Must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("whole-unit reconciliation: %+v", result)
	}
	fixturekit.Must(t, repo.WithOperation(ctx, stream, op.EventID, execution.ForemanActor, f.S.Now, func(attempt *trace.OperationAttempt, _ trace.OperationRecord) error {
		observe := attempt.Action("observe", f.S.Now())
		observe.Observation = &coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: result.Evidence, Result: &result}
		if err := attempt.Record(ctx, observe); err != nil {
			return err
		}
		completed := attempt.Action("result", f.S.Now())
		completed.Result = &result
		return attempt.Record(ctx, completed)
	}))
	_, review := journey.ApprovedReview(t, repo, stream, "resume")
	tree := strings.TrimSpace(fixturekit.DemoGit(t, f.Clone, "-C", f.Clone, "rev-parse", intake.FeatureBranch(stream)+"^{tree}"))
	if tree != strings.TrimSpace(fixturekit.DemoGit(t, f.Clone, "-C", f.Clone, "rev-parse", review.Identity.Candidate.Revision+"^{tree}")) {
		t.Fatal("feature branch contains only part of the landed candidate")
	}
	tip := strings.TrimSpace(fixturekit.DemoGit(t, f.Clone, "-C", f.Clone, "rev-parse", intake.FeatureBranch(stream)))
	wt, g, _, close, err := f.S.Lifecycle().ChangeContext(ctx, string(stream))
	fixturekit.Must(t, err)
	defer close()
	fixturekit.Must(t, f.S.Lifecycle().ApplyChange(ctx, wt, g, &c))
	if c.State != "applied" || strings.TrimSpace(fixturekit.DemoGit(t, f.Clone, "-C", f.Clone, "rev-parse", intake.FeatureBranch(stream))) != tip {
		t.Fatal("cancellation lost the reconciled whole-unit boundary")
	}
}
func TestCancelDuringPlanningCreatesNoFeatureBranch(t *testing.T) {
	t.Parallel()
	for _, scope := range []string{"workstream", "workgroup"} {
		t.Run(scope, func(t *testing.T) {
			f := journey.NewShedFixture(t, 1)
			f.Stop(t)
			repo, err := trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
			fixturekit.Must(t, err)
			defer repo.Close()
			id := intake.HandInWorkstream(f.Project, "planning-cancel")
			fixturekit.Must(t, repo.CreateWorkstreamOn(context.Background(), id, config.WorkspacesGit, f.S.Now(), intake.OwnerActor))
			before := strings.TrimSpace(fixturekit.DemoGit(t, f.Clone, "-C", f.Clone, "rev-parse", "HEAD"))
			c := approvedCancellation(t, f, repo, id, scope, "close")
			wt, g, _, close, err := f.S.Lifecycle().ChangeContext(context.Background(), string(id))
			fixturekit.Must(t, err)
			defer close()
			fixturekit.Must(t, f.S.Lifecycle().ApplyChange(context.Background(), wt, g, &c))
			st, err := repo.Workflow(id, trace.FeatureSubject)
			fixturekit.Must(t, err)
			if st.Value != intake.AbandonedState {
				t.Fatal("planning cancellation did not stop work")
			}
			if branch := fixturekit.DemoGit(t, f.Clone, "-C", f.Clone, "for-each-ref", "--format=%(refname)", "refs/heads/"+intake.FeatureBranch(id)); strings.TrimSpace(branch) != "" {
				t.Fatal("cancel created a feature branch")
			}
			if after := strings.TrimSpace(fixturekit.DemoGit(t, f.Clone, "-C", f.Clone, "rev-parse", "HEAD")); after != before {
				t.Fatal("cancel changed target repository")
			}
		})
	}
}
func TestRejectedContractDeltaResumesOnlyItsParkedRequester(t *testing.T) {
	t.Parallel()
	f, stream, repo := journey.NewApprovedFixture(t, "reject-contract")
	defer repo.Close()
	f.S.SetSole(journey.RuntimeFor(repo))
	ctx := context.Background()
	groups, err := trace.EnsureWorkgroups(ctx, repo, []config.WorkstreamID{stream}, f.S.Now(), intake.ServiceActor, "fixture")
	fixturekit.Must(t, err)
	g, _ := trace.WorkgroupOf(groups, stream)
	before, err := repo.Workflow(stream, trace.UnitSubject("dedupe"))
	fixturekit.Must(t, err)
	original, _, _, err := seal.Latest(repo, stream)
	fixturekit.Must(t, err)
	st, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	paused := trace.UnitSubject("resume") + "_waiting_project_amendment_" + string(g.ID) + "_1"
	_, err = repo.Transact(ctx, trace.Transaction{ExpectedVersion: st.Version, Transition: trace.Transition{
		Header:  trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: paused, Revision: 1, Project: repo.Project(), Workstream: stream, Unit: "resume", At: f.S.Now(), Actor: trace.Actor{Kind: "service", ID: intake.MasonRole}, Cause: "project-amendment-1"},
		Subject: trace.UnitSubject("resume"), From: st.Value, To: intake.UnitWaiting, Reason: "the protected contract request parks only this origin",
	}})
	fixturekit.Must(t, err)
	th, err := repo.Thread(stream, intake.MasonAgent("resume"))
	fixturekit.Must(t, err)
	c := lifecycle.WorkgroupChange{ID: "contract-1", State: "rejected", Decision: &lifecycle.ChangeDecision{Decision: "reject"}, Requester: &trace.ProjectAmendment{ID: "1", Workgroup: g.ID, Origin: stream, Requester: trace.Actor{Kind: "agent", ID: intake.MasonAgent("resume")}, Citations: []string{"spec#1"}, Change: "Change the offset type", Reason: "The consumer requested another type"}}
	fixturekit.Must(t, f.S.Lifecycle().SettleChangeRequester(ctx, c))
	fixturekit.Must(t, f.S.Lifecycle().SettleChangeRequester(ctx, c))
	resumed, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	kept, err := repo.Workflow(stream, trace.UnitSubject("dedupe"))
	fixturekit.Must(t, err)
	if resumed.Value != intake.UnitImplementing || kept != before {
		t.Fatal("the decision did not resume only its parked origin")
	}
	now, err := repo.Thread(stream, intake.MasonAgent("resume"))
	fixturekit.Must(t, err)
	if len(now.Turns) != len(th.Turns)+1 || !strings.Contains(now.Turns[len(now.Turns)-1].Request.Prompt, "rejected") {
		t.Fatal("the requester did not receive exactly one attributed decision")
	}
	latest, _, _, err := seal.Latest(repo, stream)
	fixturekit.Must(t, err)
	if latest.Seal != original.Seal || latest.SpecHash != original.SpecHash {
		t.Fatal("rejecting the impact changed approved intent")
	}
}

func changeFailureNotices(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, id string) []string {
	t.Helper()
	entries, err := repo.Outbox(stream)
	fixturekit.Must(t, err)
	var bodies []string
	for _, e := range entries {
		if strings.Contains(e.Event.Body, "Workgroup change "+id+" ") && strings.Contains(e.Event.Body, "failed before it was applied") {
			bodies = append(bodies, e.Event.Body)
		}
	}
	return bodies
}

func TestFailedChangeSettlesItsRequesterAndTellsTheChief(t *testing.T) {
	t.Parallel()
	f, stream, repo := journey.NewApprovedFixture(t, "failed-contract")
	defer repo.Close()
	f.S.SetSole(journey.RuntimeFor(repo))
	ctx := context.Background()
	groups, err := trace.EnsureWorkgroups(ctx, repo, []config.WorkstreamID{stream}, f.S.Now(), intake.ServiceActor, "fixture")
	fixturekit.Must(t, err)
	g, _ := trace.WorkgroupOf(groups, stream)
	_, err = repo.EnsureManager(ctx, stream, f.S.Now(), intake.ServiceActor)
	fixturekit.Must(t, err)
	st, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	paused := trace.UnitSubject("resume") + "_waiting_project_amendment_" + string(g.ID) + "_1"
	_, err = repo.Transact(ctx, trace.Transaction{ExpectedVersion: st.Version, Transition: trace.Transition{
		Header:  trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: paused, Revision: 1, Project: repo.Project(), Workstream: stream, Unit: "resume", At: f.S.Now(), Actor: trace.Actor{Kind: "service", ID: intake.MasonRole}, Cause: "project-amendment-1"},
		Subject: trace.UnitSubject("resume"), From: st.Value, To: intake.UnitWaiting, Reason: "the protected contract request parks only this origin",
	}})
	fixturekit.Must(t, err)
	th, err := repo.Thread(stream, intake.MasonAgent("resume"))
	fixturekit.Must(t, err)
	c, apiFault := f.S.Lifecycle().RequestChange(ctx, string(stream), lifecycle.ChangeRequest{Key: "failed-delta", Kind: "amend", Scope: "workgroup", Change: "Change the offset type"})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	c = journey.CurrentChange(t, f.S, stream, c.ID)
	c.Requester = &trace.ProjectAmendment{ID: "1", Workgroup: g.ID, Origin: stream, Requester: trace.Actor{Kind: "agent", ID: intake.MasonAgent("resume")}, Citations: []string{"spec#1"}, Change: "Change the offset type", Reason: "The consumer requested another type"}
	fixturekit.Must(t, f.S.Lifecycle().SaveChange(ctx, repo, &c))
	if apiFault := f.S.Lifecycle().EndStalledChange(ctx, stream, c.ID, "drafting", uint64(c.Revision), "the architect never delivered the delta"); apiFault != nil {
		t.Fatal(apiFault)
	}
	advance := func() {
		t.Helper()
		wt, g, _, close, err := f.S.Lifecycle().ChangeContext(ctx, string(stream))
		fixturekit.Must(t, err)
		defer close()
		fixturekit.Must(t, f.S.Lifecycle().AdvanceChanges(ctx, wt, g))
	}
	advance()
	advance()
	resumed, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	if resumed.Value != intake.UnitImplementing {
		t.Fatalf("the failed change left its requester %s", resumed.Value)
	}
	now, err := repo.Thread(stream, intake.MasonAgent("resume"))
	fixturekit.Must(t, err)
	if len(now.Turns) != len(th.Turns)+1 {
		t.Fatalf("the requester received %d rulings", len(now.Turns)-len(th.Turns))
	}
	if prompt := now.Turns[len(now.Turns)-1].Request.Prompt; !strings.Contains(prompt, "failed before it was applied") || !strings.Contains(prompt, "the architect never delivered the delta") {
		t.Fatalf("the requester's ruling does not carry the failure: %s", prompt)
	}
	notices := changeFailureNotices(t, repo, stream, c.ID)
	if len(notices) != 1 || !strings.Contains(notices[0], "the architect never delivered the delta") || !strings.Contains(notices[0], "retry_change") {
		t.Fatalf("chief notices of the failed change: %v", notices)
	}
	if got := journey.CurrentChange(t, f.S, stream, c.ID); !got.FailureSettled {
		t.Fatalf("the failed change is not settled: %+v", got)
	}
	retried, apiFault := f.S.Lifecycle().RetryChange(ctx, stream, c.ID)
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	if retried.FailureSettled {
		t.Fatal("a retried change kept its settled failure")
	}
	if apiFault := f.S.Lifecycle().EndStalledChange(ctx, stream, c.ID, "drafting", uint64(retried.Revision), "the retry stalled too"); apiFault != nil {
		t.Fatal(apiFault)
	}
	advance()
	if notices := changeFailureNotices(t, repo, stream, c.ID); len(notices) != 2 || !slices.ContainsFunc(notices, func(n string) bool { return strings.Contains(n, "the retry stalled too.") }) {
		t.Fatalf("chief notices after the retried change failed: %v", notices)
	}
}

func TestFailedCrossRepoChangeTellsTheChiefThroughTheLeader(t *testing.T) {
	t.Parallel()
	f := journey.NewConfiguredProductionDesignFixture(t, journey.ProductionDesignConfiguration{Options: func(opts *journey.Options) {
		opts.Reconciliation.Hold = func(_ config.WorkstreamID, op coreadapter.Operation) bool {
			return op.Action == lifecycle.DeltaTurnAction
		}
	}})
	defer f.Stop(t)
	ctx := context.Background()
	f.StartOwner(t)
	packet := f.AwaitPacket(t)
	_, err := f.C.Ratify(ctx, f.Origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	fixturekit.Must(t, err)
	f.AwaitApproved(t)
	c, apiFault := f.S.Lifecycle().RequestChange(ctx, string(f.Stream2), lifecycle.ChangeRequest{Key: "failed-shared-delta", Kind: "amend", Scope: "workgroup", Change: "Clarify the shared rules"})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	journey.LifecycleAwait(t, f, "the stalled change ends", func() bool {
		latest := journey.CurrentChange(t, f.S, f.Stream2, c.ID)
		return latest.State == "failed" || f.S.Lifecycle().EndStalledChange(ctx, f.Stream2, c.ID, latest.State, uint64(latest.Revision), "the delta turns never ran") == nil
	})
	leader, leaderProject, other, otherProject := f.Stream1, f.Project, f.Stream2, f.Project2
	if f.Stream2 < f.Stream1 {
		leader, leaderProject, other, otherProject = f.Stream2, f.Project2, f.Stream1, f.Project
	}
	leaderRepo, otherRepo := mustRepo(t, f.S, leaderProject), mustRepo(t, f.S, otherProject)
	journey.LifecycleAwait(t, f, "the leader's events tell the chief", func() bool {
		return len(changeFailureNotices(t, leaderRepo, leader, c.ID)) > 0
	})
	notices := changeFailureNotices(t, leaderRepo, leader, c.ID)
	if len(notices) != 1 || !strings.Contains(notices[0], "the delta turns never ran") || !strings.Contains(notices[0], "retry_change") {
		t.Fatalf("chief notices of the failed shared change: %v", notices)
	}
	if got := changeFailureNotices(t, otherRepo, other, c.ID); len(got) != 0 {
		t.Fatalf("the failure was told through a member other than the leader: %v", got)
	}
}

func TestCancelUpstreamIncludesDependentsInSharedImpact(t *testing.T) {
	t.Parallel()
	f := journey.NewConfiguredProductionDesignFixture(t, journey.ProductionDesignConfiguration{})
	defer f.Stop(t)
	f.StartOwner(t)
	packet := f.AwaitPacket(t)
	_, err := f.C.Ratify(context.Background(), f.Origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	fixturekit.Must(t, err)
	f.AwaitApproved(t)
	c, apiFault := f.S.Lifecycle().RequestChange(context.Background(), string(f.Origin), lifecycle.ChangeRequest{Key: "cancel-server", Kind: "cancel", Scope: "workstream", Workstream: f.Stream1, Change: "Cancel the server and its dependent client"})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	f.Wait(t, "cancel impact", func() bool { return journey.CurrentChange(t, f.S, f.Origin, c.ID).State == "presented" })
	c = journey.CurrentChange(t, f.S, f.Origin, c.ID)
	if len(c.Impact.Streams) != 2 || c.Impact.Streams[0].Action != amendment.Cancel || c.Impact.Streams[1].Action != amendment.Cancel || !c.Impact.Streams[1].Cascade {
		t.Fatalf("dependent missing: %+v", c.Impact)
	}
	conversation, err := f.C.Conversation(context.Background(), f.Origin)
	fixturekit.Must(t, err)
	count := 0
	for _, entry := range conversation.Entries {
		if entry.Kind == "action" && entry.Turn == "change-"+c.ID+"-impact" {
			count++
			if !strings.Contains(entry.Text, string(f.Stream2)) || !strings.Contains(entry.Text, "dependent") {
				t.Fatal("same impact did not show dependent")
			}
		}
	}
	if count != 1 {
		t.Fatalf("cancel prompts=%d", count)
	}
	_, apiFault = f.S.Lifecycle().DecideChange(context.Background(), string(f.Origin), c.ID, lifecycle.ChangeDecision{Revision: c.Revision, Decision: "approve", PullRequests: "draft"})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	f.Wait(t, "dependent cancellation", func() bool { return journey.CurrentChange(t, f.S, f.Origin, c.ID).State == "applied" })
	for i, id := range []config.WorkstreamID{f.Stream1, f.Stream2} {
		repo, err := f.S.Repository(f.Repos[i].ID)
		fixturekit.Must(t, err)
		st, err := repo.Workflow(id, trace.FeatureSubject)
		fixturekit.Must(t, err)
		if st.Value != intake.AbandonedState {
			t.Fatal("dependent proceeded after cancel")
		}
	}
	f.CheckContinuation(t)
}
func TestCancelInterruptedClaimAndMemberApplicationRecovers(t *testing.T) {
	t.Parallel()
	f, stream, repo := newCleanupFixture(t, config.WorkspacesGit)
	ctx := context.Background()
	c := approvedCancellation(t, f, repo, stream, "workgroup", "draft")
	th, err := repo.Thread(stream, intake.MasonAgent("upload"))
	fixturekit.Must(t, err)
	_, err = repo.ClaimTurn(ctx, stream, intake.MasonAgent("upload"), "stopped", filepath.Join(f.Opts.Config.Root, "stopped"), f.S.Now())
	fixturekit.Must(t, err)
	wt, g, _, close, err := f.S.Lifecycle().ChangeContext(ctx, string(stream))
	fixturekit.Must(t, err)
	fixturekit.Must(t, f.S.Lifecycle().ApplyChange(ctx, wt, g, &c))
	if c.State == "applied" {
		t.Fatal("crossed running claim")
	}
	close()
	repo.Close()
	repo, err = trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
	fixturekit.Must(t, err)
	defer repo.Close()
	f.S.SetSole(journey.RuntimeFor(repo))
	wt, g, _, close, err = f.S.Lifecycle().ChangeContext(ctx, string(stream))
	fixturekit.Must(t, err)
	defer close()
	c = journey.CurrentChange(t, f.S, stream, c.ID)
	crash := errors.New("interrupted after member application")
	f.S.Boundary = func(step string) error {
		if step == "change-member-applied" {
			return crash
		}
		return nil
	}
	if err := f.S.Lifecycle().ApplyChange(ctx, wt, g, &c); !errors.Is(err, crash) {
		t.Fatalf("interruption: %v", err)
	}
	f.S.ClearBoundary()
	c = journey.CurrentChange(t, f.S, stream, c.ID)
	fixturekit.Must(t, f.S.Lifecycle().ApplyChange(ctx, wt, g, &c))
	if c.State != "applied" {
		t.Fatal("restart could not settle stopped claim")
	}
	thread, err := repo.Thread(stream, intake.MasonAgent("upload"))
	fixturekit.Must(t, err)
	if len(thread.Turns) != len(th.Turns) || thread.Active != "" || thread.Turns[0].Response == nil || !thread.Turns[0].Response.Result.Cancelled {
		t.Fatal("interrupted turn not retained as cancelled")
	}
	cleanup := newCleanup(f, repo)
	fixturekit.Must(t, cleanup.Pass(ctx))
	if len(cleanup.Failures) > 0 {
		t.Fatal(cleanup.Failures)
	}
	if got := branchFile(t, f, repo, stream, execution.UnitBranch(stream, "upload"), "upload.txt"); got != "upload\n" {
		t.Fatal("unlanded work lost on recovery")
	}
}
func TestRemovedSectionCreatesObsoleteUnitAndRemovalRevision(t *testing.T) {
	t.Parallel()
	f, stream, repo := journey.NewApprovedFixture(t, "remove-section")
	defer repo.Close()
	ctx := context.Background()
	recordUnitMerged(t, repo, stream, "resume")
	spec, graph, err := lifecycle.MemberSpecAndPlan(repo, stream)
	fixturekit.Must(t, err)
	p, err := plan.Parse([]byte(graph))
	fixturekit.Must(t, err)
	p.Units = slices.DeleteFunc(p.Units, func(u plan.Unit) bool { return u.ID == "resume" })
	p.Units[0].DependsOn = nil
	data, err := plan.Encode(p)
	fixturekit.Must(t, err)
	before := trace.ProjectSpec{Behavior: "Uploads", Slices: []trace.Slice{{Workstream: stream, Spec: spec, Plan: graph}}}
	after := trace.ProjectSpec{Behavior: "Uploads", Slices: []trace.Slice{{Workstream: stream, Spec: strings.Replace(spec, "1. An interrupted upload resumes from the last acknowledged chunk.\n", "", 1), Plan: string(data)}}}
	impact, err := amendment.ClassifyImpact("remove", before, after, []amendment.Member{{Workstream: stream, State: intake.BuildingState, Units: map[string]string{"resume": intake.UnitMerged, "dedupe": intake.UnitApproved}}})
	fixturekit.Must(t, err)
	c := lifecycle.WorkgroupChange{ID: "remove", Before: before, After: after, Result: &after, Impact: impact, Request: lifecycle.ChangeRequest{Change: "Remove resumption"}}
	fixturekit.Must(t, f.S.Lifecycle().ReworkChangeMember(ctx, repo, impact.Streams[0], &c))
	st, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	if st.Value != "obsolete" {
		t.Fatal("removed section's unit remained active")
	}
	sealed, _, _, err := seal.Latest(repo, stream)
	fixturekit.Must(t, err)
	doc, err := intake.SealedPlan(repo, stream, sealed.Revision.Plan)
	fixturekit.Must(t, err)
	p, err = plan.Parse([]byte(doc.Content))
	fixturekit.Must(t, err)
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
	f := journey.NewShedFixture(t, 1)
	base := f.Upstream(t)
	stream, repo := journey.SeedBuild(t, f, "not-started", fixturekit.ValidPlan, config.WorkspacesGit, base)
	defer repo.Close()
	id := intake.HandInWorkstream(f.Project, "not-started-new")
	ctx := context.Background()
	fixturekit.Must(t, repo.CreateWorkstreamOn(ctx, id, config.WorkspacesGit, f.S.Now(), intake.OwnerActor))
	f.S.SetSole(journey.RuntimeFor(repo))
	spec, graph, err := lifecycle.MemberSpecAndPlan(repo, stream)
	fixturekit.Must(t, err)
	c := lifecycle.WorkgroupChange{ID: "replan", Request: lifecycle.ChangeRequest{Change: "Use checkpoints"}}
	sl := trace.Slice{Workstream: id, Spec: intakefixture.AmendedSpec, Plan: graph}
	fixturekit.Must(t, f.S.Lifecycle().PlanChangeMember(ctx, repo, id, sl, c.ID))
	ready, err := f.S.Lifecycle().ChangeSealReady(ctx, repo, id, c.ID)
	fixturekit.Must(t, err)
	if ready {
		t.Fatal("replan admitted before sealing")
	}
	z := &intake.Sealer{S: f.S.Intake().SealerDependencies(), Repository: repo}
	ops, err := repo.Operations(id)
	fixturekit.Must(t, err)
	for _, op := range ops {
		if op.Operation.Action == intake.SealAction {
			result, err := z.Apply(ctx, op.Operation)
			fixturekit.Must(t, err)
			if result.Outcome != "succeeded" {
				t.Fatal(result)
			}
		}
	}
	before := trace.ProjectSpec{Behavior: "Uploads", Slices: []trace.Slice{{Workstream: id, Spec: spec, Plan: graph}}}
	after := trace.ProjectSpec{Behavior: "Uploads", Slices: []trace.Slice{sl}}
	impact, err := amendment.ClassifyImpact(c.ID, before, after, []amendment.Member{{Workstream: id, State: intake.RatifiedState, Units: map[string]string{}}})
	fixturekit.Must(t, err)
	if impact.Streams[0].Action != amendment.Replan {
		t.Fatal("not-started member not replanned")
	}
	c.Before, c.After, c.Result, c.Impact = before, after, &after, impact
	fixturekit.Must(t, f.S.Lifecycle().ReworkChangeMember(ctx, repo, impact.Streams[0], &c))
	b := &intake.Builder{S: f.S.Intake().BuilderDependencies(), Repository: repo}
	fixturekit.Must(t, b.Pass(ctx))
	ops, err = repo.Operations(id)
	fixturekit.Must(t, err)
	for _, op := range ops {
		if op.Operation.Action == intake.BuildAction {
			result, err := b.Apply(ctx, op.Operation)
			fixturekit.Must(t, err)
			if result.Outcome != "succeeded" {
				t.Fatal(result)
			}
		}
	}
	st, err := repo.Workflow(id, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	if st.Value != intake.UnitReady {
		t.Fatal("replacement plan failed to initialize")
	}
	if _, err := os.Stat(filepath.Join(f.Clone, "resume.go")); !os.IsNotExist(err) {
		t.Fatal("planning committed code")
	}
}
