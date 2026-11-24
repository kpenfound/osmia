package telemetry

import (
	"crypto/sha256"
	"encoding/binary"
	"strconv"

	"github.com/kpenfound/osmia/internal/config"
	oteltrace "go.opentelemetry.io/otel/trace"
)

const (
	domainTrace        = "osmia.trace.workstream"
	domainSpan         = "osmia.span.workstream"
	domainRoot         = "osmia.span.root"
	domainFactoryTrace = "osmia.trace.factory"
	domainFactorySpan  = "osmia.span.factory"
)

func derive(size int, domain string, parts ...string) []byte {
	h := sha256.New()
	write := func(s string) {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(s)))
		h.Write(n[:])
		h.Write([]byte(s))
	}
	write(domain)
	for _, p := range parts {
		write(p)
	}
	sum := h.Sum(nil)[:size]
	allZero := true
	for _, b := range sum {
		allZero = allZero && b == 0
	}
	if allZero {
		sum[size-1] = 1
	}
	return sum
}

func traceID(domain string, parts ...string) (id oteltrace.TraceID) {
	copy(id[:], derive(len(id), domain, parts...))
	return id
}

func spanID(domain string, parts ...string) (id oteltrace.SpanID) {
	copy(id[:], derive(len(id), domain, parts...))
	return id
}

// WorkstreamTrace is the trace ID of every span of workstream w: derived from
// the workstream ID alone, so it is the same in every service run.
func WorkstreamTrace(w config.WorkstreamID) oteltrace.TraceID {
	return traceID(domainTrace, string(w))
}

// WorkstreamRoot is the span ID of the root span of workstream w's trace.
func WorkstreamRoot(w config.WorkstreamID) oteltrace.SpanID {
	return spanID(domainRoot, string(w))
}

// WorkstreamSpan is the span ID of the span in workstream w's trace named by
// parts, such as a kind and a record ID. The same workstream and parts always
// give the same ID, and different parts give different IDs.
func WorkstreamSpan(w config.WorkstreamID, parts ...string) oteltrace.SpanID {
	return spanID(domainSpan, append([]string{string(w)}, parts...)...)
}

// RecordSpan is the span ID of the span for revision of the record of workstream
// w with the given ID. A record that is rewritten has a new revision and so a
// new span, while the span of a given revision is always the same.
func RecordSpan(w config.WorkstreamID, record string, revision int) oteltrace.SpanID {
	return WorkstreamSpan(w, "record", record, strconv.Itoa(revision))
}

// FactoryTrace is the trace ID of the factory-level spans of one run, such as
// a service run: spans that belong to no workstream. run names the run and is
// kept in the service's own record so a restart does not change it.
func FactoryTrace(run string) oteltrace.TraceID {
	return traceID(domainFactoryTrace, run)
}

// FactorySpan is the span ID of the factory-level span of run named by parts,
// such as a pool identity and the start of one interval of its limit.
func FactorySpan(run string, parts ...string) oteltrace.SpanID {
	return spanID(domainFactorySpan, append([]string{run}, parts...)...)
}
