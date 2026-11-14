package slotspans

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/telemetry"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

const (
	project config.ProjectID    = "p_00000000000000000000000000000001"
	stream  config.WorkstreamID = "w_00000000000000000000000000000001"
)

var t0 = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type rig struct {
	t        *testing.T
	exporter *tracetest.InMemoryExporter
	sink     *telemetry.Sink
	clock    *clock
	ledger   string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	sink, err := telemetry.New(config.Telemetry{}, telemetry.Options{Exporter: exporter})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sink.Close() })
	return &rig{t: t, exporter: exporter, sink: sink, clock: &clock{now: t0}, ledger: filepath.Join(t.TempDir(), "slot-intervals.json")}
}

func (r *rig) recorder() *Recorder {
	return New(r.sink, Options{Ledger: r.ledger, Now: r.clock.Now})
}

func (r *rig) spans() []tracetest.SpanStub {
	r.t.Helper()
	if err := r.sink.Flush(context.Background()); err != nil {
		r.t.Fatal(err)
	}
	return r.exporter.GetSpans()
}

func check(unit string, kind Kind, work string, start time.Time) Interval {
	iv := Interval{Kind: kind, Source: SourceCheck, Pool: "checks", Role: "checks", Project: project, Workstream: stream, Unit: unit, Work: work, Start: start}
	iv.Bound(2, true)
	return iv
}

func TestIntervalSendsOneSpanWhenItEndsWithItsRecordedTimes(t *testing.T) {
	r := newRig(t)
	rec := r.recorder()
	defer rec.Close()
	iv := check("u1", Held, "1", t0)
	if _, began := rec.Begin(iv); !began {
		t.Fatal("interval not opened")
	}
	if _, began := rec.Begin(iv); began {
		t.Fatal("the same work opened a second interval")
	}
	r.clock.Advance(time.Hour) // the time of export plays no part
	if got := r.spans(); len(got) != 0 {
		t.Fatalf("a span before the interval ended: %d", len(got))
	}
	end := t0.Add(7 * time.Second)
	if !rec.End(iv, end, Released) || rec.End(iv, end, Released) {
		t.Fatal("an interval ends exactly once")
	}
	got := r.spans()
	if len(got) != 1 || got[0].Name != "osmia.slot.held" || !got[0].StartTime.Equal(t0) || !got[0].EndTime.Equal(end) {
		t.Fatalf("spans %+v", got)
	}
	if got[0].SpanContext.SpanID() != iv.SpanID() || got[0].SpanContext.TraceID() != telemetry.WorkstreamTrace(stream) || got[0].Parent.SpanID() != telemetry.WorkstreamRoot(stream) {
		t.Fatal("span IDs are not the ones derived from the workstream and the interval")
	}
}

func TestUnboundedIntervalsHaveTheMarkerAndNoLimit(t *testing.T) {
	r := newRig(t)
	rec := r.recorder()
	defer rec.Close()
	iv := Interval{Kind: Held, Source: SourceTurn, Pool: "architect", Role: "architect", Project: project, Workstream: stream, Work: "a/1", Start: t0}
	iv.Bound(0, false)
	rec.Begin(iv)
	rec.End(iv, t0.Add(time.Second), Released)
	zero := check("u", Held, "2", t0)
	zero.Bound(0, true)
	rec.Begin(zero)
	rec.End(zero, t0.Add(time.Second), Released)
	for _, s := range r.spans() {
		attrs := map[string]string{}
		for _, kv := range s.Attributes {
			attrs[string(kv.Key)] = kv.Value.String()
		}
		switch attrs["osmia.pool.id"] {
		case "architect":
			if _, ok := attrs["osmia.pool.limit"]; ok || attrs["osmia.pool.unbounded"] != "true" {
				t.Fatalf("unbounded pool attributes %v", attrs)
			}
		case "checks":
			if attrs["osmia.pool.limit"] != "0" {
				t.Fatalf("a bounded pool's own limit of zero is a limit: %v", attrs)
			}
			if _, ok := attrs["osmia.pool.unbounded"]; ok {
				t.Fatalf("bounded pool marked unbounded: %v", attrs)
			}
		}
	}
}

func TestOpenIntervalsSurviveARestartWithTheirOriginalStartAndIdentity(t *testing.T) {
	r := newRig(t)
	rec := r.recorder()
	wait := check("u1", Wait, "7", t0)
	wait.Reason = ReasonCapacity
	rec.Begin(wait)
	rec.Begin(check("u2", Held, "8", t0.Add(time.Second)))
	id := wait.SpanID()
	rec.Close()
	if got := r.spans(); len(got) != 0 {
		t.Fatalf("a restart sent %d spans for intervals that are still open", len(got))
	}

	r.clock.Advance(time.Minute)
	rec = r.recorder()
	defer rec.Close()
	// The owner asks to open the same work again and finds the recorded interval.
	again := wait
	again.Start = r.clock.Now()
	got, began := rec.Begin(again)
	if began || !got.Start.Equal(t0) || got.SpanID() != id || got.Reason != ReasonCapacity {
		t.Fatalf("recovered %+v (new: %v)", got, began)
	}
	end := t0.Add(2 * time.Minute)
	rec.End(wait, end, Granted)
	spans := r.spans()
	if len(spans) != 1 || spans[0].SpanContext.SpanID() != id || !spans[0].StartTime.Equal(t0) || !spans[0].EndTime.Equal(end) {
		t.Fatalf("spans after the restart: %+v", spans)
	}
}

