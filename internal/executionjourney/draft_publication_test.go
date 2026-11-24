package executionjourney

import (
	"context"
	"errors"
	"fmt"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type draftFixture struct {
	f          *shedFixture
	stream     config.WorkstreamID
	repository *trace.Repository
	a          *finalReviewer
	fork       string
	pulls      *fixturekit.FakePulls
	received   string
	first      coreadapter.Operation
}

func newDraftFixture(t *testing.T, style string) *draftFixture {
	t.Helper()
	f, stream, repository, a := newFinalFixtureWith(t, "draft", "")
	d := &draftFixture{f: f, stream: stream, repository: repository, a: a}
	d.fork, d.pulls = fixturekit.PublicationFork(t, f.clone, func() string { commit, _ := d.forkBranch(t); return commit })
	d.pulls.SyncHead = true
	d.received = filepath.Join(filepath.Dir(d.fork), "received")
	hook := fmt.Sprintf("#!/bin/sh\necho push >> '%s'\n", d.received)
	fixturekit.Must(t, os.WriteFile(filepath.Join(d.fork, "hooks", "pre-receive"), []byte(hook), 0700))
	f.s.setSole(&activeProject{repository: repository})
	f.s.cfg.Project.Landing = style
	f.s.options.PullRequests = d.pulls
	f.s.options.reviewChecks = fixturekit.CheckFunc(func(context.Context, string) (api.CheckResult, error) {
		return api.CheckResult{Output: "all proofs passed"}, nil
	})
	_, d.first = assembleBoth(t, f, repository, a, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n\n// Resume resumes.\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n\n// Dedupe skips.\n"})
	return d
}

func (d *draftFixture) forkBranch(t *testing.T) (string, bool) {
	t.Helper()
	return fixturekit.PublicationForkBranch(t, d.fork, intake.FeatureBranch(d.stream))
}

func (d *draftFixture) pushCount(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(d.received)
	if errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	fixturekit.Must(t, err)
	return strings.Count(string(data), "push")
}

func (d *draftFixture) read(k int) {
	d.f.finalTurn(k, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Uploads resume.", "findings": []any{}})
		return err
	})
}

func (d *draftFixture) review(t *testing.T, files map[string]string) coreadapter.Operation {
	t.Helper()
	moveFeature(t, d.f, d.stream, files)
	fixturekit.Must(t, d.a.Pass(context.Background()))
	ops := finalOperations(t, d.repository, d.stream)
	for _, o := range ops {
		if in, err := delivery.DecodeFinalReview(o.Operation); err == nil && in.Review == len(ops) {
			return o.Operation
		}
	}
	t.Fatalf("no final review %d among %+v", len(ops), ops)
	return coreadapter.Operation{}
}

func (d *draftFixture) settle(t *testing.T, op coreadapter.Operation) coreadapter.OperationResult {
	t.Helper()
	return settleOperation(t, d.f.s, d.repository, d.stream, op, d.a)
}

func (d *draftFixture) reopen(t *testing.T) {
	t.Helper()
	d.repository = fixturekit.ReopenPublication(t, d.repository, d.f.s.cfg.Root, d.f.s.cfg.Project)
	d.f.s.setSole(&activeProject{repository: d.repository})
	d.a = &finalReviewer{s: d.f.s, repository: d.repository}
}

func (d *draftFixture) pushes(t *testing.T) []delivery.Push {
	t.Helper()
	pushes, err := delivery.Pushes(d.repository, d.stream)
	fixturekit.Must(t, err)
	return pushes
}

func (d *draftFixture) feature(t *testing.T) string {
	t.Helper()
	return fixturekit.PublicationFeature(t, d.repository, d.stream)
}

func (d *draftFixture) noApproval(t *testing.T) {
	t.Helper()
	if docs := intakefixture.StreamDocuments(t, d.repository, d.stream, delivery.DeliveryDocument); len(docs) != 0 {
		t.Fatalf("a delivery approval is recorded: %+v", docs)
	}
}

func TestFinalReviewPushesTheSignedBranchAndOpensADraftWithoutApproval(t *testing.T) {
	t.Parallel()
	for _, style := range []string{"squash", "commit-per-unit"} {
		t.Run(style, func(t *testing.T) {
			t.Parallel()
			d := newDraftFixture(t, style)
			ctx := context.Background()
			d.read(1)
			result, err := d.a.Apply(ctx, d.first)
			if err != nil || result.Outcome != "succeeded" {
				t.Fatalf("final review %+v: %v", result, err)
			}
			report, found, err := delivery.LatestFinalReport(d.repository, d.stream)
			if err != nil || !found {
				t.Fatalf("final report %+v: %v", report, err)
			}
			tip, exists := d.forkBranch(t)
			if !exists || tip == report.Commit || d.pushCount(t) != 1 {
				t.Fatalf("the fork's branch is at %q (reviewed %s) after %d pushes", tip, report.Commit, d.pushCount(t))
			}
			g := intake.Workspaces(d.f.s.cfg, intake.BranchesDirectory, config.WorkspacesGit)
			signed, err := g.Commit(ctx, tip)
			fixturekit.Must(t, err)
			reviewed, err := g.Commit(ctx, report.Commit)
			fixturekit.Must(t, err)
			if signed.Tree != reviewed.Tree {
				t.Fatalf("the pushed commit holds tree %s, the reviewed one %s", signed.Tree, reviewed.Tree)
			}
			if len(d.pulls.Prs) != 1 || d.pulls.Creates != 1 || d.pulls.Readies != 0 {
				t.Fatalf("pull requests %+v, %d created, %d marked ready", d.pulls.Prs, d.pulls.Creates, d.pulls.Readies)
			}
			pr := d.pulls.Prs[0]
			presented, apiErr := d.f.s.deliveryPresentation(ctx, string(d.stream))
			if apiErr != nil {
				t.Fatal(apiErr)
			}
			if !pr.Draft || pr.State != "open" || pr.Base != "main" || pr.Head != intake.FeatureBranch(d.stream) || pr.HeadRepository != "owner/dagger" || pr.HeadCommit != tip || pr.Title != presented.Title || pr.Body != presented.Draft || strings.Contains(pr.Body, "Before you ship") {
				t.Fatalf("pull request %+v, drafted %q %q", pr, presented.Title, presented.Draft)
			}
			recorded, err := delivery.Publications(d.repository, d.stream)
			fixturekit.Must(t, err)
			if len(recorded) != 2 || recorded[0].Status != delivery.PublicationPushing || recorded[0].Commit != tip || recorded[0].Push != 1 {
				t.Fatalf("publications %+v", recorded)
			}
			pushes := d.pushes(t)
			if len(pushes) != 1 {
				t.Fatalf("pushes %+v", pushes)
			}
			push := pushes[0]
			if push.Number != 1 || push.Review != 1 || push.Operation != d.first.ID || push.Reviewed != report.Commit || push.Commit != tip || push.PullRequest != pr.Number || push.URL != pr.URL || !push.Draft || push.At.IsZero() ||
				push.Upstream != "dagger/dagger" || push.Fork != "owner/dagger" || push.Base != "main" || push.Branch != intake.FeatureBranch(d.stream) {
				t.Fatalf("push %+v for pull request %+v", push, pr)
			}
			if current, found, err := delivery.CurrentPush(d.repository, d.stream); err != nil || !found || current != push {
				t.Fatalf("current push %+v: %v", current, err)
			}
			if style == "squash" && (len(signed.Parents) != 1 || signed.Parents[0] != report.Upstream.Commit || !strings.HasPrefix(signed.Message, presented.Title)) {
				t.Fatalf("the squashed commit %+v", signed)
			}
			d.noApproval(t)
			if got := d.feature(t); got != intake.AssembledState {
				t.Fatalf("the workstream is %s, want assembled", got)
			}
			again, err := d.a.Apply(ctx, d.first)
			if err != nil || again.Outcome != "succeeded" || d.pushCount(t) != 1 || d.pulls.Creates != 1 {
				t.Fatalf("applied again %+v: %v, %d pushes, %d pull requests", again, err, d.pushCount(t), d.pulls.Creates)
			}
		})
	}
}

func TestLaterFinalReviewsPushToTheSamePullRequestDraftOrReady(t *testing.T) {
	t.Parallel()
	for name, ready := range map[string]bool{"draft": false, "ready": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := newDraftFixture(t, "commit-per-unit")
			d.read(1)
			if result := d.settle(t, d.first); result.Outcome != "succeeded" {
				t.Fatalf("first final review %+v", result)
			}
			first := d.pushes(t)[0]
			if ready {
				d.pulls.Prs[0].Draft = false
			}
			second := d.review(t, map[string]string{"internal/trace/revision.go": "package trace\n\n// Revision answers a finding.\n"})
			d.read(2)
			if result := d.settle(t, second); result.Outcome != "succeeded" {
				t.Fatalf("second final review %+v", result)
			}
			report, _, err := delivery.LatestFinalReport(d.repository, d.stream)
			fixturekit.Must(t, err)
			tip, _ := d.forkBranch(t)
			if report.Review != 2 || tip == first.Commit || tip == report.Commit || d.pushCount(t) != 2 {
				t.Fatalf("review %d: the fork's branch is at %s (first push %s, reviewed %s) after %d pushes", report.Review, tip, first.Commit, report.Commit, d.pushCount(t))
			}
			if len(d.pulls.Prs) != 1 || d.pulls.Creates != 1 || d.pulls.Readies != 0 || d.pulls.Prs[0].Draft == ready || d.pulls.Prs[0].HeadCommit != tip {
				t.Fatalf("pull requests %+v, %d created, %d marked ready", d.pulls.Prs, d.pulls.Creates, d.pulls.Readies)
			}
			pushes := d.pushes(t)
			if len(pushes) != 2 {
				t.Fatalf("pushes %+v", pushes)
			}
			push := pushes[1]
			if push.Number != 2 || push.Review != 2 || push.Operation != second.ID || push.Reviewed != report.Commit || push.Commit != tip || push.PullRequest != first.PullRequest || push.URL != first.URL || push.Draft == ready || push.At.IsZero() {
				t.Fatalf("second push %+v after %+v", push, first)
			}
			d.noApproval(t)
			if got := d.feature(t); got != intake.AssembledState {
				t.Fatalf("the workstream is %s, want assembled", got)
			}

		})
	}
}

