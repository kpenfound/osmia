package service

import (
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/recordspans"
	"github.com/kpenfound/osmia/internal/redact"
	"github.com/kpenfound/osmia/internal/telemetry"
	"github.com/kpenfound/osmia/internal/trace"
)

// startTelemetry builds the span sink cfg describes. Telemetry never stops
// the service: an exporter that cannot be built is logged without any header
// value and the service runs with telemetry off.
func (s *Service) startTelemetry(cfg config.Telemetry) *telemetry.Sink {
	sink, err := telemetry.New(cfg, telemetry.Options{Logger: s.log(), Getenv: s.options.getenv, Version: s.options.Build.Version, Exporter: s.options.telemetryExporter})
	if err != nil {
		s.log().Error("telemetry is off: the exporter could not be built", "component", "telemetry", "error", redact.Secrets(err.Error(), redact.KnownSecrets()...))
		return &telemetry.Sink{}
	}
	if sink.Enabled() {
		s.log().Info("telemetry on", "component", "telemetry", "endpoint", cfg.Endpoint, "protocol", cfg.Protocol)
	}
	return sink
}

// recorder sends the spans the trace records: a workstream's states, units,
// turns and check runs as its trace commits them, and its root span when it
// leaves the factory.
func (s *Service) recorder() recordspans.Recorder {
	return recordspans.Recorder{Sink: s.telemetry}
}

// workstreamEnded sends the closing spans of an archived workstream, which
// ended at the recorded archive time.
func (s *Service) workstreamEnded(repository *trace.Repository, stream config.WorkstreamID, end time.Time) {
	s.recorder().Ended(repository, stream, end, recordspans.Archived)
}
