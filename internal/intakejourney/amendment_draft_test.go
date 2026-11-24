package intakejourney

import (
	context "context"
	json "encoding/json"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	slices "slices"
	strings "strings"
	testing "testing"

	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	plan "github.com/kpenfound/osmia/internal/plan"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func seedAmendment(t *testing.T, f *journey.ArchitectFixture, repo *trace.Repository, stream config.WorkstreamID) {
	t.Helper()
	intakefixture.SeedAmendment(t, f.Project, f.Clock.Now, repo, stream)
}

func fileAmendment(t *testing.T, f *journey.ArchitectFixture, repo *trace.Repository, stream config.WorkstreamID, id string) {
	t.Helper()
	intakefixture.FileAmendment(t, f.Project, f.Clock.Now, repo, stream, id)
}

func TestArchitectAmendmentDrafts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name            string
		files           map[string]string
		want            string
		merged          bool
		criteria, units []string
	}{
		{"criterion", map[string]string{plan.SpecPath: strings.Replace(fixturekit.ValidSpec, "An interrupted upload resumes from the last acknowledged chunk.", "An interrupted upload resumes from a durable checkpoint.", 1)}, "proposed", false, []string{"spec#1"}, []string{"resume"}},
		{"plan-only", map[string]string{plan.PlanPath: strings.Replace(fixturekit.ValidPlan, "TestResume", "TestResumeAfterRestart", 1)}, "proposed", false, []string{}, []string{"resume"}},
		{"invalid", map[string]string{plan.PlanPath: fixturekit.CyclicPlan}, "invalid", false, nil, nil},
		{"whole-suite-criterion", map[string]string{plan.SpecPath: strings.Replace(fixturekit.ValidSpec, "An interrupted upload resumes from the last acknowledged chunk.", "All checks pass", 1)}, "invalid", false, nil, nil},
		{"whole-suite-unit", map[string]string{plan.PlanPath: intakefixture.AcceptancePlan(t, fixturekit.ValidPlan, "dagger check passes")}, "invalid", false, nil, nil},
		{"merged", map[string]string{plan.PlanPath: strings.Replace(fixturekit.ValidPlan, "TestResume", "TestResumeAfterRestart", 1)}, "invalid", true, nil, nil},
		{"declined", map[string]string{"decline.txt": "The requested change contradicts the charter."}, "declined", false, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := journey.NewArchitectFixture(t)

			f.Script("draft-1-1", map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.ValidPlan}, nil)
			f.Script("amend-1-1", tc.files, nil)
			stream := f.HandIn(t, "amend-"+tc.name, fixturekit.HandedDesign)
			f.Await(t, stream, intakefixture.Sketched)
			f.Stop(t)
			repo, err := trace.Open(f.S.Current().Root, f.S.Current().Project)
			fixturekit.Must(t, err)
			defer func() { _ = repo.Close() }()
			seedAmendment(t, f, repo, stream)
			if tc.name == "plan-only" {
				_, err := repo.MoveFeatureState(context.Background(), trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "assembled", Revision: 1, Project: f.Project, Workstream: stream, At: f.Clock.Now(), Actor: intake.OwnerActor, Cause: "fixture"}, intake.BuildingState, intake.AssembledState, "final review began")
				fixturekit.Must(t, err)
			}
			if tc.merged {
				_, err := repo.Transact(context.Background(), trace.Transaction{ExpectedVersion: 0, Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "unit-resume-merged", Revision: 1, Project: f.Project, Workstream: stream, Unit: "resume", At: f.Clock.Now(), Actor: intake.OwnerActor, Cause: "fixture"}, Subject: trace.UnitSubject("resume"), To: "merged", Reason: "landed"}})
				fixturekit.Must(t, err)
			}
			a := &intake.AmendmentDrafter{Drafter: &intake.Drafter{S: f.S.Intake().DrafterDependencies(), Repository: repo}}
			fixturekit.Must(t, a.Pass(context.Background()))
			ops, err := repo.Operations(stream)
			fixturekit.Must(t, err)
			var operation *trace.OperationRecord
			for i := range ops {
				if ops[i].Operation.Action == intake.AmendmentDraftAction {
					operation = &ops[i]
				}
			}
			if operation == nil {
				t.Fatal("amendment draft operation missing")
			}
			observation, err := a.Inspect(context.Background(), operation.Operation)
			fixturekit.Must(t, err)
			if observation.State != coreadapter.EffectAbsent || observation.Evidence == "" {
				t.Fatalf("absent amendment observation has no evidence: %+v", observation)
			}
			if tc.name == "plan-only" {
				profile, _, err := f.S.RoleExecution(f.S.Current(), intake.ArchitectRole)
				fixturekit.Must(t, err)
				turn := "amend-1-1"
				request := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: f.Project, Workstream: stream, At: f.Clock.Now(), Actor: intake.DraftingActor, Cause: operation.Operation.ID, Depth: 1}, AgentID: intake.ArchitectAgent, ThreadID: intake.ArchitectThread, TurnID: turn, Profile: profile, SystemPrompt: "Draft the amendment", Prompt: "Draft the amendment from request.json and draft/."}
				_, err = repo.EnqueueTurn(context.Background(), request)
				fixturekit.Must(t, err)
				_, err = a.Dispatch(context.Background(), stream, turn)
				fixturekit.Must(t, err)
				fixturekit.Must(t, repo.Close())
				repo, err = trace.Open(f.S.Current().Root, f.S.Current().Project)
				fixturekit.Must(t, err)
				a = &intake.AmendmentDrafter{Drafter: &intake.Drafter{S: f.S.Intake().DrafterDependencies(), Repository: repo}}
			}
			_, err = a.Apply(context.Background(), operation.Operation)
			fixturekit.Must(t, err)
			fixturekit.Must(t, repo.Close())
			repo, err = trace.Open(f.S.Current().Root, f.S.Current().Project)
			fixturekit.Must(t, err)
			a = &intake.AmendmentDrafter{Drafter: &intake.Drafter{S: f.S.Intake().DrafterDependencies(), Repository: repo}}
			_, err = a.Apply(context.Background(), operation.Operation)
			fixturekit.Must(t, err)
			fixturekit.Must(t, a.Pass(context.Background()))
			if runs := f.Runs(); len(runs) != 2 {
				t.Fatalf("architect ran after recovery: %v", runs)
			}
			state, err := repo.Workflow(stream, "amendment_1")
			fixturekit.Must(t, err)
			got := state.Value
			if got != tc.want {
				t.Fatalf("state %s, want %s", got, tc.want)
			}

			verdict := map[string]string{"proposed": intake.DraftValid, "invalid": intake.DraftInvalid, "declined": intake.DraftDeclined}[tc.want]
			if delivered := f.Delivered("amend-1-1"); len(delivered) != len(tc.files) || delivered[len(delivered)-1].Verdict != verdict {
				t.Fatalf("deliveries %+v, want verdict %s", delivered, verdict)
			}
			feature, err := repo.Workflow(stream, trace.FeatureSubject)
			fixturekit.Must(t, err)
			wantFeature := intake.BuildingState
			if tc.name == "plan-only" {
				wantFeature = intake.AssembledState
			}
			if feature.Value != wantFeature {
				t.Fatalf("feature became %s", feature.Value)
			}
			if docs := intakefixture.ReadDocs(t, repo, stream, plan.SpecDocument); len(docs) != 1 || docs[0].Content != fixturekit.ValidSpec {
				t.Fatalf("sealed spec changed: %+v", docs)
			}
			if docs := intakefixture.ReadDocs(t, repo, stream, plan.PlanDocument); len(docs) != 1 || docs[0].Content != fixturekit.ValidPlan {
				t.Fatalf("sealed plan changed: %+v", docs)
			}
			docs, err := trace.Read[trace.Document](repo, stream)
			fixturekit.Must(t, err)
			var candidate []trace.Document
			for _, doc := range docs {
				if strings.HasPrefix(doc.Path, "amendments/1/") {
					candidate = append(candidate, doc)
				}
			}
			if tc.want == "invalid" || tc.want == "declined" {
				if len(candidate) != 0 {
					t.Fatalf("invalid candidate recorded: %+v", candidate)
				}
				return
			}
			if len(candidate) != 3 {
				t.Fatalf("candidate docs: %+v", candidate)
			}
			data, err := fixturekit.WorkstreamDocument(t, f.S.Current().Root, f.Project, stream, "amendments/1/affected.json")
			fixturekit.Must(t, err)
			var affected intake.AmendmentAffected
			fixturekit.Must(t, json.Unmarshal(data, &affected))
			if !slices.Equal(affected.Criteria, tc.criteria) || !slices.Equal(affected.Units, tc.units) {
				t.Fatalf("affected %+v", affected)
			}
		})
	}
}

