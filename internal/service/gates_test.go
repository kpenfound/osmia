package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corejev "github.com/kpenfound/agentcore/jev"
	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func init() {
	gateRescan, gateScanGap = 200*time.Millisecond, 20*time.Millisecond
}

// gateJev is a fake Jev provider that answers every decision request with the
// pick its script chooses from the request's options, at the given confidence,
// and records the requests it received.
type gateJev struct {
	confidence float64
	// pick chooses among the options by state; it defaults to the first.
	pick func(state string, options []string) string
	// wait runs before each answer and may block until the request ends.
	wait func(ctx context.Context, state string) error

	mu       sync.Mutex
	requests []systemone.Request
}

func (g *gateJev) Evaluate(ctx context.Context, r systemone.Request) (systemone.Response, error) {
	g.mu.Lock()
	g.requests = append(g.requests, r)
	g.mu.Unlock()
	state, _ := r.State.(string)
	if g.wait != nil {
		if err := g.wait(ctx, state); err != nil {
			return systemone.Response{}, err
		}
	}
	var options []string
	for _, o := range r.Questions["decision"].Options {
		options = append(options, o.Name)
	}
	choice := options[0]
	if g.pick != nil {
		choice = g.pick(state, options)
	}
	var others []string
	for _, o := range options {
		if o != choice {
			others = append(others, o)
		}
	}
	return systemone.Response{Model: "jev-test", Answers: map[string]systemone.Answer{"decision": jevtest.Choice(choice, g.confidence, g.confidence, others...)},
		Usage: systemone.Usage{InputTokens: 100, OutputTokens: 5, CostUSD: 0.000004, CostKnown: true}}, nil
}

func (g *gateJev) Factory() func(corejev.Settings, string) corejev.Provider {
	return func(corejev.Settings, string) corejev.Provider { return g }
}

func (g *gateJev) Requests() []systemone.Request {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]systemone.Request(nil), g.requests...)
}

// gateJudge is a judge asking p under the default decision threshold, or an
// unconfigured one when p is nil.
func gateJudge(now func() time.Time, p *gateJev) *jev.Judge {
	settings := config.Jev{URL: config.DefaultJevURL, Model: "jev-test", APIKeyEnv: "OSMIA_TEST_JEV_KEY", Timeout: "3s", CheckThreshold: config.DefaultJevCheckThreshold, DecisionThreshold: config.DefaultJevDecisionThreshold}
	j := &jev.Judge{Config: func() config.Jev { return settings }, Getenv: func(string) string { return "" }, Now: now}
	if p != nil {
		j.Provider, j.Getenv = p.Factory(), func(string) string { return "key" }
	}
	return j
}

// fakeGate is an owner gate whose records the test controls: its decision, and
// what applying a pick does.
type fakeGate struct {
	gate    intake.Gate
	applied atomic.Int32
	mu      sync.Mutex
	state   intake.GateDecision
	// refuse is what applying the pick answers instead of recording it.
	refuse *intake.APIError
	// ownerFirst, when set, is the decision the second read finds: the owner
	// decided between Jev's judgment and its application.
	ownerFirst bool
	reads      int
}

func (f *fakeGate) decision() intake.GateDecision {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.ownerFirst && f.reads >= 2 {
		return intake.GateDecision{Recorded: true}
	}
	return f.state
}

type gateFixture struct {
	s    *Service
	w    *gateWorker
	repo *trace.Repository
	now  func() time.Time
}

func newGateFixture(t *testing.T, p *gateJev) *gateFixture {
	t.Helper()
	cfg, repo, clock := hardPauseRepository(t, 0)
	s := &Service{jev: gateJudge(clock.Now, p), cfg: cfg}
	s.options.Reconciliation.Now = clock.Now
	active := &activeProject{id: repo.Project(), repository: repo}
	s.projects = []*activeProject{active}
	f := &gateFixture{s: s, repo: repo, now: clock.Now}
	f.w = newGateWorker(s, active)
	return f
}

// restart is a new worker over the same records, with the judge replaced.
func (f *gateFixture) restart(p *gateJev) {
	f.s.jev = gateJudge(f.now, p)
	f.w = newGateWorker(f.s, f.w.active)
}

