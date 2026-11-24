package intakejourney

import (
	context "context"
	errors "errors"
	reflect "reflect"
	slices "slices"
	strings "strings"
	testing "testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	plan "github.com/kpenfound/osmia/internal/plan"
	seal "github.com/kpenfound/osmia/internal/seal"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	shed "github.com/kpenfound/osmia/internal/shed"
	trace "github.com/kpenfound/osmia/internal/trace"
)

const (
	buildRequested = "seal 1 is recorded; the build records the state of every unit of the sealed plan and moves the workstream to building"
	dependentBuilt = "seal 1 is recorded; the states of the 2 units of its plan are recorded: ready resume; planned dedupe (waiting for resume)"
)

func TestRatifiedWorkstreamBuildsWithTheDependentUnitPlanned(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	defer f.Stop(t)
	ctx := context.Background()
	f.Upstream(t)
	stream, op := f.Built(t)
	id := op.Operation.ID
	want := []intakefixture.TransitionMove{
		{ID: "build-1", Subject: intake.BuildSubject, From: "", To: "requested-1", Cause: "seal-1", Reason: buildRequested},
		{ID: intake.BuildingState, Subject: trace.FeatureSubject, From: intake.RatifiedState, To: intake.BuildingState, Cause: id, Reason: dependentBuilt},
		{ID: "unit-resume-planned", Subject: "unit-resume", From: "", To: intake.UnitPlanned, Cause: id, Reason: "unit resume is in the plan of seal 1"},
		{ID: "unit-resume-ready", Subject: "unit-resume", From: intake.UnitPlanned, To: intake.UnitReady, Cause: id, Reason: "unit resume is ready: it depends on no unit"},
		{ID: "unit-dedupe-planned", Subject: "unit-dedupe", From: "", To: intake.UnitPlanned, Cause: id, Reason: "unit dedupe is in the plan of seal 1 and waits for resume to merge"},
	}
	if got := intakefixture.Moves(t, intakefixture.BuildTransitions(t, f.S.Current().Root, f.Project, stream)); !reflect.DeepEqual(got, want) {
		t.Fatalf("transitions:\n%+v\nwant:\n%+v", got, want)
	}
	if op.Result.Evidence != dependentBuilt {
		t.Fatalf("the result's evidence %q", op.Result.Evidence)
	}
	if body := f.Notice(t, stream, intake.BuildingState); body != "Workstream state changed from ratified to building: "+dependentBuilt {
		t.Fatalf("the notice %q", body)
	}
	states, err := f.Repository().WorkflowStates(stream)
	fixturekit.Must(t, err)
	if states["unit-resume"].Value != intake.UnitReady || states["unit-dedupe"].Value != intake.UnitPlanned {
		t.Fatalf("unit states %+v", states)
	}
	f.CheckUnits(t, stream, []api.UnitStatus{{Unit: "resume", State: intake.UnitReady}, {Unit: "dedupe", State: intake.UnitPlanned}})

	b := &intake.Builder{S: f.S.Intake().BuilderDependencies(), Repository: f.Repository()}
	fixturekit.Must(t, b.Pass(ctx))
	if ops := f.BuildOperations(t, stream); len(ops) != 1 {
		t.Fatalf("build operations after a pass %+v", ops)
	}
	observed, err := b.Inspect(ctx, op.Operation)
	fixturekit.Must(t, err)
	if observed.State != coreadapter.EffectCompleted || !reflect.DeepEqual(observed.Result, &coreadapter.OperationResult{Outcome: "succeeded", Evidence: dependentBuilt}) {
		t.Fatalf("inspection %+v", observed)
	}
	again, err := b.Apply(ctx, op.Operation)
	fixturekit.Must(t, err)
	if !reflect.DeepEqual(again, coreadapter.OperationResult{Outcome: "succeeded", Evidence: dependentBuilt}) {
		t.Fatalf("applied again %+v", again)
	}
	if got := intakefixture.Moves(t, intakefixture.BuildTransitions(t, f.S.Current().Root, f.Project, stream)); !reflect.DeepEqual(got, want) {
		t.Fatalf("transitions after the replay:\n%+v", got)
	}
}

