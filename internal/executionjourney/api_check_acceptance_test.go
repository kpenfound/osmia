package executionjourney

import (
	context "context"
	json "encoding/json"
	os "os"
	slices "slices"
	strings "strings"
	testing "testing"

	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	amendment "github.com/kpenfound/osmia/internal/amendment"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	kb "github.com/kpenfound/osmia/internal/kb"
	plan "github.com/kpenfound/osmia/internal/plan"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func TestAcceptanceFactoryInstructions(t *testing.T) {
	t.Parallel()
	intakefixture.AssertAcceptanceDraftInstructions(t, intake.ArchitectPrompt("handed/stdin", 1, "", false, "", nil))
	for _, role := range []string{intake.ArchitectRole, intake.CommitteeRole} {
		policy := service.FactoryInstructions(coreadapter.Scope{Role: role})
		for _, want := range []string{"whole check suite to pass or run", "asking a reviewer to show which tests ran", "engineering defect to correct", "service-owned check gate already runs checks with smart-check selection"} {
			if !strings.Contains(policy, want) {
				t.Errorf("%s policy lacks %q: %s", role, want, policy)
			}
		}
	}
	intakefixture.AssertCheckGateReviewInstructions(t, service.FactoryInstructions(coreadapter.Scope{Role: intake.ReviewerRole}))
	intakefixture.AssertCheckGateReviewInstructions(t, service.FactoryInstructions(coreadapter.Scope{Role: intake.CommitteeRole}))
	assertFinalCheckReviewInstructions(t, delivery.FinalSystemPrompt(config.Project{}))

	assertFinalCheckReviewInstructions(t, delivery.FinalSystemPrompt(config.Project{})+service.FactoryInstructions(coreadapter.Scope{Role: intake.CommitteeRole, Turn: "final-3-committee-1-1"}))
}

func assertFinalCheckReviewInstructions(t *testing.T, prompt string) {
	t.Helper()
	for _, want := range []string{"A recorded pass satisfies every criterion that needs tests or checks to pass", "A recorded failure is evidence you may cite", "is not a finding by itself"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("final review instructions lack %q: %s", want, prompt)
		}
	}
	for _, unwanted := range []string{"check gate", "for the candidate", "smart-check selection"} {
		if strings.Contains(prompt, unwanted) {
			t.Errorf("final review instructions carry the unit-review wording %q: %s", unwanted, prompt)
		}
	}
}

