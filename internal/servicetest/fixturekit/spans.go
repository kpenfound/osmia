package fixturekit

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// Spans is a set of exported spans, read from an in-memory exporter that a
// service sent them to.
type Spans tracetest.SpanStubs

// SpansOf returns the spans the exporter holds so far.
func SpansOf(e *tracetest.InMemoryExporter) Spans { return Spans(e.GetSpans()) }

// Named returns the spans with the given name.
func (s Spans) Named(name string) Spans {
	return s.Where(func(sp tracetest.SpanStub) bool { return sp.Name == name })
}

// Where returns the spans for which keep is true.
func (s Spans) Where(keep func(tracetest.SpanStub) bool) Spans {
	var out Spans
	for _, sp := range s {
		if keep(sp) {
			out = append(out, sp)
		}
	}
	return out
}

// With returns the spans whose attribute key has the given value.
func (s Spans) With(key, value string) Spans {
	return s.Where(func(sp tracetest.SpanStub) bool { v, ok := Attr(sp, key); return ok && v == value })
}

// SortedByStart returns the spans in the order they started, ties broken by
// end.
func (s Spans) SortedByStart() Spans {
	out := slices.Clone(s)
	slices.SortStableFunc(out, func(a, b tracetest.SpanStub) int {
		if c := a.StartTime.Compare(b.StartTime); c != 0 {
			return c
		}
		return a.EndTime.Compare(b.EndTime)
	})
	return out
}

// Attr returns the span's attribute key as text and whether it has one.
func Attr(sp tracetest.SpanStub, key string) (string, bool) {
	for _, kv := range sp.Attributes {
		if string(kv.Key) == key {
			return kv.Value.String(), true
		}
	}
	return "", false
}

// Strings returns every string a span carries to a collector: its name,
// attribute keys and values, events, status, resource and instrumentation
// scope.
func Strings(sp tracetest.SpanStub) []string {
	out := []string{sp.Name, sp.Status.Code.String(), sp.Status.Description, sp.InstrumentationScope.Name, sp.InstrumentationScope.Version, sp.InstrumentationScope.SchemaURL}
	for _, kv := range sp.Attributes {
		out = append(out, string(kv.Key), kv.Value.String())
	}
	for _, e := range sp.Events {
		out = append(out, e.Name)
		for _, kv := range e.Attributes {
			out = append(out, string(kv.Key), kv.Value.String())
		}
	}
	for _, l := range sp.Links {
		for _, kv := range l.Attributes {
			out = append(out, string(kv.Key), kv.Value.String())
		}
	}
	if sp.Resource != nil {
		for _, kv := range sp.Resource.Attributes() {
			out = append(out, string(kv.Key), kv.Value.String())
		}
		out = append(out, sp.Resource.SchemaURL())
	}
	return out
}

// Leaks describes every place a marker appears in the spans: the span, the
// string that holds it and the marker. An empty result means no marker was
// sent.
func (s Spans) Leaks(markers ...string) []string {
	var out []string
	for _, sp := range s {
		for _, str := range Strings(sp) {
			for _, m := range markers {
				if m != "" && strings.Contains(str, m) {
					out = append(out, fmt.Sprintf("span %q carries %q in %q", sp.Name, m, str))
				}
			}
		}
	}
	return out
}

// Summary lists the spans one per line for a failure message: name, start,
// end and attributes.
func (s Spans) Summary() string {
	var b strings.Builder
	for _, sp := range s.SortedByStart() {
		fmt.Fprintf(&b, "%s [%s .. %s]", sp.Name, sp.StartTime.Format("15:04:05.000000000"), sp.EndTime.Format("15:04:05.000000000"))
		for _, kv := range sp.Attributes {
			fmt.Fprintf(&b, " %s=%s", kv.Key, kv.Value.String())
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// ErrCollectorRefused is what RefusingExporter returns.
var ErrCollectorRefused = errors.New("collector refused the spans")

// RefusingExporter is a span exporter whose collector refuses every export.
type RefusingExporter struct{ calls atomic.Int64 }

// Calls is how many exports were attempted.
func (e *RefusingExporter) Calls() int64 { return e.calls.Load() }

func (e *RefusingExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	e.calls.Add(1)
	return ErrCollectorRefused
}

func (e *RefusingExporter) Shutdown(context.Context) error { return nil }

// HangingExporter is a span exporter whose collector accepts a connection and
// never answers: an export blocks until Release, whatever its context says.
type HangingExporter struct {
	once    sync.Once
	release chan struct{}
	calls   atomic.Int64
}

// NewHangingExporter returns an exporter whose exports block.
func NewHangingExporter() *HangingExporter { return &HangingExporter{release: make(chan struct{})} }

// Calls is how many exports were attempted.
func (e *HangingExporter) Calls() int64 { return e.calls.Load() }

// Release lets the blocked exports return.
func (e *HangingExporter) Release() { e.once.Do(func() { close(e.release) }) }

func (e *HangingExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	e.calls.Add(1)
	<-e.release
	return ErrCollectorRefused
}

func (e *HangingExporter) Shutdown(context.Context) error { return nil }