func TestRatifiedWorkstreamWithIndependentUnitsHasEveryUnitReady(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	defer f.Stop(t)
	f.Script("draft-1-1", map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: intakefixture.IndependentPlan}, nil)
	f.Upstream(t)
	stream, op := f.Built(t)
	id := op.Operation.ID
	reason := "seal 1 is recorded; the states of the 2 units of its plan are recorded: ready resume, dedupe; planned none"
	want := []intakefixture.TransitionMove{
		{ID: "build-1", Subject: intake.BuildSubject, From: "", To: "requested-1", Cause: "seal-1", Reason: buildRequested},
		{ID: intake.BuildingState, Subject: trace.FeatureSubject, From: intake.RatifiedState, To: intake.BuildingState, Cause: id, Reason: reason},
		{ID: "unit-resume-planned", Subject: "unit-resume", From: "", To: intake.UnitPlanned, Cause: id, Reason: "unit resume is in the plan of seal 1"},
		{ID: "unit-resume-ready", Subject: "unit-resume", From: intake.UnitPlanned, To: intake.UnitReady, Cause: id, Reason: "unit resume is ready: it depends on no unit"},
		{ID: "unit-dedupe-planned", Subject: "unit-dedupe", From: "", To: intake.UnitPlanned, Cause: id, Reason: "unit dedupe is in the plan of seal 1"},
		{ID: "unit-dedupe-ready", Subject: "unit-dedupe", From: intake.UnitPlanned, To: intake.UnitReady, Cause: id, Reason: "unit dedupe is ready: it depends on no unit"},
	}
	if got := intakefixture.Moves(t, intakefixture.BuildTransitions(t, f.S.Current().Root, f.Project, stream)); !reflect.DeepEqual(got, want) {
		t.Fatalf("transitions:\n%+v\nwant:\n%+v", got, want)
	}
	f.CheckUnits(t, stream, []api.UnitStatus{{Unit: "resume", State: intake.UnitReady}, {Unit: "dedupe", State: intake.UnitReady}})
}

func TestBuildInterruptedAfterItsCommitIsNotRecordedTwice(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	defer f.Stop(t)
	f.Upstream(t)
	crashed := make(chan struct{}, 1)
	f.S.Boundary = func(name string) error {
		if name == "build-recorded" && len(crashed) == 0 {
			crashed <- struct{}{}
			return errors.New("crash")
		}
		return nil
	}
	stream, op := f.Built(t)
	f.S.Boundary = nil
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
	for _, tr := range intakefixture.BuildTransitions(t, f.S.Current().Root, f.Project, stream) {
		ids = append(ids, tr.ID)
	}
	if !slices.Equal(ids, []string{"build-1", intake.BuildingState, "unit-resume-planned", "unit-resume-ready", "unit-dedupe-planned"}) {
		t.Fatalf("transitions %v", ids)
	}
}

