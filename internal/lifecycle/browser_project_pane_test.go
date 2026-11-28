package lifecycle_test

import (
	context "context"
	json "encoding/json"
	fmt "fmt"
	os "os"
	filepath "path/filepath"
	strings "strings"
	atomic "sync/atomic"
	testing "testing"

	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	chromedp "github.com/chromedp/chromedp"
	agent "github.com/kpenfound/agentcore/agent"
	api "github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/browsertest"
	config "github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	pulls "github.com/kpenfound/osmia/internal/pulls"
	questions "github.com/kpenfound/osmia/internal/questions"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	status "github.com/kpenfound/osmia/internal/status"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestBrowserProjectPaneStatesAndSingleMemberDrilldown(t *testing.T) {
	t.Parallel()
	serviceOptions, _, repo := servicetest.ActivityFixture(t)
	intakefixture.WithWeb(t, serviceOptions.Config.Root, "127.0.0.1:0")
	opts := journey.Options{Options: serviceOptions}
	ctx := context.Background()
	states := []string{intake.HandedState, intake.RatifiedState, intake.BuildingState, intake.BuildingState, intake.DeliveredState, intake.DeliveredState, intake.AbandonedState}
	labels := []string{"planning", "ready", "building", "paused", "PR open", "delivered (merged)", "cancelled"}
	var ids []config.WorkstreamID
	for i, state := range states {
		id := config.WorkstreamID(fmt.Sprintf("w_%032x", i+200))
		ids = append(ids, id)
		fixturekit.Must(t, repo.CreateWorkstream(ctx, id, fixturekit.DemoStart, intake.OwnerActor))
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "pane-state", Revision: 1, Project: repo.Project(), Workstream: id, At: fixturekit.DemoStart, Actor: intake.OwnerActor, Cause: "fixture"}
		_, err := repo.SetFeatureState(ctx, h, state, "Project pane fixture")
		fixturekit.Must(t, err)
		if i == 4 || i == 5 {
			publication := api.DeliveryPublication{Status: delivery.PublicationOpened, PullRequest: i + 100, URL: fmt.Sprintf("https://example.invalid/pull/%d", i+100)}
			data, err := json.Marshal(publication)
			fixturekit.Must(t, err)
			h.Schema, h.ID = "osmia.trace.document", delivery.PublicationDocument
			fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: h, Path: delivery.PublicationPath, Content: string(data)}}))
			if i == 5 {
				fixturekit.Must(t, delivery.RecordOwnPullRequestOutcome(ctx, repo, fixturekit.DemoStart, id, "fixture", publication.PullRequest, delivery.OwnPullRequestOutcomeMerged))
			}
		}
	}
	fixturekit.Must(t, repo.Close())
	s, _ := journey.StartT(t, opts)
	fixturekit.Must(t, service.InspectForTest(s.Core()).SetPause(runtime.Pause{Target: runtime.Target{Scope: "workstream", Project: fixturekit.Project, Workstream: ids[3]}, Mode: "soft", Source: runtime.PauseOwner, Reason: "Owner review", SetAt: fixturekit.DemoStart}))
	p := browsertest.OpenBrowser(t)
	p.Run(chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.Await("the project pane as the entry point", `!document.querySelector('[data-view="project-pane"]').hidden`)
	for i, id := range ids {
		p.AwaitText(`[data-pane-workstream="`+string(id)+`"] [data-field=state]`, labels[i])
	}
	p.Await("one workgroup for each feature and no graph", `document.querySelectorAll('#project-pane [data-workgroup]').length === 7 && !document.querySelector('#project-pane .workgroup-graph')`)
	p.Click(`[data-drilldown="` + string(ids[2]) + `"]`)
	p.Await("the existing workstream view", `!document.querySelector('[data-view="workstream"]').hidden && document.querySelector('#workstream-head').dataset.workstream === `+browsertest.Quote(string(ids[2])))
	p.Await("the existing conversation composer", `document.querySelector('#workstreams .composer textarea') !== null`)
	p.Run(chromedp.Reload())
	p.Await("a direct workstream link survives reload", `!document.querySelector('[data-view="workstream"]').hidden`)
	p.Click("#project-home")
	p.Await("back at projects", `!document.querySelector('[data-view="project-pane"]').hidden`)
	status := s.StatusList()
	if len(status.Workstreams) != len(ids) {
		t.Fatalf("workstreams: %+v", status.Workstreams)
	}
}

