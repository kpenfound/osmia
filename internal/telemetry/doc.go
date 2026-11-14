// Package telemetry sends Osmia's OpenTelemetry traces. It builds an OTLP
// trace exporter from the [telemetry] configuration alone, derives the trace
// and span IDs of every span from recorded identities, and offers a
// best-effort span sink: emitting never blocks and never fails its caller,
// and a span that cannot be queued or exported is dropped.
//
// The package never reads an OTEL_* environment variable. Every exporter
// option and the resource are set explicitly, so the environment changes
// neither the destination nor the resource attributes. Spans carry only
// allow-listed attributes with identifier-shaped values; no prompt, reply,
// title, reason or check output can be attached through it.
package telemetry
