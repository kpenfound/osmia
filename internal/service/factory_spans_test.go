package service

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/telemetry"
	"github.com/kpenfound/osmia/internal/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func namedFactorySpans(exporter *tracetest.InMemoryExporter, name string) []tracetest.SpanStub {
	var out []tracetest.SpanStub
	for _, s := range exporter.GetSpans() {
		if s.Name == name {
			out = append(out, s)
		}
	}
	slices.SortStableFunc(out, func(a, b tracetest.SpanStub) int { return a.StartTime.Compare(b.StartTime) })
	return out
}

func factoryAttr(s tracetest.SpanStub, key string) (string, bool) {
	for _, kv := range s.Attributes {
		if string(kv.Key) == key {
			return kv.Value.String(), true
		}
	}
	return "", false
}

func poolSpans(exporter *tracetest.InMemoryExporter) map[string][]tracetest.SpanStub {
	out := map[string][]tracetest.SpanStub{}
	for _, s := range namedFactorySpans(exporter, "osmia.pool.limit") {
		id, _ := factoryAttr(s, telemetry.KeyPool)
		out[id] = append(out[id], s)
	}
	return out
}

func TestPoolLimitSpansCoverEveryConfiguredPoolAtShutdown(t *testing.T) {
	t.Parallel()
	exporter := tracetest.NewInMemoryExporter()
	opts := fixture(t)
	s, _ := startTelemetry(t, opts, TestHooks{TelemetryExporter: exporter})
	cfg := s.current().Capacity
	fixturekit.Must(t, s.Close())

	pools := poolSpans(exporter)
	want := map[string]int{masonRole: cfg.Masons, reviewerRole: cfg.Reviewers, committeeRole: cfg.Committee, checksRole: cfg.Checks, architectRole: 0}
	if len(pools) != len(want) {
		t.Fatalf("pool limit spans for %d pools, want %d: %+v", len(pools), len(want), pools)
	}
	uptime := namedFactorySpans(exporter, "osmia.service.uptime")
	if len(uptime) != 1 {
		t.Fatalf("uptime spans: %+v", uptime)
	}
	for id, limit := range want {
		spans := pools[id]
		if len(spans) != 1 {
			t.Fatalf("pool %s has %d spans, want 1", id, len(spans))
		}
		span := spans[0]
		if roles, _ := factoryAttr(span, telemetry.KeyPoolRoles); roles != `["`+id+`"]` {
			t.Fatalf("pool %s roles %q", id, roles)
		}
		if outcome, _ := factoryAttr(span, telemetry.KeyOutcome); outcome != "shutdown" {
			t.Fatalf("pool %s ended with %q", id, outcome)
		}
		got, hasLimit := factoryAttr(span, telemetry.KeyPoolLimit)
		unbounded, marked := factoryAttr(span, telemetry.KeyPoolUnbounded)
		if limit == 0 {
			if hasLimit || !marked || unbounded != "true" {
				t.Fatalf("unbounded pool %s: limit %q (%v) unbounded %q (%v)", id, got, hasLimit, unbounded, marked)
			}
		} else if !hasLimit || got != strconv.Itoa(limit) || marked {
			t.Fatalf("pool %s: limit %q (%v) unbounded marked %v, want limit %d", id, got, hasLimit, marked, limit)
		}
		if !span.StartTime.Equal(uptime[0].StartTime) || span.EndTime.Before(span.StartTime) || span.Parent.SpanID() != uptime[0].SpanContext.SpanID() || span.SpanContext.TraceID() != uptime[0].SpanContext.TraceID() {
			t.Fatalf("pool %s span %+v does not run with the uptime span %+v", id, span, uptime[0])
		}
	}
}