func TestBrowserProjectPaneProductionDesignStartAndCrossRepoGraph(t *testing.T) {
	t.Parallel()
	f := journey.NewConfiguredProductionDesignFixture(t, journey.ProductionDesignConfiguration{})
	t.Cleanup(func() { f.Stop(t) })
	f.Stop(t)
	file, err := os.OpenFile(filepath.Join(f.Opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0600)
	fixturekit.Must(t, err)
	_, err = file.WriteString(journey.ListenWeb + "[roles.mason]\nsandbox = \"container\"\nimage = \"fixture-image\"\n")
	fixturekit.Must(t, err)
	fixturekit.Must(t, file.Close())
	f.Opts.PullRequests = &fixturekit.FakePulls{Prs: []pulls.PullRequest{{Number: 123, State: "open", HeadRepository: "owner/dagger", Head: intake.FeatureBranch(f.Stream1)}}, Fork: func() string { return "" }}
	f.Start(t)
	f.Engine.Mu.Lock()
	for _, name := range []string{"mason-accept-implement", "mason-resume-implement"} {
		f.Engine.Turns[name] = func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
	}
	f.Engine.Turns["project-spec-draft-1"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		_, err := fixturekit.CallTool(ctx, tools, lifecycle.ProjectSpecDraftTool, map[string]any{
			"behavior":  "The server acknowledges stored chunk offsets; the client resumes from the acknowledgment.",
			"contracts": []map[string]any{{"name": "acknowledgment", "text": "Keep acknowledged offsets stable."}},
			"graph":     []map[string]any{{"from": string(f.Stream1), "to": string(f.Stream2), "type": "merge_order"}},
			"slices":    []map[string]any{{"workstream": string(f.Stream1), "contract_criteria": []int{2}}, {"workstream": string(f.Stream2), "contract_criteria": []int{1}}},
		})
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Project spec delivered", SessionDir: req.SessionDir, NumTurns: 1}, err
	}
	managerTurn := f.Engine.Turns["*"]
	f.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if strings.Contains(req.Prompt, "Report the shared workgroup goal.") {
			_, err := fixturekit.CallTool(ctx, tools, status.ToolName, map[string]any{"goal": "Ship resumable uploads.", "note": "Both repos share one design.", "agents": []string{}})
			return intakefixture.QuestionResult(req, "pane-chief-status", "Reported the shared goal."), err
		}
		return managerTurn(ctx, req, turn, tools)
	}
	f.Engine.Mu.Unlock()
	p := browsertest.OpenBrowser(t)
	p.Run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+f.S.WebAddr()+"/"))
	p.AwaitText(`[data-project="`+string(f.Project2)+`"] .pane-repos`, "client")
	p.Click(`[data-drilldown="` + string(f.Origin) + `"]`)
	p.Await("the original design chat", `!document.querySelector('[data-view="design"]').hidden`)
	f.SharedDesign().RecordBriefProjects(t, f.Project, f.Project2)
	brief, err := f.C.Design(context.Background(), f.Origin)
	fixturekit.Must(t, err)
	startState := func() string {
		var state string
		p.Eval(`JSON.stringify({revision: document.querySelector('#design-brief').dataset.revision, disabled: document.querySelector('#design-start').disabled, outcome: document.querySelector('#design-start-result').dataset.outcome, text: document.querySelector('#design-start-result').textContent})`, &state)
		return state
	}
	t.Logf("recorded brief revision %d; browser before refresh: %s", brief.Design.BriefRevision, startState())
	projects, err := json.Marshal([]config.ProjectID{f.Project, f.Project2})
	fixturekit.Must(t, err)
	p.Await("the confirmed brief ready to start", `document.querySelector('#design-brief').dataset.revision === `+browsertest.Quote(fmt.Sprint(brief.Design.BriefRevision))+` && JSON.stringify(Array.from(document.querySelectorAll('#design-brief [data-field=projects] li'), li => li.dataset.project)) === `+browsertest.Quote(string(projects))+` && !document.querySelector('#design-start').disabled`)
	p.AwaitText("#design-brief [data-field=projects]", "dagger (home)")
	p.AwaitText("#design-brief [data-field=projects]", "client")
	t.Logf("browser at Start: %s", startState())
	p.Click("#design-start")
	f.Wait(t, "the server accepts the displayed brief", func() bool {
		var result struct{ Outcome, Text string }
		p.Eval(`({Outcome: document.querySelector('#design-start-result').dataset.outcome, Text: document.querySelector('#design-start-result').textContent})`, &result)
		if result.Outcome == "error" || result.Outcome == "unavailable" {
			t.Fatalf("design start %s: %s", result.Outcome, result.Text)
		}
		return result.Outcome == "ok"
	})
	packet := f.AwaitPacket(t)
	originRepo, err := f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	selected, found, err := intake.DesignSelectionOf(originRepo, f.Origin)
	fixturekit.Must(t, err)
	if !found || len(selected.Members) != 2 || selected.Members[0].Repo != f.Project || selected.Members[1].Repo != f.Project2 {
		t.Fatalf("affected-repo selection: %+v", selected)
	}
	_, err = f.C.Send(context.Background(), f.Origin, "Report the shared workgroup goal.")
	fixturekit.Must(t, err)
	f.Wait(t, "both members have the shared goal", func() bool {
		listed, err := f.C.Statuses(context.Background())
		fixturekit.Must(t, err)
		return len(listed.Workstreams) == 2 && listed.Workstreams[0].Status != nil && listed.Workstreams[1].Status != nil &&
			listed.Workstreams[0].Status.Goal == "Ship resumable uploads." && listed.Workstreams[1].Status.Goal == "Ship resumable uploads."
	})
	listed, err := f.C.Statuses(context.Background())
	fixturekit.Must(t, err)
	if len(listed.Workstreams) != 2 {
		t.Fatalf("expected two real members with no design host: %+v", listed.Workstreams)
	}
	for _, w := range listed.Workstreams {
		if w.Workstream != f.Stream1 && w.Workstream != f.Stream2 || w.Workgroup == nil || w.Workgroup.ID != selected.Group ||
			len(w.Workgroup.Members) != 2 || w.Workgroup.Conversation == nil || w.Workgroup.Conversation.Workstream != f.Origin ||
			w.Status == nil || w.Status.Goal != "Ship resumable uploads." {
			t.Fatalf("member presentation: %+v", w)
		}
	}
	p.Run(chromedp.Reload())
	sidebarGroup := `#workstream-list [data-sidebar-workgroup="` + string(selected.Group) + `"]`
	p.Await("both repo-labelled members in one sidebar workgroup", `document.querySelectorAll('#workstream-list > li').length === 1 && document.querySelectorAll(`+browsertest.Quote(sidebarGroup+` [data-select]`)+`).length === 2`)
	p.Await("the design host is absent from the sidebar", `document.querySelector('[data-select="`+string(f.Origin)+`"]') === null`)
	for i, id := range []config.WorkstreamID{f.Stream1, f.Stream2} {
		repoName := "dagger"
		if i == 1 {
			repoName = "client"
		}
		selector := sidebarGroup + ` [data-select="` + string(id) + `"]`
		p.AwaitText(selector+` [data-field=goal]`, repoName)
		p.AwaitText(selector+` [data-field=project]`, repoName)
	}
	_, err = f.C.Ratify(context.Background(), f.Origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	fixturekit.Must(t, err)
	f.AwaitApproved(t)
	p.Await("the continued chat opens its workgroup without a reload", `!document.querySelector('[data-view="workgroup"]').hidden && location.hash === `+browsertest.Quote("#workgroup="+string(selected.Group)))
	p.Click(`[data-open-member="` + string(f.Stream1) + `"]`)
	assertSharedDesignConversation := func() {
		t.Helper()
		var shared api.WorkgroupFeedResponse
		fixturekit.Must(t, f.C.Do(context.Background(), "GET", api.Prefix+"/workgroups/"+string(selected.Group)+"/feed", nil, &shared))
		var found bool
		for _, entry := range shared.Entries {
			if entry.Kind == "response" && strings.Contains(entry.Text, "acknowledged offset") {
				found = true
			}
		}
		if !found {
			t.Fatalf("workgroup feed lost the adopted design reply: %+v", shared.Entries)
		}
		p.OpenActivity()
		p.Await("member activity excludes the shared conversation", `document.querySelector('#workstream-activity [data-kind="transition"]') !== null && !document.querySelector('#workstream-activity [data-kind="message"], #workstream-activity [data-kind="response"], #workstream-activity [data-kind="status"], #workstream-activity [data-kind="action"], #workstream-activity [data-kind="session"][data-role="chief_of_staff"], #workstreams [data-kind="message"], #workstreams [data-kind="response"], #workstreams [data-kind="status"], #workstreams [data-kind="action"]')`)
		p.OpenConversation()
	}
	assertSharedDesignConversation()
	upstream, err := f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	publication := api.DeliveryPublication{Status: delivery.PublicationOpened, Upstream: "dagger/dagger", Fork: "owner/dagger", Branch: intake.FeatureBranch(f.Stream1), PullRequest: 123, URL: "https://example.invalid/dagger/dagger/pull/123"}
	data, err := json.Marshal(publication)
	fixturekit.Must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: delivery.PublicationDocument, Revision: 1, Project: f.Project, Workstream: f.Stream1, At: f.Clock.Now(), Actor: execution.ForemanActor, Cause: "fixture"}
	fixturekit.Must(t, upstream.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: delivery.PublicationPath, Content: string(data)}}))
	h.Schema, h.ID = "osmia.trace.transition", "pane-upstream-published"
	_, err = upstream.SetFeatureState(context.Background(), h, intake.DeliveredState, "Published fixture PR")
	fixturekit.Must(t, err)
	f.Wait(t, "the service's merge wait with the upstream PR", func() bool {
		status, err := f.C.Status(context.Background(), f.Stream2)
		return err == nil && status.Wait == "waiting on owner: dagger/dagger#123 merge"
	})
	p.Run(chromedp.Reload())
	p.Click("#project-home")
	for _, project := range []config.ProjectID{f.Project, f.Project2} {
		card := `#project-pane [data-project="` + string(project) + `"]`
		p.Await("the one-repo project's workgroup graph and edge", `document.querySelectorAll(`+browsertest.Quote(card+` .pane-repos [data-repo]`)+`).length === 1 && document.querySelector(`+browsertest.Quote(card+` .pane-repos [data-repo="`+string(project)+`"]`)+`) !== null && document.querySelectorAll(`+browsertest.Quote(card+` [data-workgroup]`)+`).length === 1 && document.querySelector(`+browsertest.Quote(card+` [data-workgroup="`+string(selected.Group)+`"]`)+`) !== null && document.querySelectorAll(`+browsertest.Quote(card+` .workgroup-graph [data-edge]`)+`).length === 1`)
	}
	p.AwaitText(`[data-pane-workstream="`+string(f.Stream2)+`"] [data-field=state]`, "blocked")
	p.AwaitText(`[data-pane-workstream="`+string(f.Stream2)+`"] [data-field=wait]`, "waiting on owner: dagger/dagger#123 merge")
	inbox, err := f.C.Inbox(context.Background())
	fixturekit.Must(t, err)
	if len(inbox.Entries) != 0 {
		t.Fatalf("merge wait created inbox entries: %+v", inbox.Entries)
	}
	for _, id := range []config.WorkstreamID{f.Stream1, f.Stream2} {
		p.Click(`[data-drilldown="` + string(id) + `"]`)
		p.Await("the member's existing view", `!document.querySelector('[data-view="workstream"]').hidden && document.querySelector('#workstream-head').dataset.workstream === `+browsertest.Quote(string(id)))
		assertSharedDesignConversation()
		p.Click("#project-home")
	}
	var escalated atomic.Bool
	f.Engine.Mu.Lock()
	f.Engine.Turns["pane-ask"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		_, err := fixturekit.CallTool(ctx, tools, questions.AskTool, map[string]any{"question": "May the acknowledged offset contract change?"})
		return intakefixture.QuestionResult(req, "pane-worker", "Asked the chief."), err
	}
	f.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if strings.Contains(req.Prompt, "Present the shared decision") && escalated.CompareAndSwap(false, true) {
			_, err := fixturekit.CallTool(ctx, tools, questions.EscalateTool, map[string]any{
				"questions":  []string{string(f.Stream1) + ":1", string(f.Stream2) + ":1"},
				"rephrasing": "May the acknowledged offset contract change?", "blocked": "Both asking origins.",
				"options": []string{"Keep", "Change"}, "recommendation": "Keep the contract stable.",
			})
			return intakefixture.QuestionResult(req, "pane-chief", "Asked the owner."), err
		}
		return intakefixture.QuestionResult(req, "pane-chief", "Following the agreed design."), nil
	}
	f.Engine.Mu.Unlock()
	lifecycleWorker(t, f, f.Project, f.Stream1, "worker_first", "pane-ask", "Check the contract.")
	lifecycleWorker(t, f, f.Project2, f.Stream2, "worker_second", "pane-ask", "Check the contract.")
	f.Wait(t, "both member questions", func() bool {
		for i, id := range []config.WorkstreamID{f.Stream1, f.Stream2} {
			project := f.Project
			if i == 1 {
				project = f.Project2
			}
			repo, err := f.S.Repository(project)
			fixturekit.Must(t, err)
			qs, err := repo.Questions(id)
			fixturekit.Must(t, err)
			if len(qs) != 1 {
				return false
			}
		}
		return true
	})
	_, err = f.C.Send(context.Background(), f.Stream2, "Present the shared decision")
	fixturekit.Must(t, err)
	f.Wait(t, "one shared inbox decision", func() bool {
		inbox, err := f.C.Inbox(context.Background())
		fixturekit.Must(t, err)
		return len(inbox.Entries) == 1 && inbox.Entries[0].Workgroup == selected.Group
	})
	p.Run(chromedp.Reload())
	p.Click("#project-home")
	for _, project := range []config.ProjectID{f.Project, f.Project2} {
		card := `#project-pane [data-project="` + string(project) + `"]`
		p.Await("one shared decision under the member's project card", `document.querySelectorAll(`+browsertest.Quote(card+` [data-group-inbox]`)+`).length === 1 && document.querySelector(`+browsertest.Quote(card+` [data-group-inbox]`)+`).dataset.groupInbox === `+browsertest.Quote(string(selected.Group)))
		p.AwaitText(card+` [data-group-inbox]`, "1 waiting for you, open inbox")
	}
	p.Click(`#project-pane [data-project="` + string(f.Project2) + `"] [data-group-inbox]`)
	p.Await("the project inbox opens the workgroup", `!document.querySelector('[data-view=workgroup]').hidden && location.hash === `+browsertest.Quote("#workgroup="+string(selected.Group)))
	p.AwaitText("#workgroup-content [data-kind=escalation]", "May the acknowledged offset contract change?")
	p.Click("#project-home")
	p.Click(`[data-drilldown="` + string(f.Stream2) + `"]`)
	p.Await("the member excludes the shared decision and composer", `!document.querySelector('[data-view=workstream]').hidden && !document.querySelector('#workstreams [data-kind=escalation], #workstreams .composer')`)
	p.Click("#workstream-head [data-ask-chief]")
	p.AwaitText("#workgroup-content [data-kind=escalation]", "May the acknowledged offset contract change?")
	p.TypeInto("#workgroup-content [data-kind=escalation] textarea", "Keep the acknowledged offset contract stable.")
	p.Click("#workgroup-content [data-kind=escalation] button[type=submit]")
	f.Wait(t, "the server accepts the shared decision from the workgroup", func() bool {
		var result struct{ Outcome, Text string }
		p.Eval(`({Outcome: document.querySelector('#inbox-result').dataset.outcome, Text: document.querySelector('#inbox-result').textContent})`, &result)
		if result.Outcome == "error" || result.Outcome == "unavailable" {
			t.Fatalf("shared decision answer %s: %s", result.Outcome, result.Text)
		}
		return len(p.OutcomeTexts("#workgroup-content")) == 1 && result.Text == ""
	})
	f.Wait(t, "the shared decision leaves the API inbox", func() bool {
		inbox, err := f.C.Inbox(context.Background())
		fixturekit.Must(t, err)
		return len(inbox.Entries) == 0
	})
}
