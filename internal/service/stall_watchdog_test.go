package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

const stallUnit = "u1"

var stallSeeder = trace.Actor{Kind: "service", ID: "stall-seeder"}

type exitLog struct {
	mu    sync.Mutex
	exits []router.Exit
}

func (l *exitLog) submit(x router.Exit) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.exits = append(l.exits, x)
	return true
}

func (l *exitLog) occurrences() map[string]router.Exit {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]router.Exit{}
	for _, x := range l.exits {
		out[router.OccurrenceKey(x)] = x
	}
	return out
}

func (l *exitLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.exits = nil
}

type stallFixture struct {
	s     *Service
	repo  *trace.Repository
	clock *fixturekit.FixedClock
	jev   *jevfixture.Transitions
	log   *exitLog
}

func newStallFixture(t *testing.T, loop string, p *jevfixture.Transitions) *stallFixture {
	t.Helper()
	ctx := context.Background()
	key := gateKey(t, true)
	opts, cfg, repo := activityFixture(t)
	fixturekit.Must(t, repo.CreateWorkstream(ctx, fixturekit.Stream, fixturekit.DemoStart, ownerActor))
	fixturekit.Must(t, repo.Close())
	clock := &fixturekit.FixedClock{At: fixturekit.DemoStart}
	opts.Reconciliation.Now = clock.Now
	withTopLevel(t, opts, "\n"+loop+"\n"+gateJevConfig(key))
	if p == nil {
		p = &jevfixture.Transitions{}
	}
	opts.JevProvider = p.Factory()
	s, err := Start(ctx, opts)
	fixturekit.Must(t, err)
	fixturekit.Must(t, s.Close())
	repo, err = trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	t.Cleanup(func() { repo.Close() })
	f := &stallFixture{s: s, repo: repo, clock: clock, jev: p, log: &exitLog{}}
	f.transition(t, "stall-seed-feature", trace.FeatureSubject, "", BuildingState, "", stallSeeder, fixturekit.DemoStart)
	f.transition(t, "stall-seed-unit", trace.UnitSubject(stallUnit), "", UnitImplementing, stallUnit, stallSeeder, fixturekit.DemoStart)
	return f
}

func (f *stallFixture) transition(t *testing.T, id, subject, from, to, unit string, actor trace.Actor, at time.Time) {
	t.Helper()
	state, err := f.repo.RawWorkflow(fixturekit.Stream, subject)
	fixturekit.Must(t, err)
	if state.Value != from {
		t.Fatalf("subject %s is %q, not %q", subject, state.Value, from)
	}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: f.repo.Project(), Workstream: fixturekit.Stream, Unit: unit, At: at, Actor: actor, Cause: "seed"}
	_, err = f.repo.Transact(context.Background(), trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: subject, From: from, To: to, Reason: "seeded"}})
	fixturekit.Must(t, err)
}

func (f *stallFixture) watch(after string) stallWatch {
	cfg := *f.s.current()
	cfg.Loop.StallAfter = after
	return stallWatch{s: f.s, cfg: &cfg, repository: f.repo, submit: f.log.submit}
}

func (f *stallFixture) quietFor(d time.Duration) {
	f.clock.Mu.Lock()
	defer f.clock.Mu.Unlock()
	f.clock.At = fixturekit.DemoStart.Add(d)
}

func (f *stallFixture) firstPassed(w stallWatch) {
	f.s.stalls.due(w.repository.Project(), time.Now().Add(-time.Hour), stallCheckEvery)
}

func (f *stallFixture) pass(t *testing.T, w stallWatch) map[string]router.Exit {
	t.Helper()
	f.log.reset()
	fixturekit.Must(t, w.Pass(context.Background()))
	out := map[string]router.Exit{}
	for _, x := range f.log.occurrences() {
		out[x.Trigger] = x
	}
	return out
}

// A configured [loop] stall_after is the window a failing landing, rebase or
// publication is retried for; with the watchdog off the window stays at 45m.
func TestStallAfterSetsTheGiveUpWindow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, loop string
		stall      time.Duration
		window     time.Duration
	}{
		{"the default", "", 45 * time.Minute, reconcile.DefaultGiveUpWindow},
		{"a configured window", "[loop]\nstall_after = \"20m\"\n", 20 * time.Minute, 20 * time.Minute},
		{"a longer window", "[loop]\nstall_after = \"3h\"\n", 3 * time.Hour, 3 * time.Hour},
		{"the watchdog off", "[loop]\nstall_after = \"0\"\n", 0, reconcile.DefaultGiveUpWindow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := transitionFixture(t)
			if tc.loop != "" {
				withTopLevel(t, opts, "\n"+tc.loop)
			}
			s, _ := start(t, opts)
			if got := s.current().StallWindow(); got != tc.stall {
				t.Fatalf("the stall window is %s, want %s", got, tc.stall)
			}
			if got := s.GiveUpLimit(); got.Window != tc.window || got.Failures != reconcile.GiveUpFailures {
				t.Fatalf("the service's give-up limit is %+v, want a window of %s", got, tc.window)
			}
			if got := s.sole().controller.GiveUpLimit(); got.Window != tc.window {
				t.Fatalf("the controller's give-up window is %s, want %s", got.Window, tc.window)
			}
		})
	}
}

