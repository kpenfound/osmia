package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
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
	p.run(chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.await("the project pane as the entry point", `!document.querySelector('[data-view="project-pane"]').hidden`)
	for i, id := range ids {
		p.awaitText(`[data-pane-workstream="`+string(id)+`"] [data-field=state]`, labels[i])
	}
	p.await("one workgroup for each feature and no graph", `document.querySelectorAll('#project-pane [data-workgroup]').length === 7 && !document.querySelector('#project-pane .workgroup-graph')`)
	p.click(`[data-drilldown="` + string(ids[2]) + `"]`)
	p.await("the existing workstream view", `!document.querySelector('[data-view="workstream"]').hidden && document.querySelector('#workstream-head').dataset.workstream === `+quote(string(ids[2])))
	p.await("the existing conversation composer", `document.querySelector('#workstreams .composer textarea') !== null`)
	p.run(chromedp.Reload())
	p.await("a direct workstream link survives reload", `!document.querySelector('[data-view="workstream"]').hidden`)
	p.click("#project-home")
	p.await("back at projects", `!document.querySelector('[data-view="project-pane"]').hidden`)
	status := s.statusList()
	if len(status.Workstreams) != len(ids) {
		t.Fatalf("workstreams: %+v", status.Workstreams)
	}
}

func TestBrowserProjectPaneKeepsFocusedNavigationAcrossStatusUpdates(t *testing.T) {
	t.Parallel()
	f := newPageFixture(t)
	p := openBrowser(t)
	p.run(chromedp.Navigate("http://" + f.s.WebAddr() + "/"))
	selector := `[data-pane-workstream="` + string(stream) + `"] [data-drilldown]`
	p.awaitText(selector, "Ship resumable uploads.")
	p.run(chromedp.Focus(selector, chromedp.ByQuery))
	f.status(t, trace.StatusContent{Goal: "Finish the resumable upload checks.", Note: "Checks are running.", Agents: []string{}})
	p.awaitText(selector, "Finish the resumable upload checks.")
	var focused bool
	p.eval(`document.activeElement === document.querySelector(`+quote(selector)+`)`, &focused)
	if !focused {
		t.Fatal("a background status update moved keyboard focus off the workstream link")
	}
	p.click(selector)
	p.await("the refreshed link opens its workstream", `!document.querySelector('[data-view="workstream"]').hidden && document.querySelector('#workstream-head').dataset.workstream === `+quote(string(stream)))
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
	f.opts.PullRequests = &fakePulls{prs: []pulls.PullRequest{{Number: 123, State: "open", HeadRepository: "owner/dagger", Head: featureBranch(f.stream1)}}, fork: func() string { return "" }}
	f.start(t)
	f.engine.mu.Lock()
	// Ordinary builders remain in their fake turns while this browser test
	// exercises presentation and the shared decision path.
	for _, name := range []string{"mason-accept-implement", "mason-resume-implement"} {
		f.engine.turns[name] = func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
	}
	f.engine.turns["project-spec-draft-1"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		_, err := callTool(ctx, tools, ProjectSpecDraftTool, map[string]any{
			"behavior":  "The server acknowledges stored chunk offsets; the client resumes from the acknowledgment.",
			"contracts": []map[string]any{{"name": "acknowledgment", "text": "Keep acknowledged offsets stable."}},
			"graph":     []map[string]any{{"from": string(f.stream1), "to": string(f.stream2), "type": "merge_order"}},
			"slices":    []map[string]any{{"workstream": string(f.stream1), "contract_criteria": []int{2}}, {"workstream": string(f.stream2), "contract_criteria": []int{1}}},
		})
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Project spec delivered", SessionDir: req.SessionDir, NumTurns: 1}, err
	}
	f.engine.mu.Unlock()
	p := openBrowser(t)
	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.awaitText(`[data-project="`+string(f.project)+`"] .pane-repos`, "client")
	p.click(`[data-drilldown="` + string(f.origin) + `"]`)
	p.await("the original design chat", `!document.querySelector('[data-view="design"]').hidden`)
	p.click("#design-start")
	packet := f.awaitPacket(t)
	_, err = f.c.Ratify(context.Background(), f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan)
	must(t, err)
	f.awaitApproved(t)
	p.await("the continued chat opens its member without a reload", `!document.querySelector('[data-view="workstream"]').hidden && [`+quote(string(f.stream1))+`,`+quote(string(f.stream2))+`].includes(document.querySelector('#workstream-head').dataset.workstream)`)
	p.awaitText("#workstreams", "acknowledged offset")
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
	p.run(chromedp.Reload())
	p.click("#project-home")
	p.await("one cross-repo graph with its edge", `document.querySelectorAll('#project-pane [data-workgroup]').length === 1 && document.querySelectorAll('#project-pane .workgroup-graph [data-edge]').length > 0`)
	p.awaitText(`[data-pane-workstream="`+string(f.stream2)+`"] [data-field=state]`, "blocked")
	p.awaitText(`[data-pane-workstream="`+string(f.stream2)+`"] [data-field=wait]`, "waiting on owner: dagger/dagger#123 merge")
	inbox, err := f.c.Inbox(context.Background())
	must(t, err)
	if len(inbox.Entries) != 0 {
		t.Fatalf("merge wait created inbox entries: %+v", inbox.Entries)
	}
	for _, id := range []config.WorkstreamID{f.stream1, f.stream2} {
		p.click(`[data-drilldown="` + string(id) + `"]`)
		p.await("the member's existing view", `!document.querySelector('[data-view="workstream"]').hidden && document.querySelector('#workstream-head').dataset.workstream === `+quote(string(id)))
		p.awaitText("#workstreams", "acknowledged offset")
		p.click("#project-home")
	}
	// A single shared inbox entry is offered from either member using the
	// workgroup's original project/number pins, even from the second repo.
	var escalated atomic.Bool
	f.engine.mu.Lock()
	f.engine.turns["pane-ask"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		_, err := callTool(ctx, tools, questions.AskTool, map[string]any{"question": "May the acknowledged offset contract change?"})
		return questionResult(req, "pane-worker", "Asked the chief."), err
	}
	f.engine.turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
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
	f.engine.mu.Unlock()
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
	p.run(chromedp.Reload())
	p.click("#project-home")
	p.await("one shared decision in the project pane", `document.querySelectorAll('[data-group-inbox]').length === 1`)
	p.click(`[data-group-inbox]`)
	p.awaitText("#workstreams [data-kind=escalation]", "May the acknowledged offset contract change?")
	p.click("#project-home")
	p.click(`[data-drilldown="` + string(f.stream2) + `"]`)
	p.awaitText("#workstreams [data-kind=escalation]", "May the acknowledged offset contract change?")
	p.typeInto("#workstreams [data-kind=escalation] textarea", "Keep the acknowledged offset contract stable.")
	p.click("#workstreams [data-kind=escalation] button[type=submit]")
	f.wait(t, "the server accepts the shared decision from the second repo", func() bool {
		var result struct{ Outcome, Text string }
		p.eval(`({Outcome: document.querySelector('#inbox-result').dataset.outcome, Text: document.querySelector('#inbox-result').textContent})`, &result)
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

func TestBrowserProjectPaneCreatesProjectAddsRepoAndScopesActions(t *testing.T) {
	t.Parallel()
	opts, clone := projectFixture(t)
	withWeb(t, opts, "127.0.0.1:0")
	second := filepath.Join(filepath.Dir(clone), "client")
	must(t, os.MkdirAll(second, 0700))
	demoGit(t, filepath.Dir(second), "init", "-q", second)
	s, _ := start(t, opts)
	p := openBrowser(t)
	p.run(chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.click(`[data-view="project-pane"] [data-open="project-add"]`)
	for name, value := range map[string]string{"name": "Uploads", "upstream": "owner/server", "clone": clone} {
		p.typeInto(`#project-add [name=`+name+`]`, value)
	}
	p.click("#project-add [type=submit]")
	p.awaitText("#project-add .result", "Registered Uploads")
	p.click("#project-home")
	p.awaitText("#project-pane", "owner/server")
	p.click(`#project-pane [data-project-action="repo-add"]`)
	p.typeInto("#repo-add [name=upstream]", "owner/client")
	p.typeInto("#repo-add [name=clone]", second)
	p.click("#repo-add [type=submit]")
	p.awaitText("#repo-add .result", "Added repo to Uploads")
	p.await("the successful repo form clears", `document.querySelector('#repo-add [name=upstream]').value === ''`)
	p.click("#project-home")
	p.awaitText("#project-pane", "owner/client")
	p.await("both repos in one project", `document.querySelectorAll('#project-pane [data-project]').length === 1 && document.querySelectorAll('#project-pane [data-repo]').length === 2`)
	id := s.current().Projects[0].ID
	for _, view := range []string{"handin", "design-add"} {
		p.click(`#project-pane [data-project-action="` + view + `"]`)
		p.await("the action keeps its project", `document.querySelector('[data-view="`+view+`"] select[name=project]').value === `+quote(string(id)))
		p.click("#project-home")
	}
}

// Repo-local inbox numbers can be equal in different projects. Opening and
// answering either project's decision must preserve the other one's pins.
func TestBrowserProjectPaneKeepsProjectInboxDecisionsSeparate(t *testing.T) {
	t.Parallel()
	f := newTwoProjectFixture(t)
	f.opts.Threads = func(*trace.Repository, *config.Config) (coreadapter.Reconciler, error) {
		return &completedRunner{}, nil
	}
	withWeb(t, f.opts, "127.0.0.1:0")
	ctx := context.Background()
	// Seed durable decisions before background chief-event delivery can
	// enqueue turns ahead of the fixture's manually claimed chief turns.
	for _, id := range []config.ProjectID{project, otherProject} {
		repo, err := trace.Open(f.cfg.Root, f.cfg.For(id).Project)
		must(t, err)
		t.Cleanup(func() { repo.Close() })
		ws := f.streams[id]
		_, err = repo.EnsureChiefOfStaff(ctx, ws, demoStart, ownerActor)
		must(t, err)
		chief, err := repo.ChiefOfStaffThread(ws)
		must(t, err)
		claim := func(agent, thread, turn string) coreadapter.Scope {
			t.Helper()
			h := trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: id, Workstream: ws, At: demoStart, Actor: ownerActor, Cause: "fixture"}
			_, err := repo.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: agent, ThreadID: thread, TurnID: turn, Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Check the project contract."})
			must(t, err)
			_, err = repo.ClaimTurn(ctx, ws, agent, "token_"+turn, t.TempDir(), demoStart)
			must(t, err)
			return coreadapter.Scope{Project: string(id), Workstream: string(ws), Thread: thread, Turn: turn, Role: demoRole}
		}
		h := trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, ID: "pane_worker", Revision: 1, Project: id, Workstream: ws, At: demoStart, Actor: ownerActor, Cause: "fixture"}
		must(t, repo.CreateThread(ctx, trace.Agent{Header: h, Role: demoRole, ThreadID: "pane_worker_thread"}))
		q, err := repo.Ask(ctx, "pane_worker", claim("pane_worker", "pane_worker_thread", "pane_worker_turn"), "May this project's contract change?", demoStart)
		must(t, err)
		chiefScope := claim(trace.ChiefOfStaff, chief.Identity.ThreadID, "pane_chief_turn")
		chiefScope.Role = trace.ChiefOfStaff
		_, err = repo.EscalateQuestions(ctx, trace.ChiefOfStaff, chiefScope, trace.EscalationRequest{Questions: []string{q.ID}, Rephrasing: "May this project's contract change?", Blocked: "The asking worker.", Recommendation: "Keep the contract."}, demoStart)
		must(t, err)
		must(t, repo.Close())
	}
	s, c := start(t, f.opts)
	entries := entriesOf(t, c, InboxEscalation)
	if len(entries) != 2 || entries[0].Number != entries[1].Number || entries[0].Project == entries[1].Project {
		t.Fatalf("expected separate project decisions with equal numbers: %+v", entries)
	}
	p := openBrowser(t)
	p.run(chromedp.Navigate("http://" + s.WebAddr() + "/"))
	p.await("both projects in the pane", `document.querySelectorAll('#project-pane [data-project]').length === 2`)
	for i, entry := range entries {
		p.click(`[data-drilldown="` + string(entry.Workstream) + `"]`)
		card := decisionCard(entry)
		p.awaitText(card, "May this project's contract change?")
		var result struct{ Outcome, Text string }
		p.eval(`({Outcome: document.querySelector('#inbox-result').dataset.outcome, Text: document.querySelector('#inbox-result').textContent})`, &result)
		if result.Outcome != "" || result.Text != "" {
			t.Fatalf("selecting the project did not clear the previous answer result: %+v", result)
		}
		p.typeInto(card+"textarea", "Keep this project's contract stable.")
		p.click(card + "button[type=submit]")
		p.awaitGone("the answered project decision", card)
		// The card is dismissed optimistically; only the server's answer
		// confirms that the durable inbox can be checked.
		p.await("the server confirms this project's answer", `(() => {
				const result = document.querySelector('#inbox-result');
				return ['ok', 'error', 'unavailable'].includes(result.dataset.outcome);
			})()`)
		p.eval(`({Outcome: document.querySelector('#inbox-result').dataset.outcome, Text: document.querySelector('#inbox-result').textContent})`, &result)
		if result.Outcome != "ok" || result.Text != fmt.Sprintf("Answered inbox entry %d.", entry.Number) {
			t.Fatalf("project decision answer %s: %s", result.Outcome, result.Text)
		}
		remaining := entriesOf(t, c, InboxEscalation)
		if len(remaining) != 1-i || i == 0 && remaining[0].Project != entries[1].Project {
			t.Fatalf("answer affected another project: %+v", remaining)
		}
		p.click("#project-home")
	}
}
