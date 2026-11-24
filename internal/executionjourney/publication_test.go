package executionjourney

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestMarkingReadyUpdatesTheDraftOfThePushedCommit(t *testing.T) {
	t.Parallel()
	p := journey.NewPublicationFixture(t, "commit-per-unit")
	ctx := context.Background()
	fixturekit.Must(t, p.Publisher().Pass(ctx))
	if ops := p.Operations(t); len(ops) != 0 {
		t.Fatalf("marked ready without an owner approval: %+v", ops)
	}
	approval := p.Approve(t, nil)
	op := p.Request(t)
	fixturekit.Must(t, p.Publisher().Pass(ctx))
	if ops := p.Operations(t); len(ops) != 1 {
		t.Fatalf("asked twice for one approval: %d", len(ops))
	}
	result, err := p.Publisher().Apply(ctx, op)
	if err != nil || result.Outcome != "succeeded" {
		t.Fatalf("marking ready %+v: %v", result, err)
	}
	tip, _ := p.ForkBranch(t)
	if tip == p.Report.Commit {
		t.Fatal("delivery did not amend the reviewed commits")
	}
	assertDeliveryTree(t, p, tip)
	for _, unit := range []string{"resume.go", "dedupe.go"} {
		if log := fixturekit.DemoGit(t, filepath.Dir(p.Fork), "-C", p.Fork, "log", "--format=%H", tip, "--", unit); strings.Count(log, "\n") != 1 {
			t.Fatalf("the delivered branch does not keep the commit that landed %s: %q", unit, log)
		}
	}
	if p.Pulls.Creates != 1 || p.Pulls.Updates != 0 || p.Pulls.Readies != 1 || len(p.Pulls.Prs) != 1 {
		t.Fatalf("%d pull requests opened, %d updated, %d marked ready", p.Pulls.Creates, p.Pulls.Updates, p.Pulls.Readies)
	}
	pr := p.Pulls.Prs[0]
	if pr.Body != approval.Description || pr.Draft || pr.Base != "main" || pr.Head != intake.FeatureBranch(p.Stream) || pr.HeadRepository != "owner/dagger" || pr.Title != "Resumable uploads" || pr.HeadCommit != tip {
		t.Fatalf("pull request %+v", pr)
	}
	if got := p.Feature(t); got != intake.DeliveredState {
		t.Fatalf("the workstream is %s, want delivered", got)
	}
	recorded, err := delivery.Publications(p.Repository, p.Stream)
	fixturekit.Must(t, err)
	last := recorded[len(recorded)-1]
	if last.Push != 1 || last.Status != delivery.PublicationOpened || last.Reviewed != p.Report.Commit || last.Commit != tip || last.PullRequest != pr.Number || last.URL != pr.URL {
		t.Fatalf("publication %+v", last)
	}
	if body := intakefixture.NoticeOf(t, p.Repository, p.Stream, intake.DeliveredState); !strings.Contains(body, pr.URL) {
		t.Fatalf("delivery notice %q", body)
	}
	again, err := p.Publisher().Apply(ctx, op)
	if err != nil || again.Outcome != "succeeded" || p.Pulls.Creates != 1 || p.Pulls.Readies != 1 {
		t.Fatalf("a retry of a recorded marking %+v, %v, %d pull requests, %d ready calls", again, err, p.Pulls.Creates, p.Pulls.Readies)
	}
	fixturekit.Must(t, p.Publisher().Pass(ctx))
	if ops := p.Operations(t); len(ops) != 1 {
		t.Fatalf("asked to mark a delivered workstream ready: %d", len(ops))
	}
	presented, apiErr := p.S.DeliveryPresentation(ctx, string(p.Stream))
	if apiErr != nil || !presented.Delivered || presented.Ready == nil || presented.Ready.PullRequest != pr.Number || presented.Ready.Source != api.ReadyByApproval {
		t.Fatalf("delivered presentation %+v: %v", presented, apiErr)
	}
	if _, apiErr := p.S.ApproveDelivery(ctx, string(p.Stream), api.DeliveryDecision{Review: 1, ReviewRevision: 1, Commit: p.Report.Commit, DraftHash: approval.DraftHash}); apiErr == nil || apiErr.Code != api.Conflict {
		t.Fatalf("approved a delivered workstream: %v", apiErr)
	}
}

