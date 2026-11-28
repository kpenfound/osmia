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

const chiefSlotWaitReason = "waiting for a chief slot"

const chiefRole = "chief"

// ChiefSlotTurn identifies a background chief-of-staff turn waiting for or
// holding capacity.manager. At is when its request was enqueued.
type ChiefSlotTurn struct {
	Project    config.ProjectID
	Workstream config.WorkstreamID
	Agent      string
	Turn       string
	At         time.Time
}

// ChiefSlotStatus is an in-memory snapshot of factory-wide chief admission.
// Used counts the slot holders in Active. Unslotted lists the background turns
// running without a slot because the owner messaged their thread. Waiting is in
// admission order, oldest first.
type ChiefSlotStatus struct {
	Limit     int
	Used      int
	Active    []ChiefSlotTurn
	Unslotted []ChiefSlotTurn
	Waiting   []ChiefSlotTurn
}

// ChiefSlots reads chief capacity without opening any project's trace.
func (s *Service) ChiefSlots() ChiefSlotStatus {
	return s.chief.status()
}

type chiefKey struct {
	project    config.ProjectID
	workstream config.WorkstreamID
	agent      string
	turn       string
}

func (t ChiefSlotTurn) key() chiefKey {
	return chiefKey{t.Project, t.Workstream, t.Agent, t.Turn}
}

func (t ChiefSlotTurn) before(o ChiefSlotTurn) bool {
	if !t.At.Equal(o.At) {
		return t.At.Before(o.At)
	}
	return strings.Compare(t.sortKey(), o.sortKey()) < 0
}

func (t ChiefSlotTurn) sortKey() string {
	return string(t.Project) + "\x00" + string(t.Workstream) + "\x00" + t.Agent + "\x00" + t.Turn
}

type chiefSlotPool struct {
	mu        sync.Mutex
	limit     int
	held      map[*conversationPath]map[chiefKey]ChiefSlotTurn
	unslotted map[chiefKey]ChiefSlotTurn
	waiting   map[chiefKey]*chiefWait
	changed   func()
	wake      func()
}

type chiefWait struct {
	turn   ChiefSlotTurn
	gate   *chiefGate
	epoch  uint64
	exempt bool
}

type chiefGate struct {
	pool    *chiefSlotPool
	path    *conversationPath
	project config.ProjectID
	epoch   uint64
}

func newChiefSlotPool(limit int, changed, wake func()) *chiefSlotPool {
	return &chiefSlotPool{limit: limit, changed: changed, wake: wake}
}

func (p *chiefSlotPool) gate(path *conversationPath, project config.ProjectID) *chiefGate {
	if p == nil {
		return nil
	}
	return &chiefGate{pool: p, path: path, project: project}
}

func (p *chiefSlotPool) usedLocked() int {
	n := 0
	for _, held := range p.held {
		n += len(held)
	}
	return n
}

func (p *chiefSlotPool) publish() {
	if p.changed != nil {
		p.changed()
	}
}

func (p *chiefSlotPool) rescan() {
	if p.wake != nil {
		p.wake()
	}
}

func (p *chiefSlotPool) notify() {
	p.publish()
	p.rescan()
}

func (p *chiefSlotPool) resize(limit int) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.limit = limit
	p.mu.Unlock()
	p.notify()
}

func (p *chiefSlotPool) status() ChiefSlotStatus {
	if p == nil {
		return ChiefSlotStatus{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := ChiefSlotStatus{Limit: p.limit, Active: []ChiefSlotTurn{}, Unslotted: []ChiefSlotTurn{}, Waiting: []ChiefSlotTurn{}}
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
	order := func(a, b ChiefSlotTurn) int {
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

func (p *chiefSlotPool) waitingFor(stream config.WorkstreamID, agent, turn string) bool {
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

func (p *chiefSlotPool) drop(path *conversationPath) {
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

func (g *chiefGate) begin() {
	if g == nil {
		return
	}
	g.pool.mu.Lock()
	g.epoch++
	g.pool.mu.Unlock()
}

func (g *chiefGate) end() {
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

func (g *chiefGate) hold(stream config.WorkstreamID, th trace.Thread, q trace.QueuedTurn) bool {
	if g == nil || chiefOwnerStarted(th, q) {
		return false
	}
	turn := ChiefSlotTurn{Project: g.project, Workstream: stream, Agent: th.Identity.ID, Turn: q.Request.TurnID, At: q.Request.At}
	exempt := chiefOwnerBehind(th, q)
	p := g.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.waiting == nil {
		p.waiting = map[chiefKey]*chiefWait{}
	}
	p.waiting[turn.key()] = &chiefWait{turn: turn, gate: g, epoch: g.epoch, exempt: exempt}
	return false
}

func (g *chiefGate) limit(op coreadapter.Operation) (func(), bool) {
	if g == nil {
		return nil, true
	}
	in, err := thread.DecodeTurn(op)
	if err != nil {
		return nil, true
	}
	return g.pool.admit(g, chiefKey{g.project, in.Workstream, in.Agent, in.Turn})
}

func (p *chiefSlotPool) admit(g *chiefGate, key chiefKey) (func(), bool) {
	p.mu.Lock()
	w := p.waiting[key]
	if w == nil || w.gate != g {
		p.mu.Unlock()
		return nil, true
	}
	if w.exempt {
		delete(p.waiting, key)
		if p.unslotted == nil {
			p.unslotted = map[chiefKey]ChiefSlotTurn{}
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
		p.held = map[*conversationPath]map[chiefKey]ChiefSlotTurn{}
	}
	if p.held[g.path] == nil {
		p.held[g.path] = map[chiefKey]ChiefSlotTurn{}
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

func (p *chiefSlotPool) releaser(free func()) func() {
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

func chiefOwnerStarted(th trace.Thread, q trace.QueuedTurn) bool {
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

func chiefOwnerBehind(th trace.Thread, q trace.QueuedTurn) bool {
	i := slices.IndexFunc(th.Turns, func(c trace.QueuedTurn) bool { return c.Request.TurnID == q.Request.TurnID })
	if i < 0 {
		return false
	}
	return slices.ContainsFunc(th.Turns[i+1:], func(c trace.QueuedTurn) bool {
		return c.CompletedAt.IsZero() && chiefOwnerStarted(th, c)
	})
}

func (s *Service) queuedReason(th trace.Thread, q trace.QueuedTurn) string {
	reason := queuedReason(th, q)
	if reason == "starting" && trace.ManagerLike(th.Identity.Role) && s.chief.waitingFor(q.Request.Workstream, th.Identity.ID, q.Request.TurnID) {
		return chiefSlotWaitReason
	}
	return reason
}
