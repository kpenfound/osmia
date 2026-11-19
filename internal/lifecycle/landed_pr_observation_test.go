package lifecycle_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

const landedCommit = "2222222222222222222222222222222222222222"

// landedWithOpenDraft is a workstream the service landed on the base branch whose
// draft pull request the host still shows open.
func landedWithOpenDraft(t *testing.T) (*publicationFixture, *baseRefresher) {
	t.Helper()
	p := newUnpushedPublicationFixture(t, "squash", "")
	ctx := context.Background()
	const number, head = 100, "1111111111111111111111111111111111111111"
	must(t, p.publisher().recordPublication(ctx, p.stream, DeliveryPublication{Push: 1, Review: p.report.Review, Operation: "final-1", Status: publicationOpened, Style: "squash", Fork: "owner/dagger", Remote: "origin", Branch: featureBranch(p.stream),
		Reviewed: p.report.Commit, Commit: head, Upstream: "dagger/dagger", Base: "main", Draft: true, PullRequest: number, URL: "https://github.com/dagger/dagger/pull/100", Title: "Resumable uploads", PushedAt: p.s.now()}))
	p.pulls.Mu.Lock()
	p.pulls.Prs = append(p.pulls.Prs, pulls.PullRequest{Number: number, URL: "https://github.com/dagger/dagger/pull/100", State: "open", Draft: true, Head: featureBranch(p.stream), HeadRepository: "owner/dagger", HeadCommit: head, Base: "main", Title: "Resumable uploads"})
	p.pulls.Mu.Unlock()

	// The landing's own records: the landing, the merge evidence every edge reads
	// and the move to delivered, in one commit.
	state, err := p.repository.Workflow(p.stream, trace.FeatureSubject)
	must(t, err)
	at := p.s.now()
	header := func(id string) trace.Header {
		return trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: p.repository.Project(), Workstream: p.stream, At: at, Actor: ownerActor, Cause: "landing"}
	}
	landing, err := json.Marshal(delivery.DeliveryLanding{Push: 1, Review: p.report.Review, Branch: featureBranch(p.stream), Commit: landedCommit, Reviewed: p.report.Commit, Remote: "upstream", Upstream: "dagger/dagger", BaseBranch: "main", Confirmed: landedCommit, At: at, DecidedBy: trace.DecidedByOwner})
	must(t, err)
	merge, err := json.Marshal(delivery.DeliveryMerge{Branch: featureBranch(p.stream), Commit: landedCommit, Upstream: seal.Base{Remote: "upstream", Branch: "main", Commit: landedCommit}})
	must(t, err)
	h := header(DeliveredState)
	h.Schema = "osmia.trace.transition"
	_, err = p.repository.RecordDocumentsWith(context.Background(), []trace.Document{
		{Header: header(delivery.LandingDocument), Path: delivery.LandingPath, Content: string(landing)},
		{Header: header(delivery.MergeDocument), Path: delivery.MergePath, Content: string(merge)}},
		trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: trace.FeatureSubject, From: state.Value, To: DeliveredState, Reason: "landed"}})
	must(t, err)
	return p, &baseRefresher{s: p.s, repository: p.repository}
}

func hostWrites(p *publicationFixture) (finds, closes int, comments []fixturekit.PullComment) {
	p.pulls.Mu.Lock()
	defer p.pulls.Mu.Unlock()
	return p.pulls.Finds, p.pulls.Closes, append([]fixturekit.PullComment(nil), p.pulls.Comments...)
}

// The pull request poll closes the open draft of a landed workstream once, with
// a comment naming the landed commit and base branch. Polls after that and a
// poll of a restarted service, which keeps no poll cadence, look at nothing and
// post nothing more, and the workstream stays delivered and resolved.
func TestThePollClosesALandedWorkstreamsOpenDraftOnce(t *testing.T) {
	t.Parallel()
	p, refresh := landedWithOpenDraft(t)
	ctx := context.Background()

	must(t, refresh.ObserveLandedPullRequests(ctx))
	finds, closes, comments := hostWrites(p)
	if closes != 1 || len(comments) != 1 || comments[0].Number != 100 || !strings.Contains(comments[0].Text, landedCommit) || !strings.Contains(comments[0].Text, "main of dagger/dagger") {
		t.Fatalf("%d closes and comments %+v", closes, comments)
	}
	p.pulls.Mu.Lock()
	state := p.pulls.Prs[0].State
	p.pulls.Mu.Unlock()
	if state != "closed" {
		t.Fatalf("the pull request is %s", state)
	}

	for range 3 {
		jump(p.clock, p.s.now().Add(11*time.Minute))
		must(t, refresh.ObserveLandedPullRequests(ctx))
		must(t, (&baseRefresher{s: p.s, repository: p.repository}).Pass(ctx))
	}
	if again, closes, comments := hostWrites(p); closes != 1 || len(comments) != 1 || again != finds {
		t.Fatalf("later polls looked up %d more times and wrote %d closes, %d comments", again-finds, closes, len(comments))
	}
	if got := p.feature(t); got != DeliveredState {
		t.Fatalf("the workstream is %s", got)
	}
	finished, err := delivery.OwnPullRequestFinished(p.repository, p.stream)
	must(t, err)
	if !finished {
		t.Fatal("the landed workstream's pull request is not finished")
	}
}

// A pull request the host already shows merged is left alone: nothing is closed
// or commented, and the workstream's state is the landing's.
func TestThePollLeavesALandedWorkstreamsMergedPullRequestAlone(t *testing.T) {
	t.Parallel()
	p, refresh := landedWithOpenDraft(t)
	ctx := context.Background()
	p.pulls.Mu.Lock()
	p.pulls.Prs[0].State, p.pulls.Prs[0].Merged, p.pulls.Prs[0].Draft, p.pulls.Prs[0].MergeCommit = "closed", true, false, landedCommit
	p.pulls.Mu.Unlock()

	must(t, refresh.ObserveLandedPullRequests(ctx))
	jump(p.clock, p.s.now().Add(11*time.Minute))
	must(t, refresh.ObserveLandedPullRequests(ctx))
	if _, closes, comments := hostWrites(p); closes != 0 || len(comments) != 0 {
		t.Fatalf("a merged pull request had %d closes and comments %+v", closes, comments)
	}
	if got := p.feature(t); got != DeliveredState {
		t.Fatalf("the workstream is %s", got)
	}
	docs, err := trace.Read[trace.Document](p.repository, p.stream)
	must(t, err)
	evidence, err := trace.MemberResolutionFrom(docs)
	must(t, err)
	if got := evidence.Resolution(DeliveredState); got != "merged" {
		t.Fatalf("the workstream resolves as %q", got)
	}
}
