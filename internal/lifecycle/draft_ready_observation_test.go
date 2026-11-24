package lifecycle_test

import (
	"context"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestOwnerMarkingTheDraftReadyOnTheHostIsRecordedDelivered(t *testing.T) {
	t.Parallel()
	p := journey.NewUnpushedPublicationFixture(t, "squash", "")
	ctx := context.Background()
	const number, head = 100, "1111111111111111111111111111111111111111"
	fixturekit.Must(t, p.Publisher().RecordPublication(ctx, p.Stream, api.DeliveryPublication{Push: 1, Review: p.Report.Review, Operation: "final-1", Status: delivery.PublicationOpened, Style: "squash", Fork: "owner/dagger", Remote: "origin", Branch: intake.FeatureBranch(p.Stream),
		Reviewed: p.Report.Commit, Commit: head, Upstream: "dagger/dagger", Base: "main", Draft: true, PullRequest: number, URL: "https://github.com/dagger/dagger/pull/100", Title: "Resumable uploads", PushedAt: p.S.Now()}))
	p.Pulls.Mu.Lock()
	p.Pulls.Prs = append(p.Pulls.Prs, pulls.PullRequest{Number: number, URL: "https://github.com/dagger/dagger/pull/100", State: "open", Draft: true, Head: intake.FeatureBranch(p.Stream), HeadRepository: "owner/dagger", HeadCommit: head, Base: "main", Title: "Resumable uploads"})
	p.Pulls.Mu.Unlock()
	refresh := &journey.BaseRefresher{S: p.S, Repository: p.Repository}
	finds := func() int {
		p.Pulls.Mu.Lock()
		defer p.Pulls.Mu.Unlock()
		return p.Pulls.Finds
	}

	fixturekit.Must(t, refresh.ObserveDraftPullRequests(ctx))
	if finds() != 1 || p.Feature(t) != intake.AssembledState {
		t.Fatalf("a draft was looked up %d times and the workstream is %s", finds(), p.Feature(t))
	}
	fixturekit.Must(t, refresh.ObserveDraftPullRequests(ctx))
	if finds() != 1 {
		t.Fatalf("the poll ran again within its interval: %d lookups", finds())
	}

	p.Pulls.Mu.Lock()
	p.Pulls.Prs[0].Draft = false
	p.Pulls.Mu.Unlock()
	fixturekit.Jump(p.Clock, p.S.Now().Add(11*time.Minute))
	fixturekit.Must(t, refresh.ObserveDraftPullRequests(ctx))
	if got := p.Feature(t); got != intake.DeliveredState {
		t.Fatalf("the workstream is %s after the owner marked the pull request ready", got)
	}
	ready, recorded, err := delivery.ReadyAt(p.Repository, p.Stream)
	fixturekit.Must(t, err)
	if !recorded || ready.Source != api.ReadyOnHost || ready.PullRequest != number || ready.Commit != head || ready.Approval != 0 {
		t.Fatalf("the ready record %+v", ready)
	}
	transitions, err := trace.Read[trace.Transition](p.Repository, p.Stream)
	fixturekit.Must(t, err)
	var delivered *trace.Transition
	for i, tr := range transitions {
		if tr.Subject == trace.FeatureSubject && tr.To == intake.DeliveredState {
			delivered = &transitions[i]
		}
	}
	if delivered == nil || delivered.Actor != intake.OwnerActor {
		t.Fatalf("the delivery is not attributed to the owner: %+v", delivered)
	}
	p.Pulls.Mu.Lock()
	readies, creates, updates := p.Pulls.Readies, p.Pulls.Creates, p.Pulls.Updates
	p.Pulls.Mu.Unlock()
	if readies != 0 || creates != 0 || updates != 0 {
		t.Fatalf("observing wrote to the host: %d ready calls, %d creates, %d updates", readies, creates, updates)
	}

	fixturekit.Jump(p.Clock, p.S.Now().Add(11*time.Minute))
	before := finds()
	fixturekit.Must(t, refresh.ObserveDraftPullRequests(ctx))
	if finds() != before {
		t.Fatalf("a delivered workstream was looked up again")
	}
}
