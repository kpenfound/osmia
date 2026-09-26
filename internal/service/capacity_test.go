package service

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/coreadapter/adaptertest"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// queueRoleTurn creates the agent's thread in stream with the role and queues
// one turn on it, for unit.
func queueRoleTurn(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, agent, role, unit string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	owner := trace.Actor{Kind: "owner", ID: "local"}
	must(t, repo.CreateThread(ctx, trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: agent, Project: project, Workstream: stream, At: at, Actor: owner, Cause: "workstream_created"}, Role: role, ThreadID: agent}))
	_, err := repo.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_" + agent, Project: project, Workstream: stream, Unit: unit, At: at, Actor: owner, Cause: "message_" + agent, Depth: 1},
		AgentID: agent, ThreadID: agent, TurnID: agent + "-turn", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Work"})
	must(t, err)
}

// roleCapacity returns the role's entry of the capacity view.
func roleCapacity(t *testing.T, c *CapacityStatus, role string) RoleCapacity {
	t.Helper()
	for _, r := range c.Roles {
		if r.Role == role {
			return r
		}
	}
	t.Fatalf("capacity has no %s: %+v", role, c)
	return RoleCapacity{}
}

// Status counts the slots in use of each shared role kind as the scheduler
// does, and lists the queued turns and deferred ready units waiting for one;
// work a pause holds waits for no slot, while its turn in flight keeps its
// slot.
func TestStatusReportsCapacityAndWhoWaits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _, repo, clock := dailyBudgetService(t, 0, &adaptertest.Turns{})
	must(t, s.store.Resolve(runtime.Inputs{Config: s.cfg, Workstreams: []config.WorkstreamID{stream, sibling}}))
	cfg := *s.cfg
	cfg.Capacity = config.Capacity{Masons: 1, Reviewers: 1, Committee: 3, PerWorkstream: 5}
	cfg.Project.Capacity.PerWorkstream = 2
	s.cfg = &cfg
	for _, turn := range []struct {
		stream      config.WorkstreamID
		agent, role string
	}{{stream, "m1", masonRole}, {stream, "m2", masonRole}, {stream, "m3", masonRole}, {stream, "r1", reviewerRole}, {stream, "c1", committeeRole}, {sibling, "s1", masonRole}} {
		queueRoleTurn(t, repo, turn.stream, turn.agent, turn.role, "u-"+turn.agent, clock.Now())
	}
	_, err := repo.ClaimTurn(ctx, stream, "m1", "token", t.TempDir(), clock.Now())
	must(t, err)

	got := s.statusList()
	if got.Capacity == nil {
		t.Fatalf("no capacity: %+v", got.Diagnostics)
	}
	// The reviewer takes the free reviewer slot and the workstream's second
	// slot, so the workstream's masons wait for the workstream; the sibling's
	// mason waits for m1's mason slot. The committee's turns run in shed
	// rounds and never wait here.
	want := &CapacityStatus{PerWorkstream: 2, Roles: []RoleCapacity{
		{Role: masonRole, Used: 1, Limit: 1, Waiting: []SlotWait{
			{Workstream: sibling, Unit: "u-s1", Agent: "s1", Turn: "s1-turn", Reason: DeferCapacity},
			{Workstream: stream, Unit: "u-m2", Agent: "m2", Turn: "m2-turn", Reason: DeferWorkstreamCap},
			{Workstream: stream, Unit: "u-m3", Agent: "m3", Turn: "m3-turn", Reason: DeferWorkstreamCap}}},
		{Role: reviewerRole, Used: 0, Limit: 1, Waiting: []SlotWait{}},
		{Role: committeeRole, Used: 0, Limit: 3, Waiting: []SlotWait{}},
	}}
	if !reflect.DeepEqual(got.Capacity, want) {
		t.Fatalf("capacity\n%+v\nwant\n%+v", got.Capacity, want)
	}

	// Pausing the workstream holds its queued turns, which no longer wait,
	// and frees nothing: m1 is in flight and keeps its slot.
	must(t, s.setPause(runtime.Pause{Target: runtime.Target{Scope: "workstream", Project: project, Workstream: stream}, Mode: "soft", Source: runtime.PauseOwner, Reason: "Hold", SetAt: clock.Now()}))
	got = s.statusList()
	mason := roleCapacity(t, got.Capacity, masonRole)
	if mason.Used != 1 || !reflect.DeepEqual(mason.Waiting, []SlotWait{{Workstream: sibling, Unit: "u-s1", Agent: "s1", Turn: "s1-turn", Reason: DeferCapacity}}) {
		t.Fatalf("mason capacity with the workstream paused: %+v", mason)
	}
	if r := roleCapacity(t, got.Capacity, reviewerRole); len(r.Waiting) != 0 {
		t.Fatalf("reviewer capacity with the workstream paused: %+v", r)
	}

	// A ready unit the mason controller deferred for a slot waits too,
	// unless a pause now covers its workstream or the workstream is
	// abandoned; other deferrals do not wait for a slot.
	deferred := func(unit, reason string) UnitStatus {
		return UnitStatus{Unit: unit, State: UnitReady, Deferral: &UnitDispatch{Unit: unit, Decision: DispatchDeferred, Reason: reason}}
	}
	gone := AbandonedState
	list := []WorkstreamStatus{
		{Workstream: sibling, Units: []UnitStatus{deferred("a", DeferCapacity), deferred("b", DeferEntangled), deferred("c", DeferPriority), deferred("d", DeferWorkstreamCap), deferred("e", DeferPaused)}},
		{Workstream: stream, Units: []UnitStatus{deferred("f", DeferCapacity)}},
		// An abandoned workstream keeps its units' last deferral but never
		// starts them.
		{Workstream: "w_00000000000000000000000000000abc", State: &gone, Units: []UnitStatus{deferred("g", DeferCapacity)}},
	}
	capacity, unread := s.capacityStatus(list)
	if unread != nil {
		t.Fatalf("capacity diagnostic %+v", unread)
	}
	if got, want := roleCapacity(t, capacity, masonRole).Waiting, []SlotWait{
		{Workstream: sibling, Unit: "u-s1", Agent: "s1", Turn: "s1-turn", Reason: DeferCapacity},
		{Workstream: sibling, Unit: "a", Reason: DeferCapacity},
		{Workstream: sibling, Unit: "c", Reason: DeferPriority},
		{Workstream: sibling, Unit: "d", Reason: DeferWorkstreamCap},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("mason waiting with deferred units\n%+v\nwant\n%+v", got, want)
	}

	// A factory pause holds everything.
	must(t, s.setPause(runtime.Pause{Target: runtime.Target{Scope: "factory"}, Mode: "soft", Source: runtime.PauseOwner, Reason: "Travelling", SetAt: clock.Now()}))
	capacity, _ = s.capacityStatus(list)
	for _, r := range capacity.Roles {
		if len(r.Waiting) != 0 {
			t.Fatalf("%s waits under a factory pause: %+v", r.Role, r.Waiting)
		}
	}
	if roleCapacity(t, capacity, masonRole).Used != 1 {
		t.Fatalf("the turn in flight lost its slot: %+v", capacity)
	}

	// Reading the capacity dispatched nothing.
	ops, err := repo.Operations(stream)
	must(t, err)
	if len(ops) != 0 {
		t.Fatalf("reading the capacity published %d operations", len(ops))
	}
}

