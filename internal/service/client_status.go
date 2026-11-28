package service

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/fleet"
	"github.com/kpenfound/osmia/internal/trace"
)

// clientFailureLimit bounds the failed turns listed for one client.
const clientFailureLimit = 5

// idleProblemLimit bounds the missing items named in the reason a client can
// run nothing.
const idleProblemLimit = 3

type clientTrace struct {
	running  map[string]api.ClientTurn
	failures map[string][]api.ClientFailure
}

// readClientTrace reads, from the active projects' traces, the turns running
// on the clients named in want and, for every client, the turns that failed on
// it since it last ran an attempt without failing. It is best effort: a trace
// that cannot be read leaves its turns out.
func (s *Service) readClientTrace(want map[string]bool) clientTrace {
	out := clientTrace{running: map[string]api.ClientTurn{}, failures: map[string][]api.ClientFailure{}}
	type seen struct {
		failure api.ClientFailure
		failed  bool
		at      time.Time
	}
	attempts := map[string][]seen{}
	cfg, projects := s.runtimes()
	for _, p := range projects {
		if !cfg.Active(p.id) {
			continue
		}
		streams, err := p.repository.Workstreams()
		if err != nil {
			continue
		}
		for _, stream := range streams {
			threads, err := p.repository.Threads(stream)
			if err != nil {
				continue
			}
			for _, th := range threads {
				for _, q := range th.Turns {
					id := q.Request.TurnID
					if want[id] && q.Response == nil {
						turn := api.ClientTurn{Turn: id, Project: p.id, Workstream: stream, Unit: q.Request.Unit, Role: trace.DisplayRole(th.Identity.Role), Agent: trace.DisplayRole(th.Identity.ID), Profile: q.Request.Profile.Name}
						if q.Claim != nil {
							at := q.Claim.At
							turn.StartedAt = &at
						}
						if n := len(q.Attempts); n > 0 {
							turn.Profile = q.Attempts[n-1].Profile.Name
						}
						out.running[id] = turn
					}
					for _, a := range q.Attempts {
						if a.Result == nil || a.Result.Client == "" || a.Result.Cancelled {
							continue
						}
						entry := seen{at: a.At}
						if a.Failure != "" {
							entry.failed = true
							entry.failure = api.ClientFailure{Turn: id, Project: p.id, Workstream: stream, Unit: q.Request.Unit, Role: trace.DisplayRole(th.Identity.Role), Profile: a.Profile.Name, Failure: a.Failure, Class: string(a.FailureClass), At: a.At}
						}
						attempts[a.Result.Client] = append(attempts[a.Result.Client], entry)
					}
				}
			}
		}
	}
	for client, list := range attempts {
		slices.SortStableFunc(list, func(a, b seen) int { return b.at.Compare(a.at) })
		for _, e := range list {
			if !e.failed {
				break
			}
			if len(out.failures[client]) < clientFailureLimit {
				out.failures[client] = append(out.failures[client], e.failure)
			}
		}
	}
	return out
}