func TestFinalReviewPushesNothingToAMergedOrClosedPullRequest(t *testing.T) {
	t.Parallel()
	for name, finish := range map[string]func(*fixturekit.FakePulls){
		"closed": func(p *fixturekit.FakePulls) { p.Prs[0].State = "closed" },
		"merged": func(p *fixturekit.FakePulls) { p.Prs[0].State, p.Prs[0].Merged = "closed", true },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := newDraftFixture(t, "squash")
			d.read(1)
			if result := d.settle(t, d.first); result.Outcome != "succeeded" {
				t.Fatalf("first final review %+v", result)
			}
			before, _ := d.forkBranch(t)
			finish(d.pulls)
			second := d.review(t, map[string]string{"internal/trace/revision.go": "package trace\n"})
			if result := d.settle(t, second); result.Outcome != "failed" {
				t.Fatalf("second final review %+v", result)
			}
			if tip, _ := d.forkBranch(t); tip != before || d.pushCount(t) != 1 || len(d.pushes(t)) != 1 {
				t.Fatalf("the fork's branch moved from %s to %s after %d pushes, %+v", before, tip, d.pushCount(t), d.pushes(t))
			}
			if body := intakefixture.NoticeOf(t, d.repository, d.stream, "final-review-2-failed"); !strings.Contains(body, fmt.Sprintf("pull request #%d", d.pulls.Prs[0].Number)) || !strings.Contains(body, name) || !strings.Contains(body, "nothing was pushed") {
				t.Fatalf("the chief of staff's notice %q", body)
			}
			outcome, revision, err := delivery.OwnPullRequestOutcomeAt(d.repository, d.stream)
			if err != nil || revision == 0 || outcome.PullRequest != d.pulls.Prs[0].Number || outcome.Outcome != name {
				t.Fatalf("the recorded outcome %+v at revision %d: %v", outcome, revision, err)
			}
			d.noApproval(t)
		})
	}
}

