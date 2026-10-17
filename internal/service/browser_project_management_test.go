package service

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	cdppage "github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/config"
)

// observeManagementConfig records config events on the page's own live
// connection, so periodic configuration reads cannot mask a missing event.
func observeManagementConfig(p *page) {
	p.t.Helper()
	p.run(chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := cdppage.AddScriptToEvaluateOnNewDocument(`
          window.managementConfigEvents = [];
          const ServiceEvents = window.EventSource;
          window.EventSource = class extends ServiceEvents {
            constructor(...args) {
              super(...args);
              this.addEventListener('config', event => managementConfigEvents.push(JSON.parse(event.data)));
            }
          };`).Do(ctx)
		return err
	}))
}

// recordManagementWrites observes browser requests without replacing their
// responses: every write still goes to the running service.
func recordManagementWrites(p *page) {
	p.t.Helper()
	p.eval(`window.managementWrites = [];
      window.sameDocument = true;
      const serviceFetch = window.fetch;
      window.fetch = function(path, init) {
        if (init && init.method !== 'GET') managementWrites.push({path, method: init.method, body: JSON.parse(init.body || '{}')});
        return serviceFetch.apply(this, arguments);
      };`, nil)
}

func managementOutcome(p *page, selector, outcome string) {
	p.t.Helper()
	p.await("management action completed with "+outcome, `document.querySelector(`+quote(selector)+`)?.dataset.outcome === `+quote(outcome))
}

// projectChoicesInclude checks all project/repo choosers, including those in
// forms that are not currently open. Their choices must stay live too.
func projectChoicesInclude(p *page, project, repo config.ProjectID, repoPresent bool, projects int) {
	p.t.Helper()
	p.await("live project and repo choices", `(() => {
      const project = `+quote(string(project))+`, repo = `+quote(string(repo))+`;
      return [...document.querySelectorAll('[data-project-select]')].every(s =>
        [...s.options].filter(o => o.value).length === `+strconv.Itoa(projects)+` && [...s.options].some(o => o.value === project) &&
        (repo === project || ![...s.options].some(o => o.value === repo))) &&
        [...document.querySelectorAll('[data-repo-select], #priority-project')].every(s =>
          [...s.options].some(o => o.value === repo) === `+strconv.FormatBool(repoPresent)+`) &&
        [...document.querySelector('#pause-form [name=target]').options].some(o => o.value === 'project:' + repo) === `+strconv.FormatBool(repoPresent)+`;
    })()`)
}

