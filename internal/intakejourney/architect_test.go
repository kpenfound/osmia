package intakejourney

import (
	context "context"
	json "encoding/json"
	errors "errors"
	fmt "fmt"
	http "net/http"
	os "os"
	filepath "path/filepath"
	slices "slices"
	strings "strings"
	sync "sync"
	atomic "sync/atomic"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	plan "github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/projectstore"
	questions "github.com/kpenfound/osmia/internal/questions"
	scheduler "github.com/kpenfound/osmia/internal/scheduler"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func checkArchitectBoundary(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession, clone, handed, charter string) error {
	var problems []error
	fail := func(format string, args ...any) { problems = append(problems, fmt.Errorf(format, args...)) }
	listed, err := tools.ListTools(ctx, nil)
	if err != nil {
		return err
	}
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if want := []string{questions.AskTool, intake.DraftTool, "factory_context", "file_read", "inspect_code", "project_context", "read_remote_file"}; !slices.Equal(names, want) {
		fail("role tools %v, want %v", names, want)
	}
	for _, name := range []string{"file_write", "shell", "git_push", "set_status", "notes_read", "notes_write"} {
		if _, err := fixturekit.CallTool(ctx, tools, name, map[string]any{"path": "x", "content": "y"}); err == nil {
			fail("runtime called %s", name)
		}
	}
	for path, want := range map[string]string{"handed/stdin": handed, "charter.md": charter} {
		if got, err := intakefixture.ReadTool(ctx, tools, path); err != nil || got != want {
			fail("read %s: %q %v", path, got, err)
		}
	}
	context, err := intakefixture.ReadTool(ctx, tools, "context.md")
	if err != nil {
		fail("read context.md: %v", err)
	}
	for _, want := range []string{"# Project context", "context mode: file", "- charter#1: Keep changes small.", "- charter#2: Every change has a test.", "project_context", "kb/entities.json"} {
		if !strings.Contains(context, want) {
			fail("context.md lacks %q:\n%s", want, context)
		}
	}
	entities, err := fixturekit.CallTool(ctx, tools, service.ProjectContextTool, map[string]any{"path": "kb/entities.json"})
	if err != nil || !strings.Contains(entities, "internal.trace") {
		fail("retrieving entities: %s: %v", entities, err)
	}
	for _, path := range []string{"repo/CODEOWNERS", "secret.env", "../config.toml", ".git/HEAD", filepath.Join(clone, "CODEOWNERS"), "kb/entities.json"} {
		if _, err := intakefixture.ReadTool(ctx, tools, path); err == nil {
			fail("runtime read %s", path)
		}
	}
	for _, args := range []map[string]any{{"path": "notes.md", "content": "x"}, {"path": "../spec.md", "content": "x"}, {"path": "spec.md"}, {"path": "spec.md", "content": "x", "extra": 1}, {"path": "plan.json", "content": strings.Repeat("x", intake.MaxHandedBytes+1)}} {
		if _, err := fixturekit.CallTool(ctx, tools, intake.DraftTool, args); err == nil {
			fail("draft_write accepted %v", args)
		}
	}
	intakefixture.CheckReadOnlyGrants(req, verified, clone, fail)
	if req.Profile.VCSAccess || req.Workspace == nil || req.Workspace.VCS() != nil || len(req.VCSEnv) != 0 || req.Profile.Name != intake.ArchitectRole {
		fail("request carries VCS access or another role: %+v", req.Profile)
	}
	for key, value := range req.Env {
		if key != "OSMIA_MCP_TOKEN" || strings.Contains(value, fixturekit.DemoSecret) {
			fail("runtime environment exposes %s", key)
		}
	}
	for _, want := range []string{"handed/stdin", "charter.md", "context.md", "intended behaviour", "must not do", `"## Acceptance criteria"`, "numbered list", "spec#<n>", "footprint", "no cycle", "at least one acceptance item", "Start with one coherent unit", intake.DraftTool} {
		if !strings.Contains(req.Prompt, want) {
			fail("prompt lacks %q", want)
		}
	}
	if !strings.Contains(req.SystemPrompt, "architect of the dagger project") || !strings.Contains(req.SystemPrompt, "no version control tool") {
		fail("system prompt: %q", req.SystemPrompt)
	}
	if strings.Contains(req.Prompt+req.SystemPrompt, fixturekit.DemoSecret) {
		fail("prompt exposes the untracked secret")
	}
	return errors.Join(problems...)
}

func TestArchitectDraftsAndSketchesAHandedWorkstream(t *testing.T) {

	for _, name := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		t.Setenv(name, fixturekit.DemoSecret)
	}
	f := journey.NewArchitectFixture(t)
	defer f.Stop(t)
	ctx := context.Background()
	charter := intakefixture.ShedCharter
	f.Script("draft-1-1", map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.ValidPlan},
		func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
			err := checkArchitectBoundary(ctx, req, verified, tools, f.Clone, fixturekit.HandedDesign, charter)
			if req.ResumeID != "" || strings.Contains(req.Prompt, "was not accepted") {
				err = errors.Join(err, fmt.Errorf("first turn carries history: %q", req.ResumeID))
			}
			if _, e := intakefixture.ReadTool(ctx, tools, "draft/spec.md"); e == nil {
				err = errors.Join(err, errors.New("first turn sees a previous draft"))
			}
			return err
		})
	head := fixturekit.TraceCommits(t, f.Trace)
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.Await(t, stream, intakefixture.Sketched)
	if runs := f.Runs(); !slices.Equal(runs, []string{"draft-1-1"}) {
		t.Fatalf("backend runs %v", runs)
	}

	ops := f.AcknowledgedDraftOperations(t, stream)
	if len(ops) != 1 || !ops[0].Acknowledged || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" || !strings.Contains(ops[0].Result.Evidence, "draft 1 passed validation") {
		t.Fatalf("operations: %+v", ops)
	}
	operation := ops[0].Operation.ID
	for id, want := range map[string]struct{ path, content string }{plan.SpecDocument: {plan.SpecPath, fixturekit.ValidSpec}, plan.PlanDocument: {plan.PlanPath, fixturekit.ValidPlan}} {
		docs := f.Documents(t, stream, id)
		if len(docs) != 1 || docs[0].Revision != 1 || docs[0].Path != want.path || docs[0].Content != want.content || docs[0].Actor != intake.ArchitectActor || docs[0].Cause != operation || docs[0].Source != "" {
			t.Fatalf("%s revisions: %+v", id, docs)
		}
		data, err := fixturekit.WorkstreamDocument(t, f.S.Current().Root, f.Project, stream, want.path)
		if err != nil || string(data) != want.content {
			t.Fatalf("%s: %q %v", want.path, data, err)
		}
	}
	spec := intakefixture.DocumentCommit(t, f.S.Current().Root, f.Project, stream, "spec.md")
	if other := intakefixture.DocumentCommit(t, f.S.Current().Root, f.Project, stream, "plan.json"); other != spec {
		t.Fatalf("revisions landed in separate commits: %s %s", spec, other)
	}
	var moves []trace.Transition
	for _, tr := range f.Transitions(t, stream) {
		if tr.Subject == trace.FeatureSubject {
			moves = append(moves, tr)
		}
	}
	if len(moves) != 2 || moves[1].ID != "sketched" || moves[1].From != intake.HandedState || moves[1].To != intake.SketchedState || moves[1].Actor != intake.DraftingActor || moves[1].Cause != operation ||
		moves[1].Reason != "the architect's draft 1 passed validation: spec.md revision 1 with 2 acceptance criteria and plan.json revision 1 with 2 units" {
		t.Fatalf("feature transitions: %+v", moves)
	}
	outbox, err := f.Repository().Outbox(stream)
	fixturekit.Must(t, err)
	var notices []trace.OutboxEntry
	for _, e := range outbox {
		if intakefixture.ManagerEvent(e.Event) && e.TransitionID == "sketched" {
			notices = append(notices, e)
		}
	}
	if len(notices) != 1 || !strings.Contains(notices[0].Event.Body, "Workstream state changed from handed to sketched: the architect's draft 1 passed validation") {
		t.Fatalf("outbox: %+v", outbox)
	}
	if state, err := f.Repository().Workflow(stream, trace.FeatureSubject); err != nil || state != (trace.WorkflowState{Version: 2, Value: intake.SketchedState}) {
		t.Fatalf("feature state %+v %v", state, err)
	}
	status, err := f.C.Status(ctx, stream)
	fixturekit.Must(t, err)
	if status.State == nil || *status.State != intake.SketchedState {
		t.Fatalf("status: %+v", status)
	}

	th := f.ArchitectThread(t, stream)
	if len(th.Turns) != 1 || th.Turns[0].Request.TurnID != "draft-1-1" || th.Turns[0].Status() != "idle" || th.Identity.Role != intake.ArchitectRole || th.Active != "" {
		t.Fatalf("architect thread: %+v", th)
	}
	if notes, ok := fixturekit.TraceFile(t, f.Trace, "notes/architect.md"); ok {
		t.Fatalf("architect notes: %q", notes)
	}

	other := config.WorkstreamID("w_0123456789abcdef0123456789abcdef")
	fixturekit.Must(t, f.Repository().CreateWorkstream(ctx, other, f.Clock.Now(), intake.OwnerActor))
	_, err = f.Repository().SetFeatureState(ctx, trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "ratified", Revision: 1, Project: f.Project, Workstream: other, At: f.Clock.Now(), Actor: intake.OwnerActor, Cause: "owner"}, "ratified", "the owner ratified the plan")
	fixturekit.Must(t, err)
	d := &intake.Drafter{S: f.S.Intake().DrafterDependencies(), Repository: f.Repository()}
	fixturekit.Must(t, d.Pass(ctx))
	if state, err := f.Repository().Workflow(other, intake.DraftSubject); err != nil || state.Value != "" {
		t.Fatalf("a ratified workstream was drafted: %+v %v", state, err)
	}
	if _, err := f.Repository().Thread(other, intake.ArchitectAgent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a ratified workstream got an architect thread: %v", err)
	}
	same := func(got *coreadapter.OperationResult) bool {
		return got != nil && got.Outcome == ops[0].Result.Outcome && got.Evidence == ops[0].Result.Evidence && len(got.Data) == 0
	}
	if observed, err := d.Inspect(ctx, ops[0].Operation); err != nil || observed.State != coreadapter.EffectCompleted || !same(observed.Result) {
		t.Fatalf("inspect after completion: %+v %v", observed, err)
	}
	if result, err := d.Apply(ctx, ops[0].Operation); err != nil || !same(&result) {
		t.Fatalf("apply after completion: %+v %v", result, err)
	}
	if again := f.DraftOperations(t, stream); len(again) != 1 {
		t.Fatalf("a second draft was requested: %+v", again)
	}
	if th := f.ArchitectThread(t, stream); len(th.Turns) != 1 {
		t.Fatalf("a second turn was queued: %+v", th)
	}
	if docs := f.Documents(t, stream, plan.SpecDocument); len(docs) != 1 {
		t.Fatalf("retry duplicated revisions: %+v", docs)
	}
	if len(slices.DeleteFunc(f.Transitions(t, stream), func(tr trace.Transition) bool { return tr.Subject == intake.FactoryPolicySubject })) != 3 {
		t.Fatalf("transitions: %+v", f.Transitions(t, stream))
	}

	root := f.Opts.Config.Root
	if entries, err := os.ReadDir(filepath.Join(root, "views")); err != nil || len(entries) != 0 {
		t.Fatalf("views leaked: %v %v", entries, err)
	}
	turnDir := filepath.Join(root, "architect", string(f.Project), string(stream), "draft-1-1")
	if _, err := os.Lstat(filepath.Join(turnDir, "workspace")); !os.IsNotExist(err) {
		t.Fatal("staged workspace retained")
	}
	if data, err := os.ReadFile(filepath.Join(turnDir, "output", "spec.md")); err != nil || string(data) != fixturekit.ValidSpec {
		t.Fatalf("delivered spec: %q %v", data, err)
	}
	for path, content := range intakefixture.Snapshot(t, filepath.Join(root, "projects")) {
		if strings.Contains(content, fixturekit.DemoSecret) {
			t.Fatalf("secret copied into %s", path)
		}
	}
	if fixturekit.TraceCommits(t, f.Trace) == head {
		t.Fatal("the trace did not move")
	}
}

