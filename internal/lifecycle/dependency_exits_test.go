package lifecycle_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func dropsAt(p float64) *jevfixture.Transitions {
	return &jevfixture.Transitions{Script: func(a jevfixture.Ask) jevfixture.Reply {
		if a.Trigger == lifecycle.TriggerUpstreamTerminal || a.Trigger == lifecycle.TriggerBaseUnavailable {
			return jevfixture.Departs(a, lifecycle.DependencyWaived, p, 0.95)
		}
		return jevfixture.DefaultFits(a)
	}}
}

func endStream(t *testing.T, repo *trace.Repository, at time.Time, stream config.WorkstreamID) {
	t.Helper()
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "ended-" + string(stream), Revision: 1, Project: repo.Project(), Workstream: stream, At: at, Actor: intake.OwnerActor, Cause: "owner"}
	_, err := repo.SetFeatureState(context.Background(), h, intake.AbandonedState, "abandoned for the test")
	fixturekit.Must(t, err)
}

func (f *edgeGraphFixture) reopened(t *testing.T) {
	t.Helper()
	f.repository.Close()
	repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	fixturekit.Must(t, err)
	t.Cleanup(func() { repo.Close() })
	f.repository = repo
}

func (f *edgeGraphFixture) waitState(t *testing.T) trace.WorkflowState {
	t.Helper()
	state, err := f.repository.Workflow(f.downstream, lifecycle.EdgeWaitSubject)
	fixturekit.Must(t, err)
	return state
}

func TestADependentOnAnAbandonedUpstreamRaisesAnExitThatKeepsTheWaitByDefault(t *testing.T) {
	t.Parallel()
	f := newEdgeGraphFixture(t, trace.EdgeHardBlock)
	p := &jevfixture.Transitions{}
	routeJev(f.s, p)
	f.resolvePasses(t)
	endStream(t, f.repository, f.s.now(), f.upstream)
	f.resolvePasses(t)

	records := routeFor(t, f.repository, f.downstream, lifecycle.TriggerUpstreamTerminal)
	if len(records) != 1 || records[0].State != router.StateCommitted || records[0].Default != lifecycle.DependencyKept || records[0].Outcome != router.OutcomeFollowed || records[0].Target != lifecycle.DependencyKept {
		t.Fatalf("route %+v", records)
	}
	kept := lastOn(t, f.repository, f.downstream, lifecycle.DependencySubject(f.upstream, trace.EdgeHardBlock))
	if kept.To != lifecycle.DependencyKept || kept.DefaultTo != lifecycle.DependencyKept || kept.DecidedBy != trace.DecidedByCode || kept.Departed() {
		t.Fatalf("the decision %+v", kept)
	}
	if state := f.waitState(t); state.Value != "waiting" {
		t.Fatalf("the edge wait is %+v", state)
	}
	if !f.holdsMason(t, f.downstream) {
		t.Fatal("keeping the dependency released the dependent")
	}
	f.resolvePasses(t)
	f.resolvePasses(t)
	if n := len(p.TransitionRequests()); n != 1 {
		t.Fatalf("Jev was asked %d times for one piece of evidence", n)
	}
	if again := lastOn(t, f.repository, f.downstream, lifecycle.DependencySubject(f.upstream, trace.EdgeHardBlock)); again.ID != kept.ID {
		t.Fatalf("a later pass decided again: %+v", again)
	}
	if waived, err := lifecycle.EdgeWaived(f.repository, f.downstream, f.upstream, trace.EdgeHardBlock); err != nil || waived {
		t.Fatalf("a kept dependency is waived: %v %v", waived, err)
	}
}

