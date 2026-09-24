package service

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/trace"
)

// Each deferral reason is decided from what the pass ends with, in order of
// precedence, and carries the IDs that explain it and a plain-language
// message.
func TestDeferralReasons(t *testing.T) {
	t.Parallel()
	const (
		project config.ProjectID    = "p_00000000000000000000000000000001"
		first   config.WorkstreamID = "w_00000000000000000000000000000001"
		second  config.WorkstreamID = "w_00000000000000000000000000000002"
	)
	entity := func(name string) kb.Entity {
		return kb.Entity{ID: "internal." + name, Name: "internal/" + name, Aliases: []string{}, Paths: []string{"internal/" + name}, Owners: []string{}, PartOf: []string{}}
	}
	entities := kb.Map{Entities: []kb.Entity{entity("trace"), entity("upload"), entity("audit"), entity("index")}}
	units := plan.Plan{Version: 1, Units: []plan.Unit{
		{ID: "resume", Footprint: []string{"internal.trace"}},
		{ID: "dedupe", Footprint: []string{"internal.trace"}},
		{ID: "upload", Footprint: []string{"internal.upload"}},
		{ID: "audit", DependsOn: []string{"upload"}, Footprint: []string{"internal.audit"}},
		{ID: "vague", Footprint: []string{"internal.nowhere"}},
		{ID: "index", Footprint: []string{"internal.index"}},
	}}
	stream := func(id config.WorkstreamID, implementing int, inFlight ...string) building {
		states := map[string]trace.WorkflowState{}
		for _, u := range units.Units {
			states[trace.UnitSubject(u.ID)] = trace.WorkflowState{Version: 1, Value: UnitReady}
		}
		for _, u := range inFlight {
			states[trace.UnitSubject(u)] = trace.WorkflowState{Version: 2, Value: UnitImplementing}
		}
		return building{stream: id, states: states, plan: units, inFlight: inFlight, implementing: implementing}
	}
	cfg := &config.Config{Capacity: config.Capacity{Masons: 3}, Project: config.Project{ID: project, Capacity: config.ProjectCapacity{PerWorkstream: 2}}}
	factory := runtime.Pause{Target: runtime.Target{Scope: "factory"}, Mode: "soft", Source: "owner"}
	pass := func(pauses []runtime.Pause, priorities []runtime.Priority, held map[config.WorkstreamID]string, candidates ...building) dispatchPass {
		return dispatchPass{cfg: cfg, pauses: pauses, rank: scheduler.Rank(priorities, project), entities: entities, candidates: candidates, held: held}
	}
	busy := stream(first, 1, "resume")
	behind := stream(second, 1, "upload")
	for _, tc := range []struct {
		name string
		pass dispatchPass
		b    building
		unit string
		want UnitDispatch
	}{
		{"pause", pass([]runtime.Pause{factory}, nil, map[config.WorkstreamID]string{first: "resume"}), busy, "dedupe",
			UnitDispatch{Reason: DeferPaused, Pause: &factory, Message: "Waits while a factory pause is in force, set by owner."}},
		{"blocked on itself", pass(nil, nil, map[config.WorkstreamID]string{first: "dedupe"}), stream(first, 0), "dedupe",
			UnitDispatch{Reason: DeferBlocked, Blocked: "dedupe", Message: "Cannot start: the mason controller is blocked on this unit and tries again on every pass."}},
		{"blocked on another unit", pass(nil, nil, map[config.WorkstreamID]string{first: "resume"}), busy, "dedupe",
			UnitDispatch{Reason: DeferBlocked, Blocked: "resume", Message: "Waits while the mason controller is blocked on unit resume of this workstream."}},
		{"footprint overlap", pass(nil, nil, nil, busy), busy, "dedupe",
			UnitDispatch{Reason: DeferEntangled, Blockers: []plan.StartBlocker{{Unit: "resume", Reason: plan.OverlapReason}}, Message: "Waits for units in flight it is entangled with: resume (their footprints overlap)."}},
		{"dependency", pass(nil, nil, nil, behind), behind, "audit",
			UnitDispatch{Reason: DeferEntangled, Blockers: []plan.StartBlocker{{Unit: "upload", Reason: plan.DependencyReason}}, Message: "Waits for units in flight it is entangled with: upload (one depends on the other)."}},
		{"unresolved footprint", pass(nil, nil, nil, busy), busy, "vague",
			UnitDispatch{Reason: DeferEntangled, Blockers: []plan.StartBlocker{{Unit: "resume", Reason: plan.MappingReason}}, Message: "Waits for units in flight it is entangled with: resume (a footprint does not resolve through the entity map)."}},
		{"workstream cap", pass(nil, nil, nil, stream(first, 2, "resume", "upload")), stream(first, 2, "resume", "upload"), "index",
			UnitDispatch{Reason: DeferWorkstreamCap, Limit: 2, Message: "Waits for a slot in its workstream: 2 of its units are implementing, the per-workstream cap."}},
		{"capacity", pass(nil, nil, nil, busy, behind), busy, "index",
			UnitDispatch{Reason: DeferCapacity, Limit: 3, Message: "Waits for a mason slot: all 3 are in use."}},
		{"capacity behind a capped higher-priority workstream", pass(nil, []runtime.Priority{{Project: project, Workstreams: []config.WorkstreamID{second, first}}}, nil, busy, stream(second, 2, "resume", "upload")), busy, "index",
			UnitDispatch{Reason: DeferCapacity, Limit: 3, Message: "Waits for a mason slot: all 3 are in use."}},
		{"priority", pass(nil, []runtime.Priority{{Project: project, Workstreams: []config.WorkstreamID{second, first}}}, nil, busy, behind), busy, "index",
			UnitDispatch{Reason: DeferPriority, Limit: 3, Workstreams: []config.WorkstreamID{second}, Message: "Waits for a mason slot: all 3 are in use, and higher-priority workstreams start first: " + string(second) + "."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := tc.pass.deferral(tc.b, tc.unit)
			must(t, err)
			tc.want.Unit, tc.want.Version, tc.want.Decision = tc.unit, 1, DispatchDeferred
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("deferral %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A deferral shows in status only while the unit is ready in the state it
// was decided on; a start is never shown as a deferral.
func TestCurrentDeferralFollowsTheUnitState(t *testing.T) {
	t.Parallel()
	d := UnitDispatch{Unit: "resume", Version: 3, Decision: DispatchDeferred, Reason: DeferCapacity, Limit: 1, Message: "Waits for a mason slot: all 1 are in use."}
	doc := func(d UnitDispatch) trace.Document {
		data, err := json.Marshal(d)
		must(t, err)
		return trace.Document{Header: trace.Header{ID: dispatchDocument(d.Unit), Revision: 1, Unit: d.Unit}, Content: string(data)}
	}
	for _, tc := range []struct {
		name  string
		doc   trace.Document
		state trace.WorkflowState
		want  *UnitDispatch
	}{
		{"current", doc(d), trace.WorkflowState{Version: 3, Value: UnitReady}, &d},
		{"none recorded", trace.Document{}, trace.WorkflowState{Version: 3, Value: UnitReady}, nil},
		{"unit moved on", doc(d), trace.WorkflowState{Version: 4, Value: UnitImplementing}, nil},
		{"unit ready again", doc(d), trace.WorkflowState{Version: 5, Value: UnitReady}, nil},
		{"started", doc(startedDispatch("resume", trace.WorkflowState{Version: 3})), trace.WorkflowState{Version: 3, Value: UnitReady}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := currentDeferral(tc.doc, tc.state)
			must(t, err)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("deferral %+v, want %+v", got, tc.want)
			}
		})
	}
}

// deferred is the status of the ready unit of the workstream while the
// mason controller defers it as d.
func (f *shedFixture) deferred(t *testing.T, stream config.WorkstreamID, unit string, d UnitDispatch) UnitStatus {
	t.Helper()
	if d.Pause != nil {
		state, _ := f.s.store.Effective()
		for _, pause := range state.Pauses {
			if pause.Target == d.Pause.Target {
				d.Pause = &pause
				break
			}
		}
	}
	states, err := f.repository().WorkflowStates(stream)
	must(t, err)
	d.Unit, d.Version, d.Decision = unit, states[trace.UnitSubject(unit)].Version, DispatchDeferred
	return UnitStatus{Unit: unit, State: UnitReady, Reason: d.Message, Deferral: &d}
}

// overlapping is the deferral of a unit whose footprint overlaps that of
// the unit in flight.
func overlapping(unit string) UnitDispatch {
	return UnitDispatch{Reason: DeferEntangled, Blockers: []plan.StartBlocker{{Unit: unit, Reason: plan.OverlapReason}}, Message: "Waits for units in flight it is entangled with: " + unit + " (their footprints overlap)."}
}

// slotless is the deferral of a unit while every one of the mason slots is
// in use.
func slotless(masons int) UnitDispatch {
	return UnitDispatch{Reason: DeferCapacity, Limit: masons, Message: fmt.Sprintf("Waits for a mason slot: all %d are in use.", masons)}
}

// factoryPaused is the deferral of a unit while the operator's soft factory
// pause is in force.
var factoryPaused = UnitDispatch{Reason: DeferPaused, Pause: &runtime.Pause{Target: runtime.Target{Scope: "factory"}, Mode: "soft", Source: "owner"}, Message: "Waits while a factory pause is in force, set by owner."}

// blockedOnItself is the deferral of a unit the mason controller is blocked
// on.
func blockedOnItself(unit string) UnitDispatch {
	return UnitDispatch{Reason: DeferBlocked, Blocked: unit, Message: "Cannot start: the mason controller is blocked on this unit and tries again on every pass."}
}

// dispatches returns the decisions recorded on the unit, oldest first, each
// as its decision and reason, checking every revision's provenance.
func (f *shedFixture) dispatches(t *testing.T, stream config.WorkstreamID, unit string) []string {
	t.Helper()
	documents, err := trace.Read[trace.Document](f.repository(), stream)
	must(t, err)
	var out []string
	for _, doc := range documents {
		if doc.ID != dispatchDocument(unit) {
			continue
		}
		var d UnitDispatch
		must(t, json.Unmarshal([]byte(doc.Content), &d))
		if doc.Revision != len(out)+1 || doc.Path != "units/"+unit+"/dispatch.json" || doc.Unit != unit || doc.Actor != masonActor || d.Unit != unit || d.Message == "" {
			t.Fatalf("dispatch revision %+v", doc)
		}
		out = append(out, strings.TrimSuffix(d.Decision+" "+d.Reason, " "))
	}
	return out
}

// awaitDispatches waits until n decisions are recorded on the unit.
func (f *shedFixture) awaitDispatches(t *testing.T, stream config.WorkstreamID, unit string, n int) {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for len(f.dispatches(t, stream, unit)) < n {
		if time.Now().After(deadline) {
			t.Fatalf("decisions on unit %s of %s: %q, want %d", unit, stream, f.dispatches(t, stream, unit), n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Every ready unit the mason controller does not start carries why in the
// trace and in status: held by a factory pause, then behind a workstream
// earlier in the priority order, or waiting for the one mason slot, then
// held by a workstream pause. A decision is recorded once when it changes,
// however many passes see it unchanged, and a start is recorded with it.
func TestReadyUnitDecisionsFollowPauseAndPriority(t *testing.T) {
	t.Parallel()
	f, masons := newParallelMasonFixture(t, 1, 3, disjointPlan)
	defer f.stop(t)
	factory := runtime.Target{Scope: "factory"}
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: factory, Mode: "soft", Source: "owner"})
	a, _ := f.builtAs(t, "first")
	b, _ := f.builtAs(t, "second")
	// The priority order goes against the workstream ID order, which would
	// otherwise break the tie.
	lo, hi := lowHigh(a, b)
	units := []string{"resume", "upload", "audit"}
	for _, stream := range []config.WorkstreamID{lo, hi} {
		f.awaitDispatches(t, stream, "audit", 1)
	}
	for _, stream := range []config.WorkstreamID{lo, hi} {
		var want []UnitStatus
		for _, unit := range units {
			want = append(want, f.deferred(t, stream, unit, factoryPaused))
		}
		f.checkUnits(t, stream, want)
	}

	mutation(t, f.c, "PUT", "priority", PriorityRequest{Project: f.project, Workstreams: []config.WorkstreamID{hi, lo}})
	mutation(t, f.c, "DELETE", "pause", factory)
	f.awaitMasonRan(t, hi, "resume")
	settle()
	settle()
	behind := UnitDispatch{Reason: DeferPriority, Limit: 1, Workstreams: []config.WorkstreamID{hi}, Message: "Waits for a mason slot: all 1 are in use, and higher-priority workstreams start first: " + string(hi) + "."}
	f.checkUnits(t, hi, []UnitStatus{{Unit: "resume", State: UnitImplementing}, f.deferred(t, hi, "upload", slotless(1)), f.deferred(t, hi, "audit", slotless(1))})
	f.checkUnits(t, lo, []UnitStatus{f.deferred(t, lo, "resume", behind), f.deferred(t, lo, "upload", behind), f.deferred(t, lo, "audit", behind)})
	for _, unit := range units {
		if got, want := f.dispatches(t, lo, unit), []string{"deferred paused", "deferred priority"}; !slices.Equal(got, want) {
			t.Fatalf("decisions on %s of %s: %q, want %q", unit, lo, got, want)
		}
	}
	if got, want := f.dispatches(t, hi, "resume"), []string{"deferred paused", "started"}; !slices.Equal(got, want) {
		t.Fatalf("decisions on resume of %s: %q, want %q", hi, got, want)
	}

	pause := runtime.Pause{Target: runtime.Target{Scope: "workstream", Project: f.project, Workstream: hi}, Mode: "soft", Source: "owner"}
	mutation(t, f.c, "PUT", "pause", PauseRequest(pause))
	f.awaitMasonRan(t, lo, "resume")
	settle()
	masons.check(t)
	paused := UnitDispatch{Reason: DeferPaused, Pause: &pause, Message: "Waits while a workstream pause is in force, set by owner."}
	f.checkUnits(t, hi, []UnitStatus{{Unit: "resume", State: UnitImplementing}, f.deferred(t, hi, "upload", paused), f.deferred(t, hi, "audit", paused)})
	f.checkUnits(t, lo, []UnitStatus{{Unit: "resume", State: UnitImplementing}, f.deferred(t, lo, "upload", slotless(1)), f.deferred(t, lo, "audit", slotless(1))})
	for stream, want := range map[config.WorkstreamID]map[string][]string{
		lo: {"resume": {"deferred paused", "deferred priority", "started"}, "upload": {"deferred paused", "deferred priority", "deferred capacity"}},
		hi: {"resume": {"deferred paused", "started"}, "upload": {"deferred paused", "deferred capacity", "deferred paused"}},
	} {
		for unit, want := range want {
			if got := f.dispatches(t, stream, unit); !slices.Equal(got, want) {
				t.Fatalf("decisions on %s of %s: %q, want %q", unit, stream, got, want)
			}
		}
	}
}
