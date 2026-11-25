package service

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/fleet"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

func ctHandout(r *ctRig) api.ProfileHandout {
	h := api.ProfileHandout{Revision: "rev-status",
		Profiles: []api.ProfileSpec{{Name: "default", Backend: "claude", Model: "test", Fallback: "backup"}, {Name: "backup", Backend: "codex", Model: "test"}},
		Roles:    []api.RoleRequirement{{Role: "architect", Profile: "default", Chain: []string{"default", "backup"}, Backend: "claude", Model: "test", Sandbox: "none", Domains: []string{}, Skills: []string{}}}}
	r.s.Fleet().SetHandout(h)
	return h
}

func (c *ctClient) reportProfiles(h api.ProfileHandout, profiles ...api.ProfileCapability) {
	c.t.Helper()
	status, body := c.post(api.ClientRoute(c.lease, "capabilities"), api.CapabilityReport{Revision: h.Revision, Profiles: profiles})
	if status != http.StatusOK {
		c.t.Fatalf("report: %d %s", status, body)
	}
}

func clientNamed(t *testing.T, c *api.CapacityStatus, id string) api.ClientStatus {
	t.Helper()
	if c == nil {
		t.Fatal("the status holds no capacity view")
	}
	for _, client := range c.Clients {
		if client.ID == id {
			return client
		}
	}
	t.Fatalf("no client %q in %+v", id, c.Clients)
	return api.ClientStatus{}
}

func TestStatusAndTheCapacityViewListAFakeClientNextToTheLocalClient(t *testing.T) {
	t.Parallel()
	r := newCTRig(t, "architect")
	r.thread(t, "second")
	h := ctHandout(r)
	c := r.client(t, "node-1")
	c.reportProfiles(h,
		api.ProfileCapability{Profile: "default", Runnable: true},
		api.ProfileCapability{Profile: "backup", Runnable: false, Problems: []api.ProfileProblem{{Check: "codex --version", Detail: "not found", Fix: "install the codex CLI"}}})
	r.runner.Place = r.s.placeTurn(r.repo)

	r.queue(t, "t1")
	<-r.run(t, "agent")
	r.queueFor(t, "second", "t2")
	done := r.run(t, "second")
	offer := c.nextOffer()
	c.claim(offer)

	for name, capacity := range map[string]*api.CapacityStatus{"status": r.s.statusList().Capacity, "capacity view": r.s.capacityStatus(nil)} {
		local, remote := clientNamed(t, capacity, coreadapter.LocalClient), clientNamed(t, capacity, "node-1")
		if !local.Local || !local.Unbounded || local.Slots != nil || local.Lease != nil || local.Used != 0 || !local.Reported || len(local.Turns) != 0 {
			t.Fatalf("%s: the local client: %+v", name, local)
		}
		if remote.Local || remote.Name != "name-of-node-1" || remote.Slots == nil || *remote.Slots != 2 || remote.Used != 1 || remote.Draining || remote.Idle != "" {
			t.Fatalf("%s: the remote client's identity and slots: %+v", name, remote)
		}
		if remote.Lease == nil || remote.Lease.Health != api.LeaseHealthy || remote.Lease.EndsAt.IsZero() {
			t.Fatalf("%s: the remote client's lease: %+v", name, remote.Lease)
		}
		if !remote.Reported || !remote.ReportCurrent || len(remote.Profiles) != 2 || !remote.Profiles[0].Runnable || remote.Profiles[1].Runnable || len(remote.Profiles[1].Problems) != 1 || remote.Profiles[1].Problems[0].Fix != "install the codex CLI" {
			t.Fatalf("%s: the remote client's profiles: %+v", name, remote.Profiles)
		}
		if want := []api.ClientSkip{{Role: "architect", Main: "default", Profile: "backup"}}; !slices.Equal(remote.Skipped, want) {
			t.Fatalf("%s: the skipped fallbacks %+v, want %+v", name, remote.Skipped, want)
		}
		if len(remote.Turns) != 1 || remote.Turns[0].Turn != "t2" || remote.Turns[0].Role != "architect" || remote.Turns[0].Workstream != fixturekit.Stream || remote.Turns[0].Agent != "second" {
			t.Fatalf("%s: the turns running on the remote client: %+v", name, remote.Turns)
		}
		if local.Name != coreadapter.LocalClient || capacity.Clients[0].ID != coreadapter.LocalClient {
			t.Fatalf("%s: the local client is not listed first: %+v", name, capacity.Clients)
		}
	}
	agents, err := agentStatusesOn(r.repo, fixturekit.Stream, time.Now(), r.s.placedClient)
	fixturekit.Must(t, err)
	if len(agents) != 1 || agents[0].Client != "node-1" {
		t.Fatalf("the running agent does not name its client: %+v", agents)
	}

	if answer := c.result(offer.Turn, ctComplete()); answer != api.ViewAccepted {
		t.Fatalf("result: %s", answer)
	}
	fixturekit.Must(t, <-done)
	if remote := clientNamed(t, r.s.capacityStatus(nil), "node-1"); remote.Used != 0 || len(remote.Turns) != 0 {
		t.Fatalf("the client still holds the finished turn: %+v", remote)
	}
}

