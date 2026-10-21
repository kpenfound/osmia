package lifecycle_test

import (
	context "context"
	json "encoding/json"
	fmt "fmt"
	chromedp "github.com/chromedp/chromedp"
	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	pulls "github.com/kpenfound/osmia/internal/pulls"
	questions "github.com/kpenfound/osmia/internal/questions"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	status "github.com/kpenfound/osmia/internal/status"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
	os "os"
	filepath "path/filepath"
	strings "strings"
	atomic "sync/atomic"
	testing "testing"
)

// State fixtures record the facts read by the production status endpoint;
// they run no model, publication or remote operation.
func TestBrowserProjectPaneStatesAndSingleMemberDrilldown(t *testing.T) {
	t.Parallel()
	opts, repo := sidebarFixture(t)
	ctx := context.Background()
	states := []string{HandedState, RatifiedState, BuildingState, BuildingState, DeliveredState, DeliveredState, AbandonedState}
	labels := []string{"planning", "ready", "building", "paused", "PR open", "delivered (merged)", "cancelled"}
	var ids []config.WorkstreamID
	for i, state := range states {
		id := config.WorkstreamID(fmt.Sprintf("w_%032x", i+200))
		ids = append(ids, id)
		must(t, repo.CreateWorkstream(ctx, id, demoStart, ownerActor))
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "pane-state", Revision: 1, Project: repo.Project(), Workstream: id, At: demoStart, Actor: ownerActor, Cause: "fixture"}
		_, err := repo.SetFeatureState(ctx, h, state, "Project pane fixture")
		must(t, err)
		if i == 4 || i == 5 {
			publication := DeliveryPublication{Status: publicationOpened, PullRequest: i + 100, URL: fmt.Sprintf("https://example.invalid/pull/%d", i+100)}
			data, err := json.Marshal(publication)
			must(t, err)
			h.Schema, h.ID = "osmia.trace.document", publicationDocument
			must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: h, Path: publicationPath, Content: string(data)}}))
			if i == 5 {
				must(t, recordOwnPullRequestOutcome(ctx, repo, demoStart, id, "fixture", publication.PullRequest, ownPullRequestOutcomeMerged))
			}
		}
	}
	must(t, repo.Close())
	s, _ := start(t, opts)
	must(t, s.setPause(runtime.Pause{Target: runtime.Target{Scope: "workstream", Project: project, Workstream: ids[3]}, Mode: "soft", Source: runtime.PauseOwner, Reason: "Owner review", SetAt: demoStart}))
	p := openBrowser(t)
	p.Run(chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.Await("the project pane as the entry point", `!document.querySelector('[data-view="project-pane"]').hidden`)
	for i, id := range ids {
		p.AwaitText(`[data-pane-workstream="`+string(id)+`"] [data-field=state]`, labels[i])
	}
	p.Await("one workgroup for each feature and no graph", `document.querySelectorAll('#project-pane [data-workgroup]').length === 7 && !document.querySelector('#project-pane .workgroup-graph')`)
	p.Click(`[data-drilldown="` + string(ids[2]) + `"]`)
	p.Await("the existing workstream view", `!document.querySelector('[data-view="workstream"]').hidden && document.querySelector('#workstream-head').dataset.workstream === `+quote(string(ids[2])))
	p.Await("the existing conversation composer", `document.querySelector('#workstreams .composer textarea') !== null`)
	p.Run(chromedp.Reload())
	p.Await("a direct workstream link survives reload", `!document.querySelector('[data-view="workstream"]').hidden`)
	p.Click("#project-home")
	p.Await("back at projects", `!document.querySelector('[data-view="project-pane"]').hidden`)
	status := s.statusList()
	if len(status.Workstreams) != len(ids) {
		t.Fatalf("workstreams: %+v", status.Workstreams)
	}
}

