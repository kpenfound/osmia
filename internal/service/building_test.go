package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// independentPlan is validPlan with no dependency between its units.
const independentPlan = `{"version": 1, "units": [
  {"id": "resume", "title": "Resume from the last chunk", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestResume"}}], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "dedupe", "title": "Skip acknowledged chunks", "addresses": [{"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "no chunk is sent twice"}}], "depends_on": [], "footprint": ["internal.trace"]}
]}
`

// buildOperations returns the workstream's build operations.
func (f *architectFixture) buildOperations(t *testing.T, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	ops, err := f.repository().Operations(stream)
	must(t, err)
	var out []trace.OperationRecord
	for _, o := range ops {
		if o.Operation.Action == BuildAction {
			out = append(out, o)
		}
	}
	return out
}

// buildTransitions returns, in the order events.jsonl holds them, the
// transitions of the build subject, of the unit subjects and the move to
// building.
func buildTransitions(t *testing.T, directory string, stream config.WorkstreamID) []trace.Transition {
	t.Helper()
	var out []trace.Transition
	for _, tr := range allTransitions(t, directory, stream) {
		if tr.Actor == buildingActor || tr.Subject == buildSubject || tr.To == BuildingState {
			out = append(out, tr)
		}
	}
	return out
}

// allTransitions returns every transition of the workstream's events.jsonl,
// in the order it holds them.
func allTransitions(t *testing.T, directory string, stream config.WorkstreamID) []trace.Transition {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(directory, "workstreams", string(stream), "events.jsonl"))
	must(t, err)
	var out []trace.Transition
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var tr trace.Transition
		must(t, json.Unmarshal(line, &tr))
		if tr.Schema == "osmia.trace.transition" {
			out = append(out, tr)
		}
	}
	return out
}

// transitionMove is what one recorded transition says.
type transitionMove struct{ ID, Subject, From, To, Cause, Reason string }

// moves returns what the transitions say, checking that the building
// controller recorded each.
func moves(t *testing.T, transitions []trace.Transition) []transitionMove {
	t.Helper()
	var out []transitionMove
	for _, tr := range transitions {
		if tr.Actor != buildingActor {
			t.Fatalf("transition %s by %+v", tr.ID, tr.Actor)
		}
		out = append(out, transitionMove{tr.ID, tr.Subject, tr.From, tr.To, tr.Cause, tr.Reason})
	}
	return out
}

// built hands in a design that skips debate, ratifies it and returns the
// workstream once its build is acknowledged.
func (f *shedFixture) built(t *testing.T) (config.WorkstreamID, trace.OperationRecord) {
	t.Helper()
	return f.builtAs(t, "design")
}

// builtAs is built with the design handed in under key.
func (f *shedFixture) builtAs(t *testing.T, key string) (config.WorkstreamID, trace.OperationRecord) {
	t.Helper()
	stream := f.handInSkipping(t, key)
	f.awaitPacket(t, stream, "ratify: no objection stands")
	f.awaitFeature(t, stream, InShedState)
	if _, err := f.c.Ratify(context.Background(), stream, 1, 1); err != nil {
		t.Fatal(err)
	}
	f.awaitFeature(t, stream, BuildingState)
	ops := awaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return f.buildOperations(t, stream) })
	if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" {
		t.Fatalf("build operations %+v", ops)
	}
	if in, err := decodeBuild(ops[0].Operation); err != nil || in != (buildInput{Seal: 1}) {
		t.Fatalf("build operation %+v: %v", ops[0].Operation, err)
	}
	return stream, ops[0]
}

// buildOperation returns the build operation of seal k of the workstream.
func (f *architectFixture) buildOperation(stream config.WorkstreamID, k int) coreadapter.Operation {
	_, event := buildIDs(k)
	input, _ := json.Marshal(buildInput{Seal: k})
	return coreadapter.Operation{ID: trace.OperationID(f.project, stream, event), Boundary: coreadapter.RepositoryBoundary, Action: BuildAction, Input: input}
}

