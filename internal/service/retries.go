package service

import (
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/runtime"
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

func (s *Service) threadRunner(cfg *config.Config, store *trace.Repository, turns coreadapter.Turns, now func() time.Time) thread.Runner {
	r := threadRunner(cfg, store, turns, now)
	r.OnProviderLimit = s.recordProviderLimit
	r.AdmitRole = s.admitRole
	return r
}

func (s *Service) admitRole(role string) bool {
	st, _ := s.effective()
	for _, p := range st.Pauses {
		if p.Target.Scope == "role" && p.Target.Role == role {
			return false
		}
	}
	return true
}

func (s *Service) recordProviderLimit(profile coreadapter.Profile, limit coreadapter.ProviderLimit) error {
	return s.store.SetProviderLimit(runtime.ProviderLimit{Backend: profile.Backend, Status: limit.Status, Kind: limit.Kind, SetAt: s.now(), ResetsAt: limit.ResetsAt})
}

func (s *Service) effective() (runtime.State, []runtime.Diagnostic) {
	return s.store.EffectiveAt(s.now())
}
