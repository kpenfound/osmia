package browsertest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestBrowserProjectPaneKeepsFocusedNavigationAcrossStatusUpdates(t *testing.T) {
	t.Parallel()
	f := newPageFixture(t)
	p := openBrowser(t)
	p.Run(chromedp.Navigate("http://" + f.s.WebAddr() + "/"))
	selector := `[data-pane-workstream="` + string(stream) + `"] [data-drilldown]`
	p.AwaitText(selector, "Ship resumable uploads.")
	p.Run(chromedp.Focus(selector, chromedp.ByQuery))
	f.status(t, trace.StatusContent{Goal: "Finish the resumable upload checks.", Note: "Checks are running.", Agents: []string{}})
	p.AwaitText(selector, "Finish the resumable upload checks.")
	var focused bool
	p.Eval(`document.activeElement === document.querySelector(`+quote(selector)+`)`, &focused)
	if !focused {
		t.Fatal("a background status update moved keyboard focus off the workstream link")
	}
	p.Click(selector)
	p.Await("the refreshed link opens its workstream", `!document.querySelector('[data-view="workstream"]').hidden && document.querySelector('#workstream-head').dataset.workstream === `+quote(string(stream)))
}

func TestBrowserProjectPaneCreatesProjectsAndScopesActions(t *testing.T) {
	t.Parallel()
	opts, clone := projectFixture(t)
	withWeb(t, opts, "127.0.0.1:0")
	second := filepath.Join(filepath.Dir(clone), "client")
	must(t, os.MkdirAll(second, 0700))
	demoGit(t, filepath.Dir(second), "init", "-q", second)
	s, _ := start(t, opts)
	p := openBrowser(t)
	p.Run(chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.Click(`[data-view="project-pane"] [data-open="project-add"]`)
	for name, value := range map[string]string{"name": "Uploads", "upstream": "owner/server", "clone": clone} {
		p.TypeInto(`#project-add [name=`+name+`]`, value)
	}
	p.Click("#project-add [type=submit]")
	p.AwaitText("#project-add .result", "Registered Uploads")
	p.Click("#project-home")
	p.AwaitText("#project-pane", "owner/server")
	p.Click(`[data-view="project-pane"] [data-open="project-add"]`)
	p.TypeInto("#project-add [name=name]", "Client")
	p.TypeInto("#project-add [name=upstream]", "owner/client")
	p.TypeInto("#project-add [name=clone]", second)
	p.Click("#project-add [type=submit]")
	p.AwaitText("#project-add .result", "Registered Client")
	p.Await("the successful repo form clears", `document.querySelector('#project-add [name=upstream]').value === ''`)
	p.Click("#project-home")
	p.AwaitText("#project-pane", "owner/client")
	p.Await("two independent projects", `document.querySelectorAll('#project-pane [data-project]').length === 2 && document.querySelectorAll('#project-pane [data-repo]').length === 2`)
	id := service.InspectForTest(s).Config().Projects[0].ID
	for _, view := range []string{"handin", "design-add"} {
		p.Click(`#project-pane [data-project="` + string(id) + `"] [data-project-action="` + view + `"]`)
		p.Await("the action keeps its project", `document.querySelector('[data-view="`+view+`"] select[name=project]').value === `+quote(string(id)))
		p.Click("#project-home")
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
	p.Run(chromedp.Navigate("http://" + s.WebAddr() + "/"))
	p.Await("both projects in the pane", `document.querySelectorAll('#project-pane [data-project]').length === 2`)
	for i, entry := range entries {
		p.Click(`[data-drilldown="` + string(entry.Workstream) + `"]`)
		card := decisionCard(entry)
		p.AwaitText(card, "May this project's contract change?")
		var result struct{ Outcome, Text string }
		p.Eval(`({Outcome: document.querySelector('#inbox-result').dataset.outcome, Text: document.querySelector('#inbox-result').textContent})`, &result)
		if result.Outcome != "" || result.Text != "" {
			t.Fatalf("selecting the project did not clear the previous answer result: %+v", result)
		}
		p.TypeInto(card+"textarea", "Keep this project's contract stable.")
		p.Click(card + "button[type=submit]")
		p.AwaitGone("the answered project decision", card)
		// The card is dismissed optimistically; only the server's answer
		// confirms that the durable inbox can be checked.
		p.Await("the server confirms this project's answer", `(() => {
				const result = document.querySelector('#inbox-result');
				return ['ok', 'error', 'unavailable'].includes(result.dataset.outcome);
			})()`)
		p.Eval(`({Outcome: document.querySelector('#inbox-result').dataset.outcome, Text: document.querySelector('#inbox-result').textContent})`, &result)
		if result.Outcome != "ok" || result.Text != fmt.Sprintf("Answered inbox entry %d.", entry.Number) {
			t.Fatalf("project decision answer %s: %s", result.Outcome, result.Text)
		}
		remaining := entriesOf(t, c, InboxEscalation)
		if len(remaining) != 1-i || i == 0 && remaining[0].Project != entries[1].Project {
			t.Fatalf("answer affected another project: %+v", remaining)
		}
		p.Click("#project-home")
	}
}
