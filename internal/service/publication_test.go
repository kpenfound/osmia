package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/trace"
)

// fakePulls is a pull request host in memory, standing in for the real
// remote hosting the delivery publishes to. lose makes the next Create open
// the pull request and then fail, as a response lost in transit does. It
// leaves unverified the real host's network behaviour, authentication and
// actual merge or close semantics.
type fakePulls struct {
	mu       sync.Mutex
	prs      []pulls.PullRequest
	finds    int
	syncHead bool
	updates  int
	creates  int
	lose     bool
	fork     func() string
	// findErr, when set, fails every Find as a refusing host does.
	findErr error
	// uncredentialed reports the client holding no credential, as GitHub
	// without a token does.
	uncredentialed bool
}

func (c *fakePulls) Credentialed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.uncredentialed
}

func (c *fakePulls) Find(_ context.Context, repository, headRepository, branch string) ([]pulls.PullRequest, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.finds++
	if c.findErr != nil {
		return nil, c.findErr
	}
	var out []pulls.PullRequest
	for _, pr := range c.prs {
		if pr.HeadRepository == headRepository && pr.Head == branch {
			if c.syncHead {
				pr.HeadCommit = c.fork()
			}
			out = append(out, pr)
		}
	}
	return out, nil
}

func (c *fakePulls) Create(_ context.Context, repository string, n pulls.New) (pulls.PullRequest, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.creates++
	number := 100 + len(c.prs)
	pr := pulls.PullRequest{Number: number, URL: fmt.Sprintf("https://github.com/%s/pull/%d", repository, number), State: "open", Head: n.Head, HeadRepository: n.HeadRepository, HeadCommit: c.fork(), Base: n.Base, Title: n.Title, Body: n.Body}
	c.prs = append(c.prs, pr)
	if c.lose {
		c.lose = false
		return pulls.PullRequest{}, errors.New("connection reset")
	}
	return pr, nil
}

type publicationFixture struct {
	*shedFixture
	stream     config.WorkstreamID
	repository *trace.Repository
	report     FinalReport
	fork       string
	pulls      *fakePulls
}

// newPublicationFixture reviews an assembled workstream, gives the clone a
// local bare fork of owner/dagger as its origin remote and a fake pull
// request host, and configures the delivery style.
func newPublicationFixture(t *testing.T, style string) *publicationFixture {
	t.Helper()
	f, stream, repository, report := deliveryFixture(t)
	home := filepath.Dir(f.clone)
	fork := filepath.Join(home, "remotes", "owner", "dagger.git")
	must(t, os.MkdirAll(filepath.Dir(fork), 0700))
	demoGit(t, home, "init", "--quiet", "--bare", fork)
	demoGit(t, home, "-C", f.clone, "remote", "add", "origin", fork)
	p := &publicationFixture{shedFixture: f, stream: stream, repository: repository, report: report, fork: fork}
	p.pulls = &fakePulls{fork: func() string { commit, _ := p.forkBranch(t); return commit }}
	f.s.mu.Lock()
	f.s.cfg.Project.Landing = style
	f.s.options.PullRequests = p.pulls
	f.s.mu.Unlock()
	return p
}

// forkBranch returns the commit the fork's delivery branch is at, and
// whether it exists.
func (p *publicationFixture) forkBranch(t *testing.T) (string, bool) {
	t.Helper()
	out := strings.TrimSpace(demoGit(t, filepath.Dir(p.fork), "-C", p.fork, "for-each-ref", "--format=%(objectname)", "refs/heads/"+featureBranch(p.stream)))
	return out, out != ""
}

// approve records the owner's approval of the presented draft, or of the
// given description.
func (p *publicationFixture) approve(t *testing.T, description *string) DeliveryApproval {
	t.Helper()
	presented, api := p.s.deliveryPresentation(context.Background(), string(p.stream))
	if api != nil {
		t.Fatal(api)
	}
	approval, api := p.s.approveDelivery(context.Background(), string(p.stream), DeliveryDecision{Review: presented.Report.Review, ReviewRevision: presented.ReviewRevision, Commit: presented.Report.Commit, DraftHash: presented.DraftHash, Description: description})
	if api != nil {
		t.Fatal(api)
	}
	return approval
}

