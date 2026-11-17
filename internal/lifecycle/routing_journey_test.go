package lifecycle_test

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

// routeSpans collects the route spans the fixture's router sends.
type routeSpans struct {
	mu    sync.Mutex
	spans []router.SpanRoute
}

func (l *routeSpans) add(s router.SpanRoute) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.spans = append(l.spans, s)
}

// of returns the spans of the trigger.
func (l *routeSpans) of(trigger string) []router.SpanRoute {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []router.SpanRoute
	for _, s := range l.spans {
		if s.Trigger == trigger {
			out = append(out, s)
		}
	}
	return out
}

// collectSpans makes the service's router send its route spans to the result.
func collectSpans(s *Service) *routeSpans {
	l := &routeSpans{}
	s.core().Router().Spans = l.add
	return l
}

// masonCapacityFixture is a debate fixture whose service has room for a mason, so
// that a dependent's first unit can start once nothing holds it.
func masonCapacityFixture(t *testing.T) *shedFixture {
	t.Helper()
	return newDebateFixtureWith(t, 1, 1, "", func(opts *Options) {
		opts.Threads = intakefixture.ConfigureMasonThreads(t, opts.Config.Root, config.WorkspacesGit, "masons = 1\n", opts.Threads)
	})
}

// build runs the build of both sealed workstreams, as the service's build pass
// does: each starts building, with its units planned and ready.
func (f *edgeGraphFixture) build(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	b := &builder{S: f.s.intake().BuilderDependencies(), Repository: f.repository}
	must(t, b.Pass(ctx))
	for _, stream := range []config.WorkstreamID{f.upstream, f.downstream} {
		ops, err := f.repository.Operations(stream)
		must(t, err)
		for _, op := range ops {
			if op.Operation.Action != BuildAction || op.Result != nil {
				continue
			}
			result, err := b.Apply(ctx, op.Operation)
			must(t, err)
			if result.Outcome != "succeeded" {
				t.Fatalf("building %s: %+v", stream, result)
			}
		}
	}
}

// dependentWork is what a dependent's work looks like in the trace: the state of
// its first unit and the turns queued for the unit's mason.
func (f *edgeGraphFixture) dependentWork(t *testing.T) (state string, turns int) {
	t.Helper()
	unit, err := f.repository.Workflow(f.downstream, trace.UnitSubject("resume"))
	must(t, err)
	th, err := f.repository.Thread(f.downstream, masonAgent("resume"))
	if err == nil {
		turns = len(th.Turns)
	}
	return unit.Value, turns
}

// publish runs one Hearsay publish pass over the fixture's trace against a fake
// listener and returns the decisions it received.
func (f *edgeGraphFixture) publish(t *testing.T, hooks *fixturekit.Hooks) []fixturekit.HooksPost {
	t.Helper()
	os.Setenv(service.RoutingPublishTokenEnv, "journey-publisher-token")
	res, err := service.InspectForTest(f.s.core()).PublishToHearsay(context.Background(), f.repository, hooks.URL)
	must(t, err)
	if res.Unreachable {
		t.Fatalf("the publish pass: %+v", res)
	}
	return fixturekit.Decisions(hooks.Take())
}

