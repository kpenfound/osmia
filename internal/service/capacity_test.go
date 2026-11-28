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
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

type capacityRig struct {
	t     *testing.T
	s     *Service
	repo  *trace.Repository
	clock *fixturekit.DemoClock
}

func newCapacityRig(t *testing.T) *capacityRig {
	t.Helper()
	ctx := context.Background()
	s, _, repo, clock := dailyBudgetService(t, 0, &adaptertest.Turns{})
	fixturekit.Must(t, s.store.Resolve(runtime.Inputs{Config: s.cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{s.cfg.Project.ID: []config.WorkstreamID{fixturekit.Stream, fixturekit.Sibling}}}))
	cfg := *s.cfg
	cfg.Capacity = config.Capacity{Masons: 1, Reviewers: 1, Committee: 3, PerWorkstream: 5}
	cfg.Project.Capacity.PerWorkstream = 2
	s.cfg = &cfg
	for _, turn := range []struct {
		stream      config.WorkstreamID
		agent, role string
	}{{fixturekit.Stream, "mason-1", masonRole}, {fixturekit.Stream, "mason-2", masonRole}, {fixturekit.Stream, "mason-3", masonRole}, {fixturekit.Stream, "r1", reviewerRole}, {fixturekit.Stream, "c1", committeeRole}, {fixturekit.Sibling, "s1", masonRole}} {
		fixturekit.QueueRoleTurn(t, repo, turn.stream, turn.agent, turn.role, "u-"+turn.agent, clock.Now())
	}
	_, err := repo.ClaimTurn(ctx, fixturekit.Stream, "mason-1", "token", t.TempDir(), clock.Now())
	fixturekit.Must(t, err)
	s.pool = &scheduler.Shared{}
	return &capacityRig{t: t, s: s, repo: repo, clock: clock}
}

func (r *capacityRig) dispatch() {
	r.t.Helper()
	dispatch, err := r.s.dispatcher(r.s.cfg, r.repo)
	fixturekit.Must(r.t, err)
	fixturekit.Must(r.t, dispatch.Pass(context.Background()))
}

func (r *capacityRig) siblingWaiting() SlotWait {
	return SlotWait{Workstream: fixturekit.Sibling, Unit: "u-s1", Agent: "s1", Turn: "s1-turn", Reason: DeferCapacity}
}

func deferredUnit(unit, reason string) UnitStatus {
	return UnitStatus{Unit: unit, State: UnitReady, Deferral: &UnitDispatch{Unit: unit, Decision: DispatchDeferred, Reason: reason}}
}

func (r *capacityRig) deferredList() []WorkstreamStatus {
	gone := AbandonedState
	return []WorkstreamStatus{
		{Project: fixturekit.Project, Workstream: fixturekit.Sibling, Units: []UnitStatus{deferredUnit("a", DeferCapacity), deferredUnit("b", DeferEntangled), deferredUnit("c", DeferPriority), deferredUnit("d", DeferWorkstreamCap), deferredUnit("e", DeferPaused)}},
		{Project: fixturekit.Project, Workstream: fixturekit.Stream, Units: []UnitStatus{deferredUnit("f", DeferCapacity)}},
		{Project: fixturekit.Project, Workstream: "w_00000000000000000000000000000abc", State: &gone, Units: []UnitStatus{deferredUnit("g", DeferCapacity)}},
	}
}

func TestStatusCapacityReportsNothingUsedBeforeTheProjectsFirstPass(t *testing.T) {
	t.Parallel()
	r := newCapacityRig(t)
	if got := r.s.statusList(); got.Capacity == nil || fixturekit.RoleCapacityEntry(t, got.Capacity, masonRole).Used != 0 || len(fixturekit.RoleCapacityEntry(t, got.Capacity, masonRole).Waiting) != 0 {
		t.Fatalf("capacity before the project's pass: %+v", got.Capacity)
	}
}

func TestStatusCapacityReportsTheSlotsInUseAndWhoWaitsForThem(t *testing.T) {
	t.Parallel()
	r := newCapacityRig(t)
	r.dispatch()
	got := r.s.statusList()
	want := &CapacityStatus{PerWorkstream: 2, Roles: []RoleCapacity{
		{Role: masonRole, Used: 1, Limit: fixturekit.Limit(1), Waiting: []SlotWait{
			r.siblingWaiting(),
			{Workstream: fixturekit.Stream, Unit: "u-mason-2", Agent: "mason-2", Turn: "mason-2-turn", Reason: DeferWorkstreamCap},
			{Workstream: fixturekit.Stream, Unit: "u-mason-3", Agent: "mason-3", Turn: "mason-3-turn", Reason: DeferWorkstreamCap}}},
		{Role: reviewerRole, Used: 1, Limit: fixturekit.Limit(1), Waiting: []SlotWait{}},
		{Role: committeeRole, Used: 0, Limit: fixturekit.Limit(3), Waiting: []SlotWait{}},
		fixturekit.Unbounded(architectRole, 0, []SlotWait{}),
		{Role: checksRole, Limit: fixturekit.Limit(r.s.CheckSlots().Limit), Waiting: []SlotWait{}},
		{Role: chiefRole, Limit: fixturekit.Limit(r.s.current().Capacity.Manager), Waiting: []SlotWait{}},
	}}
	want.Clients = got.Capacity.Clients
	if !reflect.DeepEqual(got.Capacity, want) {
		t.Fatalf("capacity\n%+v\nwant\n%+v", got.Capacity, want)
	}
}