// Without an active project no slot is used and the top-level
// per-workstream limit is in force; turns that cannot be read leave the
// capacity null with a diagnostic.
func TestStatusCapacityWithoutProjectAndUnreadable(t *testing.T) {
	t.Parallel()
	s, _, _, _ := dailyBudgetService(t, 1, &adaptertest.Turns{})
	s.boundary = func(name string) error {
		if name == "status-capacity" {
			return errors.New("injected")
		}
		return nil
	}
	got := s.statusList()
	if got.Capacity != nil || !slices.Contains(got.Diagnostics, Diagnostic{"capacity", Internal, "cannot read the turns of project " + string(project) + "; check the trace repository"}) {
		t.Fatalf("unreadable capacity: %+v %+v", got.Capacity, got.Diagnostics)
	}
	if len(got.Workstreams) == 0 {
		t.Fatal("an unreadable capacity hid the workstreams")
	}

	s.cfg = s.cfg.WithoutProject()
	got = s.statusList()
	want := &CapacityStatus{PerWorkstream: s.cfg.Capacity.PerWorkstream, Roles: []RoleCapacity{
		{Role: masonRole, Limit: s.cfg.Capacity.Masons, Waiting: []SlotWait{}},
		{Role: reviewerRole, Limit: s.cfg.Capacity.Reviewers, Waiting: []SlotWait{}},
		{Role: committeeRole, Limit: s.cfg.Capacity.Committee, Waiting: []SlotWait{}},
	}}
	if !reflect.DeepEqual(got.Capacity, want) {
		t.Fatalf("capacity without a project %+v, want %+v", got.Capacity, want)
	}
}