// A dependent waits on an upstream that was abandoned. The upstream ends, the
// router decides the exit from waiting, and Jev drops the dependency. The
// dependent leaves waiting and goes on to do its work: its first unit starts and
// its mason's turn is queued, which the pass of the mason controller would not
// do while the dependent waited, and it is not parked again by repeated
// reconciliation or a restart. The waiver is in the dependent's own trace, the
// shared project spec is unchanged, the waiver is never a merge, the feed calls
// the drop Jev's decision once, and Hearsay is told of it once as Jev's decision
// on the work source with no author.
func TestJourneyADependentOnAnAbandonedUpstreamLeavesWaitingAndGoesOn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sf := masonCapacityFixture(t)
	p := dropsAt(0.95)
	routeJev(sf.s, p)
	spans := collectSpans(sf.s)
	f, _ := sealedEdgeFixture(t, sf)
	fixturekit.RecordHanded(t, f.repository, f.upstream, "stdin", f.s.now())
	fixturekit.RecordHanded(t, f.repository, f.downstream, "stdin", f.s.now())
	specBefore, _, _, err := f.repository.ProjectSpecOf(f.downstream)
	must(t, err)
	f.build(t)
	m := newMasonController(f.s, f.repository)
	hooks := fixturekit.NewHooks(t)

	// While the upstream lives the dependent waits: nothing of it starts and its
	// mason's turn would be held.
	f.resolvePasses(t)
	must(t, m.Pass(ctx))
	if state, turns := f.dependentWork(t); state != "ready" || turns != 0 || !f.holdsMason(t, f.downstream) {
		t.Fatalf("the dependent works while it waits: unit %q, %d mason turns, want ready and 0", state, turns)
	}

	// The upstream is abandoned: the exit is Jev's to decide.
	endStream(t, f.repository, f.s.now(), f.upstream)
	f.resolvePasses(t)
	subject := lifecycle.DependencySubject(f.upstream, trace.EdgeHardBlock)
	dropped := lastOn(t, f.repository, f.downstream, subject)
	if dropped.To != lifecycle.DependencyWaived || dropped.DefaultTo != lifecycle.DependencyKept || dropped.DecidedBy != trace.DecidedByJev || dropped.Judgment == "" || !dropped.Departed() {
		t.Fatalf("the departure %+v", dropped)
	}
	records := routeFor(t, f.repository, f.downstream, lifecycle.TriggerUpstreamTerminal)
	if len(records) != 1 || records[0].State != router.StateCommitted || records[0].Outcome != router.OutcomeDeparted || records[0].Target != lifecycle.DependencyWaived || records[0].Judgment != dropped.Judgment {
		t.Fatalf("route %+v", records)
	}
	if got := spans.of(lifecycle.TriggerUpstreamTerminal); len(got) != 1 || got[0].Outcome != router.OutcomeDeparted || got[0].Default != lifecycle.DependencyKept || got[0].Chosen != lifecycle.DependencyWaived || got[0].Judgment != dropped.Judgment {
		t.Fatalf("route spans %+v", got)
	}
	waivers, revision, err := lifecycle.EdgeWaiversAt(f.repository, f.downstream)
	must(t, err)
	if revision != 1 || len(waivers.Waivers) != 1 || waivers.Waivers[0].Upstream != f.upstream || waivers.Waivers[0].DecidedBy != trace.DecidedByJev || waivers.Waivers[0].Judgment != dropped.Judgment {
		t.Fatalf("waivers %+v at revision %d", waivers, revision)
	}
	if specAfter, _, _, err := f.repository.ProjectSpecOf(f.downstream); err != nil || len(specAfter.Graph) != len(specBefore.Graph) || specAfter.Behavior != specBefore.Behavior {
		t.Fatalf("the shared project spec changed: %+v %v", specAfter, err)
	}

	// The dependent goes on: its work is scheduled, not parked again.
	check := func(when string) {
		t.Helper()
		if f.holdsMason(t, f.downstream) {
			t.Fatalf("%s: the dependent is held", when)
		}
		if state := f.waitState(t); state.Value != "available" || state.Version != 2 {
			t.Fatalf("%s: the edge wait is %+v", when, state)
		}
		if state, turns := f.dependentWork(t); state != "implementing" || turns != 1 {
			t.Fatalf("%s: the dependent's first unit is %q with %d mason turns, want implementing and 1", when, state, turns)
		}
		observed, _, _, err := edgeObservationAt(f.repository, f.downstream)
		must(t, err)
		for _, e := range observed.Edges {
			if e.Merged {
				t.Fatalf("%s: a waiver was recorded as a merge: %+v", when, e)
			}
		}
	}
	must(t, m.Pass(ctx))
	check("after the drop")
	for range 3 {
		f.resolvePasses(t)
		must(t, m.Pass(ctx))
	}
	check("after repeated reconciliation")
	f.reopened(t)
	m = newMasonController(f.s, f.repository)
	f.resolvePasses(t)
	must(t, m.Pass(ctx))
	check("after a restart")
	if n := len(p.TransitionRequests()); n != 1 {
		t.Fatalf("Jev was asked %d times for one piece of evidence", n)
	}
	if n := departuresInFeed(t, f.s, f.repository, f.downstream); n != 1 {
		t.Fatalf("%d departures in the feed, want 1", n)
	}

	// Hearsay is told of the waiver once, as Jev's decision with no author, and
	// not as a second decision for the transition that moved the decision.
	var waivered, routed int
	for _, d := range f.publish(t, hooks) {
		switch {
		case strings.Contains(d.Str("decision"), "-jev-waiver-"):
			waivered++
			if d.Source != "osmia-work" || d.Str("author") != "" || d.Str("decider") == "owner" || !strings.Contains(d.Str("context")+d.Str("rationale"), dropped.Judgment) || !strings.Contains(d.Str("outcome"), "not recorded as merged") {
				t.Fatalf("the waiver's decision: %s on %s", d.Raw, d.Source)
			}
		case strings.Contains(d.Str("decision"), "-jev-route-"):
			routed++
		}
	}
	if waivered != 1 || routed != 0 {
		t.Fatalf("%d waivers and %d departures posted to Hearsay, want 1 and 0", waivered, routed)
	}
	for _, d := range f.publish(t, hooks) {
		if strings.Contains(d.Str("decision"), "-jev-waiver-") {
			t.Fatalf("the waiver was posted again: %s", d.Raw)
		}
	}
	if len(p.Requests()) != len(p.TransitionRequests()) {
		t.Fatalf("Jev got %d requests, %d of them transition requests", len(p.Requests()), len(p.TransitionRequests()))
	}
}