func TestArchitectResubmitsAnInvalidDraft(t *testing.T) {
	t.Parallel()
	f := journey.NewArchitectFixture(t)
	defer f.Stop(t)

	sawInvalid1, sawInvalid2 := make(chan struct{}), make(chan struct{})
	gate := func(ctx context.Context, open <-chan struct{}) error {
		select {
		case <-open:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(fixturekit.DemoTimeout):
			return errors.New("the test never released the turn")
		}
	}
	f.Script("draft-1-1", map[string]string{plan.SpecPath: fixturekit.ValidSpec},
		func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
			output := filepath.Join(filepath.Dir(req.SessionDir), "output")
			if err := os.MkdirAll(output, 0700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(output, plan.PlanPath), []byte{'{', 0xff, 0xfe, '}'}, 0600)
		})
	f.Script("draft-2-1", map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.CyclicPlan},
		func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
			if err := gate(ctx, sawInvalid1); err != nil {
				return err
			}
			var problems []error
			for _, want := range []string{"Draft 1 was not accepted:", "draft 1 of the spec and plan is invalid:\n- plan.json is not UTF-8 text", "draft/spec.md and draft/plan.json: your previous draft", "Deliver corrected files"} {
				if !strings.Contains(req.Prompt, want) {
					problems = append(problems, fmt.Errorf("prompt lacks %q:\n%s", want, req.Prompt))
				}
			}
			if got, err := intakefixture.ReadTool(ctx, tools, "draft/spec.md"); err != nil || got != fixturekit.ValidSpec {
				problems = append(problems, fmt.Errorf("previous spec: %q %v", got, err))
			}
			if _, err := intakefixture.ReadTool(ctx, tools, "draft/plan.json"); err == nil {
				problems = append(problems, errors.New("a plan that was never delivered is in the view"))
			}
			if !strings.Contains(req.Prompt, "Draft delivered") {
				problems = append(problems, errors.New("second turn did not replay the first"))
			}
			return errors.Join(problems...)
		})
	f.Script("draft-3-1", map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.ValidPlan},
		func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
			if err := gate(ctx, sawInvalid2); err != nil {
				return err
			}
			var problems []error
			for _, want := range []string{"Draft 2 was not accepted:", `unit "dedupe": dependency cycle dedupe -> resume -> dedupe`, "spec#2: no unit serves this criterion"} {
				if !strings.Contains(req.Prompt, want) {
					problems = append(problems, fmt.Errorf("prompt lacks %q:\n%s", want, req.Prompt))
				}
			}
			if got, err := intakefixture.ReadTool(ctx, tools, "draft/plan.json"); err != nil || got != fixturekit.CyclicPlan {
				problems = append(problems, fmt.Errorf("previous plan: %q %v", got, err))
			}
			return errors.Join(problems...)
		})
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.Await(t, stream, f.TransitionRecorded(t, stream, "draft-1-invalid"))

	if docs := f.Documents(t, stream, plan.SpecDocument); len(docs) != 1 || docs[0].Content != fixturekit.ValidSpec || docs[0].Actor != intake.ArchitectActor {
		t.Fatalf("spec after draft 1: %+v", docs)
	}
	if docs := f.Documents(t, stream, plan.PlanDocument); len(docs) != 0 {
		t.Fatalf("plan after draft 1: %+v", docs)
	}
	close(sawInvalid1)
	f.Await(t, stream, f.TransitionRecorded(t, stream, "draft-2-invalid"))
	if docs := f.Documents(t, stream, plan.PlanDocument); len(docs) != 1 || docs[0].Content != fixturekit.CyclicPlan || docs[0].Revision != 1 {
		t.Fatalf("plan after draft 2: %+v", docs)
	}
	if feature, err := f.Repository().Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != intake.HandedState {
		t.Fatalf("feature state %+v %v", feature, err)
	}
	close(sawInvalid2)
	f.Await(t, stream, intakefixture.Sketched)
	if runs := f.Runs(); !slices.Equal(runs, []string{"draft-1-1", "draft-2-1", "draft-3-1"}) {
		t.Fatalf("backend runs %v", runs)
	}
	ops := f.AcknowledgedDraftOperations(t, stream)
	if len(ops) != 3 {
		t.Fatalf("operations: %+v", ops)
	}
	byID := map[string]trace.Transition{}
	for _, tr := range f.Transitions(t, stream) {
		byID[tr.ID] = tr
	}
	for i, want := range []struct{ id, from, to, reason string }{
		{"draft-1", "", "drafting-1", "the workstream was handed in; the architect is asked for draft 1"},
		{"draft-1-invalid", "drafting-1", "invalid-1", "draft 1 of the spec and plan is invalid:\n- plan.json is not UTF-8 text"},
		{"draft-2", "invalid-1", "drafting-2", "draft 1 was invalid; the architect is asked for draft 2"},
		{"draft-2-invalid", "drafting-2", "invalid-2", "draft 2 of the spec and plan is invalid:\n- spec#2: no unit serves this criterion\n- unit \"dedupe\": dependency cycle dedupe -> resume -> dedupe"},
		{"draft-3", "invalid-2", "drafting-3", "draft 2 was invalid; the architect is asked for draft 3"},
	} {
		tr, ok := byID[want.id]
		if !ok || tr.Subject != intake.DraftSubject || tr.From != want.from || tr.To != want.to || tr.Actor != intake.DraftingActor || !strings.Contains(tr.Reason, want.reason) {
			t.Fatalf("transition %s: %+v (want %+v)", want.id, tr, want)
		}
		if strings.HasSuffix(want.id, "-invalid") {
			if ops[i/2].Result == nil || ops[i/2].Result.Outcome != "failed" || ops[i/2].Result.Evidence != tr.Reason || tr.Cause != ops[i/2].Operation.ID {
				t.Fatalf("operation %d: %+v", i/2, ops[i/2].Result)
			}
		}
	}
	if _, ok := byID["draft-3-invalid"]; ok {
		t.Fatal("draft 3 was refused")
	}

	for turn, want := range map[string][]intake.DraftStatus{
		"draft-1-1": {{Delivered: plan.SpecPath, Verdict: intake.DraftInvalid, Problems: []string{"plan.json is not UTF-8 text"}}},
		"draft-2-1": {
			{Delivered: plan.SpecPath, Verdict: intake.DraftInvalid, Problems: []string{"plan.json was not delivered"}},
			{Delivered: plan.PlanPath, Verdict: intake.DraftInvalid, Problems: []string{"spec#2: no unit serves this criterion", `unit "dedupe": dependency cycle dedupe -> resume -> dedupe`}},
		},
		"draft-3-1": {
			{Delivered: plan.SpecPath, Verdict: intake.DraftInvalid, Problems: []string{"plan.json was not delivered"}},
			{Delivered: plan.PlanPath, Verdict: intake.DraftValid},
		},
	} {
		got := f.Delivered(turn)
		for i := range got {
			if got[i].Next != intake.DraftNext[got[i].Verdict] {
				t.Fatalf("%s delivery %d next %q", turn, i, got[i].Next)
			}
		}
		if !slices.EqualFunc(got, want, func(a, b intake.DraftStatus) bool {
			return a.Delivered == b.Delivered && a.Verdict == b.Verdict && slices.Equal(a.Problems, b.Problems)
		}) {
			t.Fatalf("%s deliveries %+v, want %+v", turn, got, want)
		}
	}
	if tr := byID["sketched"]; tr.From != intake.HandedState || tr.Cause != ops[2].Operation.ID || !strings.Contains(tr.Reason, "draft 3 passed validation: spec.md revision 3 with 2 acceptance criteria and plan.json revision 2 with 2 units") {
		t.Fatalf("sketched: %+v", tr)
	}
	if docs := f.Documents(t, stream, plan.SpecDocument); len(docs) != 3 || docs[2].Content != fixturekit.ValidSpec {
		t.Fatalf("spec revisions: %+v", docs)
	}
	if docs := f.Documents(t, stream, plan.PlanDocument); len(docs) != 2 || docs[1].Content != fixturekit.ValidPlan {
		t.Fatalf("plan revisions: %+v", docs)
	}
	th := f.ArchitectThread(t, stream)
	if len(th.Turns) != 3 || !strings.Contains(th.Turns[1].Request.Prompt, "plan.json is not UTF-8 text") || !strings.Contains(th.Turns[2].Request.Prompt, "dependency cycle") {
		t.Fatalf("thread: %+v", th)
	}

	outbox, err := f.Repository().Outbox(stream)
	fixturekit.Must(t, err)
	for _, e := range outbox {
		if e.Event.Operation == nil && e.TransitionID != intake.HandInTransition && e.TransitionID != "sketched" {
			t.Fatalf("unexpected event: %+v", e)
		}
	}
}

