package executionjourney

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestPublicationWithoutForkReconcilesLostResponse(t *testing.T) {
	t.Parallel()
	p := journey.NewUnpushedPublicationFixture(t, "commit-per-unit", "")
	journey.UseUpstreamForPush(t, p)
	ctx := context.Background()
	main := strings.TrimSpace(fixturekit.DemoGit(t, p.Clone, "-C", p.Fork, "rev-parse", "main"))
	p.Pulls.Lose = true
	if _, err := p.TryPublishDraft(t); err == nil {
		t.Fatal("expected lost response")
	}
	p.Reopen(t)
	p.PublishDraft(t)
	if p.Pulls.Creates != 1 {
		t.Fatalf("creates=%d", p.Pulls.Creates)
	}
	p.Approve(t, nil)
	op := p.Request(t)
	in, err := delivery.DecodePublish(op)
	fixturekit.Must(t, err)
	if in.Fork != p.S.Cfg.Project.Upstream || in.Upstream != in.Fork {
		t.Fatal(in)
	}
	result, err := p.Publisher().Apply(ctx, op)
	fixturekit.Must(t, err)
	if result.Outcome != "succeeded" || p.Pulls.Creates != 1 {
		t.Fatalf("%+v creates=%d", result, p.Pulls.Creates)
	}
	if got := strings.TrimSpace(fixturekit.DemoGit(t, p.Clone, "-C", p.Fork, "rev-parse", "main")); got != main {
		t.Fatal("base branch changed")
	}
}

func sameRepositoryMaintenance(t *testing.T) *journey.PublicationFixture {
	t.Helper()
	p := journey.MaintenanceFixture(t)
	records, err := delivery.Publications(p.Repository, p.Stream)
	fixturekit.Must(t, err)
	prior := records[len(records)-1]
	journey.UseUpstreamForPush(t, p)
	parent := config.WorkstreamID("w_fffffffffffffffffffffffffffffff0")
	prior.Upstream, prior.Fork = p.S.Cfg.Project.Upstream, p.S.Cfg.Project.Upstream
	prior.Base, prior.BaseWorkstream = intake.FeatureBranch(parent), parent
	prior.URL = "https://github.com/" + prior.Upstream + "/pull/100"
	fixturekit.Must(t, p.Publisher().RecordPublication(context.Background(), p.Stream, prior))
	fixturekit.DemoGit(t, filepath.Dir(p.Clone), "-C", p.Clone, "push", "upstream", prior.Commit+":refs/heads/"+prior.Branch)
	p.Pulls.Prs[0].HeadRepository = prior.Fork
	p.Pulls.Prs[0].Base = prior.Base
	p.Pulls.Prs[0].URL = prior.URL
	return p
}

func approveMaintenance(t *testing.T, p *journey.PublicationFixture) {
	t.Helper()
	ctx := context.Background()
	reviewer := &journey.FinalReviewer{S: p.S, Repository: p.Repository}
	fixturekit.Must(t, reviewer.Pass(ctx))
	op := fixturekit.ActionOperation(t, p.Repository, p.Stream, lifecycle.DeliveryReviewAction)
	in, err := delivery.DecodeFinalReview(op)
	fixturekit.Must(t, err)
	p.FinalTurn(in.Review, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Ready upstream", "findings": []any{}})
		return err
	})
	result, err := reviewer.Apply(ctx, op)
	fixturekit.Must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	fixturekit.Must(t, p.Publisher().Pass(ctx))
	ops, err := p.Repository.Operations(p.Stream)
	fixturekit.Must(t, err)
	for _, o := range ops {
		if o.Operation.Action == lifecycle.PublishUpstreamAction {
			t.Fatal("published before renewed approval")
		}
	}
	p.Approve(t, nil)
	fixturekit.Must(t, p.Publisher().Pass(ctx))
}

func TestSameRepositoryMaintenanceRetargetRecovery(t *testing.T) {
	t.Parallel()
	for _, interruption := range []string{"publish-pushed", "publish-retargeting", "lost-response", "publish-retargeted"} {
		t.Run(interruption, func(t *testing.T) {
			p := sameRepositoryMaintenance(t)
			approveMaintenance(t, p)
			ctx := context.Background()
			op := fixturekit.ActionOperation(t, p.Repository, p.Stream, lifecycle.PublishUpstreamAction)
			in, err := delivery.DecodePublish(op)
			fixturekit.Must(t, err)
			if in.Retarget == nil || in.Retarget.Number != 100 {
				t.Fatal(in)
			}
			if interruption == "lost-response" {
				p.Pulls.Lose = true
			} else {
				p.S.Boundary = func(step string) error {
					if step == interruption {
						return errors.New("interrupted")
					}
					return nil
				}
			}
			if _, err := p.Publisher().Apply(ctx, op); err == nil {
				t.Fatal("expected interruption")
			}
			p.S.Boundary = nil
			p.Reopen(t)
			result, err := p.Publisher().Apply(ctx, op)
			fixturekit.Must(t, err)
			if result.Outcome != "succeeded" || p.Pulls.Creates != 1 || p.Pulls.Updates != 1 || p.Feature(t) != intake.DeliveredState {
				t.Fatalf("%+v creates=%d updates=%d", result, p.Pulls.Creates, p.Pulls.Updates)
			}
			records, err := delivery.Publications(p.Repository, p.Stream)
			fixturekit.Must(t, err)
			last := records[len(records)-1]
			if last.Base != "main" || last.PullRequest != 100 || last.URL != in.Retarget.URL || last.PriorURL != last.URL || last.BaseWorkstream != "" {
				t.Fatal(last)
			}
			pending, err := p.S.Delivery().UpstreamDeliveryPending(p.Repository, p.S.Cfg, p.Stream)
			fixturekit.Must(t, err)
			if pending {
				t.Fatal("completed retarget still pending")
			}
			_, err = p.Publisher().Apply(ctx, op)
			fixturekit.Must(t, err)
			if p.Pulls.Updates != 1 {
				t.Fatal("retry repeated update")
			}
		})
	}
}

