package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/trace"
)

func seedAmendment(t *testing.T, f *architectFixture, repo *trace.Repository, stream config.WorkstreamID) {
	t.Helper()
	ctx := context.Background()
	_, err := repo.MoveFeatureState(ctx, trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "building", Revision: 1, Project: f.project, Workstream: stream, At: f.clock.Now(), Actor: ownerActor, Cause: "fixture"}, SketchedState, BuildingState, "build")
	if err != nil {
		t.Fatalf("move feature to building: %v", err)
	}
	request := trace.Amendment{Header: trace.Header{Schema: "osmia.trace.amendment", Version: trace.Version, ID: "1", Revision: 1, Project: f.project, Workstream: stream, Unit: "resume", At: f.clock.Now(), Actor: trace.Actor{Kind: "agent", ID: "agent_mason_resume"}, Cause: "fixture"}, Requester: trace.Actor{Kind: "agent", ID: "agent_mason_resume"}, Role: "mason", Thread: "thread_mason_resume", Turn: "turn_resume", Citations: []string{"spec#1"}, Change: "Revise the criterion", Reason: "New constraint", Seal: 1, SealRevision: 1, SpecHash: "hash"}
	if err := repo.Append(ctx, request); err != nil {
		t.Fatalf("append request: %v", err)
	}
	_, err = repo.Transact(ctx, trace.Transaction{ExpectedVersion: 0, Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "amendment_1_filed", Revision: 1, Project: f.project, Workstream: stream, At: f.clock.Now(), Actor: request.Actor, Cause: request.Cause}, Subject: "amendment_1", To: "filed", Reason: "filed"}})
	if err != nil {
		t.Fatalf("file request: %v", err)
	}
}

