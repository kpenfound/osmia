package fixturekit

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/coreadapter/adaptertest"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// TwoProjectScenario shares repository bindings, fake turns and the fixture clock.
type TwoProjectScenario struct {
	TwoProjectData
	Turns *BlockingTurns
	mu    sync.Mutex
	live  map[config.ProjectID]*trace.Repository
}

func NewTwoProjectScenario(t *testing.T) *TwoProjectScenario {
	t.Helper()
	f := &TwoProjectScenario{TwoProjectData: TwoProjects(t), live: map[config.ProjectID]*trace.Repository{}}
	reply := adaptertest.Reply[coreadapter.SessionResult]{Value: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session"}, FinalResponse: "Done"}}
	f.Turns = &BlockingTurns{Fake: &adaptertest.Turns{Script: *adaptertest.NewScript[coreadapter.PreparedTurn](reply, reply, reply, reply, reply, reply)}, Hooks: map[string]func(context.Context){}}
	return f
}

func (f *TwoProjectScenario) Threads(r *trace.Repository, cfg *config.Config) (coreadapter.Reconciler, error) {
	if cfg.Project.ID != r.Project() {
		return nil, fmt.Errorf("project %s bound with the configuration of %q", r.Project(), cfg.Project.ID)
	}
	f.mu.Lock()
	f.live[r.Project()] = r
	f.mu.Unlock()
	return thread.Dispatcher{Runner: thread.Runner{Store: r, Turns: f.Turns, Now: f.Clock.Now}, Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
		return coreadapter.PreparedTurn{SessionDirectory: filepath.Join(f.Config.Root, "sessions", string(r.Project()), in.Agent, in.Turn)}, nil
	}}, nil
}

func (f *TwoProjectScenario) Repository(id config.ProjectID) *trace.Repository {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live[id]
}