func TestJevDropsADependencyOnAnAbandonedUpstream(t *testing.T) {
	t.Parallel()
	f := newEdgeGraphFixture(t, trace.EdgeHardBlock)
	p := dropsAt(0.95)
	routeJev(f.s, p)
	specBefore, _, found, err := f.repository.ProjectSpecOf(f.downstream)
	fixturekit.Must(t, err)
	if !found || len(specBefore.Graph) != 1 {
		t.Fatalf("the shared project spec %+v", specBefore)
	}
	f.resolvePasses(t)
	if !f.holdsMason(t, f.downstream) {
		t.Fatal("the dependent was not held before the upstream ended")
	}
	endStream(t, f.repository, f.s.now(), f.upstream)
	f.resolvePasses(t)

	subject := lifecycle.DependencySubject(f.upstream, trace.EdgeHardBlock)
	dropped := lastOn(t, f.repository, f.downstream, subject)
	if dropped.To != lifecycle.DependencyWaived || dropped.DefaultTo != lifecycle.DependencyKept || dropped.DecidedBy != trace.DecidedByJev || dropped.Judgment == "" || !dropped.Departed() {
		t.Fatalf("the departure %+v", dropped)
	}
	waivers, revision, err := lifecycle.EdgeWaiversAt(f.repository, f.downstream)
	fixturekit.Must(t, err)
	if revision != 1 || len(waivers.Waivers) != 1 {
		t.Fatalf("waivers %+v at revision %d", waivers, revision)
	}
	w := waivers.Waivers[0]
	if w.Upstream != f.upstream || w.Type != trace.EdgeHardBlock || w.DecidedBy != trace.DecidedByJev || w.Judgment != dropped.Judgment || w.Ended != lifecycle.EndedAbandoned || w.Trigger != lifecycle.TriggerUpstreamTerminal {
		t.Fatalf("the waiver %+v", w)
	}
	specAfter, _, _, err := f.repository.ProjectSpecOf(f.downstream)
	fixturekit.Must(t, err)
	if !reflect.DeepEqual(specBefore, specAfter) {
		t.Fatalf("dropping the dependency changed the shared project spec: %+v", specAfter)
	}
	records := routeFor(t, f.repository, f.downstream, lifecycle.TriggerUpstreamTerminal)
	if len(records) != 1 || records[0].Outcome != router.OutcomeDeparted || records[0].Target != lifecycle.DependencyWaived || records[0].Judgment != dropped.Judgment {
		t.Fatalf("route %+v", records)
	}
	released := f.waitState(t)
	if released.Value != "available" || released.Version != 2 {
		t.Fatalf("the edge wait after the drop %+v", released)
	}

	check := func(when string) {
		t.Helper()
		if f.holdsMason(t, f.downstream) {
			t.Fatalf("%s: the dependent is still held", when)
		}
		if waiting, err := lifecycle.EdgeWaiting(f.repository, f.downstream); err != nil || waiting {
			t.Fatalf("%s: waiting = %v, %v", when, waiting, err)
		}
		if wait, err := lifecycle.EdgeWaitDisplay(f.repository, f.downstream); err != nil || wait != "" {
			t.Fatalf("%s: wait message %q, %v", when, wait, err)
		}
		if state := f.waitState(t); state.Value != "available" || state.Version != 2 {
			t.Fatalf("%s: the edge wait was recreated: %+v", when, state)
		}
		observed, _, _, err := lifecycle.EdgeObservationAt(f.repository, f.downstream)
		fixturekit.Must(t, err)
		for _, e := range observed.Edges {
			if e.Merged {
				t.Fatalf("%s: a waiver was recorded as a merge: %+v", when, e)
			}
		}
	}
	check("after the drop")
	for i := 0; i < 3; i++ {
		f.resolvePasses(t)
	}
	check("after repeated reconciliation")
	if n := len(p.TransitionRequests()); n != 1 {
		t.Fatalf("Jev was asked %d times", n)
	}
	f.reopened(t)
	check("after a restart")
	f.resolvePasses(t)
	f.resolvePasses(t)
	check("after reconciling again after the restart")
	if n := len(p.TransitionRequests()); n != 1 {
		t.Fatalf("Jev was asked %d times after the restart", n)
	}
	if n := departuresInFeed(t, f.s, f.repository, f.downstream); n != 1 {
		t.Fatalf("%d departures in the feed, want one", n)
	}
}

func TestADropBelowTheBarHoldsTheDependentAndWakesTheChief(t *testing.T) {
	t.Parallel()
	f := newEdgeGraphFixture(t, trace.EdgeHardBlock)
	p := dropsAt(0.8)
	routeJev(f.s, p)
	f.resolvePasses(t)
	endStream(t, f.repository, f.s.now(), f.upstream)
	f.resolvePasses(t)
	f.resolvePasses(t)

	records := routeFor(t, f.repository, f.downstream, lifecycle.TriggerUpstreamTerminal)
	if len(records) != 1 || records[0].State != router.StateHeld || records[0].Outcome != router.OutcomeHeld || records[0].Hold != router.HoldBelowBar {
		t.Fatalf("route %+v", records)
	}
	if waivers, _, err := lifecycle.EdgeWaiversAt(f.repository, f.downstream); err != nil || len(waivers.Waivers) != 0 {
		t.Fatalf("a held exit dropped the dependency: %+v %v", waivers, err)
	}
	if state, err := f.repository.Workflow(f.downstream, lifecycle.DependencySubject(f.upstream, trace.EdgeHardBlock)); err != nil || state.Version != 0 {
		t.Fatalf("a held exit moved the decision: %+v %v", state, err)
	}
	if !f.holdsMason(t, f.downstream) {
		t.Fatal("a held exit released the dependent")
	}
	if n := routeNotices(t, f.repository, f.downstream); n != 1 {
		t.Fatalf("%d notices for the held exit, want one", n)
	}
	if n := len(p.TransitionRequests()); n != 1 {
		t.Fatalf("Jev was asked %d times for one occurrence", n)
	}
}