// A quiet unit and a quiet workstream with nothing to explain it each raise
// one stalled exit per stall; the router decides it once, and the pass that
// finds it decided raises nothing.
func TestStallWatchdogRaisesOneStalledExitPerStall(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newStallFixture(t, "", nil)
	w := f.watch("45m")

	f.quietFor(44 * time.Minute)
	if got := f.pass(t, w); len(got) != 0 {
		t.Fatalf("a unit quiet for less than the window raised %d exits", len(got))
	}

	f.quietFor(46 * time.Minute)
	got := f.pass(t, w)
	unit, ws := got[execution.TriggerStalled], got["ws.stalled"]
	if len(got) != 2 || unit.Subject != trace.UnitSubject(stallUnit) || unit.Unit != stallUnit || unit.Default != execution.TargetWait || ws.Subject != trace.FeatureSubject {
		t.Fatalf("a quiet unit and a quiet workstream raised %+v", got)
	}
	if again := f.pass(t, w); len(again) != 2 || router.OccurrenceKey(again[execution.TriggerStalled]) != router.OccurrenceKey(unit) || router.OccurrenceKey(again["ws.stalled"]) != router.OccurrenceKey(ws) {
		t.Fatalf("a second pass raised other occurrences: %+v", again)
	}
	if unit.Evidence.Facts["minutes_quiet"] != 46 || unit.Evidence.Facts["state"] != UnitImplementing {
		t.Fatalf("the unit's evidence %+v", unit.Evidence.Facts)
	}

	for _, x := range []router.Exit{unit, ws} {
		res, err := f.s.RouteExit(ctx, x)
		fixturekit.Must(t, err)
		if !res.Committed || res.Target != execution.TargetWait {
			t.Fatalf("the stalled exit of %s = %+v", x.Subject, res)
		}
	}
	if again := f.pass(t, w); len(again) != 0 {
		t.Fatalf("a stall decided by the router raised %d more exits", len(again))
	}
	outbox, err := f.repo.Outbox(fixturekit.Stream)
	fixturekit.Must(t, err)
	notices := 0
	for _, e := range outbox {
		if strings.Contains(e.Event.Body, "has made no progress for 46 minutes") {
			notices++
		}
	}
	if notices != 2 {
		t.Fatalf("the chief of staff got %d stall notices, want 2", notices)
	}
	if state, err := f.repo.Workflow(fixturekit.Stream, trace.UnitSubject(stallUnit)); err != nil || state.Value != UnitImplementing || state.Version != 1 {
		t.Fatalf("the unit is %+v: %v", state, err)
	}
	if n := len(f.jev.TransitionRequests()); n != 1 {
		t.Fatalf("Jev got %d transition requests, want 1 for the unit (the workstream has one way out)", n)
	}
}

// With the watchdog off nothing is raised however long a subject is quiet.
func TestStallWatchdogIsOffWithAWindowOfZero(t *testing.T) {
	t.Parallel()
	f := newStallFixture(t, "[loop]\nstall_after = \"0\"\n", nil)
	f.quietFor(1000 * time.Hour)
	w := stallWatch{s: f.s, cfg: f.s.current(), repository: f.repo, submit: f.log.submit}
	f.firstPassed(w)
	fixturekit.Must(t, w.Run(context.Background()))
	if got := f.log.occurrences(); len(got) != 0 {
		t.Fatalf("the watchdog is off but raised %d exits", len(got))
	}
}

// A pause explains the quiet. Lifting it starts the window over from when the
// pause was last seen, not from the progress before it.
func TestStallWatchdogIsExplainedByAPauseAndStartsOverWhenItLifts(t *testing.T) {
	t.Parallel()
	f := newStallFixture(t, "", nil)
	cfg := f.s.current()
	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{cfg.Project.ID: {fixturekit.Stream}}})
	fixturekit.Must(t, err)
	t.Cleanup(func() { store.Close() })
	f.s.store = store
	target := runtime.Target{Scope: "workstream", Project: cfg.Project.ID, Workstream: fixturekit.Stream}
	fixturekit.Must(t, store.SetPause(runtime.Pause{Target: target, Mode: "soft", Reason: "owner away", Source: runtime.PauseOwner, SetAt: fixturekit.DemoStart}))
	w := f.watch("45m")

	f.quietFor(2 * time.Hour)
	if got := f.pass(t, w); len(got) != 0 {
		t.Fatalf("a paused workstream raised %d exits", len(got))
	}
	fixturekit.Must(t, store.ClearPause(target, runtime.PauseOwner))
	f.quietFor(2*time.Hour + 30*time.Minute)
	if got := f.pass(t, w); len(got) != 0 {
		t.Fatalf("a workstream resumed 30 minutes ago raised %d exits", len(got))
	}
	f.quietFor(3 * time.Hour)
	if got := f.pass(t, w); len(got) != 2 {
		t.Fatalf("a workstream quiet for the window after it resumed raised %d exits, want 2", len(got))
	}
}

