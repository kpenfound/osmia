package service

import (
	"time"

	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

func (c *runtimeControls) cloudTrace(cfg *config.Config, repo *trace.Repository, scope coreadapter.Scope, now func() time.Time) coreadapter.Tool {
	return checks.TraceTool(checks.TraceFetcher{Repository: repo, Runner: traceRunner(c.service.Load(), cfg), Now: now}, scope, managerActor, trace.Manager)
}
