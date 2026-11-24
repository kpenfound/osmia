package service

import (
	"context"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/telemetry"
	"github.com/kpenfound/osmia/internal/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestServiceSendsWorkstreamSpansAndTheRootOnArchive(t *testing.T) {
	t.Parallel()
	exporter := tracetest.NewInMemoryExporter()
	opts := fixture(t)
	cfg, err := projectstore.LoadConfig(opts.Config)
	intakefixture.Must(t, err)
	created := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	repository, err := trace.Create(context.Background(), cfg.Root, cfg.Project, created, serviceActor)
	intakefixture.Must(t, err)
	intakefixture.Must(t, repository.CreateWorkstream(context.Background(), intakefixture.Stream, created, serviceActor))
	intakefixture.Must(t, repository.Close())
	s, c := startTelemetry(t, opts, TestHooks{TelemetryExporter: exporter})
	_, active := s.runtimeOf(intakefixture.Project)
	if active == nil {
		t.Fatal("no active project")
	}
	featureTransition(t, active.repository, intakefixture.Stream, "pending", created.Add(time.Minute))
	featureTransition(t, active.repository, intakefixture.Stream, "delivered", created.Add(time.Hour))
	if _, err := c.Archive(context.Background(), intakefixture.Stream); err != nil {
		t.Fatalf("archive: %v", err)
	}
	var archivedAt time.Time
	state, _ := s.store.Snapshot()
	for _, a := range state.Archived {
		if a.Workstream == intakefixture.Stream {
			archivedAt = a.ArchivedAt
		}
	}
	intakefixture.Must(t, s.Close())

	spans := exporter.GetSpans()
	states := named(spans, "osmia.workstream.state")
	roots := named(spans, "osmia.workstream")
	if len(states) != 3 || len(roots) != 1 {
		t.Fatalf("%d state spans and %d root spans: %v", len(states), len(roots), spans)
	}
	for i, w := range []struct {
		state, next string
		start, end  time.Time
	}{
		{"created", "pending", created, created.Add(time.Minute)},
		{"pending", "delivered", created.Add(time.Minute), created.Add(time.Hour)},
		{"delivered", "archived", created.Add(time.Hour), archivedAt},
	} {
		s := states[i]
		if attr(s, "osmia.state") != w.state || attr(s, "osmia.next_state") != w.next || !s.StartTime.Equal(w.start) || !s.EndTime.Equal(w.end) {
			t.Errorf("state span %d: %+v", i, s)
		}
	}
	root := roots[0]
	if root.Parent.IsValid() || root.SpanContext.SpanID() != telemetry.WorkstreamRoot(intakefixture.Stream) || !root.StartTime.Equal(created) || !root.EndTime.Equal(archivedAt) {
		t.Fatalf("root span %+v, archived at %v", root, archivedAt)
	}
	for _, s := range spans {
		if factoryTrace := telemetry.FactoryTrace("service"); s.SpanContext.TraceID() == factoryTrace {
			continue
		}
		if s.SpanContext.TraceID() != telemetry.WorkstreamTrace(intakefixture.Stream) {
			t.Errorf("span %s is not in the workstream's trace", s.Name)
		}
	}
}

func named(spans tracetest.SpanStubs, name string) tracetest.SpanStubs {
	var out tracetest.SpanStubs
	for _, s := range spans {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

func attr(s tracetest.SpanStub, key string) string {
	for _, kv := range s.Attributes {
		if string(kv.Key) == key {
			return kv.Value.String()
		}
	}
	return ""
}
