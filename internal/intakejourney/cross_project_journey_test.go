package intakejourney

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/browsertest"
	"github.com/kpenfound/osmia/internal/cli"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// splitJourney prepares the legacy config using onboarded local repositories.
// Only startup performs the split; all subsequent feature state comes from
// owner API calls and queued agent tools through production enforcement.
type splitJourney struct {
	*productionDesignFixture
	configs map[config.ProjectID]string
	charter trace.Document
}

func newSplitJourney(t *testing.T) *splitJourney {
	t.Helper()
	f := newConfiguredProductionDesignFixture(t, productionDesignConfiguration{
		handIn: true, wait: lifecycleAwait,
		options: func(o *Options) { withWeb(t, *o, "127.0.0.1:0") },
	})
	charter, err := f.c.Charter(context.Background(), f.project)
	must(t, err)
	cfg := f.s.current()
	f.stop(t)
	j := &splitJourney{productionDesignFixture: f, configs: map[config.ProjectID]string{}, charter: charter}
	for i, id := range []config.ProjectID{f.project, f.project2} {
		p := cfg.For(id).Project
		p.Name = []string{"Server", "Client"}[i]
		path, err := cfg.Root.ProjectConfig(id)
		must(t, err)
		must(t, config.WriteProjectConfig(path, p))
		data, err := os.ReadFile(path)
		must(t, err)
		source := ""
		if i == 1 {
			source = string(f.project)
		}
		j.configs[id] = " # Repository settings\n" + string(data) + "\n# Keep this comment\n"
		must(t, os.WriteFile(path, []byte(fmt.Sprintf("group_id = %q", source)+j.configs[id]), 0600))
	}
	f.start(t)
	j.checkSplit(t)
	return j
}

func (j *splitJourney) checkSplit(t *testing.T) {
	t.Helper()
	view, err := j.c.Configuration(context.Background())
	must(t, err)
	if len(view.Projects) != 2 {
		t.Fatalf("startup projects: %+v", view.Projects)
	}
	for i, id := range []config.ProjectID{j.project, j.project2} {
		p := view.Projects[i]
		if p.ID != id || p.Name != []string{"Server", "Client"}[i] || len(p.Repos) != 1 || p.Repos[0].ID != id {
			t.Fatalf("split project: %+v", p)
		}
		path, err := j.s.current().Root.ProjectConfig(id)
		must(t, err)
		data, err := os.ReadFile(path)
		must(t, err)
		if string(data) != j.configs[id] {
			t.Fatalf("startup changed more than group_id: %q", data)
		}
		charter, err := j.c.Charter(context.Background(), id)
		must(t, err)
		if charter.Content != j.charter.Content || i == 1 && charter.Cause != fmt.Sprintf("split-project:%s:charter#%d", j.project, j.charter.Revision) {
			t.Fatalf("split charter: %+v", charter)
		}
	}
}

func (j *splitJourney) checkWorkgroup(t *testing.T, p *browsertest.Page) intake.DesignSelection {
	t.Helper()
	j.checkSplit(t)
	j.awaitPacket(t)
	j.checkContinuation(t)
	r, err := j.s.repository(j.project)
	must(t, err)
	selection, found, err := intake.DesignSelectionOf(r, j.origin)
	must(t, err)
	if !found || len(selection.Members) != 2 {
		t.Fatalf("owner selection: %+v", selection)
	}
	for i, id := range []config.ProjectID{j.project, j.project2} {
		if selection.Members[i].Repo != id || selection.Members[i].Workstream != []config.WorkstreamID{j.stream1, j.stream2}[i] {
			t.Fatalf("owner selection order or member identity changed: %+v", selection)
		}
	}
	doc, err := trace.Get[trace.Document](r, j.origin, intake.DesignSelectionDocument, 1)
	must(t, err)
	if doc.Actor != ownerActor {
		t.Fatalf("project selection was not the owner's decision: %+v", doc)
	}
	var feed api.WorkgroupFeedResponse
	must(t, j.c.Do(context.Background(), "GET", "/v1/workgroups/"+string(selection.Group)+"/feed", nil, &feed))
	if len(feed.Members) != 2 {
		t.Fatalf("workgroup member summaries: %+v", feed)
	}
	for _, member := range selection.Members {
		if !slices.ContainsFunc(feed.Members, func(m api.WorkgroupMemberSummary) bool {
			return m.Repo == member.Repo && m.Workstream == member.Workstream
		}) {
			t.Fatalf("missing member %s in feed: %+v", member.Workstream, feed)
		}
	}
	p.Run(chromedp.Navigate("http://" + j.s.WebAddr() + "/"))
	for i, id := range []config.ProjectID{j.project, j.project2} {
		member := selection.Members[i].Workstream
		name := []string{"Server", "Client"}[i]
		card := `#project-pane [data-project="` + string(id) + `"]`
		p.Await("workgroup on each project card", `document.querySelector(`+quote(card+` [data-workgroup="`+string(selection.Group)+`"]`)+`) !== null`)
		p.AwaitText(card+` [data-pane-workstream="`+string(member)+`"] [data-field=repo]`, name)
		row := `[data-sidebar-workgroup="` + string(selection.Group) + `"]`
		p.AwaitText(row+` [data-select="`+string(member)+`"] [data-field=project]`, name)
	}
	p.Click(`[data-sidebar-workgroup="` + string(selection.Group) + `"] [data-select-workgroup]`)
	p.Await("workgroup members labelled by project name", `[...document.querySelectorAll('#workgroup-content .member-card [data-field=repo]')].map(n => n.textContent).sort().join(',') === 'Client,Server'`)
	return selection
}