func TestAmendmentDraftReconcilesThroughRunningService(t *testing.T) {
	t.Parallel()
	f := journey.NewArchitectFixture(t)
	t.Cleanup(func() { f.Stop(t) })
	f.Script("draft-1-1", map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.ValidPlan}, nil)
	f.Script("amend-1-1", map[string]string{
		plan.SpecPath: strings.Replace(fixturekit.ValidSpec, "An interrupted upload resumes from the last acknowledged chunk.", "An interrupted upload resumes from a durable checkpoint.", 1),
	}, nil)
	workstream := f.HandIn(t, "amendment-reconcile", fixturekit.HandedDesign)
	f.Await(t, workstream, intakefixture.Sketched)

	seedAmendment(t, f, f.Repository(), workstream)
	operations := fixturekit.AwaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord {
		t.Helper()
		ops, err := f.Repository().Operations(workstream)
		fixturekit.Must(t, err)
		var drafts []trace.OperationRecord
		for _, op := range ops {
			if op.Operation.Action == intake.AmendmentDraftAction {
				drafts = append(drafts, op)
			}
		}
		return drafts
	})
	if len(operations) != 1 || operations[0].Result == nil || operations[0].Result.Outcome != "succeeded" {
		t.Fatalf("amendment draft operation did not succeed: %+v", operations)
	}
	for _, action := range operations[0].History {
		if action.Kind == "observe" && (action.Observation == nil || action.Observation.Evidence == "") {
			t.Fatalf("amendment observation has no evidence: %+v", action)
		}
	}
	state, err := f.Repository().Workflow(workstream, "amendment_1")
	fixturekit.Must(t, err)
	if state.Value != "proposed" {
		t.Fatalf("amendment state %q, want proposed", state.Value)
	}
	feature, err := f.Repository().Workflow(workstream, trace.FeatureSubject)
	fixturekit.Must(t, err)
	if feature.Value != intake.BuildingState {
		t.Fatalf("feature state %q, want %q", feature.Value, intake.BuildingState)
	}
	adapter := &intake.AmendmentDrafter{Drafter: &intake.Drafter{S: f.S.Intake().DrafterDependencies(), Repository: f.Repository()}}
	observation, err := adapter.Inspect(context.Background(), operations[0].Operation)
	fixturekit.Must(t, err)
	if observation.State != coreadapter.EffectCompleted || observation.Evidence == "" {
		t.Fatalf("completed amendment observation has no evidence: %+v", observation)
	}
	health, err := f.C.Health(context.Background())
	fixturekit.Must(t, err)
	if !health.Ready {
		t.Fatalf("service is not ready after amendment reconciliation: %+v", health)
	}
}

