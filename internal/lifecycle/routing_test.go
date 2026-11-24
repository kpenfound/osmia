package lifecycle_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

func departTo(target string, p float64) *jevfixture.Transitions {
	return &jevfixture.Transitions{Script: func(a jevfixture.Ask) jevfixture.Reply { return jevfixture.Departs(a, target, p, 0.95) }}
}

func lastOn(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, subject string) trace.Transition {
	t.Helper()
	transitions, err := trace.Read[trace.Transition](repo, stream)
	fixturekit.Must(t, err)
	var out trace.Transition
	for _, tr := range transitions {
		if tr.Subject == subject {
			out = tr
		}
	}
	return out
}

func routeFor(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, trigger string) []router.Record {
	t.Helper()
	docs, err := trace.Read[trace.Document](repo, stream)
	fixturekit.Must(t, err)
	latest := map[string]trace.Document{}
	var order []string
	for _, d := range docs {
		if !strings.HasPrefix(d.Path, "routes/") {
			continue
		}
		if _, ok := latest[d.ID]; !ok {
			order = append(order, d.ID)
		}
		if d.Revision >= latest[d.ID].Revision {
			latest[d.ID] = d
		}
	}
	var out []router.Record
	for _, id := range order {
		var rec router.Record
		fixturekit.Must(t, json.Unmarshal([]byte(latest[id].Content), &rec))
		if rec.Trigger == trigger {
			out = append(out, rec)
		}
	}
	return out
}

func jevDepartures(t *testing.T, s *journey.Service, repo *trace.Repository, stream config.WorkstreamID) int {
	t.Helper()
	departed := journey.DeparturesInFeed(t, s, repo, stream)
	for _, d := range departed {
		if d.DecidedBy != trace.DecidedByJev || d.DefaultTo == d.To {
			t.Fatalf("feed entry %+v", d)
		}
	}
	return len(departed)
}

func routeNotices(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) int {
	t.Helper()
	outbox, err := repo.Outbox(stream)
	fixturekit.Must(t, err)
	n := 0
	for _, e := range outbox {
		if e.Event.Kind == trace.NoticeKind && strings.HasPrefix(e.TransitionID, "route-") {
			n++
		}
	}
	return n
}

func TestAnEdgeWaitThatClearsFollowsItsDefault(t *testing.T) {
	t.Parallel()
	f := newEdgeGraphFixture(t, trace.EdgeHardBlock)
	p := &jevfixture.Transitions{}
	journey.InspectForTest(f.s).SetRouterJudge(jevfixture.TransitionJudge(f.s.Now, p))

	f.resolvePasses(t)
	entered := lastOn(t, f.repository, f.downstream, lifecycle.EdgeWaitSubject)
	if entered.To != "waiting" || entered.DefaultTo != "waiting" || entered.DecidedBy != trace.DecidedByCode || entered.Departed() {
		t.Fatalf("entering the wait %+v", entered)
	}
	if len(p.TransitionRequests()) != 0 {
		t.Fatal("Jev was asked about entering a wait, which has no other target")
	}

	f.recordOwnerMerge(t, f.upstream, "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2")
	f.resolvePasses(t)
	cleared := lastOn(t, f.repository, f.downstream, lifecycle.EdgeWaitSubject)
	if cleared.To != "available" || cleared.DefaultTo != "available" || cleared.DecidedBy != trace.DecidedByCode || cleared.Departed() || cleared.Judgment == "" {
		t.Fatalf("the wait clearing %+v", cleared)
	}
	if cleared.Cause != "edge-availability" || cleared.Reason != "the workstream's blocking edges resolved" {
		t.Fatalf("the cleared wait reads as it always has: %+v", cleared)
	}
	records := routeFor(t, f.repository, f.downstream, "dep.edge_resolved")
	if len(records) != 1 || records[0].State != router.StateCommitted || records[0].Outcome != router.OutcomeFollowed || records[0].Default != "available" {
		t.Fatalf("route %+v", records)
	}
	if len(p.TransitionRequests()) != 1 {
		t.Fatalf("Jev was asked %d times, want once for the one exit", len(p.TransitionRequests()))
	}
}