func TestAClosedUpstreamPullRequestRaisesAnExitFromWaiting(t *testing.T) {
	t.Parallel()
	f := newEdgeGraphFixture(t, trace.EdgeHardBlock)
	p := dropsAt(0.95)
	routeJev(f.s, p)
	ctx := context.Background()
	f.resolvePasses(t)

	feature, err := f.repository.Workflow(f.upstream, trace.FeatureSubject)
	fixturekit.Must(t, err)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "deliver", Revision: 1, Project: f.repository.Project(), Workstream: f.upstream, At: f.s.now(), Actor: intake.OwnerActor, Cause: "fixture"}
	_, err = f.repository.MoveFeatureState(ctx, h, feature.Value, intake.DeliveredState, "fixture delivery")
	fixturekit.Must(t, err)
	cfg := f.s.about(f.repository)
	fixturekit.Must(t, (&publisher{s: f.s, repository: f.repository}).recordPublication(ctx, f.upstream, api.DeliveryPublication{Operation: "fixture-publication", Status: delivery.PublicationOpened, PullRequest: 7, URL: "https://example.invalid/pull/7", Upstream: cfg.Project.Upstream, Fork: cfg.Project.PushRepository(), Branch: intake.FeatureBranch(f.upstream), Commit: "head"}))
	f.resolvePasses(t)
	if records := routeFor(t, f.repository, f.downstream, lifecycle.TriggerUpstreamTerminal); len(records) != 0 {
		t.Fatalf("an open pull request raised an exit: %+v", records)
	}
	if !f.holdsMason(t, f.downstream) {
		t.Fatal("the dependent was released while the pull request was open")
	}

	doc, ok, err := delivery.OwnPullRequestOutcomeDocumentFor(f.repository, f.upstream, f.s.now(), "fixture", 7, delivery.OwnPullRequestOutcomeClosed)
	fixturekit.Must(t, err)
	if !ok {
		t.Fatal("no outcome document")
	}
	fixturekit.Must(t, f.repository.RecordDocuments(ctx, []trace.Document{doc}))
	f.resolvePasses(t)

	records := routeFor(t, f.repository, f.downstream, lifecycle.TriggerUpstreamTerminal)
	if len(records) != 1 || records[0].Outcome != router.OutcomeDeparted || records[0].Target != lifecycle.DependencyWaived {
		t.Fatalf("route %+v", records)
	}
	waivers, _, err := lifecycle.EdgeWaiversAt(f.repository, f.downstream)
	fixturekit.Must(t, err)
	if len(waivers.Waivers) != 1 || waivers.Waivers[0].Ended != lifecycle.EndedClosed {
		t.Fatalf("waivers %+v", waivers)
	}
	if f.holdsMason(t, f.downstream) {
		t.Fatal("the dependent is still held after the drop")
	}
}

func newTwoUpstreamFixture(t *testing.T) (*edgeGraphFixture, config.WorkstreamID) {
	t.Helper()
	f := newDebateFixture(t, 1, 1)
	f.stop(t)
	ctx := context.Background()
	repository, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	fixturekit.Must(t, err)
	t.Cleanup(func() { repository.Close() })
	first := config.WorkstreamID("w_00000000000000000000000000000c01")
	second := config.WorkstreamID("w_00000000000000000000000000000c02")
	dependent := config.WorkstreamID("w_00000000000000000000000000000c03")
	at := f.s.now()
	for _, id := range []config.WorkstreamID{first, second, dependent} {
		fixturekit.Must(t, repository.CreateWorkstream(ctx, id, at, intake.RegistrationActor))
	}
	g, err := trace.CreateWorkgroup(ctx, repository, []config.WorkstreamID{first, second, dependent}, at, intake.OwnerActor, "fixture")
	fixturekit.Must(t, err)
	spec := trace.ProjectSpec{Behavior: "The dependent needs both.", Graph: []trace.Edge{
		{From: first, To: dependent, Type: trace.EdgeHardBlock},
		{From: second, To: dependent, Type: trace.EdgeHardBlock},
	}}
	fixturekit.Must(t, trace.RecordProjectSpec(ctx, repository, g, spec, at, intake.OwnerActor, "fixture"))
	return &edgeGraphFixture{s: f.s, clock: f.clock, project: f.project, repository: repository, upstream: first, downstream: dependent}, second
}

func TestASecondUnresolvedEdgeStillBlocksAfterAWaiver(t *testing.T) {
	t.Parallel()
	f, second := newTwoUpstreamFixture(t)
	p := dropsAt(0.95)
	routeJev(f.s, p)
	f.resolvePasses(t)
	endStream(t, f.repository, f.s.now(), f.upstream)
	f.resolvePasses(t)

	if waived, err := lifecycle.EdgeWaived(f.repository, f.downstream, f.upstream, trace.EdgeHardBlock); err != nil || !waived {
		t.Fatalf("the first edge is not waived: %v %v", waived, err)
	}
	for i := 0; i < 3; i++ {
		if !f.holdsMason(t, f.downstream) {
			t.Fatal("the second, unresolved edge no longer blocks the dependent")
		}
		if waiting, err := lifecycle.EdgeWaiting(f.repository, f.downstream); err != nil || !waiting {
			t.Fatalf("waiting = %v, %v", waiting, err)
		}
		if wait, err := lifecycle.EdgeWaitDisplay(f.repository, f.downstream); err != nil || wait != lifecycle.EdgeWaitMessage(string(second)) {
			t.Fatalf("wait message %q, %v; want the second upstream's", wait, err)
		}
		if state := f.waitState(t); state.Value != "waiting" || state.Version != 1 {
			t.Fatalf("the edge wait %+v", state)
		}
		f.resolvePasses(t)
	}
	observed, _, _, err := lifecycle.EdgeObservationAt(f.repository, f.downstream)
	fixturekit.Must(t, err)
	for _, e := range observed.Edges {
		if e.Upstream == f.upstream {
			t.Fatalf("the waived edge is still observed: %+v", e)
		}
		if e.Merged {
			t.Fatalf("an edge is recorded merged: %+v", e)
		}
	}

	f.recordOwnerMerge(t, second, "d1e2f3a4b5c6d1e2f3a4b5c6d1e2f3a4b5c6d1e2")
	f.resolvePasses(t)
	if f.holdsMason(t, f.downstream) {
		t.Fatal("the dependent stayed held after the second upstream merged")
	}
	observed, _, _, err = lifecycle.EdgeObservationAt(f.repository, f.downstream)
	fixturekit.Must(t, err)
	merged := 0
	for _, e := range observed.Edges {
		if e.Merged {
			merged++
			if e.Upstream != second {
				t.Fatalf("the merge is attributed to %s", e.Upstream)
			}
		}
	}
	if merged != 1 {
		t.Fatalf("%d merged edges, want only the second", merged)
	}
}

