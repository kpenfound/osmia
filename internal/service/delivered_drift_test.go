package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDeliveredDriftRequiresApprovalAndUpdatesExistingPR(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, style      string
		noFork, conflict bool
	}{
		{name: "commits", style: "commit-per-unit"},
		{name: "squash", style: "squash"},
		{name: "upstream", style: "commit-per-unit", noFork: true},
		{name: "conflict", style: "squash", conflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPublicationFixture(t, tc.style)
			if tc.noFork {
				useUpstreamForPush(t, p)
			}
			ctx := context.Background()
			p.approve(t, nil)
			result := settleOperation(t, p.s, p.repository, p.stream, p.request(t), p.publisher())
			if result.Outcome != "succeeded" {
				t.Fatal(result)
			}
			p.pulls.syncHead = true
			before, _ := p.forkBranch(t)
			fm := cadenceForeman(p.shedFixture, p.repository, "1m")
			files := map[string]string{"DRIFT.md": "new upstream content\n"}
			if tc.conflict {
				files["resume.go"] = "package demo\n// Upstream resume.\n"
			}
			advanceUpstream(t, p.shedFixture, files)
			calls := p.pulls.finds
			if why, err := fm.driftDue(p.stream, p.s.now().Add(24*time.Hour)); err != nil || why != "" {
				t.Fatalf("automatic drift: %q %v", why, err)
			}
			passDrifts(t, fm)
			if p.pulls.finds != calls || len(driftOperations(t, p.repository, p.stream)) != 0 {
				t.Fatal("delivered branch was polled")
			}
			if _, api := p.s.rebaseWorkstream(ctx, string(p.stream)); api != nil {
				t.Fatal(api)
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
				must(t, os.WriteFile(filepath.Join(w.Path, "resume.go"), []byte("package demo\n// Upstream resume and feature resume.\n"), 0600))
				completeDriftTurn(t, p.shedFixture, p.repository, p.stream, driftMasonAgent, resolvedDone("Combined resume behavior"))
				awaitResolution(t, p.s, d, p.stream, ops[0].Operation, "review 1 of candidate")
				if tip, _ := p.forkBranch(t); tip != before {
					t.Fatal("conflict resolution pushed without approval")
				}
				completeDriftTurn(t, p.shedFixture, p.repository, p.stream, driftReviewerAgent, driftVerdict(t, approvedResolution))
			}
			result = settleOperation(t, p.s, p.repository, p.stream, ops[0].Operation, drifter{fm})
			if result.Outcome != "succeeded" {
				t.Fatal(result)
			}
			p.reopen(t)
			pending, err := upstreamDeliveryPending(p.repository, p.s.cfg, p.stream)
			must(t, err)
			if !pending {
				t.Fatal("maintenance lost after restart")
			}
			reviewer := &finalReviewer{s: p.s, repository: p.repository}
			must(t, reviewer.Pass(ctx))
			op := actionOperation(t, p.repository, p.stream, deliveryReviewAction)
			in, err := decodeFinalReview(op)
			must(t, err)
			p.finalTurn(in.Review, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
				_, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Ready after drift", "criteria": []any{map[string]any{"criterion": "spec#1", "evidence": "resume.go"}, map[string]any{"criterion": "spec#2", "evidence": "dedupe.go"}}})
				return err
			})
			result = settleOperation(t, p.s, p.repository, p.stream, op, reviewer)
			if result.Outcome != "succeeded" {
				t.Fatal(result)
			}
			must(t, p.publisher().Pass(ctx))
			all, err := p.repository.Operations(p.stream)
			must(t, err)
			for _, o := range all {
				if o.Operation.Action == publishUpstreamAction {
					t.Fatal("published without new approval")
				}
			}
			if tip, _ := p.forkBranch(t); tip != before {
				t.Fatal("remote changed before approval")
			}
			presented, api := p.s.deliveryPresentation(ctx, string(p.stream))
			if api != nil || !presented.Maintenance || presented.Approval != nil {
				t.Fatalf("presentation %+v %v", presented, api)
			}
			description := "Updated drift delivery\n\nApproved after rebase."
			p.approve(t, &description)
			must(t, p.publisher().Pass(ctx))
			publish := actionOperation(t, p.repository, p.stream, publishUpstreamAction)
			p.pulls.lose = true
			if _, err := p.publisher().Apply(ctx, publish); err == nil {
				t.Fatal("expected lost PR update response")
			}
			p.reopen(t)
			result = settleOperation(t, p.s, p.repository, p.stream, publish, p.publisher())
			if result.Outcome != "succeeded" {
				t.Fatal(result)
			}
			tip, _ := p.forkBranch(t)
			if tip == before || p.pulls.creates != 1 || p.pulls.prs[0].Body != description || p.feature(t) != DeliveredState {
				t.Fatalf("delivery tip %s creates %d state %s", tip, p.pulls.creates, p.feature(t))
			}
			pending, err = upstreamDeliveryPending(p.repository, p.s.cfg, p.stream)
			must(t, err)
			if pending {
				t.Fatal("publication did not finish maintenance")
			}
			fm = cadenceForeman(p.shedFixture, p.repository, "1m")
			if _, api := p.s.rebaseWorkstream(ctx, string(p.stream)); api != nil {
				t.Fatal(api)
			}
			passDrifts(t, fm)
			drifts := driftOperations(t, p.repository, p.stream)
			settleOperation(t, p.s, p.repository, p.stream, drifts[len(drifts)-1].Operation, drifter{fm})
			pending, err = upstreamDeliveryPending(p.repository, p.s.cfg, p.stream)
			must(t, err)
			if pending {
				t.Fatal("unchanged base requested another delivery")
			}

			result, err = p.publisher().Apply(ctx, publish)
			must(t, err)
			if result.Outcome != "succeeded" || p.pulls.creates != 1 {
				t.Fatal("retry duplicated publication")
			}
		})
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
			p.pulls.prs[0].State = "closed"
			p.pulls.prs[0].Merged = outcome == "merged"
			fm := cadenceForeman(p.shedFixture, p.repository, "1m")
			_, api := p.s.rebaseWorkstream(context.Background(), string(p.stream))
			if api != nil {
				t.Fatal(api)
			}
			passDrifts(t, fm)
			op := driftOperations(t, p.repository, p.stream)[0].Operation
			settleOperation(t, p.s, p.repository, p.stream, op, drifter{fm})
			finished, err := ownPullRequestFinished(p.repository, p.stream)
			must(t, err)
			if !finished {
				t.Fatal("closure not recorded")
			}
			if tip := featureTip(t, p.shedFixture, p.stream); tip != before {
				t.Fatal("closed PR was rebased")
			}
			if _, api := p.s.rebaseWorkstream(context.Background(), string(p.stream)); api == nil {
				t.Fatal("closed PR accepted another drift")
			}
		})
	}
}
