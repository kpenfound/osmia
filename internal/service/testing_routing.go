package service

import (
	"context"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/hearsay"
	"github.com/kpenfound/osmia/internal/trace"
)

// ArmStallWatchdog lets the next pass of the scheduler read the project's
// records for stalls, as it does once the service has been running for longer
// than the watchdog's reading interval. A fixture that moves its clock by hours
// does not wait that long on the wall clock.
func (a TestAccess) ArmStallWatchdog(project config.ProjectID) {
	a.s.stalls.mu.Lock()
	defer a.s.stalls.mu.Unlock()
	if a.s.stalls.ran == nil {
		a.s.stalls.ran = map[config.ProjectID]time.Time{}
	}
	a.s.stalls.ran[project] = time.Now().Add(-time.Hour)
}

// RoutingTokenEnv names the variables PublishToHearsay reads the credentials
// of its fake publisher from; a test sets the publisher's.
const (
	RoutingOwnerTokenEnv   = "ROUTING_JOURNEY_OWNER_TOKEN"
	RoutingPublishTokenEnv = "ROUTING_JOURNEY_PUBLISH_TOKEN"
)

// PublishToHearsay runs one Hearsay publish pass of the project over its
// committed trace against a connectors listener at hooksURL, as the service's
// publisher does on its own cadence. The service is not configured for Hearsay
// otherwise, so nothing else reaches out.
func (a TestAccess) PublishToHearsay(ctx context.Context, repo *trace.Repository, hooksURL string) (hearsay.Result, error) {
	cfg := *a.s.current()
	cfg.Hearsay = config.Hearsay{URL: "https://memory.example", Principal: "owner", TokenEnv: RoutingOwnerTokenEnv,
		Publish: &config.HearsayPublish{WorkSource: "osmia-work", OwnerSource: "osmia-owner", HooksURL: hooksURL, Agent: "osmia-publisher", TokenEnv: RoutingPublishTokenEnv}}
	return a.s.publishPass(ctx, repo, &cfg)
}