func (p *publicationFixture) publisher() *publisher {
	return &publisher{s: p.s, repository: p.repository}
}

// operations returns the workstream's publish operations, in request order.
func (p *publicationFixture) operations(t *testing.T) []coreadapter.Operation {
	t.Helper()
	ops, err := p.repository.Operations(p.stream)
	must(t, err)
	ops = slices.DeleteFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.Action != PublishAction })
	slices.SortFunc(ops, func(a, b trace.OperationRecord) int { return a.Transition.At.Compare(b.Transition.At) })
	var out []coreadapter.Operation
	for _, o := range ops {
		out = append(out, o.Operation)
	}
	return out
}

// request runs the publication controller's pass and returns the one
// publish operation it asked for.
func (p *publicationFixture) request(t *testing.T) coreadapter.Operation {
	t.Helper()
	before := len(p.operations(t))
	must(t, p.publisher().Pass(context.Background()))
	ops := p.operations(t)
	if len(ops) != before+1 {
		t.Fatalf("the pass asked for %d publications, want one more than %d", len(ops), before)
	}
	return ops[len(ops)-1]
}

func (p *publicationFixture) feature(t *testing.T) string {
	t.Helper()
	state, err := p.repository.Workflow(p.stream, trace.FeatureSubject)
	must(t, err)
	return state.Value
}

// reopen closes the trace and opens it again, as a restarted service does.
func (p *publicationFixture) reopen(t *testing.T) {
	t.Helper()
	p.repository.Close()
	reopened, err := trace.Open(p.s.cfg.Root, p.s.cfg.Project)
	must(t, err)
	t.Cleanup(func() { reopened.Close() })
	p.repository = reopened
	p.s.setSole(&activeProject{repository: reopened})
}

