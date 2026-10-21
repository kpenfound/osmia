package executionjourney

import (
	context "context"
	json "encoding/json"
	amendment "github.com/kpenfound/osmia/internal/amendment"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	kb "github.com/kpenfound/osmia/internal/kb"
	plan "github.com/kpenfound/osmia/internal/plan"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	legacytrace "github.com/kpenfound/osmia/internal/trace/legacytrace"
	os "os"
	filepath "path/filepath"
	slices "slices"
	strings "strings"
	testing "testing"
)

func assertAcceptanceDraftInstructions(t *testing.T, prompt string) {
	t.Helper()
	for _, want := range []string{"Acceptance states observable behaviours", "tests the unit adds as evidence", "Never require a whole-suite check pass or run", "no need to list existing tests to keep passing", "service-owned check gate catches regressions", "factory runs and selects checks itself", "smart-check selection"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("draft instructions lack %q: %s", want, prompt)
		}
	}
}

func TestAcceptanceFactoryInstructions(t *testing.T) {
	t.Parallel()
	assertAcceptanceDraftInstructions(t, architectPrompt("handed/stdin", 1, "", false, nil))
	for _, role := range []string{architectRole, committeeRole} {
		policy := factoryInstructions(coreadapter.Scope{Role: role})
		for _, want := range []string{"whole check suite to pass or run", "asking a reviewer to show which tests ran", "engineering defect to correct", "service-owned check gate already runs checks with smart-check selection"} {
			if !strings.Contains(policy, want) {
				t.Errorf("%s policy lacks %q: %s", role, want, policy)
			}
		}
	}
	assertCheckGateReviewInstructions(t, factoryInstructions(coreadapter.Scope{Role: reviewerRole}))
	assertCheckGateReviewInstructions(t, factoryInstructions(coreadapter.Scope{Role: committeeRole}))
	assertCheckGateReviewInstructions(t, finalSystemPrompt(config.Project{}))
}

var acceptancePlan = intakefixture.AcceptancePlan