func TestArchitectStopsAfterExhaustedDrafts(t *testing.T) {
	t.Parallel()
	f := journey.NewArchitectFixture(t)
	defer f.Stop(t)
	ctx := context.Background()
	for n := 1; n <= intake.MaxDrafts+1; n++ {
		f.Script(fmt.Sprintf("draft-%d-1", n), map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.CyclicPlan}, nil)
	}
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.Await(t, stream, intakefixture.DraftAt("exhausted"))
	if runs := f.Runs(); len(runs) != intake.MaxDrafts {
		t.Fatalf("backend runs %v", runs)
	}
	if feature, err := f.Repository().Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != intake.HandedState {
		t.Fatalf("feature state %+v %v", feature, err)
	}
	outbox, err := f.Repository().Outbox(stream)
	fixturekit.Must(t, err)
	var notices []trace.OutboxEntry
	for _, e := range outbox {
		if e.TransitionID == "draft-exhausted" {
			notices = append(notices, e)
		}
	}
	if len(notices) != 1 || !intakefixture.ManagerEvent(notices[0].Event) || notices[0].Event.Operation != nil ||
		!strings.HasPrefix(notices[0].Event.Body, "None of the architect's 3 drafts of the spec and plan was accepted and drafting has stopped; the workstream stays handed. Last draft: draft 3 of the spec and plan is invalid:\n- spec#2: no unit serves this criterion\n- unit \"dedupe\": dependency cycle") {
		t.Fatalf("outbox: %+v", outbox)
	}
	var last trace.Transition
	for _, tr := range f.Transitions(t, stream) {
		if tr.ID == "draft-exhausted" {
			last = tr
		}
	}
	if last.From != "invalid-3" || last.To != "exhausted" || last.DefaultTo != "exhausted" || last.Departed() || last.Actor != intake.DraftingActor || last.Cause != "draft-3-invalid" ||
		!strings.HasPrefix(last.Reason, "none of the architect's 3 drafts of the spec and plan was accepted; drafting stops and the workstream stays handed") {
		t.Fatalf("exhausted transition: %+v", last)
	}

	d := &intake.Drafter{S: f.S.Intake().DrafterDependencies(), Repository: f.Repository()}
	fixturekit.Must(t, d.Pass(ctx))
	if ops := f.DraftOperations(t, stream); len(ops) != intake.MaxDrafts {
		t.Fatalf("operations: %d", len(ops))
	}
	if th := f.ArchitectThread(t, stream); len(th.Turns) != intake.MaxDrafts {
		t.Fatalf("thread: %+v", th)
	}
	if docs := f.Documents(t, stream, plan.PlanDocument); len(docs) != intake.MaxDrafts {
		t.Fatalf("every draft is kept: %+v", docs)
	}

	opts, clone, engine, sessions, clock := journey.NewArchitectOptions(t)
	runner := opts.Architect
	opts.Architect = nil
	f2 := &journey.ArchitectFixture{Opts: opts, Clone: clone, Engine: engine, Sessions: sessions, Clock: clock}
	f2.Start(t)
	added, err := f2.C.AddProject(ctx, fixturekit.Request(clone))
	fixturekit.Must(t, err)
	f2.Project, f2.Trace = added.Project.ID, added.Project.Trace
	intakefixture.WriteCharter(t, f2.C, added.Project.ID, "1. Keep changes small.\n")
	stream = f2.HandIn(t, "design", fixturekit.HandedDesign)
	fixturekit.Must(t, (&intake.Drafter{S: f2.S.Intake().DrafterDependencies(), Repository: f2.Repository()}).Pass(ctx))
	if state, err := f2.Repository().Workflow(stream, intake.DraftSubject); err != nil || state.Value != "" {
		t.Fatalf("draft requested without a runner: %+v %v", state, err)
	}
	if ops := f2.DraftOperations(t, stream); len(ops) != 0 {
		t.Fatalf("operations without a runner: %+v", ops)
	}
	if _, err := f2.Repository().Thread(stream, intake.ArchitectAgent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("architect thread without a runner: %v", err)
	}
	f2.Stop(t)
	f2.Opts.Architect = runner
	f2.Script("draft-1-1", map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.ValidPlan}, nil)
	f2.Start(t)
	defer f2.Stop(t)
	f2.Await(t, stream, intakefixture.Sketched)
	if runs := f2.Runs(); !slices.Equal(runs, []string{"draft-1-1"}) {
		t.Fatalf("backend runs %v", runs)
	}
	ops := f2.DraftOperations(t, stream)
	if len(ops) != 1 {
		t.Fatalf("operations: %+v", ops)
	}

	f3 := journey.NewArchitectFixture(t)
	defer f3.Stop(t)
	f3.Engine.Mu.Lock()
	for n := 1; n <= intake.MaxDrafts; n++ {
		for attempt := 1; attempt <= intake.MaxDraftAttempts; attempt++ {
			f3.Engine.Turns[fmt.Sprintf("draft-%d-%d", n, attempt)] = func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) (*agent.Result, error) {
				return nil, errors.Join(context.Canceled, errors.New("connection dropped"))
			}
		}
	}
	f3.Engine.Mu.Unlock()
	stream = f3.HandIn(t, "design", fixturekit.HandedDesign)
	f3.Await(t, stream, intakefixture.DraftAt("exhausted"))
	if runs := f3.Runs(); len(runs) != intake.MaxDrafts*intake.MaxDraftAttempts {
		t.Fatalf("backend runs %v", runs)
	}
	ops = f3.AcknowledgedDraftOperations(t, stream)
	if len(ops) != intake.MaxDrafts || ops[0].Result == nil || !strings.Contains(ops[0].Result.Evidence, fmt.Sprintf("draft 1 failed: the architect turn was interrupted %d times by service stops", intake.MaxDraftAttempts)) {
		t.Fatalf("operations: %+v", ops)
	}
}