// This drives the one existing owner start from the actual design chat,
// then follows each allocated member to the adopted shared conversation.
func TestBrowserProjectPaneProductionDesignStartAndCrossRepoGraph(t *testing.T) {
	t.Parallel()
	f := newProductionDesignFixture(t)
	t.Cleanup(func() { f.stop(t) })
	f.stop(t)
	file, err := os.OpenFile(filepath.Join(f.opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0600)
	must(t, err)
	_, err = file.WriteString(listenWeb + "[roles.mason]\nsandbox = \"container\"\nimage = \"fixture-image\"\n")
	must(t, err)
	must(t, file.Close())
	f.opts.PullRequests = &fakePulls{Prs: []pulls.PullRequest{{Number: 123, State: "open", HeadRepository: "owner/dagger", Head: featureBranch(f.stream1)}}, Fork: func() string { return "" }}
	f.start(t)
	f.engine.Mu.Lock()
	// Ordinary builders remain in their fake turns while this browser test
	// exercises presentation and the shared decision path.
	for _, name := range []string{"mason-accept-implement", "mason-resume-implement"} {
		f.engine.Turns[name] = func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
	}
	f.engine.Turns["project-spec-draft-1"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		_, err := callTool(ctx, tools, ProjectSpecDraftTool, map[string]any{
			"behavior":  "The server acknowledges stored chunk offsets; the client resumes from the acknowledgment.",
			"contracts": []map[string]any{{"name": "acknowledgment", "text": "Keep acknowledged offsets stable."}},
			"graph":     []map[string]any{{"from": string(f.stream1), "to": string(f.stream2), "type": "merge_order"}},
			"slices":    []map[string]any{{"workstream": string(f.stream1), "contract_criteria": []int{2}}, {"workstream": string(f.stream2), "contract_criteria": []int{1}}},
		})
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Project spec delivered", SessionDir: req.SessionDir, NumTurns: 1}, err
	}
	chiefTurn := f.engine.Turns["*"]
	f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if strings.Contains(req.Prompt, "Report the shared workgroup goal.") {
			_, err := callTool(ctx, tools, status.ToolName, map[string]any{"goal": "Ship resumable uploads.", "note": "Both repos share one design.", "agents": []string{}})
			return questionResult(req, "pane-chief-status", "Reported the shared goal."), err
		}
		return chiefTurn(ctx, req, turn, tools)
	}
	f.engine.Mu.Unlock()
	p := openBrowser(t)
	p.Run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.AwaitText(`[data-project="`+string(f.project)+`"] .pane-repos`, "client")
	p.Click(`[data-drilldown="` + string(f.origin) + `"]`)
	p.Await("the original design chat", `!document.querySelector('[data-view="design"]').hidden`)
	p.Click("#design-start")
	packet := f.awaitPacket(t)
	// Both selected repos allocate real members. Their shared chief gives
	// them the same goal; the adopted design origin is a conversation host.
	originRepo, err := f.s.repository(f.project)
	must(t, err)
	selected, found, err := designSelectionOf(originRepo, f.origin)
	must(t, err)
	if !found || len(selected.Members) != 2 || selected.Members[0].Repo != f.project || selected.Members[1].Repo != f.project2 {
		t.Fatalf("affected-repo selection: %+v", selected)
	}
	_, err = f.c.Send(context.Background(), f.origin, "Report the shared workgroup goal.")
	must(t, err)
	f.wait(t, "both members have the shared goal", func() bool {
		listed, err := f.c.Statuses(context.Background())
		must(t, err)
		return len(listed.Workstreams) == 2 && listed.Workstreams[0].Status != nil && listed.Workstreams[1].Status != nil &&
			listed.Workstreams[0].Status.Goal == "Ship resumable uploads." && listed.Workstreams[1].Status.Goal == "Ship resumable uploads."
	})
	listed, err := f.c.Statuses(context.Background())
	must(t, err)
	if len(listed.Workstreams) != 2 {
		t.Fatalf("expected two real members with no design host: %+v", listed.Workstreams)
	}
	for _, w := range listed.Workstreams {
		if w.Workstream != f.stream1 && w.Workstream != f.stream2 || w.Workgroup == nil || w.Workgroup.ID != selected.Group ||
			len(w.Workgroup.Members) != 2 || w.Workgroup.Conversation == nil || w.Workgroup.Conversation.Workstream != f.origin ||
			w.Status == nil || w.Status.Goal != "Ship resumable uploads." {
			t.Fatalf("member presentation: %+v", w)
		}
	}
	p.Run(chromedp.Reload())
	sidebarGroup := `#workstream-list [data-sidebar-workgroup="` + string(selected.Group) + `"]`
	p.Await("both same-titled members in one sidebar workgroup", `document.querySelectorAll('#workstream-list > li').length === 1 && document.querySelectorAll(`+quote(sidebarGroup+` [data-select]`)+`).length === 2`)
	p.Await("the design host is absent from the sidebar", `document.querySelector('[data-select="`+string(f.origin)+`"]') === null`)
	for i, id := range []config.WorkstreamID{f.stream1, f.stream2} {
		repoName := "dagger"
		if i == 1 {
			repoName = "client"
		}
		selector := sidebarGroup + ` [data-select="` + string(id) + `"]`
		p.AwaitText(selector+` [data-field=goal]`, "Ship resumable uploads.")
		p.AwaitText(selector+` [data-field=project]`, "dagger / "+repoName)
	}
	_, err = f.c.Ratify(context.Background(), f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	must(t, err)
	f.awaitApproved(t)
	p.Await("the continued chat opens its member without a reload", `!document.querySelector('[data-view="workstream"]').hidden && [`+quote(string(f.stream1))+`,`+quote(string(f.stream2))+`].includes(document.querySelector('#workstream-head').dataset.workstream)`)
	p.AwaitText("#workstreams", "acknowledged offset")
	upstream, err := f.s.repository(f.project)
	must(t, err)
	publication := DeliveryPublication{Status: publicationOpened, Upstream: "dagger/dagger", Fork: "owner/dagger", Branch: featureBranch(f.stream1), PullRequest: 123, URL: "https://example.invalid/dagger/dagger/pull/123"}
	data, err := json.Marshal(publication)
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: publicationDocument, Revision: 1, Project: f.project, Workstream: f.stream1, At: f.clock.Now(), Actor: foremanActor, Cause: "fixture"}
	must(t, upstream.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: publicationPath, Content: string(data)}}))
	h.Schema, h.ID = "osmia.trace.transition", "pane-upstream-published"
	_, err = upstream.SetFeatureState(context.Background(), h, DeliveredState, "Published fixture PR")
	must(t, err)
	f.wait(t, "the service's merge wait with the upstream PR", func() bool {
		status, err := f.c.Status(context.Background(), f.stream2)
		return err == nil && status.Wait == "waiting on owner: dagger/dagger#123 merge"
	})
	p.Run(chromedp.Reload())
	p.Click("#project-home")
	p.Await("one cross-repo graph with its edge", `document.querySelectorAll('#project-pane [data-workgroup]').length === 1 && document.querySelectorAll('#project-pane .workgroup-graph [data-edge]').length > 0`)
	p.AwaitText(`[data-pane-workstream="`+string(f.stream2)+`"] [data-field=state]`, "blocked")
	p.AwaitText(`[data-pane-workstream="`+string(f.stream2)+`"] [data-field=wait]`, "waiting on owner: dagger/dagger#123 merge")
	inbox, err := f.c.Inbox(context.Background())
	must(t, err)
	if len(inbox.Entries) != 0 {
		t.Fatalf("merge wait created inbox entries: %+v", inbox.Entries)
	}
	for _, id := range []config.WorkstreamID{f.stream1, f.stream2} {
		p.Click(`[data-drilldown="` + string(id) + `"]`)
		p.Await("the member's existing view", `!document.querySelector('[data-view="workstream"]').hidden && document.querySelector('#workstream-head').dataset.workstream === `+quote(string(id)))
		p.AwaitText("#workstreams", "acknowledged offset")
		p.Click("#project-home")
	}
	// A single shared inbox entry is offered from either member using the
	// workgroup's original project/number pins, even from the second repo.
	var escalated atomic.Bool
	f.engine.Mu.Lock()
	f.engine.Turns["pane-ask"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		_, err := callTool(ctx, tools, questions.AskTool, map[string]any{"question": "May the acknowledged offset contract change?"})
		return questionResult(req, "pane-worker", "Asked the chief."), err
	}
	f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if strings.Contains(req.Prompt, "Present the shared decision") && escalated.CompareAndSwap(false, true) {
			_, err := callTool(ctx, tools, questions.EscalateTool, map[string]any{
				"questions":  []string{string(f.stream1) + ":1", string(f.stream2) + ":1"},
				"rephrasing": "May the acknowledged offset contract change?", "blocked": "Both asking origins.",
				"options": []string{"Keep", "Change"}, "recommendation": "Keep the contract stable.",
			})
			return questionResult(req, "pane-chief", "Asked the owner."), err
		}
		return questionResult(req, "pane-chief", "Following the agreed design."), nil
	}
	f.engine.Mu.Unlock()
	lifecycleWorker(t, f, f.project, f.stream1, "worker_first", "pane-ask", "Check the contract.")
	lifecycleWorker(t, f, f.project2, f.stream2, "worker_second", "pane-ask", "Check the contract.")
	f.wait(t, "both member questions", func() bool {
		for i, id := range []config.WorkstreamID{f.stream1, f.stream2} {
			project := f.project
			if i == 1 {
				project = f.project2
			}
			repo, err := f.s.repository(project)
			must(t, err)
			qs, err := repo.Questions(id)
			must(t, err)
			if len(qs) != 1 {
				return false
			}
		}
		return true
	})
	_, err = f.c.Send(context.Background(), f.stream2, "Present the shared decision")
	must(t, err)
	f.wait(t, "one shared inbox decision", func() bool {
		inbox, err := f.c.Inbox(context.Background())
		must(t, err)
		return len(inbox.Entries) == 1 && inbox.Entries[0].Workgroup != ""
	})
	p.Run(chromedp.Reload())
	p.Click("#project-home")
	p.Await("one shared decision in the project pane", `document.querySelectorAll('[data-group-inbox]').length === 1`)
	p.Click(`[data-group-inbox]`)
	p.AwaitText("#workstreams [data-kind=escalation]", "May the acknowledged offset contract change?")
	p.Click("#project-home")
	p.Click(`[data-drilldown="` + string(f.stream2) + `"]`)
	p.AwaitText("#workstreams [data-kind=escalation]", "May the acknowledged offset contract change?")
	p.TypeInto("#workstreams [data-kind=escalation] textarea", "Keep the acknowledged offset contract stable.")
	p.Click("#workstreams [data-kind=escalation] button[type=submit]")
	f.wait(t, "the server accepts the shared decision from the second repo", func() bool {
		var result struct{ Outcome, Text string }
		p.Eval(`({Outcome: document.querySelector('#inbox-result').dataset.outcome, Text: document.querySelector('#inbox-result').textContent})`, &result)
		if result.Outcome == "error" || result.Outcome == "unavailable" {
			t.Fatalf("shared decision answer %s: %s", result.Outcome, result.Text)
		}
		return result.Outcome == "ok"
	})
	f.wait(t, "the shared decision leaves the API inbox", func() bool {
		inbox, err := f.c.Inbox(context.Background())
		must(t, err)
		return len(inbox.Entries) == 0
	})
}
