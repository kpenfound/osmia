package lifecycle_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDeliveredDriftRequiresApprovalAndUpdatesExistingPR(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, style                      string
		noFork, conflict, projectRequest bool
	}{
		{name: "commits", style: "commit-per-unit"},
		{name: "squash", style: "squash"},
		{name: "upstream", style: "commit-per-unit", noFork: true},
		{name: "conflict", style: "squash", conflict: true},
		{name: "project-request", style: "commit-per-unit", projectRequest: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newUnpushedPublicationFixture(t, tc.style, "")
			if tc.noFork {
				useUpstreamForPush(t, p)
			}
			p.publishDraft(t)
			ctx := context.Background()
			p.approve(t, nil)
			result := settleOperation(t, p.s, p.repository, p.stream, p.request(t), p.publisher())
			if result.Outcome != "succeeded" {
				t.Fatal(result)
			}
			p.pulls.SyncHead = true
			before, _ := p.forkBranch(t)
			fm := cadenceForeman(p.shedFixture, p.repository, "1m")
			files := map[string]string{"DRIFT.md": "new upstream content\n"}
			if tc.conflict {
				files["resume.go"] = "package demo\n// Upstream resume.\n"
			}
			advanceUpstream(t, p.shedFixture, files)
			calls := p.pulls.Finds
			if why, err := fm.driftDue(p.stream, p.s.now().Add(24*time.Hour)); err != nil || why != "" {
				t.Fatalf("automatic drift: %q %v", why, err)
			}
			passDrifts(t, fm)
			if p.pulls.Finds != calls || len(driftOperations(t, p.repository, p.stream)) != 0 {
				t.Fatal("delivered branch was polled")
			}
			if tc.projectRequest {
				out, apiFault := p.s.rebaseProject(ctx, api.ProjectRebaseRequest{Project: p.project})
				if apiFault != nil || len(out.Covered) != 1 {
					t.Fatalf("project rebase: %+v %v", out, apiFault)
				}
			} else if _, apiFault := p.s.rebaseWorkstream(ctx, string(p.stream)); apiFault != nil {
				t.Fatal(apiFault)
			}
			passDrifts(t, fm)
			ops := driftOperations(t, p.repository, p.stream)
			if len(ops) != 1 {
				t.Fatalf("drifts: %v", ops)
			}
			if tc.conflict {
				d := drifter{fm}
				awaitResolution(t, p.s, d, p.stream, ops[0].Operation, "its mason's resolution of resume.go")
				w := resolutionWorkspace(t, p.shedFixture, p.stream)
				intakefixture.Must(t, os.WriteFile(filepath.Join(w.Path, "resume.go"), []byte("package demo\n// Upstream resume and feature resume.\n"), 0600))
				completeDriftTurn(t, p.shedFixture, p.repository, p.stream, service.TestDriftMasonAgent, resolvedDone("Combined resume behavior"))
				awaitResolution(t, p.s, d, p.stream, ops[0].Operation, "review 1 of candidate")
				if tip, _ := p.forkBranch(t); tip != before {
					t.Fatal("conflict resolution pushed without approval")
				}
				completeDriftTurn(t, p.shedFixture, p.repository, p.stream, service.TestDriftReviewerAgent, driftVerdict(t, approvedResolution))
			}
			result = settleOperation(t, p.s, p.repository, p.stream, ops[0].Operation, drifter{fm})
			if result.Outcome != "succeeded" {
				t.Fatal(result)
			}
			p.reopen(t)
			pending, err := lifecycle.UpstreamDeliveryPending(p.repository, p.s.cfg, p.stream)
			intakefixture.Must(t, err)
			if !pending {
				t.Fatal("maintenance lost after restart")
			}
			reviewer := &finalReviewer{s: p.s, repository: p.repository}
			intakefixture.Must(t, reviewer.Pass(ctx))
			op := fixturekit.ActionOperation(t, p.repository, p.stream, lifecycle.DeliveryReviewAction)
			in, err := delivery.DecodeFinalReview(op)
			intakefixture.Must(t, err)
			p.finalTurn(in.Review, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
				_, err := intakefixture.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Ready after drift", "findings": []any{}})
				return err
			})
			result = settleOperation(t, p.s, p.repository, p.stream, op, reviewer)
			if result.Outcome != "succeeded" {
				t.Fatal(result)
			}
			intakefixture.Must(t, p.publisher().Pass(ctx))
			all, err := p.repository.Operations(p.stream)
			intakefixture.Must(t, err)
			for _, o := range all {
				if o.Operation.Action == lifecycle.PublishUpstreamAction {
					t.Fatal("published without new approval")
				}
			}
			if tip, _ := p.forkBranch(t); tip != before {
				t.Fatal("remote changed before approval")
			}
			presented, apiFault := p.s.deliveryPresentation(ctx, string(p.stream))
			if apiFault != nil || !presented.Maintenance || presented.Approval != nil {
				t.Fatalf("presentation %+v %v", presented, apiFault)
			}
			description := "Updated drift delivery\n\nApproved after rebase."
			p.approve(t, &description)
			intakefixture.Must(t, p.publisher().Pass(ctx))
			publish := fixturekit.ActionOperation(t, p.repository, p.stream, lifecycle.PublishUpstreamAction)
			p.pulls.Lose = true
			if _, err := p.publisher().Apply(ctx, publish); err == nil {
				t.Fatal("expected lost PR update response")
			}
			p.reopen(t)
			result = settleOperation(t, p.s, p.repository, p.stream, publish, p.publisher())
			if result.Outcome != "succeeded" {
				t.Fatal(result)
			}
			tip, _ := p.forkBranch(t)
			if tip == before || p.pulls.Creates != 1 || p.pulls.Prs[0].Body != description || p.feature(t) != intake.DeliveredState {
				t.Fatalf("delivery tip %s creates %d state %s", tip, p.pulls.Creates, p.feature(t))
			}
			pending, err = lifecycle.UpstreamDeliveryPending(p.repository, p.s.cfg, p.stream)
			intakefixture.Must(t, err)
			if pending {
				t.Fatal("publication did not finish maintenance")
			}
			fm = cadenceForeman(p.shedFixture, p.repository, "1m")
			if _, apiFault := p.s.rebaseWorkstream(ctx, string(p.stream)); apiFault != nil {
				t.Fatal(apiFault)
			}
			passDrifts(t, fm)
			drifts := driftOperations(t, p.repository, p.stream)
			settleOperation(t, p.s, p.repository, p.stream, drifts[len(drifts)-1].Operation, drifter{fm})
			pending, err = lifecycle.UpstreamDeliveryPending(p.repository, p.s.cfg, p.stream)
			intakefixture.Must(t, err)
			if pending {
				t.Fatal("unchanged base requested another delivery")
			}

			result, err = p.publisher().Apply(ctx, publish)
			intakefixture.Must(t, err)
			if result.Outcome != "succeeded" || p.pulls.Creates != 1 {
				t.Fatal("retry duplicated publication")
			}
		})
	}
}