func TestMarkingReadyRefusesAReviewedCommitWithNoRecordedPush(t *testing.T) {
	t.Parallel()
	for _, style := range []string{"commit-per-unit", "squash"} {
		t.Run(style, func(t *testing.T) {
			t.Parallel()
			p := journey.NewUnpushedPublicationFixture(t, style, "")
			ctx := context.Background()
			p.Approve(t, nil)
			op := p.Request(t)
			result, err := p.Publisher().Apply(ctx, op)
			if err != nil || result.Outcome != "failed" || !strings.Contains(result.Evidence, "records no push of the reviewed commit "+p.Report.Commit) || !strings.Contains(result.Evidence, "nothing was pushed, opened or marked ready") {
				t.Fatalf("marking ready %+v: %v", result, err)
			}
			if _, pushed := p.ForkBranch(t); pushed || p.Pulls.Finds != 0 || p.Pulls.Creates != 0 || p.Pulls.Updates != 0 || p.Pulls.Readies != 0 || len(p.Pulls.Prs) != 0 {
				t.Fatalf("the refused marking pushed %v, asked the host %d times, opened %d, updated %d and marked %d ready", pushed, p.Pulls.Finds, p.Pulls.Creates, p.Pulls.Updates, p.Pulls.Readies)
			}
			if got := p.Feature(t); got != intake.AssembledState {
				t.Fatalf("the workstream is %s, want assembled", got)
			}
			if pushes, err := delivery.Pushes(p.Repository, p.Stream); err != nil || len(pushes) != 0 {
				t.Fatalf("pushes %+v: %v", pushes, err)
			}
			transition, _ := delivery.PublishIDs(1)
			if body := intakefixture.NoticeOf(t, p.Repository, p.Stream, transition+"-refused"); !strings.Contains(body, "records no push of the reviewed commit") || !strings.Contains(body, "not marked ready") {
				t.Fatalf("refusal notice %q", body)
			}
		})
	}
}

func TestMarkingReadySquashDeliversOneCommitWithTheReviewedTree(t *testing.T) {
	t.Parallel()
	p := journey.NewPublicationFixture(t, "squash")
	ctx := context.Background()
	p.Approve(t, nil)
	op := p.Request(t)
	result, err := p.Publisher().Apply(ctx, op)
	if err != nil || result.Outcome != "succeeded" {
		t.Fatalf("publication %+v: %v", result, err)
	}
	tip, _ := p.ForkBranch(t)
	g := intake.Workspaces(p.S.Cfg, intake.BranchesDirectory, config.WorkspacesGit)
	delivered, err := g.Commit(ctx, tip)
	fixturekit.Must(t, err)
	reviewed, err := g.Commit(ctx, p.Report.Commit)
	fixturekit.Must(t, err)
	if tip == p.Report.Commit || delivered.Tree != reviewed.Tree || !slices.Equal(delivered.Parents, []string{p.Report.Upstream.Commit}) {
		t.Fatalf("delivered %s %+v, reviewed %+v on %s", tip, delivered, reviewed, p.Report.Upstream.Commit)
	}
	if !strings.HasPrefix(delivered.Message, "Resumable uploads\n") {
		t.Fatalf("squash message %q", delivered.Message)
	}
	if local, _, err := g.Branch(ctx, intake.FeatureBranch(p.Stream)); err != nil || local != p.Report.Commit {
		t.Fatalf("the feature branch moved to %s: %v", local, err)
	}
	if p.Pulls.Prs[0].HeadCommit != tip || p.Feature(t) != intake.DeliveredState {
		t.Fatalf("pull request %+v, workstream %s", p.Pulls.Prs[0], p.Feature(t))
	}
	recorded, err := delivery.Publications(p.Repository, p.Stream)
	fixturekit.Must(t, err)
	if last := recorded[len(recorded)-1]; last.Style != "squash" || last.Commit != tip || last.Reviewed != p.Report.Commit || last.Push != 1 {
		t.Fatalf("publication %+v", last)
	}
}