// Provider usage sums today's known costs by the provider of the attempt each
// records, flags a lower bound when a cost is unknown, keeps costs without a
// recorded attempt apart, and names the limit in force and the roles it
// moved to a fallback profile or paused; the sums agree with the daily
// budget.
func TestStatusReportsProviderUsage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	usage := func(cost float64, known bool) adaptertest.Reply[coreadapter.SessionResult] {
		return adaptertest.Reply[coreadapter.SessionResult]{Value: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session"}, FinalResponse: "Done", Usage: coreadapter.Usage{CostUSD: cost, CostKnown: known, Turns: 1}}}
	}
	backend := &adaptertest.Turns{Script: *adaptertest.NewScript[coreadapter.PreparedTurn](usage(0.5, true), usage(0, false))}
	s, a, repo, clock := dailyBudgetService(t, 2, backend)
	for i, turn := range []string{"t-0", "t-1"} {
		op, err := thread.TurnOperation(project, "event-"+turn, thread.TurnInput{Workstream: stream, Agent: "agent-" + string(rune('0'+i)), Turn: turn})
		must(t, err)
		_, err = a.Apply(ctx, op)
		must(t, err)
	}
	appendDayCost(t, repo, sibling, "classifier", clock.Now(), 0.25, true)
	appendDayCost(t, repo, sibling, "yesterday", clock.Now().Add(-24*time.Hour), 9, true)

	// The mason falls back from claude to codex; the reviewer's profile has
	// no fallback, so the reviewer is paused.
	cfg := *s.cfg
	cfg.Profiles = map[string]config.Profile{
		"default": {Agent: "claude", Model: "test", Fallback: "other"},
		"solo":    {Agent: "claude", Model: "test"},
		"other":   {Agent: "codex", Model: "other"},
	}
	cfg.Roles = map[string]config.Role{}
	for role, binding := range s.cfg.Roles {
		cfg.Roles[role] = binding
	}
	cfg.Roles[masonRole] = config.Role{Profile: "default"}
	cfg.Roles[reviewerRole] = config.Role{Profile: "solo"}
	cfg.Roles[trace.ChiefOfStaff] = config.Role{Profile: "other"}
	s.cfg = &cfg
	must(t, s.store.Resolve(runtime.Inputs{Config: s.cfg, Workstreams: []config.WorkstreamID{stream}}))
	limit := runtime.ProviderLimit{Backend: "claude", Status: "rejected", Kind: "five_hour", SetAt: clock.Now(), ResetsAt: clock.Now().Add(time.Hour)}
	must(t, s.store.SetProviderLimit(limit))

	got := s.statusList()
	u := got.ProviderUsage
	if u == nil {
		t.Fatalf("no provider usage: %+v", got.Diagnostics)
	}
	if u.Day != "2026-09-16" || len(u.Providers) != 3 {
		t.Fatalf("provider usage %+v", u)
	}
	claude, codex, fake := u.Providers[0], u.Providers[1], u.Providers[2]
	if claude.Provider != "claude" || claude.ProviderSpend != (ProviderSpend{SpendUSD: "0"}) || claude.Limit == nil || !reflect.DeepEqual(*claude.Limit, limit) {
		t.Fatalf("claude %+v", claude)
	}
	if !slices.Contains(claude.Fallbacks, RoleFallback{Role: masonRole, Configured: "default", Profile: "other"}) || slices.ContainsFunc(claude.Fallbacks, func(f RoleFallback) bool { return f.Role == reviewerRole || f.Role == trace.ChiefOfStaff }) {
		t.Fatalf("claude fallbacks %+v", claude.Fallbacks)
	}
	for _, f := range claude.Fallbacks {
		if got.Profiles[f.Role].Source != "provider_fallback" || got.Profiles[f.Role].Name != f.Profile {
			t.Fatalf("fallback %+v disagrees with the effective profile %+v", f, got.Profiles[f.Role])
		}
	}
	if !slices.Equal(claude.PausedRoles, []string{reviewerRole}) {
		t.Fatalf("claude paused roles %+v", claude.PausedRoles)
	}
	if codex.Provider != "codex" || codex.Limit != nil || len(codex.Fallbacks) != 0 || len(codex.PausedRoles) != 0 || codex.SpendUSD != "0" {
		t.Fatalf("codex %+v", codex)
	}
	if fake.Provider != "fake" || fake.ProviderSpend != (ProviderSpend{SpendUSD: "0.5", UnknownCosts: 1, LowerBound: true}) || fake.Limit != nil {
		t.Fatalf("fake %+v", fake)
	}
	if u.Unattributed == nil || *u.Unattributed != (ProviderSpend{SpendUSD: "0.25"}) {
		t.Fatalf("unattributed %+v", u.Unattributed)
	}
	if b := got.DailyBudget; b == nil || b.SpendUSD != "0.75" || b.UnknownCosts != 1 {
		t.Fatalf("daily budget %+v disagrees with provider usage", b)
	}

	// Once the limit resets, nothing falls back and no limit is in force.
	jump(clock, limit.ResetsAt)
	got = s.statusList()
	claude = got.ProviderUsage.Providers[0]
	if claude.Limit != nil || len(claude.Fallbacks) != 0 || len(claude.PausedRoles) != 0 {
		t.Fatalf("claude after the reset %+v", claude)
	}

	s.boundary = func(name string) error {
		if name == "status-provider-usage" {
			return errors.New("injected")
		}
		return nil
	}
	got = s.statusList()
	if got.ProviderUsage != nil || !slices.Contains(got.Diagnostics, Diagnostic{"provider_usage", Internal, "cannot read today's costs or turn attempts in project " + string(project) + "; check the trace repository"}) {
		t.Fatalf("unreadable provider usage: %+v %+v", got.ProviderUsage, got.Diagnostics)
	}
}