// gate is a gate of the kind whose options are the named ones.
func (f *gateFixture) gate(kind jev.DecisionKind, key string, options ...string) *fakeGate {
	g := &fakeGate{}
	var opts []jev.DecisionOption
	for _, o := range options {
		opts = append(opts, jev.DecisionOption{Name: o, Description: o + " it"})
	}
	g.gate = intake.Gate{Kind: kind, Project: f.repo.Project(), Stream: fixturekit.Stream, Key: key, Label: key, Cause: "gate-" + key, Reached: f.now(), Options: opts,
		State: func(context.Context) (string, []jev.Source, error) { return "state of " + key, nil, nil },
		Apply: func(_ context.Context, res jev.Result) *intake.APIError {
			g.applied.Add(1)
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.refuse != nil {
				return g.refuse
			}
			g.state = intake.GateDecision{Recorded: true, ByJev: true}
			return nil
		},
		Decision: func() (intake.GateDecision, error) { return g.decision(), nil }}
	return g
}

func (f *gateFixture) result(t *testing.T, g *fakeGate) jev.Result {
	t.Helper()
	res, err := f.s.jev.Lookup(f.repo, g.gate.Stream, g.gate.Kind, g.gate.Key)
	must(t, err)
	return res
}

// A confident gate is judged once and its pick applied once; after a restart
// the decided gate is neither asked nor applied again.
func TestGateWorkerJudgesAConfidentGateAndAppliesItsPickOnce(t *testing.T) {
	t.Parallel()
	p := &gateJev{confidence: 0.97}
	f := newGateFixture(t, p)
	g := f.gate(jev.KindRatification, jev.RatificationKey(1, 1, 1), "ratify")
	f.w.handle(context.Background(), g.gate)
	if g.applied.Load() != 1 || len(p.Requests()) != 1 {
		t.Fatalf("applied %d times after %d requests", g.applied.Load(), len(p.Requests()))
	}
	if res := f.result(t, g); res.Status != jev.Binding || res.Pick != "ratify" || res.Refused != "" {
		t.Fatalf("result %+v", res)
	}
	f.restart(p)
	f.w.handle(context.Background(), g.gate)
	if g.applied.Load() != 1 || len(p.Requests()) != 1 {
		t.Fatalf("after the restart: applied %d times after %d requests", g.applied.Load(), len(p.Requests()))
	}
}

// A gate whose judgment was pending at a restart, with its request started and
// no answer recorded, is judged again after the restart and applied once.
func TestGateWorkerJudgesAGateWhoseJudgmentWasPendingAtARestart(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	var once sync.Once
	first := &gateJev{confidence: 0.97, wait: func(ctx context.Context, _ string) error {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return ctx.Err()
	}}
	f := newGateFixture(t, first)
	g := f.gate(jev.KindEscalatedContest, jev.ContestKey("contest_1"), "review")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.w.handle(ctx, g.gate)
	}()
	<-entered
	cancel()
	<-done
	if res := f.result(t, g); res.Status != jev.None || g.applied.Load() != 0 {
		t.Fatalf("after the interruption: %+v, applied %d times", res, g.applied.Load())
	}
	second := &gateJev{confidence: 0.97}
	f.restart(second)
	f.w.handle(context.Background(), g.gate)
	f.w.handle(context.Background(), g.gate)
	if g.applied.Load() != 1 || len(second.Requests()) != 1 {
		t.Fatalf("applied %d times after %d requests", g.applied.Load(), len(second.Requests()))
	}
}

// A binding pick recorded before the interruption, with no decision, is applied
// after the restart with no new request, and only once.
func TestGateWorkerAppliesARecordedPickWithoutAskingAgain(t *testing.T) {
	t.Parallel()
	p := &gateJev{confidence: 0.97}
	f := newGateFixture(t, p)
	g := f.gate(jev.KindIntentAmendment, jev.AmendmentKey("1"), "approve", "reject")
	recorded := f.s.jev.Decide(context.Background(), f.repo, jev.DecisionRequest{Scope: scopeOf(g), Cause: g.gate.Cause, Kind: g.gate.Kind, Occurrence: g.gate.Key, State: "state", Options: g.gate.Options})
	if recorded.Status != jev.Binding || len(p.Requests()) != 1 || g.applied.Load() != 0 {
		t.Fatalf("recorded %+v after %d requests", recorded, len(p.Requests()))
	}
	f.restart(&gateJev{confidence: 0.99, pick: func(string, []string) string { return "reject" }})
	f.w.handle(context.Background(), g.gate)
	f.w.handle(context.Background(), g.gate)
	if g.applied.Load() != 1 || len(p.Requests()) != 1 {
		t.Fatalf("applied %d times after %d requests", g.applied.Load(), len(p.Requests()))
	}
}