// A chief of staff's move of the unit is no progress: the stall is still counted
// from the progress before it.
func TestStallWatchdogCountsNoProgressOfTheChiefOfStaff(t *testing.T) {
	t.Parallel()
	f := newStallFixture(t, "", nil)
	f.quietFor(40 * time.Minute)
	f.transition(t, "stall-chief-move", trace.UnitSubject(stallUnit), UnitImplementing, UnitChecking, stallUnit, chiefActor, f.clock.Now())
	f.quietFor(46 * time.Minute)
	got := f.pass(t, f.watch("45m"))
	unit := got[execution.TriggerStalled]
	if len(got) != 2 || unit.From != UnitChecking || unit.Evidence.Facts["last_progress"] != fixturekit.DemoStart.UTC().Format(time.RFC3339) {
		t.Fatalf("the chief's move reset the clock: %+v", got)
	}

	f.transition(t, "stall-owner-move", trace.UnitSubject(stallUnit), UnitChecking, UnitReviewing, stallUnit, ownerActor, f.clock.Now())
	if got := f.pass(t, f.watch("45m")); len(got) != 0 {
		t.Fatalf("a unit the owner moved a moment ago raised %d exits", len(got))
	}
}

func (f *stallFixture) queueOperation(t *testing.T, action string, at time.Time) trace.OperationRecord {
	t.Helper()
	id := "stall-" + action
	event := trace.EventID(id, "operation")
	op := coreadapter.Operation{ID: trace.OperationID(f.repo.Project(), fixturekit.Stream, event), Boundary: coreadapter.RepositoryBoundary, Action: action, Input: json.RawMessage(fmt.Sprintf(`{"unit":%q}`, stallUnit))}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: f.repo.Project(), Workstream: fixturekit.Stream, Unit: stallUnit, At: at, Actor: stallSeeder, Cause: "seed"}
	_, err := f.repo.Transact(context.Background(), trace.Transaction{Transition: trace.Transition{Header: h, Subject: id, To: "queued", Reason: "seeded"},
		Events: []trace.Event{{ID: event, Kind: "operation", Body: action, Operation: &op}}})
	fixturekit.Must(t, err)
	records, err := f.repo.Operations(fixturekit.Stream)
	fixturekit.Must(t, err)
	for _, r := range records {
		if r.Operation.ID == op.ID {
			return r
		}
	}
	t.Fatal("the operation was not recorded")
	return trace.OperationRecord{}
}

func (f *stallFixture) fail(t *testing.T, record trace.OperationRecord, reason string) {
	t.Helper()
	a, _, err := f.repo.BeginOperation(context.Background(), fixturekit.Stream, record.EventID, trace.WorkflowOperation, trace.Actor{Kind: "service", ID: "stall-test"}, f.clock.Now)
	fixturekit.Must(t, err)
	if a == nil {
		t.Fatal("the operation was not due")
	}
	defer a.Close()
	retry := a.Action("retry", f.clock.Now())
	retry.Failure, retry.RetryAt = reason, f.clock.Now().Add(time.Minute)
	fixturekit.Must(t, a.Record(context.Background(), retry))
}

func TestStallWatchdogCitesTheLastFailureOfAnOperationThatCannotEnd(t *testing.T) {
	t.Parallel()
	f := newStallFixture(t, "", nil)
	w := f.watch("45m")

	f.quietFor(2 * time.Hour)
	f.queueOperation(t, LandAction, f.clock.Now())
	if got := f.pass(t, w); len(got) != 0 {
		t.Fatalf("an operation in flight did not explain the quiet: %d exits", len(got))
	}

	f.quietFor(2*time.Hour + 50*time.Minute)
	got := f.pass(t, w)
	if len(got) != 2 || len(got[execution.TriggerStalled].Evidence.Records) != 1 {
		t.Fatalf("an operation with no attempt in the window explained the quiet: %+v", got)
	}

	reason := "Clone recovery failure: restoring the checkout timed out after 5m"
	limit := f.s.GiveUpLimit().Failures
	young := newStallFixture(t, "", nil)
	young.quietFor(2 * time.Hour)
	failing := young.queueOperation(t, "rebase-unit", young.clock.Now())
	for range limit - 1 {
		young.fail(t, failing, reason)
		young.clock.Advance(2 * time.Minute)
	}
	if got := young.pass(t, young.watch("45m")); len(got) != 0 {
		t.Fatalf("an operation below the give-up limit and attempted a moment ago did not explain the quiet: %d exits", len(got))
	}

	rebase := f.queueOperation(t, "rebase-unit", f.clock.Now())
	for range limit {
		f.fail(t, rebase, reason)
		f.clock.Advance(2 * time.Minute)
	}
	unit := f.pass(t, w)[execution.TriggerStalled]
	if unit.Subject == "" {
		t.Fatal("an operation past the give-up limit explained the quiet")
	}
	var cited bool
	for _, r := range unit.Evidence.Records {
		cited = cited || strings.Contains(r.Text, reason) && strings.Contains(r.Text, fmt.Sprintf("%d consecutive failures", limit))
	}
	if !cited {
		t.Fatalf("the exit does not cite the operation's last failure: %+v", unit.Evidence.Records)
	}
	effect, err := unit.Enter(context.Background(), router.Plan{Exit: unit})
	fixturekit.Must(t, err)
	if len(effect.Events) != 1 || !strings.Contains(effect.Events[0].Body, reason) {
		t.Fatalf("the notice does not carry the last failure: %+v", effect.Events)
	}
}

