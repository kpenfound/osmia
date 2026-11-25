package scheduler

import (
	"github.com/kpenfound/osmia/internal/fleet"
	"github.com/kpenfound/osmia/internal/thread"
)

// PlacementTurn describes a candidate for placement. A candidate that resumes
// a thread's saved session names the client that holds it, whether or not
// that client can still be waited for. resumable is the thread runner's
// ResumableRoles.
func PlacementTurn(c Candidate, resumable func(role string) bool) fleet.Turn {
	t := fleet.Turn{Project: string(c.Project), Workstream: string(c.Workstream), Agent: c.Thread.Identity.ID, ID: c.Turn.Request.TurnID, Role: c.Thread.Identity.Role, Profile: c.Turn.Request.Profile.Name}
	if client, ok := thread.Resumes(c.Thread, c.Turn, resumable); ok {
		t.SessionClient = client
	}
	return t
}

func turnOf(c Candidate) turnKey {
	return turnKey{c.Workstream, c.Thread.Identity.ID, c.Turn.Request.TurnID}
}

type planned struct {
	turns map[turnKey]fleet.Turn
	// pinned is the client each resumed candidate waits for, if it is pinned.
	pinned map[turnKey]string
	// owed is the resumed turns the pass still owes a free slot of their
	// client: those the role limits and the dispatch gate let start, by turn ID.
	owed map[string]string
}

func (s *Scheduler) places(candidates []Candidate) planned {
	out := planned{turns: map[turnKey]fleet.Turn{}, pinned: map[turnKey]string{}, owed: map[string]string{}}
	placement := s.options.Placement
	if placement == nil {
		return out
	}
	for _, c := range candidates {
		ft := PlacementTurn(c, s.options.Resumable)
		out.turns[turnOf(c)] = ft
		if ft.SessionClient != "" {
			if client, ok := placement.Pinned(ft); ok {
				out.pinned[turnOf(c)] = client
			}
		}
	}
	return out
}

func (s *Scheduler) screen(p planned, candidates []Candidate, used usage, held func(Candidate) (bool, error)) error {
	for _, c := range candidates {
		client, ok := p.pinned[turnOf(c)]
		if !ok || s.refusal(used, c) != "" {
			continue
		}
		if held != nil {
			h, err := held(c)
			if err != nil {
				return err
			}
			if h {
				continue
			}
		}
		p.owed[c.Turn.Request.TurnID] = client
	}
	s.options.Placement.Owe(string(s.repository.Project()), p.owed)
	return nil
}