func TestAWaivedMergeOrderEdgeFreesAnAlreadyPublishedDraft(t *testing.T) {
	t.Parallel()
	f := newEdgeGraphFixture(t, trace.EdgeMergeOrder)
	p := dropsAt(0.95)
	routeJev(f.s, p)
	ctx := context.Background()
	f.resolvePasses(t)

	feature, err := f.repository.Workflow(f.downstream, trace.FeatureSubject)
	fixturekit.Must(t, err)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "deliver", Revision: 1, Project: f.repository.Project(), Workstream: f.downstream, At: f.s.now(), Actor: intake.OwnerActor, Cause: "fixture"}
	_, err = f.repository.MoveFeatureState(ctx, h, feature.Value, intake.DeliveredState, "fixture delivery")
	fixturekit.Must(t, err)
	cfg := f.s.about(f.repository)
	fixturekit.Must(t, (&publisher{s: f.s, repository: f.repository}).recordPublication(ctx, f.downstream, api.DeliveryPublication{Operation: "fixture-publication", Status: delivery.PublicationOpened, PullRequest: 9, URL: "https://example.invalid/pull/9", Upstream: cfg.Project.Upstream, Fork: cfg.Project.PushRepository(), Branch: intake.FeatureBranch(f.downstream), Commit: "head", Draft: true}))
	if pending, err := lifecycle.WorkgroupEdgePending(f.repository, f.downstream); err != nil || pending {
		t.Fatalf("the draft is pending before anything changed: %v %v", pending, err)
	}
	if refs, err := lifecycle.PendingMergeOrderReferences(f.repository, f.downstream); err != nil || len(refs) != 1 {
		t.Fatalf("references before the waiver %v %v", refs, err)
	}

	endStream(t, f.repository, f.s.now(), f.upstream)
	f.resolvePasses(t)
	if waived, err := lifecycle.EdgeWaived(f.repository, f.downstream, f.upstream, trace.EdgeMergeOrder); err != nil || !waived {
		t.Fatalf("the edge is not waived: %v %v", waived, err)
	}
	if pending, err := lifecycle.WorkgroupEdgePending(f.repository, f.downstream); err != nil || !pending {
		t.Fatalf("the waived draft is not pending a fresh review and approval: %v %v", pending, err)
	}
	if refs, err := lifecycle.PendingMergeOrderReferences(f.repository, f.downstream); err != nil || len(refs) != 0 {
		t.Fatalf("the waived upstream is still stated as a dependency: %v %v", refs, err)
	}
	if _, waiting, err := lifecycle.UnresolvedMergeOrderEdge(f.repository, f.downstream); err != nil || waiting {
		t.Fatalf("publication is still blocked by the waived edge: %v %v", waiting, err)
	}
	if wait, err := lifecycle.EdgePublicationWait(f.repository, f.downstream); err != nil || wait != "" {
		t.Fatalf("publication wait %q, %v", wait, err)
	}
	if resolved, err := lifecycle.WorkgroupMergeOrderResolved(f.repository, f.downstream); err != nil || resolved != "" {
		t.Fatalf("a waiver is reported as a merge: %q %v", resolved, err)
	}
	if waived, err := lifecycle.WorkgroupMergeOrderWaived(f.repository, f.downstream); err != nil || waived != string(f.upstream) {
		t.Fatalf("waived summary %q, %v", waived, err)
	}
	if wait, err := lifecycle.EdgeWaitDisplay(f.repository, f.downstream); err != nil || wait != "" {
		t.Fatalf("wait message %q, %v", wait, err)
	}
	f.reopened(t)
	f.resolvePasses(t)
	if pending, err := lifecycle.WorkgroupEdgePending(f.repository, f.downstream); err != nil || !pending {
		t.Fatalf("the waived draft stopped being pending after a restart: %v %v", pending, err)
	}
}

