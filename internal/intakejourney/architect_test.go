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

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	plan "github.com/kpenfound/osmia/internal/plan"
	questions "github.com/kpenfound/osmia/internal/questions"
	scheduler "github.com/kpenfound/osmia/internal/scheduler"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// architectFixture is an active project with a ruled charter, a clone whose
// tracked files seed the entity map, and the fake engine the architect's
// turns run in.
type architectFixture struct {
	opts        Options
	s           *Service
	serviceDone chan struct{}
	c           *Client
	project     config.ProjectID
	trace       string
	clone       string
	engine      *demoEngine
	sessions    *demoSessions
	clock       *demoClock

	scenarioOnce  sync.Once
	scenarioState *intakefixture.ArchitectScenario
}

func newArchitectOptions(t *testing.T) (Options, string, *demoEngine, *demoSessions, *demoClock) {
	t.Helper()
	opts, clone := projectFixture(t)
	engine, sessions, clock, architect, issues := intakefixture.ArchitectExecution(t, opts.Config, clone)
	opts.Reconciliation.Now, opts.Architect, opts.Issues = clock.Now, architect, issues
	return opts, clone, engine, sessions, clock
}

func newArchitectFixture(t *testing.T, configure ...func(*Options)) *architectFixture {
	t.Helper()
	opts, clone, engine, sessions, clock := newArchitectOptions(t)
	for _, apply := range configure {
		apply(&opts)
	}
	f := &architectFixture{opts: opts, clone: clone, engine: engine, sessions: sessions, clock: clock}
	f.start(t)
	f.project, f.trace = intakefixture.AddArchitectProject(t, f.c, clone, "1. Keep changes small.\n2. Every change has a test.\n", func(t *testing.T, p config.ProjectID, text string) { writeCharter(t, f.c, p, text) })
	return f
}

func (f *architectFixture) start(t *testing.T) {
	t.Helper()
	f.s, f.c, f.serviceDone = intakefixture.StartArchitect(t, func(ctx context.Context) (*Service, error) { return Start(ctx, f.opts) }, NewClient, func(c *Client) {
		if f.opts.WriteTimeout > c.defaultTimeout {
			c.defaultTimeout = f.opts.WriteTimeout
			c.Client.SetTimeoutForTest(c.defaultTimeout)
		}
	})
}

func (f *architectFixture) stop(t *testing.T) {
	t.Helper()
	intakefixture.StopArchitect(t, f.c.Close, f.s.Close, f.serviceDone)
}

func (f *architectFixture) handIn(t *testing.T, key, content string) config.WorkstreamID {
	t.Helper()
	return f.scenario().HandIn(t, f.project, key, content)
}

func (f *architectFixture) repository() *trace.Repository { return f.s.sole().repository }

// script installs the fake architect's behaviour for one turn: it delivers
// the given files and returns a successful result.
func (f *architectFixture) script(turn string, files map[string]string, check func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error) {
	f.scenario().Script(turn, files, check)
}

// delivered returns what each draft_write the scripted turn made returned.
func (f *architectFixture) delivered(turn string) []draftStatus {
	return f.scenario().Delivered(turn)
}

func (f *architectFixture) runs() []string {
	return f.scenario().Runs()
}

// await polls the workstream's feature and draft states until want accepts
// them.
func (f *architectFixture) await(t *testing.T, stream config.WorkstreamID, want func(feature, draft string) bool) {
	t.Helper()
	f.scenario().Await(t, stream, want)
}

var sketched = intakefixture.Sketched

var draftAt = intakefixture.DraftAt

func (f *architectFixture) transitionRecorded(t *testing.T, stream config.WorkstreamID, id string) func(string, string) bool {
	t.Helper()
	return f.scenario().TransitionRecorded(t, stream, id)
}

func (f *architectFixture) documents(t *testing.T, stream config.WorkstreamID, id string) []trace.Document {
	t.Helper()
	return f.scenario().Documents(t, stream, id)
}

func (f *architectFixture) transitions(t *testing.T, stream config.WorkstreamID) []trace.Transition {
	t.Helper()
	return f.scenario().Transitions(t, stream)
}

func (f *architectFixture) draftOperations(t *testing.T, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	return f.scenario().DraftOperations(t, stream)
}

// acknowledgedDraftOperations returns the draft operations once every one of
// them is acknowledged, for assertions reached through an awaited state.
func (f *architectFixture) acknowledgedDraftOperations(t *testing.T, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	return f.scenario().AcknowledgedDraftOperations(t, stream)
}

func (f *architectFixture) architectThread(t *testing.T, stream config.WorkstreamID) trace.Thread {
	t.Helper()
	return f.scenario().ArchitectThread(t, stream)
}

var readTool = intakefixture.ReadTool

var checkReadOnlyGrants = intakefixture.CheckReadOnlyGrants

