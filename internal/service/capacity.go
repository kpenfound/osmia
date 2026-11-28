package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

func (s *Service) capacityStatus(list []WorkstreamStatus) *CapacityStatus {
	cfg := s.current()
	limits := cfg.Capacity
	if cfg.HasProject() {
		limits.PerWorkstream = cfg.Project.Capacity.PerWorkstream
	}
	out := &CapacityStatus{PerWorkstream: limits.PerWorkstream, Clients: s.clientStatuses(), Roles: []RoleCapacity{
		{Role: masonRole, Limit: &limits.Masons, Waiting: []SlotWait{}},
		{Role: reviewerRole, Limit: &limits.Reviewers, Waiting: []SlotWait{}},
		{Role: committeeRole, Limit: &limits.Committee, Waiting: []SlotWait{}},
		unboundedCapacity(architectRole),
	}}
	slots := s.pool.Slots(cfg.ProjectIDs())
	state, _ := s.effective()
	for i := range out.Roles {
		role := &out.Roles[i]
		role.Used = slots.Used[role.Role]
		for _, w := range slots.Waiting {
			if w.Thread.Identity.Role == role.Role && !scheduler.Held(state.Pauses, w.Project, w.Candidate) {
				role.Waiting = append(role.Waiting, SlotWait{Workstream: w.Workstream, Unit: w.Turn.Request.Unit, Agent: w.Thread.Identity.ID, Turn: w.Turn.Request.TurnID, Reason: w.Reason, Client: w.Client})
			}
		}
		if role.Role != masonRole {
			continue
		}
		for _, w := range list {
			if scheduler.Paused(state.Pauses, w.Project, w.Workstream) || w.State != nil && *w.State == AbandonedState {
				continue
			}
			for _, u := range w.Units {
				if d := u.Deferral; d != nil && (d.Reason == DeferCapacity || d.Reason == DeferPriority || d.Reason == DeferWorkstreamCap) {
					role.Waiting = append(role.Waiting, SlotWait{Workstream: w.Workstream, Unit: u.Unit, Reason: d.Reason})
				}
			}
		}
	}
	checks := s.CheckSlots()
	role := RoleCapacity{Role: checksRole, Limit: &checks.Limit, Used: checks.Used, Waiting: []SlotWait{}}
	for _, w := range checks.Waiting {
		role.Waiting = append(role.Waiting, SlotWait{Project: w.Project, Workstream: w.Workstream, Unit: w.Unit, Review: w.Review, Session: w.Session, Preview: w.Preview, Verify: w.Verify, Reason: DeferCapacity})
	}
	out.Roles = append(out.Roles, role)
	out.Roles = append(out.Roles, s.managerCapacity(limits.Manager))
	return out
}

func (s *Service) managerCapacity(limit int) RoleCapacity {
	manager := s.ManagerSlots()
	role := RoleCapacity{Role: managerRole, Limit: &limit, Used: manager.Used, Waiting: []SlotWait{}}
	for _, w := range manager.Waiting {
		role.Waiting = append(role.Waiting, SlotWait{Project: w.Project, Workstream: w.Workstream, Agent: w.Agent, Turn: w.Turn, Reason: DeferCapacity})
	}
	return role
}

func unboundedCapacity(role string) RoleCapacity {
	return RoleCapacity{Role: role, Unbounded: true, Scope: api.UnboundedScope, Note: api.UnboundedNote, Waiting: []SlotWait{}}
}