func TestReloadSplitsOnlyThePoolWhoseLimitChanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	exporter := tracetest.NewInMemoryExporter()
	opts := fixture(t)
	files := fixturekit.ConfigFiles(t, opts.Config.Root)
	files.Write(t, files.TopText+"[capacity]\nmasons = 2\n")
	loaded, err := projectstore.LoadConfig(opts.Config)
	fixturekit.Must(t, err)
	seed, err := trace.Create(ctx, loaded.Root, loaded.Project, fixturekit.DemoStart, serviceActor)
	fixturekit.Must(t, err)
	fixturekit.Must(t, seed.CreateWorkstream(ctx, fixturekit.Stream, fixturekit.DemoStart, serviceActor))
	fixturekit.Must(t, seed.Close())
	s, c := startTelemetry(t, opts, TestHooks{TelemetryExporter: exporter})
	repo, err := s.repository(fixturekit.Project)
	fixturekit.Must(t, err)
	limits := s.current().Capacity
	release, admitted, err := s.pool.TryReserve(repo, fixturekit.Stream, masonRole, "held", &limits)
	fixturekit.Must(t, err)
	if !admitted {
		t.Fatal("the mason slot was refused")
	}
	defer release()

	files.Write(t, files.TopText+"[capacity]\nmasons = 2\n")
	_, err = c.Reload(ctx)
	fixturekit.Must(t, err)
	before := time.Now().UTC()
	files.Write(t, files.TopText+"[capacity]\nmasons = 4\n")
	_, err = c.Reload(ctx)
	fixturekit.Must(t, err)
	after := time.Now().UTC()
	fixturekit.Must(t, s.Close())

	pools := poolSpans(exporter)
	for id, spans := range pools {
		if want := map[bool]int{true: 2, false: 1}[id == masonRole]; len(spans) != want {
			t.Fatalf("pool %s has %d spans, want %d", id, len(spans), want)
		}
	}
	mason := pools[masonRole]
	first, second := mason[0], mason[1]
	if limit, _ := factoryAttr(first, telemetry.KeyPoolLimit); limit != "2" {
		t.Fatalf("the first mason interval has limit %s", limit)
	}
	if limit, _ := factoryAttr(second, telemetry.KeyPoolLimit); limit != "4" {
		t.Fatalf("the second mason interval has limit %s", limit)
	}
	if outcome, _ := factoryAttr(first, telemetry.KeyOutcome); outcome != "reloaded" {
		t.Fatalf("the first interval ended with %q", outcome)
	}
	if !first.EndTime.Equal(second.StartTime) || first.EndTime.Before(before) || first.EndTime.After(after) {
		t.Fatalf("the reload at [%s, %s] split the span at %s, %s", before, after, first.EndTime, second.StartTime)
	}
	if first.SpanContext.SpanID() == second.SpanContext.SpanID() {
		t.Fatal("two intervals share a span ID")
	}
}

