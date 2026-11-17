package execution

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/trace"
)

// contestedWithoutCandidate starts the unit resume and contests it as a mason
// contest, before its mason reported anything: the unit has no candidate.
func contestedWithoutCandidate(t *testing.T, f *buildFixture) trace.WorkflowState {
	t.Helper()
	ctx := context.Background()
	m := f.controller()
	b, found, err := m.read(stream)
	must(t, err)
	if !found {
		t.Fatal("fixture workstream is not building")
	}
	if started, _, err := m.start(ctx, b, "resume"); err != nil || !started {
		t.Fatalf("the unit did not start: %v", err)
	}
	if _, revision, err := unitReport(f.repository, stream, "resume"); err != nil || revision != 0 {
		t.Fatalf("the unit has a report: revision %d, %v", revision, err)
	}
	state, err := f.repository.Workflow(stream, trace.UnitSubject("resume"))
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "unit-resume-contested-test", Revision: 1, Project: f.repository.Project(), Workstream: stream, Unit: "resume", At: f.clock.Now(), Actor: masonActor, Cause: "turn"}
	_, err = f.repository.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: trace.UnitSubject("resume"), From: UnitImplementing, To: UnitContested, Reason: "the mason gave up"}})
	must(t, err)
	state, err = f.repository.Workflow(stream, trace.UnitSubject("resume"))
	must(t, err)
	return state
}

// A mason contest that has no candidate can still reach its revise target
// through the router: the owner's ruling is a pinned move that enters it, and a
// ruling that is the chief of staff's is judged with only the targets that need
// no candidate on offer.
func TestAMasonContestWithNoCandidateReachesItsReviseTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		trigger   string
		authority *router.Authority
		decidedBy string
	}{
		{"the owner rules", TriggerOwnerRuling, &router.Authority{Kind: router.AuthorityOwner, Target: UnitImplementing}, trace.DecidedByOwner},
		{"the chief of staff rules", TriggerChiefRuling, nil, trace.DecidedByCode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newBuildFixture(t, "mason-contest")
			state := contestedWithoutCandidate(t, f)
			x, err := f.s.UnitExit(UnitExitInput{Repo: f.repository, Stream: stream, Unit: "resume", State: state, Trigger: tc.trigger, Cause: "ruling-1",
				Default: UnitImplementing, Targets: []string{UnitImplementing, UnitReviewing}, Actor: ownerActor, Note: "Revise the unit.", Authority: tc.authority})
			must(t, err)
			res, err := f.s.Router.Route(ctx, x)
			must(t, err)
			if !res.Committed || res.State != UnitImplementing {
				t.Fatalf("result %+v", res)
			}
			got, err := f.repository.Workflow(stream, trace.UnitSubject("resume"))
			must(t, err)
			if got.Value != UnitImplementing {
				t.Fatalf("the unit is %s", got.Value)
			}
			rec, err := router.Lookup(f.repository, stream, x)
			must(t, err)
			if rec == nil || rec.DecidedBy != tc.decidedBy || rec.Default != UnitImplementing || rec.Outcome != router.OutcomeFollowed {
				t.Fatalf("route %+v", rec)
			}
			if tc.authority == nil && !slices.Equal(rec.Offered, []string{UnitImplementing}) {
				t.Fatalf("offered %v: reviewing needs a candidate", rec.Offered)
			}
		})
	}
}

// An exit a scheduling pass raises is handed to the router's worker and the pass
// goes on: Jev is asked beside the pass, never on it. The same exit raised by a
// caller that is no scheduling pass is routed in place.
func TestAnExitOnTheSchedulingPathIsHandedToTheWorker(t *testing.T) {
	t.Parallel()
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	f, stream, repo := newChecksFixture(t, "scheduling-path", &fakeChecks{Outcomes: passing()})
	asked := make(chan struct{}, 8)
	release := make(chan struct{})
	f.jev.Wait = func(ctx context.Context) error {
		asked <- struct{}{}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r := routeWith(f.s, f.clock.Now, f.jev)
	w := router.NewWorker(ctx, r)
	f.s.Router = workerRouter{r, w}

	state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	must(t, err)
	exit := func(cause string) UnitExitInput {
		return UnitExitInput{Repo: repo, Stream: stream, Unit: "resume", State: state, Trigger: TriggerChecksPassed, Cause: cause,
			Default: UnitReviewing, Targets: []string{UnitReviewing, UnitChecking}, Actor: checksActor, ID: "unit-resume-reviewing-test", HeaderCause: cause, Reason: "check run 1 passed"}
	}

	// A pass hands its exit off: it returns while Jev has not answered.
	res, err := f.s.raise(router.OnSchedulingPath(ctx), true, exit("checks-1"))
	must(t, err)
	if res.Committed {
		t.Fatalf("the pass waited for the exit: %+v", res)
	}
	select {
	case <-asked:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker never asked Jev")
	}
	if got := unitState(t, repo, stream, "resume"); got != UnitChecking {
		t.Fatalf("the unit is %s before Jev answered", got)
	}
	close(release)
	w.Wait()
	if got := unitState(t, repo, stream, "resume"); got != UnitReviewing {
		t.Fatalf("the unit is %s after the worker routed its exit", got)
	}

	// Routing on the scheduling path itself is refused: Jev is never asked there.
	if _, err := r.Route(router.OnSchedulingPath(ctx), exitFor(t, f, "checks-2")); !errors.Is(err, router.ErrSchedulingPath) {
		t.Fatalf("routing on the scheduling path: %v", err)
	}
}

func exitFor(t *testing.T, f *buildFixture, cause string) router.Exit {
	t.Helper()
	state, err := f.repository.Workflow(stream, trace.UnitSubject("resume"))
	must(t, err)
	x, err := f.s.UnitExit(UnitExitInput{Repo: f.repository, Stream: stream, Unit: "resume", State: state, Trigger: TriggerChecksPassed, Cause: cause,
		Default: UnitChecking, Targets: []string{UnitChecking, UnitReviewing}, Actor: checksActor})
	must(t, err)
	return x
}

// contestUnit moves the started unit resume, whatever state it is in, to
// contested, as a contest raised on its candidate leaves it.
func contestUnit(t *testing.T, f *buildFixture, repo *trace.Repository, id string) trace.WorkflowState {
	t.Helper()
	state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "unit-resume-contested-" + id, Revision: 1, Project: repo.Project(), Workstream: stream, Unit: "resume", At: f.clock.Now(), Actor: masonActor, Cause: "turn"}
	_, err = repo.Transact(context.Background(), trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: trace.UnitSubject("resume"), From: state.Value, To: UnitContested, Reason: "review sent the unit back too often"}})
	must(t, err)
	state, err = repo.Workflow(stream, trace.UnitSubject("resume"))
	must(t, err)
	return state
}

