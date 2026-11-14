package service

import (
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/redact"
	"github.com/kpenfound/osmia/internal/telemetry"
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
