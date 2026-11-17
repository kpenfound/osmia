package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/slotspans"
	"github.com/kpenfound/osmia/internal/telemetry"
	"github.com/kpenfound/osmia/internal/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// reservationRig is a service with the committee pool at one slot, a final
// review operation waiting for it, and slot spans in an in-memory exporter. A
// restart replaces the slot recorder and the pool, as a new run of the service
// does, over the same ledger and trace.
type reservationRig struct {
	t        *testing.T
	s        *Service
	repo     *trace.Repository
	cfg      *config.Config
	exporter *tracetest.InMemoryExporter
	sink     *telemetry.Sink
	clock    *spanClock
	ledger   string
	op       coreadapter.Operation
	// holder gives back the slot another reader holds.
	holder func()
}

func newReservationRig(t *testing.T) *reservationRig {
	t.Helper()
	cfg, repo, clock := hardPauseRepository(t, 1)
	cfg.Capacity.Committee = 1
	s, _ := hardPauseService(t, cfg, repo, clock, nil)
	s.cfg = cfg
	s.setSole(runtimeFor(repo))
	exporter := tracetest.NewInMemoryExporter()
	sink, err := telemetry.New(config.Telemetry{}, telemetry.Options{Exporter: exporter})
	must(t, err)
	t.Cleanup(func() { sink.Close() })
	ledger, err := cfg.Root.SlotIntervals()
	must(t, err)
	r := &reservationRig{t: t, s: s, repo: repo, cfg: cfg, exporter: exporter, sink: sink, clock: &spanClock{now: time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)}, ledger: ledger}
	s.telemetry = sink
	r.start()
	t.Cleanup(func() { r.s.slots.Close() })
	r.op = r.seedOperation("final-review")
	return r
}

func (r *reservationRig) start() {
	r.s.slots = slotspans.New(r.sink, slotspans.Options{Ledger: r.ledger, Now: r.clock.Now})
	r.s.pool = &scheduler.Shared{Spans: r.s.slots}
}

// restart stops the run and starts another over the same ledger.
func (r *reservationRig) restart() {
	r.t.Helper()
	r.s.slots.Close()
	r.start()
}

// seedOperation records a final review operation the workstream has not run.
func (r *reservationRig) seedOperation(event string) coreadapter.Operation {
	r.t.Helper()
	op := coreadapter.Operation{ID: trace.OperationID(project, stream, event), Boundary: coreadapter.RunnerBoundary, Action: FinalReviewAction, Input: json.RawMessage(`{}`)}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "handin-" + event, Revision: 1, Project: project, Workstream: stream, At: demoStart, Actor: ownerActor, Cause: "handin"}
	_, err := r.repo.Transact(context.Background(), trace.Transaction{Transition: trace.Transition{Header: h, Subject: trace.FeatureSubject, To: HandedState, Reason: "Handed in"}, Events: []trace.Event{{ID: event, Kind: "turn", Body: "Review the feature", Operation: &op}}})
	must(r.t, err)
	return op
}

// hold takes the committee pool's only slot for another reader.
func (r *reservationRig) hold() {
	r.t.Helper()
	release, admitted, err := r.s.pool.TryReserve(r.repo, stream, committeeRole, "holder", &r.cfg.Capacity)
	must(r.t, err)
	if !admitted {
		r.t.Fatal("the committee slot was refused")
	}
	r.holder = release
}

// reserve offers the operation its committee slot and reports whether it got it.
func (r *reservationRig) reserve() (func(), bool) {
	return r.s.reserveModelOperation(r.repo, r.op, committeeRole)
}

func (r *reservationRig) refuse() {
	r.t.Helper()
	if release, admitted := r.reserve(); admitted {
		release()
		r.t.Fatal("the operation was admitted past the committee limit")
	}
}

// open is the operation's open reservation waits.
func (r *reservationRig) open() []slotspans.Interval {
	return r.s.slots.Find(slotspans.Wait, slotspans.SourceReservation, nil)
}

