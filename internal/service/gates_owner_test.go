package service

import (
	"context"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func jevDeliveryFixture(t *testing.T, p *gateJev, extra string) (*shedFixture, config.WorkstreamID, *trace.Repository) {
	t.Helper()
	f := newDebateFixtureWith(t, 1, 1, gateJevConfig(gateKey(t, true))+extra, func(o *Options) { o.JevProvider = p.Factory() })
	stream, repository := f.buildFixture().SeedGit(t, "delivery")
	a := &delivery.FinalReviewer{S: f.s.delivery(), Repository: repository}
	intakefixture.SeedDelivery(t, repository, stream, func(unit string, files map[string]string) string {
		return mergeDirectly(t, f, repository, stream, unit, files)
	}, a.Pass, func(ctx context.Context) (FinalReport, error) {
		in, sealed, err := a.Governing(ctx, stream)
		return FinalReport{Commit: in.Commit, Seal: in.Seal, SpecHash: sealed.SpecHash, Spec: in.Spec, Plan: in.Plan, Charter: in.Charter, Upstream: &sealed.Base}, err
	}, finalReviewActor, finalReportDocument, finalReportPath, finalReviewed)
	f.s.setSole(&activeProject{id: repository.Project(), repository: repository})
	return f, stream, repository
}

func deliveryGateOf(t *testing.T, s *Service, repository *trace.Repository, stream config.WorkstreamID) (InboxEntry, *intake.Gate) {
	t.Helper()
	ctx := context.Background()
	e, open, err := s.deliveryEntry(ctx, repository, stream)
	if err != nil || !open {
		t.Fatalf("delivery card open %v: %v", open, err)
	}
	g, err := s.deliveryGate(ctx, repository, stream, e)
	fixturekit.Must(t, err)
	if g == nil || g.Kind != jev.KindDeliveryApproval || len(g.Options) != 1 || g.Options[0].Name != "approve" {
		t.Fatalf("delivery gate %+v", g)
	}
	return e, g
}

// Jev decides the delivery card at the owner threshold: a confident pick
// records the approval with Jev as its decider, actor and judgment, pinning
// exactly the review identity the card offers, and the card leaves the inbox.
func TestAConfidentJevApprovesTheDeliveryCardAtTheOwnerThreshold(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &gateJev{confidence: 0.95}
	f, stream, repository := jevDeliveryFixture(t, p, "")
	defer f.stop(t)
	e, g := deliveryGateOf(t, f.s, repository, stream)
	if gates, err := f.s.intake().ReachedGates(ctx, &intake.Project{ID: repository.Project(), Repository: repository}); err != nil || !containsGate(gates, g.ID()) {
		t.Fatalf("reached gates %+v: %v", gates, err)
	}
	w := newGateWorker(f.s, &activeProject{id: repository.Project(), repository: repository})
	w.handle(ctx, *g)

	_, doc, approval, err := delivery.DeliveryDocuments(repository, stream)
	fixturekit.Must(t, err)
	judgment, record, ok := decisionJudgment(t, repository, stream, jev.KindDeliveryApproval, g.Key)
	if !ok || record.Acceptance != "owner_threshold=0.9" || record.Occurrence.Threshold != 0.9 {
		t.Fatalf("judgment %s %+v", judgment, record)
	}
	if approval == nil || approval.DecidedBy != trace.DecidedByJev || approval.Judgment != judgment || doc.Actor != trace.JevActor {
		t.Fatalf("approval %+v by %+v", approval, doc.Actor)
	}
	if approval.Review != e.Answer.Body["review"] || approval.ReviewRevision != e.Answer.Body["review_revision"] || approval.Commit != e.Answer.Body["commit"] || approval.DraftHash != e.Answer.Body["draft_hash"] {
		t.Fatalf("approval %+v does not pin the card's identity %+v", approval, e.Answer.Body)
	}
	transitions, err := trace.Read[trace.Transition](repository, stream)
	fixturekit.Must(t, err)
	approved := false
	for _, tr := range transitions {
		if tr.Subject == delivery.DeliverySubject && tr.To == "approved-1" {
			approved = tr.Actor == trace.JevActor && strings.HasPrefix(tr.Reason, "Jev")
		}
	}
	if !approved {
		t.Fatalf("no approval transition by Jev: %+v", transitions)
	}
	if _, open, err := f.s.deliveryEntry(ctx, repository, stream); err != nil || open {
		t.Fatalf("the delivery card is still open: %v %v", open, err)
	}
	w.handle(ctx, *g)
	if len(p.Requests()) != 1 {
		t.Fatalf("requests: %d", len(p.Requests()))
	}
}

func TestADeliveryPickBelowTheOwnerThresholdLeavesTheCardToTheOwner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &gateJev{confidence: 0.95}
	f, stream, repository := jevDeliveryFixture(t, p, "decision_threshold = 0.5\nowner_threshold = 0.99\n")
	defer f.stop(t)
	_, g := deliveryGateOf(t, f.s, repository, stream)
	w := newGateWorker(f.s, &activeProject{id: repository.Project(), repository: repository})
	w.handle(ctx, *g)

	if _, _, approval, err := delivery.DeliveryDocuments(repository, stream); err != nil || approval != nil {
		t.Fatalf("approval %+v: %v", approval, err)
	}
	card, err := f.s.intake().OpenDecisions(ctx, repository, statusOf(t, repository, stream))
	fixturekit.Must(t, err)
	var found *InboxEntry
	for i := range card {
		if card[i].Kind == api.InboxDelivery {
			found = &card[i]
		}
	}
	if found == nil || found.Jev == nil || found.Jev.Pick != "approve" || found.Jev.Binding || found.Jev.Threshold != 0.99 || !strings.Contains(found.Recommendation, "Jev recommends") {
		t.Fatalf("delivery card %+v", found)
	}
}

