package intakejourney

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/browsertest"
	"github.com/kpenfound/osmia/internal/cli"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type crossProjectJourney struct {
	*journey.ProductionDesignFixture
}

func newCrossProjectJourney(t *testing.T) *crossProjectJourney {
	t.Helper()
	f := journey.NewConfiguredProductionDesignFixture(t, journey.ProductionDesignConfiguration{
		HandIn: true, Wait: journey.LifecycleAwait,
		Options: func(o *journey.Options) { intakefixture.WithWeb(t, o.Config.Root, "127.0.0.1:0") },
	})
	cfg := f.S.Current()
	f.Stop(t)
	for i, id := range []config.ProjectID{f.Project, f.Project2} {
		p := cfg.For(id).Project
		p.Name = []string{"Server", "Client"}[i]
		fixturekit.SaveProjectSettings(t, cfg.Root.String(), p)
	}
	f.Start(t)
	j := &crossProjectJourney{ProductionDesignFixture: f}
	j.checkProjects(t)
	return j
}

func (j *crossProjectJourney) checkProjects(t *testing.T) {
	t.Helper()
	view, err := j.C.Configuration(context.Background())
	fixturekit.Must(t, err)
	if len(view.Projects) != 2 {
		t.Fatalf("startup projects: %+v", view.Projects)
	}
	for i, id := range []config.ProjectID{j.Project, j.Project2} {
		p := view.Projects[i]
		if p.ID != id || p.Name != []string{"Server", "Client"}[i] || len(p.Repos) != 1 || p.Repos[0].ID != id {
			t.Fatalf("project: %+v", p)
		}
	}
}

func (j *crossProjectJourney) checkWorkgroup(t *testing.T, p *browsertest.Page) intake.DesignSelection {
	t.Helper()
	j.checkProjects(t)
	j.AwaitPacket(t)
	j.CheckContinuation(t)
	r, err := j.S.Repository(j.Project)
	fixturekit.Must(t, err)
	selection, found, err := intake.DesignSelectionOf(r, j.Origin)
	fixturekit.Must(t, err)
	if !found || len(selection.Members) != 2 {
		t.Fatalf("owner selection: %+v", selection)
	}
	for i, id := range []config.ProjectID{j.Project, j.Project2} {
		if selection.Members[i].Repo != id || selection.Members[i].Workstream != []config.WorkstreamID{j.Stream1, j.Stream2}[i] {
			t.Fatalf("owner selection order or member identity changed: %+v", selection)
		}
	}
	doc, err := trace.Get[trace.Document](r, j.Origin, intake.DesignSelectionDocument, 1)
	fixturekit.Must(t, err)
	if doc.Actor != intake.OwnerActor {
		t.Fatalf("project selection was not the owner's decision: %+v", doc)
	}
	var feed api.WorkgroupFeedResponse
	fixturekit.Must(t, j.C.Do(context.Background(), "GET", "/v1/workgroups/"+string(selection.Group)+"/feed", nil, &feed))
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
	p.Run(chromedp.Navigate("http://" + j.S.WebAddr() + "/"))
	for i, id := range []config.ProjectID{j.Project, j.Project2} {
		member := selection.Members[i].Workstream
		name := []string{"Server", "Client"}[i]
		card := `#project-pane [data-project="` + string(id) + `"]`
		p.Await("workgroup on each project card", `document.querySelector(`+browsertest.Quote(card+` [data-workgroup="`+string(selection.Group)+`"]`)+`) !== null`)
		p.AwaitText(card+` [data-pane-workstream="`+string(member)+`"] [data-field=repo]`, name)
		row := `[data-sidebar-workgroup="` + string(selection.Group) + `"]`
		p.AwaitText(row+` [data-select="`+string(member)+`"] [data-field=project]`, name)
	}
	p.Click(`[data-sidebar-workgroup="` + string(selection.Group) + `"] [data-select-workgroup]`)
	p.Await("workgroup members labelled by project name", `[...document.querySelectorAll('#workgroup-content .member-card [data-field=repo]')].map(n => n.textContent).sort().join(',') === 'Client,Server'`)
	return selection
}

