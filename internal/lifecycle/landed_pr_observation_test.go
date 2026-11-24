package lifecycle_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

const landedCommit = "2222222222222222222222222222222222222222"

func landedWithOpenDraft(t *testing.T) (*journey.PublicationFixture, *journey.BaseRefresher) {
	t.Helper()
	p := journey.NewUnpushedPublicationFixture(t, "squash", "")
	ctx := context.Background()
	const number, head = 100, "1111111111111111111111111111111111111111"
	fixturekit.Must(t, p.Publisher().RecordPublication(ctx, p.Stream, api.DeliveryPublication{Push: 1, Review: p.Report.Review, Operation: "final-1", Status: delivery.PublicationOpened, Style: "squash", Fork: "owner/dagger", Remote: "origin", Branch: intake.FeatureBranch(p.Stream),
		Reviewed: p.Report.Commit, Commit: head, Upstream: "dagger/dagger", Base: "main", Draft: true, PullRequest: number, URL: "https://github.com/dagger/dagger/pull/100", Title: "Resumable uploads", PushedAt: p.S.Now()}))
	p.Pulls.Mu.Lock()
	p.Pulls.Prs = append(p.Pulls.Prs, pulls.PullRequest{Number: number, URL: "https://github.com/dagger/dagger/pull/100", State: "open", Draft: true, Head: intake.FeatureBranch(p.Stream), HeadRepository: "owner/dagger", HeadCommit: head, Base: "main", Title: "Resumable uploads"})
	p.Pulls.Mu.Unlock()

	state, err := p.Repository.Workflow(p.Stream, trace.FeatureSubject)
	fixturekit.Must(t, err)
	at := p.S.Now()
	header := func(id string) trace.Header {
		return trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: p.Repository.Project(), Workstream: p.Stream, At: at, Actor: intake.OwnerActor, Cause: "landing"}
	}
	landing, err := json.Marshal(delivery.DeliveryLanding{Push: 1, Review: p.Report.Review, Branch: intake.FeatureBranch(p.Stream), Commit: landedCommit, Reviewed: p.Report.Commit, Remote: "upstream", Upstream: "dagger/dagger", BaseBranch: "main", Confirmed: landedCommit, At: at, DecidedBy: trace.DecidedByOwner})
	fixturekit.Must(t, err)
	merge, err := json.Marshal(delivery.DeliveryMerge{Branch: intake.FeatureBranch(p.Stream), Commit: landedCommit, Upstream: seal.Base{Remote: "upstream", Branch: "main", Commit: landedCommit}})
	fixturekit.Must(t, err)
	h := header(intake.DeliveredState)
	h.Schema = "osmia.trace.transition"
	_, err = p.Repository.RecordDocumentsWith(context.Background(), []trace.Document{
		{Header: header(delivery.LandingDocument), Path: delivery.LandingPath, Content: string(landing)},
		{Header: header(delivery.MergeDocument), Path: delivery.MergePath, Content: string(merge)}},
		trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: trace.FeatureSubject, From: state.Value, To: intake.DeliveredState, Reason: "landed"}})
	fixturekit.Must(t, err)
	return p, &journey.BaseRefresher{S: p.S, Repository: p.Repository}
}

func hostWrites(p *journey.PublicationFixture) (finds, closes int, comments []fixturekit.PullComment) {
	p.Pulls.Mu.Lock()
	defer p.Pulls.Mu.Unlock()
	return p.Pulls.Finds, p.Pulls.Closes, append([]fixturekit.PullComment(nil), p.Pulls.Comments...)
}

func TestThePollClosesALandedWorkstreamsOpenDraftOnce(t *testing.T) {
	t.Parallel()
	p, refresh := landedWithOpenDraft(t)
	ctx := context.Background()

	fixturekit.Must(t, refresh.ObserveLandedPullRequests(ctx))
	finds, closes, comments := hostWrites(p)
	if closes != 1 || len(comments) != 1 || comments[0].Number != 100 || !strings.Contains(comments[0].Text, landedCommit) || !strings.Contains(comments[0].Text, "main of dagger/dagger") {
		t.Fatalf("%d closes and comments %+v", closes, comments)
	}
	p.Pulls.Mu.Lock()
	state := p.Pulls.Prs[0].State
	p.Pulls.Mu.Unlock()
	if state != "closed" {
		t.Fatalf("the pull request is %s", state)
	}

	for range 3 {
		fixturekit.Jump(p.Clock, p.S.Now().Add(11*time.Minute))
		fixturekit.Must(t, refresh.ObserveLandedPullRequests(ctx))
		fixturekit.Must(t, (&journey.BaseRefresher{S: p.S, Repository: p.Repository}).Pass(ctx))
	}
	if again, closes, comments := hostWrites(p); closes != 1 || len(comments) != 1 || again != finds {
		t.Fatalf("later polls looked up %d more times and wrote %d closes, %d comments", again-finds, closes, len(comments))
	}
	if got := p.Feature(t); got != intake.DeliveredState {
		t.Fatalf("the workstream is %s", got)
	}
	finished, err := delivery.OwnPullRequestFinished(p.Repository, p.Stream)
	fixturekit.Must(t, err)
	if !finished {
		t.Fatal("the landed workstream's pull request is not finished")
	}
}

func TestThePollLeavesALandedWorkstreamsMergedPullRequestAlone(t *testing.T) {
	t.Parallel()
	p, refresh := landedWithOpenDraft(t)
	ctx := context.Background()
	p.Pulls.Mu.Lock()
	p.Pulls.Prs[0].State, p.Pulls.Prs[0].Merged, p.Pulls.Prs[0].Draft, p.Pulls.Prs[0].MergeCommit = "closed", true, false, landedCommit
	p.Pulls.Mu.Unlock()

	fixturekit.Must(t, refresh.ObserveLandedPullRequests(ctx))
	fixturekit.Jump(p.Clock, p.S.Now().Add(11*time.Minute))
	fixturekit.Must(t, refresh.ObserveLandedPullRequests(ctx))
	if _, closes, comments := hostWrites(p); closes != 0 || len(comments) != 0 {
		t.Fatalf("a merged pull request had %d closes and comments %+v", closes, comments)
	}
	if got := p.Feature(t); got != intake.DeliveredState {
		t.Fatalf("the workstream is %s", got)
	}
	docs, err := trace.Read[trace.Document](p.Repository, p.Stream)
	fixturekit.Must(t, err)
	evidence, err := trace.MemberResolutionFrom(docs)
	fixturekit.Must(t, err)
	if got := evidence.Resolution(intake.DeliveredState); got != "merged" {
		t.Fatalf("the workstream resolves as %q", got)
	}
}
