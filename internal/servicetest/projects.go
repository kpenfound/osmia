package servicetest

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/coreadapter/adaptertest"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

type TwoProjectFixture struct {
	Options service.Options
	Config  *config.Config
	Clock   *fixturekit.DemoClock
	Streams map[config.ProjectID]config.WorkstreamID
	Turns   *fixturekit.BlockingTurns
	Mu      sync.Mutex
	Live    map[config.ProjectID]*trace.Repository
}

func NewTwoProjectFixture(t *testing.T) *TwoProjectFixture {
	t.Helper()
	data := fixturekit.TwoProjects(t)
	opts := Options(data.Config)
	opts.Workstreams = nil
	root := opts.Config.Root
	f := &TwoProjectFixture{Options: opts, Config: data.Loaded, Clock: data.Clock, Streams: data.Streams, Live: map[config.ProjectID]*trace.Repository{}}
	reply := adaptertest.Reply[coreadapter.SessionResult]{Value: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session"}, FinalResponse: "Done"}}
	f.Turns = &fixturekit.BlockingTurns{Fake: &adaptertest.Turns{Script: *adaptertest.NewScript[coreadapter.PreparedTurn](reply, reply, reply, reply, reply, reply)}, Hooks: map[string]func(context.Context){}}
	f.Options.Threads = func(r *trace.Repository, cfg *config.Config) (coreadapter.Reconciler, error) {
		if cfg.Project.ID != r.Project() {
			return nil, fmt.Errorf("project %s bound with the configuration of %q", r.Project(), cfg.Project.ID)
		}
		f.Mu.Lock()
		f.Live[r.Project()] = r
		f.Mu.Unlock()
		return thread.Dispatcher{Runner: thread.Runner{Store: r, Turns: f.Turns, Now: f.Clock.Now},
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				return coreadapter.PreparedTurn{SessionDirectory: filepath.Join(root, "sessions", string(r.Project()), in.Agent, in.Turn)}, nil
			}}, nil
	}
	f.Options.Reconciliation.Now = f.Clock.Now
	return f
}