func TestDraftWriteCheckAcceptance(t *testing.T) {
	t.Parallel()
	ownSpec, err := os.ReadFile("testdata/check-acceptance/spec.md")
	fixturekit.Must(t, err)
	ownPlan, err := os.ReadFile("testdata/check-acceptance/plan.json")
	fixturekit.Must(t, err)
	mentions := []string{"acceptance never requires all checks to pass", "the validator rejects acceptance saying 'all checks pass'", "table tests flag 'full check run is green'", "all internal/plan tests pass"}
	mentionSpec := "## Acceptance criteria\n1. " + strings.Join(mentions, ". ") + "\n2. Acknowledged chunks are never sent again.\n"
	cases := []struct{ name, spec, graph, at string }{
		{"criterion", strings.Replace(fixturekit.ValidSpec, "An interrupted upload resumes from the last acknowledged chunk.", "All `dagger checks` pass", 1), fixturekit.ValidPlan, "spec#1"},
		{"unit", fixturekit.ValidSpec, intakefixture.AcceptancePlan(t, fixturekit.ValidPlan, "dagger check passes"), `unit "resume": acceptance item 1`},
		{"scoped-run-criterion", strings.Replace(fixturekit.ValidSpec, "An interrupted upload resumes from the last acknowledged chunk.", "Run all checks in internal/plan", 1), fixturekit.ValidPlan, ""},
		{"scoped-run-unit", fixturekit.ValidSpec, intakefixture.AcceptancePlan(t, fixturekit.ValidPlan, "Run all checks in internal/plan", "Execute every test in the parser suite"), ""},
		{"relative-scope-criterion", strings.Replace(fixturekit.ValidSpec, "An interrupted upload resumes from the last acknowledged chunk.", "Run all checks in ./internal/plan. All checks must be run under ./internal/plan.", 1), fixturekit.ValidPlan, ""},
		{"relative-scope-unit", fixturekit.ValidSpec, intakefixture.AcceptancePlan(t, fixturekit.ValidPlan, "Run all checks in ./internal/plan", "All checks must be run under ./internal/plan", "All checks pass in ./internal/plan"), ""},
		{"backticked-relative-scope-criterion", strings.Replace(fixturekit.ValidSpec, "An interrupted upload resumes from the last acknowledged chunk.", "Run all checks in `./internal/plan`. All checks must be run under `./internal/plan`.", 1), fixturekit.ValidPlan, ""},
		{"backticked-relative-scope-unit", fixturekit.ValidSpec, intakefixture.AcceptancePlan(t, fixturekit.ValidPlan, "Run all checks in `./internal/plan`", "All checks must be run under `./internal/plan`", "All checks pass in `./internal/plan`"), ""},
		{"unscoped-run-criterion", strings.Replace(fixturekit.ValidSpec, "An interrupted upload resumes from the last acknowledged chunk.", "Run all checks", 1), fixturekit.ValidPlan, "spec#1"},
		{"unscoped-run-unit", fixturekit.ValidSpec, intakefixture.AcceptancePlan(t, fixturekit.ValidPlan, "Run all checks"), `unit "resume": acceptance item 1`},
		{"run-backticked-command-criterion", strings.Replace(fixturekit.ValidSpec, "An interrupted upload resumes from the last acknowledged chunk.", "Run `dagger check`", 1), fixturekit.ValidPlan, "spec#1"},
		{"run-backticked-subject-unit", fixturekit.ValidSpec, intakefixture.AcceptancePlan(t, fixturekit.ValidPlan, "Run `all checks`"), `unit "resume": acceptance item 1`},
		{"relative-scope-then-unscoped-criterion", strings.Replace(fixturekit.ValidSpec, "An interrupted upload resumes from the last acknowledged chunk.", "Run all checks in ./internal/plan. Run all checks.", 1), fixturekit.ValidPlan, "spec#1"},
		{"relative-scope-then-unscoped-unit", fixturekit.ValidSpec, intakefixture.AcceptancePlan(t, fixturekit.ValidPlan, "Run all checks in `./internal/plan`. All checks pass."), `unit "resume": acceptance item 1`},
		{"mentions", mentionSpec, intakefixture.AcceptancePlan(t, fixturekit.ValidPlan, mentions...), ""},
		{"own-feature", string(ownSpec), string(ownPlan), ""},
	}
	for _, quote := range []struct{ name, open, close string }{
		{"double", `"`, `"`},
		{"curly-double", "“", "”"},
		{"single", "'", "'"},
		{"curly-single", "‘", "’"},
		{"backtick", "`", "`"},
	} {
		items := []string{
			"The help page displays " + quote.open + "Run all checks" + quote.close + ".",
			"The help page renders " + quote.open + "Run the full check suite" + quote.close + ".",
			"The help page displays " + quote.open + "All checks pass" + quote.close + ".",
			"The validator rejects " + quote.open + "Run all checks" + quote.close + ".",
		}
		spec := "## Acceptance criteria\n1. " + strings.Join(items, " ") + "\n2. Acknowledged chunks are never sent again.\n"
		cases = append(cases, struct{ name, spec, graph, at string }{"quoted-statements-" + quote.name, spec, intakefixture.AcceptancePlan(t, fixturekit.ValidPlan, items...), ""})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := journey.NewArchitectFixture(t)
			defer f.Stop(t)

			entities, err := kb.Load(f.Repository())
			fixturekit.Must(t, err)
			if tc.name == "own-feature" {
				entities.Entities = append(entities.Entities, kb.Entity{ID: "internal.service", Name: "Service", Paths: []string{"internal/service/**"}}, kb.Entity{ID: "internal.plan", Name: "Plan", Paths: []string{"internal/plan/**"}})
				fixturekit.Must(t, kb.Store(context.Background(), f.Repository(), entities, f.Clock.Now(), intake.ServiceActor, "fixture"))
				charter, err := f.Repository().Charter(context.Background())
				fixturekit.Must(t, err)
				fixturekit.RecordCharter(t, f.Repository(), charter.Content+"\n4. Run dagger check with --parallel 4.\n")
			}
			f.Script("draft-1-1", map[string]string{plan.SpecPath: tc.spec, plan.PlanPath: tc.graph}, nil)
			f.Script("draft-2-1", map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.ValidPlan}, nil)
			stream := f.HandIn(t, "acceptance-"+tc.name, fixturekit.HandedDesign)
			f.Await(t, stream, intakefixture.Sketched)
			deliveries := f.Delivered("draft-1-1")
			if len(deliveries) != 2 {
				t.Fatalf("deliveries: %+v", deliveries)
			}
			got := deliveries[1]
			if tc.at == "" {
				if got.Verdict != intake.DraftValid || len(got.Problems) != 0 {
					t.Fatalf("valid mentions: %+v", got)
				}
				for _, doc := range []struct{ id, content string }{{plan.SpecDocument, tc.spec}, {plan.PlanDocument, tc.graph}} {
					if docs := f.Documents(t, stream, doc.id); len(docs) != 1 || docs[0].Content != doc.content {
						t.Fatalf("valid draft not recorded: %+v", docs)
					}
				}
				return
			}
			if got.Verdict != intake.DraftInvalid || len(got.Problems) != 1 || !strings.Contains(got.Problems[0], tc.at) {
				t.Fatalf("verdict: %+v", got)
			}
			invalid := slices.DeleteFunc(f.Transitions(t, stream), func(tr trace.Transition) bool { return tr.ID != "draft-1-invalid" })
			if len(invalid) != 1 || invalid[0].Reason != "draft 1 of the spec and plan is invalid:\n- "+got.Problems[0] {
				t.Fatalf("recorded verdict differs: %+v", invalid)
			}
			if docs := f.Documents(t, stream, plan.PlanDocument); len(docs) != 2 || docs[0].Content != tc.graph {
				t.Fatalf("draft not recorded: %+v", docs)
			}
		})
	}
}

