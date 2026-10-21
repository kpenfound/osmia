package browsertest

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestBrowserRegistersHandsInEditsAndInspectsTrace(t *testing.T) {
	t.Parallel()
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
	p.Run(chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.Await("connected", `document.body.dataset.connection === 'live'`)
	p.OpenView("project-add")
	for name, value := range map[string]string{"name": "Documents", "upstream": "owner/docs", "clone": clone} {
		p.TypeInto("#project-add [name="+name+"]", value)
	}
	p.Click("#project-add [type=submit]")
	p.AwaitText("#project-add .result", "Registered Documents")
	cfg, err := c.Configuration(context.Background())
	must(t, err)
	id := cfg.Projects[0].ID
	if cfg.Projects[0].Fork != "" {
		t.Fatal("registration unexpectedly configured a fork")
	}
	p.Await("new project choice", `[...document.querySelector('#project-edit [name=project]').options].some(o => o.value === `+quote(string(id))+`)`)
	p.OpenView("projects")
	p.Choose("#project-edit [name=project]", string(id))
	p.Click("#project-edit [data-action=read-charter]")
	p.AwaitText("#project-edit .result", "Read charter revision")
	p.Run(chromedp.SetValue("#project-edit [name=content]", "1. Preserve owner decisions.\n", chromedp.ByQuery))
	p.Click("#project-edit [type=submit]")
	p.AwaitText("#project-edit .result", "Saved charter revision")
	p.Click("#new-workstream")
	p.Click(`#create-menu [data-open="handin"]`)
	p.Await("the new workstream view", `!document.querySelector('.view[data-view="handin"]').hidden`)
	p.Choose("#handin-form [name=project]", string(id))
	p.TypeInto("#handin-form [name=content]", "Make durable documents")
	p.Click("#handin-form [type=submit]")
	p.AwaitText("#handin-form .result", "Handed in")
	repo, err := service.InspectForTest(s).Repository(id)
	must(t, err)
	streams, err := repo.Workstreams()
	must(t, err)
	var ws = streams[0]
	for _, candidate := range streams {
		if candidate != librarianWorkstream(id) {
			ws = candidate
		}
	}
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, Revision: 1, Project: id, Workstream: ws, At: service.InspectForTest(s).Now(), Actor: ownerActor, Cause: "draft"}
	spec := trace.Document{Header: h, Path: "spec.md", Content: validSpec}
	spec.ID = "spec"
	plan := trace.Document{Header: h, Path: "plan.json", Content: validPlan}
	plan.ID = "plan"
	must(t, repo.RecordDocuments(context.Background(), []trace.Document{spec, plan}))
	h.Schema, h.ID = "osmia.trace.transition", "sketch"
	_, err = repo.SetFeatureState(context.Background(), h, "sketched", "Draft ready")
	must(t, err)
	// The page shows the workstream it handed in.
	p.Await("the handed-in workstream shown", `document.getElementById('workstream-head').dataset.workstream === `+quote(string(ws)))
	p.Await("the view closed", `!document.querySelector('.view[data-view="workstream"]').hidden`)
	p.SelectWorkstream(ws)
	parentText := "A prerequisite"
	parent, err := c.HandIn(context.Background(), HandInRequest{Project: id, Key: "browser-parent", Stdin: &parentText})
	must(t, err)
	// The Documents tab reads the documents and the dependency as it opens.
	p.Click("[data-tab=documents]")
	p.Await("the documents tab", `!document.querySelector('[data-panel=documents]').hidden`)
	p.AwaitText("#document-revisions", "Spec revision 1")
	p.Await("the documents read", `!document.querySelector('#documents-form [data-action=read]').disabled`)
	p.AwaitText("#base-form .result", "Dependency revision 0 loaded")
	p.Await("the dependency read", `!document.querySelector('#base-form [data-action=read]').disabled`)
	p.Await("parent dependency choice", `[...document.querySelector('#base-form [name=base]').options].some(o => o.value === `+quote(string(parent.Workstream))+`)`)
	p.Choose("#base-form [name=base]", string(parent.Workstream))
	p.Click("#base-form [type=submit]")
	p.AwaitText("#base-form .result", "Dependency revision 1 saved")
	// A separate owner's write makes the page's revision stale.
	spec.Revision = 2
	spec.Content = validSpec + "\nAnother owner edit.\n"
	must(t, repo.RecordDocuments(context.Background(), []trace.Document{spec}))
	p.TypeInto("#documents-form [name=spec]", "\nMy edit.\n")
	p.Click("#documents-form [type=submit]")
	p.AwaitText("#documents-form .result", "read it again")
	p.Click("#documents-form [data-action=read]")
	p.AwaitText("#document-revisions", "Spec revision 2")
	p.TypeInto("#documents-form [name=spec]", "\nReviewed edit.\n")
	p.Click("#documents-form [type=submit]")
	p.AwaitText("#document-revisions", "Spec revision 3")
	p.Click("[data-tab=trace]")
	p.Click("#trace-form [type=submit]")
	p.AwaitText("#trace-form .result", "Trace loaded")
	p.AwaitText("#trace-content", string(ws))
	h.ID = "ratify"
	_, err = repo.SetFeatureState(context.Background(), h, "ratified", "Owner ratified")
	must(t, err)
	p.Await("ratification disables draft editing", `document.querySelector('#documents-form [type=submit]').disabled`)
}