func TestMarkingReadyRefusesAStaleApprovalBeforeAnySideEffect(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(*testing.T, *journey.PublicationFixture){
		"branch moved": func(t *testing.T, p *journey.PublicationFixture) {
			journey.MoveFeature(t, p.ShedFixture, p.Stream, map[string]string{"late.go": "package demo\n"})
		},
		"description replaced": func(t *testing.T, p *journey.PublicationFixture) {
			edited := "# Edited\n"
			p.Approve(t, &edited)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := journey.NewPublicationFixture(t, "commit-per-unit")
			ctx := context.Background()
			p.Approve(t, nil)
			op := p.Request(t)
			change(t, p)
			result, err := p.Publisher().Apply(ctx, op)
			if err != nil || result.Outcome != "failed" {
				t.Fatalf("publication %+v: %v", result, err)
			}
			if tip, _ := p.ForkBranch(t); tip != p.Pushed || p.Pulls.Finds != p.Finds || p.Pulls.Creates != 1 || p.Pulls.Updates != 0 || p.Pulls.Readies != 0 {
				t.Fatalf("a stale approval touched the fork or pull requests: %d finds, %d creates, %d updates, %d ready calls", p.Pulls.Finds, p.Pulls.Creates, p.Pulls.Updates, p.Pulls.Readies)
			}
			if got := p.Feature(t); got != intake.AssembledState {
				t.Fatalf("the workstream is %s", got)
			}
			transition, _ := delivery.PublishIDs(1)
			if body := intakefixture.NoticeOf(t, p.Repository, p.Stream, transition+"-refused"); !strings.Contains(body, "not marked ready") {
				t.Fatalf("refusal notice %q", body)
			}
		})
	}
}

func TestMarkingReadyRecoveryRecordsOneDurableResult(t *testing.T) {
	t.Parallel()
	for _, point := range []string{"ready-described", "ready-marked", "after-trace"} {
		t.Run(point, func(t *testing.T) {
			t.Parallel()
			p := journey.NewPublicationFixture(t, "squash")
			edited := "# Edited\n"
			p.Approve(t, &edited)
			op := p.Request(t)
			p.S.Boundary = func(step string) error {
				if step == point {
					return errors.New("interrupted")
				}
				return nil
			}
			result, err := p.Publisher().Apply(context.Background(), op)
			if point == "after-trace" {
				if err != nil || result.Outcome != "succeeded" {
					t.Fatalf("delivery %+v: %v", result, err)
				}
			} else if err == nil {
				t.Fatal("marking ready was not interrupted")
			}
			p.Reopen(t)
			p.S.Boundary = nil
			result = journey.SettleOperation(t, p.S, p.Repository, p.Stream, op, p.Publisher())
			if result.Outcome != "succeeded" || p.Pulls.Creates != 1 || p.Pulls.Updates != 1 || p.Pulls.Readies != 1 || p.Pulls.Prs[0].Draft || p.Feature(t) != intake.DeliveredState {
				t.Fatalf("recovered %+v, creates %d, updates %d, ready calls %d, state %s", result, p.Pulls.Creates, p.Pulls.Updates, p.Pulls.Readies, p.Feature(t))
			}
			ops, err := p.Repository.Operations(p.Stream)
			fixturekit.Must(t, err)
			i := slices.IndexFunc(ops, func(record trace.OperationRecord) bool { return record.Operation.ID == op.ID })
			if i < 0 || ops[i].Result == nil || ops[i].Result.Outcome != "succeeded" {
				t.Fatalf("publish operation result %+v", ops)
			}
			if got := intakefixture.StreamDocuments(t, p.Repository, p.Stream, delivery.PublicationDocument); len(got) > 2 {
				t.Fatalf("duplicate publication records: %d", len(got))
			}
			if got := intakefixture.StreamDocuments(t, p.Repository, p.Stream, delivery.ReadyDocument); len(got) != 1 {
				t.Fatalf("ready records: %d", len(got))
			}
		})
	}
}