func TestTheOwnerDropsADependencyAsAPinnedMove(t *testing.T) {
	t.Parallel()
	f := newEdgeGraphFixture(t, trace.EdgeHardBlock)
	p := &jevfixture.Transitions{}
	routeJev(f.s, p)
	ctx := context.Background()
	c := f.s.lifecycle()
	f.resolvePasses(t)

	if _, apiFault := c.DropDependencyOf(ctx, f.project, f.downstream, f.repository, lifecycle.DropDependencyRequest{Upstream: f.downstream}); apiFault == nil || apiFault.Code != api.Conflict {
		t.Fatalf("a dependency that does not exist was dropped: %v", apiFault)
	}
	out, apiFault := c.DropDependencyOf(ctx, f.project, f.downstream, f.repository, lifecycle.DropDependencyRequest{Upstream: f.upstream})
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	if out.Upstream != f.upstream || out.Type != trace.EdgeHardBlock {
		t.Fatalf("the response %+v", out)
	}
	dropped := lastOn(t, f.repository, f.downstream, lifecycle.DependencySubject(f.upstream, trace.EdgeHardBlock))
	if dropped.To != lifecycle.DependencyWaived || dropped.DecidedBy != trace.DecidedByOwner || dropped.Departed() || dropped.Judgment != "" || dropped.Actor != intake.OwnerActor {
		t.Fatalf("the owner's drop %+v", dropped)
	}
	records := routeFor(t, f.repository, f.downstream, lifecycle.TriggerUpstreamTerminal)
	if len(records) != 1 || records[0].Authority != router.AuthorityOwner || records[0].Outcome != router.OutcomeFollowed || records[0].JevRequested {
		t.Fatalf("route %+v", records)
	}
	if n := len(p.TransitionRequests()); n != 0 {
		t.Fatalf("Jev was asked %d times about the owner's decision", n)
	}
	waivers, _, err := lifecycle.EdgeWaiversAt(f.repository, f.downstream)
	fixturekit.Must(t, err)
	if len(waivers.Waivers) != 1 || waivers.Waivers[0].DecidedBy != trace.DecidedByOwner || waivers.Waivers[0].Ended != "owner" {
		t.Fatalf("waivers %+v", waivers)
	}
	if f.holdsMason(t, f.downstream) {
		t.Fatal("the dependent is still held after the owner's drop")
	}
	if _, apiFault := c.DropDependencyOf(ctx, f.project, f.downstream, f.repository, lifecycle.DropDependencyRequest{Upstream: f.upstream}); apiFault == nil || apiFault.Code != api.Conflict {
		t.Fatalf("a dropped dependency was dropped again: %v", apiFault)
	}
}

func sealedEdgeFixture(t *testing.T, f *shedFixture) (*edgeGraphFixture, string) {
	t.Helper()
	ctx := context.Background()
	f.upstream(t)
	f.stop(t)
	repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	fixturekit.Must(t, err)
	t.Cleanup(func() { repo.Close() })
	up, tip := sealDependent(t, f, repo, config.WorkspacesGit, "", map[string]string{"upstream.go": "upstream\n"})
	dep, _ := sealDependent(t, f, repo, config.WorkspacesGit, "", map[string]string{"dependent.go": "dependent\n"})
	at := f.s.now()
	g, err := trace.CreateWorkgroup(ctx, repo, []config.WorkstreamID{up, dep}, at, intake.OwnerActor, "fixture")
	fixturekit.Must(t, err)
	spec := trace.ProjectSpec{Behavior: "The dependent needs the upstream.", Graph: []trace.Edge{{From: up, To: dep, Type: trace.EdgeHardBlock}}}
	fixturekit.Must(t, trace.RecordProjectSpec(ctx, repo, g, spec, at, intake.OwnerActor, "fixture"))
	return &edgeGraphFixture{s: f.s, clock: f.clock, project: f.project, repository: repo, upstream: up, downstream: dep}, tip
}

func TestAnUpstreamMergedOutsideTheFactoryRaisesAnExitAndTheDependentGoesOn(t *testing.T) {
	t.Parallel()
	sf := newDebateFixture(t, 1, 1)
	p := &jevfixture.Transitions{}
	routeJev(sf.s, p)
	f, _ := sealedEdgeFixture(t, sf)
	f.resolvePasses(t)
	if !f.holdsMason(t, f.downstream) {
		t.Fatal("the dependent was not held before the upstream merged")
	}

	endStream(t, f.repository, f.s.now(), f.upstream)
	f.resolvePasses(t)
	if records := routeFor(t, f.repository, f.downstream, lifecycle.TriggerUpstreamIntegrated); len(records) != 0 {
		t.Fatalf("an upstream whose changes are not in the base branch raised the integrated exit: %+v", records)
	}
	if records := routeFor(t, f.repository, f.downstream, lifecycle.TriggerUpstreamTerminal); len(records) != 1 {
		t.Fatalf("route %+v", records)
	}

	merged := advanceUpstream(t, sf, map[string]string{"upstream.go": "upstream\n", "UPSTREAM.md": "merged by hand\n"})
	f.resolvePasses(t)
	records := routeFor(t, f.repository, f.downstream, lifecycle.TriggerUpstreamIntegrated)
	if len(records) != 1 || records[0].State != router.StateCommitted || records[0].Default != lifecycle.DependencyAdopted || records[0].Outcome != router.OutcomeFollowed || records[0].Target != lifecycle.DependencyAdopted {
		t.Fatalf("route %+v", records)
	}
	adopted := lastOn(t, f.repository, f.downstream, lifecycle.DependencySubject(f.upstream, trace.EdgeHardBlock))
	if adopted.To != lifecycle.DependencyAdopted || adopted.From != lifecycle.DependencyKept || adopted.DecidedBy != trace.DecidedByCode || adopted.Departed() {
		t.Fatalf("the decision %+v", adopted)
	}
	observed, _, _, err := lifecycle.EdgeObservationAt(f.repository, f.downstream)
	fixturekit.Must(t, err)
	if len(observed.Edges) != 1 || !observed.Edges[0].Merged || observed.Edges[0].Commit != merged || observed.Edges[0].Reference == "" {
		t.Fatalf("the observation %+v", observed)
	}
	if waivers, _, err := lifecycle.EdgeWaiversAt(f.repository, f.downstream); err != nil || len(waivers.Waivers) != 0 {
		t.Fatalf("going on because the changes are integrated wrote a waiver: %+v %v", waivers, err)
	}
	if state := f.waitState(t); state.Value != "available" || state.Version != 2 {
		t.Fatalf("the edge wait %+v", state)
	}
	if f.holdsMason(t, f.downstream) {
		t.Fatal("the dependent is still held after its upstream's changes were found in the base branch")
	}
	f.resolvePasses(t)
	f.resolvePasses(t)
	if state := f.waitState(t); state.Value != "available" || state.Version != 2 {
		t.Fatalf("the edge wait after reconciliation %+v", state)
	}
}