func TestAScriptedDepartureKeepsAnEdgeWaitingOnceForAnObservation(t *testing.T) {
	t.Parallel()
	f := newEdgeGraphFixture(t, trace.EdgeHardBlock)
	p := departTo("waiting", 0.9)
	journey.InspectForTest(f.s).SetRouterJudge(jevfixture.TransitionJudge(f.s.Now, p))
	f.resolvePasses(t)
	f.recordOwnerMerge(t, f.upstream, "b1c2d3e4f5a6b1c2d3e4f5a6b1c2d3e4f5a6b1c2")
	f.resolvePasses(t)

	kept := lastOn(t, f.repository, f.downstream, lifecycle.EdgeWaitSubject)
	if kept.From != "waiting" || kept.To != "waiting" || kept.DefaultTo != "available" || kept.DecidedBy != trace.DecidedByJev || kept.Judgment == "" || !kept.Departed() {
		t.Fatalf("the departure %+v", kept)
	}
	if !strings.Contains(kept.Reason, "decide-transition table") {
		t.Fatalf("the departure records its table rule: %q", kept.Reason)
	}
	records := routeFor(t, f.repository, f.downstream, "dep.edge_resolved")
	if len(records) != 1 || records[0].Outcome != router.OutcomeDeparted || records[0].Target != "waiting" || records[0].Default != "available" || records[0].Judgment != kept.Judgment {
		t.Fatalf("route %+v", records)
	}

	f.resolvePasses(t)
	f.resolvePasses(t)
	if n := len(p.TransitionRequests()); n != 1 {
		t.Fatalf("Jev was asked %d times for one observation", n)
	}
	if again := lastOn(t, f.repository, f.downstream, lifecycle.EdgeWaitSubject); again.ID != kept.ID {
		t.Fatalf("a later pass moved the wait again: %+v", again)
	}

	if n := jevDepartures(t, f.s, f.repository, f.downstream); n != 1 {
		t.Fatalf("%d departures in the feed, want one", n)
	}
}

func TestAnEdgeWaitIsHeldWhenTheBestTargetIsBelowItsBar(t *testing.T) {
	t.Parallel()
	f := newEdgeGraphFixture(t, trace.EdgeHardBlock)
	p := departTo("waiting", 0.55)
	journey.InspectForTest(f.s).SetRouterJudge(jevfixture.TransitionJudge(f.s.Now, p))
	f.resolvePasses(t)
	f.recordOwnerMerge(t, f.upstream, "c1d2e3f4a5b6c1d2e3f4a5b6c1d2e3f4a5b6c1d2")
	f.resolvePasses(t)

	if state, err := f.repository.Workflow(f.downstream, lifecycle.EdgeWaitSubject); err != nil || state.Value != "waiting" || state.Version != 1 {
		t.Fatalf("a held exit moved the wait: %+v %v", state, err)
	}
	records := routeFor(t, f.repository, f.downstream, "dep.edge_resolved")
	if len(records) != 1 || records[0].Outcome != router.OutcomeHeld || records[0].Hold != router.HoldBelowBar {
		t.Fatalf("route %+v", records)
	}
	if n := routeNotices(t, f.repository, f.downstream); n != 1 {
		t.Fatalf("%d notices for the held exit, want one", n)
	}
	f.resolvePasses(t)
	if n := len(p.TransitionRequests()); n != 1 {
		t.Fatalf("Jev was asked %d times for one occurrence", n)
	}
}

func parkAndRelease(t *testing.T, f *journey.ShedFixture) (*trace.Repository, config.WorkstreamID) {
	t.Helper()
	ctx := context.Background()
	f.Upstream(t)
	f.Stop(t)
	repo, err := trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
	fixturekit.Must(t, err)
	t.Cleanup(func() { repo.Close() })
	base, _ := journey.SealDependent(t, f, repo, config.WorkspacesGit, "", map[string]string{"base.go": "base\n"})
	stream, _ := journey.SealDependent(t, f, repo, config.WorkspacesGit, base, map[string]string{"dependent.go": "dependent\n"})
	h := trace.Header{Schema: "osmia.trace.transition", Version: 1, ID: "owner-abandoned", Revision: 1, Project: repo.Project(), Workstream: base, At: f.S.Now(), Actor: intake.OwnerActor, Cause: "owner"}
	_, err = repo.SetFeatureState(ctx, h, intake.AbandonedState, "Merged out of factory")
	fixturekit.Must(t, err)
	fixturekit.Must(t, f.S.Lifecycle().BaseWaitPass(ctx, repo))
	parked := lastOn(t, repo, stream, lifecycle.BaseWaitSubject)
	if parked.To != "waiting" || parked.DefaultTo != "waiting" || parked.DecidedBy != trace.DecidedByCode {
		t.Fatalf("parking the dependent %+v", parked)
	}
	upstream := journey.AdvanceUpstream(t, f, map[string]string{"base.go": "base\n", "UPSTREAM.md": "upstream\n"})
	data, err := json.Marshal(lifecycle.BaseObservation{Base: seal.Base{Remote: "upstream", Branch: "main", Commit: upstream}})
	fixturekit.Must(t, err)
	_, revision, err := lifecycle.BaseObservationAt(repo, stream)
	fixturekit.Must(t, err)
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "base-observation", Revision: revision + 1, Project: repo.Project(), Workstream: stream, At: f.S.Now(), Actor: execution.ForemanActor, Cause: "fixture"}, Path: "base-observation.json", Content: string(data)}
	fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{doc}))
	return repo, stream
}

