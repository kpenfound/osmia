package lifecycle_test

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

type routeSpans struct {
	mu    sync.Mutex
	spans []router.SpanRoute
}

func (l *routeSpans) add(s router.SpanRoute) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.spans = append(l.spans, s)
}

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

func collectSpans(s *Service) *routeSpans {
	l := &routeSpans{}
	s.core().Router().Spans = l.add
	return l
}

func masonCapacityFixture(t *testing.T) *shedFixture {
	t.Helper()
	return newDebateFixtureWith(t, 1, 1, "", func(opts *Options) {
		opts.Threads = intakefixture.ConfigureMasonThreads(t, opts.Config.Root, config.WorkspacesGit, "masons = 1\n", opts.Threads)
	})
}

func (f *edgeGraphFixture) build(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	b := &intake.Builder{S: f.s.intake().BuilderDependencies(), Repository: f.repository}
	fixturekit.Must(t, b.Pass(ctx))
	for _, stream := range []config.WorkstreamID{f.upstream, f.downstream} {
		ops, err := f.repository.Operations(stream)
		fixturekit.Must(t, err)
		for _, op := range ops {
			if op.Operation.Action != intake.BuildAction || op.Result != nil {
				continue
			}
			result, err := b.Apply(ctx, op.Operation)
			fixturekit.Must(t, err)
			if result.Outcome != "succeeded" {
				t.Fatalf("building %s: %+v", stream, result)
			}
		}
	}
}

func (f *edgeGraphFixture) dependentWork(t *testing.T) (state string, turns int) {
	t.Helper()
	unit, err := f.repository.Workflow(f.downstream, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	th, err := f.repository.Thread(f.downstream, intake.MasonAgent("resume"))
	if err == nil {
		turns = len(th.Turns)
	}
	return unit.Value, turns
}

func (f *edgeGraphFixture) publish(t *testing.T, hooks *fixturekit.Hooks) []fixturekit.HooksPost {
	t.Helper()
	os.Setenv(service.RoutingPublishTokenEnv, "journey-publisher-token")
	res, err := service.InspectForTest(f.s.core()).PublishToHearsay(context.Background(), f.repository, hooks.URL)
	fixturekit.Must(t, err)
	if res.Unreachable {
		t.Fatalf("the publish pass: %+v", res)
	}
	return fixturekit.Decisions(hooks.Take())
}

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
	fixturekit.Must(t, err)
	f.build(t)
	m := newMasonController(f.s, f.repository)
	hooks := fixturekit.NewHooks(t)

	f.resolvePasses(t)
	fixturekit.Must(t, m.Pass(ctx))
	if state, turns := f.dependentWork(t); state != "ready" || turns != 0 || !f.holdsMason(t, f.downstream) {
		t.Fatalf("the dependent works while it waits: unit %q, %d mason turns, want ready and 0", state, turns)
	}

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
	fixturekit.Must(t, err)
	if revision != 1 || len(waivers.Waivers) != 1 || waivers.Waivers[0].Upstream != f.upstream || waivers.Waivers[0].DecidedBy != trace.DecidedByJev || waivers.Waivers[0].Judgment != dropped.Judgment {
		t.Fatalf("waivers %+v at revision %d", waivers, revision)
	}
	if specAfter, _, _, err := f.repository.ProjectSpecOf(f.downstream); err != nil || len(specAfter.Graph) != len(specBefore.Graph) || specAfter.Behavior != specBefore.Behavior {
		t.Fatalf("the shared project spec changed: %+v %v", specAfter, err)
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
		observed, _, _, err := lifecycle.EdgeObservationAt(f.repository, f.downstream)
		fixturekit.Must(t, err)
		for _, e := range observed.Edges {
			if e.Merged {
				t.Fatalf("%s: a waiver was recorded as a merge: %+v", when, e)
			}
		}
	}
	fixturekit.Must(t, m.Pass(ctx))
	check("after the drop")
	for range 3 {
		f.resolvePasses(t)
		fixturekit.Must(t, m.Pass(ctx))
	}
	check("after repeated reconciliation")
	f.reopened(t)
	m = newMasonController(f.s, f.repository)
	f.resolvePasses(t)
	fixturekit.Must(t, m.Pass(ctx))
	check("after a restart")
	if n := len(p.TransitionRequests()); n != 1 {
		t.Fatalf("Jev was asked %d times for one piece of evidence", n)
	}
	if n := departuresInFeed(t, f.s, f.repository, f.downstream); n != 1 {
		t.Fatalf("%d departures in the feed, want 1", n)
	}

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
	fixturekit.Must(t, m.Pass(ctx))
	if state, turns := f.dependentWork(t); state != "ready" || turns != 0 || !f.holdsMason(t, f.downstream) {
		t.Fatalf("the dependent works while it waits: unit %q, %d mason turns, want ready and 0", state, turns)
	}

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
	observed, _, _, err := lifecycle.EdgeObservationAt(f.repository, f.downstream)
	fixturekit.Must(t, err)
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
	fixturekit.Must(t, m.Pass(ctx))
	check("after the exit")
	for range 3 {
		f.resolvePasses(t)
		fixturekit.Must(t, m.Pass(ctx))
	}
	check("after repeated reconciliation")
	f.reopened(t)
	m = newMasonController(f.s, f.repository)
	f.resolvePasses(t)
	fixturekit.Must(t, m.Pass(ctx))
	check("after a restart")

	for _, d := range f.publish(t, hooks) {
		if strings.Contains(d.Str("decision"), "-waiver-") {
			t.Fatalf("a dependent that went on because its upstream's changes are integrated has a waiver posted: %s", d.Raw)
		}
	}
	if len(p.Requests()) != len(p.TransitionRequests()) {
		t.Fatalf("Jev got %d requests, %d of them transition requests", len(p.Requests()), len(p.TransitionRequests()))
	}
}