// A dependent waits on an upstream that was merged outside the factory. Once the
// upstream's changes are in the base branch the exit from waiting is raised and
// the router follows its default: the dependent goes on. It needs no waiver and
// no workgroup change, and the edge is recorded as merged at the base commit
// that holds the changes. The dependent starts its work and stays released
// through reconciliation and a restart, and nothing of it reaches Hearsay as a
// waiver.
func TestJourneyADependentOnAnUpstreamMergedOutsideTheFactoryLeavesWaitingAndGoesOn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sf := masonCapacityFixture(t)
	p := &jevfixture.Transitions{}
	routeJev(sf.s, p)
	spans := collectSpans(sf.s)
	f, _ := sealedEdgeFixture(t, sf)
	fixturekit.RecordHanded(t, f.repository, f.upstream, "stdin", f.s.now())
	fixturekit.RecordHanded(t, f.repository, f.downstream, "stdin", f.s.now())
	f.build(t)
	m := newMasonController(f.s, f.repository)
	hooks := fixturekit.NewHooks(t)

	f.resolvePasses(t)
	must(t, m.Pass(ctx))
	if state, turns := f.dependentWork(t); state != "ready" || turns != 0 || !f.holdsMason(t, f.downstream) {
		t.Fatalf("the dependent works while it waits: unit %q, %d mason turns, want ready and 0", state, turns)
	}

	// The upstream ended and its changes were merged by hand.
	endStream(t, f.repository, f.s.now(), f.upstream)
	merged := advanceUpstream(t, sf, map[string]string{"upstream.go": "upstream\n", "UPSTREAM.md": "merged by hand\n"})
	f.resolvePasses(t)
	records := routeFor(t, f.repository, f.downstream, lifecycle.TriggerUpstreamIntegrated)
	if len(records) != 1 || records[0].State != router.StateCommitted || records[0].Default != lifecycle.DependencyAdopted || records[0].Outcome != router.OutcomeFollowed || records[0].Target != lifecycle.DependencyAdopted {
		t.Fatalf("route %+v", records)
	}
	if got := spans.of(lifecycle.TriggerUpstreamIntegrated); len(got) != 1 || got[0].Outcome != router.OutcomeFollowed || got[0].Chosen != lifecycle.DependencyAdopted {
		t.Fatalf("route spans %+v", got)
	}
	observed, _, _, err := edgeObservationAt(f.repository, f.downstream)
	must(t, err)
	if len(observed.Edges) != 1 || !observed.Edges[0].Merged || observed.Edges[0].Commit != merged {
		t.Fatalf("the observation %+v", observed)
	}
	if waivers, _, err := lifecycle.EdgeWaiversAt(f.repository, f.downstream); err != nil || len(waivers.Waivers) != 0 {
		t.Fatalf("going on because the changes are integrated wrote a waiver: %+v %v", waivers, err)
	}

	check := func(when string) {
		t.Helper()
		if f.holdsMason(t, f.downstream) {
			t.Fatalf("%s: the dependent is held", when)
		}
		if state := f.waitState(t); state.Value != "available" || state.Version != 2 {
			t.Fatalf("%s: the edge wait is %+v", when, state)
		}
		if state, turns := f.dependentWork(t); state != "implementing" || turns != 1 {
			t.Fatalf("%s: the dependent's first unit is %q with %d mason turns, want implementing and 1", when, state, turns)
		}
	}
	must(t, m.Pass(ctx))
	check("after the exit")
	for range 3 {
		f.resolvePasses(t)
		must(t, m.Pass(ctx))
	}
	check("after repeated reconciliation")
	f.reopened(t)
	m = newMasonController(f.s, f.repository)
	f.resolvePasses(t)
	must(t, m.Pass(ctx))
	check("after a restart")

	// Going on is not a drop: no waiver is posted, and Jev was asked only about
	// the two exits from waiting.
	for _, d := range f.publish(t, hooks) {
		if strings.Contains(d.Str("decision"), "-waiver-") {
			t.Fatalf("a dependent that went on because its upstream's changes are integrated has a waiver posted: %s", d.Raw)
		}
	}
	if len(p.Requests()) != len(p.TransitionRequests()) {
		t.Fatalf("Jev got %d requests, %d of them transition requests", len(p.Requests()), len(p.TransitionRequests()))
	}
}
