package service

import (
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// turnRetries is how many times a thread turn retries an infrastructure
// failure on one profile before it falls back to that profile's fallback.
const turnRetries = 2

// threadRunner returns the runner of thread turns: an infrastructure failure
// is retried turnRetries times on its profile, then on each profile of the
// fallback chain configured in cfg with the same bound.
func threadRunner(cfg *config.Config, store *trace.Repository, turns coreadapter.Turns, now func() time.Time) thread.Runner {
	fallbacks := map[string]coreadapter.Profile{}
	for name, p := range cfg.Profiles {
		if p.Fallback == "" {
			continue
		}
		if fallback, err := cfg.NamedProfile(p.Fallback); err == nil {
			fallbacks[name] = fallback
		}
	}
	return thread.Runner{Store: store, Turns: turns, Now: now, Fallbacks: fallbacks, MaxRetries: turnRetries}
}
