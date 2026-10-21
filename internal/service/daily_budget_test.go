package service

import (
	context "context"
	errors "errors"
	api "github.com/kpenfound/osmia/internal/api"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	adaptertest "github.com/kpenfound/osmia/internal/coreadapter/adaptertest"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	scheduler "github.com/kpenfound/osmia/internal/scheduler"
	fixturekit "github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	thread "github.com/kpenfound/osmia/internal/thread"
	trace "github.com/kpenfound/osmia/internal/trace"
	big "math/big"
	reflect "reflect"
	strings "strings"
	testing "testing"
	time "time"
)

// budgetZone is the service host's time zone in daily budget tests: local
// midnight is 07:00 UTC, so demoStart is 02:00 on 2026-09-16 there.
var budgetZone = time.FixedZone("UTC-7", -7*60*60)

// dailyBudgetService is a service with the runtime store, a trace holding the
// workstream with n queued mason turns and the sibling workstream, a per-day
// budget of USD 1.00 and the host time zone budgetZone.
func dailyBudgetService(t *testing.T, n int, backend coreadapter.Turns) (*Service, abandonable, *trace.Repository, *demoClock) {
	t.Helper()
	cfg, repo, clock := hardPauseRepository(t, n)
	must(t, repo.CreateWorkstream(context.Background(), sibling, clock.Now(), trace.Actor{Kind: "owner", ID: "local"}))
	s, a := hardPauseService(t, cfg, repo, clock, backend)
	s.checkSlots = &checkSlotPool{limit: cfg.Capacity.Checks}
	budget := *cfg
	budget.Budget.PerDay = "1.00"
	s.cfg = &budget
	s.options.Location = budgetZone
	s.setSole(&activeProject{repository: repo})
	return s, a, repo, clock
}

// appendDayCost records an attempt's cost in stream, started at at.
func appendDayCost(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, id string, at time.Time, amount float64, known bool) {
	t.Helper()
	must(t, repo.Append(context.Background(), trace.Cost{Header: trace.Header{Schema: "osmia.trace.cost", Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), Workstream: stream, At: at, Actor: trace.Actor{Kind: "service", ID: "thread-runner"}, Cause: "budget-fixture"},
		Entry: coreadapter.LedgerEntry{Scope: coreadapter.Scope{Project: string(repo.Project()), Workstream: string(stream), Thread: "thread", Turn: id, Role: masonRole}, AttemptID: id, At: at, Usage: coreadapter.Usage{CostUSD: amount, CostKnown: known, Turns: 1}}}))
}

// otherBudgetProject adds a second active project to s, with its own trace
// holding stream, and returns that trace.
func otherBudgetProject(t *testing.T, s *Service, repo *trace.Repository, clock *demoClock) *trace.Repository {
	t.Helper()
	ctx := context.Background()
	owner := trace.Actor{Kind: "owner", ID: "local"}
	other, err := trace.Create(ctx, s.cfg.Root, config.Project{ID: otherProject, Clone: t.TempDir()}, clock.Now(), owner)
	must(t, err)
	t.Cleanup(func() { other.Close() })
	must(t, other.CreateWorkstream(ctx, stream, clock.Now(), owner))
	s.mu.Lock()
	s.projects = []*activeProject{runtimeFor(repo), runtimeFor(other)}
	s.mu.Unlock()
	return other
}

func factoryPause(t *testing.T, s *Service) (runtime.Pause, bool) {
	t.Helper()
	st, _ := s.store.Effective()
	return fixturekit.PauseOf(api.RuntimeResponse{Effective: st}, factoryTarget)
}

func budgetStatus(t *testing.T, s *Service) DailyBudgetStatus {
	t.Helper()
	got := s.dailyBudgetStatus()
	if got == nil {
		t.Fatal("no daily budget status")
	}
	return *got
}

