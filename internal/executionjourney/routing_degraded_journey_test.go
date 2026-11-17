package executionjourney

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest"
	"github.com/kpenfound/osmia/internal/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// degradedJourney runs a unit through the whole pipeline (its mason, its checks
// and its reviewer, three send-backs and the contest the third raises) on a
// service whose Jev is the fixture p, which a test scripts to fail the
// transition requests. The scheduler, the router and its worker, the trace, the
// status and the span sink are the product's own; p is installed before any work
// exists, so every exit of the pipeline is asked of it.
func degradedJourney(t *testing.T, p *jevfixture.Transitions) (*bounceFixture, *tracetest.InMemoryExporter) {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	b := prepareBounceFixture(t, "reviews", false, func(o *Options) { o.telemetryExporter = exporter })
	t.Cleanup(func() { b.stop(t) })
	service.InspectForTest(b.f.s.core()).SetJudge(jevfixture.TransitionJudge(b.f.s.now, p))
	b.stream = b.f.seedBuilding(t, "journey-degraded", b.graph)
	b.awaitUnit(t, UnitContested)
	return b, exporter
}

// routedUnitTransitions returns the bounce unit's transitions that a router
// chose, in the order they were committed.
func routedUnitTransitions(t *testing.T, b *bounceFixture) []trace.Transition {
	t.Helper()
	var out []trace.Transition
	for _, tr := range workTransitions(t, b.f.repository(), b.stream) {
		if tr.Subject == trace.UnitSubject(bounceUnit) && tr.DecidedBy != "" {
			out = append(out, tr)
		}
	}
	return out
}

// A scripted outage of Jev commits the defaults in degraded mode while the
// factory goes on: the unit is built, checked, reviewed and sent back three
// times and ends contested, as it does when Jev finds every default fits. Every
// exit Jev was asked about is committed as a fallback, in the transition, the
// route record and the route span, no departure is made, the status shows Jev
// degraded, and the chief of staff is not blocked on a request.
func TestJourneyAScriptedJevOutageCommitsDefaultsInDegradedMode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &jevfixture.Transitions{Fail: func(int) error { return &systemone.Error{Kind: systemone.KindUnavailable, Message: "down"} }}
	b, exporter := degradedJourney(t, p)

	// The pipeline ran to its end on defaults alone.
	if state := workflowOf(t, b.f.repository(), b.stream, bounceUnit); state.Value != UnitContested {
		t.Fatalf("the unit is %s", state.Value)
	}
	asked := p.TransitionRequests()
	if len(asked) == 0 {
		t.Fatal("no exit asked Jev")
	}
	fallbacks := 0
	for _, rec := range routeRecords(t, b.f.repository(), b.stream) {
		if !rec.JevRequested {
			continue
		}
		fallbacks++
		if rec.State != router.StateCommitted || rec.Outcome != router.OutcomeFallback || rec.Fallback == "" || rec.Target != rec.Default || rec.DecidedBy != trace.DecidedByFallback {
			t.Fatalf("an exit Jev was asked about while it was down: %+v", rec)
		}
	}
	if fallbacks == 0 {
		t.Fatal("no route record says Jev was asked")
	}

	// The transitions say fallback and keep the default; none is a departure.
	routed := routedUnitTransitions(t, b)
	var fell int
	for _, tr := range routed {
		if tr.Departed() || tr.DecidedBy == trace.DecidedByJev {
			t.Fatalf("a departure while Jev was down: %+v", tr)
		}
		if tr.DecidedBy == trace.DecidedByFallback {
			fell++
			if tr.DefaultTo != tr.To {
				t.Fatalf("a fallback that left its default: %+v", tr)
			}
		}
	}
	if fell == 0 {
		t.Fatalf("no transition says fallback: %+v", routed)
	}

	// The route spans say fallback, and the status shows Jev degraded.
	awaitJourney(t, "the fallback route spans", func() bool {
		return len(servicetest.SpansOf(exporter).Named("osmia.transition.route").With("osmia.outcome", router.OutcomeFallback)) >= fallbacks
	})
	for _, sp := range servicetest.SpansOf(exporter).Named("osmia.transition.route") {
		if outcome, _ := servicetest.Attr(sp, "osmia.outcome"); outcome == router.OutcomeDeparted {
			t.Fatalf("a departure span while Jev was down: %v", servicetest.Strings(sp))
		}
	}
	runtime, err := b.f.c.Runtime(ctx)
	must(t, err)
	if runtime.Jev.Mode != jev.ModeDegraded {
		t.Fatalf("Jev's status: %+v", runtime.Jev)
	}

	// Step journals made no request: every one was the narrow questions of an
	// exit, and the decisions Jev makes for the chief of staff are its own.
	for _, r := range p.TransitionRequests() {
		if _, ok := r.Questions[jev.QuestionFit]; !ok {
			t.Fatalf("a transition request without the default-fits question: %+v", r)
		}
	}
}

// A Jev that never answers is waited on in place for its timeout and no longer:
// every request is cut at the configured timeout, the exits commit their
// defaults, and the factory finishes the unit's pipeline anyway.
func TestJourneyAJevThatNeverAnswersIsWaitedOnOnlyUntilItsTimeout(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var waited []time.Duration
	p := &jevfixture.Transitions{Wait: func(ctx context.Context) error {
		started := time.Now()
		<-ctx.Done()
		mu.Lock()
		waited = append(waited, time.Since(started))
		mu.Unlock()
		return ctx.Err()
	}}
	b, _ := degradedJourney(t, p)

	if state := workflowOf(t, b.f.repository(), b.stream, bounceUnit); state.Value != UnitContested {
		t.Fatalf("the unit is %s", state.Value)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(waited) == 0 {
		t.Fatal("Jev was never asked")
	}
	for _, d := range waited {
		if d > 5*time.Second {
			t.Fatalf("a request waited %s for Jev, whose timeout is 1s: %v", d, waited)
		}
	}
	committed := 0
	for _, rec := range routeRecords(t, b.f.repository(), b.stream) {
		if rec.JevRequested {
			committed++
			if rec.State != router.StateCommitted || rec.Outcome != router.OutcomeFallback {
				t.Fatalf("an exit Jev never answered: %+v", rec)
			}
		}
	}
	if committed == 0 {
		t.Fatal("no exit recorded that Jev was asked")
	}
}