func TestStatusCapacityDropsTheWaitersOfAPausedWorkstream(t *testing.T) {
	t.Parallel()
	r := newCapacityRig(t)
	r.dispatch()
	fixturekit.Must(t, r.s.setPause(runtime.Pause{Target: runtime.Target{Scope: "workstream", Project: fixturekit.Project, Workstream: fixturekit.Stream}, Mode: "soft", Source: runtime.PauseOwner, Reason: "Hold", SetAt: r.clock.Now()}))
	got := r.s.statusList()
	mason := fixturekit.RoleCapacityEntry(t, got.Capacity, masonRole)
	if mason.Used != 1 || !reflect.DeepEqual(mason.Waiting, []SlotWait{r.siblingWaiting()}) {
		t.Fatalf("mason capacity with the workstream paused: %+v", mason)
	}
	if rv := fixturekit.RoleCapacityEntry(t, got.Capacity, reviewerRole); len(rv.Waiting) != 0 {
		t.Fatalf("reviewer capacity with the workstream paused: %+v", rv)
	}
}

func TestStatusCapacityListsDeferredUnitsOfActiveWorkstreamsThatWaitForASlot(t *testing.T) {
	t.Parallel()
	r := newCapacityRig(t)
	r.dispatch()
	capacity := r.s.capacityStatus(r.deferredList())
	var got []SlotWait
	for _, wait := range fixturekit.RoleCapacityEntry(t, capacity, masonRole).Waiting {
		if wait.Agent == "" {
			got = append(got, wait)
		}
	}
	want := []SlotWait{
		{Workstream: fixturekit.Sibling, Unit: "a", Reason: DeferCapacity},
		{Workstream: fixturekit.Sibling, Unit: "c", Reason: DeferPriority},
		{Workstream: fixturekit.Sibling, Unit: "d", Reason: DeferWorkstreamCap},
		{Workstream: fixturekit.Stream, Unit: "f", Reason: DeferCapacity},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mason waiting with deferred units\n%+v\nwant\n%+v", got, want)
	}
}

func TestStatusCapacityDropsTheDeferredUnitsOfAPausedWorkstream(t *testing.T) {
	t.Parallel()
	r := newCapacityRig(t)
	r.dispatch()
	fixturekit.Must(t, r.s.setPause(runtime.Pause{Target: runtime.Target{Scope: "workstream", Project: fixturekit.Project, Workstream: fixturekit.Stream}, Mode: "soft", Source: runtime.PauseOwner, Reason: "Hold", SetAt: r.clock.Now()}))
	capacity := r.s.capacityStatus(r.deferredList())
	var got []SlotWait
	for _, wait := range fixturekit.RoleCapacityEntry(t, capacity, masonRole).Waiting {
		if wait.Agent == "" {
			got = append(got, wait)
		}
	}
	want := []SlotWait{
		{Workstream: fixturekit.Sibling, Unit: "a", Reason: DeferCapacity},
		{Workstream: fixturekit.Sibling, Unit: "c", Reason: DeferPriority},
		{Workstream: fixturekit.Sibling, Unit: "d", Reason: DeferWorkstreamCap},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("deferred units waiting with the workstream paused\n%+v\nwant\n%+v", got, want)
	}
}

func TestStatusCapacityShowsNoWaitersUnderAFactoryPauseButKeepsTheSlotInFlight(t *testing.T) {
	t.Parallel()
	r := newCapacityRig(t)
	r.dispatch()
	fixturekit.Must(t, r.s.setPause(runtime.Pause{Target: runtime.Target{Scope: "factory"}, Mode: "soft", Source: runtime.PauseOwner, Reason: "Travelling", SetAt: r.clock.Now()}))
	capacity := r.s.capacityStatus(r.deferredList())
	for _, role := range capacity.Roles {
		if len(role.Waiting) != 0 {
			t.Fatalf("%s waits under a factory pause: %+v", role.Role, role.Waiting)
		}
	}
	if fixturekit.RoleCapacityEntry(t, capacity, masonRole).Used != 1 {
		t.Fatalf("the turn in flight lost its slot: %+v", capacity)
	}
}