func TestPublicationCommitPerUnitSignsTheReviewedHistoryAndOpensOnePullRequest(t *testing.T) {
	t.Parallel()
	p := newPublicationFixture(t, "commit-per-unit")
	ctx := context.Background()
	must(t, p.publisher().Pass(ctx))
	if ops := p.operations(t); len(ops) != 0 {
		t.Fatalf("published without an owner approval: %+v", ops)
	}
	approval := p.approve(t, nil)
	op := p.request(t)
	must(t, p.publisher().Pass(ctx))
	if ops := p.operations(t); len(ops) != 1 {
		t.Fatalf("asked twice for one approval: %d", len(ops))
	}
	result, err := p.publisher().Apply(ctx, op)
	if err != nil || result.Outcome != "succeeded" {
		t.Fatalf("publication %+v: %v", result, err)
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
	if p.pulls.creates != 1 || len(p.pulls.prs) != 1 {
		t.Fatalf("opened %d pull requests", p.pulls.creates)
	}
	pr := p.pulls.prs[0]
	if pr.Body != approval.Description || pr.Base != "main" || pr.Head != featureBranch(p.stream) || pr.HeadRepository != "owner/dagger" || pr.Title != "Resumable uploads" {
		t.Fatalf("pull request %+v", pr)
	}
	if got := p.feature(t); got != DeliveredState {
		t.Fatalf("the workstream is %s, want delivered", got)
	}
	recorded, err := publications(p.repository, p.stream)
	must(t, err)
	last := recorded[len(recorded)-1]
	want := DeliveryPublication{Approval: 1, Operation: op.ID, Status: publicationOpened, Style: "commit-per-unit", Fork: "owner/dagger", Remote: "origin", Branch: featureBranch(p.stream), Reviewed: p.report.Commit, Commit: tip,
		Upstream: "dagger/dagger", Base: "main", PullRequest: pr.Number, URL: pr.URL, Title: "Resumable uploads", Description: approval.Description, DescriptionHash: approval.DescriptionHash}
	if last != want {
		t.Fatalf("publication %+v, want %+v", last, want)
	}
	if body := noticeOf(t, p.repository, p.stream, DeliveredState); !strings.Contains(body, pr.URL) {
		t.Fatalf("delivery notice %q", body)
	}
	again, err := p.publisher().Apply(ctx, op)
	if err != nil || again.Outcome != "succeeded" || p.pulls.creates != 1 {
		t.Fatalf("a retry of a recorded publication %+v, %v, %d pull requests", again, err, p.pulls.creates)
	}
	must(t, p.publisher().Pass(ctx))
	if ops := p.operations(t); len(ops) != 1 {
		t.Fatalf("asked to publish a delivered workstream: %d", len(ops))
	}
	presented, api := p.s.deliveryPresentation(ctx, string(p.stream))
	if api != nil || !presented.Delivered || presented.Publication == nil || *presented.Publication != want {
		t.Fatalf("delivered presentation %+v: %v", presented, api)
	}
	if _, api := p.s.approveDelivery(ctx, string(p.stream), DeliveryDecision{Review: 1, ReviewRevision: 1, Commit: p.report.Commit, DraftHash: approval.DraftHash}); api == nil || api.Code != Conflict {
		t.Fatalf("approved a delivered workstream: %v", api)
	}
}

func TestPublicationSquashDeliversOneCommitWithTheReviewedTree(t *testing.T) {
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
	if p.pulls.prs[0].HeadCommit != tip || p.feature(t) != DeliveredState {
		t.Fatalf("pull request %+v, workstream %s", p.pulls.prs[0], p.feature(t))
	}
	recorded, err := publications(p.repository, p.stream)
	must(t, err)
	if last := recorded[len(recorded)-1]; last.Style != "squash" || last.Commit != tip || last.Reviewed != p.report.Commit {
		t.Fatalf("publication %+v", last)
	}
}

func TestPublicationRefusesAStaleApprovalBeforeAnySideEffect(t *testing.T) {
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
			if _, exists := p.forkBranch(t); exists || p.pulls.finds != 0 || p.pulls.creates != 0 {
				t.Fatalf("a stale approval touched the fork or pull requests: %d finds, %d creates", p.pulls.finds, p.pulls.creates)
			}
			if got := p.feature(t); got != AssembledState {
				t.Fatalf("the workstream is %s", got)
			}
			transition, _ := publishIDs(1)
			if body := noticeOf(t, p.repository, p.stream, transition+"-refused"); !strings.Contains(body, "not published") {
				t.Fatalf("refusal notice %q", body)
			}
		})
	}
}

func TestPublicationResumesInterruptedPushAndPullRequestCreationAcrossRestart(t *testing.T) {
	t.Parallel()
	p := newPublicationFixture(t, "squash")
	ctx := context.Background()
	p.approve(t, nil)
	op := p.request(t)
	stop := "publish-pushed"
	p.s.boundary = func(step string) error {
		if step == stop {
			return errors.New("stopped at " + step)
		}
		return nil
	}
	if _, err := p.publisher().Apply(ctx, op); err == nil {
		t.Fatal("the push was not interrupted")
	}
	pushed, exists := p.forkBranch(t)
	if !exists || len(p.pulls.prs) != 0 {
		t.Fatalf("interrupted after the push: branch %q, pull requests %+v", pushed, p.pulls.prs)
	}
	recorded, err := publications(p.repository, p.stream)
	must(t, err)
	if len(recorded) != 1 || recorded[0].Status != publicationPushing || recorded[0].Commit != pushed {
		t.Fatalf("publication before the push %+v", recorded)
	}
	p.reopen(t)
	stop = "publish-opened"
	if _, err := p.publisher().Apply(ctx, op); err == nil {
		t.Fatal("the pull request creation was not interrupted")
	}
	if tip, _ := p.forkBranch(t); tip != pushed || p.pulls.creates != 1 || p.feature(t) != AssembledState {
		t.Fatalf("the retry pushed %s over %s, opened %d pull requests, and the workstream is %s", tip, pushed, p.pulls.creates, p.feature(t))
	}
	p.reopen(t)
	stop = ""
	result, err := p.publisher().Apply(ctx, op)
	if err != nil || result.Outcome != "succeeded" {
		t.Fatalf("publication %+v: %v", result, err)
	}
	if p.pulls.creates != 1 || len(p.pulls.prs) != 1 || p.feature(t) != DeliveredState {
		t.Fatalf("opened %d pull requests; the workstream is %s", p.pulls.creates, p.feature(t))
	}
	recorded, err = publications(p.repository, p.stream)
	must(t, err)
	if len(recorded) != 2 || recorded[1].PullRequest != p.pulls.prs[0].Number || recorded[1].Commit != pushed {
		t.Fatalf("publications %+v", recorded)
	}
}