// The facts of an operation: failures, the latest attempt and the last failure.
func TestOperationFactsReadAttemptsAndTheLastRecoveryFailure(t *testing.T) {
	t.Parallel()
	queued := fixturekit.DemoStart
	claim := trace.OperationAction{Kind: "claim", At: queued.Add(time.Minute)}
	retry := func(at time.Duration, failure string) trace.OperationAction {
		return trace.OperationAction{Kind: "retry", At: queued.Add(at), Failure: failure}
	}
	rec := trace.OperationRecord{Operation: coreadapter.Operation{Action: LandAction}, Transition: trace.Header{At: queued}}
	if got := operationFacts(rec, scheduler.NotHeld); got.Failures != 0 || !got.LastAttempt.Equal(queued) || got.LastFailure != "" {
		t.Fatalf("a queued operation = %+v", got)
	}
	rec.History = []trace.OperationAction{claim, retry(2*time.Minute, "git: remote hung up"), retry(10*time.Minute, "Clone recovery failure: restore timed out"), {Kind: "claim", At: queued.Add(20 * time.Minute)}}
	got := operationFacts(rec, scheduler.HeldByOther)
	if got.Failures != 2 || !got.LastAttempt.Equal(queued.Add(20*time.Minute)) || got.LastFailure != "Clone recovery failure: restore timed out" || got.Held != scheduler.HeldByOther || got.Action != LandAction {
		t.Fatalf("an operation retried twice = %+v", got)
	}
}

// A hold is an explanation only when its own cause is a pause, an open owner
// card or a live dependency wait.
func TestHoldCauseNamesWhyAnOperationIsHeld(t *testing.T) {
	t.Parallel()
	f := newStallFixture(t, "", nil)
	cfg := f.s.current()
	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{cfg.Project.ID: {fixturekit.Stream}}})
	fixturekit.Must(t, err)
	t.Cleanup(func() { store.Close() })
	f.s.store = store
	cause := f.s.holdingCause(f.repo)
	land := coreadapter.Operation{Boundary: coreadapter.RepositoryBoundary, Action: LandAction}
	publish := coreadapter.Operation{Boundary: coreadapter.RepositoryBoundary, Action: PublishAction}

	if got := cause(fixturekit.Stream, land); got != scheduler.NotHeld {
		t.Fatalf("a landing nothing holds is held by %q", got)
	}
	if got := cause(fixturekit.Stream, publish); got != scheduler.HeldByOther {
		t.Fatalf("a publication without a credential is held by %q, want %q", got, scheduler.HeldByOther)
	}
	target := runtime.Target{Scope: "workstream", Project: cfg.Project.ID, Workstream: fixturekit.Stream}
	fixturekit.Must(t, store.SetPause(runtime.Pause{Target: target, Mode: "soft", Reason: "owner away", Source: runtime.PauseOwner, SetAt: fixturekit.DemoStart}))
	if got := cause(fixturekit.Stream, land); got != scheduler.HeldByPause {
		t.Fatalf("a landing held by a pause is held by %q", got)
	}
	if !f.s.holding(f.repo)(fixturekit.Stream, land) {
		t.Fatal("holding does not hold a landing a pause holds")
	}
}

// A router hold is no progress: it stands for the window, and then the subject
// stalls again.
func TestAnUnresolvedHoldBecomesAStallAfterTheWindow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &jevfixture.Transitions{Script: func(a jevfixture.Ask) jevfixture.Reply {
		r := jevfixture.DefaultFits(a)
		r.PickA, r.PickB = execution.UnitImplementing, execution.TargetWait
		return r
	}}
	f := newStallFixture(t, "", p)
	w := f.watch("45m")
	f.quietFor(46 * time.Minute)
	first := f.pass(t, w)[execution.TriggerStalled]
	res, err := f.s.RouteExit(ctx, first)
	fixturekit.Must(t, err)
	if res.Committed || res.Outcome != router.OutcomeHeld || res.Hold != router.HoldOrdersDisagree {
		t.Fatalf("the stalled exit = %+v, want a hold", res)
	}
	if got := f.pass(t, w); len(got) != 1 {
		t.Fatalf("a hold of the unit raised %d exits at once, want only the workstream's: %+v", len(got), got)
	}

	f.quietFor(46*time.Minute + 30*time.Minute)
	if got := f.pass(t, w); got[execution.TriggerStalled].Subject != "" {
		t.Fatalf("a held unit stalled inside the window of its hold: %+v", got)
	}
	f.quietFor(46*time.Minute + 50*time.Minute)
	second := f.pass(t, w)[execution.TriggerStalled]
	if second.Subject == "" || router.OccurrenceKey(second) == router.OccurrenceKey(first) {
		t.Fatalf("a hold that stood for the window did not stall the unit again: %+v", second)
	}
}