// checkArchitectBoundary makes the negative assertions from inside the turn:
// the view holds the handed input, the charter and the bundle and nothing
// else, the role reads files and delivers the draft, and nothing carries
// notes, write, execute, network or VCS access.
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
	if want := []string{questions.AskTool, DraftTool, "factory_context", "file_read", "inspect_code", "project_context", "read_remote_file"}; !slices.Equal(names, want) {
		fail("role tools %v, want %v", names, want)
	}
	for _, name := range []string{"file_write", "shell", "git_push", "set_status", "notes_read", "notes_write"} {
		if _, err := callTool(ctx, tools, name, map[string]any{"path": "x", "content": "y"}); err == nil {
			fail("runtime called %s", name)
		}
	}
	for path, want := range map[string]string{"handed/stdin": handed, "charter.md": charter} {
		if got, err := readTool(ctx, tools, path); err != nil || got != want {
			fail("read %s: %q %v", path, got, err)
		}
	}
	context, err := readTool(ctx, tools, "context.md")
	if err != nil {
		fail("read context.md: %v", err)
	}
	for _, want := range []string{"# Project context", "context mode: file", "- charter#1: Keep changes small.", "- charter#2: Every change has a test.", "project_context", "kb/entities.json"} {
		if !strings.Contains(context, want) {
			fail("context.md lacks %q:\n%s", want, context)
		}
	}
	entities, err := callTool(ctx, tools, projectContextTool, map[string]any{"path": "kb/entities.json"})
	if err != nil || !strings.Contains(entities, "internal.trace") {
		fail("retrieving entities: %s: %v", entities, err)
	}
	for _, path := range []string{"repo/CODEOWNERS", "secret.env", "../config.toml", ".git/HEAD", filepath.Join(clone, "CODEOWNERS"), "kb/entities.json"} {
		if _, err := readTool(ctx, tools, path); err == nil {
			fail("runtime read %s", path)
		}
	}
	for _, args := range []map[string]any{{"path": "notes.md", "content": "x"}, {"path": "../spec.md", "content": "x"}, {"path": "spec.md"}, {"path": "spec.md", "content": "x", "extra": 1}, {"path": "plan.json", "content": strings.Repeat("x", MaxHandedBytes+1)}} {
		if _, err := callTool(ctx, tools, DraftTool, args); err == nil {
			fail("draft_write accepted %v", args)
		}
	}
	checkReadOnlyGrants(req, verified, clone, fail)
	if req.Profile.VCSAccess || req.Workspace == nil || req.Workspace.VCS() != nil || len(req.VCSEnv) != 0 || req.Profile.Name != architectRole {
		fail("request carries VCS access or another role: %+v", req.Profile)
	}
	for key, value := range req.Env {
		if key != "OSMIA_MCP_TOKEN" || strings.Contains(value, demoSecret) {
			fail("runtime environment exposes %s", key)
		}
	}
	for _, want := range []string{"handed/stdin", "charter.md", "context.md", "intended behaviour", "must not do", `"## Acceptance criteria"`, "numbered list", "spec#<n>", "footprint", "no cycle", "at least one acceptance item", "Start with one coherent unit", DraftTool} {
		if !strings.Contains(req.Prompt, want) {
			fail("prompt lacks %q", want)
		}
	}
	if !strings.Contains(req.SystemPrompt, "architect of the dagger project") || !strings.Contains(req.SystemPrompt, "no version control tool") {
		fail("system prompt: %q", req.SystemPrompt)
	}
	if strings.Contains(req.Prompt+req.SystemPrompt, demoSecret) {
		fail("prompt exposes the untracked secret")
	}
	return errors.Join(problems...)
}

func TestArchitectDraftsAndSketchesAHandedWorkstream(t *testing.T) {

	for _, name := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		t.Setenv(name, demoSecret)
	}
	f := newArchitectFixture(t)
	defer f.stop(t)
	ctx := context.Background()
	charter := "1. Keep changes small.\n2. Every change has a test.\n"
	f.script("draft-1-1", map[string]string{plan.SpecPath: validSpec, plan.PlanPath: validPlan},
		func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
			err := checkArchitectBoundary(ctx, req, verified, tools, f.clone, handedDesign, charter)
			if req.ResumeID != "" || strings.Contains(req.Prompt, "was not accepted") {
				err = errors.Join(err, fmt.Errorf("first turn carries history: %q", req.ResumeID))
			}
			if _, e := readTool(ctx, tools, "draft/spec.md"); e == nil {
				err = errors.Join(err, errors.New("first turn sees a previous draft"))
			}
			return err
		})
	head := traceCommits(t, f.trace)
	stream := f.handIn(t, "design", handedDesign)
	f.await(t, stream, sketched)
	if runs := f.runs(); !slices.Equal(runs, []string{"draft-1-1"}) {
		t.Fatalf("backend runs %v", runs)
	}

	ops := f.acknowledgedDraftOperations(t, stream)
	if len(ops) != 1 || !ops[0].Acknowledged || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" || !strings.Contains(ops[0].Result.Evidence, "draft 1 passed validation") {
		t.Fatalf("operations: %+v", ops)
	}
	operation := ops[0].Operation.ID
	for id, want := range map[string]struct{ path, content string }{plan.SpecDocument: {plan.SpecPath, validSpec}, plan.PlanDocument: {plan.PlanPath, validPlan}} {
		docs := f.documents(t, stream, id)
		if len(docs) != 1 || docs[0].Revision != 1 || docs[0].Path != want.path || docs[0].Content != want.content || docs[0].Actor != architectActor || docs[0].Cause != operation || docs[0].Source != "" {
			t.Fatalf("%s revisions: %+v", id, docs)
		}
		data, err := workstreamDocument(t, f.s.current().Root, f.project, stream, want.path)
		if err != nil || string(data) != want.content {
			t.Fatalf("%s: %q %v", want.path, data, err)
		}
	}
	spec := documentCommit(t, f.s.current().Root, f.project, stream, "spec.md")
	if other := documentCommit(t, f.s.current().Root, f.project, stream, "plan.json"); other != spec {
		t.Fatalf("revisions landed in separate commits: %s %s", spec, other)
	}
	// One transition moves the feature, with its actor and reason, and its
	// notice tells the chief of staff the draft exists.
	var moves []trace.Transition
	for _, tr := range f.transitions(t, stream) {
		if tr.Subject == trace.FeatureSubject {
			moves = append(moves, tr)
		}
	}
	if len(moves) != 2 || moves[1].ID != "sketched" || moves[1].From != HandedState || moves[1].To != SketchedState || moves[1].Actor != draftingActor || moves[1].Cause != operation ||
		moves[1].Reason != "the architect's draft 1 passed validation: spec.md revision 1 with 2 acceptance criteria and plan.json revision 1 with 2 units" {
		t.Fatalf("feature transitions: %+v", moves)
	}
	outbox, err := f.repository().Outbox(stream)
	must(t, err)
	var notices []trace.OutboxEntry
	for _, e := range outbox {
		if chiefEvent(e.Event) && e.TransitionID == "sketched" {
			notices = append(notices, e)
		}
	}
	if len(notices) != 1 || !strings.Contains(notices[0].Event.Body, "Workstream state changed from handed to sketched: the architect's draft 1 passed validation") {
		t.Fatalf("outbox: %+v", outbox)
	}
	if state, err := f.repository().Workflow(stream, trace.FeatureSubject); err != nil || state != (trace.WorkflowState{Version: 2, Value: SketchedState}) {
		t.Fatalf("feature state %+v %v", state, err)
	}
	status, err := f.c.Status(ctx, stream)
	must(t, err)
	if status.State == nil || *status.State != SketchedState {
		t.Fatalf("status: %+v", status)
	}

	th := f.architectThread(t, stream)
	if len(th.Turns) != 1 || th.Turns[0].Request.TurnID != "draft-1-1" || th.Turns[0].Status() != "idle" || th.Identity.Role != architectRole || th.Active != "" {
		t.Fatalf("architect thread: %+v", th)
	}
	if notes, ok := traceFile(t, f.trace, "notes/architect.md"); ok {
		t.Fatalf("architect notes: %q", notes)
	}

	other := config.WorkstreamID("w_0123456789abcdef0123456789abcdef")
	must(t, f.repository().CreateWorkstream(ctx, other, f.clock.Now(), ownerActor))
	_, err = f.repository().SetFeatureState(ctx, trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "ratified", Revision: 1, Project: f.project, Workstream: other, At: f.clock.Now(), Actor: ownerActor, Cause: "owner"}, "ratified", "the owner ratified the plan")
	must(t, err)
	d := &drafter{S: f.s.intake().DrafterDependencies(), Repository: f.repository()}
	must(t, d.Pass(ctx))
	if state, err := f.repository().Workflow(other, draftSubject); err != nil || state.Value != "" {
		t.Fatalf("a ratified workstream was drafted: %+v %v", state, err)
	}
	if _, err := f.repository().Thread(other, architectAgent); !errors.Is(err, os.ErrNotExist) {
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
	if again := f.draftOperations(t, stream); len(again) != 1 {
		t.Fatalf("a second draft was requested: %+v", again)
	}
	if th := f.architectThread(t, stream); len(th.Turns) != 1 {
		t.Fatalf("a second turn was queued: %+v", th)
	}
	if docs := f.documents(t, stream, plan.SpecDocument); len(docs) != 1 {
		t.Fatalf("retry duplicated revisions: %+v", docs)
	}
	if len(slices.DeleteFunc(f.transitions(t, stream), func(tr trace.Transition) bool { return tr.Subject == factoryPolicySubject })) != 3 {
		t.Fatalf("transitions: %+v", f.transitions(t, stream))
	}

	root := f.opts.Config.Root
	if entries, err := os.ReadDir(filepath.Join(root, "views")); err != nil || len(entries) != 0 {
		t.Fatalf("views leaked: %v %v", entries, err)
	}
	turnDir := filepath.Join(root, "architect", string(f.project), string(stream), "draft-1-1")
	if _, err := os.Lstat(filepath.Join(turnDir, "workspace")); !os.IsNotExist(err) {
		t.Fatal("staged workspace retained")
	}
	if data, err := os.ReadFile(filepath.Join(turnDir, "output", "spec.md")); err != nil || string(data) != validSpec {
		t.Fatalf("delivered spec: %q %v", data, err)
	}
	for path, content := range snapshot(t, filepath.Join(root, "projects")) {
		if strings.Contains(content, demoSecret) {
			t.Fatalf("secret copied into %s", path)
		}
	}
	if traceCommits(t, f.trace) == head {
		t.Fatal("the trace did not move")
	}
}

