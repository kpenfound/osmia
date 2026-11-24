package execution

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/trace"
)

func publishOperation(t *testing.T, f *buildFixture, unit, action, event string) coreadapter.Operation {
	t.Helper()
	input, err := json.Marshal(map[string]any{"unit": unit})
	must(t, err)
	op := coreadapter.Operation{ID: trace.OperationID(f.repository.Project(), stream, event), Boundary: coreadapter.RepositoryBoundary, Action: action, Input: input}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: event + "-requested", Revision: 1, Project: f.repository.Project(), Workstream: stream, Unit: unit, At: f.clock.Now(), Actor: foremanActor, Cause: "test"}
	_, err = f.repository.Transact(context.Background(), trace.Transaction{Transition: trace.Transition{Header: h, Subject: "operation-test-" + event, To: "requested-1", Reason: "test"},
		Events: []trace.Event{{ID: event, Kind: action, Body: "operation", Operation: &op}}})
	must(t, err)
	return op
}

func sendBack(t *testing.T, f *buildFixture, cause, operation string) router.Exit {
	t.Helper()
	x, err := f.s.UnitExit(UnitExitInput{Repo: f.repository, Stream: stream, Unit: "resume", Trigger: TriggerChecksFailed, Cause: cause,
		Default: UnitImplementing, Targets: []string{UnitImplementing, UnitChecking, UnitContested}, Actor: ChecksActor, Operation: operation,
		ID: "unit-resume-implementing-" + cause, HeaderCause: cause, Reason: "check run 1 failed",
		Events: []trace.Event{trace.Notice("unit-resume-implementing-"+cause, "unit", "Unit resume checks failed.")}})
	must(t, err)
	return x
}

func operationHolding(t *testing.T, x router.Exit, names ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, name := range names {
		why, err := x.Prereq.Operations(context.Background(), unitTarget(name))
		must(t, err)
		out[name] = why
	}
	return out
}

func TestOnlyTheCompletingOperationIsExemptFromTheOperationsPrerequisite(t *testing.T) {
	t.Parallel()
	targets := []string{UnitImplementing, UnitChecking, UnitContested}

	t.Run("the completing operation", func(t *testing.T) {
		t.Parallel()
		f, _, _ := newChecksFixture(t, "operations-completing", &fakeChecks{Outcomes: passing()})
		land := publishOperation(t, f, "resume", LandAction, "land-1")
		for target, why := range operationHolding(t, sendBack(t, f, "completing", land.ID), targets...) {
			if why != "" {
				t.Fatalf("the completing landing holds %s: %s", target, why)
			}
		}
	})

	t.Run("another operation", func(t *testing.T) {
		t.Parallel()
		f, _, _ := newChecksFixture(t, "operations-other", &fakeChecks{Outcomes: passing()})
		land := publishOperation(t, f, "resume", LandAction, "land-1")
		rebase := publishOperation(t, f, "resume", RebaseAction, "rebase-1")
		for target, why := range operationHolding(t, sendBack(t, f, "other-rebase", land.ID), targets...) {
			if !strings.Contains(why, "rebasing unit resume") {
				t.Fatalf("a rebase in flight holds %s with %q", target, why)
			}
		}
		for target, why := range operationHolding(t, sendBack(t, f, "other-land", rebase.ID), targets...) {
			if !strings.Contains(why, "landing unit resume") {
				t.Fatalf("a landing in flight holds %s with %q", target, why)
			}
		}
		for target, why := range operationHolding(t, sendBack(t, f, "no-operation", ""), targets...) {
			if why == "" {
				t.Fatalf("an exit that names no operation is not held at %s", target)
			}
		}
	})

	t.Run("an operation of another unit", func(t *testing.T) {
		t.Parallel()
		f, _, _ := newChecksFixture(t, "operations-elsewhere", &fakeChecks{Outcomes: passing()})
		publishOperation(t, f, "dedupe", LandAction, "land-1")
		for target, why := range operationHolding(t, sendBack(t, f, "elsewhere", ""), targets...) {
			if why != "" {
				t.Fatalf("a landing of another unit holds %s: %s", target, why)
			}
		}
	})
}

func TestADefaultWithEntryEffectsWaitsForAnotherOperation(t *testing.T) {
	t.Parallel()
	f, stream, repo := newChecksFixture(t, "operations-routed", &fakeChecks{Outcomes: passing()})
	routeWith(f.s, f.clock.Now, f.jev)
	publishOperation(t, f, "resume", RebaseAction, "rebase-1")
	res, err := f.s.Router.Route(context.Background(), sendBack(t, f, "routed", ""))
	must(t, err)
	if res.Committed || res.Hold != router.HoldDefaultIllegal || unitState(t, repo, stream, "resume") != UnitChecking {
		t.Fatalf("result %+v, the unit is %s", res, unitState(t, repo, stream, "resume"))
	}
}

