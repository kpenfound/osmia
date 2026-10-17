package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
)

func TestBrowserCreateActionsShareFormsAndProjectChoicesAtPhoneWidth(t *testing.T) {
	t.Parallel()
	f := newTwoProjectFixture(t)
	names := map[config.ProjectID]string{project: "Dagger", otherProject: "Osmia"}
	for _, p := range f.cfg.Projects {
		p.Name = names[p.ID]
		must(t, config.WriteProjectConfig(filepath.Join(f.opts.Config.Root, "projects", string(p.ID), "config.toml"), p))
	}
	withWeb(t, f.opts, "127.0.0.1:0")
	clone := filepath.Join(t.TempDir(), "client")
	demoGit(t, filepath.Dir(clone), "init", "-q", clone)
	s, c := start(t, f.opts)
	_, err := c.AddRepo(context.Background(), RepoAddRequest{Project: project, Name: "client", Upstream: "owner/client", Clone: clone})
	must(t, err)
	cfg, err := c.Configuration(context.Background())
	must(t, err)
	var choices [][2]string
	for _, project := range cfg.Projects {
		choices = append(choices, [2]string{string(project.ID), project.Name})
	}
	p := openBrowser(t)
	p.run(chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.await("two projects with three repos", `document.querySelectorAll('#project-pane [data-project]').length === 2 && document.querySelectorAll('#project-pane [data-repo]').length === 3`)
	actions := []struct{ view, name string }{
		{"project-add", "Create project"}, {"repo-add", "Add repo"}, {"handin", "Hand in a feature"},
		{"design-add", "Open a design session"}, {"session-add", "Open an exploratory session"},
	}
	for _, action := range actions {
		view := `[data-view="` + action.view + `"]`
		if action.view != "project-add" {
			p.click("#project-home")
			cardAction := `[data-project="` + string(project) + `"] [data-project-action="` + action.view + `"]`
			p.awaitText(cardAction, action.name)
			p.click(cardAction)
			p.await("card action preselects its project", `!document.querySelector(`+quote(view)+`).hidden && document.querySelector(`+quote(view+` select[name=project]`)+`).value === `+quote(string(project)))
		}
		for _, entry := range []string{"header", "sidebar"} {
			if entry == "header" {
				p.click("#new-workstream")
				p.awaitText(`#create-menu [data-open="`+action.view+`"]`, action.name)
				p.click(`#create-menu [data-open="` + action.view + `"]`)
			} else {
				p.click("#picker-toggle")
				p.awaitText(`#sidebar [data-open="`+action.view+`"]`, action.name)
				p.click(`#sidebar [data-open="` + action.view + `"]`)
			}
			p.await("the shared form and action name", `!document.querySelector(`+quote(view)+`).hidden && document.querySelector(`+quote(view+` h2`)+`).textContent === `+quote(action.name)+` && document.querySelector(`+quote(view+` [type=submit]`)+`).textContent === `+quote(action.name))
			if action.view != "project-add" {
				p.await("global action asks for its project or repo", `document.querySelector(`+quote(view+` select[name=project]`)+`).value === ''`)
				if action.view != "session-add" {
					var got [][2]string
					p.eval(`[...document.querySelector(`+quote(view+` select[name=project]`)+`).options].filter(o => o.value).map(o => [o.value, o.textContent])`, &got)
					if len(got) != 2 || got[0] != choices[0] || got[1] != choices[1] {
						t.Fatalf("%s choices: %v, want %v", action.name, got, choices)
					}
				}
				// A subsequent global action must clear a previous project choice too.
				p.choose(view+` select[name=project]`, string(project))
			}
			p.await("the form fits at phone width", `document.querySelector(`+quote(view+` form`)+`).getBoundingClientRect().right <= innerWidth && document.documentElement.scrollWidth <= innerWidth`)
		}
	}
	p.openView("project-add")
	p.awaitText("#project-add-title", "Create project")
	p.click("#project-home")
	p.click(`[data-project="` + string(project) + `"] [data-project-action="projects"]`)
	p.await("the card's charter entry still opens its charter view", `!document.querySelector('[data-view=projects]').hidden && document.querySelector('#project-edit [name=project]').value === `+quote(string(project)))
	// Global create actions also ignore the workstream currently being viewed.
	p.selectWorkstream(f.streams[project])
	p.click("#new-workstream")
	p.click(`#create-menu [data-open="handin"]`)
	p.await("the workstream does not preselect a global hand-in", `document.querySelector('#handin-form [name=project]').value === ''`)
	writeCharter(t, c, project, "1. Preserve the public API.\n")
	p.click("#project-home")
	p.click(`[data-project="` + string(project) + `"] [data-project-action="design-add"]`)
	p.click("#design-add-form [type=submit]")
	p.awaitText("#design-add-form .result", "Opened design session")
	designs, err := c.Designs(context.Background(), project)
	must(t, err)
	if len(designs.Designs) != 1 || designs.Designs[0].Project != project {
		t.Fatalf("designs must belong to the chosen project: %+v", designs)
	}
}

func TestBrowserExploratorySessionOnNonDefaultRepoUsesConsistentLabels(t *testing.T) {
	t.Parallel()
	s, c, project, _, clone := sessionWebFixture(t)
	ctx := context.Background()
	second := filepath.Join(filepath.Dir(clone), "client")
	demoGit(t, filepath.Dir(clone), "clone", "-q", "--no-hardlinks", clone, second)
	upstream := filepath.Join(filepath.Dir(clone), "remotes", "owner", "client.git")
	must(t, os.MkdirAll(filepath.Dir(upstream), 0700))
	demoGit(t, filepath.Dir(clone), "clone", "-q", "--bare", "--no-hardlinks", second, upstream)
	demoGit(t, filepath.Dir(clone), "-C", second, "remote", "add", "upstream", upstream)
	added, err := c.AddRepo(ctx, RepoAddRequest{Project: project, Name: "client", Upstream: "owner/client", Clone: second})
	must(t, err)
	var repoID config.ProjectID
	for _, repo := range added.Project.Repos {
		if repo.ID != project {
			repoID = repo.ID
		}
	}
	if repoID == "" {
		t.Fatal("missing additional repo")
	}
	const label = "dagger / client"
	p := openBrowser(t)
	p.run(chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	card := `[data-project="` + string(project) + `"]`
	p.awaitText(card+` [data-repo="`+string(repoID)+`"] strong`, label)
	p.click(card + ` [data-project-action="session-add"]`)
	p.await("card selects the default repo", `document.querySelector('#session-add-form [name=project]').value === `+quote(string(project)))
	p.await("every repo is offered with its shared label", `document.querySelector('#session-add-form [name=project]').options.length === 3 && [...document.querySelector('#session-add-form [name=project]').options].some(o => o.value === `+quote(string(repoID))+` && o.textContent === `+quote(label)+`)`)
	p.choose("#session-add-form [name=project]", string(repoID))
	p.choose("#session-add-form [name=profile]", "default")
	p.typeInto("#session-add-form [name=title]", "Explore the client")
	p.click("#session-add-form [type=submit]")
	p.awaitText("#session-add-form .result", "Opened session")
	sessions, err := c.Sessions(ctx, repoID)
	must(t, err)
	if len(sessions.Sessions) != 1 || sessions.Sessions[0].Project != repoID {
		t.Fatalf("sessions on non-default repo: %+v", sessions)
	}
	id := sessions.Sessions[0].Workstream
	p.awaitText(`[data-select="`+string(id)+`"] [data-field=project]`, label)
	p.awaitText("#session-meta [data-field=project]", label)
	p.click("#project-home")
	p.awaitText(card+` [data-sessions=session] [data-pane-workstream="`+string(id)+`"] [data-field=repo]`, label)
	operation := s.StartActivity("repo-label", ActivityCheck, repoID, id, "checking the client")
	t.Cleanup(func() { operation.Finish(nil) })
	p.click("#activity-button")
	p.awaitText(`[data-activity="repo-label"] [data-field=where]`, label)
	p.closePopovers()
	p.openPauses()
	p.await("the repo pause target uses the same label", `[...document.querySelector('#pause-form [name=target]').options].some(o => o.value === `+quote("project:"+string(repoID))+` && o.textContent === `+quote(label)+`)`)
	p.choose("#pause-form [name=target]", "project:"+string(repoID))
	p.typeInto("#pause-form [name=reason]", "Inspect the client")
	p.click("#pause-form [type=submit]")
	p.awaitText("#pause-result", label+" paused")
	p.awaitText(`[data-pause="project:`+string(repoID)+`"] [data-field=scope]`, label)
	rt, err := c.Runtime(ctx)
	must(t, err)
	found := false
	for _, pause := range rt.Effective.Pauses {
		if pause.Target == (runtime.Target{Scope: "project", Project: repoID}) {
			found = true
		}
	}
	if !found {
		t.Fatal("pause did not target the selected repo")
	}
	p.openView("priority")
	p.await("the priority repo uses the same label", `[...document.querySelector('#priority-project').options].some(o => o.value === `+quote(string(repoID))+` && o.textContent === `+quote(label)+`)`)
}