// checkUnits checks the unit states the status API lists for the workstream,
// alone and in the list.
func (f *architectFixture) checkUnits(t *testing.T, stream config.WorkstreamID, want []UnitStatus) {
	t.Helper()
	ctx := context.Background()
	one, err := f.c.Status(ctx, stream)
	must(t, err)
	if one.State == nil || *one.State != BuildingState || !reflect.DeepEqual(one.Units, want) {
		t.Fatalf("status %+v, want units %+v", one, want)
	}
	list, err := f.c.Statuses(ctx)
	must(t, err)
	i := slices.IndexFunc(list.Workstreams, func(w WorkstreamStatus) bool { return w.Workstream == stream })
	if i < 0 || !reflect.DeepEqual(list.Workstreams[i].Units, want) {
		t.Fatalf("status list %+v, want units %+v", list, want)
	}
}

const (
	buildRequested = "seal 1 is recorded; the build records the state of every unit of the sealed plan and moves the workstream to building"
	dependentBuilt = "seal 1 is recorded; the states of the 2 units of its plan are recorded: ready resume; planned dedupe (waiting for resume)"
)

// Ratification moves the workstream to building on its own operation: a unit
// that depends on no unit is ready, the one that depends on it stays planned,
// and every transition is in events.jsonl with its actor and reason. Status
// lists the unit states. Inspecting or applying the operation again, and
// passes after it, record nothing more.
func TestRatifiedWorkstreamBuildsWithTheDependentUnitPlanned(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	defer f.stop(t)
	ctx := context.Background()
	f.upstream(t)
	stream, op := f.built(t)
	id := op.Operation.ID
	want := []transitionMove{
		{"build-1", buildSubject, "", "requested-1", "seal-1", buildRequested},
		{BuildingState, trace.FeatureSubject, RatifiedState, BuildingState, id, dependentBuilt},
		{"unit-resume-planned", "unit-resume", "", UnitPlanned, id, "unit resume is in the plan of seal 1"},
		{"unit-resume-ready", "unit-resume", UnitPlanned, UnitReady, id, "unit resume is ready: it depends on no unit"},
		{"unit-dedupe-planned", "unit-dedupe", "", UnitPlanned, id, "unit dedupe is in the plan of seal 1 and waits for resume to merge"},
	}
	if got := moves(t, buildTransitions(t, f.trace, stream)); !reflect.DeepEqual(got, want) {
		t.Fatalf("transitions:\n%+v\nwant:\n%+v", got, want)
	}
	if op.Result.Evidence != dependentBuilt {
		t.Fatalf("the result's evidence %q", op.Result.Evidence)
	}
	if body := f.notice(t, stream, BuildingState); body != "Workstream state changed from ratified to building: "+dependentBuilt {
		t.Fatalf("the notice %q", body)
	}
	states, err := f.repository().WorkflowStates(stream)
	must(t, err)
	if states["unit-resume"].Value != UnitReady || states["unit-dedupe"].Value != UnitPlanned {
		t.Fatalf("unit states %+v", states)
	}
	f.checkUnits(t, stream, []UnitStatus{{Unit: "resume", State: UnitReady}, {Unit: "dedupe", State: UnitPlanned}})

	// The build is at rest: a pass asks for no other, and the operation
	// inspected or applied again finds its move and records nothing.
	b := &builder{s: f.s, repository: f.repository()}
	must(t, b.Pass(ctx))
	if ops := f.buildOperations(t, stream); len(ops) != 1 {
		t.Fatalf("build operations after a pass %+v", ops)
	}
	observed, err := b.Inspect(ctx, op.Operation)
	must(t, err)
	if observed.State != coreadapter.EffectCompleted || !reflect.DeepEqual(observed.Result, &coreadapter.OperationResult{Outcome: "succeeded", Evidence: dependentBuilt}) {
		t.Fatalf("inspection %+v", observed)
	}
	again, err := b.Apply(ctx, op.Operation)
	must(t, err)
	if !reflect.DeepEqual(again, coreadapter.OperationResult{Outcome: "succeeded", Evidence: dependentBuilt}) {
		t.Fatalf("applied again %+v", again)
	}
	if got := moves(t, buildTransitions(t, f.trace, stream)); !reflect.DeepEqual(got, want) {
		t.Fatalf("transitions after the replay:\n%+v", got)
	}
}

