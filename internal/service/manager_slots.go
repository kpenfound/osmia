package service

import (
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

const managerSlotWaitReason = "waiting for a manager slot"

const managerRole = trace.ManagerDisplay

// ManagerSlotTurn identifies a background manager turn waiting for or
// holding capacity.manager. At is when its request was enqueued.
type ManagerSlotTurn struct {
	Project    config.ProjectID
	Workstream config.WorkstreamID
	Agent      string
	Turn       string
	At         time.Time
}

// ManagerSlotStatus is an in-memory snapshot of factory-wide manager admission.
// Used counts the slot holders in Active. Unslotted lists the background turns
// running without a slot because the owner messaged their thread. Waiting is in
// admission order, oldest first.
type ManagerSlotStatus struct {
	Limit     int
	Used      int
	Active    []ManagerSlotTurn
	Unslotted []ManagerSlotTurn
	Waiting   []ManagerSlotTurn
}

// ManagerSlots reads manager capacity without opening any project's trace.
func (s *Service) ManagerSlots() ManagerSlotStatus {
	return s.manager.status()
}

type managerKey struct {
	project    config.ProjectID
	workstream config.WorkstreamID
	agent      string
	turn       string
}

func (t ManagerSlotTurn) key() managerKey {
	return managerKey{t.Project, t.Workstream, t.Agent, t.Turn}
}

func (t ManagerSlotTurn) before(o ManagerSlotTurn) bool {
	if !t.At.Equal(o.At) {
		return t.At.Before(o.At)
	}
	return strings.Compare(t.sortKey(), o.sortKey()) < 0
}

func (t ManagerSlotTurn) sortKey() string {
	return string(t.Project) + "\x00" + string(t.Workstream) + "\x00" + t.Agent + "\x00" + t.Turn
}

type managerSlotPool struct {
	mu        sync.Mutex
	limit     int
	held      map[*conversationPath]map[managerKey]ManagerSlotTurn
	unslotted map[managerKey]ManagerSlotTurn
	waiting   map[managerKey]*managerWait
	changed   func()
	wake      func()
}

type managerWait struct {
	turn   ManagerSlotTurn
	gate   *managerGate
	epoch  uint64
	exempt bool
}

type managerGate struct {
	pool    *managerSlotPool
	path    *conversationPath
	project config.ProjectID
	epoch   uint64
}

func newManagerSlotPool(limit int, changed, wake func()) *managerSlotPool {
	return &managerSlotPool{limit: limit, changed: changed, wake: wake}
}

func (p *managerSlotPool) gate(path *conversationPath, project config.ProjectID) *managerGate {
	if p == nil {
		return nil
	}
	return &managerGate{pool: p, path: path, project: project}
}

func (p *managerSlotPool) usedLocked() int {
	n := 0
	for _, held := range p.held {
		n += len(held)
	}
	return n
}

func (p *managerSlotPool) publish() {
	if p.changed != nil {
		p.changed()
	}
}

func (p *managerSlotPool) rescan() {
	if p.wake != nil {
		p.wake()
	}
}

func (p *managerSlotPool) notify() {
	p.publish()
	p.rescan()
}

func (p *managerSlotPool) resize(limit int) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.limit = limit
	p.mu.Unlock()
	p.notify()
}

