package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

func probeExit(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, cause string) router.Exit {
	t.Helper()
	state, err := repository.Workflow(stream, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	return router.Exit{
		Repo: repository, Project: f.project, Workstream: stream, Unit: "resume", Machine: "probe",
		Subject: trace.UnitSubject("resume"), From: state.Value, Version: state.Version,
		Trigger: "unit.checks_failed", Cause: cause, Actor: foremanActor, At: f.clock.Now(), Default: "keep",
		Targets: []router.Target{
			{Name: "keep", Description: "Leave the unit where it is.", NoMove: true},
			{Name: "work", Description: "Start more work on the unit.", Works: true, NoMove: true, Bar: router.BarModest},
		},
		Evidence: router.Evidence{Intent: "The unit resumes from the last chunk."},
	}
}

func routeProbe(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, p *jevfixture.Transitions, cause string) router.Result {
	t.Helper()
	f.s.router.Judge = jevfixture.TransitionJudge(f.clock.Now, p)
	f.clock.Mu.Lock()
	f.clock.At = f.clock.At.Add(time.Minute)
	f.clock.Mu.Unlock()
	res, err := f.s.RouteExit(context.Background(), probeExit(t, f, repository, stream, cause))
	fixturekit.Must(t, err)
	return res
}

func sessionsOf(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) int {
	t.Helper()
	since, _, err := lastProgress(repository, stream)
	fixturekit.Must(t, err)
	costs, err := trace.Read[trace.Cost](repository, stream)
	fixturekit.Must(t, err)
	return sessionsSince(costs, stream, since)
}

func workstreamTarget(f *shedFixture, stream config.WorkstreamID) runtime.Target {
	return runtime.Target{Scope: "workstream", Project: f.project, Workstream: stream}
}

// Every request the router sends Jev is a session of the workstream: the moves
// Jev routes count toward the loop guard, which pauses the workstream at
// loop.max_sessions however the answers go.
func TestJevRoutedMovesCountTowardTheLoopGuard(t *testing.T) {
	t.Parallel()
	f, stream, repository := newReviewFixture(t, "loop-guard-jev")
	openRuntime(t, f, stream)
	g := loopGuard{s: f.s, cfg: f.s.cfg, repository: repository}
	ctx := context.Background()
	base := sessionsOf(t, repository, stream)
	f.s.cfg.Loop.MaxSessions = base + 3
	p := &jevfixture.Transitions{}

	for i := 1; i <= 2; i++ {
		res := routeProbe(t, f, repository, stream, p, fmt.Sprintf("probe-%d", i))
		if !res.Committed || res.Outcome != router.OutcomeFollowed || sessionsOf(t, repository, stream) != base+i {
			t.Fatalf("exit %d: result %+v, %d sessions counted", i, res, sessionsOf(t, repository, stream)-base)
		}
	}
	fixturekit.Must(t, g.Pass(ctx))
	if _, paused := loopPause(f, stream); paused {
		t.Fatal("the loop guard paused the workstream below the limit")
	}
	routeProbe(t, f, repository, stream, p, "probe-3")
	if n := sessionsOf(t, repository, stream) - base; n != 3 {
		t.Fatalf("%d sessions counted for three routed exits", n)
	}
	fixturekit.Must(t, g.Pass(ctx))
	pause, paused := loopPause(f, stream)
	if !paused || !strings.Contains(pause.Reason, fmt.Sprintf("reaching loop.max_sessions (%d)", base+3)) {
		t.Fatalf("the loop guard's pause %+v %v", pause, paused)
	}
}

func TestRoutedExitsThatMoveNothingAreNoProgress(t *testing.T) {
	t.Parallel()
	f, stream, repository := newReviewFixture(t, "loop-guard-no-progress")
	before, what, err := lastProgress(repository, stream)
	fixturekit.Must(t, err)
	held := &jevfixture.Transitions{Script: func(a jevfixture.Ask) jevfixture.Reply { return jevfixture.Departs(a, "work", 0.55, 0.9) }}
	if res := routeProbe(t, f, repository, stream, held, "probe-held"); res.Committed || res.Outcome != router.OutcomeHeld {
		t.Fatalf("result %+v", res)
	}
	if res := routeProbe(t, f, repository, stream, &jevfixture.Transitions{}, "probe-followed"); !res.Committed || res.Outcome != router.OutcomeFollowed {
		t.Fatalf("result %+v", res)
	}
	after, now, err := lastProgress(repository, stream)
	fixturekit.Must(t, err)
	if !after.Equal(before) || now != what {
		t.Fatalf("a routed exit that moved nothing counted as progress: %s %q, was %s %q", after, now, before, what)
	}
}

// A pause of the daily budget stops the move Jev would make: the target that
// starts work is not offered, Jev is not asked to choose, and the default
// stands. The same Jev, with no pause, departs to it.
func TestAnExhaustedBudgetStopsAJevPickedMove(t *testing.T) {
	t.Parallel()
	departs := func() *jevfixture.Transitions {
		return &jevfixture.Transitions{Script: func(a jevfixture.Ask) jevfixture.Reply { return jevfixture.Departs(a, "work", 0.95, 0.95) }}
	}

	t.Run("without a pause Jev departs", func(t *testing.T) {
		t.Parallel()
		f, stream, repository := newReviewFixture(t, "budget-control")
		openRuntime(t, f, stream)
		res := routeProbe(t, f, repository, stream, departs(), "probe-control")
		if !res.Committed || res.Outcome != router.OutcomeDeparted || res.Target != "work" {
			t.Fatalf("result %+v", res)
		}
	})

	t.Run("with the daily budget spent the move is not offered", func(t *testing.T) {
		t.Parallel()
		f, stream, repository := newReviewFixture(t, "budget-spent")
		openRuntime(t, f, stream)
		fixturekit.Must(t, f.s.store.SetPause(runtime.Pause{Target: workstreamTarget(f, stream), Mode: "soft", Reason: "the daily budget is spent", Source: runtime.PauseDailyBudget, SetAt: f.clock.Now()}))
		p := departs()
		res := routeProbe(t, f, repository, stream, p, "probe-spent")
		if !res.Committed || res.Target != "keep" || res.Outcome != router.OutcomeFollowed || len(p.TransitionRequests()) != 0 {
			t.Fatalf("result %+v, %d requests", res, len(p.TransitionRequests()))
		}
	})

	t.Run("a budget spent while Jev answers refuses the pick", func(t *testing.T) {
		t.Parallel()
		f, stream, repository := newReviewFixture(t, "budget-spent-late")
		openRuntime(t, f, stream)
		p := departs()
		p.Wait = func(context.Context) error {
			return f.s.store.SetPause(runtime.Pause{Target: workstreamTarget(f, stream), Mode: "soft", Reason: "the daily budget is spent", Source: runtime.PauseDailyBudget, SetAt: f.clock.Now()})
		}
		res := routeProbe(t, f, repository, stream, p, "probe-late")
		if res.Committed || res.Journal != router.StateRefused || res.Outcome != router.OutcomeHeld {
			t.Fatalf("result %+v", res)
		}
		var notices []string
		outbox, err := repository.Outbox(stream)
		fixturekit.Must(t, err)
		for _, e := range outbox {
			if strings.HasPrefix(e.TransitionID, "route-") && strings.HasSuffix(e.TransitionID, "-refused") {
				notices = append(notices, e.Event.Body)
			}
		}
		if len(notices) != 1 || !strings.Contains(notices[0], "the daily budget is spent") {
			t.Fatalf("notices %q", notices)
		}
	})
}

// The loop guard's limit stops the move too, before its pass has paused the
// workstream: once loop.max_sessions sessions ran since the last progress, a
// target that starts work is not offered.
func TestTheLoopGuardsLimitStopsAJevPickedMoveBeforeItsPass(t *testing.T) {
	t.Parallel()
	f, stream, repository := newReviewFixture(t, "loop-guard-stops")
	openRuntime(t, f, stream)
	f.s.cfg.Loop.MaxSessions = sessionsOf(t, repository, stream) + 2
	recordSessions(t, f, repository, stream, 2)
	p := &jevfixture.Transitions{Script: func(a jevfixture.Ask) jevfixture.Reply { return jevfixture.Departs(a, "work", 0.95, 0.95) }}
	res := routeProbe(t, f, repository, stream, p, "probe-loop")
	if !res.Committed || res.Target != "keep" || res.Outcome != router.OutcomeFollowed || len(p.TransitionRequests()) != 0 {
		t.Fatalf("result %+v, %d requests", res, len(p.TransitionRequests()))
	}
	f.s.cfg.Loop.MaxSessions = 0
	q := &jevfixture.Transitions{Script: func(a jevfixture.Ask) jevfixture.Reply { return jevfixture.Departs(a, "work", 0.95, 0.95) }}
	if res := routeProbe(t, f, repository, stream, q, "probe-lifted"); res.Target != "work" || res.Outcome != router.OutcomeDeparted {
		t.Fatalf("result %+v", res)
	}
}