func TestDraftWriteCheckAcceptance(t *testing.T) {
	t.Parallel()
	ownSpec, err := os.ReadFile("testdata/check-acceptance/spec.md")
	must(t, err)
	ownPlan, err := os.ReadFile("testdata/check-acceptance/plan.json")
	must(t, err)
	mentions := []string{"acceptance never requires all checks to pass", "the validator rejects acceptance saying 'all checks pass'", "table tests flag 'full check run is green'", "all internal/plan tests pass"}
	mentionSpec := "## Acceptance criteria\n1. " + strings.Join(mentions, ". ") + "\n2. Acknowledged chunks are never sent again.\n"
	cases := []struct{ name, spec, graph, at string }{
		{"criterion", strings.Replace(validSpec, "An interrupted upload resumes from the last acknowledged chunk.", "All `dagger checks` pass", 1), validPlan, "spec#1"},
		{"unit", validSpec, acceptancePlan(t, validPlan, "dagger check passes"), `unit "resume": acceptance item 1`},
		{"scoped-run-criterion", strings.Replace(validSpec, "An interrupted upload resumes from the last acknowledged chunk.", "Run all checks in internal/plan", 1), validPlan, ""},
		{"scoped-run-unit", validSpec, acceptancePlan(t, validPlan, "Run all checks in internal/plan", "Execute every test in the parser suite"), ""},
		{"relative-scope-criterion", strings.Replace(validSpec, "An interrupted upload resumes from the last acknowledged chunk.", "Run all checks in ./internal/plan. All checks must be run under ./internal/plan.", 1), validPlan, ""},
		{"relative-scope-unit", validSpec, acceptancePlan(t, validPlan, "Run all checks in ./internal/plan", "All checks must be run under ./internal/plan", "All checks pass in ./internal/plan"), ""},
		{"backticked-relative-scope-criterion", strings.Replace(validSpec, "An interrupted upload resumes from the last acknowledged chunk.", "Run all checks in `./internal/plan`. All checks must be run under `./internal/plan`.", 1), validPlan, ""},
		{"backticked-relative-scope-unit", validSpec, acceptancePlan(t, validPlan, "Run all checks in `./internal/plan`", "All checks must be run under `./internal/plan`", "All checks pass in `./internal/plan`"), ""},
		{"unscoped-run-criterion", strings.Replace(validSpec, "An interrupted upload resumes from the last acknowledged chunk.", "Run all checks", 1), validPlan, "spec#1"},
		{"unscoped-run-unit", validSpec, acceptancePlan(t, validPlan, "Run all checks"), `unit "resume": acceptance item 1`},
		{"run-backticked-command-criterion", strings.Replace(validSpec, "An interrupted upload resumes from the last acknowledged chunk.", "Run `dagger check`", 1), validPlan, "spec#1"},
		{"run-backticked-subject-unit", validSpec, acceptancePlan(t, validPlan, "Run `all checks`"), `unit "resume": acceptance item 1`},
		{"relative-scope-then-unscoped-criterion", strings.Replace(validSpec, "An interrupted upload resumes from the last acknowledged chunk.", "Run all checks in ./internal/plan. Run all checks.", 1), validPlan, "spec#1"},
		{"relative-scope-then-unscoped-unit", validSpec, acceptancePlan(t, validPlan, "Run all checks in `./internal/plan`. All checks pass."), `unit "resume": acceptance item 1`},
		{"mentions", mentionSpec, acceptancePlan(t, validPlan, mentions...), ""},
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
		cases = append(cases, struct{ name, spec, graph, at string }{"quoted-statements-" + quote.name, spec, acceptancePlan(t, validPlan, items...), ""})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newArchitectFixture(t)
			defer f.stop(t)
			// The feature fixture uses the production text and its original entities.
			entities, err := kb.Load(f.repository())
			must(t, err)
			if tc.name == "own-feature" {
				entities.Entities = append(entities.Entities, kb.Entity{ID: "internal.service", Name: "Service", Paths: []string{"internal/service/**"}}, kb.Entity{ID: "internal.plan", Name: "Plan", Paths: []string{"internal/plan/**"}})
				must(t, kb.Store(context.Background(), f.repository(), entities, f.clock.Now(), serviceActor, "fixture"))
			}
			f.script("draft-1-1", map[string]string{plan.SpecPath: tc.spec, plan.PlanPath: tc.graph}, nil)
			f.script("draft-2-1", map[string]string{plan.SpecPath: validSpec, plan.PlanPath: validPlan}, nil)
			stream := f.handIn(t, "acceptance-"+tc.name, handedDesign)
			f.await(t, stream, sketched)
			deliveries := f.delivered("draft-1-1")
			if len(deliveries) != 2 {
				t.Fatalf("deliveries: %+v", deliveries)
			}
			got := deliveries[1]
			if tc.at == "" {
				if got.Verdict != draftValid || len(got.Problems) != 0 {
					t.Fatalf("valid mentions: %+v", got)
				}
				for _, doc := range []struct{ id, content string }{{plan.SpecDocument, tc.spec}, {plan.PlanDocument, tc.graph}} {
					if docs := f.documents(t, stream, doc.id); len(docs) != 1 || docs[0].Content != doc.content {
						t.Fatalf("valid draft not recorded: %+v", docs)
					}
				}
				return
			}
			if got.Verdict != draftInvalid || len(got.Problems) != 1 || !strings.Contains(got.Problems[0], tc.at) {
				t.Fatalf("verdict: %+v", got)
			}
			invalid := slices.DeleteFunc(f.transitions(t, stream), func(tr trace.Transition) bool { return tr.ID != "draft-1-invalid" })
			if len(invalid) != 1 || invalid[0].Reason != "draft 1 of the spec and plan is invalid:\n- "+got.Problems[0] {
				t.Fatalf("recorded verdict differs: %+v", invalid)
			}
			if docs := f.documents(t, stream, plan.PlanDocument); len(docs) != 2 || docs[0].Content != tc.graph {
				t.Fatalf("draft not recorded: %+v", docs)
			}
		})
	}
}