// The watchdog hands its exits to the router's worker; it never asks Jev on the
// scheduling path.
func TestStallWatchdogNeverAsksJevOnTheSchedulingPath(t *testing.T) {
	t.Parallel()
	f := newStallFixture(t, "", nil)
	f.quietFor(3 * time.Hour)
	w := f.watch("45m")
	f.firstPassed(w)
	ctx := router.OnSchedulingPath(context.Background())
	fixturekit.Must(t, w.Run(ctx))
	if got := f.log.occurrences(); len(got) != 2 {
		t.Fatalf("the watchdog raised %d exits", len(got))
	}
	if n := len(f.jev.Requests()); n != 0 {
		t.Fatalf("Jev got %d requests on the scheduling path", n)
	}
	for _, x := range f.log.occurrences() {
		if _, err := f.s.RouteExit(ctx, x); err != router.ErrSchedulingPath {
			t.Fatalf("routing on the scheduling path = %v", err)
		}
	}
}

func TestStalledExitsOfferTheDefaultAndTheWaysToStartTheStageAgain(t *testing.T) {
	t.Parallel()
	for _, state := range []string{UnitImplementing, UnitWaiting, UnitChecking, UnitReviewing, UnitApproved, UnitContested} {
		if got := execution.StalledTargets(state); len(got) != 4 || got[0] != execution.TargetWait {
			t.Fatalf("a stalled unit in %s can go to %v", state, got)
		}
	}
	for _, state := range []string{UnitPlanned, UnitReady} {
		if got := execution.StalledTargets(state); len(got) != 1 || got[0] != execution.TargetWait {
			t.Fatalf("a stalled unit in %s can go to %v", state, got)
		}
	}

	f := newStallFixture(t, "", nil)
	for _, tc := range []struct {
		state string
		want  []string
	}{{AssembledState, []string{"wait", "assembled"}}, {BuildingState, []string{"wait"}}} {
		x := f.s.lifecycle().StalledExit(f.repo, fixturekit.Stream, trace.WorkflowState{Version: 3, Value: tc.state}, lifecycle.StalledInput{Cause: "stalled-test", Actor: stallActor, Notice: "quiet", Reason: "stalled"})
		var names []string
		for _, target := range x.Targets {
			names = append(names, target.Name)
			if target.Gate || target.Approval || !target.NoMove {
				t.Fatalf("the %s target of a stalled %s workstream: %+v", target.Name, tc.state, target)
			}
		}
		if x.Default != "wait" || x.Trigger != "ws.stalled" || !router.ValidTrigger(x.Trigger) || strings.Join(names, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("the stalled exit of a %s workstream: default %q targets %v", tc.state, x.Default, names)
		}
		if tc.state == AssembledState && x.Targets[1].Bar != router.BarModest {
			t.Fatalf("a fresh final review is a departure at %q", x.Targets[1].Bar)
		}
	}
}

func TestAStallHeldAgainAndAgainEndsOnTheRecoveryCard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &jevfixture.Transitions{Script: func(a jevfixture.Ask) jevfixture.Reply {
		r := jevfixture.DefaultFits(a)
		r.PickA, r.PickB = execution.UnitImplementing, execution.TargetWait
		return r
	}}
	f := newStallFixture(t, "", p)
	w := f.watch("45m")
	round := func(at time.Duration) map[string]router.Exit {
		t.Helper()
		f.quietFor(at)
		got := f.pass(t, w)
		for _, x := range got {
			if _, err := f.s.RouteExit(ctx, x); err != nil {
				t.Fatal(err)
			}
		}
		return got
	}
	held := func() int {
		t.Helper()
		n := 0
		for _, rec := range routeRecordsOf(t, f.repo, fixturekit.Stream) {
			if rec.Trigger == execution.TriggerStalled && rec.State == router.StateHeld {
				n++
			}
		}
		return n
	}

	if round(46 * time.Minute)[execution.TriggerStalled].Subject == "" || held() != 1 {
		t.Fatalf("the first stall was not held: %d held", held())
	}
	if round(96 * time.Minute)[execution.TriggerStalled].Subject == "" || held() != 2 {
		t.Fatalf("the stall after the first hold was not held: %d held", held())
	}
	if cards := recoveryEntriesOf(t, f.s, f.repo, fixturekit.Stream); len(cards) != 0 {
		t.Fatalf("a stall held once has %d cards", len(cards))
	}

	asked := len(f.jev.TransitionRequests())
	if got := round(146 * time.Minute); got[execution.TriggerStalled].Subject != "" {
		t.Fatalf("a stall held twice was raised a third time: %+v", got)
	}
	if n := len(f.jev.TransitionRequests()); n != asked || held() != 2 {
		t.Fatalf("Jev was asked %d more times and %d stalls are held", n-asked, held())
	}
	cards := recoveryEntriesOf(t, f.s, f.repo, fixturekit.Stream)
	if len(cards) != 1 {
		t.Fatalf("the stalled unit has %d recovery cards, want 1: %+v", len(cards), cards)
	}
	m := cards[0].Recovery
	if m == nil || m.Origin != api.RecoveryStall || m.Subject != trace.UnitSubject(stallUnit) || m.From != UnitImplementing || m.Target != UnitImplementing || m.Machine != execution.UnitMachine {
		t.Fatalf("the card pins %+v", m)
	}
	for _, at := range []time.Duration{150 * time.Minute, 4 * time.Hour} {
		round(at)
		if got := recoveryEntriesOf(t, f.s, f.repo, fixturekit.Stream); len(got) != 1 || recoveryDocuments(t, f.repo, fixturekit.Stream) != 1 || held() != 2 {
			t.Fatalf("later passes made %d cards and %d records and hold %d stalls", len(got), recoveryDocuments(t, f.repo, fixturekit.Stream), held())
		}
	}
}