func (r *reservationRig) waits() []tracetest.SpanStub {
	r.t.Helper()
	must(r.t, r.sink.Flush(context.Background()))
	var out []tracetest.SpanStub
	for _, sp := range r.exporter.GetSpans() {
		if sp.Name == "osmia.slot.wait" {
			out = append(out, sp)
		}
	}
	return out
}

// acknowledge runs the operation to a recorded result.
func (r *reservationRig) acknowledge() {
	r.t.Helper()
	c, err := reconcile.New(r.repo, reconcile.Options{Worker: "test", Now: r.clock.Now, Adapters: map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RunnerBoundary: reviewedAdapter{}}})
	must(r.t, err)
	must(r.t, c.Pass(context.Background()))
	records, err := r.repo.Operations(stream)
	must(r.t, err)
	if len(records) != 1 || !records[0].Acknowledged {
		r.t.Fatalf("the operation was not completed: %+v", records)
	}
}

type reviewedAdapter struct{}

func (reviewedAdapter) Inspect(context.Context, coreadapter.Operation) (coreadapter.Observation, error) {
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "not run"}, nil
}

func (reviewedAdapter) Apply(context.Context, coreadapter.Operation) (coreadapter.OperationResult, error) {
	return coreadapter.OperationResult{Outcome: "succeeded", Evidence: "reviewed"}, nil
}

// A committee operation refused for capacity keeps one wait across a restart:
// the refusal after the restart updates the recovered wait, and the admission
// that follows ends it granted, with the start and IDs from before the restart.
func TestReservationWaitSurvivesARestartAndIsGrantedOnce(t *testing.T) {
	t.Parallel()
	r := newReservationRig(t)
	r.hold()
	firstRefusal := r.clock.Now()
	r.refuse()
	open := r.open()
	if len(open) != 1 || !open[0].Start.Equal(firstRefusal) {
		t.Fatalf("open waits before the restart: %+v", open)
	}
	id := open[0].SpanID()
	r.clock.Advance(time.Minute)

	// The service restarts while the operation waits, and the pool is full again.
	r.clock.Advance(time.Hour)
	r.restart()
	r.hold()
	if got := r.waits(); len(got) != 0 {
		t.Fatalf("a restart sent %d wait spans for a wait that is still open", len(got))
	}
	if open := r.open(); len(open) != 1 || !open[0].Recovered || !open[0].Start.Equal(firstRefusal) {
		t.Fatalf("open waits after the restart: %+v", open)
	}

	// The operation is still pending, so the pass leaves its wait open.
	r.s.reservationWaitPass(r.repo)
	if got := r.waits(); len(got) != 0 || len(r.open()) != 1 {
		t.Fatalf("the pass closed the wait of a pending operation: %d spans, %d open", len(got), len(r.open()))
	}

	// A refusal after the restart updates the recovered wait.
	r.clock.Advance(time.Minute)
	r.refuse()
	open = r.open()
	if len(open) != 1 || open[0].Recovered || !open[0].Start.Equal(firstRefusal) || open[0].SpanID() != id {
		t.Fatalf("the refusal after the restart left %+v", open)
	}
	if got := r.waits(); len(got) != 0 {
		t.Fatalf("a refusal after the restart sent %d wait spans", len(got))
	}

	// Capacity frees and the operation is admitted.
	r.holder()
	r.clock.Advance(time.Minute)
	grantedAt := r.clock.Now()
	release, admitted := r.reserve()
	if !admitted {
		t.Fatal("the operation was refused once capacity was free")
	}
	defer release()
	r.s.reservationWaitPass(r.repo)

	waits := r.waits()
	if len(waits) != 1 {
		t.Fatalf("%d wait spans, want exactly one for the operation", len(waits))
	}
	sp := waits[0]
	if !sp.StartTime.Equal(firstRefusal) || !sp.EndTime.Equal(grantedAt) || sp.SpanContext.SpanID() != id || sp.SpanContext.TraceID() != telemetry.WorkstreamTrace(stream) || sp.Parent.SpanID() != telemetry.WorkstreamRoot(stream) {
		t.Fatalf("wait span %+v, want %s to %s with span %s", sp, firstRefusal, grantedAt, id)
	}
	if a := spanAttrs(sp); a["osmia.outcome"] != "granted" || a["osmia.wait_reason"] != "capacity" || a["osmia.role"] != "committee" {
		t.Fatalf("wait attributes %v", a)
	}
	r.restart()
	r.s.reservationWaitPass(r.repo)
	if got := r.waits(); len(got) != 1 || len(r.open()) != 0 {
		t.Fatalf("a later restart sent the wait again: %d spans, %d open", len(got), len(r.open()))
	}
}