func TestAmendmentCheckAcceptance(t *testing.T) {
	t.Parallel()
	f := newArchitectFixture(t)
	f.script("draft-1-1", map[string]string{plan.SpecPath: validSpec, plan.PlanPath: validPlan}, nil)
	stream := f.handIn(t, "amend-check-acceptance", handedDesign)
	f.await(t, stream, sketched)
	f.stop(t)
	repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	defer repo.Close()
	// Simulate sealed historical acceptance, which remains loadable.
	oldSpec := strings.Replace(validSpec, "An interrupted upload resumes from the last acknowledged chunk.", "All checks pass", 1)
	oldGraph := acceptancePlan(t, validPlan, "dagger check passes", "A checkpoint is durable")
	docs := []trace.Document{}
	for _, item := range []struct{ id, path, text string }{{plan.SpecDocument, plan.SpecPath, oldSpec}, {plan.PlanDocument, plan.PlanPath, oldGraph}} {
		docs = append(docs, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: item.id, Revision: 2, Project: repo.Project(), Workstream: stream, At: f.clock.Now(), Actor: architectActor, Cause: "fixture"}, Path: item.path, Content: item.text})
	}
	must(t, repo.RecordDocuments(context.Background(), docs))
	a := &amendmentDrafter{Drafter: &drafter{S: f.s.intake().DrafterDependencies(), Repository: repo}}
	for _, tc := range []struct{ name, spec, graph, at string }{
		{"carryover", strings.Replace(oldSpec, "Acknowledged chunks", "Durably acknowledged chunks", 1), strings.Replace(oldGraph, "A checkpoint is durable", "A checkpoint survives restart", 1), ""},
		{"changed-criterion", strings.Replace(oldSpec, "All checks pass", "Full check run is green", 1), oldGraph, "spec#1"},
		{"changed-item", oldSpec, acceptancePlan(t, oldGraph, "All tests pass"), `unit "resume": acceptance item 1`},
		{"added-item", oldSpec, acceptancePlan(t, oldGraph, "dagger check passes", "A checkpoint is durable", "Run the full test suite"), `unit "resume": acceptance item 3`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope := coreadapter.Scope{Project: string(repo.Project()), Workstream: string(stream), Role: architectRole}
			tool := a.DraftTool(scope, "amend-"+tc.name+"-1", a.Judge)
			for _, file := range []struct{ path, text string }{{plan.SpecPath, tc.spec}, {plan.PlanPath, tc.graph}} {
				raw, _ := json.Marshal(map[string]string{"path": file.path, "content": file.text})
				out, err := tool.Handle(context.Background(), raw)
				must(t, err)
				if file.path != plan.PlanPath {
					continue
				}
				var status draftStatus
				must(t, json.Unmarshal(out, &status))
				if tc.at == "" {
					if status.Verdict != draftValid {
						t.Fatalf("carryover: %+v", status)
					}
				} else if status.Verdict != draftInvalid || len(status.Problems) != 1 || !strings.Contains(status.Problems[0], tc.at) {
					t.Fatalf("verdict: %+v", status)
				}
			}
		})
	}
	must(t, a.Enqueue(context.Background(), f.s.current(), stream, "guidance", 1, "fixture", nil))
	thread, err := repo.Thread(stream, architectAgent)
	must(t, err)
	assertAcceptanceDraftInstructions(t, thread.Turns[len(thread.Turns)-1].Request.Prompt)
}