func TestMarkingReadyRefusesConflictingRemoteStateUntilApprovedAgain(t *testing.T) {
	t.Parallel()
	t.Run("foreign branch", func(t *testing.T) {
		t.Parallel()
		p := journey.NewPublicationFixture(t, "commit-per-unit")
		ctx := context.Background()
		p.Approve(t, nil)
		foreign := p.Report.Upstream.Commit
		fixturekit.DemoGit(t, filepath.Dir(p.Fork), "-C", p.Clone, "push", "--quiet", "--force", "origin", foreign+":refs/heads/"+intake.FeatureBranch(p.Stream))
		p.Pulls.Prs[0].HeadCommit = foreign
		op := p.Request(t)
		result, err := p.Publisher().Apply(ctx, op)
		if err != nil || result.Outcome != "failed" || !strings.Contains(result.Evidence, "no push of this workstream made") {
			t.Fatalf("publication %+v: %v", result, err)
		}
		if tip, _ := p.ForkBranch(t); tip != foreign || p.Pulls.Creates != 1 || p.Pulls.Readies != 0 || p.Feature(t) != intake.AssembledState {
			t.Fatalf("the fork branch is at %s; %d creates; %d ready calls; the workstream is %s", tip, p.Pulls.Creates, p.Pulls.Readies, p.Feature(t))
		}
		fixturekit.Must(t, p.Publisher().Pass(ctx))
		if ops := p.Operations(t); len(ops) != 1 {
			t.Fatalf("asked again without a new approval: %d", len(ops))
		}
		fixturekit.DemoGit(t, filepath.Dir(p.Fork), "-C", p.Clone, "push", "--quiet", "--force", "origin", p.Pushed+":refs/heads/"+intake.FeatureBranch(p.Stream))
		p.Pulls.Prs[0].HeadCommit = p.Pushed
		if again := p.Approve(t, nil); again.Commit != p.Report.Commit {
			t.Fatalf("approval %+v", again)
		}
		if docs := intakefixture.StreamDocuments(t, p.Repository, p.Stream, delivery.DeliveryDocument); len(docs) != 2 {
			t.Fatalf("approving after a refusal recorded %d approvals", len(docs))
		}
		retry := p.Request(t)
		result, err = p.Publisher().Apply(ctx, retry)
		if err != nil || result.Outcome != "succeeded" || p.Feature(t) != intake.DeliveredState {
			t.Fatalf("publication after approving again %+v: %v", result, err)
		}
	})
	t.Run("different pull request", func(t *testing.T) {
		t.Parallel()
		p := journey.NewPublicationFixture(t, "commit-per-unit")
		ctx := context.Background()
		p.Approve(t, nil)
		own := p.Pulls.Prs[0]
		p.Pulls.Prs = []pulls.PullRequest{{Number: 7, URL: "https://github.com/dagger/dagger/pull/7", State: "open", Head: intake.FeatureBranch(p.Stream), HeadRepository: "owner/dagger", HeadCommit: p.Pushed, Base: "main", Body: "Someone else's description"}}
		op := p.Request(t)
		result, err := p.Publisher().Apply(ctx, op)
		if err != nil || result.Outcome != "failed" || !strings.Contains(result.Evidence, fmt.Sprintf("#%d", own.Number)) {
			t.Fatalf("publication %+v: %v", result, err)
		}
		if tip, _ := p.ForkBranch(t); tip != p.Pushed || p.Pulls.Creates != 1 || p.Pulls.Readies != 0 || p.Feature(t) != intake.AssembledState {
			t.Fatalf("the fork branch is at %s; %d creates; %d ready calls; the workstream is %s", tip, p.Pulls.Creates, p.Pulls.Readies, p.Feature(t))
		}
	})
}

func TestServicePublishesAnApprovedWorkstreamAfterRestart(t *testing.T) {
	t.Parallel()
	p := journey.NewPublicationFixture(t, "commit-per-unit")
	approval := p.Approve(t, nil)
	p.Repository.Close()
	p.Opts.PullRequests = p.Pulls
	p.Start(t)
	defer p.Stop(t)
	p.AwaitFeature(t, p.Stream, intake.DeliveredState)
	if tip, _ := p.ForkBranch(t); tip == p.Report.Commit || p.Pulls.Creates != 1 || p.Pulls.Prs[0].Body != approval.Description {
		t.Fatalf("the fork branch is at %s; pull requests %+v", tip, p.Pulls.Prs)
	}
	transition, _ := delivery.PublishIDs(1)
	if got := intakefixture.TransitionByID(t, p.ShedFixture.Repository(), p.Stream, transition+"-published"); got.To != "published-1" {
		t.Fatalf("publication outcome %+v", got)
	}
}

func assertDeliveryTree(t *testing.T, p *journey.PublicationFixture, tip string) {
	t.Helper()
	tree := fixturekit.DemoGit(t, filepath.Dir(p.Clone), "-C", p.Clone, "show", "-s", "--format=%T", tip)
	reviewed := fixturekit.DemoGit(t, filepath.Dir(p.Clone), "-C", p.Clone, "show", "-s", "--format=%T", p.Report.Commit)
	if tree != reviewed {
		t.Fatal("publication changed reviewed tree")
	}
}