func (f *stallFixture) transitionOn(t *testing.T, ws config.WorkstreamID, id, subject, from, to, unit string, at time.Time) {
	t.Helper()
	state, err := f.repo.RawWorkflow(ws, subject)
	fixturekit.Must(t, err)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: f.repo.Project(), Workstream: ws, Unit: unit, At: at, Actor: stallSeeder, Cause: "seed"}
	_, err = f.repo.Transact(context.Background(), trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: subject, From: from, To: to, Reason: "seeded"}})
	fixturekit.Must(t, err)
}

func (f *stallFixture) exitsOf(ws config.WorkstreamID, subject string) []router.Exit {
	var out []router.Exit
	for _, x := range f.log.occurrences() {
		if x.Workstream == ws && x.Subject == subject {
			out = append(out, x)
		}
	}
	return out
}

func (f *stallFixture) queueTurn(t *testing.T, unit string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	h := trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, Revision: 1, ID: "mason_stall", Project: f.repo.Project(), Workstream: fixturekit.Stream, At: at, Actor: stallSeeder, Cause: "seed"}
	fixturekit.Must(t, f.repo.CreateThread(ctx, trace.Agent{Header: h, Role: "mason", ThreadID: "mason_stall_thread"}))
	h.Schema, h.ID, h.Unit = "osmia.trace.turn-request", "request_stall", unit
	_, err := f.repo.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: "mason_stall", ThreadID: "mason_stall_thread", TurnID: "stall-turn", Profile: coreadapter.Profile{Name: "mason-default", Backend: "fake", Model: "test"}, Prompt: "Build it"})
	fixturekit.Must(t, err)
}

func TestABuildingWorkstreamWithEveryUnitMergedHasAWayOutOfItsStall(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newStallFixture(t, "", nil)
	f.sealWorkstream(t, "# Spec\n", "")
	f.transition(t, "stall-unit-merged", trace.UnitSubject(stallUnit), UnitImplementing, UnitMerged, stallUnit, stallSeeder, fixturekit.DemoStart)

	feature, err := f.repo.Workflow(fixturekit.Stream, trace.FeatureSubject)
	fixturekit.Must(t, err)
	x := f.s.lifecycle().StalledExit(f.repo, fixturekit.Stream, feature, lifecycle.StalledInput{Cause: "stalled-test", Actor: stallActor, Notice: "quiet", Reason: "stalled"})
	if len(x.Targets) != 2 || x.Targets[1].Name != AssembledState || x.Targets[1].NoMove || x.Targets[1].Bar != router.BarModest || x.Targets[1].Gate || x.Targets[1].Approval || x.Default != intake.TargetWait {
		t.Fatalf("the stalled exit of a building workstream with nothing left to build: default %q targets %+v", x.Default, x.Targets)
	}

	w := f.watch("45m")
	f.quietFor(46 * time.Minute)
	got := f.pass(t, w)
	ws := got[intake.TriggerStalled]
	if len(got) != 1 || ws.Subject != trace.FeatureSubject || ws.From != BuildingState {
		t.Fatalf("the quiet workstream raised %+v", got)
	}
	if res, err := f.s.RouteExit(ctx, ws); err != nil || !res.Committed || res.Target != intake.TargetWait {
		t.Fatalf("the stalled exit = %+v, %v", res, err)
	}

	f.quietFor(46*time.Minute + 50*time.Minute)
	f.pass(t, w)
	cards := recoveryEntriesOf(t, f.s, f.repo, fixturekit.Stream)
	if len(cards) != 1 {
		t.Fatalf("the stalled workstream has %d recovery cards, want 1", len(cards))
	}
	m := cards[0].Recovery
	if m == nil || m.Unit != "" || m.Subject != trace.FeatureSubject || m.From != BuildingState || m.Target != AssembledState || m.Origin != api.RecoveryStall {
		t.Fatalf("the card pins %+v", m)
	}
	if fail := f.s.lifecycle().Assemblable(f.repo, fixturekit.Stream); fail != nil {
		t.Fatalf("the workstream the card moves to assembled cannot be assembled: %+v", fail)
	}
	if req := recoveryRequestOf(t, cards[0]); req.Target != AssembledState || req.From != BuildingState || req.Subject != trace.FeatureSubject {
		t.Fatalf("the card's answer pins %+v", req)
	}

	open := newStallFixture(t, "", nil)
	open.sealWorkstream(t, "# Spec\n", "")
	feature, err = open.repo.Workflow(fixturekit.Stream, trace.FeatureSubject)
	fixturekit.Must(t, err)
	if x := open.s.lifecycle().StalledExit(open.repo, fixturekit.Stream, feature, lifecycle.StalledInput{Cause: "stalled-test", Actor: stallActor, Notice: "quiet", Reason: "stalled"}); len(x.Targets) != 1 {
		t.Fatalf("a building workstream with an open unit can go to %+v", x.Targets)
	}
}

