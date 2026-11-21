package service

import (
	"time"

	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// cloudTrace returns the read_cloud_trace tool of one chief-of-staff turn: it
// runs the fetch with the service's Dagger Cloud token and records the output
// as a workstream document.
func (c *runtimeControls) cloudTrace(cfg *config.Config, repo *trace.Repository, scope coreadapter.Scope, now func() time.Time) coreadapter.Tool {
	return checks.TraceTool(checks.TraceFetcher{Repository: repo, Runner: traceRunner(c.service.Load(), cfg), Now: now}, scope, chiefActor, trace.ChiefOfStaff)
}
