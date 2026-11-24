package executionjourney

import (
	"context"
	"errors"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func useUpstreamForPush(t *testing.T, p *publicationFixture) {
	t.Helper()
	p.fork = strings.TrimSpace(fixturekit.DemoGit(t, p.clone, "-C", p.clone, "remote", "get-url", "upstream"))
	p.s.cfg.Project.Fork = ""
	p.pulls.SyncHead = true
	p.s.options.PullRequests = p.pulls
}

func TestPublicationWithoutForkReconcilesLostResponse(t *testing.T) {
	t.Parallel()
	p := newUnpushedPublicationFixture(t, "commit-per-unit", "")
	useUpstreamForPush(t, p)
	ctx := context.Background()
	main := strings.TrimSpace(fixturekit.DemoGit(t, p.clone, "-C", p.fork, "rev-parse", "main"))
	p.pulls.Lose = true
	if _, err := p.tryPublishDraft(t); err == nil {
		t.Fatal("expected lost response")
	}
	p.reopen(t)
	p.publishDraft(t)
	if p.pulls.Creates != 1 {
		t.Fatalf("creates=%d", p.pulls.Creates)
	}
	p.approve(t, nil)
	op := p.request(t)
	in, err := delivery.DecodePublish(op)
	fixturekit.Must(t, err)
	if in.Fork != p.s.cfg.Project.Upstream || in.Upstream != in.Fork {
		t.Fatal(in)
	}
	result, err := p.publisher().Apply(ctx, op)
	fixturekit.Must(t, err)
	if result.Outcome != "succeeded" || p.pulls.Creates != 1 {
		t.Fatalf("%+v creates=%d", result, p.pulls.Creates)
	}
	if got := strings.TrimSpace(fixturekit.DemoGit(t, p.clone, "-C", p.fork, "rev-parse", "main")); got != main {
		t.Fatal("base branch changed")
	}
}

func sameRepositoryMaintenance(t *testing.T) *publicationFixture {
	t.Helper()
	p := maintenanceFixture(t)
	records, err := delivery.Publications(p.repository, p.stream)
	fixturekit.Must(t, err)
	prior := records[len(records)-1]
	useUpstreamForPush(t, p)
	parent := config.WorkstreamID("w_fffffffffffffffffffffffffffffff0")
	prior.Upstream, prior.Fork = p.s.cfg.Project.Upstream, p.s.cfg.Project.Upstream
	prior.Base, prior.BaseWorkstream = intake.FeatureBranch(parent), parent
	prior.URL = "https://github.com/" + prior.Upstream + "/pull/100"
	fixturekit.Must(t, p.publisher().recordPublication(context.Background(), p.stream, prior))
	fixturekit.DemoGit(t, filepath.Dir(p.clone), "-C", p.clone, "push", "upstream", prior.Commit+":refs/heads/"+prior.Branch)
	p.pulls.Prs[0].HeadRepository = prior.Fork
	p.pulls.Prs[0].Base = prior.Base
	p.pulls.Prs[0].URL = prior.URL
	return p
}

func approveMaintenance(t *testing.T, p *publicationFixture) {
	t.Helper()
	ctx := context.Background()
	reviewer := &finalReviewer{s: p.s, repository: p.repository}
	fixturekit.Must(t, reviewer.Pass(ctx))
	op := fixturekit.ActionOperation(t, p.repository, p.stream, deliveryReviewAction)
	in, err := delivery.DecodeFinalReview(op)
	fixturekit.Must(t, err)
	p.finalTurn(in.Review, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Ready upstream", "findings": []any{}})
		return err
	})
	result, err := reviewer.Apply(ctx, op)
	fixturekit.Must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	fixturekit.Must(t, p.publisher().Pass(ctx))
	ops, err := p.repository.Operations(p.stream)
	fixturekit.Must(t, err)
	for _, o := range ops {
		if o.Operation.Action == publishUpstreamAction {
			t.Fatal("published before renewed approval")
		}
	}
	p.approve(t, nil)
	fixturekit.Must(t, p.publisher().Pass(ctx))
}

