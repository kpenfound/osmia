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

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

type publicationFixture struct {
	*shedFixture
	stream     config.WorkstreamID
	repository *trace.Repository
	report     FinalReport
	fork       string
	pulls      *fakePulls
	// pushed is the commit the draft push left on the fork's feature branch,
	// and finds how many times the host had been asked for pull requests by
	// then.
	pushed string
	finds  int
}

// newPublicationFixture reviews an assembled workstream, gives the clone a
// local bare fork of owner/dagger as its origin remote and a fake pull
// request host, and configures the delivery style.
func newPublicationFixture(t *testing.T, style string) *publicationFixture {
	t.Helper()
	return newPublicationFixtureWith(t, style, "")
}

func newPublicationFixtureWith(t *testing.T, style, extra string) *publicationFixture {
	t.Helper()
	p := newUnpushedPublicationFixture(t, style, extra)
	p.publishDraft(t)
	return p
}

// newUnpushedPublicationFixture is a publication fixture whose reviewed
// commit has no recorded push: the service has not yet signed it, pushed it or
// opened the draft pull request.
func newUnpushedPublicationFixture(t *testing.T, style, extra string) *publicationFixture {
	t.Helper()
	f, stream, repository, report := deliveryFixtureWith(t, extra)
	p := &publicationFixture{shedFixture: f, stream: stream, repository: repository, report: report}
	p.fork, p.pulls = fixturekit.PublicationFork(t, f.clone, func() string { commit, _ := p.forkBranch(t); return commit })
	f.s.cfg.Project.Landing = style
	f.s.options.PullRequests = p.pulls
	return p
}

// tryPublishDraft has the final review's publication sign the reviewed
// commit, push it and open the draft pull request, as the review does before
// its checks are watched, and returns why it refused and any error.
func (p *publicationFixture) tryPublishDraft(t *testing.T) (string, error) {
	t.Helper()
	report := p.report
	report.Review, report.Operation, report.Branch = 1, "operation_final_review_1", featureBranch(p.stream)
	reason, err := (&finalReviewer{s: p.s, repository: p.repository}).PublishDraft(context.Background(), p.s.cfg, p.stream, coreadapter.Operation{ID: report.Operation}, report)
	if err == nil && reason == "" {
		p.pushed, _ = p.forkBranch(t)
		p.finds = p.pulls.Finds
	}
	return reason, err
}

// publishDraft is tryPublishDraft for a publication that must succeed.
func (p *publicationFixture) publishDraft(t *testing.T) {
	t.Helper()
	if reason, err := p.tryPublishDraft(t); err != nil || reason != "" {
		t.Fatalf("publishing the draft: %q, %v", reason, err)
	}
}

func (p *publicationFixture) forkBranch(t *testing.T) (string, bool) {
	t.Helper()
	return fixturekit.PublicationForkBranch(t, p.fork, featureBranch(p.stream))
}

func (p *publicationFixture) approve(t *testing.T, description *string) DeliveryApproval {
	t.Helper()
	return fixturekit.ApprovePublication(t, p.stream, description, p.s.deliveryPresentation, p.s.approveDelivery)
}

func (p *publicationFixture) publisher() *publisher {
	return &publisher{s: p.s, repository: p.repository}
}

func (p *publicationFixture) operations(t *testing.T) []coreadapter.Operation {
	t.Helper()
	return fixturekit.PublicationOperations(t, p.repository, p.stream, PublishAction)
}

func (p *publicationFixture) request(t *testing.T) coreadapter.Operation {
	t.Helper()
	return fixturekit.RequestPublication(t, p.repository, p.stream, PublishAction, p.publisher().Pass)
}

func (p *publicationFixture) feature(t *testing.T) string {
	t.Helper()
	return fixturekit.PublicationFeature(t, p.repository, p.stream)
}

func (p *publicationFixture) reopen(t *testing.T) {
	t.Helper()
	p.repository = fixturekit.ReopenPublication(t, p.repository, p.s.cfg.Root, p.s.cfg.Project)
	p.s.setSole(&activeProject{repository: p.repository})
}

