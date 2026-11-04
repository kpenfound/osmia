package service

import (
	"context"
	"slices"
	"sync"

	"github.com/kpenfound/osmia/internal/config"
)

// CheckSlotRun identifies a service check run waiting for or holding capacity.
// Exactly one of Unit, Review, Session or Preview identifies the kind of run.
type CheckSlotRun struct {
	Project    config.ProjectID
	Workstream config.WorkstreamID
	Unit       string
	Review     int
	Session    bool
	Preview    bool
}

// CheckSlotStatus is an in-memory snapshot of factory-wide check admission.
type CheckSlotStatus struct {
	Limit   int
	Used    int
	Waiting []CheckSlotRun
	Active  []CheckSlotRun
}

// CheckSlots reads check capacity without opening any project's trace.
func (s *Service) CheckSlots() CheckSlotStatus {
	return s.checkSlots.status()
}

// checkSlotPool admits exported service check runs in arrival order. Its
// lifetime is the service's, rather than a project's or a loaded config's.
type checkSlotPool struct {
	mu      sync.Mutex
	limit   int
	used    int
	waiting []*checkSlotWait
	changed func()
	held    map[*checkSlotWait]struct{}
	// maintenance excludes admission and Dagger turn dispatch, without holding
	// mu during engine commands or blocking capacity snapshots.
	maintenance chan struct{}
	turns       map[*EngineTurn]struct{}
}

type checkSlotWait struct {
	run     CheckSlotRun
	ready   chan struct{}
	granted bool
}

func (p *checkSlotPool) status() CheckSlotStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := CheckSlotStatus{Limit: p.limit, Used: p.used, Waiting: make([]CheckSlotRun, 0, len(p.waiting))}
	for _, w := range p.waiting {
		out.Waiting = append(out.Waiting, w.run)
	}
	out.Active = p.activeChecks()
	return out
}

func (p *checkSlotPool) notify() {
	if p.changed != nil {
		p.changed()
	}
}

// admit must be called with mu held. Lowering the limit never interrupts
// executing runs; they release capacity before any further admission.
func (p *checkSlotPool) admit() {
	if p.maintenance != nil {
		return
	}
	for p.used < p.limit && len(p.waiting) > 0 {
		w := p.waiting[0]
		p.waiting = slices.Delete(p.waiting, 0, 1)
		w.granted = true
		if p.held == nil {
			p.held = make(map[*checkSlotWait]struct{})
		}
		p.held[w] = struct{}{}
		p.used++
		close(w.ready)
	}
}

func (p *checkSlotPool) resize(limit int) {
	p.mu.Lock()
	p.limit = limit
	p.admit()
	p.mu.Unlock()
	p.notify()
}

func (p *checkSlotPool) acquire(ctx context.Context, run CheckSlotRun) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	w := &checkSlotWait{run: run, ready: make(chan struct{})}
	p.mu.Lock()
	p.waiting = append(p.waiting, w)
	p.admit()
	p.mu.Unlock()
	p.notify()
	var once sync.Once
	release := func() {
		once.Do(func() {
			p.mu.Lock()
			if w.granted {
				p.used--
				delete(p.held, w)
			} else {
				i := slices.Index(p.waiting, w)
				p.waiting = slices.Delete(p.waiting, i, i+1)
			}
			p.admit()
			p.mu.Unlock()
			p.notify()
		})
	}
	select {
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	case <-w.ready:
		if err := ctx.Err(); err != nil {
			release()
			return nil, err
		}
		return release, nil
	}
}