func TestDeliverDeltaCheckAcceptance(t *testing.T) {
	t.Parallel()
	f, stream, repo := newApprovedFixture(t, "delta-check-acceptance")
	defer repo.Close()
	f.s.setSole(runtimeFor(repo))
	ctx := context.Background()
	_, err := trace.EnsureWorkgroups(ctx, repo, []config.WorkstreamID{stream}, f.s.now(), serviceActor, "fixture")
	must(t, err)
	c, api := f.s.requestChange(ctx, string(stream), ChangeRequest{Key: "acceptance-delta", Kind: "amend", Scope: "workgroup", Change: "Resume from checkpoints"})
	if api != nil {
		t.Fatal(api)
	}
	// Two pinned slices exercise legacy text both in changed and untouched slices.
	legacy := c.Before.Slices[0]
	legacy.Spec = strings.Replace(legacy.Spec, "An interrupted upload resumes from the last acknowledged chunk.", "All checks pass", 1)
	legacy.Plan = acceptancePlan(t, legacy.Plan, "dagger check passes")
	c.Before.Slices[0] = legacy
	other, err := config.NewWorkstreamID()
	must(t, err)
	otherSlice := legacy
	otherSlice.Workstream = other
	c.Before.Slices = append(c.Before.Slices, otherSlice)
	c.Members = append(c.Members, amendment.Member{Workstream: other, Units: map[string]string{}})
	// A real second workstream gives delivery validation its project context.
	must(t, repo.CreateWorkstream(ctx, other, f.s.now(), ownerActor))
	must(t, f.s.saveChange(ctx, repo, &c))
	tool := f.s.deltaTool(repo, c, architectAgent, deltaDraftTool)
	for _, tc := range []struct{ name, spec, graph, at string }{
		{"criterion", strings.Replace(legacy.Spec, "All checks pass", "Full check run is green", 1), legacy.Plan, "spec#1"},
		{"item", legacy.Spec, acceptancePlan(t, legacy.Plan, "All checks pass"), `unit "resume": acceptance item 1`},
		{"carryover", legacy.Spec, strings.Replace(legacy.Plan, "Resume", "Resume safely", 1), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			after := c.Before
			after.Slices = slices.Clone(c.Before.Slices)
			after.Slices[0].Spec, after.Slices[0].Plan = tc.spec, tc.graph
			raw, _ := json.Marshal(after)
			out, err := tool.Handle(ctx, raw)
			must(t, err)
			if tc.at != "" {
				if !strings.Contains(string(out), `"recorded":false`) || !strings.Contains(string(out), strings.ReplaceAll(tc.at, `"`, `\"`)) {
					t.Fatalf("delta refusal: %s", out)
				}
				if now := currentChange(t, f.s, stream, c.ID); now.State != "drafting" {
					t.Fatalf("invalid delta advanced: %s", now.State)
				}
			} else if !strings.Contains(string(out), `"recorded":true`) {
				t.Fatalf("legacy carryover rejected: %s", out)
			}
		})
	}
}