// A queued turn explains the quiet only while it is younger than the window: a
// turn that sits in its queue for the window is a stall.
func TestAQueuedTurnOlderThanTheWindowDoesNotExplainTheQuiet(t *testing.T) {
	t.Parallel()
	f := newStallFixture(t, "", nil)
	w := f.watch("45m")
	f.quietFor(2 * time.Hour)
	f.queueTurn(t, stallUnit, f.clock.Now())
	if got := f.pass(t, w); len(got) != 0 {
		t.Fatalf("a turn queued a moment ago did not explain the quiet: %+v", got)
	}
	f.quietFor(2*time.Hour + 50*time.Minute)
	got := f.pass(t, w)
	if len(got) != 2 || got[execution.TriggerStalled].Subject != trace.UnitSubject(stallUnit) {
		t.Fatalf("a turn queued 50 minutes ago explained the quiet: %+v", got)
	}
}

// A card of the workstream that does not hold its units, such as a recovery
// card of the workstream itself, explains the workstream's quiet but not a
// unit's.
func TestAWorkstreamCardDoesNotExplainTheQuietOfItsUnits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newStallFixture(t, "", nil)
	feature, err := f.repo.RawWorkflow(fixturekit.Stream, trace.FeatureSubject)
	fixturekit.Must(t, err)
	move := api.RecoveryMove{Occurrence: "stall-test-occurrence", Machine: intake.WorkstreamMachine, Subject: trace.FeatureSubject, From: feature.Value, Version: feature.Version, Target: AssembledState, Origin: api.RecoveryOwner, Reason: "the owner's move"}
	fixturekit.Must(t, writeRecoveryRecord(ctx, f.repo, fixturekit.Stream, recoveryRecord{RecoveryMove: move, State: recoveryOpen, Opened: fixturekit.DemoStart}, 0, stallSeeder, fixturekit.DemoStart))
	if cards := recoveryEntriesOf(t, f.s, f.repo, fixturekit.Stream); len(cards) != 1 {
		t.Fatalf("the workstream has %d open cards, want 1", len(cards))
	}
	f.quietFor(46 * time.Minute)
	got := f.pass(t, f.watch("45m"))
	if len(got) != 1 || got[execution.TriggerStalled].Subject != trace.UnitSubject(stallUnit) {
		t.Fatalf("with a card of the workstream open, the pass raised %+v, want the unit's stall alone", got)
	}
}

// A drift rebase is no progress for the watchdog: a branch rebased onto
// upstream on its cadence does not start the window over.
func TestADriftRebaseIsNoProgressForTheStallWatchdog(t *testing.T) {
	t.Parallel()
	f := newStallFixture(t, "", nil)
	f.quietFor(30 * time.Minute)
	f.transition(t, "stall-drift-rebased", driftSubject, "", driftRebased+"-1", "", stallSeeder, f.clock.Now())
	f.quietFor(46 * time.Minute)
	got := f.pass(t, f.watch("45m"))
	ws := got[intake.TriggerStalled]
	if len(got) != 2 || ws.Evidence.Facts["last_progress"] != fixturekit.DemoStart.UTC().Format(time.RFC3339) {
		t.Fatalf("a drift rebase reset the window: %+v", got)
	}
}

// An upstream explains the quiet of a workstream waiting on it only while it
// progresses within the window or is explained itself; an upstream that is
// alive but stalled explains nothing.
func TestAnUpstreamExplainsItsDependentsOnlyWhileItIsNotStalled(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newStallFixture(t, "", nil)
	upstream := config.WorkstreamID("w_00000000000000000000000000000b01")
	fixturekit.Must(t, f.repo.CreateWorkstream(ctx, upstream, fixturekit.DemoStart, ownerActor))
	f.transitionOn(t, upstream, "stall-upstream-building", trace.FeatureSubject, "", BuildingState, "", fixturekit.DemoStart)
	f.transitionOn(t, upstream, "stall-upstream-unit", trace.UnitSubject("u9"), "", UnitImplementing, "u9", fixturekit.DemoStart.Add(40*time.Minute))
	judge := func() bool {
		t.Helper()
		statuses, err := f.repo.Statuses()
		fixturekit.Must(t, err)
		w := f.watch("45m")
		p := &stallPass{statuses: statuses, after: w.cfg.StallWindow(), now: f.clock.Now(), limit: f.s.GiveUpLimit().Failures, upstreams: map[config.WorkstreamID]*bool{}}
		explains, err := w.upstreamExplains(ctx, p, upstream)
		fixturekit.Must(t, err)
		return explains
	}

	f.quietFor(46 * time.Minute)
	if !judge() {
		t.Fatal("an upstream that progressed 6 minutes ago did not explain its dependents")
	}
	f.quietFor(90 * time.Minute)
	if judge() {
		t.Fatal("an upstream quiet for 50 minutes with nothing to explain it explained its dependents")
	}
	cfg := f.s.current()
	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{cfg.Project.ID: {fixturekit.Stream, upstream}}})
	fixturekit.Must(t, err)
	t.Cleanup(func() { store.Close() })
	f.s.store = store
	fixturekit.Must(t, store.SetPause(runtime.Pause{Target: runtime.Target{Scope: "workstream", Project: cfg.Project.ID, Workstream: upstream}, Mode: "soft", Reason: "owner away", Source: runtime.PauseOwner, SetAt: fixturekit.DemoStart}))
	if !judge() {
		t.Fatal("a paused upstream did not explain its dependents")
	}
}

