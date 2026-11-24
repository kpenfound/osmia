package lifecycle_test

import (
	"context"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestOwnerMarkingTheDraftReadyOnTheHostIsRecordedDelivered(t *testing.T) {
	t.Parallel()
	p := newUnpushedPublicationFixture(t, "squash", "")
	ctx := context.Background()
	const number, head = 100, "1111111111111111111111111111111111111111"
	intakefixture.Must(t, p.publisher().recordPublication(ctx, p.stream, api.DeliveryPublication{Push: 1, Review: p.report.Review, Operation: "final-1", Status: delivery.PublicationOpened, Style: "squash", Fork: "owner/dagger", Remote: "origin", Branch: intake.FeatureBranch(p.stream),
		Reviewed: p.report.Commit, Commit: head, Upstream: "dagger/dagger", Base: "main", Draft: true, PullRequest: number, URL: "https://github.com/dagger/dagger/pull/100", Title: "Resumable uploads", PushedAt: p.s.now()}))
	p.pulls.Mu.Lock()
	p.pulls.Prs = append(p.pulls.Prs, pulls.PullRequest{Number: number, URL: "https://github.com/dagger/dagger/pull/100", State: "open", Draft: true, Head: intake.FeatureBranch(p.stream), HeadRepository: "owner/dagger", HeadCommit: head, Base: "main", Title: "Resumable uploads"})
	p.pulls.Mu.Unlock()
	refresh := &baseRefresher{s: p.s, repository: p.repository}
	finds := func() int {
		p.pulls.Mu.Lock()
		defer p.pulls.Mu.Unlock()
		return p.pulls.Finds
	}

	intakefixture.Must(t, refresh.ObserveDraftPullRequests(ctx))
	if finds() != 1 || p.feature(t) != intake.AssembledState {
		t.Fatalf("a draft was looked up %d times and the workstream is %s", finds(), p.feature(t))
	}
	intakefixture.Must(t, refresh.ObserveDraftPullRequests(ctx))
	if finds() != 1 {
		t.Fatalf("the poll ran again within its interval: %d lookups", finds())
	}

	p.pulls.Mu.Lock()
	p.pulls.Prs[0].Draft = false
	p.pulls.Mu.Unlock()
	fixturekit.Jump(p.clock, p.s.now().Add(11*time.Minute))
	intakefixture.Must(t, refresh.ObserveDraftPullRequests(ctx))
	if got := p.feature(t); got != intake.DeliveredState {
		t.Fatalf("the workstream is %s after the owner marked the pull request ready", got)
	}
	ready, recorded, err := delivery.ReadyAt(p.repository, p.stream)
	intakefixture.Must(t, err)
	if !recorded || ready.Source != api.ReadyOnHost || ready.PullRequest != number || ready.Commit != head || ready.Approval != 0 {
		t.Fatalf("the ready record %+v", ready)
	}
	transitions, err := trace.Read[trace.Transition](p.repository, p.stream)
	intakefixture.Must(t, err)
	var delivered *trace.Transition
	for i, tr := range transitions {
		if tr.Subject == trace.FeatureSubject && tr.To == intake.DeliveredState {
			delivered = &transitions[i]
		}
	}
	if delivered == nil || delivered.Actor != intake.OwnerActor {
		t.Fatalf("the delivery is not attributed to the owner: %+v", delivered)
	}
	p.pulls.Mu.Lock()
	readies, creates, updates := p.pulls.Readies, p.pulls.Creates, p.pulls.Updates
	p.pulls.Mu.Unlock()
	if readies != 0 || creates != 0 || updates != 0 {
		t.Fatalf("observing wrote to the host: %d ready calls, %d creates, %d updates", readies, creates, updates)
	}

	fixturekit.Jump(p.clock, p.s.now().Add(11*time.Minute))
	before := finds()
	intakefixture.Must(t, refresh.ObserveDraftPullRequests(ctx))
	if finds() != before {
		t.Fatalf("a delivered workstream was looked up again")
	}
}
