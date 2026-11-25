package fleet

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
)

func placementRig(t *testing.T) (*Placement, *Registry, *clock) {
	t.Helper()
	r, c := newRegistry(t)
	return &Placement{Registry: r, LocalSlots: func() (int, bool) { return 0, true }}, r, c
}

func report(t *testing.T, r *Registry, lease string, n Identity, profiles ...api.ProfileCapability) {
	t.Helper()
	if _, err := r.Report(lease, n, api.CapabilityReport{Revision: r.Handout().Revision, Profiles: profiles}); err != nil {
		t.Fatal(err)
	}
}

func runs(profile string) api.ProfileCapability { return api.ProfileCapability{Profile: profile, Runnable: true} }

func TestBeginUsesTheSlotTheSchedulerTookAndGivesItBackOnRelease(t *testing.T) {
	t.Parallel()
	p, r, _ := placementRig(t)
	n := node("a")
	report(t, r, connect(t, r, n, 1).Lease, n, runs("default"))
	turn := Turn{ID: "t1", Profile: "default"}
	if c := p.Take(turn); c.Client != "a" {
		t.Fatalf("take: %+v", c)
	}
	if c := p.Choose(Turn{ID: "t2", Profile: "default"}); c.Client != "" || c.Reason != WaitClient {
		t.Fatalf("the client's only slot is taken: %+v", c)
	}
	placed, err := p.Begin(context.Background(), turn)
	if err != nil || placed.Client != "a" || !placed.Runs("default") || placed.Runs("other") {
		t.Fatalf("begin: %+v %v", placed, err)
	}
	if id, remote := p.Locate("t1"); !remote || id.ID != "a" || id.Name != "node-a" {
		t.Fatalf("locate: %+v %v", id, remote)
	}
	placed.Release()
	placed.Release()
	if c := p.Choose(Turn{ID: "t2", Profile: "default"}); c.Client != "a" {
		t.Fatalf("the slot was not given back: %+v", c)
	}
	if _, remote := p.Locate("t1"); remote {
		t.Fatal("a released turn is still located")
	}
}

