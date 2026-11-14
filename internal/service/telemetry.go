package service

import (
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/recordspans"
	"github.com/kpenfound/osmia/internal/redact"
	"github.com/kpenfound/osmia/internal/slotspans"
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

// startSlotSpans builds the recorder of the slot spans of the capacity pools,
// with the ledger of intervals still open at the last stop. It is nil while
// telemetry is off, and then nothing is tracked or written.
func (s *Service) startSlotSpans(cfg *config.Config) *slotspans.Recorder {
	s.capacity.Store(&cfg.Capacity)
	ledger, err := cfg.Root.SlotIntervals()
	if err != nil {
		s.log().Warn("slot intervals are not kept across a restart", "component", "telemetry", "error", err.Error())
	}
	return slotspans.New(s.telemetry, slotspans.Options{Ledger: ledger, Logger: s.log(), Now: s.now})
}

// poolLimit is the limit of the pool that grants role's slots, as configured
// now: masons, reviewers and committee members have one; every other role has
// none.
func (s *Service) poolLimit(role string) (int, bool) {
	limits := s.capacity.Load()
	if limits == nil {
		return 0, false
	}
	switch role {
	case masonRole:
		return limits.Masons, true
	case reviewerRole:
		return limits.Reviewers, true
	case committeeRole:
		return limits.Committee, true
	}
	return 0, false
}