func TestABaseWaitThatClearsFollowsItsDefault(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	p := &jevfixture.Transitions{}
	journey.InspectForTest(f.S).SetRouterJudge(jevfixture.TransitionJudge(f.S.Now, p))
	repo, stream := parkAndRelease(t, f)

	fixturekit.Must(t, f.S.Lifecycle().BaseWaitPass(context.Background(), repo))
	cleared := lastOn(t, repo, stream, lifecycle.BaseWaitSubject)
	if cleared.To != "available" || cleared.DefaultTo != "available" || cleared.DecidedBy != trace.DecidedByCode || cleared.Departed() || cleared.Cause != "base-availability" {
		t.Fatalf("the wait clearing %+v", cleared)
	}
	if waiting, err := lifecycle.BaseWaiting(repo, stream); err != nil || waiting {
		t.Fatalf("the cleared wait still parks the dependent: %v %v", waiting, err)
	}
	records := routeFor(t, repo, stream, "dep.base_available")
	if len(records) != 1 || records[0].Outcome != router.OutcomeFollowed || records[0].State != router.StateCommitted {
		t.Fatalf("route %+v", records)
	}
}

func TestAScriptedDepartureKeepsABaseWaitParked(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	p := departTo("waiting", 0.9)
	journey.InspectForTest(f.S).SetRouterJudge(jevfixture.TransitionJudge(f.S.Now, p))
	repo, stream := parkAndRelease(t, f)
	ctx := context.Background()

	fixturekit.Must(t, f.S.Lifecycle().BaseWaitPass(ctx, repo))
	kept := lastOn(t, repo, stream, lifecycle.BaseWaitSubject)
	if kept.From != "waiting" || kept.To != "waiting" || kept.DefaultTo != "available" || kept.DecidedBy != trace.DecidedByJev || kept.Judgment == "" || !kept.Departed() {
		t.Fatalf("the departure %+v", kept)
	}
	if waiting, err := lifecycle.BaseWaiting(repo, stream); err != nil || !waiting {
		t.Fatalf("the dependent is not parked after Jev kept the wait: %v %v", waiting, err)
	}
	fixturekit.Must(t, f.S.Lifecycle().BaseWaitPass(ctx, repo))
	if n := len(p.TransitionRequests()); n != 1 {
		t.Fatalf("Jev was asked %d times for one observation", n)
	}
	if n := jevDepartures(t, f.S, repo, stream); n != 1 {
		t.Fatalf("%d departures in the feed, want one", n)
	}
}

func TestMovingOntoUpstreamIsAPinnedOwnerMove(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	f.Upstream(t)
	f.Stop(t)
	p := departTo("waiting", 0.9)
	journey.InspectForTest(f.S).SetRouterJudge(jevfixture.TransitionJudge(f.S.Now, p))
	ctx := context.Background()
	repo, err := trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
	fixturekit.Must(t, err)
	defer repo.Close()
	base, _ := journey.SealDependent(t, f, repo, config.WorkspacesGit, "", map[string]string{"base.go": "base\n"})
	stream, _ := journey.SealDependent(t, f, repo, config.WorkspacesGit, base, map[string]string{"dependent.go": "dependent\n"})
	h := trace.Header{Schema: "osmia.trace.transition", Version: 1, ID: "owner-abandoned", Revision: 1, Project: repo.Project(), Workstream: base, At: f.S.Now(), Actor: intake.OwnerActor, Cause: "owner"}
	_, err = repo.SetFeatureState(ctx, h, intake.AbandonedState, "Merged out of factory")
	fixturekit.Must(t, err)
	fixturekit.Must(t, f.S.Lifecycle().BaseWaitPass(ctx, repo))
	journey.AdvanceUpstream(t, f, map[string]string{"base.go": "base\n", "UPSTREAM.md": "upstream\n"})
	cfg := f.S.About(repo)
	g, err := intake.FeatureWorkspaces(cfg, repo).Of(stream)
	fixturekit.Must(t, err)

	if _, apiFault := f.S.Lifecycle().MoveOntoUpstream(ctx, cfg, repo, g, stream, base); apiFault != nil {
		t.Fatal(apiFault)
	}
	released := lastOn(t, repo, stream, lifecycle.BaseWaitSubject)
	if released.To != "available" || released.DecidedBy != trace.DecidedByOwner || released.DefaultTo != "available" || released.Departed() || released.Actor != intake.OwnerActor {
		t.Fatalf("the release %+v", released)
	}
	records := routeFor(t, repo, stream, "dep.base_integrated")
	if len(records) != 1 || records[0].Authority != router.AuthorityOwner || records[0].Outcome != router.OutcomeFollowed || records[0].JevRequested {
		t.Fatalf("route %+v", records)
	}
	if n := len(p.TransitionRequests()); n != 0 {
		t.Fatalf("Jev was asked %d times about the owner's decision", n)
	}
}

