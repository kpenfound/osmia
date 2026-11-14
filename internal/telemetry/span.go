package telemetry

import (
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// scopeName is the instrumentation scope of every span the sink sends.
const scopeName = "github.com/kpenfound/osmia"

// Span is one finished span, described by recorded identities and times. It
// is sent when it is emitted; nothing about it depends on the time of
// emission.
type Span struct {
	Name  Name
	Trace oteltrace.TraceID
	ID    oteltrace.SpanID
	// Parent is the zero ID for a root span.
	Parent     oteltrace.SpanID
	Start, End time.Time
	Attrs      []Attr
	// Failed marks the span's status as an error, without a description.
	Failed bool
	Events []Event
}

// Event is a point in time within a span.
type Event struct {
	Name  Name
	At    time.Time
	Attrs []Attr
}

// attributes returns the allow-listed attributes of attrs and how many were
// dropped.
func attributes(attrs []Attr) (kept []attribute.KeyValue, dropped int) {
	seen := map[attribute.Key]bool{}
	for _, a := range attrs {
		if !a.Valid() || seen[a.kv.Key] {
			dropped++
			continue
		}
		seen[a.kv.Key] = true
		kept = append(kept, a.kv)
	}
	return kept, dropped
}

// snapshot converts s to an exportable span, or reports it unusable: it needs
// a name built by a naming helper, IDs, and an end that is not before its start.
func (s Span) snapshot(res *resource.Resource) (sdktrace.ReadOnlySpan, int, bool) {
	if !s.Name.Valid() || !s.Trace.IsValid() || !s.ID.IsValid() || s.Start.IsZero() || s.End.Before(s.Start) {
		return nil, 0, false
	}
	attrs, dropped := attributes(s.Attrs)
	stub := tracetest.SpanStub{
		Name:                 s.Name.String(),
		SpanContext:          oteltrace.NewSpanContext(oteltrace.SpanContextConfig{TraceID: s.Trace, SpanID: s.ID, TraceFlags: oteltrace.FlagsSampled}),
		SpanKind:             oteltrace.SpanKindInternal,
		StartTime:            s.Start,
		EndTime:              s.End,
		Attributes:           attrs,
		Resource:             res,
		InstrumentationScope: instrumentation.Scope{Name: scopeName},
	}
	if s.Parent.IsValid() {
		stub.Parent = oteltrace.NewSpanContext(oteltrace.SpanContextConfig{TraceID: s.Trace, SpanID: s.Parent, TraceFlags: oteltrace.FlagsSampled})
	}
	if s.Failed {
		stub.Status = sdktrace.Status{Code: codes.Error}
	}
	for _, e := range s.Events {
		if !e.Name.Valid() || e.At.IsZero() {
			dropped++
			continue
		}
		kept, n := attributes(e.Attrs)
		dropped += n
		stub.Events = append(stub.Events, sdktrace.Event{Name: e.Name.String(), Time: e.At, Attributes: kept})
	}
	return stub.Snapshot(), dropped, true
}
