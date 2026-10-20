package scheduler

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestWorkflowReservationsShareCommitteeCapacityWithTurns(t *testing.T) {
	limits := &config.Capacity{Masons: 4, Reviewers: 2, Committee: 1, PerWorkstream: 10}
	repos, schedulers := twoProjects(t, limits)
	pool := schedulers[0].options.Shared
	release, admitted, err := pool.TryReserve(repos[0], stream, "committee", "final-reader", limits)
	must(t, err)
	if !admitted {
		t.Fatal("the free committee slot was refused")
	}
	defer release()
	if _, admitted, err := pool.TryReserve(repos[1], stream, "committee", "final-reader", limits); err != nil || admitted {
		t.Fatalf("another project's final review exceeded capacity: %v %v", admitted, err)
	}
	f := &fixture{project: config.Project{ID: second}, clock: &clock{now: start}}
	f.thread(t, repos[1], stream, "committee1", "committee")
	f.queue(t, repos[1], "committee1", "one")
	must(t, schedulers[1].Pass(context.Background()))
	if got := counts(dispatched(t, repos[1])); got["committee"] != 0 {
		t.Fatalf("committee turn exceeded the workflow's reservation: %v", got)
	}
	if slots := pool.Slots(nil); slots.Used["committee"] != 1 {
		t.Fatalf("unclaimed final review missing from capacity: %+v", slots)
	}
	release()
	release()
	must(t, schedulers[1].Pass(context.Background()))
	if got := counts(dispatched(t, repos[1])); got["committee"] != 1 {
		t.Fatalf("freed committee slot was not reused: %v", got)
	}
	if _, admitted, err := pool.TryReserve(repos[0], stream, "committee", "final-reader", limits); err != nil || admitted {
		t.Fatalf("final review exceeded a scheduled turn's capacity: %v %v", admitted, err)
	}
}

func TestWorkflowReservationCoversClaimedAndCapturedRunner(t *testing.T) {
	for _, role := range []string{"architect", "librarian", "committee"} {
		t.Run(role, func(t *testing.T) {
			f, repo := setup(t)
			defer repo.Close()
			f.thread(t, repo, stream, "runner", role)
			limits := &config.Capacity{Masons: 4, Reviewers: 2, Committee: 1, PerWorkstream: 4}
			pool := &Shared{}
			release, admitted, err := pool.TryReserve(repo, stream, role, "runner", limits)
			must(t, err)
			if !admitted {
				t.Fatal("free slot refused")
			}
			f.queue(t, repo, "runner", "one")
			q, err := repo.ClaimTurn(context.Background(), stream, "runner", "claim", t.TempDir(), f.clock.Now())
			must(t, err)
			s, err := New(repo, Options{Now: f.clock.Now, Capacity: limits, Shared: pool, Holds: func(Candidate) (bool, error) { return true, nil }})
			must(t, err)
			must(t, s.Pass(context.Background()))
			if got := pool.Slots(nil).Used[role]; got != 1 {
				t.Fatalf("claimed runner counted %d times", got)
			}
			h := q.Request.Header
			h.Schema, h.ID, h.At = "osmia.trace.turn-response", "response_one", f.clock.Now()
			err = repo.CaptureTurn(context.Background(), q.Claim.Token, trace.TurnResponse{Header: h, AgentID: "runner", ThreadID: q.Request.ThreadID, TurnID: "one", RequestID: q.Request.ID, RequestRevision: 1, Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session"}, FinalResponse: "captured", SessionDirectory: q.Claim.SessionDirectory, StartedAt: q.Claim.At}})
			must(t, err)
			release()
			// A captured response remains unfinished after a restart. Its own
			// operation must be able to recover it even at a one-slot limit.
			repo = f.reopen(t, repo)
			defer repo.Close()
			pool = &Shared{}
			must(t, pool.Join(repo))
			release, admitted, err = pool.TryReserve(repo, stream, role, "runner", limits)
			must(t, err)
			if !admitted {
				t.Fatal("captured runner blocked its own recovery")
			}
			defer release()
			if got := pool.Slots(nil).Used[role]; got != 1 {
				t.Fatalf("recovering runner counted %d times", got)
			}
		})
	}
}

func TestWorkflowReservationsKeepArchitectAndLibrarianLocalLimits(t *testing.T) {
	for _, role := range []string{"architect", "librarian"} {
		t.Run(role, func(t *testing.T) {
			f, repo := setup(t)
			defer repo.Close()
			must(t, repo.CreateWorkstream(context.Background(), other, f.clock.Now(), owner))
			pool := &Shared{}
			limits := &config.Capacity{PerWorkstream: 4}
			release, admitted, err := pool.TryReserve(repo, stream, role, "runner", limits)
			must(t, err)
			if !admitted {
				t.Fatal("free slot refused")
			}
			defer release()
			if _, admitted, err := pool.TryReserve(repo, stream, role, "runner", limits); err != nil || admitted {
				t.Fatalf("same runner admitted twice: %v %v", admitted, err)
			}
			releaseOther, admitted, err := pool.TryReserve(repo, other, role, "runner", limits)
			must(t, err)
			if !admitted {
				t.Fatal("another workstream's runner was held")
			}
			defer releaseOther()
			if got := pool.Slots(nil).Used[role]; got != 2 {
				t.Fatalf("used %d slots, want both workstreams", got)
			}
		})
	}
}

func TestWorkflowReservationLeavesWorkerAllowanceAvailable(t *testing.T) {
	f, repo := setup(t, "mason1", "mason2")
	defer repo.Close()
	limits := &config.Capacity{Masons: 2, Reviewers: 1, Committee: 1, PerWorkstream: 2}
	pool := &Shared{}
	release, admitted, err := pool.TryReserve(repo, stream, "architect", "architect", limits)
	must(t, err)
	if !admitted {
		t.Fatal("architect slot refused")
	}
	defer release()
	f.thread(t, repo, stream, "architect", "architect")
	f.queue(t, repo, "architect", "draft")
	_, err = repo.ClaimTurn(context.Background(), stream, "architect", "claim", t.TempDir(), f.clock.Now())
	must(t, err)
	for _, agent := range []string{"mason1", "mason2"} {
		f.queue(t, repo, agent, "one")
	}
	s, err := New(repo, Options{Now: f.clock.Now, Capacity: limits, Shared: pool})
	must(t, err)
	must(t, s.Pass(context.Background()))
	if got := counts(dispatched(t, repo)); got["mason"] != 2 {
		t.Fatalf("workflow reservation reduced worker concurrency: %v", got)
	}
	release()
	release, admitted, err = pool.TryReserve(repo, stream, "architect", "architect", limits)
	must(t, err)
	if !admitted {
		t.Fatal("worker allowance held an independent workflow role")
	}
	defer release()
}