func heldDrift(t *testing.T, key string) (*journey.ShedFixture, journey.Drifter, config.WorkstreamID, coreadapter.Operation) {
	t.Helper()
	f, stream, repository, _ := journey.NewFinalFixtureWith(t, key, "")
	f.S.Cfg.Shed.MaxBounces = 2
	d := journey.Drifter{Foreman: cadenceForeman(f, repository, "1h")}
	_, _, op := journey.ConflictedDrift(t, f, d, stream)
	journey.AwaitResolution(t, f.S, d, stream, op, "its mason's resolution")
	w := journey.ResolutionWorkspace(t, f, stream)
	for review := 1; review <= 2; review++ {
		fixturekit.Must(t, os.WriteFile(filepath.Join(w.Path, "CODEOWNERS"), []byte(fmt.Sprintf("/internal/ @feature%d\n", review)), 0600))
		journey.CompleteDriftTurn(t, f, repository, stream, intake.DriftMasonAgent, journey.ResolvedDone("Resolved"))
		journey.AwaitResolution(t, f.S, d, stream, op, fmt.Sprintf("review %d", review))
		journey.CompleteDriftTurn(t, f, repository, stream, intake.DriftReviewerAgent, journey.DriftVerdict(t, rejectedResolution))
		if review == 1 {
			journey.AwaitResolution(t, f.S, d, stream, op, "its mason's answer to review 1")
		}
	}
	return f, d, stream, op
}

func TestAHeldDriftRebaseFollowsItsDefault(t *testing.T) {
	t.Parallel()
	f, d, stream, op := heldDrift(t, "drift-route-held")
	p := &jevfixture.Transitions{}
	journey.InspectForTest(f.S).SetRouterJudge(jevfixture.TransitionJudge(f.S.Now, p))

	result, err := journey.AttemptOperation(t, f.S, d.Repository, stream, op, d)
	if err != nil || result.Outcome != "succeeded" || !strings.Contains(result.Evidence, "drift rebase 1 is held for the owner") {
		t.Fatalf("the drift rebase at max_bounces %+v %v", result, err)
	}
	held := lastOn(t, d.Repository, stream, lifecycle.DriftSubject)
	if held.To != lifecycle.DriftHeld+"-1" || held.DefaultTo != lifecycle.DriftHeld+"-1" || held.DecidedBy != trace.DecidedByCode || held.Departed() {
		t.Fatalf("the hold %+v", held)
	}
	records := routeFor(t, d.Repository, stream, "drift.review_rejected")
	if len(records) != 1 || records[0].Outcome != router.OutcomeFollowed || records[0].Default != "held" || records[0].Target != "held" {
		t.Fatalf("route %+v", records)
	}
}

func TestADriftHoldAtTheBounceLimitOffersNoRetry(t *testing.T) {
	t.Parallel()
	f, d, stream, op := heldDrift(t, "drift-route-hard")
	p := departTo("retried", 0.99)
	journey.InspectForTest(f.S).SetRouterJudge(jevfixture.TransitionJudge(f.S.Now, p))

	result, err := journey.AttemptOperation(t, f.S, d.Repository, stream, op, d)
	if err != nil || result.Outcome != "succeeded" {
		t.Fatalf("the drift rebase at max_bounces %+v %v", result, err)
	}
	held := lastOn(t, d.Repository, stream, lifecycle.DriftSubject)
	if held.To != lifecycle.DriftHeld+"-1" || held.DecidedBy != trace.DecidedByCode || held.Departed() {
		t.Fatalf("the hold %+v", held)
	}
	records := routeFor(t, d.Repository, stream, "drift.review_rejected")
	if len(records) != 1 || !slices.Equal(records[0].Offered, []string{"held"}) || records[0].Outcome != router.OutcomeFollowed || records[0].JevRequested {
		t.Fatalf("route %+v", records)
	}
	if n := len(p.TransitionRequests()); n != 0 {
		t.Fatalf("Jev was asked %d times about a hard limit with one legal target", n)
	}
}
