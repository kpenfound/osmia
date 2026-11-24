package servicetest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestLeaksFindsAMarkerInEveryPlaceASpanCarriesText(t *testing.T) {
	const marker = "zq-marker"
	carrying := map[string]tracetest.SpanStub{
		"name":      {Name: "span " + marker},
		"attribute": {Attributes: []attribute.KeyValue{attribute.String("osmia.role", marker)}},
		"key":       {Attributes: []attribute.KeyValue{attribute.String(marker, "x")}},
		"event":     {Events: []sdktrace.Event{{Name: marker}}},
		"event key": {Events: []sdktrace.Event{{Name: "e", Attributes: []attribute.KeyValue{attribute.String("a", marker)}}}},
		"status":    {Status: sdktrace.Status{Code: codes.Error, Description: marker}},
		"resource":  {Resource: resource.NewSchemaless(attribute.String("service.name", marker))},
		"scope":     {InstrumentationScope: instrumentation.Scope{Name: marker}},
	}
	for place, sp := range carrying {
		if leaks := (Spans{sp}).Leaks(marker); len(leaks) != 1 || !strings.Contains(leaks[0], marker) {
			t.Errorf("%s: leaks %v", place, leaks)
		}
	}
	clean := tracetest.SpanStub{Name: "osmia.unit.state", Attributes: []attribute.KeyValue{attribute.String("osmia.role", "mason")}, Resource: resource.NewSchemaless(attribute.String("service.name", "osmia"))}
	if leaks := (Spans{clean}).Leaks(marker); len(leaks) != 0 {
		t.Errorf("clean span leaks %v", leaks)
	}
}

func TestSpansSelectByNameAndAttribute(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	a := tracetest.SpanStub{Name: "a", StartTime: at.Add(time.Second), Attributes: []attribute.KeyValue{attribute.Int("n", 2)}}
	b := tracetest.SpanStub{Name: "b", StartTime: at, Attributes: []attribute.KeyValue{attribute.Int("n", 2)}}
	s := Spans{a, b}
	if got := s.Named("a"); len(got) != 1 || got[0].Name != "a" {
		t.Errorf("named: %v", got)
	}
	if got := s.With("n", "2").SortedByStart(); len(got) != 2 || got[0].Name != "b" {
		t.Errorf("with: %v", got)
	}
	if got := s.With("n", "3"); len(got) != 0 {
		t.Errorf("with a value no span has: %v", got)
	}
}

func TestFailingExporters(t *testing.T) {
	refusing := &RefusingExporter{}
	if err := refusing.ExportSpans(context.Background(), nil); err != ErrCollectorRefused || refusing.Calls() != 1 {
		t.Errorf("refusing: %v after %d calls", err, refusing.Calls())
	}
	hanging := NewHangingExporter()
	done := make(chan struct{})
	go func() { hanging.ExportSpans(context.Background(), nil); close(done) }()
	wake, releaseWake := fixturekit.Ticks(time.Millisecond)
	defer releaseWake()
	fixturekit.Until(t, "the export to start", wake, func() bool { return hanging.Calls() == 1 })
	select {
	case <-done:
		t.Fatal("a hanging export returned")
	default:
	}
	hanging.Release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a released export did not return")
	}
}

func poolSpan(name, pool string, limit int, start, end time.Time) tracetest.SpanStub {
	sp := tracetest.SpanStub{Name: name, StartTime: start, EndTime: end, Attributes: []attribute.KeyValue{attribute.String("osmia.pool.id", pool)}}
	if limit > 0 {
		sp.Attributes = append(sp.Attributes, attribute.Int("osmia.pool.limit", limit))
	} else {
		sp.Attributes = append(sp.Attributes, attribute.String("osmia.pool.unbounded", "true"))
	}
	return sp
}

func TestPoolTimesFollowLimitChangesWhileSlotsStayHeld(t *testing.T) {
	at := func(hm string) time.Time {
		v, err := time.Parse("2006-01-02 15:04", "2026-11-14 "+hm)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	const limit, held = "osmia.pool.limit", "osmia.slot.held"
	spans := Spans{
		poolSpan(limit, "mason", 2, at("10:00"), at("10:30")),
		poolSpan(limit, "mason", 4, at("10:30"), at("11:00")),
		poolSpan(held, "mason", 2, at("10:00"), at("11:00")),
		poolSpan(held, "mason", 2, at("10:00"), at("11:00")),
		poolSpan(limit, "reviewer", 1, at("10:00"), at("11:00")),
		poolSpan(limit, "checks", 2, at("10:00"), at("11:00")),
		poolSpan(held, "checks", 2, at("10:00"), at("10:20")),
		poolSpan(held, "checks", 2, at("10:10"), at("10:40")),
		poolSpan(held, "checks", 2, at("10:15"), at("10:30")),
		poolSpan(limit, "architect", 0, at("10:00"), at("10:40")),
		poolSpan(limit, "architect", 0, at("10:50"), at("11:00")),
		poolSpan(held, "architect", 0, at("10:05"), at("10:21")),
	}
	m := time.Minute
	want := map[string]PoolTime{
		"architect": {Running: 50 * m, Busy: 16 * m, Idle: 34 * m},
		"checks":    {Running: 60 * m, AtCapacity: 20 * m, Busy: 40 * m, Idle: 20 * m},
		"mason":     {Running: 60 * m, AtCapacity: 30 * m, Busy: 60 * m},
		"reviewer":  {Running: 60 * m, Idle: 60 * m},
	}
	got := spans.PoolTimes(at("10:00"), at("11:00"))
	if len(got) != len(want) {
		t.Errorf("pools %v", got)
	}
	for pool, w := range want {
		if got[pool] != w {
			t.Errorf("%s: got %+v, want %+v", pool, got[pool], w)
		}
	}
	if mason := spans.PoolTimes(at("10:15"), at("10:45"))["mason"]; mason != (PoolTime{Running: 30 * m, AtCapacity: 15 * m, Busy: 30 * m}) {
		t.Errorf("mason over 10:15 to 10:45: %+v", mason)
	}
}