func TestPublicationRecoveryRecordsOneDurableResult(t *testing.T) {
	t.Parallel()
	for _, point := range []string{"publish-recorded", "publish-pushed", "publish-opened", "after-trace"} {
		t.Run(point, func(t *testing.T) {
			t.Parallel()
			p := newPublicationFixture(t, "squash")
			p.approve(t, nil)
			op := p.request(t)
			pushIntents := 0
			p.s.boundary = func(step string) error {
				if step == "publish-recorded" {
					pushIntents++
				}
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
				t.Fatal("publication was not interrupted")
			}
			p.reopen(t)
			p.s.boundary = func(step string) error {
				if step == "publish-recorded" {
					pushIntents++
				}
				return nil
			}
			result = settleOperation(t, p.s, p.repository, p.stream, op, p.publisher())
			if result.Outcome != "succeeded" || p.pulls.creates != 1 || p.feature(t) != DeliveredState {
				t.Fatalf("recovered %+v, creates %d, state %s", result, p.pulls.creates, p.feature(t))
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
			if point == "publish-opened" {
				recorded, err := publications(p.repository, p.stream)
				must(t, err)
				if len(recorded) != 2 || recorded[1].PullRequest != p.pulls.prs[0].Number || recorded[1].Status != publicationOpened {
					t.Fatalf("publications %+v", recorded)
				}
			}
			wantPushIntents := 1
			if point == "publish-recorded" {
				wantPushIntents = 2
			}
			if pushIntents != wantPushIntents {
				t.Fatalf("push attempts %d, want %d", pushIntents, wantPushIntents)
			}
		})
	}
}

func TestPublicationVerifiesHeadAfterAutomaticPRAdvance(t *testing.T) {
	t.Parallel()
	p := newPublicationFixture(t, "squash")
	p.approve(t, nil)
	first := p.request(t)
	p.pulls.prs = []pulls.PullRequest{{Number: 7, URL: "https://github.com/dagger/dagger/pull/7", State: "closed", Head: featureBranch(p.stream), HeadRepository: "owner/dagger", Base: "main"}}
	if result, err := p.publisher().Apply(context.Background(), first); err != nil || result.Outcome != "failed" {
		t.Fatalf("first publication %+v: %v", result, err)
	}
	old, _ := p.forkBranch(t)
	p.pulls.prs = nil
	newDescription := "# A newly approved description\n"
	p.approve(t, &newDescription)
	second := p.request(t)
	p.pulls.prs = []pulls.PullRequest{{Number: 8, URL: "https://github.com/dagger/dagger/pull/8", State: "open", Head: featureBranch(p.stream), HeadRepository: "owner/dagger", HeadCommit: old, Base: "main", Body: newDescription}}
	p.s.boundary = func(step string) error {
		if step == "publish-pushed" {
			p.pulls.prs[0].HeadCommit, _ = p.forkBranch(t)
			return errors.New("interrupted after GitHub advanced the PR head")
		}
		return nil
	}
	if _, err := p.publisher().Apply(context.Background(), second); err == nil {
		t.Fatal("the publication was not interrupted")
	}
	p.reopen(t)
	p.s.boundary = nil
	result := settleOperation(t, p.s, p.repository, p.stream, second, p.publisher())
	if result.Outcome != "succeeded" || p.pulls.creates != 0 || p.pulls.prs[0].HeadCommit == old {
		t.Fatalf("publication %+v, creates %d, PR %+v", result, p.pulls.creates, p.pulls.prs[0])
	}
}

func TestPublicationRefusesConflictingRemoteStateUntilApprovedAgain(t *testing.T) {
	t.Parallel()
	t.Run("foreign branch", func(t *testing.T) {
		t.Parallel()
		p := newPublicationFixture(t, "commit-per-unit")
		ctx := context.Background()
		p.approve(t, nil)
		foreign := p.report.Upstream.Commit
		demoGit(t, filepath.Dir(p.fork), "-C", p.clone, "push", "--quiet", "origin", foreign+":refs/heads/"+featureBranch(p.stream))
		op := p.request(t)
		result, err := p.publisher().Apply(ctx, op)
		if err != nil || result.Outcome != "failed" || !strings.Contains(result.Evidence, "no publication of this workstream pushed") {
			t.Fatalf("publication %+v: %v", result, err)
		}
		if tip, _ := p.forkBranch(t); tip != foreign || p.pulls.creates != 0 || p.feature(t) != AssembledState {
			t.Fatalf("the fork branch is at %s; %d creates; the workstream is %s", tip, p.pulls.creates, p.feature(t))
		}
		must(t, p.publisher().Pass(ctx))
		if ops := p.operations(t); len(ops) != 1 {
			t.Fatalf("asked again without a new approval: %d", len(ops))
		}
		demoGit(t, filepath.Dir(p.fork), "-C", p.fork, "branch", "-D", featureBranch(p.stream))
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
		p.pulls.prs = []pulls.PullRequest{{Number: 7, URL: "https://github.com/dagger/dagger/pull/7", State: "open", Head: featureBranch(p.stream), HeadRepository: "owner/dagger", HeadCommit: p.report.Commit, Base: "main", Body: "Someone else's description"}}
		op := p.request(t)
		result, err := p.publisher().Apply(ctx, op)
		if err != nil || result.Outcome != "failed" || !strings.Contains(result.Evidence, "#7") {
			t.Fatalf("publication %+v: %v", result, err)
		}
		if tip, exists := p.forkBranch(t); exists || p.pulls.creates != 0 || p.feature(t) != AssembledState {
			t.Fatalf("the fork branch is at %s; %d creates; the workstream is %s", tip, p.pulls.creates, p.feature(t))
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
	if tip, _ := p.forkBranch(t); tip == p.report.Commit || p.pulls.creates != 1 || p.pulls.prs[0].Body != approval.Description {
		t.Fatalf("the fork branch is at %s; pull requests %+v", tip, p.pulls.prs)
	}
	transition, _ := publishIDs(1)
	if got := transitionByID(t, p.shedFixture.repository(), p.stream, transition+"-published"); got.To != "published-1" {
		t.Fatalf("publication outcome %+v", got)
	}
}

func configureDeliverySigner(t *testing.T, clone string) string {
	t.Helper()
	signer := filepath.Join(t.TempDir(), "fake-gpg")
	must(t, os.WriteFile(signer, []byte("#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '-----BEGIN PGP SIGNATURE-----' 'fixture signature' '-----END PGP SIGNATURE-----'\nprintf '%s\\n' '[GNUPG:] SIG_CREATED D 1 10 00 0 FAKE' >&2\n"), 0700))
	for k, v := range map[string]string{"user.name": "Delivery Owner", "user.email": "delivery@example.invalid", "user.signingkey": "fixture-key", "gpg.format": "openpgp", "gpg.program": signer, "commit.gpgsign": "false"} {
		demoGit(t, filepath.Dir(clone), "-C", clone, "config", k, v)
	}
	return signer
}

func assertDeliveryTree(t *testing.T, p *publicationFixture, tip string) {
	t.Helper()
	tree := demoGit(t, filepath.Dir(p.clone), "-C", p.clone, "show", "-s", "--format=%T", tip)
	reviewed := demoGit(t, filepath.Dir(p.clone), "-C", p.clone, "show", "-s", "--format=%T", p.report.Commit)
	if tree != reviewed {
		t.Fatal("publication changed reviewed tree")
	}
}

func TestPublicationSigningFailureBlocksPushUntilApprovedAgain(t *testing.T) {
	t.Parallel()
	p := newPublicationFixture(t, "squash")
	p.approve(t, nil)
	demoGit(t, filepath.Dir(p.clone), "-C", p.clone, "config", "gpg.program", "false")
	result, err := p.publisher().Apply(context.Background(), p.request(t))
	if err != nil || result.Outcome != "failed" {
		t.Fatalf("signing failure: %+v %v", result, err)
	}
	if _, exists := p.forkBranch(t); exists || p.pulls.finds != 0 || p.pulls.creates != 0 {
		t.Fatal("signing failure touched publication")
	}
	if p.feature(t) != AssembledState {
		t.Fatal("signing failure delivered the workstream")
	}
	configureDeliverySigner(t, p.clone)
	p.approve(t, nil)
	result, err = p.publisher().Apply(context.Background(), p.request(t))
	if err != nil || result.Outcome != "succeeded" {
		t.Fatalf("retry: %+v %v", result, err)
	}
}

func TestPublicationReusesSignedCommitBeforePushAfterRestart(t *testing.T) {
	t.Parallel()
	p := newPublicationFixture(t, "squash")
	p.approve(t, nil)
	op := p.request(t)
	p.s.boundary = func(step string) error {
		if step == "publish-recorded" {
			return errors.New("interrupted")
		}
		return nil
	}
	if _, err := p.publisher().Apply(context.Background(), op); err == nil {
		t.Fatal("publication was not interrupted")
	}
	records, err := publications(p.repository, p.stream)
	must(t, err)
	if len(records) != 1 {
		t.Fatalf("records: %+v", records)
	}
	signed := records[0].Commit
	if _, exists := p.forkBranch(t); exists {
		t.Fatal("pushed before durable record boundary")
	}
	p.reopen(t)
	p.s.boundary = nil
	demoGit(t, filepath.Dir(p.clone), "-C", p.clone, "config", "gpg.program", "false")
	result, err := p.publisher().Apply(context.Background(), op)
	if err != nil || result.Outcome != "succeeded" {
		t.Fatalf("recovery: %+v %v", result, err)
	}
	if tip, _ := p.forkBranch(t); tip != signed {
		t.Fatal("recovery replaced the signed commit")
	}
}

func TestPublicationUsesOwnerMessageAndRejectsSupersededMessage(t *testing.T) {
	t.Parallel()
	p := newPublicationFixture(t, "squash")
	p.approve(t, nil)
	stale := p.request(t)
	view, api := p.s.deliveryPresentation(context.Background(), string(p.stream))
	if api != nil {
		t.Fatal(api)
	}
	messages := slices.Clone(view.Messages)
	messages[0].Message = "Owner's subject\n\nOwner's explanation.\n\nCo-authored-by: Claude <noreply@anthropic.com>\nCo-authored-by: Human <human@example.invalid>"
	_, api = p.s.approveDelivery(context.Background(), string(p.stream), DeliveryDecision{Review: view.Report.Review, ReviewRevision: view.ReviewRevision, Commit: view.Report.Commit, DraftHash: view.DraftHash, Messages: messages})
	if api != nil {
		t.Fatal(api)
	}
	result, err := p.publisher().Apply(context.Background(), stale)
	if err != nil || result.Outcome != "failed" {
		t.Fatalf("stale message published: %+v %v", result, err)
	}
	result, err = p.publisher().Apply(context.Background(), p.request(t))
	if err != nil || result.Outcome != "succeeded" {
		t.Fatalf("publish: %+v %v", result, err)
	}
	tip, _ := p.forkBranch(t)
	msg := demoGit(t, filepath.Dir(p.clone), "-C", p.clone, "show", "-s", "--format=%B", tip)
	if !strings.HasPrefix(msg, "Owner's subject\n") || strings.Contains(msg, "noreply@anthropic.com") || !strings.Contains(msg, "Co-authored-by: Human") || strings.Count(msg, "Signed-off-by: Delivery Owner <delivery@example.invalid>") != 1 {
		t.Fatalf("message: %s", msg)
	}
	assertDeliveryTree(t, p, tip)
}

func (c *fakePulls) Update(_ context.Context, repository string, number int, change pulls.Update) (pulls.PullRequest, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updates++
	for i := range c.prs {
		if c.prs[i].Number != number {
			continue
		}
		c.prs[i].Base, c.prs[i].Title, c.prs[i].Body = change.Base, change.Title, change.Body
		if c.lose {
			c.lose = false
			return pulls.PullRequest{}, errors.New("connection reset")
		}
		return c.prs[i], nil
	}
	return pulls.PullRequest{}, errors.New("pull request not found")
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
	p.pulls.findErr = errors.New(refusal)
	// The controller's clock moves only as the test advances it, past every
	// time the fixture recorded.
	clock := &fixedClock{now: p.clock.Now().Add(time.Hour)}
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
			want := fmt.Sprintf("Publishing owner approval %d has failed 3 times in a row. Last failure: Effect returned error: %s", in.Approval, refusal)
			retryAt := clock.Now().Add(wait).UTC().Format(time.RFC3339)
			if !found || e.Workstream != p.stream || e.Question != want || len(e.Options) != 0 || e.Revision != in.Approval ||
				!e.OpenedAt.Equal(firstFailure) || !strings.Contains(e.Blocked, "tries again at "+retryAt) || e.Answer.Method != http.MethodGet {
				t.Fatalf("publication entry %+v, want question %q and a retry at %s", e, want, retryAt)
			}
		}
		// The next attempt waits twice as long as the one before.
		clock.Advance(wait - time.Second)
		must(t, c.Pass(ctx))
		if p.pulls.finds != attempt+1 {
			t.Fatalf("attempted %d times before the retry was due, want %d", p.pulls.finds, attempt+1)
		}
		clock.Advance(time.Second)
	}
	p.pulls.findErr = nil
	must(t, c.Pass(ctx))
	if p.feature(t) != DeliveredState || p.pulls.creates != 1 {
		t.Fatalf("state %s after %d pull requests", p.feature(t), p.pulls.creates)
	}
	if e, found := publication(); found {
		t.Fatalf("the published workstream stays raised: %+v", e)
	}
}

// TestPublicationWaitsForAGitHubToken shows a service without a credential
// holding a publication without pushing or asking the host anything, raising
// it to the inbox at once, and publishing once it has one.
func TestPublicationWaitsForAGitHubToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := newPublicationFixture(t, "squash")
	p.approve(t, nil)
	op := p.request(t)
	in, err := decodePublish(op)
	must(t, err)
	p.pulls.mu.Lock()
	p.pulls.uncredentialed = true
	p.pulls.mu.Unlock()
	clock := &fixedClock{now: p.clock.Now().Add(time.Hour)}
	c, err := reconcile.New(p.repository, reconcile.Options{Worker: "test", Now: clock.Now, RetryDelay: time.Minute,
		Adapters: map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RepositoryBoundary: p.publisher()},
		Hold:     p.s.holding(p.repository)})
	must(t, err)
	for range 3 {
		must(t, c.Pass(ctx))
		clock.Advance(time.Hour)
	}
	if _, pushed := p.forkBranch(t); pushed || p.pulls.finds != 0 || p.pulls.creates != 0 {
		t.Fatalf("a publication without a credential pushed %v, found %d times and opened %d pull requests", pushed, p.pulls.finds, p.pulls.creates)
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
	if _, pushed := p.forkBranch(t); pushed || p.pulls.finds != 0 {
		t.Fatal("a refused attempt pushed or asked the host")
	}
	p.pulls.mu.Lock()
	p.pulls.uncredentialed = false
	p.pulls.mu.Unlock()
	must(t, c.Pass(ctx))
	if p.feature(t) != DeliveredState || p.pulls.creates != 1 {
		t.Fatalf("state %s after %d pull requests", p.feature(t), p.pulls.creates)
	}
	inbox, api = p.s.inbox(ctx)
	if api != nil {
		t.Fatal(api)
	}
	if slices.ContainsFunc(inbox.Entries, func(e InboxEntry) bool { return e.Kind == InboxPublication }) {
		t.Fatal("the published workstream stays raised")
	}
}
