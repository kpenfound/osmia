package scheduler

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/fleet"
	"github.com/kpenfound/osmia/internal/trace"
)

type fleetClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fleetClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fleetClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type placeRig struct {
	t      *testing.T
	f      *fixture
	repo   *trace.Repository
	clock  *fleetClock
	reg    *fleet.Registry
	place  *fleet.Placement
	local  *int
	limits config.Capacity
	pool   *Shared
	s      *Scheduler
	leases map[string]string
}

func newPlaceRig(t *testing.T, limits config.Capacity) *placeRig {
	t.Helper()
	f, repo := setup(t)
	t.Cleanup(func() { repo.Close() })
	must(t, repo.CreateWorkstream(context.Background(), other, f.clock.Now(), owner))
	r := &placeRig{t: t, f: f, repo: repo, clock: &fleetClock{t: start}, limits: limits, leases: map[string]string{}}
	r.reset(repo)
	return r
}

func (r *placeRig) reset(repo *trace.Repository) {
	r.t.Helper()
	r.repo = repo
	r.reg = fleet.NewRegistry(fleet.Options{Now: r.clock.Now})
	r.place = &fleet.Placement{Registry: r.reg, LocalSlots: func() (int, bool) {
		if r.local == nil {
			return 0, false
		}
		return *r.local, true
	}}
	r.pool = &Shared{}
	r.rebuild()
}

func (r *placeRig) rebuild() {
	r.t.Helper()
	limits := r.limits
	s, err := New(r.repo, Options{Now: r.f.clock.Now, Capacity: &limits, Placement: r.place, Shared: r.pool})
	must(r.t, err)
	r.s = s
}

func (r *placeRig) setLocal(n int) { r.local = &n }

func (r *placeRig) agent(stream config.WorkstreamID, id, role string) {
	r.t.Helper()
	r.f.thread(r.t, r.repo, stream, id, role)
}

func (r *placeRig) queue(stream config.WorkstreamID, agent, turn string) string {
	r.t.Helper()
	id := agent + "-" + turn
	r.f.queueOn(r.t, r.repo, stream, agent, id)
	return id
}

func (r *placeRig) connect(id string, slots int) fleet.Identity {
	r.t.Helper()
	ident := fleet.Identity{ID: id, Name: "node-" + id}
	resp, err := r.reg.Connect(context.Background(), ident, api.ClientConnectRequest{Version: api.ClientProtocolVersion, Slots: slots})
	must(r.t, err)
	r.leases[id] = resp.Lease
	r.report(id, map[string]bool{"default": true})
	return ident
}

func (r *placeRig) report(id string, profiles map[string]bool) {
	r.t.Helper()
	report := api.CapabilityReport{Revision: r.reg.Handout().Revision}
	for _, name := range slices.Sorted(maps.Keys(profiles)) {
		report.Profiles = append(report.Profiles, api.ProfileCapability{Profile: name, Runnable: profiles[name]})
	}
	_, err := r.reg.Report(r.leases[id], fleet.Identity{ID: id, Name: "node-" + id}, report)
	must(r.t, err)
}

func (r *placeRig) beat(id string) {
	r.t.Helper()
	_, err := r.reg.Heartbeat(r.leases[id], fleet.Identity{ID: id, Name: "node-" + id}, api.HeartbeatRequest{})
	must(r.t, err)
}

func (r *placeRig) pass() {
	r.t.Helper()
	must(r.t, r.s.Pass(context.Background()))
}

func (r *placeRig) dispatched(streams ...config.WorkstreamID) []string {
	return dispatched(r.t, r.repo, streams...)
}

func (r *placeRig) isDispatched(agent, turn string) bool {
	return slices.Contains(r.dispatched(stream, other), agent+"/"+turn)
}

func (r *placeRig) where(turn string) string {
	for _, p := range r.place.Places() {
		if slices.Contains(p.Turns, turn) {
			return p.ID
		}
	}
	return ""
}

func (r *placeRig) used(client string) int {
	for _, p := range r.place.Places() {
		if p.ID == client {
			return p.Used
		}
	}
	return -1
}