func TestArchitectResubmitsAnInvalidDraft(t *testing.T) {
	t.Parallel()
	f := newArchitectFixture(t)
	defer f.stop(t)

	sawInvalid1, sawInvalid2 := make(chan struct{}), make(chan struct{})
	gate := func(ctx context.Context, open <-chan struct{}) error {
		select {
		case <-open:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(demoTimeout):
			return errors.New("the test never released the turn")
		}
	}
	f.script("draft-1-1", map[string]string{plan.SpecPath: validSpec},
		func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
			output := filepath.Join(filepath.Dir(req.SessionDir), "output")
			if err := os.MkdirAll(output, 0700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(output, plan.PlanPath), []byte{'{', 0xff, 0xfe, '}'}, 0600)
		})
	f.script("draft-2-1", map[string]string{plan.SpecPath: validSpec, plan.PlanPath: cyclicPlan},
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
			if got, err := readTool(ctx, tools, "draft/spec.md"); err != nil || got != validSpec {
				problems = append(problems, fmt.Errorf("previous spec: %q %v", got, err))
			}
			if _, err := readTool(ctx, tools, "draft/plan.json"); err == nil {
				problems = append(problems, errors.New("a plan that was never delivered is in the view"))
			}
			if !strings.Contains(req.Prompt, "Draft delivered") {
				problems = append(problems, errors.New("second turn did not replay the first"))
			}
			return errors.Join(problems...)
		})
	f.script("draft-3-1", map[string]string{plan.SpecPath: validSpec, plan.PlanPath: validPlan},
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
			if got, err := readTool(ctx, tools, "draft/plan.json"); err != nil || got != cyclicPlan {
				problems = append(problems, fmt.Errorf("previous plan: %q %v", got, err))
			}
			return errors.Join(problems...)
		})
	stream := f.handIn(t, "design", handedDesign)
	f.await(t, stream, f.transitionRecorded(t, stream, "draft-1-invalid"))

	if docs := f.documents(t, stream, plan.SpecDocument); len(docs) != 1 || docs[0].Content != validSpec || docs[0].Actor != architectActor {
		t.Fatalf("spec after draft 1: %+v", docs)
	}
	if docs := f.documents(t, stream, plan.PlanDocument); len(docs) != 0 {
		t.Fatalf("plan after draft 1: %+v", docs)
	}
	close(sawInvalid1)
	f.await(t, stream, f.transitionRecorded(t, stream, "draft-2-invalid"))
	if docs := f.documents(t, stream, plan.PlanDocument); len(docs) != 1 || docs[0].Content != cyclicPlan || docs[0].Revision != 1 {
		t.Fatalf("plan after draft 2: %+v", docs)
	}
	if feature, err := f.repository().Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != HandedState {
		t.Fatalf("feature state %+v %v", feature, err)
	}
	close(sawInvalid2)
	f.await(t, stream, sketched)
	if runs := f.runs(); !slices.Equal(runs, []string{"draft-1-1", "draft-2-1", "draft-3-1"}) {
		t.Fatalf("backend runs %v", runs)
	}
	ops := f.acknowledgedDraftOperations(t, stream)
	if len(ops) != 3 {
		t.Fatalf("operations: %+v", ops)
	}
	byID := map[string]trace.Transition{}
	for _, tr := range f.transitions(t, stream) {
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
		if !ok || tr.Subject != draftSubject || tr.From != want.from || tr.To != want.to || tr.Actor != draftingActor || !strings.Contains(tr.Reason, want.reason) {
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

	for turn, want := range map[string][]draftStatus{
		"draft-1-1": {{Delivered: plan.SpecPath, Verdict: draftInvalid, Problems: []string{"plan.json is not UTF-8 text"}}},
		"draft-2-1": {
			{Delivered: plan.SpecPath, Verdict: draftInvalid, Problems: []string{"plan.json was not delivered"}},
			{Delivered: plan.PlanPath, Verdict: draftInvalid, Problems: []string{"spec#2: no unit serves this criterion", `unit "dedupe": dependency cycle dedupe -> resume -> dedupe`}},
		},
		"draft-3-1": {
			{Delivered: plan.SpecPath, Verdict: draftInvalid, Problems: []string{"plan.json was not delivered"}},
			{Delivered: plan.PlanPath, Verdict: draftValid},
		},
	} {
		got := f.delivered(turn)
		for i := range got {
			if got[i].Next != draftNext[got[i].Verdict] {
				t.Fatalf("%s delivery %d next %q", turn, i, got[i].Next)
			}
		}
		if !slices.EqualFunc(got, want, func(a, b draftStatus) bool {
			return a.Delivered == b.Delivered && a.Verdict == b.Verdict && slices.Equal(a.Problems, b.Problems)
		}) {
			t.Fatalf("%s deliveries %+v, want %+v", turn, got, want)
		}
	}
	if tr := byID["sketched"]; tr.From != HandedState || tr.Cause != ops[2].Operation.ID || !strings.Contains(tr.Reason, "draft 3 passed validation: spec.md revision 3 with 2 acceptance criteria and plan.json revision 2 with 2 units") {
		t.Fatalf("sketched: %+v", tr)
	}
	if docs := f.documents(t, stream, plan.SpecDocument); len(docs) != 3 || docs[2].Content != validSpec {
		t.Fatalf("spec revisions: %+v", docs)
	}
	if docs := f.documents(t, stream, plan.PlanDocument); len(docs) != 2 || docs[1].Content != validPlan {
		t.Fatalf("plan revisions: %+v", docs)
	}
	th := f.architectThread(t, stream)
	if len(th.Turns) != 3 || !strings.Contains(th.Turns[1].Request.Prompt, "plan.json is not UTF-8 text") || !strings.Contains(th.Turns[2].Request.Prompt, "dependency cycle") {
		t.Fatalf("thread: %+v", th)
	}

	outbox, err := f.repository().Outbox(stream)
	must(t, err)
	for _, e := range outbox {
		if e.Event.Operation == nil && e.TransitionID != handInTransition && e.TransitionID != "sketched" {
			t.Fatalf("unexpected event: %+v", e)
		}
	}
}

func TestArchitectStopsAfterExhaustedDrafts(t *testing.T) {
	t.Parallel()
	f := newArchitectFixture(t)
	defer f.stop(t)
	ctx := context.Background()
	for n := 1; n <= maxDrafts+1; n++ {
		f.script(fmt.Sprintf("draft-%d-1", n), map[string]string{plan.SpecPath: validSpec, plan.PlanPath: cyclicPlan}, nil)
	}
	stream := f.handIn(t, "design", handedDesign)
	f.await(t, stream, draftAt("exhausted"))
	if runs := f.runs(); len(runs) != maxDrafts {
		t.Fatalf("backend runs %v", runs)
	}
	if feature, err := f.repository().Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != HandedState {
		t.Fatalf("feature state %+v %v", feature, err)
	}
	outbox, err := f.repository().Outbox(stream)
	must(t, err)
	var notices []trace.OutboxEntry
	for _, e := range outbox {
		if e.TransitionID == "draft-exhausted" {
			notices = append(notices, e)
		}
	}
	if len(notices) != 1 || !chiefEvent(notices[0].Event) || notices[0].Event.Operation != nil ||
		!strings.HasPrefix(notices[0].Event.Body, "None of the architect's 3 drafts of the spec and plan was accepted and drafting has stopped; the workstream stays handed. Last draft: draft 3 of the spec and plan is invalid:\n- spec#2: no unit serves this criterion\n- unit \"dedupe\": dependency cycle") {
		t.Fatalf("outbox: %+v", outbox)
	}
	var last trace.Transition
	for _, tr := range f.transitions(t, stream) {
		if tr.ID == "draft-exhausted" {
			last = tr
		}
	}
	if last.From != "invalid-3" || last.To != "exhausted" || last.Actor != draftingActor || last.Cause != "draft-3-invalid" || last.Reason != "none of the architect's 3 drafts of the spec and plan was accepted; drafting stops and the workstream stays handed" {
		t.Fatalf("exhausted transition: %+v", last)
	}

	d := &drafter{S: f.s.intake().DrafterDependencies(), Repository: f.repository()}
	must(t, d.Pass(ctx))
	if ops := f.draftOperations(t, stream); len(ops) != maxDrafts {
		t.Fatalf("operations: %d", len(ops))
	}
	if th := f.architectThread(t, stream); len(th.Turns) != maxDrafts {
		t.Fatalf("thread: %+v", th)
	}
	if docs := f.documents(t, stream, plan.PlanDocument); len(docs) != maxDrafts {
		t.Fatalf("every draft is kept: %+v", docs)
	}

	opts, clone, engine, sessions, clock := newArchitectOptions(t)
	runner := opts.Architect
	opts.Architect = nil
	f2 := &architectFixture{opts: opts, clone: clone, engine: engine, sessions: sessions, clock: clock}
	f2.start(t)
	added, err := f2.c.AddProject(ctx, request(clone))
	must(t, err)
	f2.project, f2.trace = added.Project.ID, added.Project.Trace
	writeCharter(t, f2.c, added.Project.ID, "1. Keep changes small.\n")
	stream = f2.handIn(t, "design", handedDesign)
	must(t, (&drafter{S: f2.s.intake().DrafterDependencies(), Repository: f2.repository()}).Pass(ctx))
	if state, err := f2.repository().Workflow(stream, draftSubject); err != nil || state.Value != "" {
		t.Fatalf("draft requested without a runner: %+v %v", state, err)
	}
	if ops := f2.draftOperations(t, stream); len(ops) != 0 {
		t.Fatalf("operations without a runner: %+v", ops)
	}
	if _, err := f2.repository().Thread(stream, architectAgent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("architect thread without a runner: %v", err)
	}
	f2.stop(t)
	f2.opts.Architect = runner
	f2.script("draft-1-1", map[string]string{plan.SpecPath: validSpec, plan.PlanPath: validPlan}, nil)
	f2.start(t)
	defer f2.stop(t)
	f2.await(t, stream, sketched)
	if runs := f2.runs(); !slices.Equal(runs, []string{"draft-1-1"}) {
		t.Fatalf("backend runs %v", runs)
	}
	ops := f2.draftOperations(t, stream)
	if len(ops) != 1 {
		t.Fatalf("operations: %+v", ops)
	}

	f3 := newArchitectFixture(t)
	defer f3.stop(t)
	f3.engine.Mu.Lock()
	for n := 1; n <= maxDrafts; n++ {
		for attempt := 1; attempt <= maxDraftAttempts; attempt++ {
			f3.engine.Turns[fmt.Sprintf("draft-%d-%d", n, attempt)] = func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) (*agent.Result, error) {
				return nil, errors.Join(context.Canceled, errors.New("connection dropped"))
			}
		}
	}
	f3.engine.Mu.Unlock()
	stream = f3.handIn(t, "design", handedDesign)
	f3.await(t, stream, draftAt("exhausted"))
	if runs := f3.runs(); len(runs) != maxDrafts*maxDraftAttempts {
		t.Fatalf("backend runs %v", runs)
	}
	ops = f3.acknowledgedDraftOperations(t, stream)
	if len(ops) != maxDrafts || ops[0].Result == nil || !strings.Contains(ops[0].Result.Evidence, fmt.Sprintf("draft 1 failed: the architect turn was interrupted %d times by service stops", maxDraftAttempts)) {
		t.Fatalf("operations: %+v", ops)
	}
}