func TestArchitectRedraftsAfterAFailedTurn(t *testing.T) {
	t.Parallel()
	f := journey.NewArchitectFixture(t)
	defer f.Stop(t)
	f.Engine.Mu.Lock()
	f.Engine.Turns["draft-1-1"] = func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) (*agent.Result, error) {
		return nil, errors.New("backend crashed")
	}
	f.Engine.Mu.Unlock()
	f.Script("draft-2-1", map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.ValidPlan},
		func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
			if !strings.Contains(req.Prompt, "Draft 1 was not accepted:\ndraft 1 failed: architect turn draft-1-1 failed: ") {
				return fmt.Errorf("second prompt: %q", req.Prompt)
			}
			if _, err := intakefixture.ReadTool(ctx, tools, "draft/spec.md"); err == nil {
				return errors.New("the view holds a draft that was never recorded")
			}
			return nil
		})
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.Await(t, stream, intakefixture.Sketched)

	if runs := f.Runs(); !slices.Equal(runs, []string{"draft-1-1", "draft-1-1", "draft-1-1", "draft-2-1"}) {
		t.Fatalf("backend runs %v", runs)
	}
	ops := f.AcknowledgedDraftOperations(t, stream)
	if len(ops) != 2 || ops[0].Result == nil || ops[0].Result.Outcome != "failed" ||
		!strings.HasPrefix(ops[0].Result.Evidence, "draft 1 failed: architect turn draft-1-1 failed: ") || !strings.Contains(ops[0].Result.Evidence, "backend crashed") {
		t.Fatalf("operations: %+v", ops)
	}
	reason := ops[0].Result.Evidence
	byID := map[string]trace.Transition{}
	for _, tr := range f.Transitions(t, stream) {
		byID[tr.ID] = tr
	}
	if failed := byID["draft-1-failed"]; failed.From != "drafting-1" || failed.To != "failed-1" || failed.Reason != reason || failed.Actor != intake.DraftingActor || failed.Cause != ops[0].Operation.ID {
		t.Fatalf("failed transition: %+v", failed)
	}
	if next := byID["draft-2"]; next.From != "failed-1" || next.To != "drafting-2" || next.Cause != "draft-1-failed" {
		t.Fatalf("second request: %+v", next)
	}
	th := f.ArchitectThread(t, stream)
	if len(th.Turns) != 2 || th.Turns[0].Status() != "failed" || th.Turns[1].Status() != "idle" {
		t.Fatalf("thread: %+v", th)
	}
	prompt := th.Turns[1].Request.Prompt
	if want := "Draft 1 was not accepted:\n" + reason + "\n\nNo file of that draft was recorded. Deliver both files.\n\n"; !strings.HasPrefix(prompt, want) || strings.Contains(prompt, "draft/") {
		t.Fatalf("second prompt: %q", prompt)
	}
}