// A plan whose units are all independent has every unit ready.
func TestRatifiedWorkstreamWithIndependentUnitsHasEveryUnitReady(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	defer f.stop(t)
	f.script("draft-1-1", map[string]string{plan.SpecPath: validSpec, plan.PlanPath: independentPlan}, nil)
	f.upstream(t)
	stream, op := f.built(t)
	id := op.Operation.ID
	reason := "seal 1 is recorded; the states of the 2 units of its plan are recorded: ready resume, dedupe; planned none"
	want := []transitionMove{
		{"build-1", buildSubject, "", "requested-1", "seal-1", buildRequested},
		{BuildingState, trace.FeatureSubject, RatifiedState, BuildingState, id, reason},
		{"unit-resume-planned", "unit-resume", "", UnitPlanned, id, "unit resume is in the plan of seal 1"},
		{"unit-resume-ready", "unit-resume", UnitPlanned, UnitReady, id, "unit resume is ready: it depends on no unit"},
		{"unit-dedupe-planned", "unit-dedupe", "", UnitPlanned, id, "unit dedupe is in the plan of seal 1"},
		{"unit-dedupe-ready", "unit-dedupe", UnitPlanned, UnitReady, id, "unit dedupe is ready: it depends on no unit"},
	}
	if got := moves(t, buildTransitions(t, f.trace, stream)); !reflect.DeepEqual(got, want) {
		t.Fatalf("transitions:\n%+v\nwant:\n%+v", got, want)
	}
	f.checkUnits(t, stream, []UnitStatus{{Unit: "resume", State: UnitReady}, {Unit: "dedupe", State: UnitReady}})
}

// A build interrupted after its commit and before its result is recorded is
// completed by the next attempt's inspection, with no transition recorded
// twice.
func TestBuildInterruptedAfterItsCommitIsNotRecordedTwice(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	defer f.stop(t)
	f.upstream(t)
	crashed := make(chan struct{}, 1)
	f.s.boundary = func(name string) error {
		if name == "build-recorded" && len(crashed) == 0 {
			crashed <- struct{}{}
			return errors.New("crash")
		}
		return nil
	}
	stream, op := f.built(t)
	f.s.boundary = nil
	var observed []string
	retries := 0
	for _, a := range op.History {
		switch a.Kind {
		case "observe":
			observed = append(observed, a.Observation.Evidence)
		case "retry":
			retries++
			if !strings.Contains(a.Failure, "crash") {
				t.Fatalf("retry %+v", a)
			}
		}
	}
	if retries != 1 || !slices.Equal(observed, []string{"the workstream has not moved to building", "the workstream moved to building"}) {
		t.Fatalf("%d retries, observed %q", retries, observed)
	}
	if op.Result.Evidence != dependentBuilt {
		t.Fatalf("the result %+v", op.Result)
	}
	var ids []string
	for _, tr := range buildTransitions(t, f.trace, stream) {
		ids = append(ids, tr.ID)
	}
	if !slices.Equal(ids, []string{"build-1", BuildingState, "unit-resume-planned", "unit-resume-ready", "unit-dedupe-planned"}) {
		t.Fatalf("transitions %v", ids)
	}
}

