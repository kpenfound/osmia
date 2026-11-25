package service

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/fleet"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func (c *ctClient) report(profiles ...string) {
	c.t.Helper()
	report := api.CapabilityReport{}
	for _, name := range profiles {
		report.Profiles = append(report.Profiles, api.ProfileCapability{Profile: name, Runnable: true})
	}
	if status, body := c.post(api.ClientRoute(c.lease, "capabilities"), report); status != http.StatusOK {
		c.t.Fatalf("report: %d %s", status, body)
	}
}

func (r *ctRig) thread(t *testing.T, id string) {
	t.Helper()
	h := trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, Revision: 1, ID: id, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: ownerActor, Cause: "message", Depth: 3}
	fixturekit.Must(t, r.repo.CreateThread(context.Background(), trace.Agent{Header: h, Role: "architect", ThreadID: id}))
}

func (r *ctRig) queueFor(t *testing.T, agent, turn string) {
	t.Helper()
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, Revision: 1, ID: "request_" + turn, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: ownerActor, Cause: "owner-message", Depth: 3},
		AgentID: agent, ThreadID: agent, TurnID: turn, Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "Role instructions", Prompt: "Do the work for " + turn}
	_, err := r.repo.EnqueueTurn(context.Background(), req)
	fixturekit.Must(t, err)
}

func (r *ctRig) run(t *testing.T, agent string) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := r.runner.RunNext(context.Background(), fixturekit.Stream, agent, coreadapter.PreparedTurn{SessionDirectory: mkdir(t, filepath.Join(t.TempDir(), "session-"+agent))})
		done <- err
	}()
	return done
}

func (r *ctRig) ranOn(t *testing.T, agent string) string {
	t.Helper()
	th, err := r.repo.Thread(fixturekit.Stream, agent)
	fixturekit.Must(t, err)
	for _, q := range th.Turns {
		if q.Response != nil {
			return q.Response.Result.Client
		}
	}
	return ""
}

func (r *ctRig) setLocalSlots(n int) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	cfg := *r.s.cfg
	cfg.Capacity.LocalSlots = &n
	r.s.cfg = &cfg
}

func TestTheServicePlacesTurnsRoundRobinOnTheLocalClientAndAFakeClientAndPinsAResumedTurn(t *testing.T) {
	t.Parallel()
	r := newCTRig(t, "architect")
	r.thread(t, "second")
	c := r.client(t, "node-1")
	c.report("default")
	r.runner.Place = r.s.placeTurn(r.repo)

	r.queue(t, "t1")
	<-r.run(t, "agent")
	if got := r.ranOn(t, "agent"); got != coreadapter.LocalClient {
		t.Fatalf("the first turn ran on %q", got)
	}
	if len(r.engine.Requests) == 0 {
		t.Fatal("the local client ran no session in this process")
	}

	r.queueFor(t, "second", "t2")
	done := r.run(t, "second")
	offer := c.nextOffer()
	c.claim(offer)
	if answer := c.result(offer.Turn, ctComplete()); answer != api.ViewAccepted {
		t.Fatalf("result: %s", answer)
	}
	fixturekit.Must(t, <-done)
	if got := r.ranOn(t, "second"); got != "node-1" {
		t.Fatalf("the second turn ran on %q, want the next client in the rotation", got)
	}
	if holder, ok := r.s.placement.Pinned(fleet.Turn{ID: "t3", Profile: "default", SessionClient: "node-1"}); !ok || holder != "node-1" {
		t.Fatalf("a thread whose session is on node-1 is pinned to %q %v", holder, ok)
	}
	for _, place := range r.s.ClientPlaces() {
		if place.Used != 0 {
			t.Fatalf("%s still holds slots after its turn ended: %+v", place.ID, place)
		}
	}
}

func TestWithTheLocalClientAtZeroSlotsNoSessionRunsLocallyWhileChecksStillTakeSlots(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newCTRig(t, "architect")
	c := r.client(t, "node-1")
	c.report("default")
	r.setLocalSlots(0)
	r.runner.Place = r.s.placeTurn(r.repo)

	r.queue(t, "t1")
	done := r.run(t, "agent")
	offer := c.nextOffer()
	c.claim(offer)
	if answer := c.result(offer.Turn, ctComplete()); answer != api.ViewAccepted {
		t.Fatalf("result: %s", answer)
	}
	fixturekit.Must(t, <-done)
	if got := r.ranOn(t, "agent"); got != "node-1" {
		t.Fatalf("the turn ran on %q with the local client at zero slots", got)
	}
	if len(r.engine.Requests) != 0 {
		t.Fatalf("the server ran %d sessions", len(r.engine.Requests))
	}
	release, err := r.s.checkSlots.acquire(ctx, CheckSlotRun{Project: fixturekit.Project, Workstream: fixturekit.Stream, Unit: "unit"})
	fixturekit.Must(t, err)
	release()
}

func TestWithTheLocalClientAtZeroSlotsAndNoClientATurnWaitsInsteadOfRunningLocally(t *testing.T) {
	t.Parallel()
	r := newCTRig(t, "architect")
	r.setLocalSlots(0)
	r.runner.Place = r.s.placeTurn(r.repo)
	r.queue(t, "t1")
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	_, err := r.runner.RunNext(ctx, fixturekit.Stream, "agent", coreadapter.PreparedTurn{SessionDirectory: mkdir(t, filepath.Join(t.TempDir(), "session"))})
	if err == nil {
		t.Fatal("the turn ran with nowhere to run")
	}
	if len(r.engine.Requests) != 0 {
		t.Fatalf("the server ran %d sessions", len(r.engine.Requests))
	}
}

func TestTheLocalClientsSlotCountComesFromTheConfigurationAndIsUnsetByDefault(t *testing.T) {
	t.Parallel()
	r := newCTRig(t, "architect")
	if n, set := r.s.localSlots(); set {
		t.Fatalf("local slots are set to %d by default", n)
	}
	r.setLocalSlots(0)
	if n, set := r.s.localSlots(); !set || n != 0 {
		t.Fatalf("local slots: %d %v", n, set)
	}
}