func TestBrowserProjectManagementWholePathAtPhoneWidth(t *testing.T) {
	t.Parallel()
	s, c, project, _, clone := sessionWebFixture(t)
	ctx := context.Background()
	second := filepath.Join(filepath.Dir(clone), "client")
	demoGit(t, filepath.Dir(clone), "clone", "-q", "--no-hardlinks", clone, second)
	// The local remote names the configured owner/repo, as session opening
	// resolves remotes by their URLs before fetching the base branch.
	upstream := filepath.Join(filepath.Dir(clone), "remotes", "owner", "client.git")
	must(t, os.MkdirAll(filepath.Dir(upstream), 0700))
	demoGit(t, filepath.Dir(clone), "clone", "-q", "--bare", "--no-hardlinks", second, upstream)
	demoGit(t, filepath.Dir(clone), "-C", second, "remote", "add", "upstream", upstream)
	p := openBrowser(t)
	p.run(chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	card := `#project-pane [data-project="` + string(project) + `"]`
	manage := card + ` [data-project-action=projects]`
	p.awaitText(manage, "Project management")
	recordManagementWrites(p)
	p.click(manage)
	p.await("management opened on its project", `!document.querySelector('[data-view=projects]').hidden && document.querySelector('#project-edit [name=project]').value === `+quote(string(project)))
	p.awaitText("#charter-state", "Ready for hand-in")
	p.click("#project-add-repo")
	p.await("shared add-repo form with project chosen", `!document.querySelector('[data-view=repo-add]').hidden && document.querySelector('#repo-add [name=project]').value === `+quote(string(project)))
	for name, value := range map[string]string{"name": "client", "upstream": "owner/client", "fork": "owner/client-fork", "clone": second} {
		p.typeInto("#repo-add [name="+name+"]", value)
	}
	p.click("#repo-add [type=submit]")
	managementOutcome(p, "#repo-add .result", "ok")
	cfg, err := c.Configuration(ctx)
	must(t, err)
	var repoID config.ProjectID
	for _, repo := range cfg.Projects[0].Repos {
		if repo.ID != project {
			repoID = repo.ID
		}
	}
	if repoID == "" {
		t.Fatal("add-repo form did not register a second repo")
	}
	projectChoicesInclude(p, project, repoID, true, 1)
	p.awaitText(card+` [data-repo="`+string(repoID)+`"]`, "dagger / client")
	p.click("#project-home")
	p.click(manage)
	row := `#project-repos [data-managed-repo="` + string(repoID) + `"]`
	defaultRow := `#project-repos [data-managed-repo="` + string(project) + `"]`
	for _, text := range []string{"dagger / client", "client", "owner/client", "owner/client-fork", second, "main"} {
		p.awaitText(row, text)
	}
	p.awaitText(defaultRow, "Default repo")
	p.await("management fits at phone width", `document.documentElement.scrollWidth <= innerWidth && document.querySelector('#project-repos').getBoundingClientRect().right <= innerWidth`)
	p.click("#project-edit [data-action=read-charter]")
	managementOutcome(p, "#project-edit .result", "ok")
	read, err := c.Charter(ctx, project)
	must(t, err)
	const content = "1. Preserve the public API.\n2. Keep the client compatible.\n"
	p.setValue("#project-edit [name=content]", content)
	p.click("#project-edit [type=submit]")
	managementOutcome(p, "#project-edit .result", "ok")
	saved, err := c.Charter(ctx, project)
	must(t, err)
	if saved.Content != content || saved.Revision != read.Revision+1 {
		t.Fatalf("charter saved through management: %+v", saved)
	}
	p.await("charter save used the revision read", `managementWrites.some(r => r.method === 'PUT' && r.path === `+quote(Prefix+"/projects/charter/"+string(project))+` && r.body.revision === `+strconv.Itoa(read.Revision)+`)`)
	p.awaitText("#charter-state", "2 rules")

	// Registration requests one initial extraction per repo. Observe both
	// terminal outcomes before asking for another one on the client only.
	for _, selector := range []string{row, defaultRow} {
		p.await("initial extraction finished", `/Knowledge extraction 1: (succeeded|failed)/.test(document.querySelector(`+quote(selector+` [data-field=extraction]`)+`).textContent)`)
	}
	p.click(row + ` [data-action=extract]`)
	managementOutcome(p, row+" .result", "ok")
	p.awaitText(row+` [data-field=extraction]`, "Knowledge extraction 2:")
	cfg, err = c.Configuration(ctx)
	must(t, err)
	for _, repo := range cfg.Projects[0].Repos {
		want := 1
		if repo.ID == repoID {
			want = 2
		}
		if repo.Extraction == nil || repo.Extraction.Extraction != want {
			t.Fatalf("extraction must target only client: %+v", repo)
		}
	}
	p.click(row + ` [data-action=rebase]`)
	managementOutcome(p, row+" .result", "ok")
	p.click("#project-remove")
	managementOutcome(p, "#project-result", "error")
	p.awaitText("#project-result", "still has other repos")
	cfg, err = c.Configuration(ctx)
	must(t, err)
	if len(cfg.Projects) != 1 || len(cfg.Projects[0].Repos) != 2 {
		t.Fatalf("refused project removal changed registrations: %+v", cfg.Projects)
	}
	p.await("both repos remain in pane and management", `document.querySelectorAll(`+quote(card+` [data-repo]`)+`).length === 2 && document.querySelectorAll('#project-repos [data-managed-repo]').length === 2`)

	p.click("#project-home")
	p.click(card + ` [data-project-action=session-add]`)
	p.choose("#session-add-form [name=project]", string(repoID))
	p.choose("#session-add-form [name=profile]", "default")
	p.typeInto("#session-add-form [name=title]", "Explore the new client")
	p.click("#session-add-form [type=submit]")
	managementOutcome(p, "#session-add-form .result", "ok")
	sessions, err := c.Sessions(ctx, repoID)
	must(t, err)
	if len(sessions.Sessions) != 1 || sessions.Sessions[0].Project != repoID {
		t.Fatalf("exploratory session must use client repo: %+v", sessions)
	}
	p.click("#project-home")
	p.awaitText(card+` [data-sessions=session] [data-pane-workstream="`+string(sessions.Sessions[0].Workstream)+`"]`, "dagger / client")
	p.click(manage)
	p.click(row + ` [data-action=remove]`)
	managementOutcome(p, row+" .result", "ok")
	p.awaitText(row+" .result", "Repo removed from active work")
	projectChoicesInclude(p, project, repoID, false, 1)
	p.await("project and default repo remain", `document.querySelectorAll(`+quote(card+` [data-repo]`)+`).length === 1 && document.querySelector(`+quote(defaultRow)+`) !== null && document.querySelector(`+quote(row+` [data-action=remove]`)+`).disabled`)
	cfg, err = c.Configuration(ctx)
	must(t, err)
	if len(cfg.Projects) != 1 || len(cfg.Projects[0].Repos) != 1 || cfg.Projects[0].Repos[0].ID != project {
		t.Fatalf("repo removal must keep project and sibling: %+v", cfg.Projects)
	}
	p.await("all repo actions sent only the client ID", `(() => {
      const writes = managementWrites.filter(r => ['/v1/projects/extract', '/v1/projects/rebase'].includes(r.path) || (r.path === '/v1/projects' && r.method === 'DELETE' && r.body.project !== `+quote(string(project))+`));
      return writes.length === 3 && writes.every(r => JSON.stringify(r.body) === JSON.stringify({project: `+quote(string(repoID))+`}));
    })()`)
	p.await("the same page throughout", `window.sameDocument === true`)
}

func TestBrowserProjectManagementCharterFollowsLiveRevisionsAndKeepsStaleDraft(t *testing.T) {
	t.Parallel()
	opts, clone := projectFixture(t)
	withWeb(t, opts, "127.0.0.1:0")
	s, c := start(t, opts)
	ctx := context.Background()
	added, err := c.AddProject(ctx, request(clone))
	must(t, err)
	project := added.Project.ID
	p := openBrowser(t)
	observeManagementConfig(p)
	p.run(chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.await("project card", `document.querySelector('#project-pane [data-project-action=projects]') !== null`)
	p.openView("projects")
	p.awaitText("#projects-title", "Project management")
	p.await("no outdated cog entry", `![...document.querySelectorAll('#settings-menu button')].some(b => b.textContent === 'Projects and charters')`)
	p.choose("#project-edit [name=project]", string(project))
	p.eval(`document.querySelector('#project-edit [name=project]').dispatchEvent(new Event('change', {bubbles: true}))`, nil)
	p.awaitText("#charter-state", "Not ready for hand-in")
	p.click("#project-edit [data-action=read-charter]")
	managementOutcome(p, "#project-edit .result", "ok")
	recordManagementWrites(p)
	initial, err := c.Charter(ctx, project)
	must(t, err)
	p.eval(`managementConfigEvents.length = 0`, nil)
	const external = "1. Follow the latest owner rule.\n"
	next, err := c.EditCharter(ctx, project, CharterEdit{Revision: initial.Revision, Content: external})
	must(t, err)
	p.await("external charter save announced on the event stream", `managementConfigEvents.some(e => e.project === `+quote(string(project))+`)`)
	p.await("untouched editor follows API save and its pin", `document.querySelector('#project-edit [name=content]').value === `+quote(external)+` && document.querySelector('#project-edit').dataset.revision === `+quote(strconv.Itoa(next.Revision)))
	p.awaitText("#charter-state", "Ready for hand-in")
	// Make an actual change, so the service must accept the new pin before
	// recording another revision rather than simply return unchanged text.
	const local = "1. Follow the latest owner rule.\n2. Preserve decisions.\n"
	p.setValue("#project-edit [name=content]", local)
	p.click("#project-edit [type=submit]")
	managementOutcome(p, "#project-edit .result", "ok")
	p.await("save pinned to followed revision", `managementWrites.at(-1).body.revision === `+strconv.Itoa(next.Revision))
	read, err := c.Charter(ctx, project)
	must(t, err)
	if read.Content != local || read.Revision != next.Revision+1 {
		t.Fatalf("live revision pin was not saved: %+v", read)
	}
	const draft = "1. My unsaved draft must survive.\n"
	p.setValue("#project-edit [name=content]", draft)
	const remote = "1. Another client's decision wins until reread.\n"
	other, err := c.EditCharter(ctx, project, CharterEdit{Revision: read.Revision, Content: remote})
	must(t, err)
	p.await("dirty editor keeps draft and original pin", `!document.querySelector('#charter-changed').hidden && document.querySelector('#project-edit [name=content]').value === `+quote(draft)+` && document.querySelector('#project-edit').dataset.revision === `+quote(strconv.Itoa(read.Revision)))
	p.click("#project-edit [type=submit]")
	managementOutcome(p, "#project-edit .result", "error")
	p.awaitText("#project-edit .result", "the document changed")
	p.await("stale save still used original pin", `managementWrites.at(-1).body.revision === `+strconv.Itoa(read.Revision)+` && managementWrites.at(-1).body.content === `+quote(draft))
	got, err := c.Charter(ctx, project)
	must(t, err)
	if got.Content != remote || got.Revision != other.Revision {
		t.Fatalf("stale draft overwrote the other client: %+v", got)
	}
	// A further live event must keep both the refusal and the draft.
	latest, err := c.EditCharter(ctx, project, CharterEdit{Revision: other.Revision, Content: "1. The next external decision.\n"})
	must(t, err)
	p.awaitText("#charter-state", "revision "+strconv.Itoa(latest.Revision))
	p.await("draft and refusal survive refresh", `document.querySelector('#project-edit [name=content]').value === `+quote(draft)+` && document.querySelector('#project-edit .result').dataset.outcome === 'error' && document.querySelector('#project-edit').dataset.revision === `+quote(strconv.Itoa(read.Revision)))
	p.click("#project-edit [data-action=read-charter]")
	managementOutcome(p, "#project-edit .result", "ok")
	p.await("explicit reread adopts latest text and pin", `document.querySelector('#project-edit [name=content]').value === `+quote(latest.Content)+` && document.querySelector('#project-edit').dataset.revision === `+quote(strconv.Itoa(latest.Revision))+` && document.querySelector('#charter-changed').hidden`)
	p.await("same page", `window.sameDocument === true`)
}

func TestBrowserProjectManagementFollowsExternalProjectAndRepoChanges(t *testing.T) {
	t.Parallel()
	opts, clone := projectFixture(t)
	withWeb(t, opts, "127.0.0.1:0")
	s, c := start(t, opts)
	ctx := context.Background()
	added, err := c.AddProject(ctx, request(clone))
	must(t, err)
	project := added.Project.ID
	p := openBrowser(t)
	observeManagementConfig(p)
	p.run(chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	card := `#project-pane [data-project="` + string(project) + `"]`
	p.await("project card", `document.querySelector(`+quote(card)+`) !== null`)
	p.click(card + ` [data-project-action=projects]`)
	recordManagementWrites(p)
	second := filepath.Join(t.TempDir(), "second")
	demoGit(t, filepath.Dir(second), "init", "-q", second)
	repos, err := c.AddRepo(ctx, RepoAddRequest{Project: project, Name: "second", Upstream: "owner/second", Clone: second})
	must(t, err)
	var repoID config.ProjectID
	for _, repo := range repos.Project.Repos {
		if repo.ID != project {
			repoID = repo.ID
		}
	}
	if repoID == "" {
		t.Fatal("missing API-added repo")
	}
	projectChoicesInclude(p, project, repoID, true, 1)
	p.awaitText(card+` [data-repo="`+string(repoID)+`"]`, "second")
	row := `#project-repos [data-managed-repo="` + string(repoID) + `"]`
	p.awaitText(row, "owner/second")
	p.await("initial API-added repo extraction finished", `/Knowledge extraction 1: (succeeded|failed)/.test(document.querySelector(`+quote(row+` [data-field=extraction]`)+`).textContent)`)
	p.eval(`managementConfigEvents.length = 0`, nil)
	extraction, err := c.ExtractProject(ctx, repoID)
	must(t, err)
	if extraction.Extraction.Extraction != 2 {
		t.Fatalf("API extraction number: %+v", extraction)
	}
	p.await("external extraction announced for its repo", `managementConfigEvents.some(e => e.project === `+quote(string(repoID))+`)`)
	p.await("external extraction completion shown live", `/Knowledge extraction 2: (succeeded|failed)/.test(document.querySelector(`+quote(row+` [data-field=extraction]`)+`).textContent)`)
	cfg, err := c.Configuration(ctx)
	must(t, err)
	for _, repo := range cfg.Projects[0].Repos {
		if repo.ID == project && (repo.Extraction == nil || repo.Extraction.Extraction != 1) {
			t.Fatalf("external extraction affected the sibling: %+v", repo)
		}
	}
	third := filepath.Join(t.TempDir(), "third")
	demoGit(t, filepath.Dir(third), "init", "-q", third)
	another, err := c.AddProject(ctx, ProjectAddRequest{Name: "Third", Upstream: "owner/third", Clone: third})
	must(t, err)
	projectChoicesInclude(p, project, repoID, true, 2)
	p.await("API-created project in pane", `document.querySelector('#project-pane [data-project="`+string(another.Project.ID)+`"]') !== null`)
	_, err = c.RemoveProject(ctx, repoID)
	must(t, err)
	projectChoicesInclude(p, project, repoID, false, 2)
	p.await("API removal reflected in open management and pane", `document.querySelector(`+quote(row)+`) === null && document.querySelector(`+quote(card+` [data-repo="`+string(repoID)+`"]`)+`) === null && document.querySelector(`+quote(card)+`) !== null && document.querySelector('#project-edit [name=project]').value === `+quote(string(project)))
	_, err = c.RemoveProject(ctx, another.Project.ID)
	must(t, err)
	projectChoicesInclude(p, project, repoID, false, 1)
	p.await("removed project gone and page unchanged", `document.querySelectorAll('#project-pane [data-project]').length === 1 && window.sameDocument === true`)
}
