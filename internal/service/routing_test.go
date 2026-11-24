package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"

	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/trace"
)

const transitionUnit = "u1"

func transitionFixture(t *testing.T) Options {
	t.Helper()
	opts, _, repo := activityFixture(t)
	fixturekit.Must(t, repo.CreateWorkstream(context.Background(), fixturekit.Stream, time.Now(), ownerActor))
	fixturekit.Must(t, repo.Close())
	return opts
}

func transitionExit(t *testing.T, repo *trace.Repository) router.Exit {
	t.Helper()
	subject := trace.UnitSubject(transitionUnit)
	state, err := repo.Workflow(fixturekit.Stream, subject)
	fixturekit.Must(t, err)
	if state.Value == "" {
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "routing-seed", Revision: 1, Project: repo.Project(), Workstream: fixturekit.Stream, Unit: transitionUnit, At: time.Now(), Actor: trace.Actor{Kind: "service", ID: "checks"}, Cause: "seed"}
		_, err := repo.Transact(context.Background(), trace.Transaction{Transition: trace.Transition{Header: h, Subject: subject, To: "checking", Reason: "seeded"}})
		fixturekit.Must(t, err)
		state, err = repo.Workflow(fixturekit.Stream, subject)
		fixturekit.Must(t, err)
	}
	return router.Exit{
		Repo: repo, Project: repo.Project(), Workstream: fixturekit.Stream, Unit: transitionUnit, Machine: "fixture", Subject: subject, From: state.Value, Version: state.Version,
		Trigger: "unit.checks_passed", Cause: "checks-1", Actor: trace.Actor{Kind: "service", ID: "checks"}, Default: "reviewing",
		Targets: []router.Target{
			{Name: "reviewing", Description: "Start the independent review of the candidate."},
			{Name: "checking", Description: "Run the checks again.", Bar: router.BarModest},
		},
		Evidence: router.Evidence{Intent: "The unit adds the feature.", Facts: map[string]any{"runs": 1}},
	}
}

// The service routes an exit with the Jev it already asks: Jev answering keeps
// the default and the status stays ready; a failing Jev commits the default as a
// fallback and the status shows Jev degraded.
func TestTheServiceRoutesExitsAndShowsJevDegraded(t *testing.T) {
	t.Parallel()
	key := gateKey(t, true)
	var failing atomic.Bool
	p := &jevfixture.Transitions{Fail: func(int) error {
		if failing.Load() {
			return &systemone.Error{Kind: systemone.KindUnavailable, Message: "down"}
		}
		return nil
	}}
	opts := transitionFixture(t)
	withTopLevel(t, opts, gateJevConfig(key))
	opts.JevProvider = p.Factory()
	s, c := start(t, opts)
	repo := s.sole().repository
	ctx := context.Background()

	x := transitionExit(t, repo)
	res, err := s.RouteExit(ctx, x)
	fixturekit.Must(t, err)
	if !res.Committed || res.Outcome != "followed" || res.State != "reviewing" {
		t.Fatalf("result = %+v", res)
	}
	runtime, err := c.Runtime(ctx)
	fixturekit.Must(t, err)
	if runtime.Jev.Mode != jev.ModeReady {
		t.Fatalf("status = %+v", runtime.Jev)
	}

	failing.Store(true)
	y := transitionExit(t, repo)
	y.Trigger, y.Default = "unit.review_needs_checks", "checking"
	res, err = s.RouteExit(ctx, y)
	fixturekit.Must(t, err)
	if !res.Committed || res.Outcome != "fallback" || res.State != "checking" {
		t.Fatalf("result = %+v", res)
	}
	runtime, err = c.Runtime(ctx)
	fixturekit.Must(t, err)
	if runtime.Jev.Mode != jev.ModeDegraded {
		t.Fatalf("status = %+v", runtime.Jev)
	}
}

// An exit raised on the scheduling path is submitted to the worker, which
// routes it beside the caller.
func TestTheServiceRoutesSubmittedExitsBesideTheCaller(t *testing.T) {
	t.Parallel()
	key := gateKey(t, true)
	release := make(chan struct{})
	p := &jevfixture.Transitions{Wait: func(ctx context.Context) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	opts := transitionFixture(t)
	withTopLevel(t, opts, gateJevConfig(key))
	opts.JevProvider = p.Factory()
	s, _ := start(t, opts)
	repo := s.sole().repository

	done := make(chan router.Result, 1)
	x := transitionExit(t, repo)
	if !s.SubmitExit(x, func(r router.Result, err error) {
		fixturekit.Must(t, err)
		done <- r
	}) {
		t.Fatal("the exit was not accepted")
	}
	state, err := repo.Workflow(fixturekit.Stream, x.Subject)
	fixturekit.Must(t, err)
	if state.Version != x.Version {
		t.Fatalf("the unit moved before Jev answered: %+v", state)
	}
	close(release)
	select {
	case res := <-done:
		if !res.Committed || res.State != "reviewing" {
			t.Fatalf("result = %+v", res)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the worker did not route the exit")
	}
}
