package recordspans

import (
	"context"
	"math"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/telemetry"
	"github.com/kpenfound/osmia/internal/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

const judgment = "judgment_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func (h *harness) routedUnit(id, unit, to string, at time.Time, route trace.Routing) {
	h.t.Helper()
	subject := trace.UnitSubject(unit)
	state, err := h.repo.Workflow(stream, subject)
	if err != nil {
		h.t.Fatal(err)
	}
	t := trace.Transition{Header: header("transition", id, at, service, unit), Subject: subject, From: state.Value, To: to, Reason: "reason", DecidedBy: route.DecidedBy, Judgment: route.Judgment, DefaultTo: route.DefaultTo}
	if _, err := h.repo.Transact(context.Background(), trace.Transaction{ExpectedVersion: state.Version, Transition: t}); err != nil {
		h.t.Fatalf("unit %s: %v", id, err)
	}
}

func boolValue(s tracetest.SpanStub, key string) (isTrue, present bool) {
	v, ok := value(s, key)
	if !ok {
		return false, false
	}
	return v.AsBool(), true
}

func TestStateSpansCarryTheRouting(t *testing.T) {
	h := newHarness(t)
	h.unit("u1-1", "u1", "ready", minute(1), service, "first")
	h.routedUnit("u1-2", "u1", "implementing", minute(2), trace.Routing{DecidedBy: trace.DecidedByCode, DefaultTo: "implementing"})
	h.routedUnit("u1-3", "u1", "contested", minute(10), trace.Routing{DecidedBy: trace.DecidedByJev, Judgment: judgment, DefaultTo: "checking"})
	h.routedUnit("u1-4", "u1", "reviewing", minute(11), trace.Routing{DecidedBy: trace.DecidedByOwner, DefaultTo: "implementing"})
	h.unit("u1-5", "u1", "checking", minute(12), service, "unrouted")
	if _, err := h.repo.SetFeatureStateRouted(context.Background(), header("transition", "f1", minute(3), service, ""), "building", "building", trace.Routing{DecidedBy: trace.DecidedByJevGate, Judgment: judgment, DefaultTo: "building"}); err != nil {
		t.Fatal(err)
	}
	h.feature("f2", "assembled", minute(20), service, "unrouted")

	spans := h.spans()
	type routing struct {
		decidedBy, defaultState string
		deviated, hasDeviated   bool
	}
	read := func(s tracetest.SpanStub) routing {
		d, hasDeviated := boolValue(s, "osmia.deviated")
		return routing{str(s, "osmia.decided_by"), str(s, "osmia.default_state"), d, hasDeviated}
	}
	units := named(spans, "osmia.unit.state")
	want := []routing{
		{"code", "implementing", false, true},
		{"jev", "checking", true, true},
		{"owner", "implementing", true, true},
		{},
	}
	if len(units) != len(want) {
		t.Fatalf("%d unit state spans, want %d", len(units), len(want))
	}
	for i, w := range want {
		if got := read(units[i]); got != w {
			t.Errorf("unit state span %d routing %+v, want %+v: %s", i, got, w, describe(units[i]))
		}
	}
	features := named(spans, "osmia.workstream.state")
	wantFeature := []routing{{"jev_gate", "building", false, true}, {}}
	if len(features) != len(wantFeature) {
		t.Fatalf("%d workstream state spans, want %d", len(features), len(wantFeature))
	}
	for i, w := range wantFeature {
		if got := read(features[i]); got != w {
			t.Errorf("workstream state span %d routing %+v, want %+v: %s", i, got, w, describe(features[i]))
		}
	}
	if len(named(spans, "osmia.transition.route")) != 0 {
		t.Fatal("a state span is not a route span")
	}
}

func routeFixture() Route {
	return Route{Project: project, Workstream: stream, Unit: "u1", Occurrence: "occurrence-1", Trigger: "unit.checks_failed",
		Default: "implementing", Chosen: "checking", Outcome: RouteDeparted, Confidence: ConfidenceBinding, Judgment: judgment,
		Start: minute(5), End: minute(6)}
}