func (s *Service) providerUsage(state runtime.State, profiles map[string]EffectiveProfile) (*ProviderUsageStatus, *Diagnostic) {
	cfg, projects := s.runtimes()
	day := s.localDay()
	names := map[string]bool{}
	for _, p := range cfg.Profiles {
		names[p.Agent] = true
	}
	for _, limit := range state.ProviderLimits {
		names[limit.Backend] = true
	}
	unreadable := &Diagnostic{Field: "provider_usage", Code: Internal, Message: fmt.Sprintf("cannot read today's costs or turn attempts in project %s; check the trace repository", cfg.Project.ID)}
	costs := map[string][]trace.Cost{}
	for _, p := range projects {
		if !cfg.Active(p.id) && p.pipeline == nil {
			continue
		}
		unreadable = &Diagnostic{Field: "provider_usage", Code: Internal, Message: fmt.Sprintf("cannot read today's costs or turn attempts in project %s; check the trace repository", p.id)}
		found, err := providerCosts(p.repository, day)
		if err == nil {
			err = s.step("status-provider-usage")
		}
		if err != nil {
			return nil, unreadable
		}
		for name, c := range found {
			costs[name] = append(costs[name], c...)
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
		for _, role := range trace.ConfiguredRoles(cfg) {
			configured := cfg.Roles[trace.ConfigRole(role)].Profile
			registry := name == runtime.SandboxRegistryLimit && cfg.Roles[trace.ConfigRole(role)].Sandbox == "sbx"
			if (cfg.Profiles[configured].Agent != name && !registry) || usage.Limit == nil {
				continue
			}
			switch p := profiles[role]; p.Source {
			case "provider_fallback":
				usage.Fallbacks = append(usage.Fallbacks, RoleFallback{Role: trace.DisplayRole(role), Configured: configured, Profile: p.Name})
			case "provider_pause":
				usage.PausedRoles = append(usage.PausedRoles, trace.DisplayRole(role))
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

func providerCosts(repository *trace.Repository, day calendarDay) (map[string][]trace.Cost, error) {
	var result map[string][]trace.Cost
	err := repository.WithWorkstreamDiscovery(func(streams []config.WorkstreamID) error {
		var err error
		result, err = providerCostsSnapshot(repository, day, streams)
		return err
	})
	return result, err
}

func providerCostsSnapshot(repository *trace.Repository, day calendarDay, streams []config.WorkstreamID) (map[string][]trace.Cost, error) {
	costs, err := repository.Costs()
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
			if name == "" && c.Entry.Scope.Role == jev.Role {
				name = jev.Role
			}
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

func (c *runtimeControls) capacity(repo *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	return coreadapter.Tool{Name: "capacity", Effect: coreadapter.ToolRead, Description: "Read factory-wide role, check and manager slot limits, usage, waiting work, and pauses. The clients list shows the local client and every connected client: its identity, slots used and total, lease health, draining state, the profiles it can and cannot run with their fixes, the fallback profiles it skips, why it takes no new turn, the turns running on it and the turns that failed there. A waiting turn with reason client waits for a slot of a client (the one holding its session, named in client); with reason no-client no connected client can run its profile. A numeric limit on a pool (mason, reviewer, committee, checks, manager) is its configured factory-wide limit: it comes from config.toml and applies on reload. The manager pool (capacity.manager) bounds the background manager turns running at once across every workstream and project: used counts the background turns holding a slot and waiting lists the queued ones in admission order; turns the owner starts take no manager slot. A pool with no limit field and unbounded true (architect) is not bounded factory-wide: it runs at most one turn per workstream, no configuration setting controls it, and there is nothing to raise. Never read it as zero slots. Its used and waiting entries still show its turns.", InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`), Handle: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input map[string]json.RawMessage
		if err := json.Unmarshal(raw, &input); err != nil {
			return nil, err
		}
		if input == nil || len(input) != 0 {
			return nil, errors.New("capacity accepts an empty object")
		}
		if _, _, err := repo.OwnerTurn(trace.Manager, scope); err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s := c.service.Load()
		if s == nil {
			return nil, errors.New("capacity is unavailable")
		}
		list, _, api := s.statuses()
		if api != nil {
			return nil, api
		}
		slots := s.capacityStatus(list)
		state, _ := s.effective()
		return json.Marshal(struct {
			Capacity *CapacityStatus `json:"capacity"`
			Pauses   []runtime.Pause `json:"pauses"`
		}{slots, state.Pauses})
	}}
}