func TestSameRepositoryMaintenanceRetargetRecovery(t *testing.T) {
	t.Parallel()
	for _, interruption := range []string{"publish-pushed", "publish-retargeting", "lost-response", "publish-retargeted"} {
		t.Run(interruption, func(t *testing.T) {
			p := sameRepositoryMaintenance(t)
			approveMaintenance(t, p)
			ctx := context.Background()
			op := fixturekit.ActionOperation(t, p.repository, p.stream, publishUpstreamAction)
			in, err := delivery.DecodePublish(op)
			fixturekit.Must(t, err)
			if in.Retarget == nil || in.Retarget.Number != 100 {
				t.Fatal(in)
			}
			if interruption == "lost-response" {
				p.pulls.Lose = true
			} else {
				p.s.boundary = func(step string) error {
					if step == interruption {
						return errors.New("interrupted")
					}
					return nil
				}
			}
			if _, err := p.publisher().Apply(ctx, op); err == nil {
				t.Fatal("expected interruption")
			}
			p.s.boundary = nil
			p.reopen(t)
			result, err := p.publisher().Apply(ctx, op)
			fixturekit.Must(t, err)
			if result.Outcome != "succeeded" || p.pulls.Creates != 1 || p.pulls.Updates != 1 || p.feature(t) != intake.DeliveredState {
				t.Fatalf("%+v creates=%d updates=%d", result, p.pulls.Creates, p.pulls.Updates)
			}
			records, err := delivery.Publications(p.repository, p.stream)
			fixturekit.Must(t, err)
			last := records[len(records)-1]
			if last.Base != "main" || last.PullRequest != 100 || last.URL != in.Retarget.URL || last.PriorURL != last.URL || last.BaseWorkstream != "" {
				t.Fatal(last)
			}
			pending, err := p.s.delivery().UpstreamDeliveryPending(p.repository, p.s.cfg, p.stream)
			fixturekit.Must(t, err)
			if pending {
				t.Fatal("completed retarget still pending")
			}
			_, err = p.publisher().Apply(ctx, op)
			fixturekit.Must(t, err)
			if p.pulls.Updates != 1 {
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
			op := fixturekit.ActionOperation(t, p.repository, p.stream, publishUpstreamAction)
			old, _ := p.forkBranch(t)
			p.pulls.Prs[0].HeadCommit, p.pulls.HeadLag = old, tc.lag
			pushed := ""
			for attempt := 1; attempt < tc.attempts; attempt++ {
				if result, err := p.publisher().Apply(ctx, op); err == nil {
					t.Fatalf("attempt %d settled while the pull request head was behind: %+v", attempt, result)
				}
				tip, _ := p.forkBranch(t)
				if tip == old || pushed != "" && tip != pushed {
					t.Fatalf("attempt %d left the branch at %s, old %s, first push %s", attempt, tip, old, pushed)
				}
				pushed = tip
			}
			result, err := p.publisher().Apply(ctx, op)
			fixturekit.Must(t, err)
			if tip, _ := p.forkBranch(t); result.Outcome != "succeeded" || tip != pushed || p.pulls.Prs[0].HeadCommit != pushed || p.pulls.Updates != 1 || p.feature(t) != intake.DeliveredState {
				t.Fatalf("%+v tip %s pushed %s pull request %+v updates %d", result, tip, pushed, p.pulls.Prs[0], p.pulls.Updates)
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
		old, _ := p.forkBranch(t)
		p.pulls.Prs[0].HeadCommit, p.pulls.HeadLag = old, 1
		p.s.boundary = func(step string) error {
			if step == "publish-pushed" {
				p.pulls.Prs[0].Body = "external edit"
			}
			return nil
		}
		result, err := p.publisher().Apply(context.Background(), fixturekit.ActionOperation(t, p.repository, p.stream, publishUpstreamAction))
		fixturekit.Must(t, err)
		if result.Outcome != "failed" {
			t.Fatalf("edited pull request was not refused: %+v", result)
		}
	})
	t.Run("at a commit no publication recorded", func(t *testing.T) {
		t.Parallel()
		p := sameRepositoryMaintenance(t)
		approveMaintenance(t, p)
		before, _ := p.forkBranch(t)
		p.pulls.Prs[0].HeadCommit, p.pulls.HeadLag = p.report.Upstream.Commit, 1
		result, err := p.publisher().Apply(context.Background(), fixturekit.ActionOperation(t, p.repository, p.stream, publishUpstreamAction))
		fixturekit.Must(t, err)
		if after, _ := p.forkBranch(t); result.Outcome != "failed" || after != before {
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
			before, _ := p.forkBranch(t)
			switch change {
			case "body":
				p.pulls.Prs[0].Body = "external edit"
			case "base":
				p.pulls.Prs[0].Base = "another-branch"
			case "closed":
				p.pulls.Prs[0].State = "closed"
			case "merged":
				p.pulls.Prs[0].State, p.pulls.Prs[0].Merged = "closed", true
			case "missing":
				p.pulls.Prs = []pulls.PullRequest{}
			}
			result, err := p.publisher().Apply(context.Background(), fixturekit.ActionOperation(t, p.repository, p.stream, publishUpstreamAction))
			fixturekit.Must(t, err)
			after, _ := p.forkBranch(t)
			if result.Outcome == "succeeded" || before != after || p.pulls.Updates != 0 {
				t.Fatalf("unsafe publication: %+v", result)
			}

			outcome, revision, err := delivery.OwnPullRequestOutcomeAt(p.repository, p.stream)
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
