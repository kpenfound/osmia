package browsertest

import (
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/servicetest"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestBrowserChiefPreviewThroughMemberArchive(t *testing.T) {
	f := servicetest.NewPreviewJourney(t)
	p := openBrowser(t)
	p.Run(chromedp.Navigate("http://" + f.S.WebAddr() + "/#workgroup=" + string(f.Group.ID)))
	p.Await("shared feed is open", `!document.querySelector('[data-view=workgroup]').hidden && document.querySelector('#workgroup-content .feed')`)
	p.Eval(`window.previewDocument = true`, nil)
	attachment := f.Capture(t)
	v := trace.Image{Header: trace.Header{ID: attachment.ID, Workstream: f.Member}}
	const feed = "#workgroup-content .feed"
	assertBrowserThumbnail(p, feed, v)
	entry := browserImageEntry(feed, v)
	p.Await("preview belongs to the shown member", `document.querySelector(`+quote(entry)+`)?.dataset.workstream === `+quote(string(f.Member)))
	openBrowserFullImage(p, feed, v)
	f.ArchiveMember(t)
	placeholder := "image removed when " + string(f.Member) + " was archived"
	p.AwaitText(entry+` [data-field=placeholder]`, placeholder)
	p.Await("archive updates the existing page without a broken image", `previewDocument && !document.querySelector(`+quote(entry)+`)?.querySelector('img, a')`)
	fresh := openBrowser(t)
	fresh.Run(chromedp.Navigate("http://" + f.S.WebAddr() + "/#workgroup=" + string(f.Group.ID)))
	fresh.AwaitText(entry+` [data-field=placeholder]`, placeholder)
	for _, path := range fresh.Paths() {
		if strings.HasPrefix(path, Prefix+"/images/") {
			t.Fatalf("archived preview requested image bytes: %s", path)
		}
	}
}
