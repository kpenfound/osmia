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

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/trace"
)

// routeJev makes the transition router of the fixture's service ask a Jev that
// answers as p does.
func routeJev(s *Service, p *jevfixture.Transitions) {
	service.InspectForTest(s.core()).SetRouterJudge(jevfixture.TransitionJudge(s.now, p))
}

// departTo scripts a Jev that finds the default does not fit and picks target
// at probability p, corroborating it.
func departTo(target string, p float64) *jevfixture.Transitions {
	return &jevfixture.Transitions{Script: func(a jevfixture.Ask) jevfixture.Reply { return jevfixture.Departs(a, target, p, 0.95) }}
}

// lastOn returns the latest transition of the subject.
func lastOn(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, subject string) trace.Transition {
	t.Helper()
	transitions, err := trace.Read[trace.Transition](repo, stream)
	must(t, err)
	var out trace.Transition
	for _, tr := range transitions {
		if tr.Subject == subject {
			out = tr
		}
	}
	return out
}

// routeFor returns the route record of the workstream's exits with the trigger.
func routeFor(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, trigger string) []router.Record {
	t.Helper()
	docs, err := trace.Read[trace.Document](repo, stream)
	must(t, err)
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
		must(t, json.Unmarshal([]byte(latest[id].Content), &rec))
		if rec.Trigger == trigger {
			out = append(out, rec)
		}
	}
	return out
}

// departuresInFeed counts the transitions the workstream's feed labels as
// departures from a default.
func departuresInFeed(t *testing.T, s *Service, repo *trace.Repository, stream config.WorkstreamID) int {
	t.Helper()
	s.setSole(runtimeFor(repo))
	feed, api := service.InspectForTest(s.core()).Feed(string(stream))
	if api != nil {
		t.Fatal(api.Message)
	}
	n := 0
	for _, e := range feed.Entries {
		if e.Transition != nil && e.Transition.Departed {
			if e.Transition.DecidedBy != trace.DecidedByJev || e.Transition.DefaultTo == e.Transition.To {
				t.Fatalf("feed entry %+v", e.Transition)
			}
			n++
		}
	}
	return n
}

// routeNotices counts the notices the router sent the chief of staff.
func routeNotices(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) int {
	t.Helper()
	outbox, err := repo.Outbox(stream)
	must(t, err)
	n := 0
	for _, e := range outbox {
		if e.Event.Kind == trace.NoticeKind && strings.HasPrefix(e.TransitionID, "route-") {
			n++
		}
	}
	return n
}