func TestDeliveredDriftConflictOutsideUnitFootprintCompletesEndToEnd(t *testing.T) {
	t.Parallel()
	p := newPublicationFixture(t, "squash")
	ctx := context.Background()
	p.approve(t, nil)
	result := settleOperation(t, p.s, p.repository, p.stream, p.request(t), p.publisher())
	if result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	p.pulls.SyncHead = true
	before, _ := p.forkBranch(t)
	fm := cadenceForeman(p.shedFixture, p.repository, "1m")
	moveFeature(t, p.shedFixture, p.stream, map[string]string{"shared.go": "package demo\n// Feature shared helper.\n"})
	advanceUpstream(t, p.shedFixture, map[string]string{"DRIFT.md": "new upstream content\n", "shared.go": "package demo\n// Upstream shared helper.\n"})
	if _, apiFault := p.s.rebaseWorkstream(ctx, string(p.stream)); apiFault != nil {
		t.Fatal(apiFault)
	}
	passDrifts(t, fm)
	ops := driftOperations(t, p.repository, p.stream)
	if len(ops) != 1 {
		t.Fatalf("drifts: %v", ops)
	}
	d := drifter{fm}
	awaitResolution(t, p.s, d, p.stream, ops[0].Operation, "its mason's resolution of shared.go")
	w := resolutionWorkspace(t, p.shedFixture, p.stream)
	intakefixture.Must(t, os.WriteFile(filepath.Join(w.Path, "shared.go"), []byte("package demo\n// Upstream and feature shared helper.\n"), 0600))
	completeDriftTurn(t, p.shedFixture, p.repository, p.stream, service.TestDriftMasonAgent, resolvedDone("Combined shared helper"))
	awaitResolution(t, p.s, d, p.stream, ops[0].Operation, "review 1 of candidate")
	if tip, _ := p.forkBranch(t); tip != before {
		t.Fatal("conflict resolution pushed without approval")
	}
	completeDriftTurn(t, p.shedFixture, p.repository, p.stream, service.TestDriftReviewerAgent, driftVerdict(t, approvedResolution))
	result = settleOperation(t, p.s, p.repository, p.stream, ops[0].Operation, d)
	if result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	if tip, _ := p.forkBranch(t); tip != before {
		t.Fatal("the rebase pushed the pull request on its own")
	}
	pending, err := lifecycle.UpstreamDeliveryPending(p.repository, p.s.cfg, p.stream)
	intakefixture.Must(t, err)
	if !pending {
		t.Fatal("the resolved drift did not request a delivery review")
	}
	reviewer := &finalReviewer{s: p.s, repository: p.repository}
	intakefixture.Must(t, reviewer.Pass(ctx))
	op := fixturekit.ActionOperation(t, p.repository, p.stream, lifecycle.DeliveryReviewAction)
	in, err := delivery.DecodeFinalReview(op)
	intakefixture.Must(t, err)
	p.finalTurn(in.Review, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := intakefixture.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Ready after drift", "findings": []any{}})
		return err
	})
	result = settleOperation(t, p.s, p.repository, p.stream, op, reviewer)
	if result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	intakefixture.Must(t, p.publisher().Pass(ctx))
	if tip, _ := p.forkBranch(t); tip != before {
		t.Fatal("remote changed before the owner's approval")
	}
	presented, apiFault := p.s.deliveryPresentation(ctx, string(p.stream))
	if apiFault != nil || !presented.Maintenance || presented.Approval != nil {
		t.Fatalf("presentation %+v %v", presented, apiFault)
	}
	description := "Updated drift delivery\n\nApproved after rebase outside unit footprints."
	p.approve(t, &description)
	intakefixture.Must(t, p.publisher().Pass(ctx))
	publish := fixturekit.ActionOperation(t, p.repository, p.stream, lifecycle.PublishUpstreamAction)
	result = settleOperation(t, p.s, p.repository, p.stream, publish, p.publisher())
	if result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	tip, _ := p.forkBranch(t)
	if tip == before || p.pulls.Creates != 1 || p.pulls.Prs[0].Body != description || p.feature(t) != intake.DeliveredState {
		t.Fatalf("delivery tip %s creates %d state %s", tip, p.pulls.Creates, p.feature(t))
	}
}
func TestDeliveredDriftClosedPRStopsBeforeRebase(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"closed", "merged"} {
		t.Run(outcome, func(t *testing.T) {
			p := newPublicationFixture(t, "commit-per-unit")
			p.approve(t, nil)
			settleOperation(t, p.s, p.repository, p.stream, p.request(t), p.publisher())
			before := featureTip(t, p.shedFixture, p.stream)
			advanceUpstream(t, p.shedFixture, map[string]string{"CLOSED.md": "upstream moved\n"})
			p.pulls.Prs[0].State = "closed"
			p.pulls.Prs[0].Merged = outcome == "merged"
			fm := cadenceForeman(p.shedFixture, p.repository, "1m")
			_, apiFault := p.s.rebaseWorkstream(context.Background(), string(p.stream))
			if apiFault != nil {
				t.Fatal(apiFault)
			}
			passDrifts(t, fm)
			op := driftOperations(t, p.repository, p.stream)[0].Operation
			settleOperation(t, p.s, p.repository, p.stream, op, drifter{fm})
			finished, err := delivery.OwnPullRequestFinished(p.repository, p.stream)
			intakefixture.Must(t, err)
			if !finished {
				t.Fatal("closure not recorded")
			}
			if tip := featureTip(t, p.shedFixture, p.stream); tip != before {
				t.Fatal("closed PR was rebased")
			}
			if _, apiFault := p.s.rebaseWorkstream(context.Background(), string(p.stream)); apiFault == nil {
				t.Fatal("closed PR accepted another drift")
			}
		})
	}
}
