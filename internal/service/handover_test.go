package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/events"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/issues"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/status"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// handoverChief is the fake chief of staff of the M2 demonstration. It
// records the prompt of every event turn and, once a turn tells it a
// workstream was sketched, writes that workstream's status.
type handoverChief struct {
	mu      sync.Mutex
	prompts map[string][]string
	written map[string]string
}

func (h *handoverChief) turn(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
	listed, err := tools.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	if !slices.Equal(names, []string{status.ToolName}) {
		return nil, fmt.Errorf("chief of staff tools %v", names)
	}
	stream := filepath.Base(filepath.Dir(filepath.Dir(req.SessionDir)))
	h.mu.Lock()
	h.prompts[stream] = append(h.prompts[stream], req.Prompt)
	h.mu.Unlock()
	if strings.Contains(req.Prompt, "Workstream state changed from handed to sketched") {
		out, err := callTool(ctx, tools, status.ToolName, map[string]any{
			"goal": "Ship resumable uploads.", "attention": "Review the drafted spec and plan.",
			"note": "The architect's plan passed validation.", "agents": []string{"The architect is idle."}})
		if err != nil {
			return nil, err
		}
		h.mu.Lock()
		h.written[stream] = out
		h.mu.Unlock()
	}
	return &agent.Result{ClaudeID: "session-chief", ResultText: "Noted", SessionDir: req.SessionDir, NumTurns: 1}, nil
}

