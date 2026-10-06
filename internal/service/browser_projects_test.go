package service

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
)

func TestBrowserControlsEachProjectAndKeepsPriorityOrdersSeparate(t *testing.T) {
	f := newTwoProjectFixture(t)
	ctx := context.Background()
	path := filepath.Join(f.opts.Config.Root, "config.toml")
	data, err := os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(path, append(data, []byte("[listen]\nweb = \"127.0.0.1:0\"\n")...), 0600))
	s, c := start(t, f.opts)
	p := openBrowser(t)
	p.run(chromedp.EmulateViewport(390, 844, chromedp.EmulateScale(3), chromedp.EmulateMobile), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.await("both projects in the priority selector", `document.querySelector('#priority-project').options.length === 2`)
	p.eval(`window.notReloaded = true`, nil)
	p.openPauses()

	runtimeView := func() RuntimeResponse {
		t.Helper()
		rt, err := c.Runtime(ctx)
		must(t, err)
		return rt
	}
	for _, id := range []config.ProjectID{project, otherProject} {
		ws := f.streams[id]
		p.awaitText(`[data-select="`+string(ws)+`"] [data-field=project]`, string(id))
		for _, target := range []runtime.Target{
			{Scope: "project", Project: id},
			{Scope: "workstream", Project: id, Workstream: ws},
		} {
			value := "project:" + string(id)
			if target.Scope == "workstream" {
				value = "workstream:" + string(ws)
			}
			p.choose("#pause-form select[name=target]", value)
			p.typeInto("#pause-form input[name=reason]", "Owner review")
			p.click("#pause-form button[type=submit]")
			shown := `[data-pause="` + value + `"] `
			p.awaitText(shown+"[data-field=reason]", "Owner review")
			pauses := runtimeView().Effective.Pauses
			if len(pauses) != 1 || pauses[0].Target != target {
				t.Fatalf("pause for %+v affected %+v", target, pauses)
			}
			p.click(shown + "[data-field=resume]")
			p.await("the selected scope resumed", `document.querySelector(`+quote(shown)+`) === null`)
			if pauses := runtimeView().Effective.Pauses; len(pauses) != 0 {
				t.Fatalf("resume left pauses: %+v", pauses)
			}
		}
	}

	selectProject := func(id config.ProjectID) {
		t.Helper()
		p.choose("#priority-project", string(id))
		p.eval(`document.querySelector('#priority-project').dispatchEvent(new Event('change', {bubbles: true}))`, nil)
		p.await("only the selected project's workstream", `document.querySelectorAll('#priority-list li').length === 1 && document.querySelector('#priority-list li').dataset.priority === `+quote(string(f.streams[id])))
	}
	// An unsaved selection in one project must not become another project's
	// order, including when both have no saved priority yet.
	p.openView("priority")
	p.click("#priority-list input[type=checkbox]")
	selectProject(otherProject)
	p.await("the other project's empty selection", `!document.querySelector('#priority-list input').checked`)
	for _, id := range []config.ProjectID{otherProject, project} {
		selectProject(id)
		p.click("#priority-list input[type=checkbox]")
		p.click("#priority-form button[type=submit]")
		p.awaitText("#priority-current", "In force:")
		p.awaitText("#priority-result", "Priority order set.")
	}
	orders := runtimeView().Effective.Priorities
	if len(orders) != 2 {
		t.Fatalf("priority orders: %+v", orders)
	}
	for _, order := range orders {
		if !slices.Equal(order.Workstreams, []config.WorkstreamID{f.streams[order.Project]}) {
			t.Fatalf("priority includes another project's work: %+v", order)
		}
	}
	selectProject(otherProject)
	p.click("#priority-clear")
	p.awaitText("#priority-result", "Priority order cleared.")
	p.awaitText("#priority-current", "No order is set")
	orders = runtimeView().Effective.Priorities
	if len(orders) != 1 || orders[0].Project != project {
		t.Fatalf("clearing the other project's priority affected %+v", orders)
	}

	// Configuration events remove unavailable targets without reloading the
	// page, and reset the editor to the remaining project's saved order.
	_, err = c.RemoveProject(ctx, otherProject)
	must(t, err)
	p.await("only the remaining project", `document.querySelector('#priority-project').options.length === 1 && document.querySelector('#priority-project').value === `+quote(string(project)))
	p.await("the remaining saved priority", `document.querySelector('#priority-list li').dataset.priority === `+quote(string(stream))+` && document.querySelector('#priority-list input').checked`)
	p.await("removed pause targets", `![...document.querySelector('#pause-form select[name=target]').options].some(o => o.value === `+quote("project:"+string(otherProject))+` || o.value === `+quote("workstream:"+string(otherStream))+`)`)
	_, err = c.RemoveProject(ctx, project)
	must(t, err)
	p.awaitText("#priority-current", "No project is configured.")
	p.await("disabled priority controls", `document.querySelector('#priority-form button[type=submit]').disabled && document.querySelector('#priority-clear').disabled && document.querySelector('#priority-list').children.length === 0`)
	p.await("the same document", `window.notReloaded === true`)
}