func scopeOf(g *fakeGate) coreadapter.Scope {
	return coreadapter.Scope{Project: string(g.gate.Project), Workstream: string(g.gate.Stream)}
}

// A decision of Jev's recorded but only partly carried out is carried out again
// with the recorded pick, with no request.
func TestGateWorkerFinishesAJevDecisionThatIsNotCarriedOut(t *testing.T) {
	t.Parallel()
	p := &gateJev{confidence: 0.97}
	f := newGateFixture(t, p)
	g := f.gate(jev.KindEscalatedQuestion, jev.QuestionKey("1"), "option-1", "option-2")
	f.w.handle(context.Background(), g.gate)
	if g.applied.Load() != 1 {
		t.Fatalf("applied %d times", g.applied.Load())
	}
	g.mu.Lock()
	g.state = intake.GateDecision{Recorded: true, ByJev: true, Incomplete: true}
	g.mu.Unlock()
	f.restart(nil)
	f.w.handle(context.Background(), g.gate)
	if g.applied.Load() != 2 || len(p.Requests()) != 1 {
		t.Fatalf("applied %d times after %d requests", g.applied.Load(), len(p.Requests()))
	}
	f.w.handle(context.Background(), g.gate)
	if g.applied.Load() != 2 {
		t.Fatalf("a finished decision was applied again: %d", g.applied.Load())
	}
}

// A gate the owner decided is left as the owner decided it, whether the owner
// did before the gate was looked at or between Jev's judgment and its
// application.
func TestGateWorkerLeavesAGateTheOwnerDecidedFirst(t *testing.T) {
	t.Parallel()
	t.Run("before the gate was looked at", func(t *testing.T) {
		t.Parallel()
		p := &gateJev{confidence: 0.97}
		f := newGateFixture(t, p)
		g := f.gate(jev.KindRatification, jev.RatificationKey(1, 1, 1), "ratify")
		g.state = intake.GateDecision{Recorded: true}
		f.w.handle(context.Background(), g.gate)
		if g.applied.Load() != 0 {
			t.Fatalf("applied %d times", g.applied.Load())
		}
	})
	t.Run("after Jev judged", func(t *testing.T) {
		t.Parallel()
		p := &gateJev{confidence: 0.97}
		f := newGateFixture(t, p)
		g := f.gate(jev.KindRatification, jev.RatificationKey(1, 1, 1), "ratify")
		g.ownerFirst = true
		f.w.handle(context.Background(), g.gate)
		if g.applied.Load() != 0 || len(p.Requests()) != 1 {
			t.Fatalf("applied %d times after %d requests", g.applied.Load(), len(p.Requests()))
		}
		f.restart(p)
		f.w.handle(context.Background(), g.gate)
		if g.applied.Load() != 0 {
			t.Fatalf("applied %d times after the restart", g.applied.Load())
		}
	})
}

// A gate that went to the owner after a below-threshold answer, a deferral or a
// fallback is not judged again, after a restart included, and nothing is applied.
func TestGateWorkerNeverJudgesAGateAgainOnceTheOwnerHasIt(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		first   *gateJev
		options []string
		want    jev.ResultStatus
	}{
		"below the threshold": {&gateJev{confidence: 0.6}, []string{"dismissed", "overruled"}, jev.Nonbinding},
		"deferred":            {&gateJev{confidence: 0.99, pick: func(string, []string) string { return jev.Defer }}, []string{"dismissed"}, jev.Nonbinding},
		"an outage":           {nil, []string{"dismissed", "overruled"}, jev.FallbackOnly},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newGateFixture(t, c.first)
			g := f.gate(jev.KindEscalatedObjection, jev.ObjectionKey("o1"), c.options...)
			f.w.handle(context.Background(), g.gate)
			if res := f.result(t, g); res.Status != c.want || g.applied.Load() != 0 {
				t.Fatalf("result %+v, applied %d times", res, g.applied.Load())
			}
			later := &gateJev{confidence: 0.99}
			f.restart(later)
			f.w.handle(context.Background(), g.gate)
			f.w.handle(context.Background(), g.gate)
			if len(later.Requests()) != 0 || g.applied.Load() != 0 {
				t.Fatalf("after the restart: %d requests, applied %d times", len(later.Requests()), g.applied.Load())
			}
		})
	}
}