// TestM2HandInToSketchedPlan demonstrates the M2 hand-in: see
// docs/m2-demonstration.md.
func TestM2HandInToSketchedPlan(t *testing.T) {
	ctx := context.Background()
	opts, clone, engine, sessions, clock := newArchitectOptions(t)
	home := filepath.Dir(clone)
	root := opts.Config.Root
	ref := issues.Ref{Owner: "owner", Repo: "repo", Number: 3}
	const issueBody = "# Resumable uploads\r\n\nFrom the issue tracker.\n"
	opts.Issues = &fakeIssues{token: "ghp_architect_secret", text: map[issues.Ref]string{ref: issueBody}}

	chief := &handoverChief{prompts: map[string][]string{}, written: map[string]string{}}
	engine.turns["*"] = chief.turn
	opts.Threads = func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
		turns := &isolation.Turns{Workspaces: stagedWorkspaces{}, Views: isolation.Views{Directory: filepath.Join(root, "views")}, Engine: engine,
			Grants: map[string]coreadapter.Capabilities{trace.ChiefOfStaff: {Tools: []string{status.ToolName}}},
			Select: func(_ context.Context, scope coreadapter.Scope) (isolation.Selection, error) {
				workspace := filepath.Join(root, "chief", scope.Workstream, scope.Turn)
				if err := os.MkdirAll(workspace, 0700); err != nil {
					return isolation.Selection{}, err
				}
				return isolation.Selection{Workspace: coreadapter.WorkspaceRequest{SourceDirectory: workspace, Directory: workspace}, Execution: coreadapter.ExecutionSettings{Mode: "container", Image: "fixture-image"}}, nil
			},
			Scoped: func(_ context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
				set, err := status.Tool(r, trace.ChiefOfStaff, scope, clock.Now)
				return []coreadapter.Tool{set}, err
			},
			Hosts: &coreadapter.MCPHost{Transport: &demoTransport{sessions: sessions}},
		}
		return thread.Dispatcher{Runner: thread.Runner{Store: r, Turns: turns, Now: clock.Now},
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				directory := filepath.Join(root, "sessions", string(in.Workstream), in.Turn, "session")
				return coreadapter.PreparedTurn{SessionDirectory: directory}, os.MkdirAll(directory, 0700)
			}}, nil
	}

	// Draft 1 has a dependency cycle and leaves criterion 2 unaddressed;
	// draft 2 is told why and delivers a valid plan.
	problems := "- spec#2: no unit addresses this criterion\n- unit \"dedupe\": dependency cycle dedupe -> resume -> dedupe"
	f := &architectFixture{opts: opts, clone: clone, engine: engine, sessions: sessions, clock: clock}
	f.script("draft-1-1", map[string]string{plan.SpecPath: validSpec, plan.PlanPath: cyclicPlan}, nil)
	f.script("draft-2-1", map[string]string{plan.SpecPath: validSpec, plan.PlanPath: validPlan},
		func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
			if !strings.Contains(req.Prompt, "Draft 1 was not accepted:\ndraft 1 of the spec and plan is invalid:\n"+problems) {
				return fmt.Errorf("prompt lacks the problems:\n%s", req.Prompt)
			}
			if got, err := readTool(ctx, tools, "draft/plan.json"); err != nil || got != cyclicPlan {
				return fmt.Errorf("previous plan: %q %v", got, err)
			}
			return nil
		})
	f.start(t)
	defer f.stop(t)
	added, err := f.c.AddProject(ctx, request(clone))
	must(t, err)
	f.project, f.trace = added.Project.ID, added.Project.Trace

	// An onboarded project with an empty charter takes no work.
	design := filepath.Join(home, "design.md")
	must(t, os.WriteFile(design, []byte(handedDesign), 0600))
	_, err = f.c.HandIn(ctx, HandInRequest{Project: f.project, Key: "refused", Path: design})
	var api *APIError
	if !errors.As(err, &api) || api.Code != CharterEmpty || api.Message != fmt.Sprintf("project %s cannot take work: its charter has no rules; write numbered rules (\"1. ...\") in %s", f.project, added.Project.Charter) {
		t.Fatalf("empty charter: %v", err)
	}
	if list, err := f.c.Statuses(ctx); err != nil || len(list.Workstreams) != 0 {
		t.Fatalf("the refused hand-in created a workstream: %+v %v", list, err)
	}
	must(t, os.WriteFile(added.Project.Charter, []byte("1. Keep changes small.\n2. Every change has a test.\n"), 0600))

	// Each hand-in copies its input unchanged under handed/.
	stdin := "# Resumable uploads\n\n\tFrom stdin, with a tab and é.\n"
	inputs := []struct {
		req            HandInRequest
		source, handed string
		content        string
	}{
		{HandInRequest{Key: "file", Path: design}, "file:" + design, "design.md", handedDesign},
		{HandInRequest{Key: "url", URL: "https://github.com/owner/repo/issues/3"}, "https://github.com/owner/repo/issues/3", "issue-3.md", issueBody},
		{HandInRequest{Key: "stdin", Stdin: &stdin}, "stdin", "stdin", stdin},
	}
	var streams []config.WorkstreamID
	for _, in := range inputs {
		in.req.Project = f.project
		out, err := f.c.HandIn(ctx, in.req)
		must(t, err)
		path := filepath.Join(f.trace, "workstreams", string(out.Workstream), "handed", in.handed)
		if out.Project != f.project || out.State != HandedState || out.Source != in.source || out.Handed != path {
			t.Fatalf("hand-in %s: %+v", in.req.Key, out)
		}
		if data, err := os.ReadFile(path); err != nil || string(data) != in.content {
			t.Fatalf("handed copy %s: %q %v", path, data, err)
		}
		docs := f.documents(t, out.Workstream, handedDocument)
		if len(docs) != 1 || docs[0].Content != in.content || docs[0].Source != in.source || docs[0].Path != "handed/"+in.handed || docs[0].Actor != ownerActor {
			t.Fatalf("handed document %s: %+v", in.req.Key, docs)
		}
		streams = append(streams, out.Workstream)
	}
	if unique := slices.Compact(slices.Sorted(slices.Values(streams))); len(unique) != 3 {
		t.Fatalf("hand-ins shared a workstream: %v", streams)
	}

	sketches := map[config.WorkstreamID]string{}
	for _, stream := range streams {
		f.await(t, stream, sketched)
		// Draft 1 is recorded and sent back; draft 2 moves the workstream.
		ops := f.draftOperations(t, stream)
		if len(ops) != 2 || ops[0].Result == nil || ops[0].Result.Outcome != "failed" || ops[1].Result == nil || ops[1].Result.Outcome != "succeeded" {
			t.Fatalf("draft operations of %s: %+v", stream, ops)
		}
		byID := map[string]trace.Transition{}
		for _, tr := range f.transitions(t, stream) {
			byID[tr.ID] = tr
		}
		invalid := byID["draft-1-invalid"]
		if invalid.Subject != draftSubject || invalid.To != "invalid-1" || invalid.Actor != draftingActor || invalid.Reason != "draft 1 of the spec and plan is invalid:\n"+problems || ops[0].Result.Evidence != invalid.Reason {
			t.Fatalf("invalid draft of %s: %+v", stream, invalid)
		}
		if again := byID["draft-2"]; again.From != "invalid-1" || again.To != "drafting-2" || again.Cause != "draft-1-invalid" {
			t.Fatalf("second draft request of %s: %+v", stream, again)
		}
		for id, want := range map[string][]string{plan.SpecDocument: {validSpec, validSpec}, plan.PlanDocument: {cyclicPlan, validPlan}} {
			docs := f.documents(t, stream, id)
			if len(docs) != 2 {
				t.Fatalf("%s revisions of %s: %+v", id, stream, docs)
			}
			for i, doc := range docs {
				if doc.Revision != i+1 || doc.Content != want[i] || doc.Actor != architectActor || doc.Cause != ops[i].Operation.ID {
					t.Fatalf("%s revision %d of %s: %+v", id, i+1, stream, doc)
				}
			}
		}
		sketch := byID["sketched"]
		if sketch.Subject != trace.FeatureSubject || sketch.From != HandedState || sketch.To != SketchedState || sketch.Actor != draftingActor || sketch.Cause != ops[1].Operation.ID ||
			sketch.Reason != "the architect's draft 2 passed validation: spec.md revision 2 with 2 acceptance criteria and plan.json revision 2 with 2 units" {
			t.Fatalf("sketched transition of %s: %+v", stream, sketch)
		}
		sketches[stream] = sketch.Reason
		events, err := os.ReadFile(filepath.Join(f.trace, "workstreams", string(stream), "events.jsonl"))
		must(t, err)
		if !strings.Contains(string(events), `"from":"handed","to":"sketched"`) {
			t.Fatalf("events.jsonl of %s:\n%s", stream, events)
		}
	}

	// The chief of staff hears of the hand-in and of the sketch, and its
	// status shows every new workstream.
	deadline := time.Now().Add(demoTimeout)
	for {
		list, err := f.c.Statuses(ctx)
		must(t, err)
		ready := len(list.Workstreams) == 3
		for _, st := range list.Workstreams {
			ready = ready && st.Status != nil
		}
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("statuses: %+v", list)
		}
		time.Sleep(100 * time.Millisecond)
	}
	chief.mu.Lock()
	defer chief.mu.Unlock()
	for i, stream := range streams {
		got, err := f.c.Status(ctx, stream)
		must(t, err)
		st := got.Status
		if got.Workstream != stream || got.Project != f.project || got.State == nil || *got.State != SketchedState || st == nil ||
			st.Goal != "Ship resumable uploads." || st.Attention != "Review the drafted spec and plan." || st.Revision != 1 {
			t.Fatalf("status of %s: %+v %+v", stream, got, st)
		}
		if chief.written[string(stream)] != `{"stored":true,"revision":1}` {
			t.Fatalf("set_status of %s: %q", stream, chief.written[string(stream)])
		}
		prompts := strings.Join(chief.prompts[string(stream)], "\n")
		for _, want := range []string{
			events.Preamble,
			"Workstream state changed to handed: the owner handed in handed/" + inputs[i].handed + " from " + inputs[i].source,
			"Workstream state changed from handed to sketched: " + sketches[stream],
		} {
			if !strings.Contains(prompts, want) {
				t.Fatalf("chief of staff of %s was not told %q:\n%s", stream, want, prompts)
			}
		}
		if strings.Contains(prompts, "is invalid") {
			t.Fatalf("chief of staff of %s heard of the invalid draft:\n%s", stream, prompts)
		}
	}
	drafts := slices.DeleteFunc(f.runs(), func(run string) bool { return !strings.HasPrefix(run, "draft-") })
	if slices.Sort(drafts); !slices.Equal(drafts, []string{"draft-1-1", "draft-1-1", "draft-1-1", "draft-2-1", "draft-2-1", "draft-2-1"}) {
		t.Fatalf("architect runs %v", drafts)
	}
}