// Marking ready for review updates the draft the final review pushed and
// opened, marks it ready and delivers the workstream. It pushes nothing and
// opens no pull request.
func TestMarkingReadyUpdatesTheDraftOfThePushedCommit(t *testing.T) {
	t.Parallel()
	p := newPublicationFixture(t, "commit-per-unit")
	ctx := context.Background()
	must(t, p.publisher().Pass(ctx))
	if ops := p.operations(t); len(ops) != 0 {
		t.Fatalf("marked ready without an owner approval: %+v", ops)
	}
	approval := p.approve(t, nil)
	op := p.request(t)
	must(t, p.publisher().Pass(ctx))
	if ops := p.operations(t); len(ops) != 1 {
		t.Fatalf("asked twice for one approval: %d", len(ops))
	}
	result, err := p.publisher().Apply(ctx, op)
	if err != nil || result.Outcome != "succeeded" {
		t.Fatalf("marking ready %+v: %v", result, err)
	}
	tip, _ := p.forkBranch(t)
	if tip == p.report.Commit {
		t.Fatal("delivery did not amend the reviewed commits")
	}
	assertDeliveryTree(t, p, tip)
	for _, unit := range []string{"resume.go", "dedupe.go"} {
		if log := demoGit(t, filepath.Dir(p.fork), "-C", p.fork, "log", "--format=%H", tip, "--", unit); strings.Count(log, "\n") != 1 {
			t.Fatalf("the delivered branch does not keep the commit that landed %s: %q", unit, log)
		}
	}
	if p.pulls.Creates != 1 || p.pulls.Updates != 0 || p.pulls.Readies != 1 || len(p.pulls.Prs) != 1 {
		t.Fatalf("%d pull requests opened, %d updated, %d marked ready", p.pulls.Creates, p.pulls.Updates, p.pulls.Readies)
	}
	pr := p.pulls.Prs[0]
	if pr.Body != approval.Description || pr.Draft || pr.Base != "main" || pr.Head != featureBranch(p.stream) || pr.HeadRepository != "owner/dagger" || pr.Title != "Resumable uploads" || pr.HeadCommit != tip {
		t.Fatalf("pull request %+v", pr)
	}
	if got := p.feature(t); got != DeliveredState {
		t.Fatalf("the workstream is %s, want delivered", got)
	}
	recorded, err := publications(p.repository, p.stream)
	must(t, err)
	last := recorded[len(recorded)-1]
	if last.Push != 1 || last.Status != publicationOpened || last.Reviewed != p.report.Commit || last.Commit != tip || last.PullRequest != pr.Number || last.URL != pr.URL {
		t.Fatalf("publication %+v", last)
	}
	if body := noticeOf(t, p.repository, p.stream, DeliveredState); !strings.Contains(body, pr.URL) {
		t.Fatalf("delivery notice %q", body)
	}
	again, err := p.publisher().Apply(ctx, op)
	if err != nil || again.Outcome != "succeeded" || p.pulls.Creates != 1 || p.pulls.Readies != 1 {
		t.Fatalf("a retry of a recorded marking %+v, %v, %d pull requests, %d ready calls", again, err, p.pulls.Creates, p.pulls.Readies)
	}
	must(t, p.publisher().Pass(ctx))
	if ops := p.operations(t); len(ops) != 1 {
		t.Fatalf("asked to mark a delivered workstream ready: %d", len(ops))
	}
	presented, apiErr := p.s.deliveryPresentation(ctx, string(p.stream))
	if apiErr != nil || !presented.Delivered || presented.Ready == nil || presented.Ready.PullRequest != pr.Number || presented.Ready.Source != api.ReadyByApproval {
		t.Fatalf("delivered presentation %+v: %v", presented, apiErr)
	}
	if _, api := p.s.approveDelivery(ctx, string(p.stream), DeliveryDecision{Review: 1, ReviewRevision: 1, Commit: p.report.Commit, DraftHash: approval.DraftHash}); api == nil || api.Code != Conflict {
		t.Fatalf("approved a delivered workstream: %v", api)
	}
}