// A confident pick the service refuses stays with the owner showing Jev's
// judgment, is not applied again and is not asked again, after a restart
// included. A failure to reach the records is not a refusal.
func TestGateWorkerHandsARefusedPickToTheOwnerOnce(t *testing.T) {
	t.Parallel()
	p := &gateJev{confidence: 0.97}
	f := newGateFixture(t, p)
	g := f.gate(jev.KindRatification, jev.RatificationKey(1, 1, 1), "ratify")
	g.refuse = &intake.APIError{Code: intake.Conflict, Message: "the plan is not valid"}
	f.w.handle(context.Background(), g.gate)
	attempts := g.applied.Load()
	if attempts < 1 {
		t.Fatalf("the pick was applied %d times", attempts)
	}
	res := f.result(t, g)
	if res.Status != jev.Binding || res.Pick != "ratify" || res.Refused != "the plan is not valid" {
		t.Fatalf("result %+v", res)
	}
	for range 2 {
		f.restart(&gateJev{confidence: 0.99})
		f.w.handle(context.Background(), g.gate)
	}
	if g.applied.Load() != attempts || len(p.Requests()) != 1 {
		t.Fatalf("after the restarts: applied %d times after %d requests", g.applied.Load(), len(p.Requests()))
	}

	t.Run("an unreadable record", func(t *testing.T) {
		t.Parallel()
		f := newGateFixture(t, &gateJev{confidence: 0.97})
		g := f.gate(jev.KindRatification, jev.RatificationKey(2, 1, 1), "ratify")
		g.refuse = &intake.APIError{Code: intake.Internal, Message: "cannot read the trace"}
		f.w.handle(context.Background(), g.gate)
		if res := f.result(t, g); res.Refused != "" {
			t.Fatalf("an internal failure refused the pick: %+v", res)
		}
		g.mu.Lock()
		g.refuse = nil
		g.mu.Unlock()
		f.w.handle(context.Background(), g.gate)
		if res := f.result(t, g); res.Refused != "" || g.applied.Load() != 2 {
			t.Fatalf("result %+v, applied %d times", res, g.applied.Load())
		}
	})
}

// While one gate's provider call hangs until its timeout another gate is
// judged and applied, and waking the worker never waits for either.
func TestGateWorkerOneHangingGateDoesNotHoldUpAnother(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	var once sync.Once
	p := &gateJev{confidence: 0.97, wait: func(ctx context.Context, state string) error {
		if strings.Contains(state, "slow") {
			once.Do(func() { close(entered) })
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}}
	f := newGateFixture(t, p)
	slow := f.gate(jev.KindRatification, jev.RatificationKey(1, 1, 1), "ratify")
	slow.gate.State = func(context.Context) (string, []jev.Source, error) { return "slow", nil, nil }
	fast := f.gate(jev.KindRatification, jev.RatificationKey(2, 1, 1), "ratify")
	slowDone := make(chan struct{})
	go func() {
		defer close(slowDone)
		f.w.handle(context.Background(), slow.gate)
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the slow gate's request never started")
	}

	woken := make(chan struct{})
	go func() { f.w.kick(); close(woken) }()
	select {
	case <-woken:
	case <-time.After(time.Second):
		t.Fatal("waking the worker waited for a gate")
	}
	f.w.handle(context.Background(), fast.gate)
	if fast.applied.Load() != 1 {
		t.Fatalf("the other gate was applied %d times", fast.applied.Load())
	}
	select {
	case <-slowDone:
		t.Fatal("the slow gate finished before its timeout")
	default:
	}
	select {
	case <-slowDone:
	case <-time.After(30 * time.Second):
		t.Fatal("the slow gate never gave up")
	}
	// The gate whose request timed out is a fallback: it went to the owner.
	if res := f.result(t, slow); res.Status != jev.FallbackOnly || slow.applied.Load() != 0 {
		t.Fatalf("slow gate: %+v, applied %d times", res, slow.applied.Load())
	}
	if got := fmt.Sprint(f.result(t, fast).Status); got != string(jev.Binding) {
		t.Fatalf("fast gate: %s", got)
	}
}

// The scheduler's hook only wakes the gate worker of its trace.
func TestSchedulingHookWakesTheGateWorkerWithoutWaiting(t *testing.T) {
	t.Parallel()
	f := newGateFixture(t, nil)
	f.s.gateWorkers.Store(f.repo, f.w)
	f.s.kickGates(f.repo)
	f.s.kickGates(f.repo)
	select {
	case <-f.w.wake:
	default:
		t.Fatal("the worker was not woken")
	}
	select {
	case <-f.w.wake:
		t.Fatal("wakes did not coalesce")
	default:
	}
	// A trace without a worker is not an error.
	f.s.gateWorkers.Delete(f.repo)
	f.s.kickGates(f.repo)
}
