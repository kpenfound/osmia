package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	handedDesign = "# Resumable uploads\n\nUploads that drop resume where they stopped.\n"
	validSpec    = "# Resumable uploads\n\nAn interrupted upload resumes from its last acknowledged chunk. It must not re-send acknowledged chunks.\n\n## Acceptance criteria\n\n1. An interrupted upload resumes from the last acknowledged chunk.\n2. Acknowledged chunks are never sent again.\n"
	validPlan    = `{"version": 1, "units": [
  {"id": "resume", "title": "Resume from the last chunk", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestResume"}}], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "dedupe", "title": "Skip acknowledged chunks", "addresses": [{"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "no chunk is sent twice"}}], "depends_on": ["resume"], "footprint": ["internal.trace"]}
]}
`
	// cyclicPlan has a dependency cycle and leaves criterion 2 unaddressed.
	cyclicPlan = `{"version": 1, "units": [
  {"id": "resume", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestResume"}}], "depends_on": ["dedupe"], "footprint": ["internal.trace"]},
  {"id": "dedupe", "addresses": [], "depends_on": ["resume"], "footprint": ["internal.trace"]}
]}
`
)

// architectFixture is an active project with a ruled charter, a clone whose
// tracked files seed the entity map, and the fake engine the architect's
// turns run in.
type architectFixture struct {
	opts     Options
	s        *Service
	c        *Client
	project  config.ProjectID
	trace    string
	clone    string
	engine   *demoEngine
	sessions *demoSessions
	clock    *demoClock
}

// architectContainer runs the architect in a container, the only sandbox
// the core executor accepts.
const architectContainer = `[roles.architect]
sandbox = "container"
image = "fixture-image"
`