func TestFinalReviewPushesOnceAcrossARestartAtEveryStep(t *testing.T) {
	t.Parallel()
	for _, step := range []string{"draft-recorded", "draft-pushed", "draft-opened", "draft-published"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			d := newDraftFixture(t, "squash")
			ctx := context.Background()
			d.read(1)
			d.f.s.boundary = func(name string) error {
				if name == step {
					return errors.New("stopped at " + name)
				}
				return nil
			}
			if _, err := d.a.Apply(ctx, d.first); err == nil {
				t.Fatal("the publication was not interrupted")
			}
			wantPushes := 1
			if step == "draft-recorded" {
				wantPushes = 0
			}
			if d.pushCount(t) != wantPushes || d.feature(t) != intake.AssembledState {
				t.Fatalf("%d pushes after the interruption, workstream %s", d.pushCount(t), d.feature(t))
			}
			d.reopen(t)
			d.f.s.boundary = nil
			result, err := d.a.Apply(ctx, d.first)
			if err != nil || result.Outcome != "succeeded" {
				t.Fatalf("final review after the restart %+v: %v", result, err)
			}
			tip, _ := d.forkBranch(t)
			if d.pushCount(t) != 1 || d.pulls.Creates != 1 || len(d.pulls.Prs) != 1 || d.pulls.Prs[0].HeadCommit != tip {
				t.Fatalf("%d pushes and %d pull requests after the restart: %+v", d.pushCount(t), d.pulls.Creates, d.pulls.Prs)
			}
			pushes := d.pushes(t)
			if len(pushes) != 1 || pushes[0].Number != 1 || pushes[0].Commit != tip || pushes[0].PullRequest != d.pulls.Prs[0].Number {
				t.Fatalf("pushes %+v", pushes)
			}
			d.noApproval(t)
		})
	}
}

func TestFinalReviewAdoptsAnOpenPullRequestItMadeBeforeRecordingIt(t *testing.T) {
	t.Parallel()
	d := newDraftFixture(t, "squash")
	ctx := context.Background()
	d.read(1)
	d.f.s.boundary = func(name string) error {
		if name == "draft-opened" {
			return errors.New("stopped at " + name)
		}
		return nil
	}
	if _, err := d.a.Apply(ctx, d.first); err == nil {
		t.Fatal("the publication was not interrupted")
	}
	if d.pulls.Creates != 1 || len(d.pushes(t)) != 0 {
		t.Fatalf("%d pull requests created, pushes %+v", d.pulls.Creates, d.pushes(t))
	}
	d.reopen(t)
	d.f.s.boundary = nil
	if result, err := d.a.Apply(ctx, d.first); err != nil || result.Outcome != "succeeded" {
		t.Fatalf("final review after the restart %+v: %v", result, err)
	}
	if d.pulls.Creates != 1 || len(d.pushes(t)) != 1 || d.pushes(t)[0].PullRequest != d.pulls.Prs[0].Number {
		t.Fatalf("%d pull requests created, pushes %+v", d.pulls.Creates, d.pushes(t))
	}
}