func TestStoredCheckAcceptanceMigratesAndLoads(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newArchitectFixture(t)
	f.stop(t)
	repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	stream, err := config.NewWorkstreamID()
	must(t, err)
	must(t, repo.CreateWorkstream(ctx, stream, f.s.now(), ownerActor))
	groups, err := trace.EnsureWorkgroups(ctx, repo, []config.WorkstreamID{stream}, f.s.now(), serviceActor, "fixture")
	must(t, err)
	oldSpec := strings.Replace(validSpec, "An interrupted upload resumes from the last acknowledged chunk.", "All checks pass", 1)
	oldGraph := acceptancePlan(t, validPlan, "dagger check passes")
	group := groups[0]
	projectSpec := trace.ProjectSpec{Behavior: "Resumable uploads", Slices: []trace.Slice{{Workstream: stream, Spec: oldSpec, Plan: oldGraph}}}
	must(t, trace.RecordProjectSpec(ctx, repo, group, projectSpec, f.s.now(), architectActor, "fixture"))
	projectDocs, err := trace.Read[trace.Document](repo, "")
	must(t, err)
	projectFiles := map[string][]byte{"project.json": legacytrace.ProjectManifest(string(f.project), f.s.now())}
	var projectRecords []any
	for _, d := range projectDocs {
		projectRecords = append(projectRecords, d)
		projectFiles[d.Path] = []byte(d.Content)
	}
	projectFiles["documents.jsonl"], err = legacytrace.JSONL(projectRecords...)
	must(t, err)
	// Both project slices and member documents enter through actual Git migration.
	check := plan.DraftCheck(f.trace)
	must(t, check(map[string]string{plan.SpecDocument: oldSpec, plan.PlanDocument: oldGraph}))
	must(t, repo.Close())
	must(t, os.RemoveAll(f.trace))
	must(t, legacytrace.Build(f.trace, projectFiles))
	directory, err := f.s.cfg.Root.WorkstreamTrace(f.project, stream)
	must(t, err)
	must(t, os.RemoveAll(directory))
	files := legacytrace.Workstream(string(f.project), string(stream), f.s.now())
	prefix := "workstreams/" + string(stream) + "/"
	var docs []any
	for _, item := range []struct{ id, path, text string }{
		{plan.SpecDocument, plan.SpecPath, oldSpec}, {plan.PlanDocument, plan.PlanPath, oldGraph},
		{"amendment_1_spec", "amendments/1/spec.md", oldSpec}, {"amendment_1_plan", "amendments/1/plan.json", oldGraph},
	} {
		d := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: item.id, Revision: 1, Project: f.project, Workstream: stream, At: f.s.now(), Actor: architectActor, Cause: "fixture"}, Path: item.path, Content: item.text}
		docs = append(docs, d)
		files[prefix+item.path] = []byte(item.text)
	}
	files[prefix+"documents.jsonl"], err = legacytrace.JSONL(docs...)
	must(t, err)
	must(t, legacytrace.Build(directory, files))
	must(t, trace.Migrate(ctx, f.s.cfg.Root, f.project, check, func(string, ...any) {}))
	repo, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	defer repo.Close()
	recorded, err := trace.Read[trace.Document](repo, stream)
	must(t, err)
	if len(recorded) != len(docs) {
		t.Fatalf("stored documents lost: %+v", recorded)
	}
	for _, d := range recorded {
		if d.Content != oldSpec && d.Content != oldGraph {
			t.Fatalf("stored text changed: %+v", d)
		}
	}
	loaded, _, found, err := repo.ProjectSpecOf(stream)
	must(t, err)
	if !found || len(loaded.Slices) != 1 || loaded.Slices[0].Spec != oldSpec || loaded.Slices[0].Plan != oldGraph {
		t.Fatalf("legacy slice not loadable: %+v", loaded)
	}
	for _, sl := range loaded.Slices {
		must(t, check(map[string]string{plan.SpecDocument: sl.Spec, plan.PlanDocument: sl.Plan}))
	}
	if _, err := plan.Parse([]byte(loaded.Slices[0].Plan)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directory, ".git")); !os.IsNotExist(err) {
		t.Fatalf("Git trace not migrated: %v", err)
	}
}

func TestShedRedraftCheckAcceptance(t *testing.T) {
	t.Parallel()
	f := newArchitectFixture(t)
	f.script("draft-1-1", map[string]string{plan.SpecPath: validSpec, plan.PlanPath: validPlan}, nil)
	stream := f.handIn(t, "shed-check-acceptance", handedDesign)
	f.await(t, stream, sketched)
	f.stop(t)
	repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	defer repo.Close()
	d := &debate{S: f.s.intake().DebateDependencies(), Repository: repo}
	for _, tc := range []struct{ name, path, content, at string }{
		{"criterion", plan.SpecPath, strings.Replace(validSpec, "An interrupted upload resumes from the last acknowledged chunk.", "All checks pass", 1), "spec#1"},
		{"unit", plan.PlanPath, acceptancePlan(t, validPlan, "dagger check passes"), `unit "resume": acceptance item 1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope := coreadapter.Scope{Project: string(repo.Project()), Workstream: string(stream), Role: architectRole}
			turn := "reply-acceptance-" + tc.name
			tool := d.Drafter().DraftTool(scope, turn, d.Judge)
			raw, _ := json.Marshal(map[string]string{"path": tc.path, "content": tc.content})
			out, err := tool.Handle(context.Background(), raw)
			must(t, err)
			var status draftStatus
			must(t, json.Unmarshal(out, &status))
			if status.Verdict != draftInvalid || len(status.Problems) != 1 || !strings.Contains(status.Problems[0], tc.at) {
				t.Fatalf("shed verdict: %+v", status)
			}
			_, problems, err := d.Redraft(stream, turn)
			must(t, err)
			if !slices.Equal(problems, status.Problems) {
				t.Fatalf("recording validation differs: %v versus %v", problems, status.Problems)
			}
		})
	}
}