func TestIntervalsOfAPoolWhoseHoldersDoNotSurviveAreSentAsInterrupted(t *testing.T) {
	r := newRig(t)
	rec := r.recorder()
	rec.Begin(check("u1", Held, "1", t0))
	rec.Begin(check("u2", Wait, "2", t0.Add(time.Second)))
	rec.Close()
	r.clock.Advance(time.Minute)
	rec = r.recorder()
	defer rec.Close()
	rec.Interrupt("checks")
	spans := r.spans()
	if len(spans) != 2 {
		t.Fatalf("%d spans, want both intervals", len(spans))
	}
	for _, s := range spans {
		if !s.EndTime.Equal(t0.Add(time.Minute)) {
			t.Fatalf("interrupted span ends at %s, want the start of the run that found it", s.EndTime)
		}
	}
	rec.Interrupt("checks")
	if len(r.spans()) != 2 {
		t.Fatal("interrupting twice sent a span twice")
	}
}

func TestRecoveredIntervalsNoOwnerTakesUpExpireAsInterrupted(t *testing.T) {
	r := newRig(t)
	rec := r.recorder()
	rec.Begin(check("u1", Held, "1", t0))
	rec.Close()
	r.clock.Advance(time.Minute)
	rec = r.recorder()
	defer rec.Close()
	rec.Expire(r.clock.Now().Add(RecoveryGrace / 2))
	if len(r.spans()) != 0 {
		t.Fatal("an interval expired within the grace")
	}
	rec.Expire(r.clock.Now().Add(RecoveryGrace + time.Second))
	spans := r.spans()
	if len(spans) != 1 || !spans[0].EndTime.Equal(t0.Add(time.Minute)) {
		t.Fatalf("spans %+v", spans)
	}
	// An interval an owner took up is its owner's to end.
	rec.Begin(check("u2", Held, "2", t0))
	rec.Expire(r.clock.Now().Add(2 * RecoveryGrace))
	if len(r.spans()) != 1 {
		t.Fatal("a live interval expired")
	}
}

func TestAnUnreadableLedgerStartsEmpty(t *testing.T) {
	r := newRig(t)
	if err := os.WriteFile(r.ledger, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := r.recorder()
	defer rec.Close()
	if open := rec.Find(Held, SourceCheck, nil); len(open) != 0 {
		t.Fatalf("open %+v", open)
	}
	rec.Begin(check("u", Held, "1", t0))
}

func reservationWait(agent string, start time.Time) Interval {
	iv := Interval{Kind: Wait, Source: SourceReservation, Pool: "committee", Role: "committee", Project: project, Workstream: stream, Agent: agent, Work: agent, Start: start, Reason: ReasonCapacity}
	iv.Bound(1, true)
	return iv
}

func TestReservationWaitsSurviveARestartAndAreNotAbandonedBeforeTheirOwnerAsksAgain(t *testing.T) {
	r := newRig(t)
	rec := r.recorder()
	wait := reservationWait("reader", t0)
	rec.Begin(wait)
	id := wait.SpanID()
	rec.Close()

	// The service was down for longer than a wait stays open without a refusal.
	r.clock.Advance(time.Hour)
	rec = r.recorder()
	defer rec.Close()
	other := reservationWait("another", r.clock.Now())
	rec.Begin(other)
	rec.Abandon(r.clock.Now().Add(-5 * time.Minute))
	if got := r.spans(); len(got) != 0 {
		t.Fatalf("a recovered wait was abandoned before its operation asked again: %+v", got)
	}
	again := reservationWait("reader", r.clock.Now())
	got, began := rec.Begin(again)
	if began || !got.Start.Equal(t0) || got.SpanID() != id {
		t.Fatalf("the refusal after the restart opened %+v (new: %v)", got, began)
	}
	end := r.clock.Now()
	rec.End(again, end, Granted)
	spans := r.spans()
	if len(spans) != 1 || spans[0].SpanContext.SpanID() != id || !spans[0].StartTime.Equal(t0) || !spans[0].EndTime.Equal(end) {
		t.Fatalf("spans after the restart: %+v", spans)
	}
}

func TestNilRecorderDoesNothing(t *testing.T) {
	var rec *Recorder
	iv := check("u", Held, "1", t0)
	if _, began := rec.Begin(iv); began || rec.End(iv, t0, Released) {
		t.Fatal("a nil recorder tracked an interval")
	}
	rec.Interrupt("checks")
	rec.Expire(t0)
	rec.Close()
	if rec.Find(Held, SourceCheck, nil) != nil || !rec.Now().IsZero() {
		t.Fatal("a nil recorder reported state")
	}
	off, err := telemetry.New(config.Telemetry{}, telemetry.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if New(off, Options{Ledger: filepath.Join(t.TempDir(), "ledger.json")}) != nil {
		t.Fatal("a recorder was built while telemetry is off")
	}
}