func TestAmendmentCheckAcceptance(t *testing.T) {
	t.Parallel()
	f := journey.NewArchitectFixture(t)
	f.Script("draft-1-1", map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.ValidPlan}, nil)
	stream := f.HandIn(t, "amend-check-acceptance", fixturekit.HandedDesign)
	f.Await(t, stream, intakefixture.Sketched)
	f.Stop(t)
	repo, err := trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
	fixturekit.Must(t, err)
	defer repo.Close()

	oldSpec := strings.Replace(fixturekit.ValidSpec, "An interrupted upload resumes from the last acknowledged chunk.", "All checks pass", 1)
	oldGraph := intakefixture.AcceptancePlan(t, fixturekit.ValidPlan, "dagger check passes", "A checkpoint is durable")
	docs := []trace.Document{}
	for _, item := range []struct{ id, path, text string }{{plan.SpecDocument, plan.SpecPath, oldSpec}, {plan.PlanDocument, plan.PlanPath, oldGraph}} {
		docs = append(docs, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: item.id, Revision: 2, Project: repo.Project(), Workstream: stream, At: f.Clock.Now(), Actor: intake.ArchitectActor, Cause: "fixture"}, Path: item.path, Content: item.text})
	}
	fixturekit.Must(t, repo.RecordDocuments(context.Background(), docs))
	a := &intake.AmendmentDrafter{Drafter: &intake.Drafter{S: f.S.Intake().DrafterDependencies(), Repository: repo}}
	for _, tc := range []struct{ name, spec, graph, at string }{
		{"carryover", strings.Replace(oldSpec, "Acknowledged chunks", "Durably acknowledged chunks", 1), strings.Replace(oldGraph, "A checkpoint is durable", "A checkpoint survives restart", 1), ""},
		{"changed-criterion", strings.Replace(oldSpec, "All checks pass", "Full check run is green", 1), oldGraph, "spec#1"},
		{"changed-item", oldSpec, intakefixture.AcceptancePlan(t, oldGraph, "All tests pass"), `unit "resume": acceptance item 1`},
		{"added-item", oldSpec, intakefixture.AcceptancePlan(t, oldGraph, "dagger check passes", "A checkpoint is durable", "Run the full test suite"), `unit "resume": acceptance item 3`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope := coreadapter.Scope{Project: string(repo.Project()), Workstream: string(stream), Role: intake.ArchitectRole}
			tool := a.DraftTool(scope, "amend-"+tc.name+"-1", a.Judge)
			for _, file := range []struct{ path, text string }{{plan.SpecPath, tc.spec}, {plan.PlanPath, tc.graph}} {
				raw, _ := json.Marshal(map[string]string{"path": file.path, "content": file.text})
				out, err := tool.Handle(context.Background(), raw)
				fixturekit.Must(t, err)
				if file.path != plan.PlanPath {
					continue
				}
				var status intake.DraftStatus
				fixturekit.Must(t, json.Unmarshal(out, &status))
				if tc.at == "" {
					if status.Verdict != intake.DraftValid {
						t.Fatalf("carryover: %+v", status)
					}
				} else if status.Verdict != intake.DraftInvalid || len(status.Problems) != 1 || !strings.Contains(status.Problems[0], tc.at) {
					t.Fatalf("verdict: %+v", status)
				}
			}
		})
	}
	fixturekit.Must(t, a.Enqueue(context.Background(), f.S.Current(), stream, "guidance", 1, "fixture", nil))
	thread, err := repo.Thread(stream, intake.ArchitectAgent)
	fixturekit.Must(t, err)
	intakefixture.AssertAcceptanceDraftInstructions(t, thread.Turns[len(thread.Turns)-1].Request.Prompt)
}

