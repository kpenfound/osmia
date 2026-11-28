package executionjourney

import (
	context "context"
	fmt "fmt"
	os "os"
	filepath "path/filepath"
	slices "slices"
	strings "strings"
	sync "sync"
	testing "testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	events "github.com/kpenfound/osmia/internal/events"
	isolation "github.com/kpenfound/osmia/internal/isolation"
	issues "github.com/kpenfound/osmia/internal/issues"
	plan "github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	status "github.com/kpenfound/osmia/internal/status"
	thread "github.com/kpenfound/osmia/internal/thread"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type handoverManager struct {
	mu      sync.Mutex
	prompts map[string][]string
	written map[string]string
}

func (h *handoverManager) turn(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
	listed, err := tools.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	if !slices.Equal(names, []string{status.ToolName}) {
		return nil, fmt.Errorf("manager tools %v", names)
	}
	stream := filepath.Base(filepath.Dir(filepath.Dir(req.SessionDir)))
	h.mu.Lock()
	h.prompts[stream] = append(h.prompts[stream], req.Prompt)
	h.mu.Unlock()
	if strings.Contains(req.Prompt, "Workstream state changed from handed to sketched") {
		out, err := fixturekit.CallTool(ctx, tools, status.ToolName, map[string]any{
			"goal": "Ship resumable uploads.",
			"note": "The architect's plan passed validation.", "agents": []string{"The architect is idle."}})
		if err != nil {
			return nil, err
		}
		h.mu.Lock()
		h.written[stream] = out
		h.mu.Unlock()
	}
	return &agent.Result{ClaudeID: "session-manager", ResultText: "Noted", SessionDir: req.SessionDir, NumTurns: 1}, nil
}

func TestHandInToSketchedPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, clone, engine, sessions, clock := journey.NewArchitectOptions(t)
	home := filepath.Dir(clone)
	root := opts.Config.Root
	ref := issues.Ref{Owner: "owner", Repo: "repo", Number: 3}
	const issueBody = "# Resumable uploads\r\n\nFrom the issue tracker.\n"
	opts.Issues = &fixturekit.FakeIssues{Token: "ghp_architect_secret", Text: map[issues.Ref]string{ref: issueBody}}

	manager := &handoverManager{prompts: map[string][]string{}, written: map[string]string{}}
	engine.Turns["*"] = manager.turn
	opts.Threads = func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
		turns := &isolation.Turns{Workspaces: intake.StagedWorkspaces{}, Views: isolation.Views{Directory: filepath.Join(root, "views")}, Engine: engine,
			Grants: map[string]coreadapter.Capabilities{trace.Manager: {Tools: []string{status.ToolName}}},
			Select: func(_ context.Context, scope coreadapter.Scope) (isolation.Selection, error) {
				workspace := filepath.Join(root, "manager", scope.Workstream, scope.Turn)
				if err := os.MkdirAll(workspace, 0700); err != nil {
					return isolation.Selection{}, err
				}
				return isolation.Selection{Workspace: coreadapter.WorkspaceRequest{SourceDirectory: workspace, Directory: workspace}, Execution: coreadapter.ExecutionSettings{Mode: "container", Image: "fixture-image"}}, nil
			},
			Scoped: func(_ context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
				set, err := status.Tool(r, trace.Manager, scope, clock.Now)
				return []coreadapter.Tool{set}, err
			},
			Hosts: &coreadapter.MCPHost{Transport: &fixturekit.DemoTransport{Sessions: sessions}},
		}
		return thread.Dispatcher{Runner: thread.Runner{Jev: fixturekit.UnconfiguredJudge(nil), Store: r, Turns: turns, Now: clock.Now},
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				directory := filepath.Join(root, "sessions", string(in.Workstream), in.Turn, "session")
				return coreadapter.PreparedTurn{SessionDirectory: directory}, os.MkdirAll(directory, 0700)
			}}, nil
	}

	problems := "- spec#2: no unit serves this criterion\n- unit \"dedupe\": dependency cycle dedupe -> resume -> dedupe"
	f := &journey.ArchitectFixture{Opts: opts, Clone: clone, Engine: engine, Sessions: sessions, Clock: clock}
	f.Script("draft-1-1", map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.CyclicPlan}, nil)
	f.Script("draft-2-1", map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.ValidPlan},
		func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
			if !strings.Contains(req.Prompt, "Draft 1 was not accepted:\ndraft 1 of the spec and plan is invalid:\n"+problems) {
				return fmt.Errorf("prompt lacks the problems:\n%s", req.Prompt)
			}
			if got, err := intakefixture.ReadTool(ctx, tools, "draft/plan.json"); err != nil || got != fixturekit.CyclicPlan {
				return fmt.Errorf("previous plan: %q %v", got, err)
			}
			return nil
		})
	f.Start(t)
	defer f.Stop(t)
	added, err := f.C.AddProject(ctx, fixturekit.Request(clone))
	fixturekit.Must(t, err)
	f.Project, f.Trace = added.Project.ID, added.Project.Trace

	design := filepath.Join(home, "design.md")
	fixturekit.Must(t, os.WriteFile(design, []byte(fixturekit.HandedDesign), 0600))
	intakefixture.WriteCharter(t, f.C, added.Project.ID, intakefixture.ShedCharter)

	stdin := "# Resumable uploads\n\n\tFrom stdin, with a tab and é.\n"
	inputs := []struct {
		req            api.HandInRequest
		source, handed string
		content        string
	}{
		{api.HandInRequest{Key: "file", Path: design}, "file:" + design, "design.md", fixturekit.HandedDesign},
		{api.HandInRequest{Key: "url", URL: "https://github.com/owner/repo/issues/3"}, "https://github.com/owner/repo/issues/3", "issue-3.md", issueBody},
		{api.HandInRequest{Key: "stdin", Stdin: &stdin}, "stdin", "stdin", stdin},
	}
	var streams []config.WorkstreamID
	for _, in := range inputs {
		in.req.Project = f.Project
		out, err := f.C.HandIn(ctx, in.req)
		fixturekit.Must(t, err)
		if out.Project != f.Project || out.State != intake.HandedState || out.Source != in.source {
			t.Fatalf("hand-in %s: %+v", in.req.Key, out)
		}
		if data, err := fixturekit.WorkstreamDocument(t, f.S.Cfg.Root, f.Project, out.Workstream, "handed/"+in.handed); err != nil || string(data) != in.content {
			t.Fatalf("handed copy %s: %q %v", in.handed, data, err)
		}
		docs := f.Documents(t, out.Workstream, intake.HandedDocument)
		if len(docs) != 1 || docs[0].Content != in.content || docs[0].Source != in.source || docs[0].Path != "handed/"+in.handed || docs[0].Actor != intake.OwnerActor {
			t.Fatalf("handed document %s: %+v", in.req.Key, docs)
		}
		streams = append(streams, out.Workstream)
	}
	if unique := slices.Compact(slices.Sorted(slices.Values(streams))); len(unique) != 3 {
		t.Fatalf("hand-ins shared a workstream: %v", streams)
	}

	sketches := map[config.WorkstreamID]string{}
	for _, stream := range streams {
		f.Await(t, stream, intakefixture.Sketched)

		ops := f.AcknowledgedDraftOperations(t, stream)
		if len(ops) != 2 || ops[0].Result == nil || ops[0].Result.Outcome != "failed" || ops[1].Result == nil || ops[1].Result.Outcome != "succeeded" {
			t.Fatalf("draft operations of %s: %+v", stream, ops)
		}
		byID := map[string]trace.Transition{}
		for _, tr := range f.Transitions(t, stream) {
			byID[tr.ID] = tr
		}
		invalid := byID["draft-1-invalid"]
		if invalid.Subject != intake.DraftSubject || invalid.To != "invalid-1" || invalid.Actor != intake.DraftingActor || invalid.Reason != "draft 1 of the spec and plan is invalid:\n"+problems || ops[0].Result.Evidence != invalid.Reason {
			t.Fatalf("invalid draft of %s: %+v", stream, invalid)
		}
		if again := byID["draft-2"]; again.From != "invalid-1" || again.To != "drafting-2" || again.Cause != "draft-1-invalid" {
			t.Fatalf("second draft request of %s: %+v", stream, again)
		}
		for id, want := range map[string][]string{plan.SpecDocument: {fixturekit.ValidSpec, fixturekit.ValidSpec}, plan.PlanDocument: {fixturekit.CyclicPlan, fixturekit.ValidPlan}} {
			docs := f.Documents(t, stream, id)
			if len(docs) != 2 {
				t.Fatalf("%s revisions of %s: %+v", id, stream, docs)
			}
			for i, doc := range docs {
				if doc.Revision != i+1 || doc.Content != want[i] || doc.Actor != intake.ArchitectActor || doc.Cause != ops[i].Operation.ID {
					t.Fatalf("%s revision %d of %s: %+v", id, i+1, stream, doc)
				}
			}
		}
		sketch := byID["sketched"]
		if sketch.Subject != trace.FeatureSubject || sketch.From != intake.HandedState || sketch.To != intake.SketchedState || sketch.Actor != intake.DraftingActor || sketch.Cause != ops[1].Operation.ID ||
			sketch.Reason != "the architect's draft 2 passed validation: spec.md revision 2 with 2 acceptance criteria and plan.json revision 2 with 2 units" {
			t.Fatalf("sketched transition of %s: %+v", stream, sketch)
		}
		sketches[stream] = sketch.Reason
		if events := intakefixture.AllTransitions(t, f.S.Cfg.Root, f.Project, stream); !slices.ContainsFunc(events, func(tr trace.Transition) bool { return tr.From == "handed" && tr.To == "sketched" }) {
			t.Fatalf("transitions of %s: %+v", stream, events)
		}
	}

	intakefixture.Soon(t, "every workstream to carry a status", func() bool {
		list, err := f.C.Statuses(ctx)
		fixturekit.Must(t, err)
		ready := len(list.Workstreams) == 3
		for _, st := range list.Workstreams {
			ready = ready && st.Status != nil
		}
		return ready
	})
	manager.mu.Lock()
	defer manager.mu.Unlock()
	for i, stream := range streams {
		got, err := f.C.Status(ctx, stream)
		fixturekit.Must(t, err)
		st := got.Status
		if got.Workstream != stream || got.Project != f.Project || got.State == nil || *got.State != intake.SketchedState || st == nil ||
			st.Goal != "Ship resumable uploads." || st.Attention != "" || st.Revision != 1 {
			t.Fatalf("status of %s: %+v %+v", stream, got, st)
		}
		if manager.written[string(stream)] != `{"stored":true,"revision":1}` {
			t.Fatalf("set_status of %s: %q", stream, manager.written[string(stream)])
		}
		prompts := strings.Join(manager.prompts[string(stream)], "\n")
		for _, want := range []string{
			events.Preamble,
			"Workstream state changed to handed: the owner handed in handed/" + inputs[i].handed + " from " + inputs[i].source,
			"Workstream state changed from handed to sketched: " + sketches[stream],
		} {
			if !strings.Contains(prompts, want) {
				t.Fatalf("manager of %s was not told %q:\n%s", stream, want, prompts)
			}
		}
		if strings.Contains(prompts, "is invalid") {
			t.Fatalf("manager of %s heard of the invalid draft:\n%s", stream, prompts)
		}
	}
	drafts := slices.DeleteFunc(f.Runs(), func(run string) bool { return !strings.HasPrefix(run, "draft-") })
	if slices.Sort(drafts); !slices.Equal(drafts, []string{"draft-1-1", "draft-1-1", "draft-1-1", "draft-2-1", "draft-2-1", "draft-2-1"}) {
		t.Fatalf("architect runs %v", drafts)
	}
}
