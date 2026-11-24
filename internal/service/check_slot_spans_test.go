package service

import (
	"context"
	"fmt"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/slotspans"
	"github.com/kpenfound/osmia/internal/telemetry"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type spanClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *spanClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *spanClock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}

func spanAttrs(s tracetest.SpanStub) map[string]string {
	out := map[string]string{}
	for _, kv := range s.Attributes {
		out[string(kv.Key)] = kv.Value.String()
	}
	return out
}

func waitForQueue(t *testing.T, p *checkSlotPool, n int) {
	t.Helper()
	intakefixture.Soon(t, fmt.Sprintf("the check queue to hold %d runs", n), func() bool { return len(p.status().Waiting) == n })
}

func TestCheckPoolSendsHeldAndWaitSlotSpans(t *testing.T) {
	ctx := context.Background()
	exporter := tracetest.NewInMemoryExporter()
	sink, err := telemetry.New(config.Telemetry{}, telemetry.Options{Exporter: exporter})
	fixturekit.Must(t, err)
	defer sink.Close()
	clock := &spanClock{now: time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)}
	rec := slotspans.New(sink, slotspans.Options{Now: clock.Now})
	defer rec.Close()
	p := &checkSlotPool{limit: 1, spans: rec, now: clock.Now}
	const ws config.WorkstreamID = "w_00000000000000000000000000000001"
	run := func(unit string) CheckSlotRun {
		return CheckSlotRun{Project: "p_00000000000000000000000000000001", Workstream: ws, Unit: unit}
	}

	t0 := clock.Now()
	holder, err := p.acquire(ctx, run("holder"))
	fixturekit.Must(t, err)
	t1 := clock.Advance(5 * time.Second)
	queued := queuedSlot(ctx, p, run("queued"))
	waitForQueue(t, p, 1)
	cancelCtx, cancel := context.WithCancel(ctx)
	canceled := queuedSlot(cancelCtx, p, run("canceled"))
	waitForQueue(t, p, 2)
	t2 := clock.Advance(5 * time.Second)
	cancel()
	if release := awaitCheck(t, canceled); release != nil {
		t.Fatal("a canceled check was granted a slot")
	}
	t3 := clock.Advance(5 * time.Second)
	holder()
	release := awaitCheck(t, queued)
	t4 := clock.Advance(5 * time.Second)
	release()

	fixturekit.Must(t, sink.Flush(ctx))
	type bounds struct{ start, end time.Time }
	waits, holds := map[string]bounds{}, map[string]bounds{}
	outcomes := map[string]string{}
	for _, s := range exporter.GetSpans() {
		a := spanAttrs(s)
		if a["osmia.role"] != "checks" || a["osmia.pool.id"] != "checks" || a["osmia.pool.limit"] != "1" || a["osmia.project.id"] == "" {
			t.Fatalf("span %s attributes %v", s.Name, a)
		}
		switch s.Name {
		case "osmia.slot.wait":
			waits[a["osmia.unit.id"]] = bounds{s.StartTime, s.EndTime}
			outcomes[a["osmia.unit.id"]] = a["osmia.outcome"]
			if a["osmia.wait_reason"] != "capacity" {
				t.Fatalf("wait attributes %v", a)
			}
		case "osmia.slot.held":
			holds[a["osmia.unit.id"]] = bounds{s.StartTime, s.EndTime}
			if a["osmia.outcome"] != "released" {
				t.Fatalf("hold attributes %v", a)
			}
		default:
			t.Fatalf("unexpected span %s", s.Name)
		}
	}
	want := func(got map[string]bounds, unit string, start, end time.Time) {
		t.Helper()
		b, ok := got[unit]
		if !ok || !b.start.Equal(start) || !b.end.Equal(end) {
			t.Fatalf("%s spans %v, want %s to %s", unit, got, start, end)
		}
	}
	want(holds, "holder", t0, t3)
	want(holds, "queued", t3, t4)
	want(waits, "queued", t1, t3)
	want(waits, "canceled", t1, t2)
	if len(holds) != 2 || len(waits) != 2 {
		t.Fatalf("holds %v waits %v: a canceled check holds no slot and a granted one waits once", holds, waits)
	}
	if outcomes["queued"] != "granted" || outcomes["canceled"] != "canceled" {
		t.Fatalf("wait outcomes %v", outcomes)
	}
}