func TestDeliverDeltaCheckAcceptance(t *testing.T) {
	t.Parallel()
	f, stream, repo := journey.NewApprovedFixture(t, "delta-check-acceptance")
	defer repo.Close()
	f.S.SetSole(journey.RuntimeFor(repo))
	ctx := context.Background()
	_, err := trace.EnsureWorkgroups(ctx, repo, []config.WorkstreamID{stream}, f.S.Now(), intake.ServiceActor, "fixture")
	fixturekit.Must(t, err)
	c, apiErr := f.S.Lifecycle().RequestChange(ctx, string(stream), (lifecycle.ChangeRequest{Key: "acceptance-delta", Kind: "amend", Scope: "workgroup", Change: "Resume from checkpoints"}))
	if apiErr != nil {
		t.Fatal(apiErr)
	}

	edited := c.Before.Slices[0]
	edited.Spec = strings.Replace(edited.Spec, "An interrupted upload resumes from the last acknowledged chunk.", "All checks pass", 1)
	edited.Plan = intakefixture.AcceptancePlan(t, edited.Plan, "dagger check passes")
	c.Before.Slices[0] = edited
	other, err := config.NewWorkstreamID()
	fixturekit.Must(t, err)
	otherSlice := edited
	otherSlice.Workstream = other
	c.Before.Slices = append(c.Before.Slices, otherSlice)
	c.Members = append(c.Members, amendment.Member{Workstream: other, Units: map[string]string{}})

	fixturekit.Must(t, repo.CreateWorkstream(ctx, other, f.S.Now(), intake.OwnerActor))
	fixturekit.Must(t, f.S.Lifecycle().SaveChange(ctx, repo, &c))
	tool := f.S.Lifecycle().DeltaTool(repo, c, intake.ArchitectAgent, lifecycle.DeltaDraftTool)
	for _, tc := range []struct{ name, spec, graph, at string }{
		{"criterion", strings.Replace(edited.Spec, "All checks pass", "Full check run is green", 1), edited.Plan, "spec#1"},
		{"item", edited.Spec, intakefixture.AcceptancePlan(t, edited.Plan, "All checks pass"), `unit "resume": acceptance item 1`},
		{"carryover", edited.Spec, strings.Replace(edited.Plan, "Resume", "Resume safely", 1), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			after := c.Before
			after.Slices = slices.Clone(c.Before.Slices)
			after.Slices[0].Spec, after.Slices[0].Plan = tc.spec, tc.graph
			raw, _ := json.Marshal(after)
			out, err := tool.Handle(ctx, raw)
			fixturekit.Must(t, err)
			if tc.at != "" {
				if !strings.Contains(string(out), `"recorded":false`) || !strings.Contains(string(out), strings.ReplaceAll(tc.at, `"`, `\"`)) {
					t.Fatalf("delta refusal: %s", out)
				}
				if now := journey.CurrentChange(t, f.S, stream, c.ID); now.State != "drafting" {
					t.Fatalf("invalid delta advanced: %s", now.State)
				}
			} else if !strings.Contains(string(out), `"recorded":true`) {
				t.Fatalf("carryover rejected: %s", out)
			}
		})
	}
}

func TestShedRedraftCheckAcceptance(t *testing.T) {
	t.Parallel()
	f := journey.NewArchitectFixture(t)
	f.Script("draft-1-1", map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.ValidPlan}, nil)
	stream := f.HandIn(t, "shed-check-acceptance", fixturekit.HandedDesign)
	f.Await(t, stream, intakefixture.Sketched)
	f.Stop(t)
	repo, err := trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
	fixturekit.Must(t, err)
	defer repo.Close()
	d := &intake.Debate{S: f.S.Intake().DebateDependencies(), Repository: repo}
	for _, tc := range []struct{ name, path, content, at string }{
		{"criterion", plan.SpecPath, strings.Replace(fixturekit.ValidSpec, "An interrupted upload resumes from the last acknowledged chunk.", "All checks pass", 1), "spec#1"},
		{"unit", plan.PlanPath, intakefixture.AcceptancePlan(t, fixturekit.ValidPlan, "dagger check passes"), `unit "resume": acceptance item 1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope := coreadapter.Scope{Project: string(repo.Project()), Workstream: string(stream), Role: intake.ArchitectRole}
			turn := "reply-acceptance-" + tc.name
			tool := d.Drafter().DraftTool(scope, turn, d.Judge)
			raw, _ := json.Marshal(map[string]string{"path": tc.path, "content": tc.content})
			out, err := tool.Handle(context.Background(), raw)
			fixturekit.Must(t, err)
			var status intake.DraftStatus
			fixturekit.Must(t, json.Unmarshal(out, &status))
			if status.Verdict != intake.DraftInvalid || len(status.Problems) != 1 || !strings.Contains(status.Problems[0], tc.at) {
				t.Fatalf("shed verdict: %+v", status)
			}
			_, problems, err := d.Redraft(stream, turn)
			fixturekit.Must(t, err)
			if !slices.Equal(problems, status.Problems) {
				t.Fatalf("recording validation differs: %v versus %v", problems, status.Problems)
			}
		})
	}
}