func TestReadingCapacityPublishesNoOperations(t *testing.T) {
	t.Parallel()
	r := newCapacityRig(t)
	r.dispatch()
	ops, err := r.repo.Operations(fixturekit.Stream)
	fixturekit.Must(t, err)
	r.s.statusList()
	r.s.capacityStatus(r.deferredList())
	after, err := r.repo.Operations(fixturekit.Stream)
	fixturekit.Must(t, err)
	if len(after) != len(ops) {
		t.Fatalf("reading the capacity published %d operations", len(after)-len(ops))
	}
}

// Without an active project no slot is used and the top-level
// per-workstream limit is in force.
func TestStatusCapacityWithoutProject(t *testing.T) {
	t.Parallel()
	s, _, _, _ := dailyBudgetService(t, 1, &adaptertest.Turns{})
	s.cfg = s.cfg.WithLoadedProjects(nil)
	got := s.statusList()
	want := &CapacityStatus{PerWorkstream: s.cfg.Capacity.PerWorkstream, Roles: []RoleCapacity{
		{Role: masonRole, Limit: fixturekit.Limit(s.cfg.Capacity.Masons), Waiting: []SlotWait{}},
		{Role: reviewerRole, Limit: fixturekit.Limit(s.cfg.Capacity.Reviewers), Waiting: []SlotWait{}},
		{Role: committeeRole, Limit: fixturekit.Limit(s.cfg.Capacity.Committee), Waiting: []SlotWait{}},
		fixturekit.Unbounded(architectRole, 0, []SlotWait{}),
		{Role: checksRole, Limit: fixturekit.Limit(s.cfg.Capacity.Checks), Waiting: []SlotWait{}},
		{Role: chiefRole, Limit: fixturekit.Limit(s.cfg.Capacity.Manager), Waiting: []SlotWait{}},
	}}
	want.Clients = got.Capacity.Clients
	if !reflect.DeepEqual(got.Capacity, want) {
		t.Fatalf("capacity without a project %+v, want %+v", got.Capacity, want)
	}
}

type providerUsageRig struct {
	s     *Service
	clock *fixturekit.DemoClock
	limit runtime.ProviderLimit
}