func TestArchitectDraftWaitsForARunner(t *testing.T) {
	t.Parallel()
	f := journey.NewArchitectFixture(t)
	ctx := context.Background()
	entered := make(chan struct{})
	var once sync.Once
	f.Engine.Mu.Lock()
	f.Engine.Turns["draft-1-1"] = func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return nil, ctx.Err()
	}
	f.Engine.Mu.Unlock()
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	select {
	case <-entered:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("the draft did not start")
	}
	f.Stop(t)

	retried := func(before int, turns int) int {
		t.Helper()
		var n int
		f.S.Until(t, "the draft to be retried", func() bool {
			ops := f.DraftOperations(t, stream)
			if len(ops) != 1 || ops[0].Result != nil {
				t.Fatalf("operations: %+v", ops)
			}
			n = 0
			for _, a := range ops[0].History {
				if a.Kind == "retry" && strings.Contains(a.Failure, "this service has no agent runner for the architect") {
					n++
				}
			}
			return n > before
		})
		if state, err := f.Repository().Workflow(stream, intake.DraftSubject); err != nil || state.Value != "drafting-1" {
			t.Fatalf("draft state %+v %v", state, err)
		}
		if th := f.ArchitectThread(t, stream); len(th.Turns) != turns {
			t.Fatalf("thread: %+v", th)
		}
		if runs := f.Runs(); !slices.Equal(runs, []string{"draft-1-1"}) {
			t.Fatalf("backend runs %v", runs)
		}
		return n
	}

	runner := f.Opts.Architect
	f.Opts.Architect = nil
	f.Start(t)
	count := retried(0, 1)
	f.Stop(t)

	cfg, err := projectstore.LoadConfig(f.Opts.Config)
	fixturekit.Must(t, err)
	repo, err := trace.Open(cfg.Root, config.Project{ID: f.Project, Clone: f.Clone})
	fixturekit.Must(t, err)
	th, err := repo.Thread(stream, intake.ArchitectAgent)
	fixturekit.Must(t, err)
	second := th.Turns[0].Request
	second.ID, second.TurnID, second.At = "request_draft-1-2", "draft-1-2", f.Clock.Now()
	_, err = repo.EnqueueTurn(ctx, second)
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo.Close())
	f.Start(t)
	retried(count, 2)
	if th := f.ArchitectThread(t, stream); th.Turns[1].Claim != nil {
		t.Fatalf("queued turn: %+v", th.Turns[1])
	}
	f.Stop(t)

	f.Opts.Architect = runner
	f.Script("draft-1-2", map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.ValidPlan}, nil)
	f.Start(t)
	defer f.Stop(t)
	f.Await(t, stream, intakefixture.Sketched)
	if runs := f.Runs(); !slices.Equal(runs, []string{"draft-1-1", "draft-1-2"}) {
		t.Fatalf("backend runs %v", runs)
	}
}

