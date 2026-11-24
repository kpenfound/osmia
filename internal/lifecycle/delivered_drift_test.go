package lifecycle_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
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
			p := journey.NewUnpushedPublicationFixture(t, tc.style, "")
			if tc.noFork {
				journey.UseUpstreamForPush(t, p)
			}
			p.PublishDraft(t)
			ctx := context.Background()
			p.Approve(t, nil)
			result := journey.SettleOperation(t, p.S, p.Repository, p.Stream, p.Request(t), p.Publisher())
			if result.Outcome != "succeeded" {
				t.Fatal(result)
			}
			p.Pulls.SyncHead = true
			before, _ := p.ForkBranch(t)
			fm := cadenceForeman(p.ShedFixture, p.Repository, "1m")
			files := map[string]string{"DRIFT.md": "new upstream content\n"}
			if tc.conflict {
				files["resume.go"] = "package demo\n// Upstream resume.\n"
			}
			journey.AdvanceUpstream(t, p.ShedFixture, files)
			calls := p.Pulls.Finds
			if why, err := fm.DriftDriver().DriftDue(p.Stream, p.S.Now().Add(24*time.Hour)); err != nil || why != "" {
				t.Fatalf("automatic drift: %q %v", why, err)
			}
			passDrifts(t, fm)
			if p.Pulls.Finds != calls || len(journey.DriftOperations(t, p.Repository, p.Stream)) != 0 {
				t.Fatal("delivered branch was polled")
			}
			if tc.projectRequest {
				out, apiFault := p.S.Lifecycle().RebaseProject(ctx, api.ProjectRebaseRequest{Project: p.Project})
				if apiFault != nil || len(out.Covered) != 1 {
					t.Fatalf("project rebase: %+v %v", out, apiFault)
				}
			} else if _, apiFault := p.S.Lifecycle().RebaseWorkstream(ctx, string(p.Stream)); apiFault != nil {
				t.Fatal(apiFault)
			}
			passDrifts(t, fm)
			ops := journey.DriftOperations(t, p.Repository, p.Stream)
			if len(ops) != 1 {
				t.Fatalf("drifts: %v", ops)
			}
			if tc.conflict {
				d := journey.Drifter{Foreman: fm}
				journey.AwaitResolution(t, p.S, d, p.Stream, ops[0].Operation, "its mason's resolution of resume.go")
				w := journey.ResolutionWorkspace(t, p.ShedFixture, p.Stream)
				fixturekit.Must(t, os.WriteFile(filepath.Join(w.Path, "resume.go"), []byte("package demo\n// Upstream resume and feature resume.\n"), 0600))
				journey.CompleteDriftTurn(t, p.ShedFixture, p.Repository, p.Stream, intake.DriftMasonAgent, journey.ResolvedDone("Combined resume behavior"))
				journey.AwaitResolution(t, p.S, d, p.Stream, ops[0].Operation, "review 1 of candidate")
				if tip, _ := p.ForkBranch(t); tip != before {
					t.Fatal("conflict resolution pushed without approval")
				}
				journey.CompleteDriftTurn(t, p.ShedFixture, p.Repository, p.Stream, intake.DriftReviewerAgent, journey.DriftVerdict(t, journey.ApprovedResolution))
			}
			result = journey.SettleOperation(t, p.S, p.Repository, p.Stream, ops[0].Operation, journey.Drifter{Foreman: fm})
			if result.Outcome != "succeeded" {
				t.Fatal(result)
			}
			p.Reopen(t)
			pending, err := lifecycle.UpstreamDeliveryPending(p.Repository, p.S.Cfg, p.Stream)
			fixturekit.Must(t, err)
			if !pending {
				t.Fatal("maintenance lost after restart")
			}
			reviewer := &journey.FinalReviewer{S: p.S, Repository: p.Repository}
			fixturekit.Must(t, reviewer.Pass(ctx))
			op := fixturekit.ActionOperation(t, p.Repository, p.Stream, lifecycle.DeliveryReviewAction)
			in, err := delivery.DecodeFinalReview(op)
			fixturekit.Must(t, err)
			p.FinalTurn(in.Review, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
				_, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Ready after drift", "findings": []any{}})
				return err
			})
			result = journey.SettleOperation(t, p.S, p.Repository, p.Stream, op, reviewer)
			if result.Outcome != "succeeded" {
				t.Fatal(result)
			}
			fixturekit.Must(t, p.Publisher().Pass(ctx))
			all, err := p.Repository.Operations(p.Stream)
			fixturekit.Must(t, err)
			for _, o := range all {
				if o.Operation.Action == lifecycle.PublishUpstreamAction {
					t.Fatal("published without new approval")
				}
			}
			if tip, _ := p.ForkBranch(t); tip != before {
				t.Fatal("remote changed before approval")
			}
			presented, apiFault := p.S.DeliveryPresentation(ctx, string(p.Stream))
			if apiFault != nil || !presented.Maintenance || presented.Approval != nil {
				t.Fatalf("presentation %+v %v", presented, apiFault)
			}
			description := "Updated drift delivery\n\nApproved after rebase."
			p.Approve(t, &description)
			fixturekit.Must(t, p.Publisher().Pass(ctx))
			publish := fixturekit.ActionOperation(t, p.Repository, p.Stream, lifecycle.PublishUpstreamAction)
			p.Pulls.Lose = true
			if _, err := p.Publisher().Apply(ctx, publish); err == nil {
				t.Fatal("expected lost PR update response")
			}
			p.Reopen(t)
			result = journey.SettleOperation(t, p.S, p.Repository, p.Stream, publish, p.Publisher())
			if result.Outcome != "succeeded" {
				t.Fatal(result)
			}
			tip, _ := p.ForkBranch(t)
			if tip == before || p.Pulls.Creates != 1 || p.Pulls.Prs[0].Body != description || p.Feature(t) != intake.DeliveredState {
				t.Fatalf("delivery tip %s creates %d state %s", tip, p.Pulls.Creates, p.Feature(t))
			}
			pending, err = lifecycle.UpstreamDeliveryPending(p.Repository, p.S.Cfg, p.Stream)
			fixturekit.Must(t, err)
			if pending {
				t.Fatal("publication did not finish maintenance")
			}
			fm = cadenceForeman(p.ShedFixture, p.Repository, "1m")
			if _, apiFault := p.S.Lifecycle().RebaseWorkstream(ctx, string(p.Stream)); apiFault != nil {
				t.Fatal(apiFault)
			}
			passDrifts(t, fm)
			drifts := journey.DriftOperations(t, p.Repository, p.Stream)
			journey.SettleOperation(t, p.S, p.Repository, p.Stream, drifts[len(drifts)-1].Operation, journey.Drifter{Foreman: fm})
			pending, err = lifecycle.UpstreamDeliveryPending(p.Repository, p.S.Cfg, p.Stream)
			fixturekit.Must(t, err)
			if pending {
				t.Fatal("unchanged base requested another delivery")
			}

			result, err = p.Publisher().Apply(ctx, publish)
			fixturekit.Must(t, err)
			if result.Outcome != "succeeded" || p.Pulls.Creates != 1 {
				t.Fatal("retry duplicated publication")
			}
		})
	}
}