func statusOf(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) trace.WorkstreamStatus {
	t.Helper()
	statuses, err := repository.Statuses()
	fixturekit.Must(t, err)
	for _, w := range statuses {
		if w.Workstream == stream {
			return w
		}
	}
	t.Fatalf("no status of %s", stream)
	return trace.WorkstreamStatus{}
}

func containsGate(gates []intake.Gate, id string) bool {
	for _, g := range gates {
		if g.ID() == id {
			return true
		}
	}
	return false
}

func presentCancellation(t *testing.T, f *gateFixture) lifecycle.WorkgroupChange {
	t.Helper()
	ctx := context.Background()
	stream := fixturekit.Stream
	groups, err := trace.EnsureWorkgroups(ctx, f.repo, []config.WorkstreamID{stream}, f.now(), serviceActor, "fixture")
	fixturekit.Must(t, err)
	_, err = f.repo.EnsureManager(ctx, stream, f.now(), serviceActor)
	fixturekit.Must(t, err)
	c := lifecycle.WorkgroupChange{ID: "cancel", Workgroup: groups[0].ID, Origin: stream, Host: stream, State: "presented",
		Request: lifecycle.ChangeRequest{Key: "cancel", Kind: "cancel", Scope: "workgroup", Change: "Superseded by the owner"}}
	fixturekit.Must(t, f.s.lifecycle().SaveChange(ctx, f.repo, &c))
	return c
}

// Jev decides a cancellation's impact summary at the owner threshold, from the
// options it offers, none of which needs text: the decision is recorded with
// Jev as its decider and judgment, and the conversation labels it.
func TestAConfidentJevDecidesACancellationImpactSummary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &gateJev{confidence: 0.96, pick: func(_ string, options []string) string { return cancelApproveDraft }}
	f := newGateFixture(t, p)
	c := presentCancellation(t, f)
	gates, err := f.s.cancellationGates(ctx, f.repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if len(gates) != 1 || gates[0].Kind != jev.KindCancellation || gates[0].Key != jev.CancellationKey(c.ID, c.Revision) || len(gates[0].Options) != 3 {
		t.Fatalf("gates %+v", gates)
	}
	f.w.handle(ctx, gates[0])
	changes, err := readChanges(f.repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	judgment, record, _ := decisionJudgment(t, f.repo, fixturekit.Stream, jev.KindCancellation, gates[0].Key)
	if len(changes) != 1 || changes[0].Decision == nil || changes[0].Decision.Decision != "approve" || changes[0].Decision.PullRequests != "draft" ||
		changes[0].Decision.DecidedBy != trace.DecidedByJev || changes[0].Decision.Judgment != judgment || judgment == "" || record.Acceptance != "owner_threshold=0.9" {
		t.Fatalf("changes %+v judgment %s %+v", changes, judgment, record)
	}
	if gates, err := f.s.cancellationGates(ctx, f.repo, fixturekit.Stream); err != nil || len(gates) != 0 {
		t.Fatalf("gates after the decision %+v: %v", gates, err)
	}
	f.w.handle(ctx, gates[0])
	if len(p.Requests()) != 1 {
		t.Fatalf("requests: %d", len(p.Requests()))
	}
}

// A cancellation pick below the owner threshold decides nothing: the impact
// summary stays presented for the owner.
func TestACancellationPickBelowTheOwnerThresholdStaysTheOwners(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newGateFixture(t, &gateJev{confidence: 0.85})
	presentCancellation(t, f)
	gates, err := f.s.cancellationGates(ctx, f.repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if len(gates) != 1 {
		t.Fatalf("gates %+v", gates)
	}
	f.w.handle(ctx, gates[0])
	changes, err := readChanges(f.repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	if len(changes) != 1 || changes[0].Decision != nil || changes[0].State != "presented" {
		t.Fatalf("changes %+v", changes)
	}
	if res, err := f.s.jev.Lookup(f.repo, fixturekit.Stream, jev.KindCancellation, gates[0].Key); err != nil || res.Status != jev.Nonbinding || res.Threshold != 0.9 {
		t.Fatalf("lookup %+v %v", res, err)
	}
}
