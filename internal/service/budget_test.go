package service

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestBudgetAmendmentThresholdAndRestart(t *testing.T) {
	f, stream := builtForAmendment(t, 1)
	active := true
	defer func() {
		if active {
			f.stop(t)
		}
	}()
	repo := f.repository()
	baseline, err := trace.Read[trace.Cost](repo, stream)
	must(t, err)
	unknown := 1
	for _, cost := range baseline {
		if !cost.Entry.Usage.CostKnown {
			unknown++
		}
	}
	s := &Service{cfg: &config.Config{Budget: config.Budget{PerUnit: "0.30"}}, options: Options{Reconciliation: reconcile.Options{Now: f.clock.Now}}}
	signal := budgetSignals{s: s, repository: repo}
	ctx := context.Background()
	appendCost := func(id string, amount float64, known bool) {
		t.Helper()
		must(t, repo.Append(ctx, trace.Cost{Header: trace.Header{Schema: "osmia.trace.cost", Version: trace.Version, ID: id, Revision: 1, Project: f.project, Workstream: stream, At: f.clock.Now(), Actor: trace.Actor{Kind: "service", ID: "thread-runner"}, Cause: "budget-fixture"}, Entry: coreadapter.LedgerEntry{Scope: coreadapter.Scope{Project: string(f.project), Workstream: string(stream), Thread: "thread", Turn: id, Role: "mason"}, AttemptID: id, At: f.clock.Now(), Usage: coreadapter.Usage{CostUSD: amount, CostKnown: known, Turns: 1}}}))
	}
	requests := func() []trace.Amendment {
		t.Helper()
		got, err := trace.Read[trace.Amendment](repo, stream)
		must(t, err)
		return got
	}
	appendCost("cost1", 0.1, true)
	appendCost("unknown", 0, false)
	must(t, signal.Pass(ctx))
	if got := requests(); len(got) != 0 {
		t.Fatalf("below threshold: %+v", got)
	}
	appendCost("cost2", 0.2, true)
	must(t, signal.Pass(ctx))
	if got := requests(); len(got) != 0 {
		t.Fatalf("equal threshold: %+v", got)
	}
	appendCost("cost3", 0.0100001, true)
	must(t, signal.Pass(ctx))
	must(t, signal.Pass(ctx))
	got := requests()
	if len(got) != 1 || got[0].Workstream != stream || got[0].Role != "service" || !strings.Contains(got[0].Reason, "at least USD 0.3100001") || !strings.Contains(got[0].Reason, fmt.Sprintf("%d attempt(s) have unknown cost", unknown)) {
		t.Fatalf("budget request: %+v", got)
	}
	state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	must(t, err)
	if state.Value == UnitWaiting {
		t.Fatal("budget request parked the unit")
	}
	f.stop(t)
	active = false
	f.start(t)
	active = true
	repo = f.repository()
	signal = budgetSignals{s: s, repository: repo}
	must(t, signal.Pass(ctx))
	if got := requests(); len(got) != 1 {
		t.Fatalf("restart filed duplicate requests: %+v", got)
	}
}
