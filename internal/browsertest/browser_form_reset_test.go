package browsertest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chromedp/chromedp"
)

func TestBrowserPageResetsTheProjectAddFormAfterSuccessAndKeepsItAfterFailure(t *testing.T) {
	t.Parallel()
	p := openBrowser(t)
	opts, clone := projectFixture(t)
	path := filepath.Join(opts.Config.Root, "config.toml")
	data, err := os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(path, append(data, []byte(listenWeb)...), 0600))
	s, _ := start(t, opts)

	values := func() (name, upstream, fork, cloneField, base string) {
		p.Eval(`document.querySelector('#project-add [name=name]').value`, &name)
		p.Eval(`document.querySelector('#project-add [name=upstream]').value`, &upstream)
		p.Eval(`document.querySelector('#project-add [name=fork]').value`, &fork)
		p.Eval(`document.querySelector('#project-add [name=clone]').value`, &cloneField)
		p.Eval(`document.querySelector('#project-add [name=base_branch]').value`, &base)
		return
	}
	fill := func(name, upstream, fork string) {
		t.Helper()
		p.TypeInto("#project-add [name=name]", name)
		p.TypeInto("#project-add [name=upstream]", upstream)
		p.TypeInto("#project-add [name=fork]", fork)
		p.TypeInto("#project-add [name=clone]", clone)
	}

	p.Run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.OpenView("project-add")

	fill("Documents", "owner/docs", "owner/docs-fork")
	p.Click("#project-add [type=submit]")
	p.AwaitText("#project-add .result", "Registered Documents")
	if name, upstream, fork, cloneField, base := values(); name != "" || upstream != "" || fork != "" || cloneField != "" || base != "main" {
		t.Fatalf("the new project form after a successful registration: name %q, upstream %q, fork %q, clone %q, base %q", name, upstream, fork, cloneField, base)
	}

	fill("Notes", "owner/notes", "")
	p.Click("#project-add [type=submit]")
	p.AwaitText("#project-add .result", "already owns this registration or clone")
	p.Await("an error outcome", `document.getElementById('project-add').querySelector('.result').dataset.outcome === 'error'`)
	if name, upstream, fork, cloneField, _ := values(); name != "Notes" || upstream != "owner/notes" || fork != "" || cloneField != clone {
		t.Fatalf("the new project form after a failed registration: name %q, upstream %q, fork %q, clone %q", name, upstream, fork, cloneField)
	}
}

func TestBrowserPageResetsTheArchiveFormAfterSuccessAndKeepsItAfterFailure(t *testing.T) {
	t.Parallel()
	p := openBrowser(t)
	f := newPageFixture(t)

	fields := func() (action, note string, archive bool) {
		p.Eval(`document.querySelector('#workstream-action [name=action]').value`, &action)
		p.Eval(`document.querySelector('#workstream-action [name=note]').value`, &note)
		p.Eval(`document.querySelector('#workstream-action [name=archive]').checked`, &archive)
		return
	}

	p.Run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.SelectWorkstream(quiet)
	p.Click("[data-tab=documents]")
	p.Await("the documents tab", `!document.querySelector('[data-panel=documents]').hidden`)

	p.SetValue("#workstream-action [name=action]", "abandon")
	p.Click("#workstream-action [name=archive]")
	p.Await("the abandonment chosen", `document.querySelector('#workstream-action [name=action]').value === 'abandon' && document.querySelector('#workstream-action [name=archive]').checked`)
	p.TypeInto("#workstream-action [name=note]", "Superseded by the upload design.")
	p.Click("#workstream-action [type=submit]")
	p.Await("the next workstream shown", `document.getElementById('workstream-head').dataset.workstream === `+quote(string(stream)))
	p.Await("the next workstream has no abandonment outcome", `document.querySelector('#workstream-action .result').textContent === ''`)
	if action, note, archive := fields(); action != "object" || note != "" || archive {
		t.Fatalf("the debate-and-abandonment form after a successful action: action %q, note %q, archive %v", action, note, archive)
	}

	p.SetValue("#workstream-action [name=action]", "abandon")
	p.Eval(`document.querySelector('#workstream-action [name=archive]').checked = true`, nil)
	p.TypeInto("#workstream-action [name=note]", "Trying again.")
	p.Eval(`(() => {
      const form = document.getElementById('workstream-action');
      form.elements.workstream.value = `+quote(string(quiet))+`;
      form.requestSubmit(form.querySelector('[type=submit]'));
    })()`, nil)
	p.AwaitText("#workstream-action .result", "permanently archived")
	if action, note, archive := fields(); action != "abandon" || note != "Trying again." || !archive {
		t.Fatalf("the debate-and-abandonment form after a refused action: action %q, note %q, archive %v", action, note, archive)
	}
}
