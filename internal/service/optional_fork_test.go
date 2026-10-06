package service

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func useUpstreamForPush(t *testing.T, p *publicationFixture) {
	t.Helper()
	p.fork = strings.TrimSpace(demoGit(t, p.clone, "-C", p.clone, "remote", "get-url", "upstream"))
	p.s.cfg.Project.Fork = ""
	p.pulls.syncHead = true
	p.s.options.PullRequests = p.pulls
}

func TestPublicationWithoutForkReconcilesLostResponse(t *testing.T) {
	t.Parallel()
	p := newPublicationFixture(t, "commit-per-unit")
	useUpstreamForPush(t, p)
	ctx := context.Background()
	main := strings.TrimSpace(demoGit(t, p.clone, "-C", p.fork, "rev-parse", "main"))
	p.approve(t, nil)
	op := p.request(t)
	in, err := decodePublish(op)
	must(t, err)
	if in.Fork != p.s.cfg.Project.Upstream || in.Upstream != in.Fork {
		t.Fatal(in)
	}
	p.pulls.lose = true
	if _, err := p.publisher().Apply(ctx, op); err == nil {
		t.Fatal("expected lost response")
	}
	p.reopen(t)
	result, err := p.publisher().Apply(ctx, op)
	must(t, err)
	if result.Outcome != "succeeded" || p.pulls.creates != 1 {
		t.Fatalf("%+v creates=%d", result, p.pulls.creates)
	}
	if got := strings.TrimSpace(demoGit(t, p.clone, "-C", p.fork, "rev-parse", "main")); got != main {
		t.Fatal("base branch changed")
	}
}

func sameRepositoryMaintenance(t *testing.T) *publicationFixture {
	t.Helper()
	p := maintenanceFixture(t)
	records, err := publications(p.repository, p.stream)
	must(t, err)
	prior := records[len(records)-1]
	useUpstreamForPush(t, p)
	parent := config.WorkstreamID("w_fffffffffffffffffffffffffffffff0")
	prior.Upstream, prior.Fork = p.s.cfg.Project.Upstream, p.s.cfg.Project.Upstream
	prior.Base, prior.BaseWorkstream = featureBranch(parent), parent
	prior.URL = "https://github.com/" + prior.Upstream + "/pull/100"
	must(t, p.publisher().recordPublication(context.Background(), p.stream, prior))
	demoGit(t, filepath.Dir(p.clone), "-C", p.clone, "push", "upstream", prior.Commit+":refs/heads/"+prior.Branch)
	p.pulls.prs[0].HeadRepository = prior.Fork
	p.pulls.prs[0].Base = prior.Base
	p.pulls.prs[0].URL = prior.URL
	return p
}

func approveMaintenance(t *testing.T, p *publicationFixture) {
	t.Helper()
	ctx := context.Background()
	reviewer := &finalReviewer{s: p.s, repository: p.repository}
	must(t, reviewer.Pass(ctx))
	op := actionOperation(t, p.repository, p.stream, deliveryReviewAction)
	in, err := decodeFinalReview(op)
	must(t, err)
	p.finalTurn(in.Review, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Ready upstream", "criteria": []any{map[string]any{"criterion": "spec#1", "evidence": "resume.go"}, map[string]any{"criterion": "spec#2", "evidence": "dedupe.go"}}})
		return err
	})
	result, err := reviewer.Apply(ctx, op)
	must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	must(t, p.publisher().Pass(ctx))
	ops, err := p.repository.Operations(p.stream)
	must(t, err)
	for _, o := range ops {
		if o.Operation.Action == publishUpstreamAction {
			t.Fatal("published before renewed approval")
		}
	}
	p.approve(t, nil)
	must(t, p.publisher().Pass(ctx))
}

func TestSameRepositoryMaintenanceRetargetRecovery(t *testing.T) {
	t.Parallel()
	for _, interruption := range []string{"publish-pushed", "publish-retargeting", "lost-response", "publish-retargeted"} {
		t.Run(interruption, func(t *testing.T) {
			p := sameRepositoryMaintenance(t)
			approveMaintenance(t, p)
			ctx := context.Background()
			op := actionOperation(t, p.repository, p.stream, publishUpstreamAction)
			in, err := decodePublish(op)
			must(t, err)
			if in.Retarget == nil || in.Retarget.Number != 100 {
				t.Fatal(in)
			}
			if interruption == "lost-response" {
				p.pulls.lose = true
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
			must(t, err)
			if result.Outcome != "succeeded" || p.pulls.creates != 1 || p.pulls.updates != 1 || p.feature(t) != DeliveredState {
				t.Fatalf("%+v creates=%d updates=%d", result, p.pulls.creates, p.pulls.updates)
			}
			records, err := publications(p.repository, p.stream)
			must(t, err)
			last := records[len(records)-1]
			if last.Base != "main" || last.PullRequest != 100 || last.URL != in.Retarget.URL || last.PriorURL != last.URL || last.BaseWorkstream != "" {
				t.Fatal(last)
			}
			pending, err := upstreamDeliveryPending(p.repository, p.s.cfg, p.stream)
			must(t, err)
			if pending {
				t.Fatal("completed retarget still pending")
			}
			_, err = p.publisher().Apply(ctx, op)
			must(t, err)
			if p.pulls.updates != 1 {
				t.Fatal("retry repeated update")
			}
		})
	}
}

func TestSameRepositoryMaintenanceRefusesExternalPREdits(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"body", "base", "closed", "missing"} {
		t.Run(change, func(t *testing.T) {
			p := sameRepositoryMaintenance(t)
			approveMaintenance(t, p)
			before, _ := p.forkBranch(t)
			switch change {
			case "body":
				p.pulls.prs[0].Body = "external edit"
			case "base":
				p.pulls.prs[0].Base = "another-branch"
			case "closed":
				p.pulls.prs[0].State = "closed"
			case "missing":
				p.pulls.prs = []pulls.PullRequest{}
			}
			result, err := p.publisher().Apply(context.Background(), actionOperation(t, p.repository, p.stream, publishUpstreamAction))
			must(t, err)
			after, _ := p.forkBranch(t)
			if result.Outcome == "succeeded" || before != after || p.pulls.updates != 0 {
				t.Fatalf("unsafe publication: %+v", result)
			}
		})
	}
}
