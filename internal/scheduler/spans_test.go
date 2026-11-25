package scheduler

import (
	"context"
	"maps"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/slotspans"
	"github.com/kpenfound/osmia/internal/telemetry"
	"github.com/kpenfound/osmia/internal/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type manual struct {
	mu  sync.Mutex
	now time.Time
}

func (c *manual) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manual) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}

type rig struct {
	t        *testing.T
	exporter *tracetest.InMemoryExporter
	sink     *telemetry.Sink
	clock    *manual
	ledger   string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	sink, err := telemetry.New(config.Telemetry{}, telemetry.Options{Exporter: exporter})
	must(t, err)
	t.Cleanup(func() { sink.Close() })
	return &rig{t: t, exporter: exporter, sink: sink, clock: &manual{now: start}, ledger: filepath.Join(t.TempDir(), "slot-intervals.json")}
}

func (r *rig) recorder() *slotspans.Recorder {
	return slotspans.New(r.sink, slotspans.Options{Ledger: r.ledger, Now: r.clock.Now})
}

func (r *rig) spans(name string) []tracetest.SpanStub {
	r.t.Helper()
	must(r.t, r.sink.Flush(context.Background()))
	var out []tracetest.SpanStub
	for _, s := range r.exporter.GetSpans() {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

func attrs(s tracetest.SpanStub) map[string]string {
	out := map[string]string{}
	for _, kv := range s.Attributes {
		out[string(kv.Key)] = kv.Value.String()
	}
	return out
}

func finish(t *testing.T, repo *trace.Repository, ws config.WorkstreamID, agent, turn string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	q, err := repo.ClaimTurn(ctx, ws, agent, "claim_"+agent+turn, t.TempDir(), at)
	must(t, err)
	h := q.Request.Header
	h.Schema, h.ID, h.At = "osmia.trace.turn-response", "response_"+agent+turn, at
	must(t, repo.CaptureTurn(ctx, q.Claim.Token, trace.TurnResponse{Header: h, AgentID: agent, ThreadID: q.Request.ThreadID, TurnID: turn, RequestID: q.Request.ID, RequestRevision: 1,
		Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session"}, FinalResponse: "done", SessionDirectory: q.Claim.SessionDirectory, StartedAt: q.Claim.At}}))
	must(t, repo.CompleteTurn(ctx, ws, agent, turn, q.Claim.Token, at))
}

func mustTime(t *testing.T, want, got time.Time, what string) {
	t.Helper()
	if !want.Equal(got) {
		t.Fatalf("%s at %s, want %s", what, got.Sub(start), want.Sub(start))
	}
}

func TestScheduledTurnHoldsOneSlotSpanFromDispatchToWhereThePoolStopsCounting(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	f, repo := setup(t, "mason1")
	defer repo.Close()
	f.thread(t, repo, stream, "reviewer1", "reviewer")
	f.thread(t, repo, stream, "chief", trace.ChiefOfStaff)
	f.queue(t, repo, "mason1", "one")
	f.queue(t, repo, "reviewer1", "one")
	f.queue(t, repo, "chief", "one")
	rec := r.recorder()
	defer rec.Close()
	limits := &config.Capacity{Masons: 1, Reviewers: 2, Committee: 1, PerWorkstream: 5}
	s, err := New(repo, Options{Now: r.clock.Now, Capacity: limits, Shared: &Shared{Spans: rec}})
	must(t, err)
	t0 := r.clock.Now()
	must(t, s.Pass(ctx))
	if got := len(dispatched(t, repo)); got != 3 {
		t.Fatalf("dispatched %d turns, want mason, reviewer and chief of staff", got)
	}
	if got := r.spans("osmia.slot.held"); len(got) != 0 {
		t.Fatalf("held spans while the slots are held: %d", len(got))
	}
	r.clock.Advance(10 * time.Second)
	finish(t, repo, stream, "mason1", "one", r.clock.Now())
	finish(t, repo, stream, "reviewer1", "one", r.clock.Now())
	finish(t, repo, stream, "chief", "one", r.clock.Now())
	if got := r.spans("osmia.slot.held"); len(got) != 0 {
		t.Fatalf("held spans ended at turn completion: %d", len(got))
	}
	r.clock.Advance(5 * time.Second)
	released := r.clock.Now()
	must(t, s.Pass(ctx))
	held := r.spans("osmia.slot.held")
	if len(held) != 2 {
		t.Fatalf("%d held spans, want one for the mason and one for the reviewer", len(held))
	}
	byRole := map[string]tracetest.SpanStub{}
	for _, h := range held {
		byRole[attrs(h)["osmia.role"]] = h
	}
	for role, limit := range map[string]string{"mason": "1", "reviewer": "2"} {
		h, ok := byRole[role]
		if !ok {
			t.Fatalf("no held span for %s: %v", role, byRole)
		}
		mustTime(t, t0, h.StartTime, role+" hold start")
		mustTime(t, released, h.EndTime, role+" hold end")
		a := attrs(h)
		if a["osmia.pool.id"] != role || a["osmia.pool.limit"] != limit || a["osmia.outcome"] != "released" || a["osmia.project.id"] != string(project) || a["osmia.workstream.id"] != string(stream) || a["osmia.unit.id"] != "unit" {
			t.Fatalf("%s hold attributes %v", role, a)
		}
		if _, ok := a["osmia.pool.unbounded"]; ok {
			t.Fatalf("bounded pool marked unbounded: %v", a)
		}
		if h.Parent.SpanID() != telemetry.WorkstreamRoot(stream) || h.SpanContext.TraceID() != telemetry.WorkstreamTrace(stream) {
			t.Fatalf("%s hold is not in the workstream's trace", role)
		}
	}
	if got := r.spans("osmia.slot.wait"); len(got) != 0 {
		t.Fatalf("%d wait spans when every turn got a slot", len(got))
	}
}

func TestWaitSpanStartsAtTheFirstRefusalAndKeepsItsIdentityAcrossARestart(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	f, repo := setup(t, "mason1", "mason2")
	defer repo.Close()
	limits := &config.Capacity{Masons: 1, Reviewers: 1, Committee: 1, PerWorkstream: 5}
	paused := false
	holds := func(c Candidate) (bool, error) { return paused && c.Thread.Identity.ID == "mason2", nil }
	pass := func(rec *slotspans.Recorder) {
		t.Helper()
		pool := &Shared{Spans: rec}
		must(t, pool.Join(repo))
		s, err := New(repo, Options{Now: r.clock.Now, Capacity: limits, Holds: holds, Shared: pool})
		must(t, err)
		must(t, s.Pass(ctx))
	}
	rec := r.recorder()
	f.queue(t, repo, "mason1", "one")
	pass(rec)
	paused = true
	r.clock.Advance(time.Minute)
	f.queue(t, repo, "mason2", "one")
	pass(rec)
	if open := rec.Find(slotspans.Wait, slotspans.SourceTurn, nil); len(open) != 0 {
		t.Fatalf("a wait opened under a pause: %+v", open)
	}
	r.clock.Advance(30 * time.Second)
	paused = false
	refusedAt := r.clock.Now()
	pass(rec)
	open := rec.Find(slotspans.Wait, slotspans.SourceTurn, nil)
	if len(open) != 1 {
		t.Fatalf("open waits after the first refusal: %+v", open)
	}
	mustTime(t, refusedAt, open[0].Start, "wait start")
	id := open[0].SpanID()
	if got := r.spans("osmia.slot.wait"); len(got) != 0 {
		t.Fatalf("a wait span before the wait ended: %d", len(got))
	}

	rec.Close()
	r.clock.Advance(20 * time.Second)
	rec = r.recorder()
	defer rec.Close()
	pass(rec)
	if again := rec.Find(slotspans.Wait, slotspans.SourceTurn, nil); len(again) != 1 || again[0].SpanID() != id {
		t.Fatalf("the wait after the restart: %+v, want the one that began at the first refusal", again)
	}

	r.clock.Advance(5 * time.Second)
	finish(t, repo, stream, "mason1", "one", r.clock.Now())
	r.clock.Advance(time.Second)
	granted := r.clock.Now()
	pass(rec)
	waits := r.spans("osmia.slot.wait")
	if len(waits) != 1 {
		t.Fatalf("%d wait spans, want exactly one", len(waits))
	}
	w := waits[0]
	mustTime(t, refusedAt, w.StartTime, "wait start")
	mustTime(t, granted, w.EndTime, "wait end")
	if w.SpanContext.SpanID() != id {
		t.Fatalf("wait span ID %s changed across the restart, was %s", w.SpanContext.SpanID(), id)
	}
	a := attrs(w)
	if a["osmia.outcome"] != "granted" || a["osmia.wait_reason"] != "capacity" || a["osmia.role"] != "mason" || a["osmia.pool.limit"] != "1" || a["osmia.unit.id"] != "unit" {
		t.Fatalf("wait attributes %v", a)
	}
	if got := len(r.spans("osmia.slot.held")); got != 1 {
		t.Fatalf("%d held spans, want mason1's alone (mason2 still holds its slot)", got)
	}
}

func TestWaitSpanEndsWhenTheTurnIsCanceledBlockedOrItsProjectLeaves(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	f, repo := setup(t, "mason1", "mason2", "mason3", "mason4")
	defer repo.Close()
	limits := &config.Capacity{Masons: 1, Reviewers: 1, Committee: 1, PerWorkstream: 9}
	blocked := false
	pool := &Shared{Spans: r.recorder()}
	defer pool.Spans.Close()
	s, err := New(repo, Options{Now: r.clock.Now, Capacity: limits, Shared: pool, Holds: func(c Candidate) (bool, error) {
		return blocked && c.Thread.Identity.ID == "mason3", nil
	}})
	must(t, err)
	for _, agent := range []string{"mason1", "mason2", "mason3", "mason4"} {
		f.queue(t, repo, agent, "one")
	}
	must(t, s.Pass(ctx))
	if open := pool.Spans.Find(slotspans.Wait, slotspans.SourceTurn, nil); len(open) != 3 {
		t.Fatalf("open waits %d, want three", len(open))
	}
	r.clock.Advance(10 * time.Second)
	canceledAt := r.clock.Now()
	n, err := repo.CancelQueuedTurn(ctx, stream, "mason2", "one", canceledAt, owner, "no longer needed")
	must(t, err)
	if n != 1 {
		t.Fatalf("canceled %d turns", n)
	}
	r.clock.Advance(10 * time.Second)
	blocked = true
	blockedAt := r.clock.Now()
	must(t, s.Pass(ctx))
	r.clock.Advance(10 * time.Second)
	leftAt := r.clock.Now()
	pool.Leave(project)
	got := map[string]tracetest.SpanStub{}
	for _, w := range r.spans("osmia.slot.wait") {
		got[attrs(w)["osmia.outcome"]] = w
	}
	if len(got) != 3 || got["canceled"].Name == "" || got["blocked"].Name == "" || got["removed"].Name == "" {
		t.Fatalf("wait spans by outcome %v", slices.Sorted(maps.Keys(got)))
	}
	mustTime(t, canceledAt, got["canceled"].EndTime, "canceled wait end")
	mustTime(t, blockedAt, got["blocked"].EndTime, "blocked wait end")
	mustTime(t, leftAt, got["removed"].EndTime, "removed wait end")
	for outcome, w := range got {
		mustTime(t, start, w.StartTime, outcome+" wait start")
	}
}

func TestSlotHeldAcrossARestartIsSentOnceWithItsOriginalStart(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	f, repo := setup(t, "mason1")
	defer repo.Close()
	f.queue(t, repo, "mason1", "one")
	limits := &config.Capacity{Masons: 1, Reviewers: 1, Committee: 1, PerWorkstream: 5}
	rec := r.recorder()
	s, err := New(repo, Options{Now: r.clock.Now, Capacity: limits, Shared: &Shared{Spans: rec}})
	must(t, err)
	t0 := r.clock.Now()
	must(t, s.Pass(ctx))
	held := rec.Find(slotspans.Held, slotspans.SourceTurn, nil)
	if len(held) != 1 {
		t.Fatalf("open holds %+v", held)
	}
	id := held[0].SpanID()
	rec.Close()

	r.clock.Advance(time.Minute)
	rec = r.recorder()
	defer rec.Close()
	raised := &config.Capacity{Masons: 4, Reviewers: 1, Committee: 1, PerWorkstream: 5}
	pool := &Shared{Spans: rec, Limits: func(role string) (int, bool) { return limitOf(raised, role) }}
	must(t, pool.Join(repo))
	s, err = New(repo, Options{Now: r.clock.Now, Capacity: raised, Shared: pool})
	must(t, err)
	must(t, s.Pass(ctx))
	if got := r.spans("osmia.slot.held"); len(got) != 0 {
		t.Fatalf("%d held spans while the slot is still held", len(got))
	}
	r.clock.Advance(time.Minute)
	finish(t, repo, stream, "mason1", "one", r.clock.Now())
	r.clock.Advance(time.Second)
	released := r.clock.Now()
	must(t, s.Pass(ctx))
	got := r.spans("osmia.slot.held")
	if len(got) != 1 {
		t.Fatalf("%d held spans, want one", len(got))
	}
	mustTime(t, t0, got[0].StartTime, "hold start")
	mustTime(t, released, got[0].EndTime, "hold end")
	if got[0].SpanContext.SpanID() != id {
		t.Fatalf("hold span ID changed across the restart")
	}
	if a := attrs(got[0]); a["osmia.pool.limit"] != "1" {
		t.Fatalf("hold limit %q, want the limit in force at its start", a["osmia.pool.limit"])
	}
}

func TestWorkflowReservationIsHeldFromAdmissionAndItsRunnerTurnIsNotCountedTwice(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	limits := &config.Capacity{Masons: 4, Reviewers: 2, Committee: 1, PerWorkstream: 10}
	pool := &Shared{Spans: r.recorder()}
	defer pool.Spans.Close()
	var repos [2]*trace.Repository
	var schedulers [2]*Scheduler
	var fixtures [2]*fixture
	base := t.TempDir()
	for i, id := range []config.ProjectID{project, second} {
		f, repo := setupIn(t, base, id)
		t.Cleanup(func() { repo.Close() })
		f.thread(t, repo, stream, "committee1", "committee")
		must(t, pool.Join(repo))
		runner := func(c Candidate) (bool, error) { return c.Thread.Identity.ID == "final-reader", nil }
		s, err := New(repo, Options{Now: r.clock.Now, Capacity: limits, Shared: pool, Holds: runner})
		must(t, err)
		repos[i], schedulers[i], fixtures[i] = repo, s, f
	}
	fixtures[0].thread(t, repos[0], stream, "final-reader", "committee")
	t0 := r.clock.Now()
	release, admitted, err := pool.TryReserve(repos[0], stream, "committee", "final-reader", limits)
	must(t, err)
	if !admitted {
		t.Fatal("the free committee slot was refused")
	}
	r.clock.Advance(5 * time.Second)
	refusedAt := r.clock.Now()
	fixtures[1].queue(t, repos[1], "committee1", "one")
	must(t, schedulers[1].Pass(ctx))
	r.clock.Advance(10 * time.Second)
	fixtures[0].queue(t, repos[0], "final-reader", "one")
	must(t, schedulers[0].Pass(ctx))
	if open := pool.Spans.Find(slotspans.Held, slotspans.SourceTurn, func(iv slotspans.Interval) bool { return iv.Project == project }); len(open) != 0 {
		t.Fatalf("the reserved runner counted as a second hold: %+v", open)
	}
	r.clock.Advance(5 * time.Second)
	finish(t, repos[0], stream, "final-reader", "one", r.clock.Now())
	r.clock.Advance(5 * time.Second)
	releasedAt := r.clock.Now()
	release()
	must(t, schedulers[0].Pass(ctx))
	r.clock.Advance(time.Second)
	grantedAt := r.clock.Now()
	must(t, schedulers[1].Pass(ctx))

	held := r.spans("osmia.slot.held")
	if len(held) != 1 {
		t.Fatalf("%d held spans, want the reservation's alone", len(held))
	}
	mustTime(t, t0, held[0].StartTime, "reservation hold start")
	mustTime(t, releasedAt, held[0].EndTime, "reservation hold end")
	a := attrs(held[0])
	if a["osmia.role"] != "committee" || a["osmia.pool.id"] != "committee" || a["osmia.pool.limit"] != "1" || a["osmia.project.id"] != string(project) || a["osmia.outcome"] != "released" {
		t.Fatalf("reservation hold attributes %v", a)
	}
	waits := r.spans("osmia.slot.wait")
	if len(waits) != 1 {
		t.Fatalf("%d wait spans, want the competing project's", len(waits))
	}
	mustTime(t, refusedAt, waits[0].StartTime, "competing wait start")
	mustTime(t, grantedAt, waits[0].EndTime, "competing wait end")
	if a := attrs(waits[0]); a["osmia.project.id"] != string(second) || a["osmia.outcome"] != "granted" || a["osmia.wait_reason"] != "capacity" {
		t.Fatalf("competing wait attributes %v", a)
	}
}

func TestRefusedReservationWaitsUntilItIsAdmittedOrAbandoned(t *testing.T) {
	r := newRig(t)
	f, repo := setup(t)
	defer repo.Close()
	f.thread(t, repo, stream, "reader", "committee")
	limits := &config.Capacity{Committee: 1, PerWorkstream: 3}
	pool := &Shared{Spans: r.recorder()}
	defer pool.Spans.Close()
	hold, admitted, err := pool.TryReserve(repo, stream, "committee", "holder", limits)
	must(t, err)
	if !admitted {
		t.Fatal("free slot refused")
	}
	refusedAt := r.clock.Now()
	if _, admitted, _ := pool.TryReserve(repo, stream, "committee", "reader", limits); admitted {
		t.Fatal("a second committee reservation was admitted")
	}
	r.clock.Advance(time.Minute)
	if _, admitted, _ := pool.TryReserve(repo, stream, "committee", "reader", limits); admitted {
		t.Fatal("a second committee reservation was admitted")
	}
	hold()
	r.clock.Advance(time.Second)
	grantedAt := r.clock.Now()
	release, admitted, err := pool.TryReserve(repo, stream, "committee", "reader", limits)
	must(t, err)
	if !admitted {
		t.Fatal("released slot refused")
	}
	defer release()
	waits := r.spans("osmia.slot.wait")
	if len(waits) != 1 {
		t.Fatalf("%d wait spans, want one for the whole refusal", len(waits))
	}
	mustTime(t, refusedAt, waits[0].StartTime, "reservation wait start")
	mustTime(t, grantedAt, waits[0].EndTime, "reservation wait end")
	if attrs(waits[0])["osmia.outcome"] != "granted" {
		t.Fatalf("attributes %v", attrs(waits[0]))
	}

	if _, admitted, _ := pool.TryReserve(repo, stream, "committee", "late", limits); admitted {
		t.Fatal("admitted past the limit")
	}
	lastRefusal := r.clock.Now()
	r.clock.Advance(StaleReservationWait + time.Minute)
	if _, admitted, _ := pool.TryReserve(repo, stream, "committee", "another", limits); admitted {
		t.Fatal("admitted past the limit")
	}
	var abandoned []tracetest.SpanStub
	for _, w := range r.spans("osmia.slot.wait") {
		if attrs(w)["osmia.outcome"] == "abandoned" {
			abandoned = append(abandoned, w)
		}
	}
	if len(abandoned) != 1 {
		t.Fatalf("%d abandoned waits", len(abandoned))
	}
	mustTime(t, lastRefusal, abandoned[0].EndTime, "abandoned wait end")
}

func TestUnboundedPoolsCarryTheMarkerAndNoLimit(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	f, repo := setup(t)
	defer repo.Close()
	f.thread(t, repo, stream, "architect1", "architect")
	f.queue(t, repo, "architect1", "one")
	limits := &config.Capacity{Masons: 1, Reviewers: 1, Committee: 1, PerWorkstream: 3}
	pool := &Shared{Spans: r.recorder()}
	defer pool.Spans.Close()
	s, err := New(repo, Options{Now: r.clock.Now, Capacity: limits, Shared: pool})
	must(t, err)
	must(t, s.Pass(ctx))
	must(t, repo.CreateWorkstream(ctx, other, f.clock.Now(), owner))
	release, admitted, err := pool.TryReserve(repo, other, "architect", "reserved", limits)
	must(t, err)
	if !admitted {
		t.Fatal("architect reservation refused")
	}
	r.clock.Advance(time.Minute)
	release()
	finish(t, repo, stream, "architect1", "one", r.clock.Now())
	r.clock.Advance(time.Second)
	must(t, s.Pass(ctx))
	held := r.spans("osmia.slot.held")
	if len(held) != 2 {
		t.Fatalf("%d held spans, want the architect turn and the architect reservation", len(held))
	}
	for _, h := range held {
		a := attrs(h)
		if _, limited := a["osmia.pool.limit"]; limited || a["osmia.pool.unbounded"] != "true" || a["osmia.pool.id"] != a["osmia.role"] {
			t.Fatalf("unbounded pool attributes %v", a)
		}
	}
}

func TestNoSpansAreTrackedWhileTelemetryIsOff(t *testing.T) {
	ctx := context.Background()
	f, repo := setup(t, "mason1")
	defer repo.Close()
	f.queue(t, repo, "mason1", "one")
	off, err := telemetry.New(config.Telemetry{}, telemetry.Options{})
	must(t, err)
	rec := slotspans.New(off, slotspans.Options{Ledger: filepath.Join(t.TempDir(), "ledger.json")})
	if rec != nil {
		t.Fatal("a recorder was built for a sink that is off")
	}
	s, err := New(repo, Options{Now: f.clock.Now, Capacity: &config.Capacity{Masons: 1, Reviewers: 1, Committee: 1, PerWorkstream: 3}, Shared: &Shared{Spans: rec}})
	must(t, err)
	must(t, s.Pass(ctx))
	if got := dispatched(t, repo); len(got) != 1 {
		t.Fatalf("dispatched %v", got)
	}
}

func TestSlotSpansOfTurnsOnAClientCarryTheClientIdentity(t *testing.T) {
	spans := newRig(t)
	rec := spans.recorder()
	defer rec.Close()
	r := newPlaceRig(t, big())
	r.pool = &Shared{Spans: rec}
	r.rebuild()
	r.setLocal(0)
	a := r.connect("nCLIENTA1", 1)
	r.agent(stream, "resumed", "mason")
	r.agent(stream, "busy", "mason")
	first := r.queue(stream, "resumed", "one")
	r.pass()
	r.finish(stream, "resumed", first, a.ID)
	busy := r.queue(stream, "busy", "one")
	r.pass()
	second := r.queue(stream, "resumed", "two")
	r.pass()
	if r.isDispatched("resumed", second) {
		t.Fatal("the resumed turn started while its client was full")
	}
	r.finish(stream, "busy", busy, a.ID)
	r.pass()
	if r.where(second) != a.ID {
		t.Fatalf("the resumed turn runs on %q", r.where(second))
	}

	waits := spans.spans("osmia.slot.wait")
	if len(waits) != 1 {
		t.Fatalf("%d wait spans, want the resumed turn's wait for its client", len(waits))
	}
	if got := attrs(waits[0]); got[telemetry.KeyClient] != a.ID || got[telemetry.KeyWaitReason] != slotspans.ReasonClient || got["osmia.outcome"] != slotspans.Granted {
		t.Fatalf("wait span attributes %v", got)
	}
	held := spans.spans("osmia.slot.held")
	if len(held) < 2 {
		t.Fatalf("%d held spans, want one for each turn that ended on the client", len(held))
	}
	for _, h := range held {
		if got := attrs(h); got[telemetry.KeyClient] != a.ID {
			t.Fatalf("held span attributes %v", got)
		}
	}
}

func TestSlotSpansOfALocalOnlyFactoryCarryNoClientUnlessPlacementNamesOne(t *testing.T) {
	spans := newRig(t)
	rec := spans.recorder()
	defer rec.Close()
	f, repo := setup(t, "mason1")
	defer repo.Close()
	f.queue(t, repo, "mason1", "one")
	limits := &config.Capacity{Masons: 1, Reviewers: 1, Committee: 1, PerWorkstream: 5}
	s, err := New(repo, Options{Now: spans.clock.Now, Capacity: limits, Shared: &Shared{Spans: rec}})
	must(t, err)
	must(t, s.Pass(context.Background()))
	spans.clock.Advance(time.Minute)
	finish(t, repo, stream, "mason1", "one", spans.clock.Now())
	must(t, s.Pass(context.Background()))
	held := spans.spans("osmia.slot.held")
	if len(held) != 1 {
		t.Fatalf("%d held spans", len(held))
	}
	if _, ok := attrs(held[0])[telemetry.KeyClient]; ok {
		t.Fatalf("a turn no client was chosen for carries a client: %v", attrs(held[0]))
	}
}
