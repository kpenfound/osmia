package intakejourney

import (
	context "context"
	json "encoding/json"
	errors "errors"
	reflect "reflect"
	slices "slices"
	strings "strings"
	testing "testing"
	time "time"

	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	plan "github.com/kpenfound/osmia/internal/plan"
	seal "github.com/kpenfound/osmia/internal/seal"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	shed "github.com/kpenfound/osmia/internal/shed"
	trace "github.com/kpenfound/osmia/internal/trace"
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
	return f.scenario().BuildOperations(t, stream)
}

var buildTransitions = intakefixture.BuildTransitions

var allTransitions = intakefixture.AllTransitions

type transitionMove = intakefixture.TransitionMove

var moves = intakefixture.Moves

// built hands in a design that skips debate, ratifies it and returns the
// workstream once its build is acknowledged.
func (f *shedFixture) built(t *testing.T) (config.WorkstreamID, trace.OperationRecord) {
	t.Helper()
	return f.builtAs(t, "design")
}

// builtAs is built with the design handed in under key.
func (f *shedFixture) builtAs(t *testing.T, key string) (config.WorkstreamID, trace.OperationRecord) {
	t.Helper()
	return f.scenario().BuiltAs(t, key)
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
	intakefixture.CheckUnits(t, f.c, f.checkStatusError, stream, want)
}

func (f *architectFixture) checkStatusError(t *testing.T, err error, elapsed time.Duration) {
	t.Helper()
	intakefixture.CheckStatusError(t, intakefixture.StatusService{Done: f.s.done, Wait: f.s.Wait, Socket: f.s.Socket, Ready: f.s.ready.Load}, err, elapsed)
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
		{ID: "build-1", Subject: buildSubject, From: "", To: "requested-1", Cause: "seal-1", Reason: buildRequested},
		{ID: BuildingState, Subject: trace.FeatureSubject, From: RatifiedState, To: BuildingState, Cause: id, Reason: dependentBuilt},
		{ID: "unit-resume-planned", Subject: "unit-resume", From: "", To: UnitPlanned, Cause: id, Reason: "unit resume is in the plan of seal 1"},
		{ID: "unit-resume-ready", Subject: "unit-resume", From: UnitPlanned, To: UnitReady, Cause: id, Reason: "unit resume is ready: it depends on no unit"},
		{ID: "unit-dedupe-planned", Subject: "unit-dedupe", From: "", To: UnitPlanned, Cause: id, Reason: "unit dedupe is in the plan of seal 1 and waits for resume to merge"},
	}
	if got := moves(t, buildTransitions(t, f.s.current().Root, f.project, stream)); !reflect.DeepEqual(got, want) {
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

	b := &builder{S: f.s.intake().BuilderDependencies(), Repository: f.repository()}
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
	if got := moves(t, buildTransitions(t, f.s.current().Root, f.project, stream)); !reflect.DeepEqual(got, want) {
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
		{ID: "build-1", Subject: buildSubject, From: "", To: "requested-1", Cause: "seal-1", Reason: buildRequested},
		{ID: BuildingState, Subject: trace.FeatureSubject, From: RatifiedState, To: BuildingState, Cause: id, Reason: reason},
		{ID: "unit-resume-planned", Subject: "unit-resume", From: "", To: UnitPlanned, Cause: id, Reason: "unit resume is in the plan of seal 1"},
		{ID: "unit-resume-ready", Subject: "unit-resume", From: UnitPlanned, To: UnitReady, Cause: id, Reason: "unit resume is ready: it depends on no unit"},
		{ID: "unit-dedupe-planned", Subject: "unit-dedupe", From: "", To: UnitPlanned, Cause: id, Reason: "unit dedupe is in the plan of seal 1"},
		{ID: "unit-dedupe-ready", Subject: "unit-dedupe", From: UnitPlanned, To: UnitReady, Cause: id, Reason: "unit dedupe is ready: it depends on no unit"},
	}
	if got := moves(t, buildTransitions(t, f.s.current().Root, f.project, stream)); !reflect.DeepEqual(got, want) {
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
	for _, tr := range buildTransitions(t, f.s.current().Root, f.project, stream) {
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

	repo, err := trace.Open(f.s.current().Root, f.s.current().Project)
	must(t, err)
	b := &builder{S: f.s.intake().BuilderDependencies(), Repository: repo}
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
		if n := len(buildTransitions(t, f.s.current().Root, f.project, stream)); n != recorded {
			t.Fatalf("build on seal %d recorded %d transitions, had %d", k, n, recorded)
		}
	}
	apply(2, "building on seal 2 failed: it is not the latest seal of the workstream")

	_, err = repo.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: h("planted-unit"), Subject: trace.UnitSubject("dedupe"), To: UnitPlanned, Reason: "planted"}})
	must(t, err)
	recorded = len(buildTransitions(t, f.s.current().Root, f.project, stream))
	apply(1, "building on seal 1 failed: the workflow changed while the build ran; the workstream is ratified")

	record(2, 2, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: plan.PlanDocument, Revision: 2, Project: f.project, Workstream: stream, At: f.clock.Now(), Actor: ownerActor, Cause: "planted"}, Path: plan.PlanPath, Content: "not a plan"})
	apply(2, "building on seal 2 failed: the sealed plan does not parse: ")

	must(t, b.Pass(ctx))
	must(t, b.Pass(ctx))
	var requests []string
	for _, tr := range buildTransitions(t, f.s.current().Root, f.project, stream) {
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

	must(t, b.Pass(ctx))
	if n := len(buildTransitions(t, f.s.current().Root, f.project, stream)); n != recorded {
		t.Fatalf("a pass over the abandoned workstream recorded %d transitions, had %d", n, recorded)
	}
	must(t, repo.Close())

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
	if len(list.Workstreams) != 2 || !reflect.DeepEqual(list.Diagnostics, []Diagnostic{{Field: "units", Code: Internal, Message: message}}) {
		t.Fatalf("status list %+v", list)
	}
	for _, w := range list.Workstreams {
		if !reflect.DeepEqual(w.Units, []UnitStatus{}) {
			t.Fatalf("units of %s: %+v", w.Workstream, w.Units)
		}
	}
}
