package service

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/trace"
)

const routingJudgment = "judgment_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// routedFixture is a workstream trace holding unit u1 transitions chosen in
// every way the router records: a followed default, a Jev departure, a
// fallback, an owner's pinned move, one of Jev's gate decisions, a reviewer's
// verdict, and a transition no router chose.
func routedFixture(t *testing.T) *trace.Repository {
	t.Helper()
	ctx := context.Background()
	cfg, err := projectstore.LoadConfig(fixtureAt(t, t.TempDir()).Config)
	must(t, err)
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, demoStart, owner)
	must(t, err)
	t.Cleanup(func() { repo.Close() })
	must(t, repo.CreateWorkstream(ctx, stream, demoStart, owner))

	var version uint64
	from := ""
	step := func(id, to string, actor trace.Actor, route trace.Routing) {
		t.Helper()
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: project, Workstream: stream, Unit: "u1", At: demoStart.Add(time.Duration(version+1) * time.Minute), Actor: actor, Cause: "fixture", Depth: 1}
		tr := trace.Transition{Header: h, Subject: trace.UnitSubject("u1"), From: from, To: to, Reason: "reason " + id, DecidedBy: route.DecidedBy, Judgment: route.Judgment, DefaultTo: route.DefaultTo}
		state, err := repo.Transact(ctx, trace.Transaction{ExpectedVersion: version, Transition: tr})
		must(t, err)
		version, from = state.Version, to
	}
	svc := trace.Actor{Kind: "service", ID: "osmia"}
	step("t-unrouted", "ready", svc, trace.Routing{})
	step("t-followed", "implementing", svc, trace.Routing{DecidedBy: trace.DecidedByCode, DefaultTo: "implementing"})
	step("t-departed", "contested", trace.JevActor, trace.Routing{DecidedBy: trace.DecidedByJev, Judgment: routingJudgment, DefaultTo: "checking"})
	step("t-fallback", "reviewing", svc, trace.Routing{DecidedBy: trace.DecidedByFallback, DefaultTo: "reviewing"})
	step("t-pinned", "implementing", owner, trace.Routing{DecidedBy: trace.DecidedByOwner, DefaultTo: "contested"})
	step("t-gate", "checking", trace.JevActor, trace.Routing{DecidedBy: trace.DecidedByJevGate, Judgment: routingJudgment, DefaultTo: "reviewing"})
	step("t-verdict", "approved", svc, trace.Routing{DecidedBy: trace.DecidedByReviewer, DefaultTo: "approved"})
	return repo
}

// The routing a transition records is in the feed, and the feed marks a
// transition as a departure only when Jev departed from the default, whoever
// recorded the others.
func TestFeedCarriesTheRoutingAndMarksOnlyDepartures(t *testing.T) {
	t.Parallel()
	repo := routedFixture(t)
	entries, err := feedEntries(repo, repo, stream)
	must(t, err)
	type view struct {
		decidedBy, judgment, defaultTo string
		departed                       bool
	}
	got := map[string]view{}
	for _, e := range entries {
		if e.Kind == "transition" && e.Transition != nil {
			tr := e.Transition
			got[tr.To+"/"+tr.Reason] = view{tr.DecidedBy, tr.Judgment, tr.DefaultTo, tr.Departed}
		}
	}
	want := map[string]view{
		"ready/reason t-unrouted":        {},
		"implementing/reason t-followed": {"code", "", "implementing", false},
		"contested/reason t-departed":    {"jev", routingJudgment, "checking", true},
		"reviewing/reason t-fallback":    {"fallback", "", "reviewing", false},
		"implementing/reason t-pinned":   {"owner", "", "contested", false},
		"checking/reason t-gate":         {"jev_gate", routingJudgment, "reviewing", false},
		"approved/reason t-verdict":      {"reviewer", "", "approved", false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("feed routing\n got %+v\nwant %+v", got, want)
	}

	// The fields survive the API's JSON encoding, and a transition without
	// them encodes none.
	for _, e := range entries {
		if e.Kind != "transition" || e.Transition == nil {
			continue
		}
		data, err := json.Marshal(e)
		must(t, err)
		var back api.FeedEntry
		must(t, json.Unmarshal(data, &back))
		if !reflect.DeepEqual(back.Transition, e.Transition) {
			t.Fatalf("round trip of %s: %+v, want %+v", data, back.Transition, e.Transition)
		}
		var raw struct {
			Transition map[string]any `json:"transition"`
		}
		must(t, json.Unmarshal(data, &raw))
		_, hasDecidedBy := raw.Transition["decided_by"]
		if hasDecidedBy != (e.Transition.DecidedBy != "") {
			t.Fatalf("decided_by encoded=%v for %+v", hasDecidedBy, e.Transition)
		}
	}
}

// A unit's trace history carries the same routing and the same single mark.
func TestUnitTraceHistoryCarriesTheRoutingAndMarksOnlyDepartures(t *testing.T) {
	t.Parallel()
	repo := routedFixture(t)
	walk, err := loadTraceWalk(repo, stream)
	must(t, err)
	unit, ok := walk.unit("u1", "")
	if !ok {
		t.Fatal("no trace for u1")
	}
	departed := 0
	routed := 0
	for _, e := range unit.History {
		if e.Ref.Kind != "transition" {
			continue
		}
		if e.DecidedBy != "" {
			routed++
		}
		if e.Departed {
			departed++
			if e.DecidedBy != trace.DecidedByJev || e.Judgment != routingJudgment || e.DefaultTo != "checking" {
				t.Fatalf("the departure records %+v", e)
			}
		}
	}
	if routed != 6 || departed != 1 {
		t.Fatalf("%d routed history events and %d departures, want 6 and 1: %+v", routed, departed, unit.History)
	}
}
