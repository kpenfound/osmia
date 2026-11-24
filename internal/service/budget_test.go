package service

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

type budgetRig struct {
	t      *testing.T
	f      *shedFixture
	stream config.WorkstreamID
	s      *Service
	signal budgetSignals
}

func newBudgetRig(t *testing.T) *budgetRig {
	t.Helper()
	f, stream := builtForAmendment(t, 1)
	t.Cleanup(func() { f.stop(t) })
	s := &Service{cfg: &config.Config{Budget: config.Budget{PerUnit: "0.30"}}, options: Options{Reconciliation: reconcile.Options{Now: f.clock.Now}}}
	return &budgetRig{t: t, f: f, stream: stream, s: s, signal: budgetSignals{s: s, repository: f.repository()}}
}

func (r *budgetRig) appendCost(id string, amount float64, known bool) {
	r.t.Helper()
	f, stream, ctx := r.f, r.stream, context.Background()
	intakefixture.Must(r.t, r.f.repository().Append(ctx, trace.Cost{Header: trace.Header{Schema: "osmia.trace.cost", Version: trace.Version, ID: id, Revision: 1, Project: f.project, Workstream: stream, At: f.clock.Now(), Actor: trace.Actor{Kind: "service", ID: "thread-runner"}, Cause: "budget-fixture"}, Entry: coreadapter.LedgerEntry{Scope: coreadapter.Scope{Project: string(f.project), Workstream: string(stream), Thread: "thread", Turn: id, Role: "mason"}, AttemptID: id, At: f.clock.Now(), Usage: coreadapter.Usage{CostUSD: amount, CostKnown: known, Turns: 1}}}))
}

func (r *budgetRig) pass() {
	r.t.Helper()
	intakefixture.Must(r.t, r.signal.Pass(context.Background()))
}

func (r *budgetRig) requests() []trace.Amendment {
	r.t.Helper()
	got, err := trace.Read[trace.Amendment](r.f.repository(), r.stream)
	intakefixture.Must(r.t, err)
	return got
}

func (r *budgetRig) unknownCostAttempts() int {
	r.t.Helper()
	baseline, err := trace.Read[trace.Cost](r.f.repository(), r.stream)
	intakefixture.Must(r.t, err)
	unknown := 1
	for _, cost := range baseline {
		if !cost.Entry.Usage.CostKnown {
			unknown++
		}
	}
	return unknown
}

func (r *budgetRig) overspend() {
	r.t.Helper()
	r.appendCost("cost1", 0.1, true)
	r.appendCost("unknown", 0, false)
	r.appendCost("cost2", 0.2, true)
	r.appendCost("cost3", 0.0100001, true)
}

func TestBudgetAmendmentIsNotRequestedAtOrBelowTheThreshold(t *testing.T) {
	t.Parallel()
	r := newBudgetRig(t)
	r.appendCost("cost1", 0.1, true)
	r.appendCost("unknown", 0, false)
	r.pass()
	if got := r.requests(); len(got) != 0 {
		t.Fatalf("below threshold: %+v", got)
	}
	r.appendCost("cost2", 0.2, true)
	r.pass()
	if got := r.requests(); len(got) != 0 {
		t.Fatalf("equal threshold: %+v", got)
	}
}

func TestBudgetAmendmentIsRequestedOnceAboveTheThreshold(t *testing.T) {
	t.Parallel()
	r := newBudgetRig(t)
	unknown := r.unknownCostAttempts()
	r.overspend()
	r.pass()
	r.pass()
	got := r.requests()
	if len(got) != 1 || got[0].Workstream != r.stream || got[0].Role != "service" || !strings.Contains(got[0].Reason, "at least USD 0.3100001") || !strings.Contains(got[0].Reason, fmt.Sprintf("%d attempt(s) have unknown cost", unknown)) {
		t.Fatalf("budget request: %+v", got)
	}
}

func TestBudgetAmendmentDoesNotParkTheUnit(t *testing.T) {
	t.Parallel()
	r := newBudgetRig(t)
	r.overspend()
	r.pass()
	state, err := r.f.repository().Workflow(r.stream, trace.UnitSubject("resume"))
	intakefixture.Must(t, err)
	if state.Value == UnitWaiting {
		t.Fatal("budget request parked the unit")
	}
}

func TestBudgetAmendmentIsNotFiledAgainAfterARestart(t *testing.T) {
	t.Parallel()
	r := newBudgetRig(t)
	r.overspend()
	r.pass()
	if got := r.requests(); len(got) != 1 {
		t.Fatalf("before the restart: %+v", got)
	}
	r.f.stop(t)
	r.f.start(t)
	r.signal = budgetSignals{s: r.s, repository: r.f.repository()}
	r.pass()
	if got := r.requests(); len(got) != 1 {
		t.Fatalf("restart filed duplicate requests: %+v", got)
	}
}