func TestArchitectDraftsBeforeTheScheduleHook(t *testing.T) {
	t.Parallel()
	opts, clone, engine, sessions, clock := journey.NewArchitectOptions(t)
	var repository atomic.Pointer[trace.Repository]
	var mu sync.Mutex
	var seen []string

	undrafted := map[string]bool{}
	opts.Reconciliation.Schedule = func(context.Context) error {
		repo := repository.Load()
		if repo == nil {
			return nil
		}
		streams, err := repo.Workstreams()
		if err != nil {
			return err
		}
		for _, stream := range streams {
			feature, err := repo.Workflow(stream, trace.FeatureSubject)
			if err != nil {
				return err
			}
			if feature.Value != intake.HandedState {
				continue
			}
			draft, err := repo.Workflow(stream, intake.DraftSubject)
			if err != nil {
				return err
			}
			mu.Lock()
			key := fmt.Sprint(stream)
			if draft.Value == "" && !undrafted[key] {
				undrafted[key] = true
			} else {
				seen = append(seen, draft.Value)
			}
			mu.Unlock()
		}
		return nil
	}
	f := &journey.ArchitectFixture{Opts: opts, Clone: clone, Engine: engine, Sessions: sessions, Clock: clock}
	f.Start(t)
	defer f.Stop(t)
	added, err := f.C.AddProject(context.Background(), fixturekit.Request(clone))
	fixturekit.Must(t, err)
	f.Project, f.Trace = added.Project.ID, added.Project.Trace
	intakefixture.WriteCharter(t, f.C, added.Project.ID, "1. Keep changes small.\n")
	repository.Store(f.Repository())
	f.Script("draft-1-1", map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.ValidPlan}, nil)
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.Await(t, stream, intakefixture.Sketched)

	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 || slices.ContainsFunc(seen, func(v string) bool { return v != "drafting-1" }) {
		t.Fatalf("draft states the Schedule hook saw: %q", seen)
	}
}