func TestTheUnitsLimitsPrerequisiteStopsTheTargetsThatStartWork(t *testing.T) {
	t.Parallel()
	const why = "the loop guard: 12 agent sessions ran since the workstream's last progress"
	f, _, _ := newChecksFixture(t, "limits-prerequisite", &fakeChecks{Outcomes: passing()})
	x := sendBack(t, f, "limits", "")
	limits := func(name string) string {
		t.Helper()
		got, err := x.Prereq.Limits(context.Background(), unitTarget(name))
		must(t, err)
		return got
	}
	for _, name := range []string{UnitImplementing, UnitChecking, UnitReviewing, UnitContested} {
		if got := limits(name); got != "" {
			t.Fatalf("with no limit reached %s is held: %s", name, got)
		}
	}
	f.s.Limits = func(context.Context, router.Exit) (string, error) { return why, nil }
	for _, name := range []string{UnitImplementing, UnitReviewing} {
		if got := limits(name); got != why {
			t.Fatalf("with the limit reached %s is held by %q", name, got)
		}
	}
	for _, name := range []string{UnitChecking, UnitContested, UnitApproved} {
		if got := limits(name); got != "" {
			t.Fatalf("%s starts no agent work and is held: %s", name, got)
		}
	}
}

func TestEveryWayOfReachingAUnitTargetMeetsTheLimits(t *testing.T) {
	t.Parallel()
	const why = "work is paused (daily-budget): the daily budget is spent"
	ownerMove := func(t *testing.T, f *buildFixture, cause string) router.Exit {
		t.Helper()
		x, err := f.s.UnitExit(UnitExitInput{Repo: f.repository, Stream: stream, Unit: "resume", Trigger: TriggerMoveOwner, Cause: cause, Default: UnitReviewing,
			Actor: ownerActor, Active: true, Note: "Review it.", Authority: &router.Authority{Kind: router.AuthorityOwner, Target: UnitReviewing}})
		must(t, err)
		return x
	}

	t.Run("a default with entry effects is held", func(t *testing.T) {
		t.Parallel()
		f, stream, repo := newChecksFixture(t, "limits-default", &fakeChecks{Outcomes: passing()})
		routeWith(f.s, f.clock.Now, f.jev)
		f.s.Limits = func(context.Context, router.Exit) (string, error) { return why, nil }
		res, err := f.s.Router.Route(context.Background(), sendBack(t, f, "limits-default", ""))
		must(t, err)
		if res.Committed || res.Hold != router.HoldDefaultIllegal || unitState(t, repo, stream, "resume") != UnitChecking {
			t.Fatalf("result %+v", res)
		}
	})

	t.Run("an owner's pinned decision is refused", func(t *testing.T) {
		t.Parallel()
		f, stream, repo := newChecksFixture(t, "limits-pinned", &fakeChecks{Outcomes: passing()})
		routeWith(f.s, f.clock.Now, f.jev)
		f.s.Limits = func(context.Context, router.Exit) (string, error) { return why, nil }
		asked := len(f.jev.TransitionRequests())
		res, err := f.s.Router.Route(context.Background(), ownerMove(t, f, "limits-pinned"))
		must(t, err)
		if res.Committed || res.Journal != router.StateRefused || res.Outcome != router.OutcomeHeld || unitState(t, repo, stream, "resume") != UnitChecking {
			t.Fatalf("result %+v", res)
		}
		if len(f.jev.TransitionRequests()) != asked {
			t.Fatal("a pinned decision asked Jev")
		}
	})

	t.Run("a pinned decision that passes is committed with its authority", func(t *testing.T) {
		t.Parallel()
		f, stream, repo := newChecksFixture(t, "limits-clear", &fakeChecks{Outcomes: passing()})
		routeWith(f.s, f.clock.Now, f.jev)
		f.s.Limits = func(context.Context, router.Exit) (string, error) { return "", nil }
		x := ownerMove(t, f, "limits-clear")
		asked := len(f.jev.TransitionRequests())
		res, err := f.s.Router.Route(context.Background(), x)
		must(t, err)
		if !res.Committed || res.Target != UnitReviewing || res.Outcome != router.OutcomeFollowed || unitState(t, repo, stream, "resume") != UnitReviewing {
			t.Fatalf("result %+v", res)
		}
		rec, err := router.Lookup(repo, stream, x)
		must(t, err)
		if rec == nil || rec.DecidedBy != trace.DecidedByOwner || rec.Authority != router.AuthorityOwner || rec.JevRequested || len(f.jev.TransitionRequests()) != asked {
			t.Fatalf("route %+v", rec)
		}
	})

	t.Run("a pick is refused when the limit is reached while Jev answers", func(t *testing.T) {
		t.Parallel()
		f, stream, repo := newChecksFixture(t, "limits-pick", &fakeChecks{Outcomes: passing()})
		routeWith(f.s, f.clock.Now, f.jev)
		reached := false
		f.s.Limits = func(context.Context, router.Exit) (string, error) {
			if reached {
				return why, nil
			}
			return "", nil
		}
		f.jev.Wait = func(context.Context) error { reached = true; return nil }
		res, err := f.s.Router.Route(context.Background(), sendBack(t, f, "limits-pick", ""))
		must(t, err)
		if res.Committed || res.Journal != router.StateRefused || res.Outcome != router.OutcomeHeld || unitState(t, repo, stream, "resume") != UnitChecking {
			t.Fatalf("result %+v", res)
		}
	})
}