func (p *managerSlotPool) status() ManagerSlotStatus {
	if p == nil {
		return ManagerSlotStatus{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := ManagerSlotStatus{Limit: p.limit, Active: []ManagerSlotTurn{}, Unslotted: []ManagerSlotTurn{}, Waiting: []ManagerSlotTurn{}}
	for _, held := range p.held {
		for _, t := range held {
			out.Active = append(out.Active, t)
		}
	}
	for _, t := range p.unslotted {
		out.Unslotted = append(out.Unslotted, t)
	}
	for _, w := range p.waiting {
		if !w.exempt {
			out.Waiting = append(out.Waiting, w.turn)
		}
	}
	out.Used = len(out.Active)
	order := func(a, b ManagerSlotTurn) int {
		if a.before(b) {
			return -1
		}
		if b.before(a) {
			return 1
		}
		return 0
	}
	slices.SortFunc(out.Active, order)
	slices.SortFunc(out.Unslotted, order)
	slices.SortFunc(out.Waiting, order)
	return out
}

func (p *managerSlotPool) waitingFor(stream config.WorkstreamID, agent, turn string) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, w := range p.waiting {
		if k.workstream == stream && k.agent == agent && k.turn == turn && !w.exempt {
			return true
		}
	}
	return false
}

func (p *managerSlotPool) drop(path *conversationPath) {
	if p == nil {
		return
	}
	p.mu.Lock()
	removed := false
	for k, w := range p.waiting {
		if w.gate.path == path {
			delete(p.waiting, k)
			removed = true
		}
	}
	p.mu.Unlock()
	if removed {
		p.notify()
	}
}

func (g *managerGate) begin() {
	if g == nil {
		return
	}
	g.pool.mu.Lock()
	g.epoch++
	g.pool.mu.Unlock()
}

func (g *managerGate) end() {
	if g == nil {
		return
	}
	p := g.pool
	p.mu.Lock()
	removed := false
	for k, w := range p.waiting {
		if w.gate == g && w.epoch < g.epoch {
			delete(p.waiting, k)
			removed = true
		}
	}
	p.mu.Unlock()
	if removed {
		p.notify()
	}
}

func (g *managerGate) hold(stream config.WorkstreamID, th trace.Thread, q trace.QueuedTurn) bool {
	if g == nil || managerOwnerStarted(th, q) {
		return false
	}
	turn := ManagerSlotTurn{Project: g.project, Workstream: stream, Agent: th.Identity.ID, Turn: q.Request.TurnID, At: q.Request.At}
	exempt := managerOwnerBehind(th, q)
	p := g.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.waiting == nil {
		p.waiting = map[managerKey]*managerWait{}
	}
	p.waiting[turn.key()] = &managerWait{turn: turn, gate: g, epoch: g.epoch, exempt: exempt}
	return false
}

func (g *managerGate) limit(op coreadapter.Operation) (func(), bool) {
	if g == nil {
		return nil, true
	}
	in, err := thread.DecodeTurn(op)
	if err != nil {
		return nil, true
	}
	return g.pool.admit(g, managerKey{g.project, in.Workstream, in.Agent, in.Turn})
}

func (p *managerSlotPool) admit(g *managerGate, key managerKey) (func(), bool) {
	p.mu.Lock()
	w := p.waiting[key]
	if w == nil || w.gate != g {
		p.mu.Unlock()
		return nil, true
	}
	if w.exempt {
		delete(p.waiting, key)
		if p.unslotted == nil {
			p.unslotted = map[managerKey]ManagerSlotTurn{}
		}
		p.unslotted[key] = w.turn
		p.mu.Unlock()
		p.publish()
		return p.releaser(func() { delete(p.unslotted, key) }), true
	}
	older := 0
	for _, o := range p.waiting {
		if o != w && !o.exempt && o.turn.before(w.turn) {
			older++
		}
	}
	if p.limit-p.usedLocked()-older <= 0 {
		p.mu.Unlock()
		return nil, false
	}
	delete(p.waiting, key)
	if p.held == nil {
		p.held = map[*conversationPath]map[managerKey]ManagerSlotTurn{}
	}
	if p.held[g.path] == nil {
		p.held[g.path] = map[managerKey]ManagerSlotTurn{}
	}
	p.held[g.path][key] = w.turn
	more := p.limit-p.usedLocked() > 0 && len(p.waiting) > 0
	p.mu.Unlock()
	p.publish()
	if more {
		p.rescan()
	}
	return p.releaser(func() {
		delete(p.held[g.path], key)
		if len(p.held[g.path]) == 0 {
			delete(p.held, g.path)
		}
	}), true
}

func (p *managerSlotPool) releaser(free func()) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			free()
			p.mu.Unlock()
			p.notify()
		})
	}
}

func managerOwnerStarted(th trace.Thread, q trace.QueuedTurn) bool {
	for range len(th.Turns) + 1 {
		if q.Request.Actor.Kind == ownerActor.Kind {
			return true
		}
		if q.Request.Actor != recoveryActor {
			return false
		}
		i := slices.IndexFunc(th.Turns, func(c trace.QueuedTurn) bool {
			return c.Response != nil && c.Response.ID == q.Request.Cause
		})
		if i < 0 {
			return false
		}
		q = th.Turns[i]
	}
	return false
}

func managerOwnerBehind(th trace.Thread, q trace.QueuedTurn) bool {
	i := slices.IndexFunc(th.Turns, func(c trace.QueuedTurn) bool { return c.Request.TurnID == q.Request.TurnID })
	if i < 0 {
		return false
	}
	return slices.ContainsFunc(th.Turns[i+1:], func(c trace.QueuedTurn) bool {
		return c.CompletedAt.IsZero() && managerOwnerStarted(th, c)
	})
}

func (s *Service) queuedReason(th trace.Thread, q trace.QueuedTurn) string {
	reason := queuedReason(th, q)
	if reason == "starting" && trace.ManagerLike(th.Identity.Role) && s.manager.waitingFor(q.Request.Workstream, th.Identity.ID, q.Request.TurnID) {
		return managerSlotWaitReason
	}
	return reason
}
