package executionjourney

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func degradedJourney(t *testing.T, p *jevfixture.Transitions) (*bounceFixture, *tracetest.InMemoryExporter) {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	b := prepareBounceFixture(t, "reviews", false, func(o *journey.Options) { o.TelemetryExporter = exporter })
	t.Cleanup(func() { b.stop(t) })
	service.InspectForTest(b.f.S.Core()).SetJudge(jevfixture.TransitionJudge(b.f.S.Now, p))
	b.stream = seedBuilding(b.f, t, "journey-degraded", b.graph)
	b.awaitUnit(t, intake.UnitContested)
	return b, exporter
}

func routedUnitTransitions(t *testing.T, b *bounceFixture) []trace.Transition {
	t.Helper()
	var out []trace.Transition
	for _, tr := range intakefixture.WorkTransitions(t, b.f.Repository(), b.stream) {
		if tr.Subject == trace.UnitSubject(bounceUnit) && tr.DecidedBy != "" {
			out = append(out, tr)
		}
	}
	return out
}

func TestJourneyAScriptedJevOutageCommitsDefaultsInDegradedMode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &jevfixture.Transitions{Fail: func(int) error { return &systemone.Error{Kind: systemone.KindUnavailable, Message: "down"} }}
	b, exporter := degradedJourney(t, p)

	if state := intakefixture.UnitWorkflow(t, b.f.Repository(), b.stream, bounceUnit); state.Value != intake.UnitContested {
		t.Fatalf("the unit is %s", state.Value)
	}
	asked := p.TransitionRequests()
	if len(asked) == 0 {
		t.Fatal("no exit asked Jev")
	}
	fallbacks := 0
	for _, rec := range journey.RouteRecords(t, b.f.Repository(), b.stream) {
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

	intakefixture.Soon(t, "the fallback route spans", func() bool {
		return len(fixturekit.SpansOf(exporter).Named("osmia.transition.route").With("osmia.outcome", router.OutcomeFallback)) >= fallbacks
	})
	for _, sp := range fixturekit.SpansOf(exporter).Named("osmia.transition.route") {
		if outcome, _ := fixturekit.Attr(sp, "osmia.outcome"); outcome == router.OutcomeDeparted {
			t.Fatalf("a departure span while Jev was down: %v", fixturekit.Strings(sp))
		}
	}
	runtime, err := b.f.C.Runtime(ctx)
	fixturekit.Must(t, err)
	if runtime.Jev.Mode != jev.ModeDegraded {
		t.Fatalf("Jev's status: %+v", runtime.Jev)
	}

	for _, r := range p.TransitionRequests() {
		if _, ok := r.Questions[jev.QuestionFit]; !ok {
			t.Fatalf("a transition request without the default-fits question: %+v", r)
		}
	}
}

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

	if state := intakefixture.UnitWorkflow(t, b.f.Repository(), b.stream, bounceUnit); state.Value != intake.UnitContested {
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
	for _, rec := range journey.RouteRecords(t, b.f.Repository(), b.stream) {
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
