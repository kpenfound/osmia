package lifecycle_test

import (
	"context"
	"path/filepath"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// runChief binds the fake chief of staff through the thread dispatcher. It
// exercises the Runner's resume mechanism directly, as chiefTurns always
// authorizes resume; production confines that capability to the session role
// alone (thread.Runner.ResumableRoles, set only in internal/service/enforcement.go).
func runChief(opts *Options, cfg *config.Config, turns *chiefTurns) {
	opts.Threads = func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
		return thread.Dispatcher{Runner: thread.Runner{Store: r, Turns: turns, Now: opts.Reconciliation.Now, ResumableRoles: func(string) bool { return true }},
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				return coreadapter.PreparedTurn{SessionDirectory: filepath.Join(cfg.Root.String(), "sessions", in.Agent, in.Turn)}, nil
			}}, nil
	}
}