func TestStatusShowsADrainingClient(t *testing.T) {
	t.Parallel()
	r := newCTRig(t, "architect")
	h := ctHandout(r)
	c := r.client(t, "node-1")
	c.reportProfiles(h, api.ProfileCapability{Profile: "default", Runnable: true})
	if status, body := c.post(api.ClientRoute(c.lease, "drain"), api.DrainRequest{}); status != http.StatusOK {
		t.Fatalf("drain: %d %s", status, body)
	}
	remote := clientNamed(t, r.s.capacityStatus(nil), "node-1")
	if !remote.Draining || remote.Idle != api.ClientIdleDraining || !strings.Contains(remote.IdleReason, "draining") {
		t.Fatalf("a draining client: %+v", remote)
	}
}

func TestStatusShowsWhyAnIdleClientThatCanRunNothingGetsNoTurns(t *testing.T) {
	t.Parallel()
	r := newCTRig(t, "architect")
	h := ctHandout(r)
	r.setLocalSlots(0)
	c := r.client(t, "node-1")
	c.reportProfiles(h,
		api.ProfileCapability{Profile: "default", Runnable: false, Problems: []api.ProfileProblem{{Check: "claude --version", Detail: "executable not found", Fix: "install the claude CLI and put it on PATH"}}},
		api.ProfileCapability{Profile: "backup", Runnable: false, Problems: []api.ProfileProblem{{Check: "sbx", Fix: "install sbx and run sbx login"}}})

	for name, capacity := range map[string]*api.CapacityStatus{"status": r.s.statusList().Capacity, "capacity view": r.s.capacityStatus(nil)} {
		remote := clientNamed(t, capacity, "node-1")
		if remote.Idle != api.ClientIdleNoProfile || remote.Used != 0 || remote.Lease == nil || len(remote.Turns) != 0 {
			t.Fatalf("%s: an idle client that can run nothing: %+v", name, remote)
		}
		for _, want := range []string{"can run no profile", "claude --version", "install the claude CLI and put it on PATH", "sbx login"} {
			if !strings.Contains(remote.IdleReason, want) {
				t.Fatalf("%s: the reason %q lacks %q", name, remote.IdleReason, want)
			}
		}
		if len(remote.Skipped) != 0 {
			t.Fatalf("%s: a client that cannot run the main profile skips fallbacks: %+v", name, remote.Skipped)
		}
	}
	if choice := r.s.placement.Choose(fleet.Turn{ID: "t-idle", Profile: "default"}); choice.Client != "" || choice.Reason != fleet.WaitNoClient {
		t.Fatalf("a client that can run nothing was given a turn: %+v", choice)
	}
}

func TestStatusShowsALocalClientSetToNoSlotsAndAClientThatHasNotReported(t *testing.T) {
	t.Parallel()
	r := newCTRig(t, "architect")
	r.setLocalSlots(0)
	r.client(t, "node-1")
	capacity := r.s.capacityStatus(nil)
	local := clientNamed(t, capacity, coreadapter.LocalClient)
	if local.Idle != api.ClientIdleNoSlots || local.Unbounded || local.Slots == nil || *local.Slots != 0 || !strings.Contains(local.IdleReason, "capacity.local_slots") {
		t.Fatalf("the local client at zero slots: %+v", local)
	}
	remote := clientNamed(t, capacity, "node-1")
	if remote.Idle != api.ClientIdleNoReport || remote.Reported || len(remote.Profiles) != 0 || !strings.Contains(remote.IdleReason, "not reported") {
		t.Fatalf("a client that has not reported: %+v", remote)
	}
}

func TestStatusTiesATurnThatFailedOnARemoteClientToThatClient(t *testing.T) {
	t.Parallel()
	r := newCTRig(t, "architect")
	r.thread(t, "second")
	h := ctHandout(r)
	r.setLocalSlots(0)
	c := r.client(t, "node-1")
	c.reportProfiles(h, api.ProfileCapability{Profile: "default", Runnable: true})
	r.runner.Place = r.s.placeTurn(r.repo)

	r.queue(t, "t1")
	done := r.run(t, "agent")
	offer := c.nextOffer()
	c.claim(offer)
	failed := coreadapter.SessionResult{IsError: true, ExitCode: 1, FinalResponse: "not logged in: run sbx login", StartedAt: fixturekit.DemoStart}
	if answer := c.result(offer.Turn, failed); answer != api.ViewAccepted {
		t.Fatalf("result: %s", answer)
	}
	<-done

	for name, capacity := range map[string]*api.CapacityStatus{"status": r.s.statusList().Capacity, "capacity view": r.s.capacityStatus(nil)} {
		remote := clientNamed(t, capacity, "node-1")
		if len(remote.Failures) != 1 || remote.Failures[0].Turn != "t1" || remote.Failures[0].Workstream != fixturekit.Stream || remote.Failures[0].Role != "architect" || remote.Failures[0].Failure == "" {
			t.Fatalf("%s: the failed turn is not tied to the client that ran it: %+v", name, remote.Failures)
		}
		if local := clientNamed(t, capacity, coreadapter.LocalClient); len(local.Failures) != 0 {
			t.Fatalf("%s: the failure is tied to the local client: %+v", name, local.Failures)
		}
	}
	streaks := r.s.statusList().FailureStreaks
	if len(streaks) != 1 || !slices.Equal(streaks[0].Clients, []string{"node-1"}) {
		t.Fatalf("the failure streak does not name the client: %+v", streaks)
	}
}