func TestArchitectAmendmentDrafts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                    string
		files                   map[string]string
		want                    string
		merged                  bool
		criteria, units, proofs []string
	}{
		{"criterion", map[string]string{plan.SpecPath: strings.Replace(validSpec, "An interrupted upload resumes from the last acknowledged chunk.", "An interrupted upload resumes from a durable checkpoint.", 1)}, "proposed", false, []string{"spec#1"}, []string{"resume"}, []string{"resume:spec#1"}},
		{"plan-only", map[string]string{plan.PlanPath: strings.Replace(validPlan, "TestResume", "TestResumeAfterRestart", 1)}, "proposed", false, []string{}, []string{"resume"}, []string{"resume:spec#1"}},
		{"invalid", map[string]string{plan.PlanPath: cyclicPlan}, "invalid", false, nil, nil, nil},
		{"merged", map[string]string{plan.PlanPath: strings.Replace(validPlan, "TestResume", "TestResumeAfterRestart", 1)}, "invalid", true, nil, nil, nil},
		{"declined", map[string]string{"decline.txt": "The requested change contradicts the charter."}, "declined", false, nil, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newArchitectFixture(t)

			f.script("draft-1-1", map[string]string{plan.SpecPath: validSpec, plan.PlanPath: validPlan}, nil)
			f.script("amend-1-1", tc.files, nil)
			stream := f.handIn(t, "amend-"+tc.name, handedDesign)
			f.await(t, stream, sketched)
			f.stop(t)
			repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
			must(t, err)
			defer func() { _ = repo.Close() }()
			seedAmendment(t, f, repo, stream)
			if tc.name == "plan-only" {
				_, err := repo.MoveFeatureState(context.Background(), trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "assembled", Revision: 1, Project: f.project, Workstream: stream, At: f.clock.Now(), Actor: ownerActor, Cause: "fixture"}, BuildingState, AssembledState, "final review began")
				must(t, err)
			}
			if tc.merged {
				_, err := repo.Transact(context.Background(), trace.Transaction{ExpectedVersion: 0, Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "unit-resume-merged", Revision: 1, Project: f.project, Workstream: stream, Unit: "resume", At: f.clock.Now(), Actor: ownerActor, Cause: "fixture"}, Subject: trace.UnitSubject("resume"), To: "merged", Reason: "landed"}})
				must(t, err)
			}
			a := &amendmentDrafter{&drafter{s: f.s, repository: repo}}
			must(t, a.Pass(context.Background()))
			ops, err := repo.Operations(stream)
			must(t, err)
			var operation *trace.OperationRecord
			for i := range ops {
				if ops[i].Operation.Action == AmendmentDraftAction {
					operation = &ops[i]
				}
			}
			if operation == nil {
				t.Fatal("amendment draft operation missing")
			}
			if tc.name == "plan-only" {
				profile, _, err := f.s.roleExecution(f.s.current(), architectRole)
				must(t, err)
				turn := "amend-1-1"
				request := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: f.project, Workstream: stream, At: f.clock.Now(), Actor: draftingActor, Cause: operation.Operation.ID, Depth: 1}, AgentID: architectAgent, ThreadID: architectThread, TurnID: turn, Profile: profile, SystemPrompt: "Draft the amendment", Prompt: "Draft the amendment from request.json and draft/."}
				_, err = repo.EnqueueTurn(context.Background(), request)
				must(t, err)
				_, err = a.dispatch(context.Background(), stream, turn)
				must(t, err)
				must(t, repo.Close())
				repo, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
				must(t, err)
				a = &amendmentDrafter{&drafter{s: f.s, repository: repo}}
			}
			_, err = a.Apply(context.Background(), operation.Operation)
			must(t, err)
			must(t, repo.Close())
			repo, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
			must(t, err)
			a = &amendmentDrafter{&drafter{s: f.s, repository: repo}}
			_, err = a.Apply(context.Background(), operation.Operation)
			must(t, err)
			must(t, a.Pass(context.Background()))
			if runs := f.runs(); len(runs) != 2 {
				t.Fatalf("architect ran after recovery: %v", runs)
			}
			state, err := repo.Workflow(stream, "amendment_1")
			must(t, err)
			got := state.Value
			if got != tc.want {
				t.Fatalf("state %s, want %s", got, tc.want)
			}
			feature, err := repo.Workflow(stream, trace.FeatureSubject)
			must(t, err)
			wantFeature := BuildingState
			if tc.name == "plan-only" {
				wantFeature = AssembledState
			}
			if feature.Value != wantFeature {
				t.Fatalf("feature became %s", feature.Value)
			}
			if docs := readDocs(t, repo, stream, plan.SpecDocument); len(docs) != 1 || docs[0].Content != validSpec {
				t.Fatalf("sealed spec changed: %+v", docs)
			}
			if docs := readDocs(t, repo, stream, plan.PlanDocument); len(docs) != 1 || docs[0].Content != validPlan {
				t.Fatalf("sealed plan changed: %+v", docs)
			}
			docs, err := trace.Read[trace.Document](repo, stream)
			must(t, err)
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
			data, err := os.ReadFile(filepath.Join(f.trace, "workstreams", string(stream), "amendments", "1", "affected.json"))
			must(t, err)
			var affected amendmentAffected
			must(t, json.Unmarshal(data, &affected))
			if !slices.Equal(affected.Criteria, tc.criteria) || !slices.Equal(affected.Units, tc.units) || !slices.Equal(affected.Proofs, tc.proofs) {
				t.Fatalf("affected %+v", affected)
			}
		})
	}
}

func readDocs(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, id string) []trace.Document {
	t.Helper()
	all, err := trace.Read[trace.Document](repo, stream)
	must(t, err)
	var docs []trace.Document
	for _, doc := range all {
		if doc.ID == id {
			docs = append(docs, doc)
		}
	}
	return docs
}

func TestAmendmentAffectedSetIncludesEveryAddressingUnit(t *testing.T) {
	t.Parallel()
	graph, err := plan.Parse([]byte(parallelPlan))
	must(t, err)
	revised := strings.Replace(validSpec, "1. An interrupted upload resumes from the last acknowledged chunk.", "1. An interrupted upload resumes from a durable checkpoint.", 1)
	got := affectedRevision(validSpec, revised, graph, graph)
	if !slices.Equal(got.Criteria, []string{"spec#1"}) || !slices.Equal(got.Units, []string{"resume", "upload"}) || !slices.Equal(got.Proofs, []string{"resume:spec#1", "upload:spec#1"}) {
		t.Fatalf("affected set %+v", got)
	}
}