// Known spend of every workstream on the host's local calendar day pauses the
// factory softly once it reaches the limit; costs of the previous local day
// and unknown costs do not count, and status reports the spend as a lower
// bound while any attempt's cost is unknown.
func TestDailyBudgetPausesTheFactoryAtTheLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _, repo, clock := dailyBudgetService(t, 0, nil)
	daily := dailyBudget{s: s, repository: repo}
	// 06:30 UTC on the same UTC date is still the previous local day.
	appendDayCost(t, repo, stream, "yesterday", time.Date(2026, 9, 16, 6, 30, 0, 0, time.UTC), 5, true)
	appendDayCost(t, repo, stream, "first", clock.Now(), 0.4, true)
	appendDayCost(t, repo, sibling, "second", clock.Now(), 0.5, true)
	appendDayCost(t, repo, stream, "unknown", clock.Now(), 0, false)
	must(t, daily.Pass(ctx))
	if p, ok := factoryPause(t, s); ok {
		t.Fatalf("paused below the limit: %+v", p)
	}
	if got, want := budgetStatus(t, s), (DailyBudgetStatus{Day: "2026-09-16", SpendUSD: "0.9", LimitUSD: "1.00", UnknownCosts: 1, LowerBound: true}); got != want {
		t.Fatalf("status %+v, want %+v", got, want)
	}

	appendDayCost(t, repo, sibling, "third", clock.Now(), 0.1, true)
	must(t, daily.Pass(ctx))
	p, ok := factoryPause(t, s)
	if !ok || p.Mode != "soft" || p.Source != runtime.PauseDailyBudget || !strings.Contains(p.Reason, "known spend on 2026-09-16 is USD 1 of the USD 1.00 per-day budget") || !strings.Contains(p.Reason, "1 attempt(s) have unknown cost") {
		t.Fatalf("budget pause: %+v", p)
	}
	if st, _ := s.store.Snapshot(); st.BudgetPausedOn != "2026-09-16" {
		t.Fatalf("budget pause day: %+v", st)
	}
	must(t, daily.Pass(ctx))
	if again, _ := factoryPause(t, s); again != p {
		t.Fatalf("a second pass changed the pause: %+v", again)
	}
	if !scheduler.Paused(mustEffective(s).Pauses, project, stream) || !scheduler.Paused(mustEffective(s).Pauses, project, sibling) {
		t.Fatal("the budget pause does not hold the workstreams")
	}
}

// Each project's pass sums only its own trace and counts the other
// project's spend from what that project's own pass published, so the
// factory pauses on their combined spend even when the other trace can no
// longer be read.
func TestDailyBudgetCountsAnotherProjectFromItsPublishedSpend(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _, repo, clock := dailyBudgetService(t, 0, nil)
	other := otherBudgetProject(t, s, repo, clock)
	appendDayCost(t, other, stream, "other", clock.Now(), 0.6, true)
	must(t, dailyBudget{s: s, repository: other}.Pass(ctx))
	must(t, other.Close())
	appendDayCost(t, repo, stream, "first", clock.Now(), 0.3, true)
	must(t, dailyBudget{s: s, repository: repo}.Pass(ctx))
	if p, ok := factoryPause(t, s); ok {
		t.Fatalf("paused below the limit: %+v", p)
	}
	appendDayCost(t, repo, stream, "second", clock.Now(), 0.1, true)
	must(t, dailyBudget{s: s, repository: repo}.Pass(ctx))
	if p, ok := factoryPause(t, s); !ok || !strings.Contains(p.Reason, "known spend on 2026-09-16 is USD 1 of the USD 1.00 per-day budget") {
		t.Fatalf("budget pause on the projects' combined spend: %+v %v", p, ok)
	}
}

// A project no longer active stops counting toward the factory's daily
// spend, and stays forgotten until its own pass publishes its spend again.
func TestDailyBudgetForgetsARemovedProjectsSpend(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _, repo, clock := dailyBudgetService(t, 0, nil)
	other := otherBudgetProject(t, s, repo, clock)
	appendDayCost(t, other, stream, "other", clock.Now(), 0.6, true)
	must(t, dailyBudget{s: s, repository: other}.Pass(ctx))
	s.setSole(runtimeFor(repo))
	appendDayCost(t, repo, stream, "first", clock.Now(), 0.5, true)
	must(t, dailyBudget{s: s, repository: repo}.Pass(ctx))
	if p, ok := factoryPause(t, s); ok {
		t.Fatalf("a removed project's spend counted: %+v", p)
	}
	s.mu.Lock()
	s.projects = append(s.projects, runtimeFor(other))
	s.mu.Unlock()
	must(t, dailyBudget{s: s, repository: repo}.Pass(ctx))
	if p, ok := factoryPause(t, s); ok {
		t.Fatalf("a forgotten project's spend counted before its own pass: %+v", p)
	}
}

