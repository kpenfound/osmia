package browsertest

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
)

func TestBrowserProjectPaneGroupsCleanedArchivesAcrossRestart(t *testing.T) {
	t.Parallel()
	opts, repo := sidebarFixture(t)
	ctx := context.Background()
	must(t, repo.CreateWorkstream(ctx, stream, demoStart, ownerActor))
	must(t, repo.Close())
	archives := []runtime.Archive{
		{Project: project, Workstream: "w_000000000000000000000000000000a1", Title: "Published legacy request", State: DeliveredState, DeliveryLink: "https://example.invalid/pull/42"},
		{Project: project, Workstream: "w_000000000000000000000000000000a2", Title: "Cancelled legacy request", State: AbandonedState},
	}
	for i := range archives {
		archives[i].ArchivedAt = demoStart
		archives[i].CleanedAt = demoStart.Add(time.Minute)
	}
	before, err := json.Marshal(runtime.State{Version: runtime.Version, Archived: archives})
	must(t, err)
	path := filepath.Join(opts.Config.Root, "runtime.json")
	must(t, os.WriteFile(path, before, 0600))
	var previous map[config.WorkstreamID]config.WorkgroupID
	for range 2 {
		s, c := start(t, opts)
		p := openBrowser(t)
		p.Run(chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile), chromedp.Navigate("http://"+s.WebAddr()+"/"))
		p.Await("three workgroups and no single-member graph", `document.querySelectorAll('#project-pane [data-workgroup]').length === 3 && !document.querySelector('#project-pane .workgroup-graph')`)
		current := map[config.WorkstreamID]config.WorkgroupID{}
		for i, a := range archives {
			status, err := c.Status(ctx, a.Workstream)
			must(t, err)
			if status.Workgroup == nil || !reflect.DeepEqual(status.Workgroup.Members, []config.WorkstreamID{a.Workstream}) || !status.Archived || !status.TraceDeleted {
				t.Fatalf("archive has no one-member workgroup: %+v", status)
			}
			current[a.Workstream] = status.Workgroup.ID
			row := `[data-project="` + string(project) + `"] [data-workgroup="` + string(status.Workgroup.ID) + `"] [data-pane-workstream="` + string(a.Workstream) + `"]`
			p.AwaitText(row, a.Title)
			label := "blocked"
			if i == 1 {
				label = "cancelled"
			}
			p.AwaitText(row+" [data-field=state]", label)
			p.AwaitText(row, "Archived · trace deleted")
			p.Await("exactly one archived row in its workgroup", `document.querySelectorAll(`+quote(row)+`).length === 1 && document.querySelector(`+quote(row+" [data-drilldown]")+`).disabled`)
			if i == 0 {
				p.AwaitText(row+" [data-field=wait]", "no pinned merge or cancel evidence")
			}
			if _, err := os.Lstat(filepath.Join(opts.Config.Root, "workstreams", string(project), string(a.Workstream))); !os.IsNotExist(err) {
				t.Fatalf("cleaned history recreated: %v", err)
			}
		}
		if previous != nil && !reflect.DeepEqual(previous, current) {
			t.Fatalf("restart changed workgroups: before=%v after=%v", previous, current)
		}
		previous = current
		p.Click(`[data-drilldown="` + string(stream) + `"]`)
		p.Await("the live member drill-down beside cleaned archives", `!document.querySelector('[data-view="workstream"]').hidden && document.querySelector('#workstream-head').dataset.workstream === `+quote(string(stream)))
		p.Await("the existing conversation", `document.querySelector('#workstreams .composer textarea') !== null`)
		p.Click("#project-home")
		p.Await("the project overview", `!document.querySelector('[data-view="project-pane"]').hidden`)
		must(t, s.Close())
		after, err := os.ReadFile(path)
		must(t, err)
		if !bytes.Equal(before, after) {
			t.Fatalf("archive records changed: before=%s after=%s", before, after)
		}
	}
}