func TestDeliveredDriftConflictOutsideUnitFootprintCompletesEndToEnd(t *testing.T) {
	t.Parallel()
	p := journey.NewPublicationFixture(t, "squash")
	ctx := context.Background()
	p.Approve(t, nil)
	result := journey.SettleOperation(t, p.S, p.Repository, p.Stream, p.Request(t), p.Publisher())
	if result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	p.Pulls.SyncHead = true
	before, _ := p.ForkBranch(t)
	fm := cadenceForeman(p.ShedFixture, p.Repository, "1m")
	journey.MoveFeature(t, p.ShedFixture, p.Stream, map[string]string{"shared.go": "package demo\n// Feature shared helper.\n"})
	journey.AdvanceUpstream(t, p.ShedFixture, map[string]string{"DRIFT.md": "new upstream content\n", "shared.go": "package demo\n// Upstream shared helper.\n"})
	if _, apiFault := p.S.Lifecycle().RebaseWorkstream(ctx, string(p.Stream)); apiFault != nil {
		t.Fatal(apiFault)
	}
	passDrifts(t, fm)
	ops := journey.DriftOperations(t, p.Repository, p.Stream)
	if len(ops) != 1 {
		t.Fatalf("drifts: %v", ops)
	}
	d := journey.Drifter{Foreman: fm}
	journey.AwaitResolution(t, p.S, d, p.Stream, ops[0].Operation, "its mason's resolution of shared.go")
	w := journey.ResolutionWorkspace(t, p.ShedFixture, p.Stream)
	fixturekit.Must(t, os.WriteFile(filepath.Join(w.Path, "shared.go"), []byte("package demo\n// Upstream and feature shared helper.\n"), 0600))
	journey.CompleteDriftTurn(t, p.ShedFixture, p.Repository, p.Stream, intake.DriftMasonAgent, journey.ResolvedDone("Combined shared helper"))
	journey.AwaitResolution(t, p.S, d, p.Stream, ops[0].Operation, "review 1 of candidate")
	if tip, _ := p.ForkBranch(t); tip != before {
		t.Fatal("conflict resolution pushed without approval")
	}
	journey.CompleteDriftTurn(t, p.ShedFixture, p.Repository, p.Stream, intake.DriftReviewerAgent, journey.DriftVerdict(t, journey.ApprovedResolution))
	result = journey.SettleOperation(t, p.S, p.Repository, p.Stream, ops[0].Operation, d)
	if result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	if tip, _ := p.ForkBranch(t); tip != before {
		t.Fatal("the rebase pushed the pull request on its own")
	}
	pending, err := lifecycle.UpstreamDeliveryPending(p.Repository, p.S.Cfg, p.Stream)
	fixturekit.Must(t, err)
	if !pending {
		t.Fatal("the resolved drift did not request a delivery review")
	}
	reviewer := &journey.FinalReviewer{S: p.S, Repository: p.Repository}
	fixturekit.Must(t, reviewer.Pass(ctx))
	op := fixturekit.ActionOperation(t, p.Repository, p.Stream, lifecycle.DeliveryReviewAction)
	in, err := delivery.DecodeFinalReview(op)
	fixturekit.Must(t, err)
	p.FinalTurn(in.Review, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Ready after drift", "findings": []any{}})
		return err
	})
	result = journey.SettleOperation(t, p.S, p.Repository, p.Stream, op, reviewer)
	if result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	fixturekit.Must(t, p.Publisher().Pass(ctx))
	if tip, _ := p.ForkBranch(t); tip != before {
		t.Fatal("remote changed before the owner's approval")
	}
	presented, apiFault := p.S.DeliveryPresentation(ctx, string(p.Stream))
	if apiFault != nil || !presented.Maintenance || presented.Approval != nil {
		t.Fatalf("presentation %+v %v", presented, apiFault)
	}
	description := "Updated drift delivery\n\nApproved after rebase outside unit footprints."
	p.Approve(t, &description)
	fixturekit.Must(t, p.Publisher().Pass(ctx))
	publish := fixturekit.ActionOperation(t, p.Repository, p.Stream, lifecycle.PublishUpstreamAction)
	result = journey.SettleOperation(t, p.S, p.Repository, p.Stream, publish, p.Publisher())
	if result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	tip, _ := p.ForkBranch(t)
	if tip == before || p.Pulls.Creates != 1 || p.Pulls.Prs[0].Body != description || p.Feature(t) != intake.DeliveredState {
		t.Fatalf("delivery tip %s creates %d state %s", tip, p.Pulls.Creates, p.Feature(t))
	}
}
func TestDeliveredDriftClosedPRStopsBeforeRebase(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"closed", "merged"} {
		t.Run(outcome, func(t *testing.T) {
			p := journey.NewPublicationFixture(t, "commit-per-unit")
			p.Approve(t, nil)
			journey.SettleOperation(t, p.S, p.Repository, p.Stream, p.Request(t), p.Publisher())
			before := journey.FeatureTip(t, p.ShedFixture, p.Stream)
			journey.AdvanceUpstream(t, p.ShedFixture, map[string]string{"CLOSED.md": "upstream moved\n"})
			p.Pulls.Prs[0].State = "closed"
			p.Pulls.Prs[0].Merged = outcome == "merged"
			fm := cadenceForeman(p.ShedFixture, p.Repository, "1m")
			_, apiFault := p.S.Lifecycle().RebaseWorkstream(context.Background(), string(p.Stream))
			if apiFault != nil {
				t.Fatal(apiFault)
			}
			passDrifts(t, fm)
			op := journey.DriftOperations(t, p.Repository, p.Stream)[0].Operation
			journey.SettleOperation(t, p.S, p.Repository, p.Stream, op, journey.Drifter{Foreman: fm})
			finished, err := delivery.OwnPullRequestFinished(p.Repository, p.Stream)
			fixturekit.Must(t, err)
			if !finished {
				t.Fatal("closure not recorded")
			}
			if tip := journey.FeatureTip(t, p.ShedFixture, p.Stream); tip != before {
				t.Fatal("closed PR was rebased")
			}
			if _, apiFault := p.S.Lifecycle().RebaseWorkstream(context.Background(), string(p.Stream)); apiFault == nil {
				t.Fatal("closed PR accepted another drift")
			}
		})
	}
}