func TestRouteSpanCarriesTheSixAttributes(t *testing.T) {
	h := newHarness(t)
	rec := Recorder{Sink: h.sink}
	if !rec.Route(routeFixture()) {
		t.Fatal("the route span was not sent")
	}
	spans := named(h.spans(), "osmia.transition.route")
	if len(spans) != 1 {
		t.Fatalf("%d route spans", len(spans))
	}
	s := spans[0]
	for key, want := range map[string]string{
		"osmia.trigger": "unit.checks_failed", "osmia.default_state": "implementing", "osmia.next_state": "checking",
		"osmia.outcome": "departed", "osmia.confidence": "binding", "osmia.judgment": judgment,
		"osmia.project.id": string(project), "osmia.workstream.id": string(stream), "osmia.unit.id": "u1",
	} {
		if got := str(s, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if len(s.Attributes) != 9 {
		t.Errorf("the route span has %d attributes: %s", len(s.Attributes), describe(s))
	}
	if s.Parent.SpanID() != telemetry.WorkstreamRoot(stream) || s.SpanContext.TraceID() != telemetry.WorkstreamTrace(stream) || !s.StartTime.Equal(minute(5)) || !s.EndTime.Equal(minute(6)) {
		t.Errorf("route span placement: %s", describe(s))
	}
	word := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+=-]{0,127}$`)
	for _, kv := range s.Attributes {
		if !word.MatchString(kv.Value.AsString()) {
			t.Errorf("%s is not a word or an ID: %q", kv.Key, kv.Value.AsString())
		}
		if !slices.Contains(telemetry.AttributeKeys(), string(kv.Key)) {
			t.Errorf("%s is not on the allow-list", kv.Key)
		}
	}
}

func TestRouteSpanHasOneIdentityPerExit(t *testing.T) {
	h := newHarness(t)
	rec := Recorder{Sink: h.sink}
	rec.Route(routeFixture())
	rec.Route(routeFixture())
	other := routeFixture()
	other.Occurrence = "occurrence-2"
	other.Unit, other.Judgment, other.Outcome, other.Confidence, other.Chosen = "", "", RouteFallback, ConfidenceNone, "implementing"
	rec.Route(other)
	spans := named(h.spans(), "osmia.transition.route")
	if len(spans) != 3 {
		t.Fatalf("%d route spans, want 3", len(spans))
	}
	if spans[0].SpanContext.SpanID() != spans[1].SpanContext.SpanID() || spans[0].SpanContext.SpanID() == spans[2].SpanContext.SpanID() {
		t.Fatal("route span identities do not follow the occurrence")
	}
	if _, ok := value(spans[2], "osmia.unit.id"); ok {
		t.Error("a workstream exit carries a unit")
	}
	if _, ok := value(spans[2], "osmia.judgment"); ok {
		t.Error("a decision without a judgment carries one")
	}
	if str(spans[2], "osmia.outcome") != "fallback" || str(spans[2], "osmia.confidence") != "none" {
		t.Errorf("fallback span: %s", describe(spans[2]))
	}
}

func TestRouteSpanRefusesValuesThatAreNotFixedWords(t *testing.T) {
	h := newHarness(t)
	rec := Recorder{Sink: h.sink}
	for name, mutate := range map[string]func(*Route){
		"a trigger that is free text":  func(r *Route) { r.Trigger = "the checks failed again" },
		"an unlisted trigger":          func(r *Route) { r.Trigger = "unit.invented" },
		"an outcome that is free text": func(r *Route) { r.Outcome = "Jev preferred it" },
		"an unlisted outcome":          func(r *Route) { r.Outcome = "decided" },
		"a probability for a band":     func(r *Route) { r.Confidence = "0.93" },
		"an unlisted band":             func(r *Route) { r.Confidence = "certain" },
		"no occurrence":                func(r *Route) { r.Occurrence = "" },
		"no workstream":                func(r *Route) { r.Workstream = "" },
		"no decision time":             func(r *Route) { r.End = time.Time{} },
	} {
		r := routeFixture()
		mutate(&r)
		if rec.Route(r) {
			t.Errorf("%s was sent", name)
		}
	}
	if spans := named(h.spans(), "osmia.transition.route"); len(spans) != 0 {
		t.Fatalf("%d route spans were sent", len(spans))
	}
	r := routeFixture()
	r.Judgment, r.Default = "Jev said so, with 93% confidence", "Needs Work"
	if !rec.Route(r) {
		t.Fatal("the span with unusable ID values was not sent")
	}
	s := named(h.spans(), "osmia.transition.route")[0]
	if _, ok := value(s, "osmia.judgment"); ok {
		t.Error("free text reached the judgment attribute")
	}
	if _, ok := value(s, "osmia.default_state"); ok {
		t.Error("free text reached the default state attribute")
	}
}

func TestConfidenceBand(t *testing.T) {
	for _, c := range []struct {
		p    float64
		want string
	}{
		{0, ConfidenceLow}, {0.49, ConfidenceLow}, {0.5, ConfidenceModest}, {0.69, ConfidenceModest},
		{0.7, ConfidenceHigh}, {0.89, ConfidenceHigh}, {0.9, ConfidenceBinding}, {1, ConfidenceBinding},
		{-0.1, ConfidenceNone}, {1.1, ConfidenceNone}, {math.NaN(), ConfidenceNone},
	} {
		if got := ConfidenceBand(c.p, 0.9); got != c.want {
			t.Errorf("ConfidenceBand(%v, 0.9) = %q, want %q", c.p, got, c.want)
		}
	}
	if got := ConfidenceBand(0.6, 0.6); got != ConfidenceBinding {
		t.Errorf("ConfidenceBand(0.6, 0.6) = %q", got)
	}
	for _, band := range ConfidenceBands {
		if !telemetry.Confidence(band).Valid() {
			t.Errorf("band %q is not a valid attribute value", band)
		}
	}
	for _, word := range append(slices.Clone(RouteTriggers), RouteOutcomes...) {
		if !telemetry.Trigger(word).Valid() {
			t.Errorf("%q is not a valid trigger value", word)
		}
	}
}