// A ruling of the chief of staff on a contested unit is an exit the router
// judges. With Jev finding the default fits, the ruling is entered as it was
// given and its transition records the default it took. When Jev's answers
// depart from it, the unit is entered into the other legal target through the
// shared effects of a move, the transition names Jev as its decider, and the
// departure is displayed.
func TestAChiefRulingIsJudgedAndAScriptedDepartureIsRecorded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	exit := func(f *buildFixture, repo *trace.Repository, state trace.WorkflowState) router.Exit {
		x, err := f.s.UnitExit(UnitExitInput{Repo: repo, Stream: stream, Unit: "resume", State: state, Trigger: TriggerChiefRuling, Cause: "ruling-1",
			Default: UnitReviewing, Targets: []string{UnitReviewing, UnitImplementing}, Actor: chiefActor, Note: "Review the candidate again.",
			ID: "unit-resume-reviewing-ruling-1", HeaderCause: "ruling-1", Reason: "the chief of staff ruled review"})
		must(t, err)
		return x
	}
	last := func(t *testing.T, repo *trace.Repository) trace.Transition {
		t.Helper()
		transitions, err := trace.Read[trace.Transition](repo, stream)
		must(t, err)
		var out trace.Transition
		for _, tr := range transitions {
			if tr.Subject == trace.UnitSubject("resume") {
				out = tr
			}
		}
		return out
	}

	t.Run("the default fits", func(t *testing.T) {
		t.Parallel()
		f, _, repo := newChecksFixture(t, "ruling-fits", &fakeChecks{Outcomes: passing()})
		routeWith(f.s, f.clock.Now, f.jev)
		x := exit(f, repo, contestUnit(t, f, repo, "fits"))
		res, err := f.s.Router.Route(ctx, x)
		must(t, err)
		moved := last(t, repo)
		if !res.Committed || moved.To != UnitReviewing || moved.DefaultTo != UnitReviewing || moved.DecidedBy != trace.DecidedByCode || moved.Departed() {
			t.Fatalf("result %+v, transition %+v", res, moved)
		}
	})

	t.Run("a departure", func(t *testing.T) {
		t.Parallel()
		f, _, repo := newChecksFixture(t, "ruling-departs", &fakeChecks{Outcomes: passing()})
		f.jev.Script = func(a jevfixture.Ask) jevfixture.Reply { return jevfixture.Departs(a, UnitImplementing, 0.9, 0.9) }
		routeWith(f.s, f.clock.Now, f.jev)
		x := exit(f, repo, contestUnit(t, f, repo, "departs"))
		res, err := f.s.Router.Route(ctx, x)
		must(t, err)
		moved := last(t, repo)
		if !res.Committed || res.Outcome != router.OutcomeDeparted || moved.To != UnitImplementing || moved.DefaultTo != UnitReviewing || moved.DecidedBy != trace.DecidedByJev || moved.Judgment == "" || !moved.Departed() {
			t.Fatalf("result %+v, transition %+v", res, moved)
		}
		rec, err := router.Lookup(repo, stream, x)
		must(t, err)
		if rec == nil || rec.Outcome != router.OutcomeDeparted || rec.Default != UnitReviewing || rec.Target != UnitImplementing || rec.Judgment != moved.Judgment {
			t.Fatalf("route %+v", rec)
		}
	})

	t.Run("a pick below its bar holds the unit", func(t *testing.T) {
		t.Parallel()
		f, _, repo := newChecksFixture(t, "ruling-below-bar", &fakeChecks{Outcomes: passing()})
		f.jev.Script = func(a jevfixture.Ask) jevfixture.Reply { return jevfixture.Departs(a, UnitImplementing, 0.55, 0.9) }
		routeWith(f.s, f.clock.Now, f.jev)
		x := exit(f, repo, contestUnit(t, f, repo, "below"))
		res, err := f.s.Router.Route(ctx, x)
		must(t, err)
		if res.Committed || res.Outcome != router.OutcomeHeld || res.Hold != router.HoldBelowBar || unitState(t, repo, stream, "resume") != UnitContested {
			t.Fatalf("result %+v, the unit is %s", res, unitState(t, repo, stream, "resume"))
		}
	})
}