func TestDraftPublicationSigningFailureBlocksPushUntilSigned(t *testing.T) {
	t.Parallel()
	p := journey.NewUnpushedPublicationFixture(t, "squash", "")
	fixturekit.DemoGit(t, filepath.Dir(p.Clone), "-C", p.Clone, "config", "gpg.program", "false")
	reason, err := p.TryPublishDraft(t)
	if err != nil || !strings.Contains(reason, "pre-publication amendment failed") {
		t.Fatalf("signing failure: %q %v", reason, err)
	}
	if _, exists := p.ForkBranch(t); exists || p.Pulls.Creates != 0 {
		t.Fatal("signing failure touched publication")
	}
	if p.Feature(t) != intake.AssembledState {
		t.Fatal("signing failure delivered the workstream")
	}
	fixturekit.ConfigureDeliverySigner(t, p.Clone)
	p.PublishDraft(t)
	p.Approve(t, nil)
	result, err := p.Publisher().Apply(context.Background(), p.Request(t))
	if err != nil || result.Outcome != "succeeded" {
		t.Fatalf("retry: %+v %v", result, err)
	}
}

func TestRepeatedPublicationFailuresReachTheInbox(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := journey.NewPublicationFixture(t, "squash")
	p.Approve(t, nil)
	in, err := delivery.DecodePublish(p.Request(t))
	fixturekit.Must(t, err)
	refusal := "find pull requests of acme/dagger from owner/dagger:osmia/w: GitHub answered 403: Resource not accessible by personal access token; the request needs the token permissions pull_requests=read"
	p.Pulls.FindErr = errors.New(refusal)

	clock := &fixturekit.FixedClock{At: p.Clock.Now().Add(time.Hour)}
	c, err := reconcile.New(p.Repository, reconcile.Options{Worker: "test", Now: clock.Now, RetryDelay: time.Minute,
		Adapters: map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RepositoryBoundary: p.Publisher()},
		Hold:     func(_ config.WorkstreamID, op coreadapter.Operation) bool { return op.Action != delivery.PublishAction }})
	fixturekit.Must(t, err)
	publication := func() (api.InboxEntry, bool) {
		t.Helper()
		inbox, apiErr := p.S.Inbox(ctx)
		if apiErr != nil {
			t.Fatal(apiErr)
		}
		for _, e := range inbox.Entries {
			if e.Kind == api.InboxPublication {
				return e, true
			}
		}
		return api.InboxEntry{}, false
	}
	var firstFailure time.Time
	for attempt, wait := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute} {
		fixturekit.Must(t, c.Pass(ctx))
		if attempt == 0 {
			firstFailure = clock.Now()
		}
		e, found := publication()
		if attempt < intake.PublicationEscalation-1 {
			if found {
				t.Fatalf("raised after %d failures: %+v", attempt+1, e)
			}
		} else {
			want := fmt.Sprintf("Marking the pull request ready for review (owner approval %d) has failed 3 times in a row. Last failure: Effect returned error: %s", in.Approval, refusal)
			retryAt := clock.Now().Add(wait).UTC().Format(time.RFC3339)
			if !found || e.Workstream != p.Stream || e.Question != want || len(e.Options) != 0 || e.Revision != in.Approval ||
				!e.OpenedAt.Equal(firstFailure) || !strings.Contains(e.Blocked, "tries again at "+retryAt) || e.Answer.Method != http.MethodGet {
				t.Fatalf("publication entry %+v, want question %q and a retry at %s", e, want, retryAt)
			}
		}

		clock.Advance(wait - time.Second)
		fixturekit.Must(t, c.Pass(ctx))
		if p.Pulls.Finds-p.Finds != attempt+1 {
			t.Fatalf("attempted %d times before the retry was due, want %d", p.Pulls.Finds-p.Finds, attempt+1)
		}
		clock.Advance(time.Second)
	}
	p.Pulls.FindErr = nil
	fixturekit.Must(t, c.Pass(ctx))
	if p.Feature(t) != intake.DeliveredState || p.Pulls.Creates != 1 {
		t.Fatalf("state %s after %d pull requests", p.Feature(t), p.Pulls.Creates)
	}
	if e, found := publication(); found {
		t.Fatalf("the published workstream stays raised: %+v", e)
	}
}

func TestPublicationWaitsForAGitHubToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := journey.NewPublicationFixture(t, "squash")
	p.Approve(t, nil)
	op := p.Request(t)
	in, err := delivery.DecodePublish(op)
	fixturekit.Must(t, err)
	p.Pulls.Mu.Lock()
	p.Pulls.Uncredentialed = true
	p.Pulls.Mu.Unlock()
	clock := &fixturekit.FixedClock{At: p.Clock.Now().Add(time.Hour)}
	c, err := reconcile.New(p.Repository, reconcile.Options{Worker: "test", Now: clock.Now, RetryDelay: time.Minute,
		Adapters: map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RepositoryBoundary: p.Publisher()},
		Hold:     p.S.Holding(p.Repository)})
	fixturekit.Must(t, err)
	for range 3 {
		fixturekit.Must(t, c.Pass(ctx))
		clock.Advance(time.Hour)
	}
	if tip, _ := p.ForkBranch(t); tip != p.Pushed || p.Pulls.Finds != p.Finds || p.Pulls.Creates != 1 || p.Pulls.Readies != 0 {
		t.Fatalf("a publication without a credential left the branch at %s, found %d times, opened %d pull requests and marked %d ready", tip, p.Pulls.Finds-p.Finds, p.Pulls.Creates, p.Pulls.Readies)
	}
	ops, err := p.Repository.Operations(p.Stream)
	fixturekit.Must(t, err)
	i := slices.IndexFunc(ops, func(r trace.OperationRecord) bool { return r.Operation.ID == op.ID })
	if i < 0 || len(ops[i].History) != 0 {
		t.Fatalf("the held publication was attempted: %+v", ops)
	}
	requested := ops[i].Transition.At
	inbox, apiErr := p.S.Inbox(ctx)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	i = slices.IndexFunc(inbox.Entries, func(e api.InboxEntry) bool { return e.Kind == api.InboxPublication })
	if i < 0 {
		t.Fatalf("the held publication is not in the inbox: %+v", inbox.Entries)
	}
	e := inbox.Entries[i]
	if !strings.Contains(e.Question, "waits for a GitHub token") || !strings.Contains(e.Blocked, "Restart osmia serve with GITHUB_TOKEN set") ||
		!strings.Contains(e.Blocked, in.Upstream) || !e.OpenedAt.Equal(requested) || len(e.Options) != 0 {
		t.Fatalf("held publication entry %+v", e)
	}

	if _, err := p.Publisher().Apply(ctx, op); !errors.Is(err, delivery.ErrNoPullRequestCredential) {
		t.Fatalf("an attempt without a credential: %v", err)
	}
	if tip, _ := p.ForkBranch(t); tip != p.Pushed || p.Pulls.Finds != p.Finds {
		t.Fatal("a refused attempt pushed or asked the host")
	}
	p.Pulls.Mu.Lock()
	p.Pulls.Uncredentialed = false
	p.Pulls.Mu.Unlock()
	fixturekit.Must(t, c.Pass(ctx))
	if p.Feature(t) != intake.DeliveredState || p.Pulls.Creates != 1 {
		t.Fatalf("state %s after %d pull requests", p.Feature(t), p.Pulls.Creates)
	}
	inbox, apiErr = p.S.Inbox(ctx)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if slices.ContainsFunc(inbox.Entries, func(e api.InboxEntry) bool { return e.Kind == api.InboxPublication }) {
		t.Fatal("the published workstream stays raised")
	}
}

func TestApprovingRefusesEditedCommitMessages(t *testing.T) {
	t.Parallel()
	p := journey.NewPublicationFixture(t, "squash")
	view, apiErr := p.S.DeliveryPresentation(context.Background(), string(p.Stream))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	messages := slices.Clone(view.Messages)
	messages[0].Message = "Owner's subject\n\nOwner's explanation.\n"
	_, apiErr = p.S.ApproveDelivery(context.Background(), string(p.Stream), api.DeliveryDecision{Review: view.Report.Review, ReviewRevision: view.ReviewRevision, Commit: view.Report.Commit, DraftHash: view.DraftHash, Messages: messages})
	if apiErr == nil || apiErr.Code != api.Validation {
		t.Fatalf("edited commit messages: %v", apiErr)
	}
	if docs := intakefixture.StreamDocuments(t, p.Repository, p.Stream, delivery.DeliveryDocument); len(docs) != 0 {
		t.Fatalf("an approval was recorded: %+v", docs)
	}
}
