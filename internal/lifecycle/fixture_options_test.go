package lifecycle_test

import (
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

func testOptions(cfg config.Options) Options {
	return Options{Options: service.Options{Config: cfg, Build: api.Identity{Version: "test", Commit: "abc"}, Workstreams: []config.WorkstreamID{fixturekit.Stream}, ShutdownTimeout: 100 * time.Millisecond}}
}