// Spend another project published for an earlier local day does not count
// toward today's.
func TestDailyBudgetIgnoresAnotherProjectsSpendOfAnEarlierDay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _, repo, clock := dailyBudgetService(t, 0, nil)
	other := otherBudgetProject(t, s, repo, clock)
	appendDayCost(t, other, stream, "other", clock.Now(), 0.6, true)
	must(t, dailyBudget{s: s, repository: other}.Pass(ctx))
	jump(clock, time.Date(2026, 9, 17, 8, 0, 0, 0, time.UTC))
	appendDayCost(t, repo, stream, "next-day", clock.Now(), 0.5, true)
	must(t, dailyBudget{s: s, repository: repo}.Pass(ctx))
	if p, ok := factoryPause(t, s); ok {
		t.Fatalf("an earlier day's spend counted: %+v", p)
	}
}

// A project's published spend reports a change only when it differs from
// what it last published, or when another project is forgotten.
func TestDaySpendsReportOnlyChanges(t *testing.T) {
	t.Parallel()
	var d daySpends
	spend := func(amount string) spendTotal {
		known, _ := new(big.Rat).SetString(amount)
		return spendTotal{known: known, precision: 2}
	}
	members := []config.ProjectID{project, otherProject}
	if _, changed := d.total(project, "2026-09-16", spend("0.25"), members); !changed {
		t.Fatal("a first publication reported no change")
	}
	if _, changed := d.total(project, "2026-09-16", spend("0.25"), members); changed {
		t.Fatal("an unchanged publication reported a change")
	}
	if _, changed := d.total(project, "2026-09-17", spend("0.25"), members); !changed {
		t.Fatal("a new day reported no change")
	}
	if _, changed := d.total(otherProject, "2026-09-17", spend("0.5"), members); !changed {
		t.Fatal("another project's first publication reported no change")
	}
	total, changed := d.total(project, "2026-09-17", spend("0.25"), []config.ProjectID{project})
	if !changed || total.String() != "0.25" {
		t.Fatalf("forgetting a removed project: changed %t, total %s", changed, total)
	}
}

func mustEffective(s *Service) runtime.State {
	st, _ := s.store.Effective()
	return st
}

// The budget pause lasts until the next local midnight; the new day's spend
// starts from zero and pauses again when it reaches the limit.
func TestDailyBudgetPauseExpiresAtTheLocalDayBoundary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _, repo, clock := dailyBudgetService(t, 0, nil)
	daily := dailyBudget{s: s, repository: repo}
	appendDayCost(t, repo, stream, "spent", clock.Now(), 1.25, true)
	must(t, daily.Pass(ctx))
	if _, ok := factoryPause(t, s); !ok {
		t.Fatal("no budget pause at the limit")
	}
	// 06:59 UTC is still 23:59 of the paused local day.
	jump(clock, time.Date(2026, 9, 17, 6, 59, 0, 0, time.UTC))
	must(t, daily.Pass(ctx))
	if _, ok := factoryPause(t, s); !ok {
		t.Fatal("the budget pause expired before local midnight")
	}
	jump(clock, time.Date(2026, 9, 17, 7, 0, 0, 0, time.UTC))
	must(t, daily.Pass(ctx))
	if p, ok := factoryPause(t, s); ok {
		t.Fatalf("the budget pause outlived its local day: %+v", p)
	}
	if got, want := budgetStatus(t, s), (DailyBudgetStatus{Day: "2026-09-17", SpendUSD: "0", LimitUSD: "1.00"}); got != want {
		t.Fatalf("new day status %+v, want %+v", got, want)
	}
	appendDayCost(t, repo, sibling, "next-day", clock.Now(), 1, true)
	must(t, daily.Pass(ctx))
	if p, ok := factoryPause(t, s); !ok || p.Source != runtime.PauseDailyBudget || !strings.Contains(p.Reason, "2026-09-17") {
		t.Fatalf("the next day's spend did not pause: %+v", p)
	}
}