func (f *stallFixture) seedChange(t *testing.T, c lifecycle.WorkgroupChange, revision int, at time.Time) {
	t.Helper()
	data, err := json.Marshal(c)
	fixturekit.Must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: lifecycle.WorkgroupChangeDocument(c.ID), Revision: revision, Project: f.repo.Project(), Workstream: fixturekit.Stream, At: at, Actor: stallSeeder, Cause: "seed"}
	fixturekit.Must(t, f.repo.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: lifecycle.WorkgroupChangePath(c.ID), Content: string(data)}}))
}

func TestAWorkgroupChangeSittingInItsDraftStallsAndEndsOnARecoveryCard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newStallFixture(t, "", nil)
	change := lifecycle.WorkgroupChange{ID: "c1", Workgroup: "wg", Host: fixturekit.Stream, Request: lifecycle.ChangeRequest{Key: "c1", Kind: "amend", Scope: "workgroup", Change: "Rename the field"}, State: "drafting", Failure: "member history is unavailable"}
	f.seedChange(t, change, 1, fixturekit.DemoStart)
	subject := lifecycle.WorkgroupChangeDocument("c1")
	w := f.watch("45m")

	f.quietFor(44 * time.Minute)
	f.pass(t, w)
	if got := f.exitsOf(fixturekit.Stream, subject); len(got) != 0 {
		t.Fatalf("a change drafted for 44 minutes raised %+v", got)
	}
	f.quietFor(46 * time.Minute)
	f.pass(t, w)
	got := f.exitsOf(fixturekit.Stream, subject)
	if len(got) != 1 || got[0].Trigger != intake.TriggerStalled || got[0].Default != intake.TargetWait || len(got[0].Targets) != 1 {
		t.Fatalf("a change drafted for 46 minutes raised %+v", got)
	}
	x := got[0]
	if x.Evidence.Facts["state"] != "drafting" || x.Evidence.Facts["held"] != change.Failure || x.Evidence.Facts["minutes_quiet"] != 46 {
		t.Fatalf("the change's evidence %+v", x.Evidence.Facts)
	}
	if res, err := f.s.RouteExit(ctx, x); err != nil || !res.Committed || res.Target != intake.TargetWait {
		t.Fatalf("the change's stalled exit = %+v, %v", res, err)
	}
	outbox, err := f.repo.Outbox(fixturekit.Stream)
	fixturekit.Must(t, err)
	told := 0
	for _, e := range outbox {
		if strings.Contains(e.Event.Body, "Workgroup change c1 has sat drafting for 46 minutes") && strings.Contains(e.Event.Body, change.Failure) {
			told++
		}
	}
	if told != 1 {
		t.Fatalf("the chief of staff was told of the change %d times, want 1", told)
	}

	f.quietFor(46*time.Minute + 50*time.Minute)
	f.pass(t, w)
	if again := f.exitsOf(fixturekit.Stream, subject); len(again) != 0 {
		t.Fatalf("a decided stall of the change was raised again: %+v", again)
	}
	var card *api.RecoveryMove
	for _, e := range recoveryEntriesOf(t, f.s, f.repo, fixturekit.Stream) {
		if e.Recovery != nil && e.Recovery.Subject == subject {
			card = e.Recovery
		}
	}
	if card == nil || card.From != "drafting" || card.Version != 1 || card.Target != "failed" || card.Origin != api.RecoveryStall {
		t.Fatalf("the change's card pins %+v", card)
	}

	change.State = "debating"
	f.seedChange(t, change, 2, f.clock.Now())
	for _, e := range recoveryEntriesOf(t, f.s, f.repo, fixturekit.Stream) {
		if e.Recovery != nil && e.Recovery.Subject == subject {
			t.Fatalf("a change that moved on still has its card: %+v", e.Recovery)
		}
	}

	presented := newStallFixture(t, "", nil)
	presented.seedChange(t, lifecycle.WorkgroupChange{ID: "c2", Workgroup: "wg", Host: fixturekit.Stream, State: "presented"}, 1, fixturekit.DemoStart)
	presented.quietFor(3 * time.Hour)
	presented.pass(t, presented.watch("45m"))
	if got := presented.exitsOf(fixturekit.Stream, lifecycle.WorkgroupChangeDocument("c2")); len(got) != 0 {
		t.Fatalf("a presented change raised %+v", got)
	}
}