func TestArchitectRedraftsAfterAFailedTurn(t *testing.T) {
	t.Parallel()
	f := newArchitectFixture(t)
	defer f.stop(t)
	f.engine.Mu.Lock()
	f.engine.Turns["draft-1-1"] = func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) (*agent.Result, error) {
		return nil, errors.New("backend crashed")
	}
	f.engine.Mu.Unlock()
	f.script("draft-2-1", map[string]string{plan.SpecPath: validSpec, plan.PlanPath: validPlan},
		func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
			if !strings.Contains(req.Prompt, "Draft 1 was not accepted:\ndraft 1 failed: architect turn draft-1-1 failed: ") {
				return fmt.Errorf("second prompt: %q", req.Prompt)
			}
			if _, err := readTool(ctx, tools, "draft/spec.md"); err == nil {
				return errors.New("the view holds a draft that was never recorded")
			}
			return nil
		})
	stream := f.handIn(t, "design", handedDesign)
	f.await(t, stream, sketched)

	if runs := f.runs(); !slices.Equal(runs, []string{"draft-1-1", "draft-1-1", "draft-1-1", "draft-2-1"}) {
		t.Fatalf("backend runs %v", runs)
	}
	ops := f.acknowledgedDraftOperations(t, stream)
	if len(ops) != 2 || ops[0].Result == nil || ops[0].Result.Outcome != "failed" ||
		!strings.HasPrefix(ops[0].Result.Evidence, "draft 1 failed: architect turn draft-1-1 failed: ") || !strings.Contains(ops[0].Result.Evidence, "backend crashed") {
		t.Fatalf("operations: %+v", ops)
	}
	reason := ops[0].Result.Evidence
	byID := map[string]trace.Transition{}
	for _, tr := range f.transitions(t, stream) {
		byID[tr.ID] = tr
	}
	if failed := byID["draft-1-failed"]; failed.From != "drafting-1" || failed.To != "failed-1" || failed.Reason != reason || failed.Actor != draftingActor || failed.Cause != ops[0].Operation.ID {
		t.Fatalf("failed transition: %+v", failed)
	}
	if next := byID["draft-2"]; next.From != "failed-1" || next.To != "drafting-2" || next.Cause != "draft-1-failed" {
		t.Fatalf("second request: %+v", next)
	}
	th := f.architectThread(t, stream)
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
	f := newArchitectFixture(t)
	ctx := context.Background()
	entered := make(chan struct{})
	var once sync.Once
	f.engine.Mu.Lock()
	f.engine.Turns["draft-1-1"] = func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return nil, ctx.Err()
	}
	f.engine.Mu.Unlock()
	stream := f.handIn(t, "design", handedDesign)
	select {
	case <-entered:
	case <-time.After(demoTimeout):
		t.Fatal("the draft did not start")
	}
	f.stop(t)

	retried := func(before int, turns int) int {
		t.Helper()
		deadline := time.Now().Add(demoTimeout)
		for {
			ops := f.draftOperations(t, stream)
			if len(ops) != 1 || ops[0].Result != nil {
				t.Fatalf("operations: %+v", ops)
			}
			n := 0
			for _, a := range ops[0].History {
				if a.Kind == "retry" && strings.Contains(a.Failure, "this service has no agent runner for the architect") {
					n++
				}
			}
			if n > before {
				if state, err := f.repository().Workflow(stream, draftSubject); err != nil || state.Value != "drafting-1" {
					t.Fatalf("draft state %+v %v", state, err)
				}
				if th := f.architectThread(t, stream); len(th.Turns) != turns {
					t.Fatalf("thread: %+v", th)
				}
				if runs := f.runs(); !slices.Equal(runs, []string{"draft-1-1"}) {
					t.Fatalf("backend runs %v", runs)
				}
				return n
			}
			if time.Now().After(deadline) {
				t.Fatalf("the draft was not retried: %+v", ops[0].History)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	runner := f.opts.Architect
	f.opts.Architect = nil
	f.start(t)
	count := retried(0, 1)
	f.stop(t)

	cfg, err := config.Load(f.opts.Config)
	must(t, err)
	repo, err := trace.Open(cfg.Root, config.Project{ID: f.project, Clone: f.clone})
	must(t, err)
	th, err := repo.Thread(stream, architectAgent)
	must(t, err)
	second := th.Turns[0].Request
	second.ID, second.TurnID, second.At = "request_draft-1-2", "draft-1-2", f.clock.Now()
	_, err = repo.EnqueueTurn(ctx, second)
	must(t, err)
	must(t, repo.Close())
	f.start(t)
	retried(count, 2)
	if th := f.architectThread(t, stream); th.Turns[1].Claim != nil {
		t.Fatalf("queued turn: %+v", th.Turns[1])
	}
	f.stop(t)

	f.opts.Architect = runner
	f.script("draft-1-2", map[string]string{plan.SpecPath: validSpec, plan.PlanPath: validPlan}, nil)
	f.start(t)
	defer f.stop(t)
	f.await(t, stream, sketched)
	if runs := f.runs(); !slices.Equal(runs, []string{"draft-1-1", "draft-1-2"}) {
		t.Fatalf("backend runs %v", runs)
	}
}

func TestArchitectDraftsBeforeTheScheduleHook(t *testing.T) {
	t.Parallel()
	opts, clone, engine, sessions, clock := newArchitectOptions(t)
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
			if feature.Value != HandedState {
				continue
			}
			draft, err := repo.Workflow(stream, draftSubject)
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
	f := &architectFixture{opts: opts, clone: clone, engine: engine, sessions: sessions, clock: clock}
	f.start(t)
	defer f.stop(t)
	added, err := f.c.AddProject(context.Background(), request(clone))
	must(t, err)
	f.project, f.trace = added.Project.ID, added.Project.Trace
	writeCharter(t, f.c, added.Project.ID, "1. Keep changes small.\n")
	repository.Store(f.repository())
	f.script("draft-1-1", map[string]string{plan.SpecPath: validSpec, plan.PlanPath: validPlan}, nil)
	stream := f.handIn(t, "design", handedDesign)
	f.await(t, stream, sketched)

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
			f := newArchitectFixture(t)
			ctx := context.Background()

			entered := make(chan struct{})
			var once sync.Once
			f.engine.Mu.Lock()
			f.engine.Turns["draft-1-1"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
				once.Do(func() { close(entered) })
				<-ctx.Done()
				return nil, ctx.Err()
			}
			f.engine.Mu.Unlock()
			stream := f.handIn(t, "design", handedDesign)
			select {
			case <-entered:
			case <-time.After(demoTimeout):
				t.Fatal("the draft did not start")
			}
			f.stop(t)

			cfg, err := config.Load(f.opts.Config)
			must(t, err)
			repo, err := trace.Open(cfg.Root, config.Project{ID: f.project, Clone: f.clone})
			must(t, err)
			th, err := repo.Thread(stream, architectAgent)
			must(t, err)
			if len(th.Turns) != 1 || th.Turns[0].Status() != "interrupted" || th.Active != "" {
				t.Fatalf("interrupted turn: %+v", th)
			}
			if state, err := repo.Workflow(stream, draftSubject); err != nil || state.Value != "drafting-1" {
				t.Fatalf("draft state %+v %v", state, err)
			}
			ops, err := repo.Operations(stream)
			must(t, err)
			operation := ""
			for _, o := range ops {
				if o.Operation.Action == DraftAction {
					operation = o.Operation.ID
				}
			}
			runs := []string{"draft-1-1"}
			if crash == "during-turn" {
				f.script("draft-1-2", map[string]string{plan.SpecPath: validSpec, plan.PlanPath: validPlan}, nil)
				runs = append(runs, "draft-1-2")
			} else {

				second := th.Turns[0].Request
				second.ID, second.TurnID, second.At = "request_draft-1-2", "draft-1-2", f.clock.Now()
				_, err = repo.EnqueueTurn(ctx, second)
				must(t, err)
				directory := filepath.Join(cfg.Root.String(), "architect", string(f.project), string(stream), "draft-1-2")
				claimed, err := repo.ClaimTurn(ctx, stream, architectAgent, "earlier-session", filepath.Join(directory, "session"), f.clock.Now())
				must(t, err)
				h := second.Header
				h.Schema, h.ID, h.At, h.Actor = "osmia.trace.turn-response", trace.EventID(second.ID, "response"), f.clock.Now(), trace.Actor{Kind: "service", ID: "thread-runner"}
				response := trace.TurnResponse{Header: h, AgentID: architectAgent, ThreadID: second.ThreadID, TurnID: second.TurnID, RequestID: second.ID, RequestRevision: second.Revision,
					Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: second.Profile.Backend, ID: "session-draft-1-2"}, SessionDirectory: claimed.Claim.SessionDirectory, StartedAt: claimed.Claim.At, Duration: time.Second, FinalResponse: "Draft delivered"}}
				must(t, repo.CaptureTurn(ctx, "earlier-session", response))
				if crash != "captured" {
					must(t, repo.CompleteTurn(ctx, stream, architectAgent, second.TurnID, "earlier-session", f.clock.Now()))
				}
				must(t, os.MkdirAll(filepath.Join(directory, "output"), 0700))
				must(t, os.WriteFile(filepath.Join(directory, "output", "spec.md"), []byte(validSpec), 0600))
				must(t, os.WriteFile(filepath.Join(directory, "output", "plan.json"), []byte(validPlan), 0600))
				if crash == "recorded" || crash == "moved" {
					at := f.clock.Now()
					header := func(id string) trace.Header {
						return trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: f.project, Workstream: stream, At: at, Actor: architectActor, Cause: operation, Depth: 1}
					}
					must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: header(plan.SpecDocument), Path: plan.SpecPath, Content: validSpec}, {Header: header(plan.PlanDocument), Path: plan.PlanPath, Content: validPlan}}))
				}
				if crash == "moved" {

					h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "abandoned", Revision: 1, Project: f.project, Workstream: stream, At: f.clock.Now(), Actor: ownerActor, Cause: "owner"}
					_, err := repo.SetFeatureState(ctx, h, "abandoned", "the owner abandoned the workstream")
					must(t, err)
				}
			}
			must(t, repo.Close())

			f.start(t)
			defer f.stop(t)
			if crash == "moved" {
				f.await(t, stream, draftAt("failed-1"))
				ops := f.acknowledgedDraftOperations(t, stream)
				if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "failed" || !strings.Contains(ops[0].Result.Evidence, "the workstream is abandoned, not handed, so the valid draft was recorded and not presented") {
					t.Fatalf("operations: %+v", ops)
				}
				if state, err := f.repository().Workflow(stream, trace.FeatureSubject); err != nil || state.Value != "abandoned" {
					t.Fatalf("feature state %+v %v", state, err)
				}
				must(t, (&drafter{S: f.s.intake().DrafterDependencies(), Repository: f.repository()}).Pass(ctx))
				if ops := f.draftOperations(t, stream); len(ops) != 1 {
					t.Fatalf("another draft was requested: %+v", ops)
				}
				if docs := f.documents(t, stream, plan.SpecDocument); len(docs) != 1 {
					t.Fatalf("spec revisions: %+v", docs)
				}
				return
			}
			f.await(t, stream, sketched)
			if got := f.runs(); !slices.Equal(got, runs) {
				t.Fatalf("backend runs %v, want %v", got, runs)
			}
			th = f.architectThread(t, stream)
			if len(th.Turns) != 2 || th.Turns[0].Status() != "interrupted" || th.Turns[0].Response.Failure == "" || th.Turns[1].Status() != "idle" || th.Active != "" {
				t.Fatalf("thread after recovery: %+v", th)
			}

			ops = f.acknowledgedDraftOperations(t, stream)
			kinds := map[string]int{}
			for _, a := range ops[0].History {
				kinds[a.Kind]++
			}
			if len(ops) != 1 || !ops[0].Acknowledged || kinds["result"] != 1 || kinds["claim"] < 2 || ops[0].Result.Outcome != "succeeded" {
				t.Fatalf("operation history: %+v", kinds)
			}
			for _, id := range []string{plan.SpecDocument, plan.PlanDocument} {
				if docs := f.documents(t, stream, id); len(docs) != 1 || docs[0].Cause != operation {
					t.Fatalf("%s revisions: %+v", id, docs)
				}
			}
			moves := 0
			for _, tr := range f.transitions(t, stream) {
				if tr.To == SketchedState {
					moves++
				}
			}
			if moves != 1 {
				t.Fatalf("sketched transitions: %d", moves)
			}
			if state, err := f.repository().Workflow(stream, trace.FeatureSubject); err != nil || state != (trace.WorkflowState{Version: 2, Value: SketchedState}) {
				t.Fatalf("feature state %+v %v", state, err)
			}
		})
	}
}

