package telemetry

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

var (
	start = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	span  = Span{
		Name:   SpanName("unit", "state"),
		Trace:  WorkstreamTrace(streamA),
		ID:     RecordSpan(streamA, "rec-1", 1),
		Parent: WorkstreamRoot(streamA),
		Start:  start,
		End:    start.Add(90 * time.Second),
		Attrs:  []Attr{Role("mason"), Workstream(streamA), Unit("u1"), State("implementing"), NextState("reviewing"), Cause("review_bounce")},
	}
)

func memorySink(t *testing.T, opts Options) (*Sink, *tracetest.InMemoryExporter) {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	opts.Exporter = exporter
	sink, err := New(config.Telemetry{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sink.Close() })
	return sink, exporter
}

func flush(t *testing.T, sink *Sink) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sink.Flush(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestTelemetryOffBuildsNoExporterAndMakesNoConnection(t *testing.T) {
	listener := collector(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+listener.addr)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://"+listener.addr)
	var reads int
	for _, cfg := range []config.Telemetry{
		{},
		{Protocol: config.TelemetryProtocolGRPC},
		{Protocol: config.TelemetryProtocolHTTP, Insecure: true, HeadersEnv: "OSMIA_TEST_HEADERS"},
	} {
		sink, err := New(cfg, Options{Getenv: func(string) string { reads++; return "authorization=x" }})
		if err != nil || sink == nil || sink.Enabled() {
			t.Fatalf("%+v: sink %+v, %v", cfg, sink, err)
		}
		sink.Emit(span)
		if err := sink.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := sink.Close(); err != nil {
			t.Fatal(err)
		}
		if (sink.Stats() != Stats{}) {
			t.Fatalf("a disabled sink counted %+v", sink.Stats())
		}
	}
	var none *Sink
	none.Emit(span)
	if none.Enabled() || none.Close() != nil || none.Flush(context.Background()) != nil {
		t.Fatal("a nil sink must discard spans")
	}
	if reads != 0 {
		t.Fatalf("the headers variable was read %d times with telemetry off", reads)
	}
	if n := listener.connections(); n != 0 {
		t.Fatalf("telemetry off made %d connections", n)
	}
}

func TestEmittedSpansKeepTheirRecordedIdentityAndTimes(t *testing.T) {
	sink, exporter := memorySink(t, Options{Version: "v1.2.3"})
	s := span
	s.Attrs = append(s.Attrs, String("osmia.prompt", "do the thing"), Role("reviewer"))
	s.Events = []Event{{Name: SpanName("loop_guard"), At: start.Add(time.Second), Attrs: []Attr{Cause("trip"), String("osmia.reason", "stuck")}}, {Name: Name{}, At: start}}
	s.Failed = true
	sink.Emit(s)
	flush(t, sink)
	got := exporter.GetSpans()
	if len(got) != 1 {
		t.Fatalf("exported %d spans", len(got))
	}
	g := got[0]
	if g.Name != "osmia.unit.state" || g.SpanContext.TraceID() != span.Trace || g.SpanContext.SpanID() != span.ID || g.Parent.SpanID() != span.Parent || g.Parent.TraceID() != span.Trace {
		t.Fatalf("identity: %+v parent %+v", g.SpanContext, g.Parent)
	}
	if !g.StartTime.Equal(span.Start) || !g.EndTime.Equal(span.End) {
		t.Fatalf("times %v - %v, want %v - %v", g.StartTime, g.EndTime, span.Start, span.End)
	}
	if g.Status.Code.String() != "Error" || g.Status.Description != "" {
		t.Fatalf("status %+v", g.Status)
	}
	attrs := map[string]string{}
	for _, kv := range g.Attributes {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	if len(attrs) != 6 || attrs[KeyRole] != "mason" || attrs[KeyCause] != "review_bounce" || attrs[KeyWorkstream] != string(streamA) || attrs[KeyNextState] != "reviewing" {
		t.Fatalf("attributes %+v", attrs)
	}
	if len(g.Events) != 1 || g.Events[0].Name != "osmia.loop_guard" || len(g.Events[0].Attributes) != 1 || !g.Events[0].Time.Equal(start.Add(time.Second)) {
		t.Fatalf("events %+v", g.Events)
	}
	if st := sink.Stats(); st.Exported != 1 || st.DroppedAttrs != 4 || st.Dropped != 0 || st.Rejected != 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestResourceIgnoresTheEnvironment(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "from-env")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment=env,service.name=also-env")
	sink, exporter := memorySink(t, Options{Version: "v1.2.3"})
	sink.Emit(span)
	flush(t, sink)
	got := exporter.GetSpans()
	if len(got) != 1 {
		t.Fatalf("exported %d spans", len(got))
	}
	attrs := map[string]string{}
	for _, kv := range got[0].Resource.Attributes() {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	if len(attrs) != 2 || attrs["service.name"] != "osmia" || attrs["service.version"] != "v1.2.3" {
		t.Fatalf("resource attributes %+v", attrs)
	}
}

func TestUnusableSpansAreRejected(t *testing.T) {
	sink, exporter := memorySink(t, Options{})
	for _, mutate := range map[string]func(*Span){
		"no name":    func(s *Span) { s.Name = Name{} },
		"no trace":   func(s *Span) { s.Trace = [16]byte{} },
		"no span":    func(s *Span) { s.ID = [8]byte{} },
		"no start":   func(s *Span) { s.Start = time.Time{} },
		"end before": func(s *Span) { s.End = s.Start.Add(-time.Second) },
	} {
		s := span
		mutate(&s)
		sink.Emit(s)
	}
	flush(t, sink)
	if n := len(exporter.GetSpans()); n != 0 || sink.Stats().Rejected != 5 {
		t.Fatalf("exported %d, stats %+v", n, sink.Stats())
	}
}

type blockingExporter struct {
	release      chan struct{}
	honorContext bool
	started      chan struct{}
	once         sync.Once
}

func (b *blockingExporter) ExportSpans(ctx context.Context, _ []sdktrace.ReadOnlySpan) error {
	b.once.Do(func() { close(b.started) })
	if b.honorContext {
		select {
		case <-b.release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	}
	<-b.release
	return nil
}

func (b *blockingExporter) Shutdown(context.Context) error { return nil }

func newBlocking(t *testing.T, honorContext bool) *blockingExporter {
	t.Helper()
	b := &blockingExporter{release: make(chan struct{}), honorContext: honorContext, started: make(chan struct{})}
	t.Cleanup(func() { close(b.release) })
	return b
}

func TestBlockedExporterNeverBlocksEmitOrShutdown(t *testing.T) {
	for _, honorContext := range []bool{true, false} {
		t.Run(map[bool]string{true: "exporter honors its context", false: "exporter ignores its context"}[honorContext], func(t *testing.T) {
			blocked := newBlocking(t, honorContext)
			sink, err := New(config.Telemetry{}, Options{Exporter: blocked, QueueSize: 8, BatchSize: 4, FlushInterval: time.Millisecond, ExportTimeout: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			sink.Emit(span)
			<-blocked.started
			began := time.Now()
			for range 5000 {
				sink.Emit(span)
			}
			if took := time.Since(began); took > 2*time.Second {
				t.Fatalf("5000 emits took %v against a blocked exporter", took)
			}
			if st := sink.Stats(); st.Dropped < 4900 {
				t.Fatalf("the full queue dropped %d of 5001 spans", st.Dropped)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			began = time.Now()
			err = sink.Shutdown(ctx)
			if !errors.Is(err, ErrShutdownTimeout) || time.Since(began) > 2*time.Second {
				t.Fatalf("shutdown returned %v after %v", err, time.Since(began))
			}
			began = time.Now()
			sink.Emit(span)
			if time.Since(began) > time.Second {
				t.Fatal("emit after shutdown blocked")
			}
		})
	}
}

func TestFailingExporterDropsBatchesQuietly(t *testing.T) {
	var logs bytes.Buffer
	var calls atomic.Int32
	failing := funcExporter(func(context.Context, []sdktrace.ReadOnlySpan) error {
		if calls.Add(1)%2 == 0 {
			panic("exporter bug with s3cret")
		}
		return errors.New("collector said no to Bearer s3cret")
	})
	sink, err := New(config.Telemetry{}, Options{Exporter: failing, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	sink.secrets = []string{"s3cret"}
	for range 3 {
		sink.Emit(span)
		flush(t, sink)
	}
	if st := sink.Stats(); st.Failed != 3 || st.Exported != 0 {
		t.Fatalf("stats %+v", st)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	if strings.Contains(out, "s3cret") || strings.Count(out, "export failed") != 1 || !strings.Contains(out, "[redacted]") {
		t.Fatalf("failure log:\n%s", out)
	}
}

type funcExporter func(context.Context, []sdktrace.ReadOnlySpan) error

func (f funcExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	return f(ctx, spans)
}
func (funcExporter) Shutdown(context.Context) error { return nil }

func TestShutdownFlushesPendingSpans(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	sink, err := New(config.Telemetry{}, Options{Exporter: exporter, FlushInterval: time.Hour, BatchSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 95 {
		s := span
		s.ID = RecordSpan(streamA, "rec", i)
		sink.Emit(s)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sink.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(exporter.GetSpans()); n != 95 {
		t.Fatalf("shutdown exported %d of 95 spans", n)
	}
	if st := sink.Stats(); st.Exported != 95 || st.Dropped != 0 {
		t.Fatalf("stats %+v", st)
	}
	if err := sink.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	sink.Emit(span)
	if st := sink.Stats(); st.Dropped != 1 {
		t.Fatalf("stats after a late emit %+v", st)
	}
}

func TestEmitFromManyGoroutinesIsSafe(t *testing.T) {
	sink, exporter := memorySink(t, Options{QueueSize: 64, BatchSize: 8, FlushInterval: time.Millisecond})
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 200 {
				sink.Emit(span)
			}
		})
	}
	wg.Wait()
	flush(t, sink)
	st := sink.Stats()
	if got := int64(len(exporter.GetSpans())); got != st.Exported || st.Exported+st.Dropped != 3200 {
		t.Fatalf("exported %d, stats %+v", got, st)
	}
}