func parkOnAbandonedBase(t *testing.T, f *shedFixture) (*trace.Repository, config.WorkstreamID, config.WorkstreamID) {
	t.Helper()
	ctx := context.Background()
	f.upstream(t)
	f.stop(t)
	repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	fixturekit.Must(t, err)
	t.Cleanup(func() { repo.Close() })
	base, _ := sealDependent(t, f, repo, config.WorkspacesGit, "", map[string]string{"base.go": "base\n"})
	stream, _ := sealDependent(t, f, repo, config.WorkspacesGit, base, map[string]string{"dependent.go": "dependent\n"})
	endStream(t, repo, f.s.now(), base)
	fixturekit.Must(t, f.s.baseWaitPass(ctx, repo))
	if waiting, err := lifecycle.BaseWaiting(repo, stream); err != nil || !waiting {
		t.Fatalf("the dependent is not parked: %v %v", waiting, err)
	}
	return repo, base, stream
}

func TestAnAbandonedBaseMergedByHandRaisesAnExitAndTheDependentGoesOn(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	p := &jevfixture.Transitions{}
	routeJev(f.s, p)
	repo, base, stream := parkOnAbandonedBase(t, f)
	ctx := context.Background()
	upstream := advanceUpstream(t, f, map[string]string{"base.go": "base\n", "UPSTREAM.md": "upstream\n"})

	fixturekit.Must(t, f.s.edgeResolverPass(ctx, repo))
	records := routeFor(t, repo, stream, lifecycle.TriggerBaseIntegrated)
	if len(records) != 1 || records[0].State != router.StateCommitted || records[0].Default != lifecycle.DependencyAdopted || records[0].Outcome != router.OutcomeFollowed {
		t.Fatalf("route %+v", records)
	}
	decided := lastOn(t, repo, stream, lifecycle.DependencySubject(base, lifecycle.BaseEdgeType))
	if decided.To != lifecycle.DependencyAdopted || decided.DecidedBy != trace.DecidedByCode || decided.Departed() {
		t.Fatalf("the decision %+v", decided)
	}
	if waiting, err := lifecycle.BaseWaiting(repo, stream); err != nil || waiting {
		t.Fatalf("the dependent is still parked: %v %v", waiting, err)
	}
	if observed, _, err := lifecycle.BaseObservationAt(repo, stream); err != nil || observed.Base.Commit != upstream || observed.Base.Workstream != "" || observed.Unavailable {
		t.Fatalf("the base observation %+v: %v", observed, err)
	}
	if waivers, _, err := lifecycle.EdgeWaiversAt(repo, stream); err != nil || len(waivers.Waivers) != 0 {
		t.Fatalf("going on because the base is integrated wrote a waiver: %+v %v", waivers, err)
	}
	fixturekit.Must(t, f.s.baseWaitPass(ctx, repo))
	if state, err := repo.Workflow(stream, lifecycle.BaseWaitSubject); err != nil || state.Value != "available" {
		t.Fatalf("the base wait after its next pass %+v: %v", state, err)
	}
}