// The service keeps the intervals still open when it stops, and the next run
// sends the ones no holder survived as interrupted, ending where it started.
func TestCheckSlotHeldWhenTheServiceStopsIsSentInterruptedByTheNextRun(t *testing.T) {
	ctx := context.Background()
	exporter := tracetest.NewInMemoryExporter()
	opts := fixture(t)
	s, _ := startTelemetry(t, opts, TestHooks{TelemetryExporter: exporter})
	if s.slots == nil || s.pool.Spans != s.slots || s.checkSlots.spans != s.slots {
		t.Fatal("the capacity owners do not share the service's slot recorder")
	}
	run := CheckSlotRun{Project: "p_00000000000000000000000000000001", Workstream: "w_00000000000000000000000000000001", Unit: "unit"}
	if _, err := s.checkSlots.acquire(ctx, run); err != nil {
		t.Fatal(err)
	}
	fixturekit.Must(t, s.Close())
	if _, err := os.Stat(filepath.Join(opts.Config.Root, "slot-intervals.json")); err != nil {
		t.Fatalf("the open interval was not kept: %v", err)
	}
	if got := slotSpansOf(exporter); len(got) != 0 {
		t.Fatalf("a stop sent %d slot spans for a slot still held", len(got))
	}

	s, _ = startTelemetry(t, opts, TestHooks{TelemetryExporter: exporter})
	fixturekit.Must(t, s.Close())
	got := slotSpansOf(exporter)
	if len(got) != 1 || got[0].Name != "osmia.slot.held" {
		t.Fatalf("spans after the next run: %+v", got)
	}
	if a := spanAttrs(got[0]); a["osmia.outcome"] != "interrupted" || a["osmia.role"] != "checks" || a["osmia.pool.limit"] == "" {
		t.Fatalf("attributes %v", a)
	}
	if !got[0].EndTime.After(got[0].StartTime) {
		t.Fatalf("interrupted span %s to %s", got[0].StartTime, got[0].EndTime)
	}
}

func slotSpansOf(exporter *tracetest.InMemoryExporter) tracetest.SpanStubs {
	var out tracetest.SpanStubs
	for _, sp := range exporter.GetSpans() {
		if strings.HasPrefix(sp.Name, "osmia.slot.") {
			out = append(out, sp)
		}
	}
	return out
}

// With telemetry off nothing is tracked, so no ledger is written.
func TestSlotIntervalsAreNotKeptWhileTelemetryIsOff(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	s, _ := start(t, opts)
	if s.slots != nil {
		t.Fatal("a slot recorder was built while telemetry is off")
	}
	if _, err := s.checkSlots.acquire(context.Background(), CheckSlotRun{Unit: "unit"}); err != nil {
		t.Fatal(err)
	}
	fixturekit.Must(t, s.Close())
	if _, err := os.Stat(filepath.Join(opts.Config.Root, "slot-intervals.json")); !os.IsNotExist(err) {
		t.Fatalf("ledger stat: %v", err)
	}
}

// The limits slot spans carry follow the configuration in force.
func TestPoolLimitsFollowTheConfigurationInForce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts := fixture(t)
	files := configFiles(t, opts)
	s, c := start(t, opts)
	before, _ := s.poolLimit(masonRole)
	files.write(t, files.topText+"[capacity]\nmasons = "+strconv.Itoa(before+3)+"\n")
	_, err := c.Reload(ctx)
	fixturekit.Must(t, err)
	if got, bounded := s.poolLimit(masonRole); !bounded || got != before+3 {
		t.Fatalf("mason limit %d after a reload that set %d", got, before+3)
	}
	if _, bounded := s.poolLimit(architectRole); bounded {
		t.Fatal("the architect pool has a limit")
	}
}