// An owner who clears the budget pause keeps the factory running for the rest
// of that local day, across a restart; the budget pauses again the next day.
// A factory pause of the owner is never replaced by the budget.
func TestOwnerClearSuppressesTheDailyBudgetForTheRestOfTheDay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _, repo, clock := dailyBudgetService(t, 0, nil)
	daily := dailyBudget{s: s, repository: repo}
	appendDayCost(t, repo, stream, "spent", clock.Now(), 2, true)
	must(t, daily.Pass(ctx))
	if _, ok := factoryPause(t, s); !ok {
		t.Fatal("no budget pause at the limit")
	}
	must(t, s.store.ClearPause(factoryTarget, runtime.PauseOwner))
	appendDayCost(t, repo, stream, "more", clock.Now(), 1, true)
	must(t, daily.Pass(ctx))
	if p, ok := factoryPause(t, s); ok {
		t.Fatalf("the budget pause came back the day the owner cleared it: %+v", p)
	}

	// A restart reopens the runtime store from disk.
	cfg := *s.cfg
	must(t, s.store.Close())
	store, _, err := runtime.Open(runtime.Inputs{Config: &cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{cfg.Project.ID: []config.WorkstreamID{stream}}})
	must(t, err)
	t.Cleanup(func() { store.Close() })
	s.store = store
	jump(clock, time.Date(2026, 9, 17, 6, 58, 0, 0, time.UTC))
	must(t, daily.Pass(ctx))
	if p, ok := factoryPause(t, s); ok {
		t.Fatalf("the budget pause came back after a restart the same day: %+v", p)
	}

	jump(clock, time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC))
	must(t, s.store.SetPause(runtime.Pause{Target: factoryTarget, Mode: "soft", Reason: "Travelling", Source: runtime.PauseOwner, SetAt: clock.Now()}))
	appendDayCost(t, repo, stream, "next-day", clock.Now(), 1, true)
	must(t, daily.Pass(ctx))
	if p, _ := factoryPause(t, s); p.Source != runtime.PauseOwner || p.Reason != "Travelling" {
		t.Fatalf("the budget replaced the owner's pause: %+v", p)
	}
	must(t, s.store.ClearPause(factoryTarget, runtime.PauseOwner))
	must(t, daily.Pass(ctx))
	if p, ok := factoryPause(t, s); !ok || p.Source != runtime.PauseDailyBudget || !strings.Contains(p.Reason, "2026-09-17") {
		t.Fatalf("the budget did not pause the next day: %+v", p)
	}
}

// A turn running when the budget pauses the factory finishes; the next queued
// turn is held.
func TestDailyBudgetLetsTheRunningTurnFinish(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var s *Service
	var repo *trace.Repository
	var clock *demoClock
	var passErr error
	backend := turnFunc(func(ctx context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		appendDayCost(t, repo, stream, "running", clock.Now(), 3, true)
		passErr = dailyBudget{s: s, repository: repo}.Pass(ctx)
		if _, ok := factoryPause(t, s); !ok {
			passErr = errors.New("the budget did not pause during the turn")
		}
		return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session-" + p.Scope.Turn}, FinalResponse: "Done " + p.Scope.Turn}, ctx.Err()
	})
	s, a, repo, clock := dailyBudgetService(t, 2, backend)
	op, err := thread.TurnOperation(project, "event-0", thread.TurnInput{Workstream: stream, Agent: "agent-0", Turn: "t-0"})
	must(t, err)
	result, err := a.Apply(ctx, op)
	must(t, err)
	must(t, passErr)
	th, err := repo.Thread(stream, "agent-0")
	must(t, err)
	if q := th.Turns[0]; result.Outcome != "idle" || q.Status() != "idle" || q.Response == nil || q.Response.Stop != nil || q.Response.Result.FinalResponse != "Done t-0" {
		t.Fatalf("the running turn did not finish: %+v %+v", result, q)
	}
	next, err := repo.Thread(stream, "agent-1")
	must(t, err)
	held, err := s.dispatchHolds(s.cfg, repo)(scheduler.Candidate{Project: project, Workstream: stream, Thread: next, Turn: next.Turns[0]})
	must(t, err)
	if !held {
		t.Fatal("a queued turn was not held under the budget pause")
	}
}

// Status carries no daily budget without budget.per_day, and today's spend
// with it; the API returns both through the status response.
func TestStatusReportsTheDailyBudget(t *testing.T) {
	t.Parallel()
	s, _, repo, clock := dailyBudgetService(t, 0, &adaptertest.Turns{})
	appendDayCost(t, repo, sibling, "spent", clock.Now(), 0.25, true)
	if got := s.statusList(); got.DailyBudget == nil || got.DailyBudget.SpendUSD != "0" {
		t.Fatalf("status daily budget before the project's pass published its spend: %+v", got.DailyBudget)
	}
	must(t, dailyBudget{s: s, repository: repo}.Pass(context.Background()))
	got := s.statusList()
	if want := (&DailyBudgetStatus{Day: "2026-09-16", SpendUSD: "0.25", LimitUSD: "1.00"}); !reflect.DeepEqual(got.DailyBudget, want) {
		t.Fatalf("status daily budget %+v, want %+v", got.DailyBudget, want)
	}
	cfg := *s.cfg
	cfg.Budget.PerDay = ""
	s.cfg = &cfg
	if got := s.statusList(); got.DailyBudget != nil {
		t.Fatalf("status without a daily budget: %+v", got.DailyBudget)
	}
}