// A pause that is set and later released is one span covering it, with its
// scope and source, and without its reason.
func TestPauseIsOneSpanWithoutItsReason(t *testing.T) {
	t.Parallel()
	exporter := tracetest.NewInMemoryExporter()
	s, _ := startTelemetry(t, fixture(t), TestHooks{TelemetryExporter: exporter})
	const reason = "reason-text with spaces"
	target := runtime.Target{Scope: "workstream", Project: fixturekit.Project, Workstream: fixturekit.Stream}
	setAt := time.Now().UTC().Add(-time.Minute)
	fixturekit.Must(t, s.setPause(runtime.Pause{Target: target, Mode: "soft", Reason: reason, Source: runtime.PauseOwner, SetAt: setAt}))
	fixturekit.Must(t, s.clearPause(target))
	fixturekit.Must(t, s.clearPause(target))
	fixturekit.Must(t, s.Close())

	spans := namedFactorySpans(exporter, "osmia.pause")
	if len(spans) != 1 {
		t.Fatalf("pause spans: %+v", spans)
	}
	span := spans[0]
	for key, want := range map[string]string{telemetry.KeyPauseScope: "workstream", telemetry.KeyPauseSource: "owner", telemetry.KeyOutcome: "released", telemetry.KeyProject: string(fixturekit.Project), telemetry.KeyWorkstream: string(fixturekit.Stream)} {
		if got, _ := factoryAttr(span, key); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	if !span.StartTime.Equal(setAt) || !span.EndTime.After(span.StartTime) {
		t.Fatalf("the pause span runs %s to %s, set at %s", span.StartTime, span.EndTime, setAt)
	}
	for _, s := range exporter.GetSpans() {
		for _, kv := range s.Attributes {
			if strings.Contains(kv.Value.String(), "reason") || strings.Contains(s.Name, "reason") {
				t.Fatalf("span %s carries pause reason text in %s", s.Name, kv.Key)
			}
		}
	}
}

// A pause replaced by another on the same target ends the first as replaced;
// a pause still set at shutdown is sent when it ends, after a restart too.
func TestPauseSpanSurvivesARestart(t *testing.T) {
	t.Parallel()
	exporter := tracetest.NewInMemoryExporter()
	opts := fixture(t)
	s, _ := startTelemetry(t, opts, TestHooks{TelemetryExporter: exporter})
	target := runtime.Target{Scope: "project", Project: fixturekit.Project}
	setAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	fixturekit.Must(t, s.setPause(runtime.Pause{Target: target, Mode: "soft", Reason: "first", Source: runtime.PauseOwner, SetAt: setAt}))
	fixturekit.Must(t, s.setPause(runtime.Pause{Target: target, Mode: "hard", Reason: "second", Source: runtime.PauseOwner, SetAt: setAt.Add(time.Minute)}))
	fixturekit.Must(t, s.Close())
	if spans := namedFactorySpans(exporter, "osmia.pause"); len(spans) != 1 {
		t.Fatalf("the replaced pause: %+v", spans)
	} else if outcome, _ := factoryAttr(spans[0], telemetry.KeyOutcome); outcome != "replaced" {
		t.Fatalf("outcome %q", outcome)
	}

	s2, _ := startTelemetry(t, opts, TestHooks{TelemetryExporter: exporter})
	fixturekit.Must(t, s2.clearPause(target))
	fixturekit.Must(t, s2.Close())
	spans := namedFactorySpans(exporter, "osmia.pause")
	if len(spans) != 2 || !spans[1].StartTime.Equal(setAt.Add(time.Minute)) {
		t.Fatalf("pause spans after the restart: %+v", spans)
	}
	if spans[0].SpanContext.SpanID() == spans[1].SpanContext.SpanID() {
		t.Fatal("two pauses share a span ID")
	}
}

// Service start is an event and stopping the service sends an uptime span
// from the start to the shutdown.
func TestServiceStartEventAndUptimeSpan(t *testing.T) {
	t.Parallel()
	exporter := tracetest.NewInMemoryExporter()
	s, _ := startTelemetry(t, fixture(t), TestHooks{TelemetryExporter: exporter})
	fixturekit.Must(t, s.Close())
	starts, uptime := namedFactorySpans(exporter, "osmia.service.start"), namedFactorySpans(exporter, "osmia.service.uptime")
	if len(starts) != 1 || len(uptime) != 1 {
		t.Fatalf("start %+v, uptime %+v", starts, uptime)
	}
	start := starts[0]
	if !start.StartTime.Equal(start.EndTime) || !start.StartTime.Equal(uptime[0].StartTime) || len(start.Events) != 1 || start.Events[0].Name != "osmia.service.start" {
		t.Fatalf("service start %+v, uptime %+v", start, uptime[0])
	}
	if !uptime[0].EndTime.After(uptime[0].StartTime) || start.Parent.SpanID() != uptime[0].SpanContext.SpanID() {
		t.Fatalf("uptime %+v", uptime[0])
	}
	if outcome, _ := factoryAttr(uptime[0], telemetry.KeyOutcome); outcome != "shutdown" {
		t.Fatalf("uptime outcome %q", outcome)
	}
	for _, s := range []tracetest.SpanStub{start, uptime[0]} {
		if s.SpanContext.TraceID() != telemetry.FactoryTrace(factoryRun) {
			t.Fatalf("%s is not in the factory trace", s.Name)
		}
	}
}

// With telemetry off nothing is recorded: no spans and no ledger file.
func TestFactorySpansWriteNothingWhileTelemetryIsOff(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	s, _ := start(t, opts)
	fixturekit.Must(t, s.Close())
	if _, err := os.Stat(filepath.Join(opts.Config.Root, "service-intervals.json")); !os.IsNotExist(err) {
		t.Fatalf("the interval ledger exists with telemetry off: %v", err)
	}
}

// The loop guard tripping on a workstream is an event, and its pause is a span
// of source loop-guard once the owner resumes the workstream.
func TestLoopGuardTripIsAnEventAndItsPauseASpan(t *testing.T) {
	t.Parallel()
	exporter := tracetest.NewInMemoryExporter()
	f, stream, repository := newReviewFixture(t, "loop-guard")
	openRuntime(t, f, stream)
	f.s.cfg.Loop.MaxSessions = 3
	sink, err := telemetry.New(config.Telemetry{}, telemetry.Options{Exporter: exporter})
	fixturekit.Must(t, err)
	f.s.spans = newFactorySpans(sink, "", f.s.now, nil)
	f.s.store.ObservePauseEnd(func(p runtime.Pause, replaced bool) { f.s.spans.pauseEnded(p, f.s.now(), replaced) })
	f.s.spans.begin(poolsOf(f.s.cfg.Capacity))
	g := loopGuard{s: f.s, cfg: f.s.cfg, repository: repository}
	ctx := context.Background()

	recordSessions(t, f, repository, stream, 3)
	fixturekit.Must(t, g.Pass(ctx))
	pause, paused := loopPause(f, stream)
	if !paused {
		t.Fatal("the loop guard did not pause the workstream")
	}
	fixturekit.Must(t, f.s.store.ClearPause(pause.Target, runtime.PauseOwner))
	f.s.spans.end()
	fixturekit.Must(t, sink.Close())

	trips := namedFactorySpans(exporter, "osmia.loop_guard.trip")
	if len(trips) != 1 {
		t.Fatalf("loop guard trips: %+v", trips)
	}
	for key, want := range map[string]string{telemetry.KeyProject: string(f.project), telemetry.KeyWorkstream: string(stream), telemetry.KeyPauseSource: runtime.PauseLoopGuard} {
		if got, _ := factoryAttr(trips[0], key); got != want {
			t.Fatalf("trip %s = %q, want %q", key, got, want)
		}
	}
	if !trips[0].StartTime.Equal(trips[0].EndTime) || len(trips[0].Events) != 1 {
		t.Fatalf("trip is not an event: %+v", trips[0])
	}
	pauses := namedFactorySpans(exporter, "osmia.pause")
	if len(pauses) != 1 {
		t.Fatalf("pause spans: %+v", pauses)
	}
	if source, _ := factoryAttr(pauses[0], telemetry.KeyPauseSource); source != runtime.PauseLoopGuard {
		t.Fatalf("pause source %q", source)
	}
	for _, s := range exporter.GetSpans() {
		for _, kv := range s.Attributes {
			if strings.Contains(kv.Value.String(), "Loop guard") {
				t.Fatalf("span %s carries the pause reason in %s", s.Name, kv.Key)
			}
		}
		for _, e := range s.Events {
			for _, kv := range e.Attributes {
				if strings.Contains(kv.Value.String(), "Loop guard") {
					t.Fatalf("event %s carries the pause reason", e.Name)
				}
			}
		}
	}
}

// A run that never shut down leaves its open intervals in the ledger; the next
// start sends them, closed at the last time the run was seen alive.
func TestNextStartClosesTheIntervalsOfARunThatNeverShutDown(t *testing.T) {
	t.Parallel()
	exporter := tracetest.NewInMemoryExporter()
	sink, err := telemetry.New(config.Telemetry{}, telemetry.Options{Exporter: exporter})
	fixturekit.Must(t, err)
	ledger := filepath.Join(t.TempDir(), "service-intervals.json")
	var mu sync.Mutex
	clock := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	advance := func(d time.Duration) { mu.Lock(); clock = clock.Add(d); mu.Unlock() }
	capacity := config.Capacity{Masons: 2, Reviewers: 2, Committee: 3, Checks: 2}

	first := newFactorySpans(sink, ledger, now, nil)
	first.begin(poolsOf(capacity))
	advance(time.Minute)
	capacity.Masons = 4
	first.reload(now(), poolsOf(capacity))
	advance(time.Minute)
	first.mu.Lock()
	first.save(now())
	first.mu.Unlock()
	crashed, err := os.ReadFile(ledger)
	fixturekit.Must(t, err)
	last := now()
	first.end()
	fixturekit.Must(t, os.WriteFile(ledger, crashed, 0o600))
	fixturekit.Must(t, sink.Flush(context.Background()))
	exporter.Reset()

	advance(time.Hour)
	second := newFactorySpans(sink, ledger, now, nil)
	second.begin(poolsOf(capacity))
	second.end()
	fixturekit.Must(t, sink.Close())

	var interrupted []tracetest.SpanStub
	for _, s := range exporter.GetSpans() {
		if outcome, _ := factoryAttr(s, telemetry.KeyOutcome); outcome == "interrupted" {
			interrupted = append(interrupted, s)
		}
	}
	if len(interrupted) != 6 {
		t.Fatalf("%d spans of the interrupted run, want its five pools and its uptime", len(interrupted))
	}
	for _, s := range interrupted {
		if !s.EndTime.Equal(last) {
			t.Fatalf("%s ended at %s, want the last time the run was alive, %s", s.Name, s.EndTime, last)
		}
	}
	uptime := namedFactorySpans(exporter, "osmia.service.uptime")
	if len(uptime) != 2 || uptime[0].SpanContext.SpanID() == uptime[1].SpanContext.SpanID() {
		t.Fatalf("uptime spans %+v", uptime)
	}
	if _, err := os.Stat(ledger); !os.IsNotExist(err) {
		t.Fatalf("the ledger outlives a clean shutdown: %v", err)
	}
}
