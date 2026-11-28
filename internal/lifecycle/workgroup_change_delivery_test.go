package lifecycle_test

import (
	"context"
	"encoding/json"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/amendment"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func presentDeliveryDelta(t *testing.T, p *journey.PublicationFixture) lifecycle.WorkgroupChange {
	t.Helper()
	ctx := context.Background()
	_, err := trace.EnsureWorkgroups(ctx, p.Repository, []config.WorkstreamID{p.Stream}, p.S.Now(), intake.ServiceActor, "fixture")
	fixturekit.Must(t, err)
	_, err = p.Repository.EnsureManager(ctx, p.Stream, p.S.Now(), intake.ServiceActor)
	fixturekit.Must(t, err)
	c, apiFault := p.S.Lifecycle().RequestChange(ctx, string(p.Stream), lifecycle.ChangeRequest{Key: "checkpoint", Kind: "amend", Scope: "workgroup", Change: "Resume from checkpoints"})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	c.After = c.Before
	c.After.Slices = slices.Clone(c.Before.Slices)
	c.After.Slices[0].Spec = intakefixture.AmendedSpec
	c.Impact, err = amendment.ClassifyImpact(c.ID, c.Before, c.After, c.Members)
	fixturekit.Must(t, err)
	c.State = "presented"
	fixturekit.Must(t, p.S.Lifecycle().SaveChange(ctx, p.Repository, &c))
	c, apiFault = p.S.Lifecycle().DecideChange(ctx, string(p.Stream), c.ID, lifecycle.ChangeDecision{Revision: c.Revision, Decision: "approve"})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	return c
}
func TestAmendOpenPullRequestRequiresFreshApprovalOnSameBranch(t *testing.T) {
	t.Parallel()
	t.Run("changed-description", func(t *testing.T) { testAmendOpenPullRequest(t, true) })
	t.Run("unchanged-description", func(t *testing.T) { testAmendOpenPullRequest(t, false) })
}
func testAmendOpenPullRequest(t *testing.T, changeDescription bool) {
	p := journey.NewPublicationFixture(t, "commit-per-unit")
	ctx := context.Background()
	oldApproval := p.Approve(t, nil)
	op := p.Request(t)
	result, err := p.Publisher().Apply(ctx, op)
	fixturekit.Must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("initial publication: %+v", result)
	}
	p.Pulls.SyncHead = true
	oldPR := p.Pulls.Prs[0]
	oldTip, _ := p.ForkBranch(t)
	c := presentDeliveryDelta(t, p)
	if c.Impact.Streams[0].Action != amendment.Rework {
		t.Fatal("open PR was not classified for rework")
	}
	wt, g, _, close, err := p.S.Lifecycle().ChangeContext(ctx, string(p.Stream))
	fixturekit.Must(t, err)
	fixturekit.Must(t, p.S.Lifecycle().ApplyChange(ctx, wt, g, &c))
	close()
	if p.Feature(t) != intake.BuildingState {
		t.Fatal("open PR did not return to the build loop")
	}
	if _, reason, err := p.S.DeliveryGate(ctx, p.Repository, p.Stream, oldApproval.Description); err != nil || reason == "" {
		t.Fatalf("old approval still valid: %q %v", reason, err)
	}
	fixturekit.Must(t, p.Publisher().Pass(ctx))
	if len(p.Operations(t)) != 1 {
		t.Fatal("rework published without fresh approval")
	}
	if tip, _ := p.ForkBranch(t); tip != oldTip {
		t.Fatal("amendment approval pushed code")
	}
	journey.MergeDirectly(t, p.ShedFixture, p.Repository, p.Stream, amendment.RevisionID(c.ID, "resume"), map[string]string{"resume.go": "package demo\n// Resume restores the checkpoint.\n"})
	a := &journey.FinalReviewer{S: p.S, Repository: p.Repository}
	fixturekit.Must(t, a.Pass(ctx))
	fixturekit.Must(t, a.Pass(ctx))
	ops := journey.FinalOperations(t, p.Repository, p.Stream)
	if len(ops) != 1 {
		t.Fatalf("fresh reviews: %d", len(ops))
	}
	in, err := delivery.DecodeFinalReview(ops[0].Operation)
	fixturekit.Must(t, err)
	p.FinalTurn(in.Review, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Checkpoint delta holds", "findings": []any{}})
		return err
	})
	result, err = a.Apply(ctx, ops[0].Operation)
	fixturekit.Must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("fresh final review: %+v", result)
	}
	report, _, err := delivery.LatestFinalReport(p.Repository, p.Stream)
	fixturekit.Must(t, err)
	if report.Seal <= p.Report.Seal || report.SpecHash == p.Report.SpecHash {
		t.Fatal("review did not pin amended intent")
	}
	fixturekit.Must(t, p.Publisher().Pass(ctx))
	if len(p.Operations(t)) != 1 {
		t.Fatal("fresh review reused old owner approval")
	}
	description := "Uploads resume from durable checkpoints. The reviewed revision preserves acknowledged chunks."
	var edit *string
	expectedUpdates := 0
	if changeDescription {
		edit, expectedUpdates = &description, 1
	}
	approval := p.Approve(t, edit)
	if !changeDescription && approval.Description != oldApproval.Description {
		t.Fatal("unchanged description fixture changed the approved text")
	}
	op = p.Request(t)
	inPublish, err := delivery.DecodePublish(op)
	fixturekit.Must(t, err)
	if inPublish.Retarget != nil || inPublish.Maintenance {
		t.Fatalf("wrong PR rework operation: %+v", inPublish)
	}
	result, err = p.Publisher().Apply(ctx, op)
	fixturekit.Must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("PR rework: %+v", result)
	}
	if p.Pulls.Creates != 1 || p.Pulls.Updates != expectedUpdates || p.Pulls.Prs[0].Number != oldPR.Number || p.Pulls.Prs[0].Head != oldPR.Head || p.Pulls.Prs[0].Body != approval.Description {
		t.Fatalf("rework did not update the same PR with fresh approved text: creates=%d updates=%d expected updates=%d old=%+v current=%+v approved description=%q", p.Pulls.Creates, p.Pulls.Updates, expectedUpdates, oldPR, p.Pulls.Prs[0], approval.Description)
	}
	tip, _ := p.ForkBranch(t)
	if tip == oldTip {
		t.Fatal("approved revision was not pushed")
	}
	if commit := fixturekit.DemoGit(t, p.Clone, "-C", p.Clone, "cat-file", "commit", tip); !strings.Contains(commit, "gpgsig -----BEGIN PGP SIGNATURE-----") || !strings.Contains(commit, "Delivery Owner <delivery@example.invalid>") {
		t.Fatalf("rework lacks the configured owner's signature: %q", commit)
	}
}
func TestAmendMergedWorkCreatesOneFollowupAndKeepsMerge(t *testing.T) {
	t.Parallel()
	p := journey.NewPublicationFixture(t, "commit-per-unit")
	ctx := context.Background()
	p.Approve(t, nil)
	result, err := p.Publisher().Apply(ctx, p.Request(t))
	fixturekit.Must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	pr := p.Pulls.Prs[0]
	fixturekit.Must(t, delivery.RecordOwnPullRequestOutcome(ctx, p.Repository, p.S.Now(), p.Stream, "owner", pr.Number, delivery.OwnPullRequestOutcomeMerged))
	beforeState, err := p.Repository.Workflow(p.Stream, trace.FeatureSubject)
	fixturekit.Must(t, err)
	tip := strings.TrimSpace(fixturekit.DemoGit(t, p.Clone, "-C", p.Clone, "rev-parse", intake.FeatureBranch(p.Stream)))
	c := presentDeliveryDelta(t, p)
	if c.Impact.Streams[0].Action != amendment.FollowUp {
		t.Fatal("merged work was reopened")
	}
	wt, g, _, close, err := p.S.Lifecycle().ChangeContext(ctx, string(p.Stream))
	fixturekit.Must(t, err)
	defer close()
	fixturekit.Must(t, p.S.Lifecycle().ApplyChange(ctx, wt, g, &c))
	id := c.Followups[p.Stream]
	if id == "" || c.State == "applied" {
		t.Fatal("follow-up must wait for its seal")
	}
	z := &intake.Sealer{S: p.S.Intake().SealerDependencies(), Repository: p.Repository}
	fixturekit.Must(t, z.Pass(ctx))
	ops, err := p.Repository.Operations(id)
	fixturekit.Must(t, err)
	for _, op := range ops {
		if op.Operation.Action != intake.SealAction {
			continue
		}
		result, err = z.Apply(ctx, op.Operation)
		fixturekit.Must(t, err)
		if result.Outcome != "succeeded" {
			t.Fatalf("follow-up seal: %+v", result)
		}
	}
	fixturekit.Must(t, p.S.Lifecycle().ApplyChange(ctx, wt, g, &c))
	if c.State != "applied" {
		t.Fatal("follow-up did not become ready")
	}
	groups, _, err := trace.Workgroups(wt)
	fixturekit.Must(t, err)
	group, _ := trace.WorkgroupOf(groups, id)
	if len(group.Members) != 2 || group.Members[0] != p.Stream {
		t.Fatal("follow-up gained an independent workgroup")
	}
	close()
	p.Reopen(t)
	wt, g, _, close, err = p.S.Lifecycle().ChangeContext(ctx, string(id))
	fixturekit.Must(t, err)
	defer close()
	c = journey.CurrentChange(t, p.S, id, c.ID)
	if c.Followups[p.Stream] != id || c.State != "applied" {
		t.Fatal("restart duplicated follow-up")
	}
	afterState, err := p.Repository.Workflow(p.Stream, trace.FeatureSubject)
	fixturekit.Must(t, err)
	if afterState != beforeState || strings.TrimSpace(fixturekit.DemoGit(t, p.Clone, "-C", p.Clone, "rev-parse", intake.FeatureBranch(p.Stream))) != tip {
		t.Fatal("merged member changed")
	}
	revision, found, err := trace.ApprovedWorkgroupRevision(wt, g.Members[0])
	fixturekit.Must(t, err)
	if !found || len(revision.Spec.Slices) != 2 {
		t.Fatal("shared project spec lost merged history or follow-up")
	}
	data, err := json.Marshal(revision)
	fixturekit.Must(t, err)
	if !strings.Contains(string(data), string(id)) {
		t.Fatal("follow-up absent")
	}
	if _, ok := trace.SliceFor(revision.Spec, p.Stream); !ok {
		t.Fatal("merged slice was replaced")
	}
	if _, err := plan.Parse([]byte(revision.Spec.Slices[1].Plan)); err != nil {
		t.Fatal(err)
	}
}