// clientStatuses lists the local client and every connected client the way
// placement counts them: identity, slots, lease, what each can run, the
// fallbacks it skips, why it takes nothing, and the turns running on it and
// the ones that failed there.
func (s *Service) clientStatuses() []api.ClientStatus {
	out := []api.ClientStatus{}
	if s.placement == nil || s.fleet == nil {
		return out
	}
	places := s.placement.Places()
	want := map[string]bool{}
	for _, place := range places {
		for _, turn := range place.Turns {
			want[turn] = true
		}
	}
	found := s.readClientTrace(want)
	handout := s.fleet.Handout()
	connected := map[string]fleet.Client{}
	for _, c := range s.fleet.Clients() {
		connected[c.Identity.ID] = c
	}
	for _, place := range places {
		status := api.ClientStatus{ID: place.ID, Name: place.Name, Local: place.Local, Used: place.Used, Draining: place.Draining, Idle: place.Idle,
			Profiles: []api.ClientProfileStatus{}, Skipped: []api.ClientSkip{}, Turns: []api.ClientTurn{}, Failures: []api.ClientFailure{}}
		if place.Unbounded {
			status.Unbounded = true
		} else {
			slots := place.Slots
			status.Slots = &slots
		}
		for _, id := range place.Turns {
			turn, ok := found.running[id]
			if !ok {
				turn = api.ClientTurn{Turn: id}
			}
			status.Turns = append(status.Turns, turn)
		}
		status.Failures = append(status.Failures, found.failures[place.ID]...)
		var caps *api.CapabilityReport
		if place.Local {
			status.Reported, status.ReportCurrent = true, true
			for _, p := range handout.Profiles {
				status.Profiles = append(status.Profiles, api.ClientProfileStatus{Profile: p.Name, Runnable: true})
			}
		} else if c, ok := connected[place.ID]; ok {
			status.Lease = &api.ClientLease{Health: c.Health, ConnectedAt: c.ConnectedAt, LastSeen: c.LastSeen, EndsAt: c.LeaseEndsAt}
			if place.Slots <= 0 && !place.Draining {
				status.Idle = api.ClientIdleNoSlots
			}
			if c.Capabilities != nil {
				caps = c.Capabilities
				status.Reported, status.ReportCurrent = true, c.ReportCurrent()
				for _, p := range caps.Profiles {
					status.Profiles = append(status.Profiles, api.ClientProfileStatus{Profile: p.Profile, Runnable: p.Runnable, Problems: slices.Clone(p.Problems)})
				}
			}
			status.Skipped = skippedFallbacks(handout, c)
		}
		status.IdleReason = idleReason(status, caps)
		out = append(out, status)
	}
	return out
}

func skippedFallbacks(h api.ProfileHandout, c fleet.Client) []api.ClientSkip {
	out := []api.ClientSkip{}
	for _, role := range h.Roles {
		if len(role.Chain) < 2 || !c.CanRun(role.Chain[0]) {
			continue
		}
		for _, name := range role.Chain[1:] {
			if !c.CanRun(name) {
				out = append(out, api.ClientSkip{Role: trace.DisplayRole(role.Role), Main: role.Chain[0], Profile: name})
			}
		}
	}
	return out
}

func idleReason(c api.ClientStatus, caps *api.CapabilityReport) string {
	switch c.Idle {
	case api.ClientIdleDraining:
		return "It is draining: it takes no new turns and keeps its lease while its running turns finish."
	case api.ClientIdleNoSlots:
		if c.Local {
			return "The local client has no slots, so no session runs on the server's machine. Set capacity.local_slots above 0, or remove it, to run sessions here."
		}
		return "It declared no slots. Start osmia client again with --slots set to 1 or more."
	case api.ClientIdleFull:
		return "Every slot is in use; it takes a turn when one ends."
	case api.ClientIdleNoReport:
		return "It has not reported what it can run yet; it takes turns once its first report arrives."
	case api.ClientIdleNoProfile:
		return "It can run no profile. " + missing(caps)
	}
	return ""
}

func missing(caps *api.CapabilityReport) string {
	if caps == nil {
		return ""
	}
	var parts []string
	seen := map[string]bool{}
	for _, p := range caps.Profiles {
		for _, problem := range p.Problems {
			text := problem.Check
			if problem.Detail != "" {
				text += " (" + problem.Detail + ")"
			}
			if problem.Fix != "" {
				text += ": " + problem.Fix
			}
			if !seen[text] {
				seen[text] = true
				parts = append(parts, text)
			}
		}
	}
	if len(parts) == 0 {
		return "Its report lists no profile it can run."
	}
	if len(parts) > idleProblemLimit {
		parts = append(parts[:idleProblemLimit], fmt.Sprintf("and %d more", len(parts)-idleProblemLimit))
	}
	return "Missing: " + strings.Join(parts, "; ") + "."
}

func (s *Service) placedClient(turn string) string {
	if s.placement == nil {
		return ""
	}
	client, _ := s.placement.PlacedOn(turn)
	return client
}