// A reviewed commit with no recorded push is never published by marking
// ready: the owner's approval is refused with the reason, nothing is pushed,
// no pull request is opened or marked ready, and the workstream is not
// delivered.
func TestMarkingReadyRefusesAReviewedCommitWithNoRecordedPush(t *testing.T) {
	t.Parallel()
	for _, style := range []string{"commit-per-unit", "squash"} {
		t.Run(style, func(t *testing.T) {
			t.Parallel()
			p := newUnpushedPublicationFixture(t, style, "")
			ctx := context.Background()
			p.approve(t, nil)
			op := p.request(t)
			result, err := p.publisher().Apply(ctx, op)
			if err != nil || result.Outcome != "failed" || !strings.Contains(result.Evidence, "records no push of the reviewed commit "+p.report.Commit) || !strings.Contains(result.Evidence, "nothing was pushed, opened or marked ready") {
				t.Fatalf("marking ready %+v: %v", result, err)
			}
			if _, pushed := p.forkBranch(t); pushed || p.pulls.Finds != 0 || p.pulls.Creates != 0 || p.pulls.Updates != 0 || p.pulls.Readies != 0 || len(p.pulls.Prs) != 0 {
				t.Fatalf("the refused marking pushed %v, asked the host %d times, opened %d, updated %d and marked %d ready", pushed, p.pulls.Finds, p.pulls.Creates, p.pulls.Updates, p.pulls.Readies)
			}
			if got := p.feature(t); got != AssembledState {
				t.Fatalf("the workstream is %s, want assembled", got)
			}
			if pushes, err := delivery.Pushes(p.repository, p.stream); err != nil || len(pushes) != 0 {
				t.Fatalf("pushes %+v: %v", pushes, err)
			}
			transition, _ := publishIDs(1)
			if body := noticeOf(t, p.repository, p.stream, transition+"-refused"); !strings.Contains(body, "records no push of the reviewed commit") || !strings.Contains(body, "not marked ready") {
				t.Fatalf("refusal notice %q", body)
			}
		})
	}
}

func TestMarkingReadySquashDeliversOneCommitWithTheReviewedTree(t *testing.T) {
	t.Parallel()
	p := newPublicationFixture(t, "squash")
	ctx := context.Background()
	p.approve(t, nil)
	op := p.request(t)
	result, err := p.publisher().Apply(ctx, op)
	if err != nil || result.Outcome != "succeeded" {
		t.Fatalf("publication %+v: %v", result, err)
	}
	tip, _ := p.forkBranch(t)
	g := workspaces(p.s.cfg, branchesDirectory, config.WorkspacesGit)
	delivered, err := g.Commit(ctx, tip)
	must(t, err)
	reviewed, err := g.Commit(ctx, p.report.Commit)
	must(t, err)
	if tip == p.report.Commit || delivered.Tree != reviewed.Tree || !slices.Equal(delivered.Parents, []string{p.report.Upstream.Commit}) {
		t.Fatalf("delivered %s %+v, reviewed %+v on %s", tip, delivered, reviewed, p.report.Upstream.Commit)
	}
	if !strings.HasPrefix(delivered.Message, "Resumable uploads\n") {
		t.Fatalf("squash message %q", delivered.Message)
	}
	if local, _, err := g.Branch(ctx, featureBranch(p.stream)); err != nil || local != p.report.Commit {
		t.Fatalf("the feature branch moved to %s: %v", local, err)
	}
	if p.pulls.Prs[0].HeadCommit != tip || p.feature(t) != DeliveredState {
		t.Fatalf("pull request %+v, workstream %s", p.pulls.Prs[0], p.feature(t))
	}
	recorded, err := publications(p.repository, p.stream)
	must(t, err)
	if last := recorded[len(recorded)-1]; last.Style != "squash" || last.Commit != tip || last.Reviewed != p.report.Commit || last.Push != 1 {
		t.Fatalf("publication %+v", last)
	}
}