func TestAmendmentDraftInspectRunningTurnHasEvidence(t *testing.T) {
	t.Parallel()
	f := journey.NewArchitectFixture(t)
	f.Script("draft-1-1", map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.ValidPlan}, nil)
	workstream := f.HandIn(t, "amendment-inspect-running", fixturekit.HandedDesign)
	f.Await(t, workstream, intakefixture.Sketched)
	f.Stop(t)

	repo, err := trace.Open(f.S.Current().Root, f.S.Current().Project)
	fixturekit.Must(t, err)
	defer repo.Close()
	seedAmendment(t, f, repo, workstream)
	adapter := &intake.AmendmentDrafter{Drafter: &intake.Drafter{S: f.S.Intake().DrafterDependencies(), Repository: repo}}
	fixturekit.Must(t, adapter.Pass(context.Background()))
	operations, err := repo.Operations(workstream)
	fixturekit.Must(t, err)
	var operation *trace.OperationRecord
	for i := range operations {
		if operations[i].Operation.Action == intake.AmendmentDraftAction {
			operation = &operations[i]
			break
		}
	}
	if operation == nil {
		t.Fatal("amendment draft operation missing")
	}
	profile, _, err := f.S.RoleExecution(f.S.Current(), intake.ArchitectRole)
	fixturekit.Must(t, err)
	turn := "amend-1-1"
	request := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: f.Project, Workstream: workstream, At: f.Clock.Now(), Actor: intake.DraftingActor, Cause: operation.Operation.ID, Depth: 1}, AgentID: intake.ArchitectAgent, ThreadID: intake.ArchitectThread, TurnID: turn, Profile: profile, SystemPrompt: "Draft the amendment", Prompt: "Draft the amendment from request.json and draft/."}
	_, err = repo.EnqueueTurn(context.Background(), request)
	fixturekit.Must(t, err)
	_, err = repo.ClaimTurn(context.Background(), workstream, intake.ArchitectAgent, "running-amendment-turn", "fixture-session", f.Clock.Now())
	fixturekit.Must(t, err)
	observation, err := adapter.Inspect(context.Background(), operation.Operation)
	fixturekit.Must(t, err)
	if observation.State != coreadapter.EffectUnknown || observation.Evidence == "" {
		t.Fatalf("running amendment observation has no evidence: %+v", observation)
	}
}

func TestAmendmentAffectedSetIncludesEveryServingUnit(t *testing.T) {
	t.Parallel()
	graph, err := plan.Parse([]byte(journey.ParallelPlan))
	fixturekit.Must(t, err)
	revised := strings.Replace(fixturekit.ValidSpec, "1. An interrupted upload resumes from the last acknowledged chunk.", "1. An interrupted upload resumes from a durable checkpoint.", 1)
	got := intake.AffectedRevision(fixturekit.ValidSpec, revised, graph, graph)
	if !slices.Equal(got.Criteria, []string{"spec#1"}) || !slices.Equal(got.Units, []string{"resume", "upload"}) {
		t.Fatalf("affected set %+v", got)
	}
}