func TestArchitectDraftSurvivesRestart(t *testing.T) {
	t.Parallel()
	for _, crash := range []string{"during-turn", "captured", "completed", "recorded", "moved"} {
		t.Run(crash, func(t *testing.T) {
			f := journey.NewArchitectFixture(t)
			ctx := context.Background()

			entered := make(chan struct{})
			var once sync.Once
			f.Engine.Mu.Lock()
			f.Engine.Turns["draft-1-1"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
				once.Do(func() { close(entered) })
				<-ctx.Done()
				return nil, ctx.Err()
			}
			f.Engine.Mu.Unlock()
			stream := f.HandIn(t, "design", fixturekit.HandedDesign)
			select {
			case <-entered:
			case <-time.After(fixturekit.DemoTimeout):
				t.Fatal("the draft did not start")
			}
			f.Stop(t)

			cfg, err := projectstore.LoadConfig(f.Opts.Config)
			fixturekit.Must(t, err)
			repo, err := trace.Open(cfg.Root, config.Project{ID: f.Project, Clone: f.Clone})
			fixturekit.Must(t, err)
			th, err := repo.Thread(stream, intake.ArchitectAgent)
			fixturekit.Must(t, err)
			if len(th.Turns) != 1 || th.Turns[0].Status() != "interrupted" || th.Active != "" {
				t.Fatalf("interrupted turn: %+v", th)
			}
			if state, err := repo.Workflow(stream, intake.DraftSubject); err != nil || state.Value != "drafting-1" {
				t.Fatalf("draft state %+v %v", state, err)
			}
			ops, err := repo.Operations(stream)
			fixturekit.Must(t, err)
			operation := ""
			for _, o := range ops {
				if o.Operation.Action == intake.DraftAction {
					operation = o.Operation.ID
				}
			}
			runs := []string{"draft-1-1"}
			if crash == "during-turn" {
				f.Script("draft-1-2", map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.ValidPlan}, nil)
				runs = append(runs, "draft-1-2")
			} else {

				second := th.Turns[0].Request
				second.ID, second.TurnID, second.At = "request_draft-1-2", "draft-1-2", f.Clock.Now()
				_, err = repo.EnqueueTurn(ctx, second)
				fixturekit.Must(t, err)
				directory := filepath.Join(cfg.Root.String(), "architect", string(f.Project), string(stream), "draft-1-2")
				claimed, err := repo.ClaimTurn(ctx, stream, intake.ArchitectAgent, "earlier-session", filepath.Join(directory, "session"), f.Clock.Now())
				fixturekit.Must(t, err)
				h := second.Header
				h.Schema, h.ID, h.At, h.Actor = "osmia.trace.turn-response", trace.EventID(second.ID, "response"), f.Clock.Now(), trace.Actor{Kind: "service", ID: "thread-runner"}
				response := trace.TurnResponse{Header: h, AgentID: intake.ArchitectAgent, ThreadID: second.ThreadID, TurnID: second.TurnID, RequestID: second.ID, RequestRevision: second.Revision,
					Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: second.Profile.Backend, ID: "session-draft-1-2"}, SessionDirectory: claimed.Claim.SessionDirectory, StartedAt: claimed.Claim.At, Duration: time.Second, FinalResponse: "Draft delivered"}}
				fixturekit.Must(t, repo.CaptureTurn(ctx, "earlier-session", response))
				if crash != "captured" {
					fixturekit.Must(t, repo.CompleteTurn(ctx, stream, intake.ArchitectAgent, second.TurnID, "earlier-session", f.Clock.Now()))
				}
				fixturekit.Must(t, os.MkdirAll(filepath.Join(directory, "output"), 0700))
				fixturekit.Must(t, os.WriteFile(filepath.Join(directory, "output", "spec.md"), []byte(fixturekit.ValidSpec), 0600))
				fixturekit.Must(t, os.WriteFile(filepath.Join(directory, "output", "plan.json"), []byte(fixturekit.ValidPlan), 0600))
				if crash == "recorded" || crash == "moved" {
					at := f.Clock.Now()
					header := func(id string) trace.Header {
						return trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: f.Project, Workstream: stream, At: at, Actor: intake.ArchitectActor, Cause: operation, Depth: 1}
					}
					fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: header(plan.SpecDocument), Path: plan.SpecPath, Content: fixturekit.ValidSpec}, {Header: header(plan.PlanDocument), Path: plan.PlanPath, Content: fixturekit.ValidPlan}}))
				}
				if crash == "moved" {

					h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "abandoned", Revision: 1, Project: f.Project, Workstream: stream, At: f.Clock.Now(), Actor: intake.OwnerActor, Cause: "owner"}
					_, err := repo.SetFeatureState(ctx, h, "abandoned", "the owner abandoned the workstream")
					fixturekit.Must(t, err)
				}
			}
			fixturekit.Must(t, repo.Close())

			f.Start(t)
			defer f.Stop(t)
			if crash == "moved" {
				f.Await(t, stream, intakefixture.DraftAt("failed-1"))
				ops := f.AcknowledgedDraftOperations(t, stream)
				if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "failed" || !strings.Contains(ops[0].Result.Evidence, "the workstream is abandoned, not handed, so the valid draft was recorded and not presented") {
					t.Fatalf("operations: %+v", ops)
				}
				if state, err := f.Repository().Workflow(stream, trace.FeatureSubject); err != nil || state.Value != "abandoned" {
					t.Fatalf("feature state %+v %v", state, err)
				}
				fixturekit.Must(t, (&intake.Drafter{S: f.S.Intake().DrafterDependencies(), Repository: f.Repository()}).Pass(ctx))
				if ops := f.DraftOperations(t, stream); len(ops) != 1 {
					t.Fatalf("another draft was requested: %+v", ops)
				}
				if docs := f.Documents(t, stream, plan.SpecDocument); len(docs) != 1 {
					t.Fatalf("spec revisions: %+v", docs)
				}
				return
			}
			f.Await(t, stream, intakefixture.Sketched)
			if got := f.Runs(); !slices.Equal(got, runs) {
				t.Fatalf("backend runs %v, want %v", got, runs)
			}
			th = f.ArchitectThread(t, stream)
			if len(th.Turns) != 2 || th.Turns[0].Status() != "interrupted" || th.Turns[0].Response.Failure == "" || th.Turns[1].Status() != "idle" || th.Active != "" {
				t.Fatalf("thread after recovery: %+v", th)
			}

			ops = f.AcknowledgedDraftOperations(t, stream)
			kinds := map[string]int{}
			for _, a := range ops[0].History {
				kinds[a.Kind]++
			}
			if len(ops) != 1 || !ops[0].Acknowledged || kinds["result"] != 1 || kinds["claim"] < 2 || ops[0].Result.Outcome != "succeeded" {
				t.Fatalf("operation history: %+v", kinds)
			}
			for _, id := range []string{plan.SpecDocument, plan.PlanDocument} {
				if docs := f.Documents(t, stream, id); len(docs) != 1 || docs[0].Cause != operation {
					t.Fatalf("%s revisions: %+v", id, docs)
				}
			}
			moves := 0
			for _, tr := range f.Transitions(t, stream) {
				if tr.To == intake.SketchedState {
					moves++
				}
			}
			if moves != 1 {
				t.Fatalf("sketched transitions: %d", moves)
			}
			if state, err := f.Repository().Workflow(stream, trace.FeatureSubject); err != nil || state != (trace.WorkflowState{Version: 2, Value: intake.SketchedState}) {
				t.Fatalf("feature state %+v %v", state, err)
			}
		})
	}
}