func TestAbandonStopsTheArchitectDraft(t *testing.T) {
	t.Parallel()
	const evidence = "draft 1 failed: the workstream was abandoned, so the architect runs no turn for it"

	blocked := func(f *architectFixture) chan struct{} {
		entered := make(chan struct{})
		var once sync.Once
		f.engine.Mu.Lock()
		f.engine.Turns["draft-1-1"] = func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
			once.Do(func() { close(entered) })
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(30 * time.Second):
				return nil, errors.New("the turn was not cancelled")
			}
		}
		f.engine.Mu.Unlock()
		return entered
	}
	check := func(t *testing.T, f *architectFixture, stream config.WorkstreamID) {
		t.Helper()
		f.await(t, stream, draftAt("failed-1"))
		ops := f.acknowledgedDraftOperations(t, stream)
		if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "failed" || ops[0].Result.Evidence != evidence {
			t.Fatalf("operations: %+v", ops)
		}
		if got := f.runs(); !slices.Equal(got, []string{"draft-1-1"}) {
			t.Fatalf("backend runs %v", got)
		}
		th := f.architectThread(t, stream)
		if len(th.Turns) != 1 || th.Turns[0].Status() != "interrupted" || !th.Turns[0].Response.Result.Cancelled || th.Active != "" {
			t.Fatalf("thread: %+v", th)
		}
		if state, err := f.repository().Workflow(stream, trace.FeatureSubject); err != nil || state.Value != AbandonedState {
			t.Fatalf("feature state %+v %v", state, err)
		}
		must(t, (&drafter{S: f.s.intake().DrafterDependencies(), Repository: f.repository()}).Pass(context.Background()))
		if ops := f.draftOperations(t, stream); len(ops) != 1 {
			t.Fatalf("another draft was requested: %+v", ops)
		}
		if docs := f.documents(t, stream, plan.SpecDocument); len(docs) != 0 {
			t.Fatalf("spec revisions: %+v", docs)
		}
	}

	t.Run("running turn", func(t *testing.T) {
		f := newArchitectFixture(t)
		defer f.stop(t)
		entered := blocked(f)
		stream := f.handIn(t, "design", handedDesign)
		select {
		case <-entered:
		case <-time.After(demoTimeout):
			t.Fatal("the draft did not start")
		}
		if code, body := abandonCall(t, f.s, stream, `{"reason":"Superseded"}`); code != http.StatusOK {
			t.Fatalf("abandon: %d %s", code, body)
		}
		check(t, f, stream)
	})

	t.Run("after a restart", func(t *testing.T) {
		f := newArchitectFixture(t)
		entered := blocked(f)
		stream := f.handIn(t, "design", handedDesign)
		select {
		case <-entered:
		case <-time.After(demoTimeout):
			t.Fatal("the draft did not start")
		}
		f.stop(t)

		cfg, err := config.Load(f.opts.Config)
		must(t, err)
		repo, err := trace.Open(cfg.Root, config.Project{ID: f.project, Clone: f.clone})
		must(t, err)
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: abandonTransition, Revision: 1, Project: f.project, Workstream: stream, At: f.clock.Now(), Actor: ownerActor, Cause: abandonTransition}
		_, err = repo.SetFeatureStateUnless(context.Background(), h, AbandonedState, "Superseded", AbandonedState, DeliveredState)
		must(t, err)
		must(t, repo.Close())
		f.start(t)
		defer f.stop(t)
		check(t, f, stream)
	})

	t.Run("queued turn", func(t *testing.T) {
		f := newArchitectFixture(t)
		entered := blocked(f)
		stream := f.handIn(t, "design", handedDesign)
		select {
		case <-entered:
		case <-time.After(demoTimeout):
			t.Fatal("the draft did not start")
		}
		f.stop(t)

		ctx := context.Background()
		cfg, err := config.Load(f.opts.Config)
		must(t, err)
		repo, err := trace.Open(cfg.Root, config.Project{ID: f.project, Clone: f.clone})
		must(t, err)
		defer repo.Close()
		th, err := repo.Thread(stream, architectAgent)
		must(t, err)
		second := th.Turns[0].Request
		second.ID, second.TurnID, second.At = "request_draft-1-2", "draft-1-2", f.clock.Now()
		_, err = repo.EnqueueTurn(ctx, second)
		must(t, err)
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: abandonTransition, Revision: 1, Project: f.project, Workstream: stream, At: f.clock.Now(), Actor: ownerActor, Cause: abandonTransition}
		_, err = repo.SetFeatureStateUnless(ctx, h, AbandonedState, "Superseded", AbandonedState, DeliveredState)
		must(t, err)
		ops, err := repo.Operations(stream)
		must(t, err)
		var op coreadapter.Operation
		for _, o := range ops {
			if o.Operation.Action == DraftAction {
				op = o.Operation
			}
		}
		result, err := (&drafter{S: f.s.intake().DrafterDependencies(), Repository: repo}).Apply(ctx, op)
		if err != nil || result.Outcome != "failed" || result.Evidence != evidence {
			t.Fatalf("result %+v: %v", result, err)
		}
		if got := f.runs(); !slices.Equal(got, []string{"draft-1-1"}) {
			t.Fatalf("backend runs %v", got)
		}
		th, err = repo.Thread(stream, architectAgent)
		must(t, err)
		if len(th.Turns) != 2 || th.Turns[1].Response == nil || !th.Turns[1].Response.Result.Cancelled || th.Turns[1].Response.Actor != abandonActor || th.Turns[1].Response.Failure != cancelReason {
			t.Fatalf("thread: %+v", th)
		}
	})
}

