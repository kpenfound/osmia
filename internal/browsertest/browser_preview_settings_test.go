package browsertest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
)

func openPreviewSettings(p *page, project config.ProjectID) {
	p.T.Helper()
	p.Click("#project-home")
	p.Click(`#project-pane [data-project="` + string(project) + `"] [data-project-action=projects]`)
	p.Await("preview settings for the selected project", `!document.querySelector('[data-view=projects]').hidden && document.querySelector('#project-edit [name=project]').value === `+quote(string(project)))
}

func previewFields(p *page, values map[string]string) {
	p.T.Helper()
	for name, value := range values {
		p.SetValue("#project-preview [name="+name+"]", value)
		p.Eval(`document.querySelector(`+quote("#project-preview [name="+name+"]")+`).dispatchEvent(new Event('input', {bubbles: true}))`, nil)
	}
}

func assertPreviewConfiguration(t *testing.T, c *Client, project config.ProjectID, want *config.Preview) {
	t.Helper()
	cfg, err := c.Configuration(context.Background())
	must(t, err)
	got := cfg.Effective.For(project).Project.Preview
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("API preview = %+v, want %+v", got, want)
	}
}

func TestBrowserProjectPreviewLifecycle(t *testing.T) {
	t.Parallel()
	opts, clone := projectFixture(t)
	withWeb(t, opts, "127.0.0.1:0")
	s, c := start(t, opts)
	added, err := c.AddProject(context.Background(), request(clone))
	must(t, err)
	project := added.Project.ID
	p := openBrowser(t)
	p.Run(chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	openPreviewSettings(p, project)
	p.AwaitText("#preview-title", "Preview")
	p.AwaitText("#preview-state", "Previews are off.")
	p.Await("disable is unavailable while off", `document.querySelector('#project-preview [data-action=disable-preview]').disabled`)
	previewFields(p, map[string]string{"call": `["serve","--title","Hello world"]`, "port": "8080"})
	p.Click("#project-preview [type=submit]")
	managementOutcome(p, "#project-preview .result", "ok")
	want := &config.Preview{Call: []string{"serve", "--title", "Hello world"}, Port: 8080, Path: "/", Timeout: "5m"}
	assertPreviewConfiguration(t, c, project, want)
	p.AwaitText("#preview-state", "Previews are enabled.")
	// A fresh page must display the loaded values, including service defaults.
	p.Run(chromedp.Reload())
	openPreviewSettings(p, project)
	for _, text := range []string{`["serve","--title","Hello world"]`, "8080", "/", "5m"} {
		p.AwaitText("#preview-settings", text)
	}
	p.Await("loaded editor and phone layout", `document.querySelector('#project-preview [name=path]').value === '/' && document.querySelector('#project-preview [name=timeout]').value === '5m' && document.documentElement.scrollWidth <= innerWidth`)
	previewFields(p, map[string]string{"call": `["web","--dev"]`, "port": "3000", "path": "/settings?tab=preview", "timeout": "10m"})
	p.Click("#project-preview [type=submit]")
	managementOutcome(p, "#project-preview .result", "ok")
	want = &config.Preview{Call: []string{"web", "--dev"}, Port: 3000, Path: "/settings?tab=preview", Timeout: "10m"}
	assertPreviewConfiguration(t, c, project, want)
	p.AwaitText("#preview-settings", "/settings?tab=preview")
	p.Await("other project settings remain read-only", `!document.querySelector('#project-repos [contenteditable]') && [...document.querySelectorAll('[data-view=projects] input, [data-view=projects] textarea, [data-view=projects] select')].every(n => n.closest('#project-edit') ? ['project', 'content'].includes(n.name) : n.closest('#project-preview') && ['call', 'port', 'path', 'timeout'].includes(n.name))`)
	p.Click("#project-preview [data-action=disable-preview]")
	managementOutcome(p, "#project-preview .result", "ok")
	assertPreviewConfiguration(t, c, project, nil)
	p.AwaitText("#preview-state", "Previews are off.")
	p.Await("disabled preview clears editor", `document.querySelector('#project-preview [name=call]').value === '' && document.querySelector('#project-preview [name=port]').value === ''`)
}

func TestBrowserProjectPreviewRefusalsKeepInput(t *testing.T) {
	t.Parallel()
	opts, clone := projectFixture(t)
	withWeb(t, opts, "127.0.0.1:0")
	s, c := start(t, opts)
	added, err := c.AddProject(context.Background(), request(clone))
	must(t, err)
	project := added.Project.ID
	p := openBrowser(t)
	p.Run(chromedp.Navigate("http://" + s.WebAddr() + "/"))
	openPreviewSettings(p, project)
	recordManagementWrites(p)
	previewFields(p, map[string]string{"call": `["serve","--title","Owner's draft"]`, "port": "8080", "path": "settings", "timeout": "5m"})
	p.Click("#project-preview [type=submit]")
	managementOutcome(p, "#project-preview .result", "error")
	p.AwaitText("#project-preview .result", "preview.path")
	p.Await("API validation keeps every field", `managementWrites.at(-1).method === 'PUT' && document.querySelector('#project-preview [name=call]').value === `+quote(`["serve","--title","Owner's draft"]`)+` && document.querySelector('#project-preview [name=port]').value === '8080' && document.querySelector('#project-preview [name=path]').value === 'settings' && document.querySelector('#project-preview [name=timeout]').value === '5m'`)
	assertPreviewConfiguration(t, c, project, nil)
	// A live configuration update must not erase a refused draft or its message.
	_, err = c.SetProjectPreview(context.Background(), project, api.ProjectPreviewRequest{Call: []string{"external"}, Port: 9000})
	must(t, err)
	p.AwaitText("#preview-settings", "external")
	p.Await("refused draft survives live config", `document.querySelector('#project-preview .result').dataset.outcome === 'error' && document.querySelector('#project-preview [name=path]').value === 'settings' && document.querySelector('#project-preview [name=port]').value === '8080'`)
	previewFields(p, map[string]string{"path": "/settings"})
	p.Click("#project-preview [type=submit]")
	managementOutcome(p, "#project-preview .result", "ok")
	previewFields(p, map[string]string{"call": `["web","--owner-draft"]`, "port": "3000", "path": "/unsaved", "timeout": "12m"})
	path := filepath.Join(opts.Config.Root, "projects", string(project), "config.toml")
	data, err := os.ReadFile(path)
	must(t, err)
	drifted := append(data, []byte("# hand edit that must survive\n")...)
	must(t, os.WriteFile(path, drifted, 0600))
	for _, action := range []string{"[type=submit]", "[data-action=disable-preview]"} {
		p.Click("#project-preview " + action)
		managementOutcome(p, "#project-preview .result", "error")
		p.AwaitText("#project-preview .result", "drift")
		p.Await("drift refusal retains every draft field", `document.querySelector('#project-preview [name=call]').value === `+quote(`["web","--owner-draft"]`)+` && document.querySelector('#project-preview [name=port]').value === '3000' && document.querySelector('#project-preview [name=path]').value === '/unsaved' && document.querySelector('#project-preview [name=timeout]').value === '12m'`)
		got, err := os.ReadFile(path)
		must(t, err)
		if string(got) != string(drifted) {
			t.Fatal("refused preview save changed config.toml")
		}
	}
	assertPreviewConfiguration(t, c, project, &config.Preview{Call: []string{"serve", "--title", "Owner's draft"}, Port: 8080, Path: "/settings", Timeout: "5m"})
}

func TestBrowserProjectPreviewResponsesStayWithTheirOpening(t *testing.T) {
	t.Parallel()
	opts, clone := projectFixture(t)
	withWeb(t, opts, "127.0.0.1:0")
	s, c := start(t, opts)
	ctx := context.Background()
	first, err := c.AddProject(ctx, request(clone))
	must(t, err)
	secondClone := filepath.Join(t.TempDir(), "second")
	demoGit(t, filepath.Dir(secondClone), "init", "-q", secondClone)
	second, err := c.AddProject(ctx, ProjectAddRequest{Name: "Second", Upstream: "owner/second", Clone: secondClone})
	must(t, err)
	_, err = c.SetProjectPreview(ctx, second.Project.ID, api.ProjectPreviewRequest{Call: []string{"second"}, Port: 9000})
	must(t, err)
	p := openOutcomePage(t, s, `
      window.previewSettled = 0;
      const previewFetch = window.fetch;
      window.fetch = async (url, init) => {
        const response = await previewFetch(url, init);
        if (url.includes('/projects/preview/') && init?.method !== 'GET') {
          const json = response.json.bind(response);
          response.json = async () => {
            const body = await json();
            setTimeout(() => previewSettled++, 0);
            return body;
          };
        }
        return response;
      };`)
	url := "/v1/projects/preview/" + string(first.Project.ID)
	release := func() {
		t.Helper()
		var settled int
		p.Eval(`previewSettled`, &settled)
		releaseOutcome(p, url)
		p.Await("released preview callback completed", `previewSettled > `+fmt.Sprint(settled))
	}
	for _, refused := range []bool{false, true} {
		openPreviewSettings(p, first.Project.ID)
		path := "/saved"
		if refused {
			path = "invalid"
		}
		previewFields(p, map[string]string{"call": `["first"]`, "port": "8080", "path": path})
		holdOutcome(p, url)
		p.Click("#project-preview [type=submit]")
		p.Await("real preview response held", `typeof releaseOutcome[`+quote(url)+`] === 'function'`)
		assertOutcome(p, "#project-preview .result", "pending", "Sending…")
		p.Choose("#project-edit [name=project]", string(second.Project.ID))
		p.Eval(`document.querySelector('#project-edit [name=project]').dispatchEvent(new Event('change', {bubbles: true}))`, nil)
		p.AwaitText("#preview-settings", "9000")
		assertOutcome(p, "#project-preview .result", "", "")
		previewFields(p, map[string]string{"call": `["second","draft"]`, "path": "/second-draft"})
		release()
		assertOutcome(p, "#project-preview .result", "", "")
		p.Await("late response preserves the second project's draft", `document.querySelector('#project-preview [name=call]').value === '["second","draft"]' && document.querySelector('#project-preview [name=path]').value === '/second-draft' && !document.querySelector('#project-preview [type=submit]').disabled`)
		p.Click("#project-preview [type=submit]")
		managementOutcome(p, "#project-preview .result", "ok")
		assertPreviewConfiguration(t, c, second.Project.ID, &config.Preview{Call: []string{"second", "draft"}, Port: 9000, Path: "/second-draft", Timeout: "5m"})
		// Reopening the originating project is a new outcome scope as well.
		openPreviewSettings(p, first.Project.ID)
		previewFields(p, map[string]string{"call": `["first"]`, "port": "8081", "path": path})
		holdOutcome(p, url)
		p.Click("#project-preview [type=submit]")
		p.Await("preview response held across reopening", `typeof releaseOutcome[`+quote(url)+`] === 'function'`)
		openPreviewSettings(p, first.Project.ID)
		previewFields(p, map[string]string{"call": `["reopened","draft"]`, "path": "/reopened-draft"})
		release()
		p.Await("late completion leaves reopening untouched", `!document.querySelector('#project-preview [type=submit]').disabled && document.querySelector('#project-preview .result').dataset.outcome === '' && document.querySelector('#project-preview [name=call]').value === '["reopened","draft"]' && document.querySelector('#project-preview [name=path]').value === '/reopened-draft'`)
	}
	var responses int
	p.Eval(`outcomeRequests[`+quote(url)+`]`, &responses)
	if responses != 4 {
		t.Fatalf("preview dispatched %d held writes, want 4", responses)
	}
}
