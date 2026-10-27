package browsertest

import (
	"slices"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
)

func TestBrowserHandInOffersAllProjectsAndPreservesSelectionOrder(t *testing.T) {
	t.Parallel()
	f := newTwoProjectFixture(t)
	withWeb(t, f.opts, "127.0.0.1:0")
	s, c := start(t, f.opts)
	for _, id := range []config.ProjectID{project, otherProject} {
		writeCharter(t, c, id, "1. Keep owner decisions.\n")
	}
	p := openBrowser(t)
	p.Run(chromedp.Navigate("http://" + s.WebAddr() + "/"))
	p.Click("#new-workstream")
	p.Click(`#create-menu [data-open="handin"]`)
	p.Await("the hand-in view", `!document.querySelector('.view[data-view="handin"]').hidden`)
	p.Await("every active project in a multi-select", `document.querySelector('#handin-form [name=project]').multiple && document.querySelector('#handin-form [name=project]').options.length === 2`)
	// The last project in DOM order is chosen first. Later selection events
	// retain that home project even when an earlier option is added.
	p.Eval(`(() => {
  const select=document.querySelector('#handin-form [name=project]');
  for(const option of select.options) option.selected=false;
  select.options[1].selected=true;
  select.dispatchEvent(new Event('change',{bubbles:true}));
  select.options[0].selected=true;
  select.dispatchEvent(new Event('change',{bubbles:true}));
 })()`, nil)
	var want []config.ProjectID
	p.Eval(`[...document.querySelector('#handin-form [name=project]').options].map(o=>o.value).reverse()`, &want)
	p.TypeInto("#handin-form [name=content]", "Implement across both selected projects.")
	p.Click("#handin-form [type=submit]")
	p.AwaitText("#handin-form .result", "Handed in")
	r, err := service.InspectForTest(s).Repository(want[0])
	must(t, err)
	streams, err := r.Workstreams()
	must(t, err)
	var found bool
	for _, stream := range streams {
		selected, yes, err := intake.DesignSelectionOf(r, stream)
		must(t, err)
		if !yes {
			continue
		}
		got := []config.ProjectID{}
		for _, member := range selected.Members {
			got = append(got, member.Repo)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("web changed the chosen order: %v, want %v", got, want)
		}
		found = true
	}
	if !found {
		t.Fatal("web hand-in recorded no owner project list")
	}
}