func newProviderUsageRig(t *testing.T) *providerUsageRig {
	t.Helper()
	ctx := context.Background()
	usage := func(cost float64, known bool) adaptertest.Reply[coreadapter.SessionResult] {
		return adaptertest.Reply[coreadapter.SessionResult]{Value: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session"}, FinalResponse: "Done", Usage: coreadapter.Usage{CostUSD: cost, CostKnown: known, Turns: 1}}}
	}
	backend := &adaptertest.Turns{Script: *adaptertest.NewScript[coreadapter.PreparedTurn](usage(0.5, true), usage(0, false))}
	s, a, repo, clock := dailyBudgetService(t, 2, backend)
	for i, turn := range []string{"t-0", "t-1"} {
		op, err := thread.TurnOperation(fixturekit.Project, "event-"+turn, thread.TurnInput{Workstream: fixturekit.Stream, Agent: "agent-" + string(rune('0'+i)), Turn: turn})
		fixturekit.Must(t, err)
		_, err = a.Apply(ctx, op)
		fixturekit.Must(t, err)
	}
	appendDayCost(t, repo, fixturekit.Sibling, "unmatched", clock.Now(), 0.25, true)
	appendDayCost(t, repo, fixturekit.Sibling, "yesterday", clock.Now().Add(-24*time.Hour), 9, true)
	fixturekit.Must(t, repo.Append(ctx, trace.Cost{Header: trace.Header{Schema: "osmia.trace.cost", Version: trace.Version, ID: "judgment-cost", Revision: 1, Project: fixturekit.Project, Workstream: fixturekit.Sibling, At: clock.Now(), Actor: trace.Actor{Kind: "service", ID: jev.Role}, Cause: "budget-fixture"},
		Entry: coreadapter.LedgerEntry{Scope: coreadapter.Scope{Project: string(fixturekit.Project), Workstream: string(fixturekit.Sibling), Thread: "thread", Turn: "judged", Role: jev.Role}, AttemptID: "judgment-attempt", At: clock.Now(), Usage: coreadapter.Usage{CostUSD: 0.125, CostKnown: true}}}))
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
	cfg.Roles[config.ManagerRole] = config.Role{Profile: "other"}
	s.cfg = &cfg
	fixturekit.Must(t, s.store.Resolve(runtime.Inputs{Config: s.cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{s.cfg.Project.ID: []config.WorkstreamID{fixturekit.Stream}}}))
	limit := runtime.ProviderLimit{Backend: "claude", Status: "rejected", Kind: "five_hour", SetAt: clock.Now(), ResetsAt: clock.Now().Add(time.Hour)}
	fixturekit.Must(t, s.store.SetProviderLimit(limit))
	fixturekit.Must(t, dailyBudget{s: s, repository: repo}.Pass(ctx))

	return &providerUsageRig{s: s, clock: clock, limit: limit}
}

func (r *providerUsageRig) status(t *testing.T) StatusResponse {
	t.Helper()
	got := r.s.statusList()
	if got.ProviderUsage == nil {
		t.Fatalf("no provider usage: %+v", got.Diagnostics)
	}
	return got
}

func TestStatusProviderUsageReportsTheDayAndEveryProvider(t *testing.T) {
	t.Parallel()
	u := newProviderUsageRig(t).status(t).ProviderUsage
	if u.Day != "2026-09-16" || len(u.Providers) != 4 {
		t.Fatalf("provider usage %+v", u)
	}
}

func TestStatusProviderUsageCountsJevJudgmentSpend(t *testing.T) {
	t.Parallel()
	judged := newProviderUsageRig(t).status(t).ProviderUsage.Providers[3]
	if judged.Provider != jev.Role || judged.ProviderSpend != (ProviderSpend{SpendUSD: "0.125"}) {
		t.Fatalf("jev %+v", judged)
	}
}

func TestStatusProviderUsageShowsALimitedProvidersFallbacksAndPausedRoles(t *testing.T) {
	t.Parallel()
	r := newProviderUsageRig(t)
	got := r.status(t)
	claude := got.ProviderUsage.Providers[0]
	if claude.Provider != "claude" || claude.ProviderSpend != (ProviderSpend{SpendUSD: "0"}) || claude.Limit == nil || !reflect.DeepEqual(*claude.Limit, r.limit) {
		t.Fatalf("claude %+v", claude)
	}
	if !slices.Contains(claude.Fallbacks, RoleFallback{Role: masonRole, Configured: "default", Profile: "other"}) || slices.ContainsFunc(claude.Fallbacks, func(f RoleFallback) bool { return f.Role == reviewerRole || f.Role == trace.Manager }) {
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
}

func TestStatusProviderUsageShowsAnUnlimitedProviderWithoutFallbacks(t *testing.T) {
	t.Parallel()
	codex := newProviderUsageRig(t).status(t).ProviderUsage.Providers[1]
	if codex.Provider != "codex" || codex.Limit != nil || len(codex.Fallbacks) != 0 || len(codex.PausedRoles) != 0 || codex.SpendUSD != "0" {
		t.Fatalf("codex %+v", codex)
	}
}

func TestStatusProviderUsageSumsKnownSpendAndCountsUnknownCosts(t *testing.T) {
	t.Parallel()
	fake := newProviderUsageRig(t).status(t).ProviderUsage.Providers[2]
	if fake.Provider != "fake" || fake.ProviderSpend != (ProviderSpend{SpendUSD: "0.5", UnknownCosts: 1, LowerBound: true}) || fake.Limit != nil {
		t.Fatalf("fake %+v", fake)
	}
}

func TestStatusProviderUsageReportsSpendNoProviderOwns(t *testing.T) {
	t.Parallel()
	u := newProviderUsageRig(t).status(t).ProviderUsage
	if u.Unattributed == nil || *u.Unattributed != (ProviderSpend{SpendUSD: "0.25"}) {
		t.Fatalf("unattributed %+v", u.Unattributed)
	}
}

func TestStatusDailyBudgetAgreesWithProviderUsage(t *testing.T) {
	t.Parallel()
	got := newProviderUsageRig(t).status(t)
	if b := got.DailyBudget; b == nil || b.SpendUSD != "0.875" || b.UnknownCosts != 1 {
		t.Fatalf("daily budget %+v disagrees with provider usage", b)
	}
}

func TestStatusProviderUsageDropsALimitAtItsReset(t *testing.T) {
	t.Parallel()
	r := newProviderUsageRig(t)
	fixturekit.Jump(r.clock, r.limit.ResetsAt)
	claude := r.status(t).ProviderUsage.Providers[0]
	if claude.Limit != nil || len(claude.Fallbacks) != 0 || len(claude.PausedRoles) != 0 {
		t.Fatalf("claude after the reset %+v", claude)
	}
}

func TestStatusReportsUnreadableProviderUsageAsADiagnostic(t *testing.T) {
	t.Parallel()
	r := newProviderUsageRig(t)
	r.s.setBoundary(func(name string) error {
		if name == "status-provider-usage" {
			return errors.New("injected")
		}
		return nil
	})
	got := r.s.statusList()
	if got.ProviderUsage != nil || !slices.Contains(got.Diagnostics, Diagnostic{Field: "provider_usage", Code: Internal, Message: "cannot read today's costs or turn attempts in project " + string(fixturekit.Project) + "; check the trace repository"}) {
		t.Fatalf("unreadable provider usage: %+v %+v", got.ProviderUsage, got.Diagnostics)
	}
}