func TestBuildFailsWithoutRecording(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	ctx := context.Background()
	f.Stop(t)
	f.Opts.Committee = nil
	f.Start(t)
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.Await(t, stream, intakefixture.Sketched)
	other := f.HandIn(t, "other", fixturekit.HandedDesign)
	f.Await(t, other, intakefixture.Sketched)
	f.Stop(t)

	repo, err := trace.Open(f.S.Current().Root, f.S.Current().Project)
	fixturekit.Must(t, err)
	b := &intake.Builder{S: f.S.Intake().BuilderDependencies(), Repository: repo}
	h := func(id string) trace.Header {
		return trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: f.Project, Workstream: stream, At: f.Clock.Now(), Actor: intake.OwnerActor, Cause: "planted"}
	}
	_, err = repo.SetFeatureState(ctx, h("planted-ratified"), intake.RatifiedState, "planted")
	fixturekit.Must(t, err)
	record := func(k, planRevision int, docs ...trace.Document) {
		t.Helper()
		_, doc, _, err := seal.Latest(repo, stream)
		fixturekit.Must(t, err)
		content, err := seal.Encode(seal.Seal{Version: seal.Version, Seal: k, Round: 1, Revision: shed.Pin{Spec: 1, Plan: planRevision}, SpecHash: seal.SpecHash(fixturekit.ValidSpec),
			Base: seal.Base{Remote: "upstream", Branch: "main", Commit: "0123456789abcdef0123456789abcdef01234567"}, Branch: "osmia/" + string(stream), Workspace: "/planted"})
		fixturekit.Must(t, err)
		docs = append(docs, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: seal.DocumentID, Revision: doc.Revision + 1, Project: f.Project, Workstream: stream, At: f.Clock.Now(), Actor: intake.SealingActor, Cause: "planted"}, Path: seal.Path, Content: string(content)})
		fixturekit.Must(t, repo.RecordDocuments(ctx, docs))
	}
	record(1, 1)
	var recorded int
	apply := func(k int, want string) {
		t.Helper()
		got, err := b.Apply(ctx, f.BuildOperation(stream, k))
		fixturekit.Must(t, err)
		if got.Outcome != "failed" || !strings.HasPrefix(got.Evidence, want) {
			t.Fatalf("build on seal %d: %+v, want %q", k, got, want)
		}
		if n := len(intakefixture.BuildTransitions(t, f.S.Current().Root, f.Project, stream)); n != recorded {
			t.Fatalf("build on seal %d recorded %d transitions, had %d", k, n, recorded)
		}
	}
	apply(2, "building on seal 2 failed: it is not the latest seal of the workstream")

	_, err = repo.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: h("planted-unit"), Subject: trace.UnitSubject("dedupe"), To: intake.UnitPlanned, Reason: "planted"}})
	fixturekit.Must(t, err)
	recorded = len(intakefixture.BuildTransitions(t, f.S.Current().Root, f.Project, stream))
	apply(1, "building on seal 1 failed: the workflow changed while the build ran; the workstream is ratified")

	record(2, 2, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: plan.PlanDocument, Revision: 2, Project: f.Project, Workstream: stream, At: f.Clock.Now(), Actor: intake.OwnerActor, Cause: "planted"}, Path: plan.PlanPath, Content: "not a plan"})
	apply(2, "building on seal 2 failed: the sealed plan does not parse: ")

	fixturekit.Must(t, b.Pass(ctx))
	fixturekit.Must(t, b.Pass(ctx))
	var requests []string
	for _, tr := range intakefixture.BuildTransitions(t, f.S.Current().Root, f.Project, stream) {
		if tr.Subject == intake.BuildSubject {
			requests = append(requests, tr.From+"->"+tr.To)
		}
	}
	if !slices.Equal(requests, []string{"->requested-2"}) {
		t.Fatalf("build requests %v", requests)
	}
	recorded++

	_, err = repo.SetFeatureState(ctx, h(intake.AbandonTransition), intake.AbandonedState, "Not needed.")
	fixturekit.Must(t, err)
	apply(2, "building on seal 2 failed: the workstream is abandoned, not ratified")

	fixturekit.Must(t, b.Pass(ctx))
	if n := len(intakefixture.BuildTransitions(t, f.S.Current().Root, f.Project, stream)); n != recorded {
		t.Fatalf("a pass over the abandoned workstream recorded %d transitions, had %d", n, recorded)
	}
	fixturekit.Must(t, repo.Close())

	f.Start(t)
	defer f.Stop(t)
	message := "cannot read the unit states of workstream " + string(stream) + "; check the trace repository"
	if _, err := f.C.Status(ctx, stream); !intakefixture.Failed(err, api.Internal) || !strings.Contains(err.Error(), message) {
		t.Fatalf("status of the workstream: %v", err)
	}
	healthy, err := f.C.Status(ctx, other)
	fixturekit.Must(t, err)
	if healthy.State == nil || *healthy.State != intake.SketchedState || !reflect.DeepEqual(healthy.Units, []api.UnitStatus{}) {
		t.Fatalf("status of the other workstream %+v", healthy)
	}
	list, err := f.C.Statuses(ctx)
	fixturekit.Must(t, err)
	if len(list.Workstreams) != 2 || !reflect.DeepEqual(list.Diagnostics, []api.Diagnostic{{Field: "units", Code: api.Internal, Message: message}}) {
		t.Fatalf("status list %+v", list)
	}
	for _, w := range list.Workstreams {
		if !reflect.DeepEqual(w.Units, []api.UnitStatus{}) {
			t.Fatalf("units of %s: %+v", w.Workstream, w.Units)
		}
	}
}