// A recovered wait whose operation is gone from the factory, finished, or held
// back by a pause is closed once, with how it ended, and not again by later
// passes or restarts.
func TestRecoveredReservationWaitsOfOperationsNoLongerPendingAreClosedOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		outcome string
		// refuse opens the wait before the restart and returns its start; change
		// is what happens to the operation while the service is down.
		refuse func(*reservationRig) time.Time
		change func(*reservationRig)
	}{
		{"workstream removed", "removed", func(r *reservationRig) time.Time {
			gone, err := config.NewWorkstreamID()
			must(t, err)
			start := r.clock.Now()
			r.s.slots.Begin(reservationWaitOf(gone, start))
			return start
		}, func(*reservationRig) {}},
		{"operation completed elsewhere", "completed", refusedOperation, func(r *reservationRig) { r.acknowledge() }},
		{"workstream canceled", "canceled", refusedOperation, func(r *reservationRig) {
			r.acknowledge()
			h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, Revision: 1, ID: "supersede", Project: project, Workstream: stream, At: demoStart, Actor: ownerActor, Cause: "supersede"}
			_, err := r.repo.SetFeatureState(ctx, h, AbandonedState, "Replaced by another workstream")
			must(t, err)
		}},
		{"workstream paused", "blocked", refusedOperation, func(r *reservationRig) {
			must(t, r.s.setPause(runtime.Pause{Target: runtime.Target{Scope: "workstream", Project: project, Workstream: stream}, Mode: "soft", Reason: "Looking at it", Source: runtime.PauseOwner, SetAt: r.clock.Now()}))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newReservationRig(t)
			start := tc.refuse(r)
			tc.change(r)
			r.clock.Advance(time.Hour)
			r.restart()
			if open := r.open(); len(open) != 1 || !open[0].Recovered {
				t.Fatalf("the restart left %+v", open)
			}
			if got := r.waits(); len(got) != 0 {
				t.Fatalf("a restart sent %d wait spans for a wait that is still open", len(got))
			}
			r.s.reservationWaitPass(r.repo)
			r.s.reservationWaitPass(r.repo)
			r.restart()
			r.s.reservationWaitPass(r.repo)
			waits := r.waits()
			if len(waits) != 1 || len(r.open()) != 0 {
				t.Fatalf("%d wait spans and %d open waits, want the wait closed once", len(waits), len(r.open()))
			}
			a := spanAttrs(waits[0])
			if a["osmia.outcome"] != tc.outcome || !waits[0].StartTime.Equal(start) || waits[0].EndTime.Before(start) {
				t.Fatalf("wait %s to %s with %v, want outcome %s from %s", waits[0].StartTime, waits[0].EndTime, a, tc.outcome, start)
			}
		})
	}
}

// refusedOperation fills the committee pool and has the rig's operation refused.
func refusedOperation(r *reservationRig) time.Time {
	r.hold()
	start := r.clock.Now()
	r.refuse()
	return start
}

func reservationWaitOf(workstream config.WorkstreamID, start time.Time) slotspans.Interval {
	iv := slotspans.Interval{Kind: slotspans.Wait, Source: slotspans.SourceReservation, Pool: committeeRole, Role: committeeRole, Project: project, Workstream: workstream, Agent: committeeAgent(1), Work: committeeAgent(1), Start: start, Reason: slotspans.ReasonCapacity}
	iv.Bound(1, true)
	return iv
}
