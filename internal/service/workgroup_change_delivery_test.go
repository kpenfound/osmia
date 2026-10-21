package service

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/amendment"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// presentDeliveryDelta supplies a debated delta for controller tests. The
// production design-thread test exercises the tools that produce this packet.
func presentDeliveryDelta(t *testing.T, p *publicationFixture) WorkgroupChange {
	t.Helper()
	ctx := context.Background()
	_, err := trace.EnsureWorkgroups(ctx, p.repository, []config.WorkstreamID{p.stream}, p.s.now(), serviceActor, "fixture")
	must(t, err)
	_, err = p.repository.EnsureChiefOfStaff(ctx, p.stream, p.s.now(), serviceActor)
	must(t, err)
	c, api := p.s.requestChange(ctx, string(p.stream), ChangeRequest{Key: "checkpoint", Kind: "amend", Scope: "workgroup", Change: "Resume from checkpoints"})
	if api != nil {
		t.Fatal(api)
	}
	c.After = c.Before
	c.After.Slices = slices.Clone(c.Before.Slices)
	c.After.Slices[0].Spec = amendedSpec
	c.Impact, err = amendment.ClassifyImpact(c.ID, c.Before, c.After, c.Members)
	must(t, err)
	c.State = "presented"
	must(t, p.s.saveChange(ctx, p.repository, &c))
	c, api = p.s.decideChange(ctx, string(p.stream), c.ID, ChangeDecision{Revision: c.Revision, Decision: "approve"})
	if api != nil {
		t.Fatal(api)
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
	must(t, err)
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
	must(t, err)
	must(t, p.s.applyChange(ctx, wt, g, &c))
	close()
	if p.feature(t) != BuildingState {
		t.Fatal("open PR did not return to the build loop")
	}
	if _, reason, err := p.s.deliveryGate(ctx, p.repository, p.stream, oldApproval.Description); err != nil || reason == "" {
		t.Fatalf("old approval still valid: %q %v", reason, err)
	}
	must(t, p.publisher().Pass(ctx))
	if len(p.operations(t)) != 1 {
		t.Fatal("rework published without fresh approval")
	}
	if tip, _ := p.forkBranch(t); tip != oldTip {
		t.Fatal("amendment approval pushed code")
	}
	mergeDirectly(t, p.shedFixture, p.repository, p.stream, amendment.RevisionID(c.ID, "resume"), map[string]string{"resume.go": "package demo\n// Resume restores the checkpoint.\n"})
	a := &finalReviewer{s: p.s, repository: p.repository}
	must(t, a.Pass(ctx))
	must(t, a.Pass(ctx))
	ops := finalOperations(t, p.repository, p.stream)
	if len(ops) != 1 {
		t.Fatalf("fresh reviews: %d", len(ops))
	}
	in, err := decodeFinalReview(ops[0].Operation)
	must(t, err)
	p.finalTurn(in.Review, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Checkpoint delta holds", "findings": []any{}})
		return err
	})
	result, err = a.Apply(ctx, ops[0].Operation)
	must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("fresh final review: %+v", result)
	}
	report, _, err := latestFinalReport(p.repository, p.stream)
	must(t, err)
	if report.Seal <= p.report.Seal || report.SpecHash == p.report.SpecHash {
		t.Fatal("review did not pin amended intent")
	}
	// The reviewed branch still needs its own owner's delivery approval.
	must(t, p.publisher().Pass(ctx))
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
	inPublish, err := decodePublish(op)
	must(t, err)
	if inPublish.Retarget == nil || inPublish.Retarget.Number != oldPR.Number || inPublish.Maintenance {
		t.Fatalf("wrong PR rework operation: %+v", inPublish)
	}
	result, err = p.publisher().Apply(ctx, op)
	must(t, err)
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
	if commit := demoGit(t, p.clone, "-C", p.clone, "cat-file", "commit", tip); !strings.Contains(commit, "gpgsig -----BEGIN PGP SIGNATURE-----") || !strings.Contains(commit, "Delivery Owner <delivery@example.invalid>") {
		t.Fatalf("rework lacks the configured owner's signature: %q", commit)
	}
}

func TestAmendMergedWorkCreatesOneFollowupAndKeepsMerge(t *testing.T) {
	t.Parallel()
	p := newPublicationFixture(t, "commit-per-unit")
	ctx := context.Background()
	p.approve(t, nil)
	result, err := p.publisher().Apply(ctx, p.request(t))
	must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	pr := p.pulls.Prs[0]
	must(t, recordOwnPullRequestOutcome(ctx, p.repository, p.s.now(), p.stream, "owner", pr.Number, ownPullRequestOutcomeMerged))
	beforeState, err := p.repository.Workflow(p.stream, trace.FeatureSubject)
	must(t, err)
	tip := strings.TrimSpace(demoGit(t, p.clone, "-C", p.clone, "rev-parse", featureBranch(p.stream)))
	c := presentDeliveryDelta(t, p)
	if c.Impact.Streams[0].Action != amendment.FollowUp {
		t.Fatal("merged work was reopened")
	}
	wt, g, _, close, err := p.s.changeContext(ctx, string(p.stream))
	must(t, err)
	defer close()
	must(t, p.s.applyChange(ctx, wt, g, &c))
	id := c.Followups[p.stream]
	if id == "" || c.State == "applied" {
		t.Fatal("follow-up must wait for its seal")
	}
	z := &sealer{s: p.s, repository: p.repository}
	must(t, z.Pass(ctx))
	ops, err := p.repository.Operations(id)
	must(t, err)
	for _, op := range ops {
		if op.Operation.Action != SealAction {
			continue
		}
		result, err = z.Apply(ctx, op.Operation)
		must(t, err)
		if result.Outcome != "succeeded" {
			t.Fatalf("follow-up seal: %+v", result)
		}
	}
	must(t, p.s.applyChange(ctx, wt, g, &c))
	if c.State != "applied" {
		t.Fatal("follow-up did not become ready")
	}
	// Reconciliation and a reopened trace preserve the one allocated identity.
	groups, _, err := trace.Workgroups(wt)
	must(t, err)
	group, _ := trace.WorkgroupOf(groups, id)
	if len(group.Members) != 2 || group.Members[0] != p.stream {
		t.Fatal("follow-up gained an independent workgroup")
	}
	close()
	p.reopen(t)
	wt, g, _, close, err = p.s.changeContext(ctx, string(id))
	must(t, err)
	defer close()
	c = currentChange(t, p.s, id, c.ID)
	if c.Followups[p.stream] != id || c.State != "applied" {
		t.Fatal("restart duplicated follow-up")
	}
	afterState, err := p.repository.Workflow(p.stream, trace.FeatureSubject)
	must(t, err)
	if afterState != beforeState || strings.TrimSpace(demoGit(t, p.clone, "-C", p.clone, "rev-parse", featureBranch(p.stream))) != tip {
		t.Fatal("merged member changed")
	}
	revision, found, err := trace.ApprovedWorkgroupRevision(wt, g.Members[0])
	must(t, err)
	if !found || len(revision.Spec.Slices) != 2 {
		t.Fatal("shared project spec lost merged history or follow-up")
	}
	data, err := json.Marshal(revision)
	must(t, err)
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
