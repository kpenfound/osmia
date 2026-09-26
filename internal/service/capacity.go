package service

import (
	"fmt"
	"maps"
	"slices"

	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// capacityStatus reports the slots of the mason, reviewer and committee role
// kinds as the scheduler counts them, with the queued turns its gate would
// admit but that find no free slot, and, for masons, the ready units of list
// whose latest mason controller decision is to wait for a slot. Work a pause
// in force holds, and work of an abandoned workstream, is not waiting. Without an active project no slot is used.
func (s *Service) capacityStatus(list []WorkstreamStatus) (*CapacityStatus, *Diagnostic) {
	s.mu.Lock()
	active, cfg := s.active, s.cfg
	s.mu.Unlock()
	limits := cfg.Capacity
	if cfg.HasProject() {
		limits.PerWorkstream = cfg.Project.Capacity.PerWorkstream
	}
	out := &CapacityStatus{PerWorkstream: limits.PerWorkstream, Roles: []RoleCapacity{
		{Role: masonRole, Limit: limits.Masons, Waiting: []SlotWait{}},
		{Role: reviewerRole, Limit: limits.Reviewers, Waiting: []SlotWait{}},
		{Role: committeeRole, Limit: limits.Committee, Waiting: []SlotWait{}},
	}}
	if !cfg.HasProject() || active == nil {
		return out, nil
	}
	project, repository := cfg.Project.ID, active.repository
	librarian := librarianWorkstream(project)
	unreadable := &Diagnostic{"capacity", Internal, fmt.Sprintf("cannot read the turns of project %s; check the trace repository", project)}
	dispatch, err := scheduler.New(repository, scheduler.Options{Now: s.now, Capacity: &limits, Priorities: s.priorities})
	if err != nil {
		return nil, unreadable
	}
	slots, err := dispatch.Slots(func(c scheduler.Candidate) (bool, error) { return s.holds(project, librarian, repository, c) })
	if err == nil {
		err = s.step("status-capacity")
	}
	if err != nil {
		return nil, unreadable
	}
	state, _ := s.effective()
	for i := range out.Roles {
		role := &out.Roles[i]
		role.Used = slots.Used[role.Role]
		for _, w := range slots.Waiting {
			if w.Thread.Identity.Role == role.Role {
				role.Waiting = append(role.Waiting, SlotWait{Workstream: w.Workstream, Unit: w.Turn.Request.Unit, Agent: w.Thread.Identity.ID, Turn: w.Turn.Request.TurnID, Reason: w.Reason})
			}
		}
		if role.Role != masonRole {
			continue
		}
		for _, w := range list {
			if scheduler.Paused(state.Pauses, project, w.Workstream) || w.State != nil && *w.State == AbandonedState {
				continue
			}
			for _, u := range w.Units {
				if d := u.Deferral; d != nil && (d.Reason == DeferCapacity || d.Reason == DeferPriority || d.Reason == DeferWorkstreamCap) {
					role.Waiting = append(role.Waiting, SlotWait{Workstream: w.Workstream, Unit: u.Unit, Reason: d.Reason})
				}
			}
		}
	}
	return out, nil
}

// providerUsage reports today's known spend by provider, the provider of the
// turn attempt each cost records, next to each provider's usage limit in
// state and the roles profiles says run a fallback, or are paused, because
// of it. The providers are those of the configured profiles, of today's
// costs and of the limits, by name.
func (s *Service) providerUsage(state runtime.State, profiles map[string]EffectiveProfile) (*ProviderUsageStatus, *Diagnostic) {
	s.mu.Lock()
	active, cfg := s.active, s.cfg
	s.mu.Unlock()
	day := s.localDay()
	names := map[string]bool{}
	for _, p := range cfg.Profiles {
		names[p.Agent] = true
	}
	for _, limit := range state.ProviderLimits {
		names[limit.Backend] = true
	}
	unreadable := &Diagnostic{"provider_usage", Internal, fmt.Sprintf("cannot read today's costs or turn attempts in project %s; check the trace repository", cfg.Project.ID)}
	costs := map[string][]trace.Cost{}
	if cfg.HasProject() && active != nil {
		var err error
		if costs, err = providerCosts(active.repository, day); err == nil {
			err = s.step("status-provider-usage")
		}
		if err != nil {
			return nil, unreadable
		}
		for name := range costs {
			if name != "" {
				names[name] = true
			}
		}
	}
	out := &ProviderUsageStatus{Day: day.date, Providers: []ProviderUsage{}}
	for _, name := range slices.Sorted(maps.Keys(names)) {
		spend, err := providerSpend(costs[name])
		if err != nil {
			return nil, unreadable
		}
		usage := ProviderUsage{Provider: name, ProviderSpend: spend, Fallbacks: []RoleFallback{}, PausedRoles: []string{}}
		for _, limit := range state.ProviderLimits {
			if limit.Backend == name {
				usage.Limit = &limit
				break
			}
		}
		for _, role := range slices.Sorted(maps.Keys(cfg.Roles)) {
			configured := cfg.Roles[role].Profile
			if cfg.Profiles[configured].Agent != name || usage.Limit == nil {
				continue
			}
			switch p := profiles[role]; p.Source {
			case "provider_fallback":
				usage.Fallbacks = append(usage.Fallbacks, RoleFallback{Role: role, Configured: configured, Profile: p.Name})
			case "provider_pause":
				usage.PausedRoles = append(usage.PausedRoles, role)
			}
		}
		out.Providers = append(out.Providers, usage)
	}
	if len(costs[""]) > 0 {
		spend, err := providerSpend(costs[""])
		if err != nil {
			return nil, unreadable
		}
		out.Unattributed = &spend
	}
	return out, nil
}

// providerCosts returns the costs of every workstream's attempts that started
// on day, by the provider of the turn attempt each one records; costs no
// recorded attempt matches, such as a classifier's, are under "".
func providerCosts(repository *trace.Repository, day calendarDay) (map[string][]trace.Cost, error) {
	costs, err := repository.Costs()
	if err != nil {
		return nil, err
	}
	streams, err := repository.Workstreams()
	if err != nil {
		return nil, err
	}
	providers := map[string]string{}
	for _, stream := range streams {
		threads, err := repository.Threads(stream)
		if err != nil {
			return nil, err
		}
		for _, th := range threads {
			for _, q := range th.Turns {
				for _, a := range q.Attempts {
					providers[thread.AttemptID(q.Request.ID, a.Number)] = a.Profile.Backend
				}
			}
		}
	}
	out := map[string][]trace.Cost{}
	for _, c := range costs {
		if !c.Entry.At.Before(day.start) && c.Entry.At.Before(day.end) {
			name := providers[c.Entry.AttemptID]
			out[name] = append(out[name], c)
		}
	}
	return out, nil
}

func providerSpend(costs []trace.Cost) (ProviderSpend, error) {
	spend, err := sumCosts(costs)
	if err != nil {
		return ProviderSpend{}, err
	}
	return ProviderSpend{SpendUSD: spend.String(), UnknownCosts: spend.unknown, LowerBound: spend.unknown > 0}, nil
}