func TestSchedulerLeavesArchitectTurnsToTheDrafter(t *testing.T) {
	t.Parallel()
	f := newArchitectFixture(t)
	defer f.stop(t)
	ctx := context.Background()
	const created = config.WorkstreamID("w_00000000000000000000000000000001")
	must(t, f.repository().CreateWorkstream(ctx, created, f.clock.Now(), ownerActor))
	holds := f.s.dispatchHolds(f.s.current(), f.repository())
	architect := scheduler.Candidate{Workstream: created, Thread: trace.Thread{Identity: trace.Agent{Role: architectRole}}}
	if held, err := holds(architect); err != nil || !held {
		t.Fatalf("architect turn not held: %t %v", held, err)
	}
	other := scheduler.Candidate{Workstream: created, Thread: trace.Thread{Identity: trace.Agent{Role: "mason"}}}
	if held, err := holds(other); err != nil || held {
		t.Fatalf("mason turn held: %t %v", held, err)
	}
	f.stop(t)

	opts, clone, engine, sessions, clock := newArchitectOptions(t)
	opts.Threads = func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
		return &completedRunner{}, nil
	}
	f2 := &architectFixture{opts: opts, clone: clone, engine: engine, sessions: sessions, clock: clock}
	f2.start(t)
	defer f2.stop(t)
	added, err := f2.c.AddProject(ctx, request(clone))
	must(t, err)
	f2.project, f2.trace = added.Project.ID, added.Project.Trace
	writeCharter(t, f2.c, added.Project.ID, "1. Keep changes small.\n")
	f2.script("draft-1-1", map[string]string{plan.SpecPath: validSpec, plan.PlanPath: validPlan}, nil)
	stream := f2.handIn(t, "design", handedDesign)
	f2.await(t, stream, sketched)
	if runs := f2.runs(); !slices.Equal(runs, []string{"draft-1-1"}) {
		t.Fatalf("backend runs %v", runs)
	}
	ops, err := f2.repository().Operations(stream)
	must(t, err)
	for _, o := range ops {
		if o.Operation.Action != DraftAction {
			var in struct{ Agent string }
			must(t, json.Unmarshal(o.Operation.Input, &in))
			if in.Agent == architectAgent {
				t.Fatalf("the scheduler published %s for the architect's turn", o.Operation.Action)
			}
		}
	}
}

func (f *architectFixture) scenario() *intakefixture.ArchitectScenario {
	f.scenarioOnce.Do(func() {
		f.scenarioState = &intakefixture.ArchitectScenario{Client: func() intakefixture.WorkflowClient { return f.c }, Engine: f.engine, Repository: f.repository, Changes: func(t *testing.T) (func(), func()) { return serviceChanges(t, f.s) }}
	})
	return f.scenarioState
}
