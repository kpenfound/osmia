package scheduler

import (
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/slotspans"
	"github.com/kpenfound/osmia/internal/telemetry"
	"github.com/kpenfound/osmia/internal/trace"
)

// StaleReservationWait is how long a refused workflow reservation stays an
// open wait without being refused again before its wait span is sent as
// abandoned, at the last refusal.
const StaleReservationWait = 5 * time.Minute

type flight struct {
	workstream config.WorkstreamID
	agent      string
	turn       string
	role       string
	unit       string
	client     string
	start      time.Time
}

func flightOf(c Candidate, start time.Time, client string) flight {
	return flight{workstream: c.Workstream, agent: c.Thread.Identity.ID, turn: c.Turn.Request.TurnID, role: c.Thread.Identity.Role, unit: c.Turn.Request.Unit, client: client, start: start}
}

func (l loaded) flight(stream config.WorkstreamID, t trace.Thread, q trace.QueuedTurn) flight {
	f := flight{workstream: stream, agent: t.Identity.ID, turn: q.Request.TurnID, role: t.Identity.Role, unit: q.Request.Unit, start: l.dispatchedAt[turnKey{stream, t.Identity.ID, q.Request.TurnID}]}
	if f.start.IsZero() && q.Claim != nil {
		f.start = q.Claim.At
	}
	if l.client != nil {
		f.client = l.client(q.Request.TurnID)
	}
	return f
}

func (u usage) addTurn(f flight) {
	if trace.ManagerLike(f.role) {
		return
	}
	u.flights[turnKey{f.workstream, f.agent, f.turn}] = f
}

func turnWork(agent, turn string) string { return agent + "/" + turn }

func limitOf(limits *config.Capacity, role string) (int, bool) {
	if limits == nil {
		return 0, false
	}
	switch role {
	case "mason":
		return limits.Masons, true
	case "reviewer":
		return limits.Reviewers, true
	case "committee":
		return limits.Committee, true
	}
	return 0, false
}

func (s *Scheduler) limits(role string) (int, bool) { return limitOf(s.options.Capacity, role) }

func (p *Shared) spans() *slotspans.Recorder {
	if p == nil {
		return nil
	}
	return p.Spans
}

func (f flight) interval(project config.ProjectID) slotspans.Interval {
	return slotspans.Interval{Kind: slotspans.Held, Source: slotspans.SourceTurn, Pool: telemetry.PoolID(f.role), Role: f.role, Project: project, Workstream: f.workstream, Unit: f.unit, Agent: f.agent, Client: f.client, Work: turnWork(f.agent, f.turn), Start: f.start}
}

func (p *Shared) syncHolds(project config.ProjectID, flights map[turnKey]flight, limits func(string) (int, bool), at time.Time) {
	rec := p.spans()
	if rec == nil {
		return
	}
	covered := map[turnKey]bool{}
	for _, iv := range rec.Find(slotspans.Held, slotspans.SourceReservation, func(iv slotspans.Interval) bool { return iv.Project == project }) {
		covered[turnKey{workstream: iv.Workstream, agent: iv.Agent}] = true
	}
	live := map[string]bool{}
	for key, f := range flights {
		if covered[turnKey{workstream: key.workstream, agent: key.agent}] {
			continue
		}
		iv := f.interval(project)
		if iv.Start.IsZero() {
			iv.Start = at
		}
		if limits != nil {
			iv.Bound(limits(f.role))
		}
		have, _ := rec.Begin(iv)
		live[have.Key()] = true
	}
	for _, iv := range rec.Find(slotspans.Held, slotspans.SourceTurn, func(iv slotspans.Interval) bool { return iv.Project == project }) {
		if !live[iv.Key()] {
			rec.End(iv, at, slotspans.Released)
		}
	}
	rec.Expire(at)
}

func (p *Shared) endProject(project config.ProjectID, at time.Time) {
	rec := p.spans()
	if rec == nil {
		return
	}
	of := func(iv slotspans.Interval) bool { return iv.Project == project }
	for _, source := range []string{slotspans.SourceTurn, slotspans.SourceReservation} {
		for _, iv := range rec.Find(slotspans.Held, source, of) {
			rec.End(iv, at, slotspans.Released)
		}
		for _, iv := range rec.Find(slotspans.Wait, source, of) {
			rec.End(iv, at, slotspans.Removed)
		}
	}
}

func (s *Scheduler) waitInterval(c Candidate) slotspans.Interval {
	role := c.Thread.Identity.Role
	return slotspans.Interval{Kind: slotspans.Wait, Source: slotspans.SourceTurn, Pool: telemetry.PoolID(role), Role: role, Project: c.Project, Workstream: c.Workstream,
		Unit: c.Turn.Request.Unit, Agent: c.Thread.Identity.ID, Work: turnWork(c.Thread.Identity.ID, c.Turn.Request.TurnID)}
}

func (s *Scheduler) refused(rec *slotspans.Recorder, c Candidate, reason, client string) {
	if rec == nil {
		return
	}
	iv := s.waitInterval(c)
	iv.Start, iv.Reason, iv.Client = s.options.Now(), reason, client
	iv.Bound(s.limits(c.Thread.Identity.Role))
	rec.Begin(iv)
}

func (s *Scheduler) endGoneWaits(rec *slotspans.Recorder, l loaded, touched map[string]bool) {
	if rec == nil {
		return
	}
	project := s.repository.Project()
	for _, iv := range rec.Find(slotspans.Wait, slotspans.SourceTurn, func(iv slotspans.Interval) bool { return iv.Project == project }) {
		if touched[iv.Key()] {
			continue
		}
		end, outcome := s.fate(l, iv)
		rec.End(iv, end, outcome)
	}
}

const canceledSessionDirectory = "none"

func (s *Scheduler) fate(l loaded, iv slotspans.Interval) (time.Time, string) {
	now := s.options.Now()
	for _, t := range l.threads[iv.Workstream] {
		if t.Identity.ID != iv.Agent {
			continue
		}
		for _, q := range t.Turns {
			if turnWork(iv.Agent, q.Request.TurnID) != iv.Work {
				continue
			}
			key := turnKey{iv.Workstream, iv.Agent, q.Request.TurnID}
			switch {
			case !q.CompletedAt.IsZero() && !l.dispatched[key] && (q.Claim == nil || q.Claim.SessionDirectory == canceledSessionDirectory):
				return q.CompletedAt, slotspans.Canceled
			case l.dispatched[key] && !l.dispatchedAt[key].IsZero():
				return l.dispatchedAt[key], slotspans.Granted
			case q.Claim != nil:
				return q.Claim.At, slotspans.Granted
			}
			return now, slotspans.Blocked
		}
	}
	return now, slotspans.Removed
}

func reservationInterval(kind slotspans.Kind, project config.ProjectID, stream config.WorkstreamID, role, agent string, at time.Time, limits *config.Capacity) slotspans.Interval {
	iv := slotspans.Interval{Kind: kind, Source: slotspans.SourceReservation, Pool: telemetry.PoolID(role), Role: role, Project: project, Workstream: stream, Agent: agent, Work: agent, Start: at}
	iv.Bound(limitOf(limits, role))
	return iv
}
