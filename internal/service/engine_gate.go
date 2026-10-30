package service

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/kpenfound/osmia/internal/config"
)

// EngineControl is the lifecycle boundary implemented by engine.Controller.
// Applicable must not probe or modify the engine.
type EngineControl interface {
	Applicable(action string) error
	Restart(context.Context) error
	Prune(context.Context) error
}

// EngineTurn identifies a dispatched turn that can use Dagger.
type EngineTurn struct {
	Project    config.ProjectID
	Workstream config.WorkstreamID
	Unit       string
	Agent      string
	Turn       string
}

// EngineBusyError is a temporary refusal. Only admitted checks and dispatched
// Dagger turns appear in this snapshot; queued work is not a blocker.
type EngineBusyError struct {
	Action string
	Checks []CheckSlotRun
	Turns  []EngineTurn
}

func (e *EngineBusyError) Error() string {
	var blockers []string
	for _, run := range e.Checks {
		kind := "session"
		if run.Preview {
			kind = "preview"
		} else if run.Unit != "" {
			kind = "unit " + run.Unit
		} else if run.Review != 0 {
			kind = fmt.Sprintf("review %d", run.Review)
		}
		blockers = append(blockers, fmt.Sprintf("check run project %s workstream %s %s", run.Project, run.Workstream, kind))
	}
	for _, turn := range e.Turns {
		blockers = append(blockers, fmt.Sprintf("Dagger turn project %s workstream %s unit %s agent %s turn %s", turn.Project, turn.Workstream, turn.Unit, turn.Agent, turn.Turn))
	}
	return fmt.Sprintf("engine %s temporarily unavailable: %s", e.Action, strings.Join(blockers, "; "))
}

// RestartEngine refuses busy work or excludes new engine work for the entire
// restart. The caller supplies the controller bound to its engine configuration.
func (s *Service) RestartEngine(ctx context.Context, control EngineControl) error {
	return s.checkSlots.engineAction(ctx, control, "restart", control.Restart)
}

// PruneEngine holds the same exclusion as RestartEngine and check admission.
func (s *Service) PruneEngine(ctx context.Context, control EngineControl) error {
	return s.checkSlots.engineAction(ctx, control, "prune", control.Prune)
}

// activeChecks requires mu. Snapshots never expose the held map to callers.
func (p *checkSlotPool) activeChecks() []CheckSlotRun {
	checks := make([]CheckSlotRun, 0, len(p.held))
	for w := range p.held {
		checks = append(checks, w.run)
	}
	slices.SortFunc(checks, func(a, b CheckSlotRun) int {
		return cmp.Or(cmp.Compare(a.Project, b.Project), cmp.Compare(a.Workstream, b.Workstream), cmp.Compare(a.Unit, b.Unit), cmp.Compare(a.Review, b.Review))
	})
	return checks
}

// startEngineTurn waits before the thread runner claims the turn or records an
// attempt. The lease lasts until the dispatched operation has settled.
func (p *checkSlotPool) startEngineTurn(ctx context.Context, turn EngineTurn) (func(), error) {
	for {
		p.mu.Lock()
		if err := ctx.Err(); err != nil {
			p.mu.Unlock()
			return nil, err
		}
		if done := p.maintenance; done != nil {
			p.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-done:
				continue
			}
		}
		if p.turns == nil {
			p.turns = make(map[*EngineTurn]struct{})
		}
		p.turns[&turn] = struct{}{}
		p.mu.Unlock()
		var once sync.Once
		return func() {
			once.Do(func() {
				p.mu.Lock()
				delete(p.turns, &turn)
				p.mu.Unlock()
			})
		}, nil
	}
}

func (p *checkSlotPool) engineAction(ctx context.Context, control EngineControl, action string, run func(context.Context) error) error {
	if err := control.Applicable(action); err != nil {
		return err
	}
	for {
		p.mu.Lock()
		if err := ctx.Err(); err != nil {
			p.mu.Unlock()
			return err
		}
		if done := p.maintenance; done != nil {
			p.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-done:
				continue
			}
		}
		busy := &EngineBusyError{Action: action, Checks: p.activeChecks()}
		for turn := range p.turns {
			busy.Turns = append(busy.Turns, *turn)
		}
		if len(busy.Checks) > 0 || len(busy.Turns) > 0 {
			slices.SortFunc(busy.Turns, func(a, b EngineTurn) int {
				return cmp.Or(cmp.Compare(a.Project, b.Project), cmp.Compare(a.Workstream, b.Workstream), cmp.Compare(a.Agent, b.Agent), cmp.Compare(a.Turn, b.Turn))
			})
			p.mu.Unlock()
			return busy
		}
		p.maintenance = make(chan struct{})
		p.mu.Unlock()
		defer func() {
			p.mu.Lock()
			close(p.maintenance)
			p.maintenance = nil
			p.admit()
			p.mu.Unlock()
			p.notify()
		}()
		return run(ctx)
	}
}