func newArchitectOptions(t *testing.T) (Options, string, *demoEngine, *demoSessions, *demoClock) {
	t.Helper()
	opts, clone := projectFixture(t)
	home := filepath.Dir(clone)
	configFile, err := os.OpenFile(filepath.Join(opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = configFile.WriteString(architectContainer)
	must(t, errors.Join(err, configFile.Close()))
	must(t, os.MkdirAll(filepath.Join(clone, "internal", "trace"), 0700))
	must(t, os.WriteFile(filepath.Join(clone, "internal", "trace", "git.go"), []byte("package trace\n"), 0600))
	must(t, os.WriteFile(filepath.Join(clone, "CODEOWNERS"), []byte("/internal/ @core\n"), 0600))
	must(t, os.WriteFile(filepath.Join(clone, "secret.env"), []byte("TOKEN="+demoSecret+"\n"), 0600))
	demoGit(t, home, "-C", clone, "add", "internal", "CODEOWNERS")
	demoGit(t, home, "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	sessions := &demoSessions{byKey: map[string]*mcp.ClientSession{}}
	engine := &demoEngine{sessions: sessions, turns: map[string]demoTurn{}}
	engine.resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error {
		return coreadapter.ErrResumeUnavailable
	}
	clock := &demoClock{now: demoStart}
	opts.Reconciliation.Now = clock.Now
	opts.Issues = &fakeIssues{token: "ghp_architect_secret"}
	opts.Architect = &Architect{Engine: engine, Hosts: &coreadapter.MCPHost{Transport: &demoTransport{sessions: sessions}}}
	return opts, clone, engine, sessions, clock
}

func newArchitectFixture(t *testing.T) *architectFixture {
	t.Helper()
	opts, clone, engine, sessions, clock := newArchitectOptions(t)
	f := &architectFixture{opts: opts, clone: clone, engine: engine, sessions: sessions, clock: clock}
	f.start(t)
	added, err := f.c.AddProject(context.Background(), request(clone))
	must(t, err)
	f.project, f.trace = added.Project.ID, added.Project.Trace
	must(t, os.WriteFile(added.Project.Charter, []byte("1. Keep changes small.\n2. Every change has a test.\n"), 0600))
	return f
}

func (f *architectFixture) start(t *testing.T) {
	t.Helper()
	s, err := Start(context.Background(), f.opts)
	must(t, err)
	f.s, f.c = s, NewClient(s.Socket())
}

func (f *architectFixture) stop(t *testing.T) {
	t.Helper()
	f.c.Close()
	must(t, f.s.Close())
}

func (f *architectFixture) handIn(t *testing.T, key, content string) config.WorkstreamID {
	t.Helper()
	out, err := f.c.HandIn(context.Background(), HandInRequest{Project: f.project, Key: key, Stdin: &content})
	must(t, err)
	return out.Workstream
}

func (f *architectFixture) repository() *trace.Repository { return f.s.active.repository }

// script installs the fake architect's behaviour for one turn: it delivers
// the given files and returns a successful result.
func (f *architectFixture) script(turn string, files map[string]string, check func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error) {
	f.engine.mu.Lock()
	defer f.engine.mu.Unlock()
	f.engine.turns[turn] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		var err error
		if check != nil {
			err = check(ctx, req, verified, tools)
		}
		for _, path := range []string{plan.SpecPath, plan.PlanPath} {
			content, ok := files[path]
			if !ok {
				continue
			}
			if _, e := callTool(ctx, tools, DraftTool, map[string]any{"path": path, "content": content}); e != nil {
				err = errors.Join(err, fmt.Errorf("deliver %s: %w", path, e))
			}
		}
		if err != nil {
			return nil, err
		}
		return &agent.Result{ClaudeID: "session-" + turn, ResultText: "Draft delivered", SessionDir: req.SessionDir, NumTurns: 2}, nil
	}
}

func (f *architectFixture) runs() []string {
	f.engine.mu.Lock()
	defer f.engine.mu.Unlock()
	return slices.Clone(f.engine.runs)
}

// await polls the workstream's feature and draft states until want accepts
// them.
func (f *architectFixture) await(t *testing.T, stream config.WorkstreamID, want func(feature, draft string) bool) {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for {
		feature, err := f.repository().Workflow(stream, trace.FeatureSubject)
		must(t, err)
		draft, err := f.repository().Workflow(stream, draftSubject)
		must(t, err)
		if want(feature.Value, draft.Value) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("workstream %s stayed %q with draft %q", stream, feature.Value, draft.Value)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func sketched(feature, _ string) bool { return feature == SketchedState }
func draftAt(value string) func(string, string) bool {
	return func(_, draft string) bool { return draft == value }
}

func (f *architectFixture) documents(t *testing.T, stream config.WorkstreamID, id string) []trace.Document {
	t.Helper()
	docs, err := trace.Read[trace.Document](f.repository(), stream)
	must(t, err)
	var out []trace.Document
	for _, d := range docs {
		if d.ID == id {
			out = append(out, d)
		}
	}
	return out
}

func (f *architectFixture) transitions(t *testing.T, stream config.WorkstreamID) []trace.Transition {
	t.Helper()
	transitions, err := trace.Read[trace.Transition](f.repository(), stream)
	must(t, err)
	return transitions
}

func (f *architectFixture) draftOperations(t *testing.T, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	ops, err := f.repository().Operations(stream)
	must(t, err)
	var out []trace.OperationRecord
	for _, o := range ops {
		if o.Operation.Action == DraftAction {
			out = append(out, o)
		}
	}
	slices.SortFunc(out, func(a, b trace.OperationRecord) int { return a.Transition.At.Compare(b.Transition.At) })
	return out
}

func (f *architectFixture) architectThread(t *testing.T, stream config.WorkstreamID) trace.Thread {
	t.Helper()
	th, err := f.repository().Thread(stream, architectAgent)
	must(t, err)
	return th
}

// readTool reads a file through the turn's file_read tool.
func readTool(ctx context.Context, tools *mcp.ClientSession, path string) (string, error) {
	got, err := callTool(ctx, tools, "file_read", map[string]any{"path": path})
	if err != nil {
		return "", err
	}
	var text string
	err = json.Unmarshal([]byte(got), &text)
	return text, err
}

// checkReadOnlyGrants asserts the grants core verified for a read-only turn:
// the private view read-only, the session read-only with the scratch
// directory the turn starts in, the scoped MCP server as the only tool, and
// no VCS or host environment.
func checkReadOnlyGrants(req agent.Request, verified *agent.Turn, clone string, fail func(string, ...any)) {
	g := req.Grants
	if g == nil || len(g.Mounts) != 3 {
		fail("grants %+v are not a read-only view", g)
		return
	}
	view, scratch := g.Mounts[0].Path, filepath.Join(req.SessionDir, "work")
	if g.Mounts[0].Access != agent.ReadOnly || g.Mounts[1] != (agent.Mount{Path: req.SessionDir, Access: agent.ReadOnly}) || g.Mounts[2] != (agent.Mount{Path: scratch, Access: agent.ReadWrite}) ||
		req.Workspace == nil || req.Workspace.Directory() != scratch || view == clone || strings.HasPrefix(view, clone+string(filepath.Separator)) {
		fail("grants %+v expose more than the read-only private view", g)
	}
	if g.VCS || verified.VCS || !slices.Equal(verified.DeniedExecutables, agent.VCSExecutables) {
		fail("VCS is not denied: %+v", verified)
	}
	if !slices.Equal(g.Tools, []string{"mcp__osmia_0"}) || verified.Tools == nil || len(verified.Tools) != 0 {
		fail("tools %v %v exceed the scoped MCP server", g.Tools, verified.Tools)
	}
	for _, kv := range verified.Env {
		key, value, _ := strings.Cut(kv, "=")
		if key != "HOME" && req.Env[key] != value {
			fail("verified environment exposes %s", key)
		}
	}
}

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
	if want := []string{DraftTool, "file_read"}; !slices.Equal(names, want) {
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
	for _, want := range []string{"# Project context", "context mode: file", "- charter#1: Keep changes small.", "- charter#2: Every change has a test.", "## Entities", "internal.trace"} {
		if !strings.Contains(context, want) {
			fail("context.md lacks %q:\n%s", want, context)
		}
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
	for _, want := range []string{"handed/stdin", "charter.md", "context.md", "intended behaviour", "must not do", `"## Acceptance criteria"`, "numbered list", "spec#<n>", "footprint", "no cycle", "named proof", "How finely the work is cut into units is your call", DraftTool} {
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
	head := demoGit(t, filepath.Dir(f.clone), "-C", f.trace, "rev-parse", "HEAD")
	stream := f.handIn(t, "design", handedDesign)
	f.await(t, stream, sketched)
	if runs := f.runs(); !slices.Equal(runs, []string{"draft-1-1"}) {
		t.Fatalf("backend runs %v", runs)
	}
	// The draft is recorded as architect-authored revisions caused by the
	// draft operation, in one commit, and the files are on disk.
	ops := f.draftOperations(t, stream)
	if len(ops) != 1 || !ops[0].Acknowledged || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" || !strings.Contains(ops[0].Result.Evidence, "draft 1 passed validation") {
		t.Fatalf("operations: %+v", ops)
	}
	operation := ops[0].Operation.ID
	for id, want := range map[string]struct{ path, content string }{plan.SpecDocument: {plan.SpecPath, validSpec}, plan.PlanDocument: {plan.PlanPath, validPlan}} {
		docs := f.documents(t, stream, id)
		if len(docs) != 1 || docs[0].Revision != 1 || docs[0].Path != want.path || docs[0].Content != want.content || docs[0].Actor != architectActor || docs[0].Cause != operation || docs[0].Source != "" {
			t.Fatalf("%s revisions: %+v", id, docs)
		}
		data, err := os.ReadFile(filepath.Join(f.trace, "workstreams", string(stream), want.path))
		if err != nil || string(data) != want.content {
			t.Fatalf("%s on disk: %q %v", want.path, data, err)
		}
	}
	streamDir := "workstreams/" + string(stream) + "/"
	spec := demoGit(t, filepath.Dir(f.clone), "-C", f.trace, "log", "-1", "--format=%H", "--", streamDir+"spec.md")
	if other := demoGit(t, filepath.Dir(f.clone), "-C", f.trace, "log", "-1", "--format=%H", "--", streamDir+"plan.json"); other != spec {
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
	events, err := os.ReadFile(filepath.Join(f.trace, streamDir, "events.jsonl"))
	must(t, err)
	if !strings.Contains(string(events), `"from":"handed","to":"sketched"`) {
		t.Fatalf("events.jsonl:\n%s", events)
	}
	outbox, err := f.repository().Outbox(stream)
	must(t, err)
	var notices []trace.OutboxEntry
	for _, e := range outbox {
		if e.Event.Kind == trace.NoticeKind && e.TransitionID == "sketched" {
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
	// Exactly one architect turn was queued, and the thread continues.
	th := f.architectThread(t, stream)
	if len(th.Turns) != 1 || th.Turns[0].Request.TurnID != "draft-1-1" || th.Turns[0].Status() != "idle" || th.Identity.Role != architectRole || th.Active != "" {
		t.Fatalf("architect thread: %+v", th)
	}
	if _, err := os.Stat(filepath.Join(f.trace, "notes", "architect.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("architect notes: %v", err)
	}
	// Another pass, and applying the operation again as a retry after a stop
	// would, record nothing more; a workstream in another feature state is
	// not drafted.
	other := config.WorkstreamID("w_0123456789abcdef0123456789abcdef")
	must(t, f.repository().CreateWorkstream(ctx, other, f.clock.Now(), ownerActor))
	_, err = f.repository().SetFeatureState(ctx, trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "ratified", Revision: 1, Project: f.project, Workstream: other, At: f.clock.Now(), Actor: ownerActor, Cause: "owner"}, "ratified", "the owner ratified the plan")
	must(t, err)
	d := &drafter{s: f.s, repository: f.repository()}
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
	if len(f.transitions(t, stream)) != 3 {
		t.Fatalf("transitions: %+v", f.transitions(t, stream))
	}
	// The turn left nothing behind but its session and delivered files, and
	// the trace's commits since hand-in are the workstream's own.
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
	if strings.TrimSpace(head) == strings.TrimSpace(demoGit(t, filepath.Dir(f.clone), "-C", f.trace, "rev-parse", "HEAD")) {
		t.Fatal("the trace did not move")
	}
}

func TestArchitectResubmitsAnInvalidDraft(t *testing.T) {
	f := newArchitectFixture(t)
	defer f.stop(t)
	// Draft 1 delivers no plan through the tool but leaves bytes that are not
	// UTF-8 text where the service reads the delivery; draft 2 a plan with a
	// cycle and an unaddressed criterion; draft 3 a valid plan.
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
			var problems []error
			for _, want := range []string{"Draft 2 was not accepted:", `unit "dedupe": dependency cycle dedupe -> resume -> dedupe`, "spec#2: no unit addresses this criterion"} {
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
	f.await(t, stream, draftAt("invalid-1"))
	// The invalid draft is recorded with its problems and the state stays
	// handed; the chief of staff hears nothing yet.
	if docs := f.documents(t, stream, plan.SpecDocument); len(docs) != 1 || docs[0].Content != validSpec || docs[0].Actor != architectActor {
		t.Fatalf("spec after draft 1: %+v", docs)
	}
	if docs := f.documents(t, stream, plan.PlanDocument); len(docs) != 0 {
		t.Fatalf("plan after draft 1: %+v", docs)
	}
	f.await(t, stream, draftAt("invalid-2"))
	if docs := f.documents(t, stream, plan.PlanDocument); len(docs) != 1 || docs[0].Content != cyclicPlan || docs[0].Revision != 1 {
		t.Fatalf("plan after draft 2: %+v", docs)
	}
	if feature, err := f.repository().Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != HandedState {
		t.Fatalf("feature state %+v %v", feature, err)
	}
	f.await(t, stream, sketched)
	if runs := f.runs(); !slices.Equal(runs, []string{"draft-1-1", "draft-2-1", "draft-3-1"}) {
		t.Fatalf("backend runs %v", runs)
	}
	ops := f.draftOperations(t, stream)
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
		{"draft-2-invalid", "drafting-2", "invalid-2", "draft 2 of the spec and plan is invalid:\n- spec#2: no unit addresses this criterion\n- unit \"dedupe\": dependency cycle dedupe -> resume -> dedupe"},
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
	// The chief of staff was told nothing about the invalid drafts.
	outbox, err := f.repository().Outbox(stream)
	must(t, err)
	for _, e := range outbox {
		if e.Event.Operation == nil && e.TransitionID != handInTransition && e.TransitionID != "sketched" {
			t.Fatalf("unexpected event: %+v", e)
		}
	}
}

func TestArchitectStopsAfterExhaustedDrafts(t *testing.T) {
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
	if len(notices) != 1 || notices[0].Event.Kind != trace.NoticeKind || notices[0].Event.Operation != nil ||
		!strings.HasPrefix(notices[0].Event.Body, "None of the architect's 3 drafts of the spec and plan was accepted and drafting has stopped; the workstream stays handed. Last draft: draft 3 of the spec and plan is invalid:\n- spec#2: no unit addresses this criterion\n- unit \"dedupe\": dependency cycle") {
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
	// Nothing more is requested, however many passes run.
	d := &drafter{s: f.s, repository: f.repository()}
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

	// A service without an architect runner requests nothing: the workstream
	// waits, handed, and a service with a runner drafts it.
	opts, clone, engine, sessions, clock := newArchitectOptions(t)
	runner := opts.Architect
	opts.Architect = nil
	f2 := &architectFixture{opts: opts, clone: clone, engine: engine, sessions: sessions, clock: clock}
	f2.start(t)
	added, err := f2.c.AddProject(ctx, request(clone))
	must(t, err)
	f2.project, f2.trace = added.Project.ID, added.Project.Trace
	must(t, os.WriteFile(added.Project.Charter, []byte("1. Keep changes small.\n"), 0600))
	stream = f2.handIn(t, "design", handedDesign)
	must(t, (&drafter{s: f2.s, repository: f2.repository()}).Pass(ctx))
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

	// A draft whose every turn is interrupted fails with the count, and the
	// drafts are exhausted the same way.
	f3 := newArchitectFixture(t)
	defer f3.stop(t)
	f3.engine.mu.Lock()
	for n := 1; n <= maxDrafts; n++ {
		for attempt := 1; attempt <= maxDraftAttempts; attempt++ {
			f3.engine.turns[fmt.Sprintf("draft-%d-%d", n, attempt)] = func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) (*agent.Result, error) {
				return nil, errors.Join(context.Canceled, errors.New("connection dropped"))
			}
		}
	}
	f3.engine.mu.Unlock()
	stream = f3.handIn(t, "design", handedDesign)
	f3.await(t, stream, draftAt("exhausted"))
	if runs := f3.runs(); len(runs) != maxDrafts*maxDraftAttempts {
		t.Fatalf("backend runs %v", runs)
	}
	ops = f3.draftOperations(t, stream)
	if len(ops) != maxDrafts || ops[0].Result == nil || !strings.Contains(ops[0].Result.Evidence, fmt.Sprintf("draft 1 failed: the architect turn was interrupted %d times by service stops", maxDraftAttempts)) {
		t.Fatalf("operations: %+v", ops)
	}
}

func TestArchitectRedraftsAfterAFailedTurn(t *testing.T) {
	f := newArchitectFixture(t)
	defer f.stop(t)
	f.engine.mu.Lock()
	f.engine.turns["draft-1-1"] = func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) (*agent.Result, error) {
		return nil, errors.New("backend crashed")
	}
	f.engine.mu.Unlock()
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
	if runs := f.runs(); !slices.Equal(runs, []string{"draft-1-1", "draft-2-1"}) {
		t.Fatalf("backend runs %v", runs)
	}
	ops := f.draftOperations(t, stream)
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
	f := newArchitectFixture(t)
	ctx := context.Background()
	entered := make(chan struct{})
	var once sync.Once
	f.engine.mu.Lock()
	f.engine.turns["draft-1-1"] = func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return nil, ctx.Err()
	}
	f.engine.mu.Unlock()
	stream := f.handIn(t, "design", handedDesign)
	select {
	case <-entered:
	case <-time.After(demoTimeout):
		t.Fatal("the draft did not start")
	}
	f.stop(t)

	// retried waits until the pending draft was retried more than before for
	// the missing runner, and checks that nothing else happened to it.
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

	// A service without a runner starts no second attempt: the draft stays
	// pending and unspent.
	runner := f.opts.Architect
	f.opts.Architect = nil
	f.start(t)
	count := retried(0, 1)
	f.stop(t)

	// Nor does it run an attempt an earlier service accepted and did not
	// reserve.
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

	// A service with a runner then drafts the workstream.
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
	opts, clone, engine, sessions, clock := newArchitectOptions(t)
	var repository atomic.Pointer[trace.Repository]
	var mu sync.Mutex
	var seen []string
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
			seen = append(seen, draft.Value)
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
	must(t, os.WriteFile(added.Project.Charter, []byte("1. Keep changes small.\n"), 0600))
	repository.Store(f.repository())
	f.script("draft-1-1", map[string]string{plan.SpecPath: validSpec, plan.PlanPath: validPlan}, nil)
	stream := f.handIn(t, "design", handedDesign)
	f.await(t, stream, sketched)
	// The embedder's hook ran, and whenever it saw the handed workstream the
	// drafter had already requested its draft in the same pass.
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 || slices.ContainsFunc(seen, func(v string) bool { return v != "drafting-1" }) {
		t.Fatalf("draft states the Schedule hook saw: %q", seen)
	}
}

func TestArchitectDraftSurvivesRestart(t *testing.T) {
	for _, crash := range []string{"during-turn", "captured", "completed", "recorded", "moved"} {
		t.Run(crash, func(t *testing.T) {
			f := newArchitectFixture(t)
			ctx := context.Background()
			// The first attempt is stopped mid-turn: the backend returns on
			// cancellation.
			entered := make(chan struct{})
			var once sync.Once
			f.engine.mu.Lock()
			f.engine.turns["draft-1-1"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
				once.Do(func() { close(entered) })
				<-ctx.Done()
				return nil, ctx.Err()
			}
			f.engine.mu.Unlock()
			stream := f.handIn(t, "design", handedDesign)
			select {
			case <-entered:
			case <-time.After(demoTimeout):
				t.Fatal("the draft did not start")
			}
			f.stop(t)

			// The stopped service left the turn interrupted and the operation
			// claimed.
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
				// The stopped service ran a second attempt to the end: the
				// draft was delivered and captured, completed, or already
				// recorded as revisions, but the workstream was not moved.
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
					// The workstream left handed before the draft was presented.
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
				ops := f.draftOperations(t, stream)
				if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "failed" || !strings.Contains(ops[0].Result.Evidence, "the workstream is abandoned, not handed, so the valid draft was recorded and not presented") {
					t.Fatalf("operations: %+v", ops)
				}
				if state, err := f.repository().Workflow(stream, trace.FeatureSubject); err != nil || state.Value != "abandoned" {
					t.Fatalf("feature state %+v %v", state, err)
				}
				must(t, (&drafter{s: f.s, repository: f.repository()}).Pass(ctx))
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
			// One result, one set of revisions, one transition: the retries
			// produced no duplicates.
			ops = f.draftOperations(t, stream)
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
	const evidence = "draft 1 failed: the workstream was abandoned, so the architect runs no turn for it"
	// blocked scripts the first turn to run until its context is cancelled.
	blocked := func(f *architectFixture) chan struct{} {
		entered := make(chan struct{})
		var once sync.Once
		f.engine.mu.Lock()
		f.engine.turns["draft-1-1"] = func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
			once.Do(func() { close(entered) })
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(30 * time.Second):
				return nil, errors.New("the turn was not cancelled")
			}
		}
		f.engine.mu.Unlock()
		return entered
	}
	check := func(t *testing.T, f *architectFixture, stream config.WorkstreamID) {
		t.Helper()
		f.await(t, stream, draftAt("failed-1"))
		ops := f.draftOperations(t, stream)
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
		must(t, (&drafter{s: f.s, repository: f.repository()}).Pass(context.Background()))
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
		// The owner's abandonment was recorded and the service stopped
		// before it cancelled the interrupted turn.
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
		// The stop interrupted the first attempt. The second was accepted and
		// not reserved when the workstream was abandoned, and nothing
		// cancelled it.
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
		result, err := (&drafter{s: f.s, repository: repo}).Apply(ctx, op)
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
	f := newArchitectFixture(t)
	defer f.stop(t)
	ctx := context.Background()
	const created = config.WorkstreamID("w_00000000000000000000000000000001")
	must(t, f.repository().CreateWorkstream(ctx, created, f.clock.Now(), ownerActor))
	gate := f.s.admit(f.project, f.repository())
	architect := scheduler.Candidate{Workstream: created, Thread: trace.Thread{Identity: trace.Agent{Role: architectRole}}}
	if admitted, err := gate(ctx, architect); err != nil || admitted {
		t.Fatalf("architect turn admitted: %t %v", admitted, err)
	}
	other := scheduler.Candidate{Workstream: created, Thread: trace.Thread{Identity: trace.Agent{Role: "mason"}}}
	if admitted, err := gate(ctx, other); err != nil || !admitted {
		t.Fatalf("mason turn declined: %t %v", admitted, err)
	}
	f.stop(t)

	// With a bound thread reconciler that would complete any turn operation
	// without running the architect's isolated turn path, the draft still runs
	// through the drafter and the scheduler publishes no operation for it.
	opts, clone, engine, sessions, clock := newArchitectOptions(t)
	opts.Threads = func(r *trace.Repository) (coreadapter.Reconciler, error) {
		return &completedRunner{}, nil
	}
	f2 := &architectFixture{opts: opts, clone: clone, engine: engine, sessions: sessions, clock: clock}
	f2.start(t)
	defer f2.stop(t)
	added, err := f2.c.AddProject(ctx, request(clone))
	must(t, err)
	f2.project, f2.trace = added.Project.ID, added.Project.Trace
	must(t, os.WriteFile(added.Project.Charter, []byte("1. Keep changes small.\n"), 0600))
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