func TestCrossProjectHandInJourney(t *testing.T) {
	t.Parallel()
	for _, surface := range []string{"API", "CLI"} {
		t.Run(surface, func(t *testing.T) {
			t.Parallel()
			p := browsertest.OpenBrowser(t)
			j := newCrossProjectJourney(t)
			if surface == "API" {
				handInProduction(t, j.ProductionDesignFixture, api.HandInRequest{})
			} else {
				j.cliHandIn(t)
			}
			before := j.checkWorkgroup(t, p)
			j.Stop(t)
			j.Start(t)
			after := j.checkWorkgroup(t, p)
			if !intakefixture.EqualJSONTest(before, after) {
				t.Fatal("restart changed the owner selection or member identities")
			}
		})
	}
}

func (j *crossProjectJourney) cliHandIn(t *testing.T) {
	t.Helper()
	ready := make(chan struct{})
	defer close(ready)
	j.Engine.Mu.Lock()
	for name, turn := range j.Engine.Turns {
		j.Engine.Turns[name] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
			select {
			case <-ready:
				return turn(ctx, req, verified, tools)
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	j.Engine.Mu.Unlock()
	var out, stderr bytes.Buffer
	code := cli.Run(context.Background(), []string{"handin", string(j.Project), "-", "--with", string(j.Project2), "--root", j.Opts.Config.Root, "--json"}, strings.NewReader(intakefixture.ProductionBrief), &out, &stderr)
	if code != 0 {
		t.Fatalf("CLI handin exited %d: %s", code, &stderr)
	}
	var handed api.HandInResponse
	fixturekit.Must(t, json.Unmarshal(out.Bytes(), &handed))
	j.Origin = handed.Workstream
	j.Stream1 = intake.HandInWorkstream(j.Project, "design-"+string(j.Origin))
	j.Stream2 = intake.HandInWorkstream(j.Project2, "design-"+string(j.Origin))
	r, err := j.S.Repository(j.Project)
	fixturekit.Must(t, err)
	j.Handed, err = trace.Get[trace.Document](r, j.Origin, intake.HandedDocument, 1)
	fixturekit.Must(t, err)
	j.Wait(t, "CLI hand-in allocation", func() bool {
		state, err := r.Workflow(j.Origin, intake.DesignStartSubject)
		fixturekit.Must(t, err)
		return state.Value == "complete"
	})
	manager, host, _, closeManager, err := j.S.ManagerRepository(context.Background(), r, j.Origin)
	fixturekit.Must(t, err)
	j.Before, err = manager.Thread(host, trace.Manager)
	closeManager()
	fixturekit.Must(t, err)
}

func TestCrossProjectDesignJourney(t *testing.T) {
	t.Parallel()
	p := browsertest.OpenBrowser(t)
	j := newCrossProjectJourney(t)
	ctx := context.Background()
	opened, err := j.C.OpenDesign(ctx, j.Project)
	fixturekit.Must(t, err)
	j.Origin = opened.Design.Workstream
	j.Stream1 = intake.HandInWorkstream(j.Project, "design-"+string(j.Origin))
	j.Stream2 = intake.HandInWorkstream(j.Project2, "design-"+string(j.Origin))
	j.Engine.Mu.Lock()
	j.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		names, err := intakefixture.ToolNames(ctx, tools)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(names, intake.DraftBriefToolName) {
			return &agent.Result{ClaudeID: "session-chief-journey", ResultText: "The workgroup follows the agreed brief."}, nil
		}
		projects := []config.ProjectID{j.Project}
		if strings.Contains(req.Prompt, "Add Client") {
			projects = append(projects, j.Project2)
		}
		result, err := fixturekit.CallTool(ctx, tools, intake.DraftBriefToolName, map[string]any{"content": intakefixture.ProductionBrief, "projects": projects})
		if err == nil && !strings.Contains(result, `"recorded":true`) {
			err = fmt.Errorf("brief refused: %s", result)
		}
		return &agent.Result{ClaudeID: "session-design-journey", ResultText: "Brief recorded."}, err
	}
	j.Engine.Mu.Unlock()
	var displayed api.DesignResponse
	for i, message := range []string{"Draft the server upload design.", "Add Client to the brief as well."} {
		_, err = j.C.DesignMessage(ctx, j.Origin, message)
		fixturekit.Must(t, err)
		awaitDesignConversation(t, j.C, j.Origin, func(c api.ConversationResponse) bool {
			return len(c.Entries) == (i+1)*2 && c.Entries[len(c.Entries)-1].State == api.TurnDone
		})
		view, err := j.C.Design(ctx, j.Origin)
		fixturekit.Must(t, err)
		if view.Design.BriefRevision != i+1 || view.Design.Brief != strings.TrimSpace(intakefixture.ProductionBrief) || !slices.Equal(view.Design.Projects, []config.ProjectID{j.Project, j.Project2}[:i+1]) {
			t.Fatalf("displayed brief: %+v", view.Design)
		}
		if i == 0 {
			displayed = view
		}
	}
	err = j.C.Do(ctx, "POST", "/v1/design/"+string(j.Origin)+"/start", api.DesignStartRequest{BriefRevision: &displayed.Design.BriefRevision}, nil)
	if !intakefixture.Failed(err, api.Conflict) {
		t.Fatalf("start with earlier displayed revision: %v", err)
	}
	r, err := j.S.Repository(j.Project)
	fixturekit.Must(t, err)
	docs, err := trace.Read[trace.Document](r, j.Origin)
	fixturekit.Must(t, err)
	for _, d := range docs {
		if d.ID == intake.HandedDocument || d.ID == intake.DesignSelectionDocument || d.ID == intake.DesignStartIntentDocument {
			t.Fatalf("stale start recorded %s", d.ID)
		}
	}
	for i, id := range []config.ProjectID{j.Project, j.Project2} {
		repo, err := j.S.Repository(id)
		fixturekit.Must(t, err)
		streams, err := repo.Workstreams()
		fixturekit.Must(t, err)
		if slices.Contains(streams, []config.WorkstreamID{j.Stream1, j.Stream2}[i]) {
			t.Fatal("stale start allocated a member")
		}
	}
	displayed, err = j.C.Design(ctx, j.Origin)
	fixturekit.Must(t, err)
	j.Before, err = r.ManagerThread(j.Origin)
	fixturekit.Must(t, err)
	j.Design, err = r.DesignerThread(j.Origin)
	fixturekit.Must(t, err)
	fixturekit.Must(t, j.C.Do(ctx, "POST", "/v1/design/"+string(j.Origin)+"/start", api.DesignStartRequest{BriefRevision: &displayed.Design.BriefRevision}, nil))
	j.Handed, err = trace.Get[trace.Document](r, j.Origin, intake.HandedDocument, 1)
	fixturekit.Must(t, err)
	before := j.checkWorkgroup(t, p)
	intent, found, err := intake.DesignStartIntentOf(r, j.Origin)
	fixturekit.Must(t, err)
	if !found || intent.BriefRevision != displayed.Design.BriefRevision || !slices.Equal(intent.Projects, displayed.Design.Projects) || !strings.HasPrefix(intent.Handed.Content, displayed.Design.Brief) {
		t.Fatalf("start differs from displayed brief: %+v", intent)
	}
	j.Stop(t)
	j.Start(t)
	after := j.checkWorkgroup(t, p)
	if !intakefixture.EqualJSONTest(before, after) {
		t.Fatal("restart changed the design's owner decision")
	}
	r, err = j.S.Repository(j.Project)
	fixturekit.Must(t, err)
	recovered, found, err := intake.DesignStartIntentOf(r, j.Origin)
	fixturekit.Must(t, err)
	if !found || !intakefixture.EqualJSONTest(intent, recovered) {
		t.Fatal("restart changed the pinned brief, revision or projects")
	}
}