func TestJevDropsAnAbandonedBaseWhoseChangesAreNotInUpstream(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	p := dropsAt(0.95)
	routeJev(f.s, p)
	repo, base, stream := parkOnAbandonedBase(t, f)
	ctx := context.Background()
	upstream := advanceUpstream(t, f, map[string]string{"UPSTREAM.md": "upstream\n"})

	fixturekit.Must(t, f.s.edgeResolverPass(ctx, repo))
	records := routeFor(t, repo, stream, lifecycle.TriggerBaseUnavailable)
	if len(records) != 1 || records[0].Outcome != router.OutcomeDeparted || records[0].Target != lifecycle.DependencyWaived {
		t.Fatalf("route %+v", records)
	}
	dropped := lastOn(t, repo, stream, lifecycle.DependencySubject(base, lifecycle.BaseEdgeType))
	if dropped.To != lifecycle.DependencyWaived || dropped.DefaultTo != lifecycle.DependencyKept || dropped.DecidedBy != trace.DecidedByJev || !dropped.Departed() || dropped.Judgment == "" {
		t.Fatalf("the departure %+v", dropped)
	}
	waivers, _, err := lifecycle.EdgeWaiversAt(repo, stream)
	fixturekit.Must(t, err)
	if len(waivers.Waivers) != 1 || waivers.Waivers[0].Upstream != base || waivers.Waivers[0].Type != lifecycle.BaseEdgeType || waivers.Waivers[0].DecidedBy != trace.DecidedByJev {
		t.Fatalf("waivers %+v", waivers)
	}
	if waiting, err := lifecycle.BaseWaiting(repo, stream); err != nil || waiting {
		t.Fatalf("the dependent is still parked: %v %v", waiting, err)
	}
	if observed, _, err := lifecycle.BaseObservationAt(repo, stream); err != nil || observed.Base.Commit != upstream || observed.Base.Workstream != "" {
		t.Fatalf("the base observation %+v: %v", observed, err)
	}
	for i := 0; i < 3; i++ {
		fixturekit.Must(t, f.s.baseWaitPass(ctx, repo))
		fixturekit.Must(t, f.s.edgeResolverPass(ctx, repo))
	}
	if waiting, err := lifecycle.BaseWaiting(repo, stream); err != nil || waiting {
		t.Fatalf("the dependent was parked again: %v %v", waiting, err)
	}
	if n := len(p.TransitionRequests()); n != 1 {
		t.Fatalf("Jev was asked %d times", n)
	}
}

func TestAWaivedMergeOrderEdgeLetsTheDraftBeMarkedReadyAfterAFreshApproval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repository, report, repo2, upstream := newCrossRepoEdgeDependent(t)
	routeJev(f.s, dropsAt(0.95))
	home := filepath.Dir(f.clone)
	fork := filepath.Join(home, "remotes", "owner", "dagger.git")
	fixturekit.ConfigureDeliverySigner(t, f.clone)
	fixturekit.Must(t, os.MkdirAll(filepath.Dir(fork), 0700))
	fixturekit.DemoGit(t, home, "init", "--quiet", "--bare", fork)
	fixturekit.DemoGit(t, home, "-C", f.clone, "remote", "add", "origin", fork)
	pull := &fixturekit.FakePulls{SyncHead: true, Fork: func() string {
		return strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", fork, "for-each-ref", "--format=%(objectname)", "refs/heads/"+intake.FeatureBranch(stream)))
	}}
	f.s.mu.Lock()
	f.s.options.PullRequests = pull
	f.s.mu.Unlock()

	upstreamPullRequest := api.DeliveryPublication{Status: delivery.PublicationOpened, Upstream: "owner/other", Fork: "owner/other-fork", Branch: intake.FeatureBranch(upstream), Commit: "d1e2f3a4b5c6d1e2f3a4b5c6d1e2f3a4b5c6d1e2", PullRequest: 77, URL: "https://example.invalid/owner/other/pull/77"}
	recordUpstreamOwnPullRequest(t, f.s, repo2, upstream, upstreamPullRequest)
	pull.Mu.Lock()
	pull.Prs = append(pull.Prs, pulls.PullRequest{Number: upstreamPullRequest.PullRequest, URL: upstreamPullRequest.URL, State: "open", HeadRepository: upstreamPullRequest.Fork, Head: upstreamPullRequest.Branch, HeadCommit: upstreamPullRequest.Commit, Base: "main"})
	pull.Mu.Unlock()
	fixturekit.Must(t, f.s.edgeResolverPass(ctx, repository))

	pub := &publicationFixture{shedFixture: f, stream: stream, repository: repository, report: report, fork: fork, pulls: pull}
	pub.publishDraft(t)
	pub.approve(t, nil)
	result, err := pub.publisher().Apply(ctx, pub.request(t))
	if err != nil || result.Outcome != "succeeded" {
		t.Fatalf("publication %+v: %v", result, err)
	}
	pull.Mu.Lock()
	if len(pull.Prs) != 2 || !pull.Prs[1].Draft || pull.Readies != 0 {
		t.Fatalf("the dependent's pull request is not a draft: %+v, ready calls %d", pull.Prs, pull.Readies)
	}
	pull.Mu.Unlock()
	if pending, err := lifecycle.UpstreamDeliveryPending(repository, f.s.about(repository), stream); err != nil || pending {
		t.Fatalf("maintenance is pending before anything changed: %v %v", pending, err)
	}

	endStream(t, repo2, f.s.now(), upstream)
	f.clock.Mu.Lock()
	f.clock.At = f.clock.At.Add(11 * time.Minute)
	f.clock.Mu.Unlock()
	fixturekit.Must(t, f.s.edgeResolverPass(ctx, repository))
	if waived, err := lifecycle.EdgeWaived(repository, stream, upstream, trace.EdgeMergeOrder); err != nil || !waived {
		t.Fatalf("the edge is not waived: %v %v", waived, err)
	}
	for i := 0; i < 2; i++ {
		fixturekit.Must(t, f.s.edgeResolverPass(ctx, repository))
		fixturekit.Must(t, f.s.edgeWaitPass(ctx, repository))
	}
	if pending, err := lifecycle.UpstreamDeliveryPending(repository, f.s.about(repository), stream); err != nil || !pending {
		t.Fatalf("the waived draft is not pending a fresh review and approval: %v %v", pending, err)
	}
	if resolved, err := lifecycle.WorkgroupMergeOrderResolved(repository, stream); err != nil || resolved != "" {
		t.Fatalf("a waiver is reported as a merge: %q %v", resolved, err)
	}

	a := &finalReviewer{s: f.s, repository: repository}
	f.clock.Mu.Lock()
	f.clock.At = f.clock.At.Add(time.Second)
	f.clock.Mu.Unlock()
	fixturekit.Must(t, a.Pass(ctx))
	op := fixturekit.ActionOperation(t, repository, stream, lifecycle.DeliveryReviewAction)
	in, err := delivery.DecodeFinalReview(op)
	fixturekit.Must(t, err)
	if in.EdgeWaived != string(upstream) || in.EdgeResolved != "" || !in.Maintenance {
		t.Fatalf("the fresh review does not pin the waiver: %+v", in)
	}
	f.finalTurn(in.Review, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Dependency dropped", "findings": []any{}})
		return err
	})
	if res := settleOperation(t, f.s, repository, stream, op, a); res.Outcome != "succeeded" {
		t.Fatal(res)
	}
	fresh, found, err := delivery.LatestFinalReport(repository, stream)
	fixturekit.Must(t, err)
	if !found || !fresh.Maintenance || fresh.EdgeWaived != string(upstream) || fresh.EdgeResolved != "" || report.EdgeWaived != "" {
		t.Fatalf("the fresh review %+v", fresh)
	}
	pull.Mu.Lock()
	readies := pull.Readies
	pull.Mu.Unlock()
	if readies != 0 {
		t.Fatal("the pull request was marked ready before any fresh approval")
	}
	freshApproval := pub.approve(t, nil)
	if freshApproval.Review != fresh.Review {
		t.Fatalf("the fresh approval read review %d, want %d", freshApproval.Review, fresh.Review)
	}
	fixturekit.Must(t, pub.publisher().Pass(ctx))
	op2 := fixturekit.ActionOperation(t, repository, stream, lifecycle.PublishUpstreamAction)
	result2, err := pub.publisher().Apply(ctx, op2)
	if err != nil || result2.Outcome != "succeeded" {
		t.Fatalf("the fresh publication %+v: %v", result2, err)
	}
	pull.Mu.Lock()
	draft, readies, creates := pull.Prs[1].Draft, pull.Readies, pull.Creates
	pull.Mu.Unlock()
	if draft || readies != 1 || creates != 1 {
		t.Fatalf("draft %v, marked ready %d times, pull requests opened %d times; want a ready pull request marked once and opened once", draft, readies, creates)
	}
}

