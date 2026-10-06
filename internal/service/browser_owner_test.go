package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestBrowserRegistersHandsInEditsAndInspectsTrace(t *testing.T) {
	opts, clone := projectFixture(t)
	path := filepath.Join(opts.Config.Root, "config.toml")
	data, err := os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(path, append(data, []byte("[listen]\nweb = \"127.0.0.1:0\"\n")...), 0600))
	must(t, os.MkdirAll(filepath.Join(clone, "internal", "trace"), 0700))
	must(t, os.WriteFile(filepath.Join(clone, "internal", "trace", "trace.go"), []byte("package trace\n"), 0600))
	demoGit(t, filepath.Dir(clone), "-C", clone, "add", "internal")
	s, c := start(t, opts)
	p := openBrowser(t)
	p.run(chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.await("connected", `document.body.dataset.connection === 'live'`)
	p.openView("project-add")
	for name, value := range map[string]string{"name": "Documents", "upstream": "owner/docs", "clone": clone} {
		p.typeInto("#project-add [name="+name+"]", value)
	}
	p.click("#project-add [type=submit]")
	p.awaitText("#project-add .result", "Registered Documents")
	cfg, err := c.Configuration(context.Background())
	must(t, err)
	id := cfg.Projects[0].ID
	if cfg.Projects[0].Fork != "" {
		t.Fatal("registration unexpectedly configured a fork")
	}
	p.await("new project choice", `[...document.querySelector('#project-edit [name=project]').options].some(o => o.value === `+quote(string(id))+`)`)
	p.openView("projects")
	p.choose("#project-edit [name=project]", string(id))
	p.click("#project-edit [data-action=read-charter]")
	p.awaitText("#project-edit .result", "Read charter revision")
	p.run(chromedp.SetValue("#project-edit [name=content]", "1. Preserve owner decisions.\n", chromedp.ByQuery))
	p.click("#project-edit [type=submit]")
	p.awaitText("#project-edit .result", "Saved charter revision")
	p.click("#new-workstream")
	p.await("the new workstream view", `!document.querySelector('.view[data-view="handin"]').hidden`)
	p.choose("#handin-form [name=project]", string(id))
	p.typeInto("#handin-form [name=content]", "Make durable documents")
	p.click("#handin-form [type=submit]")
	p.awaitText("#handin-form .result", "Handed in")
	repo, err := s.repository(id)
	must(t, err)
	streams, err := repo.Workstreams()
	must(t, err)
	var ws = streams[0]
	for _, candidate := range streams {
		if candidate != librarianWorkstream(id) {
			ws = candidate
		}
	}
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, Revision: 1, Project: id, Workstream: ws, At: s.now(), Actor: ownerActor, Cause: "draft"}
	spec := trace.Document{Header: h, Path: "spec.md", Content: validSpec}
	spec.ID = "spec"
	plan := trace.Document{Header: h, Path: "plan.json", Content: validPlan}
	plan.ID = "plan"
	must(t, repo.RecordDocuments(context.Background(), []trace.Document{spec, plan}))
	h.Schema, h.ID = "osmia.trace.transition", "sketch"
	_, err = repo.SetFeatureState(context.Background(), h, "sketched", "Draft ready")
	must(t, err)
	// The page shows the workstream it handed in.
	p.await("the handed-in workstream shown", `document.getElementById('workstream-head').dataset.workstream === `+quote(string(ws)))
	p.await("the view closed", `!document.querySelector('.view[data-view="workstream"]').hidden`)
	p.selectWorkstream(ws)
	parentText := "A prerequisite"
	parent, err := c.HandIn(context.Background(), HandInRequest{Project: id, Key: "browser-parent", Stdin: &parentText})
	must(t, err)
	// The Documents tab reads the documents and the dependency as it opens.
	p.click("[data-tab=documents]")
	p.await("the documents tab", `!document.querySelector('[data-panel=documents]').hidden`)
	p.awaitText("#document-revisions", "Spec revision 1")
	p.await("the documents read", `!document.querySelector('#documents-form [data-action=read]').disabled`)
	p.awaitText("#base-form .result", "Dependency revision 0 loaded")
	p.await("the dependency read", `!document.querySelector('#base-form [data-action=read]').disabled`)
	p.await("parent dependency choice", `[...document.querySelector('#base-form [name=base]').options].some(o => o.value === `+quote(string(parent.Workstream))+`)`)
	p.choose("#base-form [name=base]", string(parent.Workstream))
	p.click("#base-form [type=submit]")
	p.awaitText("#base-form .result", "Dependency revision 1 saved")
	// A separate owner's write makes the page's revision stale.
	spec.Revision = 2
	spec.Content = validSpec + "\nAnother owner edit.\n"
	must(t, repo.RecordDocuments(context.Background(), []trace.Document{spec}))
	p.typeInto("#documents-form [name=spec]", "\nMy edit.\n")
	p.click("#documents-form [type=submit]")
	p.awaitText("#documents-form .result", "read it again")
	p.click("#documents-form [data-action=read]")
	p.awaitText("#document-revisions", "Spec revision 2")
	p.typeInto("#documents-form [name=spec]", "\nReviewed edit.\n")
	p.click("#documents-form [type=submit]")
	p.awaitText("#document-revisions", "Spec revision 3")
	p.click("[data-tab=trace]")
	p.click("#trace-form [type=submit]")
	p.awaitText("#trace-form .result", "Trace loaded")
	p.awaitText("#trace-content", string(ws))
	h.ID = "ratify"
	_, err = repo.SetFeatureState(context.Background(), h, "ratified", "Owner ratified")
	must(t, err)
	p.await("ratification disables draft editing", `document.querySelector('#documents-form [type=submit]').disabled`)
}
