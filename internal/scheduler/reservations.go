package scheduler

import (
	"maps"
	"sync"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// TryReserve offers a workflow operation its role's capacity without waiting
// for a slot. The release must run after inspection, execution and recording
// have ended. Architect and librarian roles retain their per-workstream role
// limit; committee shares its configured role limit with scheduled turns.
// The operation's own runner turns are covered by this reservation. Workflow
// reservations do not consume the per-workstream allowance for worker turns.
func (p *Shared) TryReserve(repository *trace.Repository, stream config.WorkstreamID, role, agent string, limits *config.Capacity) (func(), bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.usageMu.Lock()
	busy := p.reservations[reservationKey{repository.Project(), stream, role, agent}] > 0
	p.usageMu.Unlock()
	if busy {
		return nil, false, nil
	}
	l, err := load(repository)
	if err != nil {
		return nil, false, err
	}
	own := newUsage()
	p.countWithout(l, own, agent, stream)
	used := newUsage()
	used.merge(own)
	used.merge(p.reserved(l.project))
	for kind, n := range p.others(l.project) {
		used.roles[kind] += n
	}
	candidate := Candidate{Project: l.project, Workstream: stream, Thread: trace.Thread{Identity: trace.Agent{Role: role}}}
	s := Scheduler{options: Options{Capacity: limits}}
	if s.roleRefusal(used, candidate) != "" {
		return nil, false, nil
	}
	key := reservationKey{l.project, stream, role, agent}
	p.usageMu.Lock()
	if p.usage == nil {
		p.usage = map[config.ProjectID]published{}
	}
	previous := p.usage[l.project]
	p.usage[l.project] = published{roles: maps.Clone(own.roles), waiting: previous.waiting}
	if p.reservations == nil {
		p.reservations = map[reservationKey]int{}
	}
	p.reservations[key]++
	p.usageMu.Unlock()
	if p.Changed != nil {
		p.Changed()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			p.usageMu.Lock()
			p.reservations[key]--
			if p.reservations[key] == 0 {
				delete(p.reservations, key)
			}
			p.usageMu.Unlock()
			p.mu.Unlock()
			if p.Changed != nil {
				p.Changed()
			}
		})
	}, true, nil
}

func (p *Shared) reserved(project config.ProjectID) usage {
	out := newUsage()
	if p == nil {
		return out
	}
	p.usageMu.Lock()
	defer p.usageMu.Unlock()
	for key, n := range p.reservations {
		if key.project == project {
			out.roles[key.role] += n
			out.local[localKey{key.project, key.workstream, key.role}] += n
		}
	}
	return out
}

// count excludes runner turns covered by workflow reservations, so publishing
// a scheduler pass neither doubles their use nor loses an unclaimed slot.
// Callers hold mu, which also fences reservation release against publication.
func (p *Shared) count(l loaded, used usage) {
	p.countWithout(l, used, "", "")
}

func (p *Shared) countWithout(l loaded, used usage, agent string, owner config.WorkstreamID) {
	covered := map[turnKey]bool{}
	if p != nil {
		p.usageMu.Lock()
		for key := range p.reservations {
			if key.project == l.project {
				covered[turnKey{workstream: key.workstream, agent: key.agent}] = true
			}
		}
		p.usageMu.Unlock()
	}
	if agent != "" {
		covered[turnKey{workstream: owner, agent: agent}] = true
	}
	for _, stream := range l.streams {
		for _, t := range l.threads[stream] {
			if !covered[turnKey{workstream: stream, agent: t.Identity.ID}] && inFlight(t, stream, l.dispatched) {
				used.add(l.project, stream, t.Identity.Role)
			}
		}
	}
}

// reservationKey identifies the runner thread covered by an operation's slot.
type reservationKey struct {
	project    config.ProjectID
	workstream config.WorkstreamID
	role       string
	agent      string
}
