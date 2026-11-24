package lifecycle_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/amendment"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func presentDeliveryDelta(t *testing.T, p *publicationFixture) lifecycle.WorkgroupChange {
	t.Helper()
	ctx := context.Background()
	_, err := trace.EnsureWorkgroups(ctx, p.repository, []config.WorkstreamID{p.stream}, p.s.now(), intake.ServiceActor, "fixture")
	intakefixture.Must(t, err)
	_, err = p.repository.EnsureChiefOfStaff(ctx, p.stream, p.s.now(), intake.ServiceActor)
	intakefixture.Must(t, err)
	c, apiFault := p.s.requestChange(ctx, string(p.stream), lifecycle.ChangeRequest{Key: "checkpoint", Kind: "amend", Scope: "workgroup", Change: "Resume from checkpoints"})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	c.After = c.Before
	c.After.Slices = slices.Clone(c.Before.Slices)
	c.After.Slices[0].Spec = amendedSpec
	c.Impact, err = amendment.ClassifyImpact(c.ID, c.Before, c.After, c.Members)
	intakefixture.Must(t, err)
	c.State = "presented"
	intakefixture.Must(t, p.s.saveChange(ctx, p.repository, &c))
	c, apiFault = p.s.decideChange(ctx, string(p.stream), c.ID, lifecycle.ChangeDecision{Revision: c.Revision, Decision: "approve"})
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
	p := newPublicationFixture(t, "commit-per-unit")
	ctx := context.Background()
	oldApproval := p.approve(t, nil)
	op := p.request(t)
	result, err := p.publisher().Apply(ctx, op)
	intakefixture.Must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("initial publication: %+v", result)
	}
	p.pulls.SyncHead = true
	oldPR := p.pulls.Prs[0]
	oldTip, _ := p.forkBranch(t)
	c := presentDeliveryDelta(t, p)
	if c.Impact.Streams[0].Action != amendment.Rework {
		t.Fatal("open PR was not classified for rework")
	}
	wt, g, _, close, err := p.s.changeContext(ctx, string(p.stream))
	intakefixture.Must(t, err)
	intakefixture.Must(t, p.s.applyChange(ctx, wt, g, &c))
	close()
	if p.feature(t) != intakefixture.BuildingState {
		t.Fatal("open PR did not return to the build loop")
	}
	if _, reason, err := p.s.deliveryGate(ctx, p.repository, p.stream, oldApproval.Description); err != nil || reason == "" {
		t.Fatalf("old approval still valid: %q %v", reason, err)
	}
	intakefixture.Must(t, p.publisher().Pass(ctx))
	if len(p.operations(t)) != 1 {
		t.Fatal("rework published without fresh approval")
	}
	if tip, _ := p.forkBranch(t); tip != oldTip {
		t.Fatal("amendment approval pushed code")
	}
	mergeDirectly(t, p.shedFixture, p.repository, p.stream, amendment.RevisionID(c.ID, "resume"), map[string]string{"resume.go": "package demo\n// Resume restores the checkpoint.\n"})
	a := &finalReviewer{s: p.s, repository: p.repository}
	intakefixture.Must(t, a.Pass(ctx))
	intakefixture.Must(t, a.Pass(ctx))
	ops := finalOperations(t, p.repository, p.stream)
	if len(ops) != 1 {
		t.Fatalf("fresh reviews: %d", len(ops))
	}
	in, err := delivery.DecodeFinalReview(ops[0].Operation)
	intakefixture.Must(t, err)
	p.finalTurn(in.Review, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := intakefixture.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Checkpoint delta holds", "findings": []any{}})
		return err
	})
	result, err = a.Apply(ctx, ops[0].Operation)
	intakefixture.Must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("fresh final review: %+v", result)
	}
	report, _, err := delivery.LatestFinalReport(p.repository, p.stream)
	intakefixture.Must(t, err)
	if report.Seal <= p.report.Seal || report.SpecHash == p.report.SpecHash {
		t.Fatal("review did not pin amended intent")
	}
	intakefixture.Must(t, p.publisher().Pass(ctx))
	if len(p.operations(t)) != 1 {
		t.Fatal("fresh review reused old owner approval")
	}
	description := "Uploads resume from durable checkpoints. The reviewed revision preserves acknowledged chunks."
	var edit *string
	expectedUpdates := 0
	if changeDescription {
		edit, expectedUpdates = &description, 1
	}
	approval := p.approve(t, edit)
	if !changeDescription && approval.Description != oldApproval.Description {
		t.Fatal("unchanged description fixture changed the approved text")
	}
	op = p.request(t)
	inPublish, err := delivery.DecodePublish(op)
	intakefixture.Must(t, err)
	if inPublish.Retarget != nil || inPublish.Maintenance {
		t.Fatalf("wrong PR rework operation: %+v", inPublish)
	}
	result, err = p.publisher().Apply(ctx, op)
	intakefixture.Must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("PR rework: %+v", result)
	}
	if p.pulls.Creates != 1 || p.pulls.Updates != expectedUpdates || p.pulls.Prs[0].Number != oldPR.Number || p.pulls.Prs[0].Head != oldPR.Head || p.pulls.Prs[0].Body != approval.Description {
		t.Fatalf("rework did not update the same PR with fresh approved text: creates=%d updates=%d expected updates=%d old=%+v current=%+v approved description=%q", p.pulls.Creates, p.pulls.Updates, expectedUpdates, oldPR, p.pulls.Prs[0], approval.Description)
	}
	tip, _ := p.forkBranch(t)
	if tip == oldTip {
		t.Fatal("approved revision was not pushed")
	}
	if commit := fixturekit.DemoGit(t, p.clone, "-C", p.clone, "cat-file", "commit", tip); !strings.Contains(commit, "gpgsig -----BEGIN PGP SIGNATURE-----") || !strings.Contains(commit, "Delivery Owner <delivery@example.invalid>") {
		t.Fatalf("rework lacks the configured owner's signature: %q", commit)
	}
}
func TestAmendMergedWorkCreatesOneFollowupAndKeepsMerge(t *testing.T) {
	t.Parallel()
	p := newPublicationFixture(t, "commit-per-unit")
	ctx := context.Background()
	p.approve(t, nil)
	result, err := p.publisher().Apply(ctx, p.request(t))
	intakefixture.Must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	pr := p.pulls.Prs[0]
	intakefixture.Must(t, delivery.RecordOwnPullRequestOutcome(ctx, p.repository, p.s.now(), p.stream, "owner", pr.Number, delivery.OwnPullRequestOutcomeMerged))
	beforeState, err := p.repository.Workflow(p.stream, trace.FeatureSubject)
	intakefixture.Must(t, err)
	tip := strings.TrimSpace(fixturekit.DemoGit(t, p.clone, "-C", p.clone, "rev-parse", intake.FeatureBranch(p.stream)))
	c := presentDeliveryDelta(t, p)
	if c.Impact.Streams[0].Action != amendment.FollowUp {
		t.Fatal("merged work was reopened")
	}
	wt, g, _, close, err := p.s.changeContext(ctx, string(p.stream))
	intakefixture.Must(t, err)
	defer close()
	intakefixture.Must(t, p.s.applyChange(ctx, wt, g, &c))
	id := c.Followups[p.stream]
	if id == "" || c.State == "applied" {
		t.Fatal("follow-up must wait for its seal")
	}
	z := &intake.Sealer{S: p.s.intake().SealerDependencies(), Repository: p.repository}
	intakefixture.Must(t, z.Pass(ctx))
	ops, err := p.repository.Operations(id)
	intakefixture.Must(t, err)
	for _, op := range ops {
		if op.Operation.Action != intake.SealAction {
			continue
		}
		result, err = z.Apply(ctx, op.Operation)
		intakefixture.Must(t, err)
		if result.Outcome != "succeeded" {
			t.Fatalf("follow-up seal: %+v", result)
		}
	}
	intakefixture.Must(t, p.s.applyChange(ctx, wt, g, &c))
	if c.State != "applied" {
		t.Fatal("follow-up did not become ready")
	}
	groups, _, err := trace.Workgroups(wt)
	intakefixture.Must(t, err)
	group, _ := trace.WorkgroupOf(groups, id)
	if len(group.Members) != 2 || group.Members[0] != p.stream {
		t.Fatal("follow-up gained an independent workgroup")
	}
	close()
	p.reopen(t)
	wt, g, _, close, err = p.s.changeContext(ctx, string(id))
	intakefixture.Must(t, err)
	defer close()
	c = currentChange(t, p.s, id, c.ID)
	if c.Followups[p.stream] != id || c.State != "applied" {
		t.Fatal("restart duplicated follow-up")
	}
	afterState, err := p.repository.Workflow(p.stream, trace.FeatureSubject)
	intakefixture.Must(t, err)
	if afterState != beforeState || strings.TrimSpace(fixturekit.DemoGit(t, p.clone, "-C", p.clone, "rev-parse", intake.FeatureBranch(p.stream))) != tip {
		t.Fatal("merged member changed")
	}
	revision, found, err := trace.ApprovedWorkgroupRevision(wt, g.Members[0])
	intakefixture.Must(t, err)
	if !found || len(revision.Spec.Slices) != 2 {
		t.Fatal("shared project spec lost merged history or follow-up")
	}
	data, err := json.Marshal(revision)
	intakefixture.Must(t, err)
	if !strings.Contains(string(data), string(id)) {
		t.Fatal("follow-up absent")
	}
	if _, ok := trace.SliceFor(revision.Spec, p.stream); !ok {
		t.Fatal("merged slice was replaced")
	}
	if _, err := plan.Parse([]byte(revision.Spec.Slices[1].Plan)); err != nil {
		t.Fatal(err)
	}
}