// With Jev finding the default fits, the wait of a hard-blocked dependent
// ends as it always has once its upstream merged, and the transition records
// the default it took. Entering the wait is code's own default.
func TestAnEdgeWaitThatClearsFollowsItsDefault(t *testing.T) {
	t.Parallel()
	f := newEdgeGraphFixture(t, trace.EdgeHardBlock)
	p := &jevfixture.Transitions{}
	routeJev(f.s, p)

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

// A scripted departure keeps a dependent waiting although its upstream looks
// merged. It is recorded as Jev's, shown as a departure in the feed, asked once
// for the observation, and a pick below its bar holds the wait instead.
func TestAScriptedDepartureKeepsAnEdgeWaitingOnceForAnObservation(t *testing.T) {
	t.Parallel()
	f := newEdgeGraphFixture(t, trace.EdgeHardBlock)
	p := departTo("waiting", 0.9)
	routeJev(f.s, p)
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

	// Jev kept the wait for this observation: the next passes do not ask again.
	f.resolvePasses(t)
	f.resolvePasses(t)
	if n := len(p.TransitionRequests()); n != 1 {
		t.Fatalf("Jev was asked %d times for one observation", n)
	}
	if again := lastOn(t, f.repository, f.downstream, lifecycle.EdgeWaitSubject); again.ID != kept.ID {
		t.Fatalf("a later pass moved the wait again: %+v", again)
	}

	if n := departuresInFeed(t, f.s, f.repository, f.downstream); n != 1 {
		t.Fatalf("%d departures in the feed, want one", n)
	}
}

func TestAnEdgeWaitIsHeldWhenTheBestTargetIsBelowItsBar(t *testing.T) {
	t.Parallel()
	f := newEdgeGraphFixture(t, trace.EdgeHardBlock)
	p := departTo("waiting", 0.55)
	routeJev(f.s, p)
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

// parkAndRelease parks a dependent on its abandoned base and pins the base
// observation to upstream, as moving it onto upstream records, so that the next
// base wait pass finds the wait cleared. It returns the dependent.
func parkAndRelease(t *testing.T, f *shedFixture) (*trace.Repository, config.WorkstreamID) {
	t.Helper()
	ctx := context.Background()
	f.upstream(t)
	f.stop(t)
	repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	t.Cleanup(func() { repo.Close() })
	base, _ := sealDependent(t, f, repo, config.WorkspacesGit, "", map[string]string{"base.go": "base\n"})
	stream, _ := sealDependent(t, f, repo, config.WorkspacesGit, base, map[string]string{"dependent.go": "dependent\n"})
	h := trace.Header{Schema: "osmia.trace.transition", Version: 1, ID: "owner-abandoned", Revision: 1, Project: repo.Project(), Workstream: base, At: f.s.now(), Actor: ownerActor, Cause: "owner"}
	_, err = repo.SetFeatureState(ctx, h, AbandonedState, "Merged out of factory")
	must(t, err)
	must(t, f.s.baseWaitPass(ctx, repo))
	parked := lastOn(t, repo, stream, baseWaitSubject)
	if parked.To != "waiting" || parked.DefaultTo != "waiting" || parked.DecidedBy != trace.DecidedByCode {
		t.Fatalf("parking the dependent %+v", parked)
	}
	upstream := advanceUpstream(t, f, map[string]string{"base.go": "base\n", "UPSTREAM.md": "upstream\n"})
	data, err := json.Marshal(lifecycle.BaseObservation{Base: seal.Base{Remote: "upstream", Branch: "main", Commit: upstream}})
	must(t, err)
	_, revision, err := baseObservationAt(repo, stream)
	must(t, err)
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "base-observation", Revision: revision + 1, Project: repo.Project(), Workstream: stream, At: f.s.now(), Actor: foremanActor, Cause: "fixture"}, Path: "base-observation.json", Content: string(data)}
	must(t, repo.RecordDocuments(ctx, []trace.Document{doc}))
	return repo, stream
}

// With Jev finding the default fits, a same-repo base wait that clears ends as
// it always has and its transition records the default it took.
func TestABaseWaitThatClearsFollowsItsDefault(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	p := &jevfixture.Transitions{}
	routeJev(f.s, p)
	repo, stream := parkAndRelease(t, f)

	must(t, f.s.baseWaitPass(context.Background(), repo))
	cleared := lastOn(t, repo, stream, baseWaitSubject)
	if cleared.To != "available" || cleared.DefaultTo != "available" || cleared.DecidedBy != trace.DecidedByCode || cleared.Departed() || cleared.Cause != "base-availability" {
		t.Fatalf("the wait clearing %+v", cleared)
	}
	if waiting, err := baseWaiting(repo, stream); err != nil || waiting {
		t.Fatalf("the cleared wait still parks the dependent: %v %v", waiting, err)
	}
	records := routeFor(t, repo, stream, "dep.base_available")
	if len(records) != 1 || records[0].Outcome != router.OutcomeFollowed || records[0].State != router.StateCommitted {
		t.Fatalf("route %+v", records)
	}
}

// A scripted departure keeps the dependent parked on its base: the park really
// holds it, the departure is recorded, and the wait is not asked about again for
// the same observation.
func TestAScriptedDepartureKeepsABaseWaitParked(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	p := departTo("waiting", 0.9)
	routeJev(f.s, p)
	repo, stream := parkAndRelease(t, f)
	ctx := context.Background()

	must(t, f.s.baseWaitPass(ctx, repo))
	kept := lastOn(t, repo, stream, baseWaitSubject)
	if kept.From != "waiting" || kept.To != "waiting" || kept.DefaultTo != "available" || kept.DecidedBy != trace.DecidedByJev || kept.Judgment == "" || !kept.Departed() {
		t.Fatalf("the departure %+v", kept)
	}
	if waiting, err := baseWaiting(repo, stream); err != nil || !waiting {
		t.Fatalf("the dependent is not parked after Jev kept the wait: %v %v", waiting, err)
	}
	must(t, f.s.baseWaitPass(ctx, repo))
	if n := len(p.TransitionRequests()); n != 1 {
		t.Fatalf("Jev was asked %d times for one observation", n)
	}
	if n := departuresInFeed(t, f.s, repo, stream); n != 1 {
		t.Fatalf("%d departures in the feed, want one", n)
	}
}