// A build that finds its seal replaced, a unit that already has a state, a
// sealed plan that does not parse or a workstream no longer ratified fails
// with the reason and records nothing. The pass asks once for the build of
// the latest seal and never for an abandoned workstream's. Status reports a
// workstream whose sealed plan cannot be read with no units and a diagnostic
// that names it, and every other workstream as ever.
func TestBuildFailsWithoutRecording(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	ctx := context.Background()
	f.stop(t)
	f.opts.Committee = nil
	f.start(t)
	stream := f.handIn(t, "design", handedDesign)
	f.await(t, stream, sketched)
	other := f.handIn(t, "other", handedDesign)
	f.await(t, other, sketched)
	f.stop(t)

	// The workstream is planted ratified on seal 1, unbuilt.
	repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	b := &builder{s: f.s, repository: repo}
	h := func(id string) trace.Header {
		return trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: f.project, Workstream: stream, At: f.clock.Now(), Actor: ownerActor, Cause: "planted"}
	}
	_, err = repo.SetFeatureState(ctx, h("planted-ratified"), RatifiedState, "planted")
	must(t, err)
	record := func(k, planRevision int, docs ...trace.Document) {
		t.Helper()
		_, doc, _, err := seal.Latest(repo, stream)
		must(t, err)
		content, err := seal.Encode(seal.Seal{Version: seal.Version, Seal: k, Round: 1, Revision: shed.Pin{Spec: 1, Plan: planRevision}, SpecHash: seal.SpecHash(validSpec),
			Base: seal.Base{Remote: "upstream", Branch: "main", Commit: "0123456789abcdef0123456789abcdef01234567"}, Branch: "osmia/" + string(stream), Workspace: "/planted"})
		must(t, err)
		docs = append(docs, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: seal.DocumentID, Revision: doc.Revision + 1, Project: f.project, Workstream: stream, At: f.clock.Now(), Actor: sealingActor, Cause: "planted"}, Path: seal.Path, Content: string(content)})
		must(t, repo.RecordDocuments(ctx, docs))
	}
	record(1, 1)
	var recorded int
	apply := func(k int, want string) {
		t.Helper()
		got, err := b.Apply(ctx, f.buildOperation(stream, k))
		must(t, err)
		if got.Outcome != "failed" || !strings.HasPrefix(got.Evidence, want) {
			t.Fatalf("build on seal %d: %+v, want %q", k, got, want)
		}
		if n := len(buildTransitions(t, f.trace, stream)); n != recorded {
			t.Fatalf("build on seal %d recorded %d transitions, had %d", k, n, recorded)
		}
	}
	apply(2, "building on seal 2 failed: it is not the latest seal of the workstream")

	// A unit of the sealed plan has a state already: the commit refuses it.
	_, err = repo.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: h("planted-unit"), Subject: trace.UnitSubject("dedupe"), To: UnitPlanned, Reason: "planted"}})
	must(t, err)
	recorded = len(buildTransitions(t, f.trace, stream))
	apply(1, "building on seal 1 failed: the workflow changed while the build ran; the workstream is ratified")

	// A seal of a plan revision that does not parse.
	record(2, 2, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: plan.PlanDocument, Revision: 2, Project: f.project, Workstream: stream, At: f.clock.Now(), Actor: ownerActor, Cause: "planted"}, Path: plan.PlanPath, Content: "not a plan"})
	apply(2, "building on seal 2 failed: the sealed plan does not parse: ")

	// The pass asks for the build of the latest seal once.
	must(t, b.Pass(ctx))
	must(t, b.Pass(ctx))
	var requests []string
	for _, tr := range buildTransitions(t, f.trace, stream) {
		if tr.Subject == buildSubject {
			requests = append(requests, tr.From+"->"+tr.To)
		}
	}
	if !slices.Equal(requests, []string{"->requested-2"}) {
		t.Fatalf("build requests %v", requests)
	}
	recorded++

	_, err = repo.SetFeatureState(ctx, h(abandonTransition), AbandonedState, "Not needed.")
	must(t, err)
	apply(2, "building on seal 2 failed: the workstream is abandoned, not ratified")
	// An abandoned workstream is asked for no build.
	must(t, b.Pass(ctx))
	if n := len(buildTransitions(t, f.trace, stream)); n != recorded {
		t.Fatalf("a pass over the abandoned workstream recorded %d transitions, had %d", n, recorded)
	}
	must(t, repo.Close())

	// Status cannot list the units of a seal whose plan does not parse, and
	// says so for that workstream alone.
	f.start(t)
	defer f.stop(t)
	message := "cannot read the unit states of workstream " + string(stream) + "; check the trace repository"
	if _, err := f.c.Status(ctx, stream); !failed(err, Internal) || !strings.Contains(err.Error(), message) {
		t.Fatalf("status of the workstream: %v", err)
	}
	healthy, err := f.c.Status(ctx, other)
	must(t, err)
	if healthy.State == nil || *healthy.State != SketchedState || !reflect.DeepEqual(healthy.Units, []UnitStatus{}) {
		t.Fatalf("status of the other workstream %+v", healthy)
	}
	list, err := f.c.Statuses(ctx)
	must(t, err)
	if len(list.Workstreams) != 2 || !reflect.DeepEqual(list.Diagnostics, []Diagnostic{{"units", Internal, message}}) {
		t.Fatalf("status list %+v", list)
	}
	for _, w := range list.Workstreams {
		if !reflect.DeepEqual(w.Units, []UnitStatus{}) {
			t.Fatalf("units of %s: %+v", w.Workstream, w.Units)
		}
	}
}