func TestMarkingReadyRefusesAStaleApprovalBeforeAnySideEffect(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(*testing.T, *publicationFixture){
		"branch moved": func(t *testing.T, p *publicationFixture) {
			moveFeature(t, p.shedFixture, p.stream, map[string]string{"late.go": "package demo\n"})
		},
		"description replaced": func(t *testing.T, p *publicationFixture) {
			edited := "# Edited\n"
			p.approve(t, &edited)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := newPublicationFixture(t, "commit-per-unit")
			ctx := context.Background()
			p.approve(t, nil)
			op := p.request(t)
			change(t, p)
			result, err := p.publisher().Apply(ctx, op)
			if err != nil || result.Outcome != "failed" {
				t.Fatalf("publication %+v: %v", result, err)
			}
			if tip, _ := p.forkBranch(t); tip != p.pushed || p.pulls.Finds != p.finds || p.pulls.Creates != 1 || p.pulls.Updates != 0 || p.pulls.Readies != 0 {
				t.Fatalf("a stale approval touched the fork or pull requests: %d finds, %d creates, %d updates, %d ready calls", p.pulls.Finds, p.pulls.Creates, p.pulls.Updates, p.pulls.Readies)
			}
			if got := p.feature(t); got != AssembledState {
				t.Fatalf("the workstream is %s", got)
			}
			transition, _ := publishIDs(1)
			if body := noticeOf(t, p.repository, p.stream, transition+"-refused"); !strings.Contains(body, "not marked ready") {
				t.Fatalf("refusal notice %q", body)
			}
		})
	}
}

// Marking ready with an edited description after an interruption at any step
// leaves one pull request, one description update and one ready call, and
// records one durable result.
func TestMarkingReadyRecoveryRecordsOneDurableResult(t *testing.T) {
	t.Parallel()
	for _, point := range []string{"ready-described", "ready-marked", "after-trace"} {
		t.Run(point, func(t *testing.T) {
			t.Parallel()
			p := newPublicationFixture(t, "squash")
			edited := "# Edited\n"
			p.approve(t, &edited)
			op := p.request(t)
			p.s.boundary = func(step string) error {
				if step == point {
					return errors.New("interrupted")
				}
				return nil
			}
			result, err := p.publisher().Apply(context.Background(), op)
			if point == "after-trace" {
				if err != nil || result.Outcome != "succeeded" {
					t.Fatalf("delivery %+v: %v", result, err)
				}
			} else if err == nil {
				t.Fatal("marking ready was not interrupted")
			}
			p.reopen(t)
			p.s.boundary = nil
			result = settleOperation(t, p.s, p.repository, p.stream, op, p.publisher())
			if result.Outcome != "succeeded" || p.pulls.Creates != 1 || p.pulls.Updates != 1 || p.pulls.Readies != 1 || p.pulls.Prs[0].Draft || p.feature(t) != DeliveredState {
				t.Fatalf("recovered %+v, creates %d, updates %d, ready calls %d, state %s", result, p.pulls.Creates, p.pulls.Updates, p.pulls.Readies, p.feature(t))
			}
			ops, err := p.repository.Operations(p.stream)
			must(t, err)
			i := slices.IndexFunc(ops, func(record trace.OperationRecord) bool { return record.Operation.ID == op.ID })
			if i < 0 || ops[i].Result == nil || ops[i].Result.Outcome != "succeeded" {
				t.Fatalf("publish operation result %+v", ops)
			}
			if got := streamDocuments(t, p.repository, p.stream, publicationDocument); len(got) > 2 {
				t.Fatalf("duplicate publication records: %d", len(got))
			}
			if got := streamDocuments(t, p.repository, p.stream, delivery.ReadyDocument); len(got) != 1 {
				t.Fatalf("ready records: %d", len(got))
			}
		})
	}
}