func TestCrossProjectHandInJourneyAfterSplit(t *testing.T) {
	t.Parallel()
	for _, surface := range []string{"API", "CLI"} {
		t.Run(surface, func(t *testing.T) {
			t.Parallel()
			p := openBrowser(t)
			j := newSplitJourney(t)
			if surface == "API" {
				handInProduction(t, j.productionDesignFixture, HandInRequest{})
			} else {
				j.cliHandIn(t)
			}
			before := j.checkWorkgroup(t, p)
			j.stop(t)
			j.start(t)
			after := j.checkWorkgroup(t, p)
			if !equalJSONTest(before, after) {
				t.Fatal("restart changed the owner selection or member identities")
			}
		})
	}
}

func (j *splitJourney) cliHandIn(t *testing.T) {
	t.Helper()
	// The CLI generates its own key. Hold fake model execution until the
	// response supplies the identities used by the scripted drafting turns.
	ready := make(chan struct{})
	defer close(ready)
	j.engine.Mu.Lock()
	for name, turn := range j.engine.Turns {
		j.engine.Turns[name] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
			select {
			case <-ready:
				return turn(ctx, req, verified, tools)
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	j.engine.Mu.Unlock()
	var out, stderr bytes.Buffer
	code := cli.Run(context.Background(), []string{"handin", string(j.project), "-", "--with", string(j.project2), "--root", j.opts.Config.Root, "--json"}, strings.NewReader(productionBrief), &out, &stderr)
	if code != 0 {
		t.Fatalf("CLI handin exited %d: %s", code, &stderr)
	}
	var handed api.HandInResponse
	must(t, json.Unmarshal(out.Bytes(), &handed))
	j.origin = handed.Workstream
	j.stream1 = intake.HandInWorkstream(j.project, "design-"+string(j.origin))
	j.stream2 = intake.HandInWorkstream(j.project2, "design-"+string(j.origin))
	r, err := j.s.repository(j.project)
	must(t, err)
	j.handed, err = trace.Get[trace.Document](r, j.origin, intake.HandedDocument, 1)
	must(t, err)
	j.wait(t, "CLI hand-in allocation", func() bool {
		state, err := r.Workflow(j.origin, intake.DesignStartSubject)
		must(t, err)
		return state.Value == "complete"
	})
	chief, host, _, closeChief, err := j.s.chiefRepository(context.Background(), r, j.origin)
	must(t, err)
	j.before, err = chief.Thread(host, trace.ChiefOfStaff)
	closeChief()
	must(t, err)
}

func TestCrossProjectDesignJourneyAfterSplit(t *testing.T) {
	t.Parallel()
	p := openBrowser(t)
	j := newSplitJourney(t)
	ctx := context.Background()
	opened, err := j.c.OpenDesign(ctx, j.project)
	must(t, err)
	j.origin = opened.Design.Workstream
	j.stream1 = intake.HandInWorkstream(j.project, "design-"+string(j.origin))
	j.stream2 = intake.HandInWorkstream(j.project2, "design-"+string(j.origin))
	j.engine.Mu.Lock()
	j.engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		names, err := toolNames(ctx, tools)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(names, intake.DraftBriefToolName) {
			return &agent.Result{ClaudeID: "session-chief-journey", ResultText: "The workgroup follows the agreed brief."}, nil
		}
		projects := []config.ProjectID{j.project}
		if strings.Contains(req.Prompt, "Add Client") {
			projects = append(projects, j.project2)
		}
		result, err := callTool(ctx, tools, intake.DraftBriefToolName, map[string]any{"content": productionBrief, "projects": projects})
		if err == nil && !strings.Contains(result, `"recorded":true`) {
			err = fmt.Errorf("brief refused: %s", result)
		}
		return &agent.Result{ClaudeID: "session-design-journey", ResultText: "Brief recorded."}, err
	}
	j.engine.Mu.Unlock()
	var displayed api.DesignResponse
	for i, message := range []string{"Draft the server upload design.", "Add Client to the brief as well."} {
		_, err = j.c.DesignMessage(ctx, j.origin, message)
		must(t, err)
		awaitDesignConversation(t, j.c, j.origin, func(c ConversationResponse) bool {
			return len(c.Entries) == (i+1)*2 && c.Entries[len(c.Entries)-1].State == api.TurnDone
		})
		view, err := j.c.Design(ctx, j.origin)
		must(t, err)
		if view.Design.BriefRevision != i+1 || view.Design.Brief != strings.TrimSpace(productionBrief) || !slices.Equal(view.Design.Projects, []config.ProjectID{j.project, j.project2}[:i+1]) {
			t.Fatalf("displayed brief: %+v", view.Design)
		}
		if i == 0 {
			displayed = view
		}
	}
	err = j.c.Do(ctx, "POST", "/v1/design/"+string(j.origin)+"/start", api.DesignStartRequest{BriefRevision: &displayed.Design.BriefRevision}, nil)
	if !failed(err, Conflict) {
		t.Fatalf("start with earlier displayed revision: %v", err)
	}
	r, err := j.s.repository(j.project)
	must(t, err)
	docs, err := trace.Read[trace.Document](r, j.origin)
	must(t, err)
	for _, d := range docs {
		if d.ID == intake.HandedDocument || d.ID == intake.DesignSelectionDocument || d.ID == intake.DesignStartIntentDocument {
			t.Fatalf("stale start recorded %s", d.ID)
		}
	}
	for i, id := range []config.ProjectID{j.project, j.project2} {
		repo, err := j.s.repository(id)
		must(t, err)
		streams, err := repo.Workstreams()
		must(t, err)
		if slices.Contains(streams, []config.WorkstreamID{j.stream1, j.stream2}[i]) {
			t.Fatal("stale start allocated a member")
		}
	}
	displayed, err = j.c.Design(ctx, j.origin)
	must(t, err)
	j.before, err = r.ChiefOfStaffThread(j.origin)
	must(t, err)
	must(t, j.c.Do(ctx, "POST", "/v1/design/"+string(j.origin)+"/start", api.DesignStartRequest{BriefRevision: &displayed.Design.BriefRevision}, nil))
	j.handed, err = trace.Get[trace.Document](r, j.origin, intake.HandedDocument, 1)
	must(t, err)
	before := j.checkWorkgroup(t, p)
	intent, found, err := intake.DesignStartIntentOf(r, j.origin)
	must(t, err)
	if !found || intent.BriefRevision != displayed.Design.BriefRevision || !slices.Equal(intent.Projects, displayed.Design.Projects) || !strings.HasPrefix(intent.Handed.Content, displayed.Design.Brief) {
		t.Fatalf("start differs from displayed brief: %+v", intent)
	}
	j.stop(t)
	j.start(t)
	after := j.checkWorkgroup(t, p)
	if !equalJSONTest(before, after) {
		t.Fatal("restart changed the design's owner decision")
	}
	r, err = j.s.repository(j.project)
	must(t, err)
	recovered, found, err := intake.DesignStartIntentOf(r, j.origin)
	must(t, err)
	if !found || !equalJSONTest(intent, recovered) {
		t.Fatal("restart changed the pinned brief, revision or projects")
	}
}