func (r *placeRig) finish(stream config.WorkstreamID, agent, turn, client string) {
	r.t.Helper()
	ctx := context.Background()
	token := "claim-" + turn
	q, err := r.repo.ClaimTurn(ctx, stream, agent, token, r.t.TempDir(), r.f.clock.Now())
	must(r.t, err)
	h := q.Request.Header
	h.Schema, h.ID, h.At = "osmia.trace.turn-response", "response_"+turn, r.f.clock.Now()
	res := trace.TurnResponse{Header: h, AgentID: agent, ThreadID: q.Request.ThreadID, TurnID: turn, RequestID: q.Request.ID, RequestRevision: 1,
		Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session-" + agent}, SessionDirectory: q.Claim.SessionDirectory, StartedAt: r.f.clock.Now(), FinalResponse: "done", Client: client}}
	must(r.t, r.repo.CaptureTurn(ctx, token, res))
	must(r.t, r.repo.CompleteTurn(ctx, stream, agent, turn, token, r.f.clock.Now()))
	r.place.Release(turn)
}

func big() config.Capacity {
	return config.Capacity{Masons: 20, Reviewers: 20, Committee: 20, PerWorkstream: 20}
}

func TestATurnStartsOnlyWhenItsRoleLimitAndTheClientsSlotsHaveRoom(t *testing.T) {
	t.Parallel()
	for _, role := range []string{"mason", "reviewer", "committee", "architect", "foreman"} {
		t.Run(role+" waits for the client", func(t *testing.T) {
			t.Parallel()
			r := newPlaceRig(t, big())
			r.setLocal(0)
			r.connect("a", 1)
			r.agent(stream, "x1", role)
			r.agent(other, "x2", role)
			first := r.queue(stream, "x1", "one")
			second := r.queue(other, "x2", "one")
			r.pass()
			r.pass()
			if got := r.dispatched(stream, other); !slices.Equal(got, []string{"x1/" + first}) && !slices.Equal(got, []string{"x2/" + second}) {
				t.Fatalf("a client with one slot took %v", got)
			}
			ran, ranStream, ranAgent, waiting := first, stream, "x1", "x2/"+second
			if r.isDispatched("x2", second) {
				ran, ranStream, ranAgent, waiting = second, other, "x2", "x1/"+first
			}
			r.finish(ranStream, ranAgent, ran, "a")
			r.pass()
			if got := r.dispatched(stream, other); !slices.Contains(got, waiting) || len(got) != 2 {
				t.Fatalf("the freed slot was not taken by %s: %v", waiting, got)
			}
		})
		t.Run(role+" waits for the role limit", func(t *testing.T) {
			t.Parallel()
			limits := big()
			limits.Masons, limits.Reviewers, limits.Committee = 1, 1, 1
			r := newPlaceRig(t, limits)
			r.setLocal(0)
			r.connect("a", 5)
			r.agent(stream, "x1", role)
			r.agent(stream, "x2", role)
			r.queue(stream, "x1", "one")
			r.queue(stream, "x2", "one")
			r.pass()
			r.pass()
			if got := r.dispatched(); len(got) != 1 {
				t.Fatalf("role limit with room on the client dispatched %v", got)
			}
			if used := r.used("a"); used != 1 {
				t.Fatalf("the held back turn took a client slot: %d used", used)
			}
		})
	}
}

func TestNewTurnsRotateAcrossTheLocalClientAndFakeClientsEachPickedOncePerCycle(t *testing.T) {
	t.Parallel()
	r := newPlaceRig(t, config.Capacity{Masons: 1, Reviewers: 1, Committee: 1, PerWorkstream: 20})
	a := r.connect("a", 2)
	b := r.connect("b", 1)
	for i := range 6 {
		r.agent(stream, fmt.Sprintf("m%d", i), "mason")
		r.queue(stream, fmt.Sprintf("m%d", i), "one")
	}
	var order []string
	for i := range 6 {
		r.pass()
		agent := fmt.Sprintf("m%d", i)
		turn := agent + "-one"
		if !r.isDispatched(agent, turn) {
			t.Fatalf("pass %d dispatched %v", i, r.dispatched())
		}
		client := r.where(turn)
		order = append(order, client)
		r.finish(stream, agent, turn, client)
	}
	want := []string{fleet.Local, a.ID, b.ID, fleet.Local, a.ID, b.ID}
	if !slices.Equal(order, want) {
		t.Fatalf("turns went to %v, want %v", order, want)
	}
}

func TestEachClientHasOnePlaceInTheRotationHoweverManySlotsItHas(t *testing.T) {
	t.Parallel()
	r := newPlaceRig(t, big())
	r.setLocal(3)
	a := r.connect("a", 2)
	b := r.connect("b", 1)
	for i := range 6 {
		r.agent(stream, fmt.Sprintf("m%d", i), "mason")
		r.queue(stream, fmt.Sprintf("m%d", i), "one")
	}
	r.pass()
	if got := r.dispatched(); len(got) != 6 {
		t.Fatalf("dispatched %v", got)
	}
	if l, ua, ub := r.used(fleet.Local), r.used(a.ID), r.used(b.ID); l != 3 || ua != 2 || ub != 1 {
		t.Fatalf("slots used: local %d, a %d, b %d, want 3, 2, 1", l, ua, ub)
	}
}

func TestABigClientIsPickedNoMoreOftenThanASmallOneWhileBothHaveRoom(t *testing.T) {
	t.Parallel()
	r := newPlaceRig(t, big())
	r.setLocal(1)
	r.connect("a", 4)
	r.connect("b", 1)
	for i := range 3 {
		r.agent(stream, fmt.Sprintf("m%d", i), "mason")
		r.queue(stream, fmt.Sprintf("m%d", i), "one")
	}
	r.pass()
	var used []string
	for _, p := range r.place.Places() {
		used = append(used, fmt.Sprintf("%s=%d", p.ID, p.Used))
	}
	if !slices.Equal(used, []string{"local=1", "a=1", "b=1"}) {
		t.Fatalf("the clients took %v", used)
	}
}

func TestATurnHeldBackByTheRoleLimitLeavesTheRotationWhereItWas(t *testing.T) {
	t.Parallel()
	r := newPlaceRig(t, config.Capacity{Masons: 1, Reviewers: 1, Committee: 1, PerWorkstream: 20})
	a := r.connect("a", 2)
	r.connect("b", 2)
	r.agent(stream, "m1", "mason")
	r.agent(stream, "m2", "mason")
	first := r.queue(stream, "m1", "one")
	r.pass()
	if got := r.where(first); got != fleet.Local {
		t.Fatalf("the first turn went to %q", got)
	}
	second := r.queue(stream, "m2", "two")
	r.pass()
	r.pass()
	if got := r.dispatched(); len(got) != 1 {
		t.Fatalf("the role limit let %v start", got)
	}
	r.finish(stream, "m1", first, fleet.Local)
	r.pass()
	if got := r.where(second); got != a.ID {
		t.Fatalf("the held back turn went to %q, want the next client in the rotation, %q", got, a.ID)
	}
}

func TestWhenEveryClientIsFullANewTurnWaitsAndTakesNothing(t *testing.T) {
	t.Parallel()
	r := newPlaceRig(t, big())
	r.setLocal(0)
	r.connect("a", 1)
	b := r.connect("b", 1)
	for _, agent := range []string{"m1", "m2", "m3"} {
		r.agent(stream, agent, "mason")
	}
	one := r.queue(stream, "m1", "one")
	r.pass()
	if got := r.where(one); got != "a" {
		t.Fatalf("first turn on %q", got)
	}
	two := r.queue(stream, "m2", "two")
	three := r.queue(stream, "m3", "three")
	r.pass()
	if got := r.where(two); got != b.ID {
		t.Fatalf("second turn on %q", got)
	}
	if got := r.where(three); got != "" {
		t.Fatalf("third turn took a slot on %q although every client is full", got)
	}
	if got := r.dispatched(); len(got) != 2 {
		t.Fatalf("dispatched %v", got)
	}
}

func TestAResumedTurnWaitsForItsSessionsBusyClientWhileAnotherIsIdleAndHoldsNoRoleSlot(t *testing.T) {
	t.Parallel()
	r := newPlaceRig(t, config.Capacity{Masons: 3, Reviewers: 1, Committee: 1, PerWorkstream: 20})
	r.setLocal(0)
	a := r.connect("a", 1)
	r.agent(stream, "resumed", "mason")
	r.agent(stream, "busy", "mason")
	first := r.queue(stream, "resumed", "one")
	r.pass()
	r.finish(stream, "resumed", first, a.ID)
	busy := r.queue(stream, "busy", "one")
	r.pass()
	if got := r.where(busy); got != a.ID {
		t.Fatalf("the busy turn is on %q", got)
	}
	b := r.connect("b", 2)
	second := r.queue(stream, "resumed", "two")
	for range 3 {
		r.pass()
	}
	if r.isDispatched("resumed", second) || r.where(second) != "" {
		t.Fatalf("the resumed turn started while its client was busy and %s was idle: %v", b.ID, r.dispatched())
	}
	if r.used(b.ID) != 0 {
		t.Fatalf("the idle client took %d turns", r.used(b.ID))
	}
	slots := r.pool.Slots([]config.ProjectID{project})
	if slots.Used["mason"] != 1 {
		t.Fatalf("role slots in use: %v, want only the busy turn's", slots.Used)
	}
	if len(slots.Waiting) != 1 || slots.Waiting[0].Reason != WaitClient || slots.Waiting[0].Client != a.ID {
		t.Fatalf("waiting: %+v", slots.Waiting)
	}
}

func TestAResumedTurnTakesTheNextFreedSlotAheadOfAQueuedNewTurn(t *testing.T) {
	t.Parallel()
	r := newPlaceRig(t, big())
	r.setLocal(0)
	a := r.connect("a", 1)
	r.agent(other, "resumed", "mason")
	r.agent(stream, "busy", "mason")
	r.agent(stream, "fresh", "mason")
	first := r.queue(other, "resumed", "one")
	r.pass()
	r.finish(other, "resumed", first, a.ID)
	busy := r.queue(stream, "busy", "one")
	r.pass()
	second := r.queue(other, "resumed", "two")
	fresh := r.queue(stream, "fresh", "one")
	r.pass()
	if r.where(second) != "" || r.where(fresh) != "" {
		t.Fatalf("a turn started while the client's only slot was taken: %v", r.dispatched(stream, other))
	}
	r.finish(stream, "busy", busy, a.ID)
	r.pass()
	if !r.isDispatched("resumed", second) || r.isDispatched("fresh", fresh) {
		t.Fatalf("the freed slot went to %v; the resumed turn comes first although the new turn is ahead of it in the queue", r.dispatched(stream, other))
	}
	if r.where(second) != a.ID {
		t.Fatalf("the resumed turn runs on %q", r.where(second))
	}
}

func TestAFreedSlotIsKeptForAWaitingResumedTurnAgainstNewTurnsFromOutsideThePass(t *testing.T) {
	t.Parallel()
	r := newPlaceRig(t, big())
	r.setLocal(0)
	a := r.connect("a", 1)
	r.agent(stream, "resumed", "mason")
	r.agent(stream, "busy", "mason")
	first := r.queue(stream, "resumed", "one")
	r.pass()
	r.finish(stream, "resumed", first, a.ID)
	busy := r.queue(stream, "busy", "one")
	r.pass()
	r.queue(stream, "resumed", "two")
	r.pass()
	r.finish(stream, "busy", busy, a.ID)
	if c := r.place.Take(fleet.Turn{ID: "chief-turn", Profile: "default", Role: "chief_of_staff"}); c.Client != "" {
		t.Fatalf("a new turn from outside the pass took the slot owed to the resumed turn: %+v", c)
	}
	r.pass()
	if r.where("resumed-two") != a.ID {
		t.Fatalf("the resumed turn did not take the freed slot")
	}
}

func resumedRig(t *testing.T) (*placeRig, fleet.Identity, fleet.Identity) {
	t.Helper()
	r := newPlaceRig(t, big())
	r.setLocal(0)
	a := r.connect("a", 1)
	b := r.connect("b", 2)
	r.agent(stream, "resumed", "mason")
	first := r.queue(stream, "resumed", "one")
	r.pass()
	r.finish(stream, "resumed", first, a.ID)
	return r, a, b
}

func TestAPinnedTurnGoesRoundRobinWhenItsClientsLeaseHasEnded(t *testing.T) {
	t.Parallel()
	r, _, b := resumedRig(t)
	r.clock.advance(90 * time.Second)
	r.beat("b")
	r.clock.advance(90 * time.Second)
	r.beat("b")
	second := r.queue(stream, "resumed", "two")
	r.pass()
	if got := r.where(second); got != b.ID {
		t.Fatalf("the turn went to %q after its client's lease ended, want %q", got, b.ID)
	}
}

func TestAPinnedTurnStillGoesToItsClientWhileTheLeaseIsLateWithinTheGraceWindow(t *testing.T) {
	t.Parallel()
	r, a, _ := resumedRig(t)
	r.clock.advance(100 * time.Second)
	r.beat("b")
	if c, ok := r.reg.Client("a"); !ok || c.Health != fleet.LeaseLate {
		t.Fatalf("the client's lease: %+v %v", c, ok)
	}
	second := r.queue(stream, "resumed", "two")
	r.pass()
	if got := r.where(second); got != a.ID {
		t.Fatalf("the turn went to %q during its client's grace window, want %q", got, a.ID)
	}
}

func TestAPinnedTurnGoesRoundRobinWhenItsClientIsDraining(t *testing.T) {
	t.Parallel()
	r, a, b := resumedRig(t)
	must(t, r.reg.Drain(r.leases["a"], a))
	second := r.queue(stream, "resumed", "two")
	r.pass()
	if got := r.where(second); got != b.ID {
		t.Fatalf("the turn went to %q although its client is draining, want %q", got, b.ID)
	}
}

func TestAPinnedTurnOfTheLocalClientGoesRoundRobinWhenLocalSlotsAreZero(t *testing.T) {
	t.Parallel()
	r := newPlaceRig(t, big())
	a := r.connect("a", 2)
	r.agent(stream, "resumed", "mason")
	first := r.queue(stream, "resumed", "one")
	r.pass()
	r.finish(stream, "resumed", first, fleet.Local)
	second := r.queue(stream, "resumed", "two")
	if client, ok := r.place.Pinned(fleet.Turn{ID: second, Profile: "default", SessionClient: fleet.Local}); !ok || client != fleet.Local {
		t.Fatalf("with no local setting the turn is pinned to %q %v", client, ok)
	}
	r.setLocal(0)
	r.pass()
	if got := r.where(second); got != a.ID {
		t.Fatalf("the turn went to %q with the local client at zero slots, want %q", got, a.ID)
	}
}

func TestAPinnedTurnOfTheLocalClientWaitsForALocalSlot(t *testing.T) {
	t.Parallel()
	r := newPlaceRig(t, big())
	r.setLocal(1)
	r.agent(stream, "resumed", "mason")
	r.agent(stream, "busy", "mason")
	first := r.queue(stream, "resumed", "one")
	r.pass()
	r.finish(stream, "resumed", first, fleet.Local)
	r.queue(stream, "busy", "one")
	r.pass()
	if r.where("busy-one") != fleet.Local {
		t.Fatalf("the busy turn is on %q", r.where("busy-one"))
	}
	r.connect("a", 2)
	second := r.queue(stream, "resumed", "two")
	r.pass()
	if got := r.where(second); got != "" {
		t.Fatalf("the turn started on %q while the local client was full", got)
	}
}

func TestAResumedTurnIsStillPinnedToTheClientTheTraceRecordsAfterARestart(t *testing.T) {
	t.Parallel()
	r, a, _ := resumedRig(t)
	repo := r.f.reopen(t, r.repo)
	r.reset(repo)
	r.setLocal(0)
	r.connect("a", 1)
	r.connect("b", 2)
	r.agent(stream, "busy", "mason")
	r.queue(stream, "busy", "one")
	th, err := repo.Thread(stream, "resumed")
	must(t, err)
	if client, ok := th.SessionClient(); !ok || client != a.ID {
		t.Fatalf("the trace records the session on %q %v", client, ok)
	}
	r.pass()
	if got := r.where("busy-one"); got != a.ID {
		t.Fatalf("the filler turn is on %q", got)
	}
	second := r.queue(stream, "resumed", "two")
	r.pass()
	if got := r.where(second); got != "" {
		t.Fatalf("after a restart the resumed turn started on %q while the client holding its session was full", got)
	}
}

func TestAfterARestartAResumedTurnWaitsForItsClientToReconnectWithinTheGraceWindow(t *testing.T) {
	t.Parallel()
	r, a, b := resumedRig(t)
	second := r.queue(stream, "resumed", "two")
	repo := r.f.reopen(t, r.repo)
	r.reset(repo)
	r.setLocal(0)
	r.connect("b", 2)
	r.pass()
	if got := r.where(second); got != "" {
		t.Fatalf("the turn started on %q before its client could reconnect", got)
	}
	r.clock.advance(time.Minute)
	r.beat("b")
	r.pass()
	if got := r.where(second); got != "" {
		t.Fatalf("the turn started on %q inside the grace window", got)
	}
	r.clock.advance(70 * time.Second)
	r.beat("b")
	r.pass()
	if got := r.where(second); got != b.ID {
		t.Fatalf("the turn went to %q after the window, want %q; its client %q never came back", got, b.ID, a.ID)
	}
}

func TestAClientNeverReceivesATurnWhoseMainProfileItCannotRun(t *testing.T) {
	t.Parallel()
	r := newPlaceRig(t, big())
	r.setLocal(0)
	r.connect("a", 5)
	r.report("a", map[string]bool{"default": false, "other": true})
	b := r.connect("b", 1)
	for i := range 3 {
		r.agent(stream, fmt.Sprintf("m%d", i), "mason")
		r.queue(stream, fmt.Sprintf("m%d", i), "one")
	}
	r.pass()
	if got := r.dispatched(); len(got) != 1 {
		t.Fatalf("dispatched %v", got)
	}
	if r.used("a") != 0 || r.used(b.ID) != 1 {
		t.Fatalf("a took %d turns it cannot run and b %d", r.used("a"), r.used(b.ID))
	}
	waits := r.pool.Slots([]config.ProjectID{project}).Waiting
	if len(waits) != 2 || waits[0].Reason != WaitClient {
		t.Fatalf("waiting %+v", waits)
	}
}

func TestAClientThatCanRunNothingReceivesNoTurnsAndStaysListed(t *testing.T) {
	t.Parallel()
	r := newPlaceRig(t, big())
	r.setLocal(0)
	r.connect("a", 5)
	r.report("a", map[string]bool{"default": false})
	r.agent(stream, "m1", "mason")
	r.queue(stream, "m1", "one")
	r.pass()
	if got := r.dispatched(); len(got) != 0 {
		t.Fatalf("a client that can run nothing was given %v", got)
	}
	waits := r.pool.Slots([]config.ProjectID{project}).Waiting
	if len(waits) != 1 || waits[0].Reason != WaitNoClient {
		t.Fatalf("waiting %+v", waits)
	}
	var idle string
	for _, p := range r.place.Places() {
		if p.ID == "a" {
			idle = p.Idle
		}
	}
	if idle != fleet.IdleNoProfile {
		t.Fatalf("the client is idle because %q", idle)
	}
}

func TestAClientThatHasNotReportedReceivesNoTurns(t *testing.T) {
	t.Parallel()
	r := newPlaceRig(t, big())
	r.setLocal(0)
	_, err := r.reg.Connect(context.Background(), fleet.Identity{ID: "a", Name: "node-a"}, api.ClientConnectRequest{Version: api.ClientProtocolVersion, Slots: 3})
	must(t, err)
	r.agent(stream, "m1", "mason")
	r.queue(stream, "m1", "one")
	r.pass()
	if got := r.dispatched(); len(got) != 0 {
		t.Fatalf("dispatched %v", got)
	}
}

func TestWithTheLocalClientAloneAndNoSettingTheRoleCeilingsHoldAsBefore(t *testing.T) {
	t.Parallel()
	for _, placed := range []bool{false, true} {
		t.Run(fmt.Sprintf("placement %v", placed), func(t *testing.T) {
			t.Parallel()
			f, repo := crowd(t)
			defer repo.Close()
			options := Options{Now: f.clock.Now, Capacity: &config.Capacity{Masons: 2, Reviewers: 1, Committee: 1, PerWorkstream: 10}}
			if placed {
				options.Placement = &fleet.Placement{Registry: fleet.NewRegistry(fleet.Options{})}
			}
			s, err := New(repo, options)
			must(t, err)
			must(t, s.Pass(context.Background()))
			got := counts(dispatched(t, repo, stream, other))
			if want := map[string]int{"architect": 2, "committee": 1, "mason": 2, "reviewer": 1}; !maps.Equal(got, want) {
				t.Fatalf("dispatched by role %v, want %v", got, want)
			}
		})
	}
}

func TestWithTheLocalClientAtZeroNothingIsDispatchedAndTheTurnsWaitForAClient(t *testing.T) {
	t.Parallel()
	f, repo := crowd(t)
	defer repo.Close()
	place := &fleet.Placement{Registry: fleet.NewRegistry(fleet.Options{}), LocalSlots: func() (int, bool) { return 0, true }}
	pool := &Shared{}
	s, err := New(repo, Options{Now: f.clock.Now, Capacity: &config.Capacity{Masons: 2, Reviewers: 1, Committee: 1, PerWorkstream: 10}, Placement: place, Shared: pool})
	must(t, err)
	must(t, s.Pass(context.Background()))
	if got := dispatched(t, repo, stream, other); len(got) != 0 {
		t.Fatalf("dispatched %v with the local client at zero slots", got)
	}
	waits := pool.Slots([]config.ProjectID{project}).Waiting
	if len(waits) == 0 {
		t.Fatal("no turn waits")
	}
	for _, w := range waits {
		if w.Reason != WaitNoClient {
			t.Fatalf("%s waits for %q", w.Thread.Identity.ID, w.Reason)
		}
	}
}