func TestMarkingReadyRefusesConflictingRemoteStateUntilApprovedAgain(t *testing.T) {
	t.Parallel()
	t.Run("foreign branch", func(t *testing.T) {
		t.Parallel()
		p := newPublicationFixture(t, "commit-per-unit")
		ctx := context.Background()
		p.approve(t, nil)
		foreign := p.report.Upstream.Commit
		demoGit(t, filepath.Dir(p.fork), "-C", p.clone, "push", "--quiet", "--force", "origin", foreign+":refs/heads/"+featureBranch(p.stream))
		p.pulls.Prs[0].HeadCommit = foreign
		op := p.request(t)
		result, err := p.publisher().Apply(ctx, op)
		if err != nil || result.Outcome != "failed" || !strings.Contains(result.Evidence, "no push of this workstream made") {
			t.Fatalf("publication %+v: %v", result, err)
		}
		if tip, _ := p.forkBranch(t); tip != foreign || p.pulls.Creates != 1 || p.pulls.Readies != 0 || p.feature(t) != AssembledState {
			t.Fatalf("the fork branch is at %s; %d creates; %d ready calls; the workstream is %s", tip, p.pulls.Creates, p.pulls.Readies, p.feature(t))
		}
		must(t, p.publisher().Pass(ctx))
		if ops := p.operations(t); len(ops) != 1 {
			t.Fatalf("asked again without a new approval: %d", len(ops))
		}
		demoGit(t, filepath.Dir(p.fork), "-C", p.clone, "push", "--quiet", "--force", "origin", p.pushed+":refs/heads/"+featureBranch(p.stream))
		p.pulls.Prs[0].HeadCommit = p.pushed
		if again := p.approve(t, nil); again.Commit != p.report.Commit {
			t.Fatalf("approval %+v", again)
		}
		if docs := streamDocuments(t, p.repository, p.stream, deliveryDocument); len(docs) != 2 {
			t.Fatalf("approving after a refusal recorded %d approvals", len(docs))
		}
		retry := p.request(t)
		result, err = p.publisher().Apply(ctx, retry)
		if err != nil || result.Outcome != "succeeded" || p.feature(t) != DeliveredState {
			t.Fatalf("publication after approving again %+v: %v", result, err)
		}
	})
	t.Run("different pull request", func(t *testing.T) {
		t.Parallel()
		p := newPublicationFixture(t, "commit-per-unit")
		ctx := context.Background()
		p.approve(t, nil)
		own := p.pulls.Prs[0]
		p.pulls.Prs = []pulls.PullRequest{{Number: 7, URL: "https://github.com/dagger/dagger/pull/7", State: "open", Head: featureBranch(p.stream), HeadRepository: "owner/dagger", HeadCommit: p.pushed, Base: "main", Body: "Someone else's description"}}
		op := p.request(t)
		result, err := p.publisher().Apply(ctx, op)
		if err != nil || result.Outcome != "failed" || !strings.Contains(result.Evidence, fmt.Sprintf("#%d", own.Number)) {
			t.Fatalf("publication %+v: %v", result, err)
		}
		if tip, _ := p.forkBranch(t); tip != p.pushed || p.pulls.Creates != 1 || p.pulls.Readies != 0 || p.feature(t) != AssembledState {
			t.Fatalf("the fork branch is at %s; %d creates; %d ready calls; the workstream is %s", tip, p.pulls.Creates, p.pulls.Readies, p.feature(t))
		}
	})
}

func TestServicePublishesAnApprovedWorkstreamAfterRestart(t *testing.T) {
	t.Parallel()
	p := newPublicationFixture(t, "commit-per-unit")
	approval := p.approve(t, nil)
	p.repository.Close()
	p.opts.PullRequests = p.pulls
	p.start(t)
	defer p.stop(t)
	p.awaitFeature(t, p.stream, DeliveredState)
	if tip, _ := p.forkBranch(t); tip == p.report.Commit || p.pulls.Creates != 1 || p.pulls.Prs[0].Body != approval.Description {
		t.Fatalf("the fork branch is at %s; pull requests %+v", tip, p.pulls.Prs)
	}
	transition, _ := publishIDs(1)
	if got := transitionByID(t, p.shedFixture.repository(), p.stream, transition+"-published"); got.To != "published-1" {
		t.Fatalf("publication outcome %+v", got)
	}
}

func assertDeliveryTree(t *testing.T, p *publicationFixture, tip string) {
	t.Helper()
	tree := demoGit(t, filepath.Dir(p.clone), "-C", p.clone, "show", "-s", "--format=%T", tip)
	reviewed := demoGit(t, filepath.Dir(p.clone), "-C", p.clone, "show", "-s", "--format=%T", p.report.Commit)
	if tree != reviewed {
		t.Fatal("publication changed reviewed tree")
	}
}