func TestBeginWaitsForASlotAndTakesItWhenItIsFree(t *testing.T) {
	t.Parallel()
	p, r, _ := placementRig(t)
	n := node("a")
	report(t, r, connect(t, r, n, 1).Lease, n, runs("default"))
	p.Take(Turn{ID: "busy", Profile: "default"})
	done := make(chan Placed, 1)
	go func() {
		placed, err := p.Begin(context.Background(), Turn{ID: "late", Profile: "default"})
		if err != nil {
			t.Error(err)
		}
		done <- placed
	}()
	select {
	case placed := <-done:
		t.Fatalf("started with no free slot: %+v", placed)
	case <-time.After(300 * time.Millisecond):
	}
	p.Release("busy")
	select {
	case placed := <-done:
		if placed.Client != "a" {
			t.Fatalf("placed on %q", placed.Client)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the turn did not take the freed slot")
	}
}

func TestBeginReturnsWhenTheContextEndsWhileWaiting(t *testing.T) {
	t.Parallel()
	p, _, _ := placementRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := p.Begin(ctx, Turn{ID: "t", Profile: "default"}); err == nil {
		t.Fatal("a turn with nowhere to run began")
	}
}

func TestBeginPlacesATurnAgainWhenItsReservedClientStartedDraining(t *testing.T) {
	t.Parallel()
	p, r, _ := placementRig(t)
	a, b := node("a"), node("b")
	la, lb := connect(t, r, a, 1), connect(t, r, b, 1)
	report(t, r, la.Lease, a, runs("default"))
	report(t, r, lb.Lease, b, runs("default"))
	turn := Turn{ID: "t1", Profile: "default"}
	if c := p.Take(turn); c.Client != "a" {
		t.Fatalf("take: %+v", c)
	}
	if err := r.Drain(la.Lease, a); err != nil {
		t.Fatal(err)
	}
	placed, err := p.Begin(context.Background(), turn)
	if err != nil || placed.Client != "b" {
		t.Fatalf("begin: %+v %v", placed, err)
	}
}

func TestATurnWithNoReservationIsPlacedWhereItsSessionIs(t *testing.T) {
	t.Parallel()
	p, r, _ := placementRig(t)
	a, b := node("a"), node("b")
	report(t, r, connect(t, r, a, 1).Lease, a, runs("default"))
	report(t, r, connect(t, r, b, 1).Lease, b, runs("default"))
	placed, err := p.Begin(context.Background(), Turn{ID: "t1", Profile: "default", SessionClient: "b"})
	if err != nil || placed.Client != "b" {
		t.Fatalf("begin: %+v %v", placed, err)
	}
}

func TestAnUnplacedTurnIsLocatedOnTheLocalClient(t *testing.T) {
	t.Parallel()
	p, _, _ := placementRig(t)
	if _, remote := p.Locate("never-placed"); remote {
		t.Fatal("a turn nobody placed runs elsewhere than the local client")
	}
}

func TestSlotsOfTurnsAHeldOrOfferedByTheClientCountEvenWithoutAReservation(t *testing.T) {
	t.Parallel()
	p, r, _ := placementRig(t)
	n := node("a")
	resp := connect(t, r, n, 1)
	report(t, r, resp.Lease, n, runs("default"))
	offered := make(chan struct{})
	go func() {
		close(offered)
		_, _ = r.Offer(context.Background(), "a", api.TurnOffer{Turn: "reattached"})
	}()
	<-offered
	for range 100 {
		if c, _ := r.Client("a"); len(c.Offered) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if c := p.Choose(Turn{ID: "t", Profile: "default"}); c.Client != "" {
		t.Fatalf("a client with its slot offered a turn took another: %+v", c)
	}
}

func TestPlacesListsTheLocalClientAndEveryClientWithWhyItTakesNothing(t *testing.T) {
	t.Parallel()
	p, r, _ := placementRig(t)
	a, b, c, d := node("a"), node("b"), node("c"), node("d")
	report(t, r, connect(t, r, a, 1).Lease, a, runs("default"))
	lb := connect(t, r, b, 2)
	report(t, r, lb.Lease, b, runs("default"))
	if err := r.Drain(lb.Lease, b); err != nil {
		t.Fatal(err)
	}
	report(t, r, connect(t, r, c, 2).Lease, c, api.ProfileCapability{Profile: "default", Problems: []api.ProfileProblem{{Check: "claude"}}})
	connect(t, r, d, 2)
	p.Take(Turn{ID: "t1", Profile: "default"})
	var got []string
	for _, place := range p.Places() {
		got = append(got, place.ID+":"+place.Idle)
	}
	if want := []string{"local:no-slots", "a:full", "b:draining", "c:no-profile", "d:no-report"}; !slices.Equal(got, want) {
		t.Fatalf("places %v, want %v", got, want)
	}
	if places := p.Places(); places[1].Used != 1 || !slices.Equal(places[1].Turns, []string{"t1"}) || places[0].Slots != 0 || places[0].Unbounded {
		t.Fatalf("places: %+v", places)
	}
}

func TestAnUnsetLocalClientIsUnboundedAndAlwaysTakesNewTurnsFirst(t *testing.T) {
	t.Parallel()
	_, r, _ := placementRig(t)
	p := &Placement{Registry: r}
	for _, id := range []string{"t1", "t2", "t3"} {
		if c := p.Take(Turn{ID: id, Profile: "default"}); c.Client != Local {
			t.Fatalf("%s went to %q", id, c.Client)
		}
	}
	if places := p.Places(); !places[0].Unbounded || places[0].Used != 3 || places[0].Idle != "" {
		t.Fatalf("local: %+v", places[0])
	}
}

func TestAReservedSlotThatNeverStartsIsGivenBackAfterAWhile(t *testing.T) {
	t.Parallel()
	p, r, c := placementRig(t)
	n := node("a")
	lease := connect(t, r, n, 1).Lease
	report(t, r, lease, n, runs("default"))
	p.Take(Turn{ID: "never", Profile: "default"})
	if got := p.Choose(Turn{ID: "t", Profile: "default"}); got.Client != "" {
		t.Fatalf("the reserved slot was free at once: %+v", got)
	}
	for range 11 {
		c.advance(time.Minute)
		if _, err := r.Heartbeat(lease, n, api.HeartbeatRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := p.Choose(Turn{ID: "t", Profile: "default"}); got.Client != "a" {
		t.Fatalf("the slot of a turn that never started was not given back: %+v", got)
	}
}
