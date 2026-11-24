package service

import (
	"context"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/slotspans"
	"github.com/kpenfound/osmia/internal/telemetry"
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

type checkSlotPool struct {
	mu          sync.Mutex
	limit       int
	used        int
	waiting     []*checkSlotWait
	changed     func()
	held        map[*checkSlotWait]struct{}
	maintenance chan struct{}
	turns       map[*EngineTurn]struct{}
	spans       *slotspans.Recorder
	now         func() time.Time
}

type checkSlotWait struct {
	run     CheckSlotRun
	ready   chan struct{}
	granted bool
	work    string
}

func (p *checkSlotPool) interval(kind slotspans.Kind, w *checkSlotWait, at time.Time) slotspans.Interval {
	iv := slotspans.Interval{Kind: kind, Source: slotspans.SourceCheck, Pool: telemetry.PoolID(checksRole), Role: checksRole, Project: w.run.Project, Workstream: w.run.Workstream, Unit: w.run.Unit, Work: w.work, Start: at}
	iv.Bound(p.limit, true)
	if kind == slotspans.Wait {
		iv.Reason = slotspans.ReasonCapacity
	}
	return iv
}

func (p *checkSlotPool) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now().UTC()
}

func (p *checkSlotPool) status() CheckSlotStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := CheckSlotStatus{Limit: p.limit, Used: p.used, Waiting: make([]CheckSlotRun, 0, len(p.waiting))}
	for _, w := range p.waiting {
		out.Waiting = append(out.Waiting, w.run)
	}
	out.Active = p.activeChecksLocked()
	return out
}

func (p *checkSlotPool) notify() {
	if p.changed != nil {
		p.changed()
	}
}

func (p *checkSlotPool) admitLocked() {
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
		if p.spans != nil {
			now := p.clock()
			p.spans.End(p.interval(slotspans.Wait, w, now), now, slotspans.Granted)
			p.spans.Begin(p.interval(slotspans.Held, w, now))
		}
		close(w.ready)
	}
}

func (p *checkSlotPool) resize(limit int) {
	p.mu.Lock()
	p.limit = limit
	p.admitLocked()
	p.mu.Unlock()
	p.notify()
}

func (p *checkSlotPool) acquire(ctx context.Context, run CheckSlotRun) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	w := &checkSlotWait{run: run, ready: make(chan struct{})}
	if p.spans != nil {
		w.work = strconv.FormatUint(p.spans.Sequence(), 10)
	}
	p.mu.Lock()
	p.waiting = append(p.waiting, w)
	p.admitLocked()
	if p.spans != nil && !w.granted {
		p.spans.Begin(p.interval(slotspans.Wait, w, p.clock()))
	}
	p.mu.Unlock()
	p.notify()
	var once sync.Once
	release := func() {
		once.Do(func() {
			p.mu.Lock()
			if w.granted {
				p.used--
				delete(p.held, w)
				p.spans.End(p.interval(slotspans.Held, w, time.Time{}), p.clock(), slotspans.Released)
			} else {
				i := slices.Index(p.waiting, w)
				p.waiting = slices.Delete(p.waiting, i, i+1)
				p.spans.End(p.interval(slotspans.Wait, w, time.Time{}), p.clock(), slotspans.Canceled)
			}
			p.admitLocked()
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