func TestAbandonStopsTheArchitectDraft(t *testing.T) {
	t.Parallel()
	const evidence = "draft 1 failed: the workstream was abandoned, so the architect runs no turn for it"

	blocked := func(f *journey.ArchitectFixture) chan struct{} {
		entered := make(chan struct{})
		var once sync.Once
		f.Engine.Mu.Lock()
		f.Engine.Turns["draft-1-1"] = func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
			once.Do(func() { close(entered) })
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(30 * time.Second):
				return nil, errors.New("the turn was not cancelled")
			}
		}
		f.Engine.Mu.Unlock()
		return entered
	}
	check := func(t *testing.T, f *journey.ArchitectFixture, stream config.WorkstreamID) {
		t.Helper()
		f.Await(t, stream, intakefixture.DraftAt("failed-1"))
		ops := f.AcknowledgedDraftOperations(t, stream)
		if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "failed" || ops[0].Result.Evidence != evidence {
			t.Fatalf("operations: %+v", ops)
		}
		if got := f.Runs(); !slices.Equal(got, []string{"draft-1-1"}) {
			t.Fatalf("backend runs %v", got)
		}
		th := f.ArchitectThread(t, stream)
		if len(th.Turns) != 1 || th.Turns[0].Status() != "interrupted" || !th.Turns[0].Response.Result.Cancelled || th.Active != "" {
			t.Fatalf("thread: %+v", th)
		}
		if state, err := f.Repository().Workflow(stream, trace.FeatureSubject); err != nil || state.Value != intake.AbandonedState {
			t.Fatalf("feature state %+v %v", state, err)
		}
		fixturekit.Must(t, (&intake.Drafter{S: f.S.Intake().DrafterDependencies(), Repository: f.Repository()}).Pass(context.Background()))
		if ops := f.DraftOperations(t, stream); len(ops) != 1 {
			t.Fatalf("another draft was requested: %+v", ops)
		}
		if docs := f.Documents(t, stream, plan.SpecDocument); len(docs) != 0 {
			t.Fatalf("spec revisions: %+v", docs)
		}
	}

	t.Run("running turn", func(t *testing.T) {
		f := journey.NewArchitectFixture(t)
		defer f.Stop(t)
		entered := blocked(f)
		stream := f.HandIn(t, "design", fixturekit.HandedDesign)
		select {
		case <-entered:
		case <-time.After(fixturekit.DemoTimeout):
			t.Fatal("the draft did not start")
		}
		if code, body := intakefixture.AbandonCall(t, f.S.Handle, stream, `{"reason":"Superseded"}`); code != http.StatusOK {
			t.Fatalf("abandon: %d %s", code, body)
		}
		check(t, f, stream)
	})

	t.Run("after a restart", func(t *testing.T) {
		f := journey.NewArchitectFixture(t)
		entered := blocked(f)
		stream := f.HandIn(t, "design", fixturekit.HandedDesign)
		select {
		case <-entered:
		case <-time.After(fixturekit.DemoTimeout):
			t.Fatal("the draft did not start")
		}
		f.Stop(t)

		cfg, err := projectstore.LoadConfig(f.Opts.Config)
		fixturekit.Must(t, err)
		repo, err := trace.Open(cfg.Root, config.Project{ID: f.Project, Clone: f.Clone})
		fixturekit.Must(t, err)
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: intake.AbandonTransition, Revision: 1, Project: f.Project, Workstream: stream, At: f.Clock.Now(), Actor: intake.OwnerActor, Cause: intake.AbandonTransition}
		_, err = repo.SetFeatureStateUnless(context.Background(), h, intake.AbandonedState, "Superseded", intake.AbandonedState, intake.DeliveredState)
		fixturekit.Must(t, err)
		fixturekit.Must(t, repo.Close())
		f.Start(t)
		defer f.Stop(t)
		check(t, f, stream)
	})

	t.Run("queued turn", func(t *testing.T) {
		f := journey.NewArchitectFixture(t)
		entered := blocked(f)
		stream := f.HandIn(t, "design", fixturekit.HandedDesign)
		select {
		case <-entered:
		case <-time.After(fixturekit.DemoTimeout):
			t.Fatal("the draft did not start")
		}
		f.Stop(t)

		ctx := context.Background()
		cfg, err := projectstore.LoadConfig(f.Opts.Config)
		fixturekit.Must(t, err)
		repo, err := trace.Open(cfg.Root, config.Project{ID: f.Project, Clone: f.Clone})
		fixturekit.Must(t, err)
		defer repo.Close()
		th, err := repo.Thread(stream, intake.ArchitectAgent)
		fixturekit.Must(t, err)
		second := th.Turns[0].Request
		second.ID, second.TurnID, second.At = "request_draft-1-2", "draft-1-2", f.Clock.Now()
		_, err = repo.EnqueueTurn(ctx, second)
		fixturekit.Must(t, err)
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: intake.AbandonTransition, Revision: 1, Project: f.Project, Workstream: stream, At: f.Clock.Now(), Actor: intake.OwnerActor, Cause: intake.AbandonTransition}
		_, err = repo.SetFeatureStateUnless(ctx, h, intake.AbandonedState, "Superseded", intake.AbandonedState, intake.DeliveredState)
		fixturekit.Must(t, err)
		ops, err := repo.Operations(stream)
		fixturekit.Must(t, err)
		var op coreadapter.Operation
		for _, o := range ops {
			if o.Operation.Action == intake.DraftAction {
				op = o.Operation
			}
		}
		result, err := (&intake.Drafter{S: f.S.Intake().DrafterDependencies(), Repository: repo}).Apply(ctx, op)
		if err != nil || result.Outcome != "failed" || result.Evidence != evidence {
			t.Fatalf("result %+v: %v", result, err)
		}
		if got := f.Runs(); !slices.Equal(got, []string{"draft-1-1"}) {
			t.Fatalf("backend runs %v", got)
		}
		th, err = repo.Thread(stream, intake.ArchitectAgent)
		fixturekit.Must(t, err)
		if len(th.Turns) != 2 || th.Turns[1].Response == nil || !th.Turns[1].Response.Result.Cancelled || th.Turns[1].Response.Actor != intake.AbandonActor || th.Turns[1].Response.Failure != intake.CancelReason {
			t.Fatalf("thread: %+v", th)
		}
	})
}

func TestSchedulerLeavesArchitectTurnsToTheDrafter(t *testing.T) {
	t.Parallel()
	f := journey.NewArchitectFixture(t)
	defer f.Stop(t)
	ctx := context.Background()
	const created = config.WorkstreamID("w_00000000000000000000000000000001")
	fixturekit.Must(t, f.Repository().CreateWorkstream(ctx, created, f.Clock.Now(), intake.OwnerActor))
	holds := f.S.DispatchHolds(f.S.Current(), f.Repository())
	architect := scheduler.Candidate{Workstream: created, Thread: trace.Thread{Identity: trace.Agent{Role: intake.ArchitectRole}}}
	if held, err := holds(architect); err != nil || !held {
		t.Fatalf("architect turn not held: %t %v", held, err)
	}
	other := scheduler.Candidate{Workstream: created, Thread: trace.Thread{Identity: trace.Agent{Role: "mason"}}}
	if held, err := holds(other); err != nil || held {
		t.Fatalf("mason turn held: %t %v", held, err)
	}
	f.Stop(t)

	opts, clone, engine, sessions, clock := journey.NewArchitectOptions(t)
	opts.Threads = func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
		return &fixturekit.CompletedRunner{}, nil
	}
	f2 := &journey.ArchitectFixture{Opts: opts, Clone: clone, Engine: engine, Sessions: sessions, Clock: clock}
	f2.Start(t)
	defer f2.Stop(t)
	added, err := f2.C.AddProject(ctx, fixturekit.Request(clone))
	fixturekit.Must(t, err)
	f2.Project, f2.Trace = added.Project.ID, added.Project.Trace
	intakefixture.WriteCharter(t, f2.C, added.Project.ID, "1. Keep changes small.\n")
	f2.Script("draft-1-1", map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.ValidPlan}, nil)
	stream := f2.HandIn(t, "design", fixturekit.HandedDesign)
	f2.Await(t, stream, intakefixture.Sketched)
	if runs := f2.Runs(); !slices.Equal(runs, []string{"draft-1-1"}) {
		t.Fatalf("backend runs %v", runs)
	}
	ops, err := f2.Repository().Operations(stream)
	fixturekit.Must(t, err)
	for _, o := range ops {
		if o.Operation.Action != intake.DraftAction {
			var in struct{ Agent string }
			fixturekit.Must(t, json.Unmarshal(o.Operation.Input, &in))
			if in.Agent == intake.ArchitectAgent {
				t.Fatalf("the scheduler published %s for the architect's turn", o.Operation.Action)
			}
		}
	}
}