// A draft publication whose commits cannot be signed pushes nothing, and
// publishes once they can.
func TestDraftPublicationSigningFailureBlocksPushUntilSigned(t *testing.T) {
	t.Parallel()
	p := newUnpushedPublicationFixture(t, "squash", "")
	demoGit(t, filepath.Dir(p.clone), "-C", p.clone, "config", "gpg.program", "false")
	reason, err := p.tryPublishDraft(t)
	if err != nil || !strings.Contains(reason, "pre-publication amendment failed") {
		t.Fatalf("signing failure: %q %v", reason, err)
	}
	if _, exists := p.forkBranch(t); exists || p.pulls.Creates != 0 {
		t.Fatal("signing failure touched publication")
	}
	if p.feature(t) != AssembledState {
		t.Fatal("signing failure delivered the workstream")
	}
	configureDeliverySigner(t, p.clone)
	p.publishDraft(t)
	p.approve(t, nil)
	result, err := p.publisher().Apply(context.Background(), p.request(t))
	if err != nil || result.Outcome != "succeeded" {
		t.Fatalf("retry: %+v %v", result, err)
	}
}

// TestRepeatedPublicationFailuresReachTheInbox shows a publication the host
// keeps refusing backing off and raised to the inbox with the host's
// explanation, and the entry closing once an attempt publishes.
func TestRepeatedPublicationFailuresReachTheInbox(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := newPublicationFixture(t, "squash")
	p.approve(t, nil)
	in, err := decodePublish(p.request(t))
	must(t, err)
	refusal := "find pull requests of acme/dagger from owner/dagger:osmia/w: GitHub answered 403: Resource not accessible by personal access token; the request needs the token permissions pull_requests=read"
	p.pulls.FindErr = errors.New(refusal)
	// The controller's clock moves only as the test advances it, past every
	// time the fixture recorded.
	clock := &fixedClock{At: p.clock.Now().Add(time.Hour)}
	c, err := reconcile.New(p.repository, reconcile.Options{Worker: "test", Now: clock.Now, RetryDelay: time.Minute,
		Adapters: map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RepositoryBoundary: p.publisher()},
		Hold:     func(_ config.WorkstreamID, op coreadapter.Operation) bool { return op.Action != PublishAction }})
	must(t, err)
	publication := func() (InboxEntry, bool) {
		t.Helper()
		inbox, api := p.s.inbox(ctx)
		if api != nil {
			t.Fatal(api)
		}
		for _, e := range inbox.Entries {
			if e.Kind == InboxPublication {
				return e, true
			}
		}
		return InboxEntry{}, false
	}
	var firstFailure time.Time
	for attempt, wait := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute} {
		must(t, c.Pass(ctx))
		if attempt == 0 {
			firstFailure = clock.Now()
		}
		e, found := publication()
		if attempt < publicationEscalation-1 {
			if found {
				t.Fatalf("raised after %d failures: %+v", attempt+1, e)
			}
		} else {
			want := fmt.Sprintf("Marking the pull request ready for review (owner approval %d) has failed 3 times in a row. Last failure: Effect returned error: %s", in.Approval, refusal)
			retryAt := clock.Now().Add(wait).UTC().Format(time.RFC3339)
			if !found || e.Workstream != p.stream || e.Question != want || len(e.Options) != 0 || e.Revision != in.Approval ||
				!e.OpenedAt.Equal(firstFailure) || !strings.Contains(e.Blocked, "tries again at "+retryAt) || e.Answer.Method != http.MethodGet {
				t.Fatalf("publication entry %+v, want question %q and a retry at %s", e, want, retryAt)
			}
		}
		// The next attempt waits twice as long as the one before.
		clock.Advance(wait - time.Second)
		must(t, c.Pass(ctx))
		if p.pulls.Finds-p.finds != attempt+1 {
			t.Fatalf("attempted %d times before the retry was due, want %d", p.pulls.Finds-p.finds, attempt+1)
		}
		clock.Advance(time.Second)
	}
	p.pulls.FindErr = nil
	must(t, c.Pass(ctx))
	if p.feature(t) != DeliveredState || p.pulls.Creates != 1 {
		t.Fatalf("state %s after %d pull requests", p.feature(t), p.pulls.Creates)
	}
	if e, found := publication(); found {
		t.Fatalf("the published workstream stays raised: %+v", e)
	}
}

