package browsertest

import (
	"context"
	"testing"

	"github.com/chromedp/chromedp"
)

func TestBrowserProjectManagementHasNoPreviewSettings(t *testing.T) {
	t.Parallel()
	opts, clone := projectFixture(t)
	withWeb(t, opts, "127.0.0.1:0")
	s, c := start(t, opts)
	added, err := c.AddProject(context.Background(), request(clone))
	must(t, err)
	p := openOutcomePage(t, s, `
 window.previewRequests = [];
 const previewFetch = window.fetch;
 window.fetch = (url, init) => {
   if (String(url).includes('/projects/preview/')) previewRequests.push(String(url));
   return previewFetch(url, init);
 };`)
	for i := 0; i < 2; i++ {
		p.Click("#project-home")
		p.Click(`#project-pane [data-project="` + string(added.Project.ID) + `"] [data-project-action=projects]`)
		p.Await("project management loaded without preview controls", `!document.querySelector('[data-view=projects]').hidden && document.querySelector('#project-edit [name=project]').value === `+quote(string(added.Project.ID))+` && Number(document.querySelector('#project-edit').dataset.revision) > 0 && !document.querySelector('#project-preview, #preview-title, #preview-settings') && ![...document.querySelectorAll('[data-view=projects] input, [data-view=projects] textarea')].some(n => ['call','port','path','timeout'].includes(n.name)) && previewRequests.length === 0`)
		if i == 0 {
			p.Run(chromedp.Reload())
		}
	}
}