func TestMaintenancePublicationWaitsForThePullRequestHeadAfterItsPush(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name          string
		lag, attempts int
	}{
		{name: "after the push", lag: 1, attempts: 2},
		{name: "before the retry's check", lag: 2, attempts: 2},
		{name: "across a whole retry", lag: 3, attempts: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := sameRepositoryMaintenance(t)
			approveMaintenance(t, p)
			ctx := context.Background()
			op := fixturekit.ActionOperation(t, p.Repository, p.Stream, lifecycle.PublishUpstreamAction)
			old, _ := p.ForkBranch(t)
			p.Pulls.Prs[0].HeadCommit, p.Pulls.HeadLag = old, tc.lag
			pushed := ""
			for attempt := 1; attempt < tc.attempts; attempt++ {
				if result, err := p.Publisher().Apply(ctx, op); err == nil {
					t.Fatalf("attempt %d settled while the pull request head was behind: %+v", attempt, result)
				}
				tip, _ := p.ForkBranch(t)
				if tip == old || pushed != "" && tip != pushed {
					t.Fatalf("attempt %d left the branch at %s, old %s, first push %s", attempt, tip, old, pushed)
				}
				pushed = tip
			}
			result, err := p.Publisher().Apply(ctx, op)
			fixturekit.Must(t, err)
			if tip, _ := p.ForkBranch(t); result.Outcome != "succeeded" || tip != pushed || p.Pulls.Prs[0].HeadCommit != pushed || p.Pulls.Updates != 1 || p.Feature(t) != intake.DeliveredState {
				t.Fatalf("%+v tip %s pushed %s pull request %+v updates %d", result, tip, pushed, p.Pulls.Prs[0], p.Pulls.Updates)
			}
		})
	}
}

func TestMaintenancePublicationRefusesABehindPullRequestThatDiffersOtherwise(t *testing.T) {
	t.Parallel()
	t.Run("edited after the push", func(t *testing.T) {
		t.Parallel()
		p := sameRepositoryMaintenance(t)
		approveMaintenance(t, p)
		old, _ := p.ForkBranch(t)
		p.Pulls.Prs[0].HeadCommit, p.Pulls.HeadLag = old, 1
		p.S.Boundary = func(step string) error {
			if step == "publish-pushed" {
				p.Pulls.Prs[0].Body = "external edit"
			}
			return nil
		}
		result, err := p.Publisher().Apply(context.Background(), fixturekit.ActionOperation(t, p.Repository, p.Stream, lifecycle.PublishUpstreamAction))
		fixturekit.Must(t, err)
		if result.Outcome != "failed" {
			t.Fatalf("edited pull request was not refused: %+v", result)
		}
	})
	t.Run("at a commit no publication recorded", func(t *testing.T) {
		t.Parallel()
		p := sameRepositoryMaintenance(t)
		approveMaintenance(t, p)
		before, _ := p.ForkBranch(t)
		p.Pulls.Prs[0].HeadCommit, p.Pulls.HeadLag = p.Report.Upstream.Commit, 1
		result, err := p.Publisher().Apply(context.Background(), fixturekit.ActionOperation(t, p.Repository, p.Stream, lifecycle.PublishUpstreamAction))
		fixturekit.Must(t, err)
		if after, _ := p.ForkBranch(t); result.Outcome != "failed" || after != before {
			t.Fatalf("unrecorded pull request head was not refused: %+v, branch %s to %s", result, before, after)
		}
	})
}

func TestSameRepositoryMaintenanceRefusesExternalPREdits(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"body", "base", "closed", "merged", "missing"} {
		t.Run(change, func(t *testing.T) {
			p := sameRepositoryMaintenance(t)
			approveMaintenance(t, p)
			before, _ := p.ForkBranch(t)
			switch change {
			case "body":
				p.Pulls.Prs[0].Body = "external edit"
			case "base":
				p.Pulls.Prs[0].Base = "another-branch"
			case "closed":
				p.Pulls.Prs[0].State = "closed"
			case "merged":
				p.Pulls.Prs[0].State, p.Pulls.Prs[0].Merged = "closed", true
			case "missing":
				p.Pulls.Prs = []pulls.PullRequest{}
			}
			result, err := p.Publisher().Apply(context.Background(), fixturekit.ActionOperation(t, p.Repository, p.Stream, lifecycle.PublishUpstreamAction))
			fixturekit.Must(t, err)
			after, _ := p.ForkBranch(t)
			if result.Outcome == "succeeded" || before != after || p.Pulls.Updates != 0 {
				t.Fatalf("unsafe publication: %+v", result)
			}

			outcome, revision, err := delivery.OwnPullRequestOutcomeAt(p.Repository, p.Stream)
			fixturekit.Must(t, err)
			switch change {
			case "closed", "merged":
				want := delivery.OwnPullRequestOutcomeClosed
				if change == "merged" {
					want = delivery.OwnPullRequestOutcomeMerged
				}
				if revision != 1 || outcome.PullRequest != 100 || outcome.Outcome != want {
					t.Fatalf("own-PR outcome %+v revision %d, want #100 %s", outcome, revision, want)
				}
			default:
				if revision != 0 {
					t.Fatalf("own-PR outcome recorded without a merged or closed pull request: %+v revision %d", outcome, revision)
				}
			}
		})
	}
}