// TestPublicationWaitsForAGitHubToken shows a service without a credential
// holding the marking ready of a pushed draft without changing or asking the
// host anything, raising it to the inbox at once, and marking it ready once
// it has one.
func TestPublicationWaitsForAGitHubToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := newPublicationFixture(t, "squash")
	p.approve(t, nil)
	op := p.request(t)
	in, err := decodePublish(op)
	must(t, err)
	p.pulls.Mu.Lock()
	p.pulls.Uncredentialed = true
	p.pulls.Mu.Unlock()
	clock := &fixedClock{At: p.clock.Now().Add(time.Hour)}
	c, err := reconcile.New(p.repository, reconcile.Options{Worker: "test", Now: clock.Now, RetryDelay: time.Minute,
		Adapters: map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RepositoryBoundary: p.publisher()},
		Hold:     p.s.holding(p.repository)})
	must(t, err)
	for range 3 {
		must(t, c.Pass(ctx))
		clock.Advance(time.Hour)
	}
	if tip, _ := p.forkBranch(t); tip != p.pushed || p.pulls.Finds != p.finds || p.pulls.Creates != 1 || p.pulls.Readies != 0 {
		t.Fatalf("a publication without a credential left the branch at %s, found %d times, opened %d pull requests and marked %d ready", tip, p.pulls.Finds-p.finds, p.pulls.Creates, p.pulls.Readies)
	}
	ops, err := p.repository.Operations(p.stream)
	must(t, err)
	i := slices.IndexFunc(ops, func(r trace.OperationRecord) bool { return r.Operation.ID == op.ID })
	if i < 0 || len(ops[i].History) != 0 {
		t.Fatalf("the held publication was attempted: %+v", ops)
	}
	requested := ops[i].Transition.At
	inbox, api := p.s.inbox(ctx)
	if api != nil {
		t.Fatal(api)
	}
	i = slices.IndexFunc(inbox.Entries, func(e InboxEntry) bool { return e.Kind == InboxPublication })
	if i < 0 {
		t.Fatalf("the held publication is not in the inbox: %+v", inbox.Entries)
	}
	e := inbox.Entries[i]
	if !strings.Contains(e.Question, "waits for a GitHub token") || !strings.Contains(e.Blocked, "Restart osmia serve with GITHUB_TOKEN set") ||
		!strings.Contains(e.Blocked, in.Upstream) || !e.OpenedAt.Equal(requested) || len(e.Options) != 0 {
		t.Fatalf("held publication entry %+v", e)
	}
	// A direct attempt is refused before any side effect.
	if _, err := p.publisher().Apply(ctx, op); !errors.Is(err, errNoPullRequestCredential) {
		t.Fatalf("an attempt without a credential: %v", err)
	}
	if tip, _ := p.forkBranch(t); tip != p.pushed || p.pulls.Finds != p.finds {
		t.Fatal("a refused attempt pushed or asked the host")
	}
	p.pulls.Mu.Lock()
	p.pulls.Uncredentialed = false
	p.pulls.Mu.Unlock()
	must(t, c.Pass(ctx))
	if p.feature(t) != DeliveredState || p.pulls.Creates != 1 {
		t.Fatalf("state %s after %d pull requests", p.feature(t), p.pulls.Creates)
	}
	inbox, api = p.s.inbox(ctx)
	if api != nil {
		t.Fatal(api)
	}
	if slices.ContainsFunc(inbox.Entries, func(e InboxEntry) bool { return e.Kind == InboxPublication }) {
		t.Fatal("the published workstream stays raised")
	}
}

// Commit messages are fixed when the commits are pushed to the pull request,
// so an approval that edits them is refused and records nothing.
func TestApprovingRefusesEditedCommitMessages(t *testing.T) {
	t.Parallel()
	p := newPublicationFixture(t, "squash")
	view, api := p.s.deliveryPresentation(context.Background(), string(p.stream))
	if api != nil {
		t.Fatal(api)
	}
	messages := slices.Clone(view.Messages)
	messages[0].Message = "Owner's subject\n\nOwner's explanation.\n"
	_, api = p.s.approveDelivery(context.Background(), string(p.stream), DeliveryDecision{Review: view.Report.Review, ReviewRevision: view.ReviewRevision, Commit: view.Report.Commit, DraftHash: view.DraftHash, Messages: messages})
	if api == nil || api.Code != Validation {
		t.Fatalf("edited commit messages: %v", api)
	}
	if docs := streamDocuments(t, p.repository, p.stream, deliveryDocument); len(docs) != 0 {
		t.Fatalf("an approval was recorded: %+v", docs)
	}
}