// The owner moving a parked workstream onto upstream is a pinned move: the
// router applies exactly the release with the base observation, records the
// owner as its decider and asks Jev nothing.
func TestMovingOntoUpstreamIsAPinnedOwnerMove(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	f.upstream(t)
	f.stop(t)
	p := departTo("waiting", 0.9)
	routeJev(f.s, p)
	ctx := context.Background()
	repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	defer repo.Close()
	base, _ := sealDependent(t, f, repo, config.WorkspacesGit, "", map[string]string{"base.go": "base\n"})
	stream, _ := sealDependent(t, f, repo, config.WorkspacesGit, base, map[string]string{"dependent.go": "dependent\n"})
	h := trace.Header{Schema: "osmia.trace.transition", Version: 1, ID: "owner-abandoned", Revision: 1, Project: repo.Project(), Workstream: base, At: f.s.now(), Actor: ownerActor, Cause: "owner"}
	_, err = repo.SetFeatureState(ctx, h, AbandonedState, "Merged out of factory")
	must(t, err)
	must(t, f.s.baseWaitPass(ctx, repo))
	advanceUpstream(t, f, map[string]string{"base.go": "base\n", "UPSTREAM.md": "upstream\n"})
	cfg := f.s.about(repo)
	g, err := featureWorkspaces(cfg, repo).Of(stream)
	must(t, err)

	if _, api := f.s.moveOntoUpstream(ctx, cfg, repo, g, stream, base); api != nil {
		t.Fatal(api)
	}
	released := lastOn(t, repo, stream, baseWaitSubject)
	if released.To != "available" || released.DecidedBy != trace.DecidedByOwner || released.DefaultTo != "available" || released.Departed() || released.Actor != ownerActor {
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

// heldDrift drives a drift rebase to the point its conflict resolution is
// rejected shed.max_bounces times, and returns what it needs to attempt the
// operation that holds it.
func heldDrift(t *testing.T, key string) (*shedFixture, drifter, config.WorkstreamID, coreadapter.Operation) {
	t.Helper()
	f, stream, repository, _ := newFinalFixture(t, key)
	f.s.cfg.Shed.MaxBounces = 2
	d := drifter{cadenceForeman(f, repository, "1h")}
	_, _, op := conflictedDrift(t, f, d, stream)
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution")
	w := resolutionWorkspace(t, f, stream)
	for review := 1; review <= 2; review++ {
		must(t, os.WriteFile(filepath.Join(w.Path, "CODEOWNERS"), []byte(fmt.Sprintf("/internal/ @feature%d\n", review)), 0600))
		completeDriftTurn(t, f, repository, stream, driftMasonAgent, resolvedDone("Resolved"))
		awaitResolution(t, f.s, d, stream, op, fmt.Sprintf("review %d", review))
		completeDriftTurn(t, f, repository, stream, driftReviewerAgent, driftVerdict(t, rejectedResolution))
		if review == 1 {
			awaitResolution(t, f.s, d, stream, op, "its mason's answer to review 1")
		}
	}
	return f, d, stream, op
}

// A drift rebase whose conflict resolution stopped is an exit: holding it for
// the owner is the default, entered as it always was, and its transition
// records the default it took.
func TestAHeldDriftRebaseFollowsItsDefault(t *testing.T) {
	t.Parallel()
	f, d, stream, op := heldDrift(t, "drift-route-held")
	p := &jevfixture.Transitions{}
	routeJev(f.s, p)

	result, err := attemptOperation(t, f.s, d.repository, stream, op, d)
	if err != nil || result.Outcome != "succeeded" || !strings.Contains(result.Evidence, "drift rebase 1 is held for the owner") {
		t.Fatalf("the drift rebase at max_bounces %+v %v", result, err)
	}
	held := lastOn(t, d.repository, stream, driftSubject)
	if held.To != driftHeld+"-1" || held.DefaultTo != driftHeld+"-1" || held.DecidedBy != trace.DecidedByCode || held.Departed() || held.Judgment == "" {
		t.Fatalf("the hold %+v", held)
	}
	records := routeFor(t, d.repository, stream, "drift.review_rejected")
	if len(records) != 1 || records[0].Outcome != router.OutcomeFollowed || records[0].Default != "held" || records[0].Target != "held" {
		t.Fatalf("route %+v", records)
	}
}

// A scripted departure asks for the drift rebase again instead of holding it:
// the drift subject enters the request of the next drift rebase through the
// shared effect, the operation of the held one ends, the transition names Jev
// and the default it left, and the new operation is published.
func TestAScriptedDepartureAsksForTheDriftRebaseAgain(t *testing.T) {
	t.Parallel()
	f, d, stream, op := heldDrift(t, "drift-route-retried")
	routeJev(f.s, departTo("retried", 0.9))

	result, err := attemptOperation(t, f.s, d.repository, stream, op, d)
	if err != nil || result.Outcome != "succeeded" {
		t.Fatalf("the drift rebase at max_bounces %+v %v", result, err)
	}
	moved := lastOn(t, d.repository, stream, driftSubject)
	if moved.To != "requested-2" || moved.DefaultTo != driftHeld+"-1" || moved.DecidedBy != trace.DecidedByJev || moved.Judgment == "" || !moved.Departed() || moved.ID != "drift-1-retried" {
		t.Fatalf("the departure %+v", moved)
	}
	if !strings.Contains(moved.Reason, "decide-transition table") || !strings.Contains(moved.Reason, "drift rebase 2 fetches") {
		t.Fatalf("the departure reads as the retried request: %q", moved.Reason)
	}
	ops, err := d.repository.Operations(stream)
	must(t, err)
	next := slices.IndexFunc(ops, func(o trace.OperationRecord) bool {
		_, event := lifecycle.DriftIDs(2)
		return o.Operation.ID == trace.OperationID(d.repository.Project(), stream, event) && o.Result == nil
	})
	if next < 0 {
		t.Fatalf("the drift rebase asked for again is not an operation: %+v", ops)
	}
	if again, err := d.Apply(context.Background(), op); err != nil || again.Outcome != "succeeded" {
		t.Fatalf("a retry of the ended operation %+v %v", again, err)
	}
	if n := len(routeFor(t, d.repository, stream, "drift.review_rejected")); n != 1 {
		t.Fatalf("%d routes for one hold", n)
	}
	if n := departuresInFeed(t, f.s, d.repository, stream); n != 1 {
		t.Fatalf("%d departures in the feed, want one", n)
	}
}

// The drift hold's departure is legal only at its bar: a pick below the modest
// bar leaves the drift rebase's operation pending, nothing moves and the chief of
// staff gets one notice.
func TestADriftHoldBelowItsBarMovesNothing(t *testing.T) {
	t.Parallel()
	f, d, stream, op := heldDrift(t, "drift-route-below-bar")
	routeJev(f.s, departTo("retried", 0.55))

	result, err := attemptOperation(t, f.s, d.repository, stream, op, d)
	if err == nil || !strings.Contains(err.Error(), "held by the router") {
		t.Fatalf("the operation ended or failed another way: %+v %v", result, err)
	}
	if state, err := d.repository.Workflow(stream, driftSubject); err != nil || strings.HasPrefix(state.Value, driftHeld) || strings.HasPrefix(state.Value, "requested-2") {
		t.Fatalf("a held exit moved the drift rebase: %+v %v", state, err)
	}
	if n := routeNotices(t, d.repository, stream); n != 1 {
		t.Fatalf("%d notices for the held exit, want one", n)
	}
	if _, err := attemptOperation(t, f.s, d.repository, stream, op, d); err == nil {
		t.Fatal("a retry committed what the router held")
	}
	if n := routeNotices(t, d.repository, stream); n != 1 {
		t.Fatalf("a retry sent another notice: %d", n)
	}
}