func TestTheChiefOfStaffDropsADependency(t *testing.T) {
	t.Parallel()
	f := newEdgeGraphFixture(t, trace.EdgeHardBlock)
	p := &jevfixture.Transitions{}
	routeJev(f.s, p)
	host, err := f.repository.ChiefOfStaffHost(f.downstream)
	fixturekit.Must(t, err)
	scope := intakefixture.ClaimChiefTurn(t, f.repository, f.project, host, func() time.Time { return fixturekit.DemoStart }, intakefixture.ChiefTurnInput{
		Turn: "events_drop", Prompt: "The upstream will not merge.", Actor: intake.ServiceActor, Cause: "fixture", Token: "token-events_drop", Directory: "events_drop",
	})
	f.resolvePasses(t)
	controls := &runtimeControls{}
	controls.service.Store(f.s)
	tool := controls.dropDependency(f.repository, scope)
	out := callMove(t, tool, fmt.Sprintf(`{"upstream":%q,"reason":"The dependent works without it.","workstream":%q}`, f.upstream, f.downstream))
	if !strings.Contains(out, `"recorded":true`) {
		t.Fatalf("the chief's drop: %s", out)
	}
	dropped := lastOn(t, f.repository, f.downstream, lifecycle.DependencySubject(f.upstream, trace.EdgeHardBlock))
	if dropped.To != lifecycle.DependencyWaived || dropped.DecidedBy != trace.DecidedByChief || dropped.Judgment != "" {
		t.Fatalf("the chief's drop %+v", dropped)
	}
	if n := len(p.TransitionRequests()); n != 0 {
		t.Fatalf("Jev was asked %d times about the chief's decision", n)
	}
	waivers, _, err := lifecycle.EdgeWaiversAt(f.repository, f.downstream)
	fixturekit.Must(t, err)
	if len(waivers.Waivers) != 1 || waivers.Waivers[0].DecidedBy != trace.DecidedByChief || waivers.Waivers[0].Ended != trace.ChiefOfStaff {
		t.Fatalf("waivers %+v", waivers)
	}
	if f.holdsMason(t, f.downstream) {
		t.Fatal("the dependent is still held after the chief's drop")
	}
}
