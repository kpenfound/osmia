package browsertest

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"slices"
	"strings"
	"testing"
	"time"

	cdppage "github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/trace"
)

func recordBrowserImage(t *testing.T, repo *trace.Repository, id config.WorkstreamID, key string) trace.Image {
	t.Helper()
	var data bytes.Buffer
	must(t, png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 1200, 800))))
	v, err := repo.RecordImage(context.Background(), trace.Header{Project: repo.Project(), Workstream: id, At: time.Now().UTC(), Actor: ownerActor, Cause: "browser-image"}, key, "image/png", data.Bytes(), "image-turn")
	must(t, err)
	return v
}

func browserImageEntry(feed string, v trace.Image) string {
	return feed + ` [data-kind=image][data-image="` + v.ID + `"]`
}

func assertBrowserThumbnail(p *page, feed string, v trace.Image) {
	p.T.Helper()
	entry := browserImageEntry(feed, v)
	url := Prefix + "/images/" + string(v.Workstream) + "/" + v.ID
	p.Await("a loaded thumbnail from the image endpoint", `(() => {
 const entry = document.querySelector(`+quote(entry)+`);
 const img = entry?.querySelector('[data-field=image]');
 const link = entry?.querySelector('[data-field=image-link]');
 const rect = img?.getBoundingClientRect();
 return img?.complete && img.naturalWidth === 1200 && img.naturalHeight === 800 &&
 rect.width > 0 && rect.width < img.naturalWidth && rect.height < img.naturalHeight &&
 rect.right <= document.documentElement.clientWidth &&
 img.getAttribute('src') === `+quote(url)+` && link?.getAttribute('href') === `+quote(url)+` &&
 link.target === '_blank' && link.rel.includes('noopener') && link.rel.includes('noreferrer') &&
 entry.id === `+quote("feed-image-"+v.ID)+`;
 })()`)
	if !slices.Contains(p.Paths(), url) {
		p.T.Fatalf("thumbnail did not request %s", url)
	}
}

func openBrowserFullImage(p *page, feed string, v trace.Image) {
	p.T.Helper()
	ctx, cancel := context.WithTimeout(p.Ctx, browserTimeout)
	defer cancel()
	var origin string
	p.Eval("location.origin", &origin)
	url := origin + Prefix + "/images/" + string(v.Workstream) + "/" + v.ID
	next := chromedp.WaitNewTarget(ctx, func(info *target.Info) bool { return info.URL == url })
	p.Click(browserImageEntry(feed, v) + ` [data-field=image-link]`)
	select {
	case id := <-next:
		tab, closeTab := chromedp.NewContext(p.Ctx, chromedp.WithTargetID(id))
		defer closeTab()
		tab, stop := context.WithTimeout(tab, browserTimeout)
		defer stop()
		var contentType string
		must(p.T, chromedp.Run(tab, chromedp.WaitReady("img", chromedp.ByQuery), chromedp.Evaluate("document.contentType", &contentType)))
		if contentType != "image/png" {
			p.T.Fatalf("full-size content type: %s", contentType)
		}
	case <-ctx.Done():
		p.T.Fatal("thumbnail did not open the full-size image")
	}
	p.Run(chromedp.ActionFunc(func(ctx context.Context) error { return cdppage.BringToFront().Do(ctx) }))
}

func TestBrowserWorkstreamImages(t *testing.T) {
	f := newPageFixture(t)
	seedScrollMessages(t, f.c, stream)
	repo := service.InspectForTest(f.s).SoleRepository()
	first := recordBrowserImage(t, repo, stream, "first")
	p := openFeedScrollPage(t, f.s, "#workstream="+string(stream), "")
	feed := "#workstreams .feed"
	assertBrowserThumbnail(p, feed, first)
	feedAtBottom(p, feed)
	scrollFeedUp(p)
	rereadFeed(p)
	feedScrollUnchanged(p)
	p.TypeInto("#workstreams .composer textarea", "Unsent image draft")
	p.Eval(`document.querySelector('#workstreams textarea').focus({preventScroll:true}); window.imageDocument = true`, nil)
	second := recordBrowserImage(t, repo, stream, "second")
	assertBrowserThumbnail(p, feed, second)
	feedAtBottom(p, feed)
	p.Await("SSE keeps the document and draft focus", `imageDocument && document.activeElement === document.querySelector('#workstreams textarea') && document.activeElement.value === 'Unsent image draft'`)
	openBrowserFullImage(p, feed, second)
	p.Run(chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile))
	assertBrowserThumbnail(p, feed, second)
}

func TestBrowserWorkgroupImagesAndArchivedPlaceholder(t *testing.T) {
	f := newWorkgroupPageFixture(t, false)
	seedScrollMessages(t, f.c, stream)
	v := recordBrowserImage(t, f.repo, quiet, "member-image")
	p := openFeedScrollPage(t, f.s, "#workgroup="+string(f.group.ID), "")
	feed := "#workgroup-content .feed"
	p.AwaitText(feed, "History 13.")
	feedAtBottom(p, feed)
	scrollFeedUp(p)
	p.Eval(`window.imageDocument = true`, nil)
	host, err := f.repo.ChiefOfStaffHost(stream)
	must(t, err)
	_, err = f.repo.PublishImageReference(context.Background(), trace.Header{Project: f.repo.Project(), Workstream: host, At: time.Now().UTC(), Actor: ownerActor, Cause: "browser-reference"}, quiet, v.ID, "image-turn")
	must(t, err)
	assertBrowserThumbnail(p, feed, v)
	feedAtBottom(p, feed)
	entry := browserImageEntry(feed, v)
	p.AwaitText(entry+` [data-field=repo]`, "Uploads repo")
	p.Await("member attribution", `document.querySelector(`+quote(entry)+`)?.dataset.workstream === `+quote(string(quiet)))
	scrollFeedUp(p)
	rereadFeed(p)
	feedScrollUnchanged(p)
	openBrowserFullImage(p, feed, v)
	// The same image stays in its member's own feed in a multi-member group.
	p.Click(`[data-open-member="` + string(quiet) + `"]`)
	assertBrowserThumbnail(p, "#workstreams .feed", v)
	openWorkgroup(p, f.group.ID)
	_, err = f.c.Abandon(context.Background(), quiet, "Finished with this member")
	must(t, err)
	_, err = f.c.Archive(context.Background(), quiet)
	must(t, err)
	p.AwaitText(entry+` [data-field=placeholder]`, "image removed when "+string(quiet)+" was archived")
	p.Await("the placeholder has no image or link", `!document.querySelector(`+quote(entry)+`)?.querySelector('img, a') && imageDocument`)
	// A fresh opening of the archived reference never requests deleted bytes.
	fresh := openFeedScrollPage(t, f.s, "#workgroup="+string(f.group.ID), "")
	fresh.AwaitText(entry+` [data-field=placeholder]`, "image removed when "+string(quiet)+" was archived")
	rereadFeed(fresh)
	for _, path := range fresh.Paths() {
		if strings.HasPrefix(path, Prefix+"/images/") {
			t.Fatalf("archived placeholder requested %s", path)
		}
	}
}

func TestBrowserSessionImages(t *testing.T) {
	s, c, project, engine, _ := sessionWebFixture(t)
	engine.Turn = func(context.Context, agent.Request) (*agent.Result, error) {
		return &agent.Result{ClaudeID: "session", ResultText: strings.Repeat(scrollReply, 45)}, nil
	}
	opened, err := c.OpenSession(context.Background(), SessionOpenRequest{Project: project, Profile: "default"})
	must(t, err)
	id := opened.Session.Workstream
	p := openFeedScrollPage(t, s, "#workstream="+string(id), "")
	p.TypeInto("#session-form textarea", "Seed image history")
	p.Click("#session-form [type=submit]")
	p.AwaitText("#session-feed", scrollReply[:22])
	p.Await("the session is idle", textOf("#session-meta [data-field=state]")+` === 'idle' && feedPending === 0`)
	_, _, repo, apiErr := service.InspectForTest(s).SessionTrace(string(id))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	scrollFeedUp(p)
	p.TypeInto("#session-form textarea", "Unsent session draft")
	p.Eval(`document.querySelector('#session-form textarea').focus({preventScroll:true}); window.imageDocument = true`, nil)
	v := recordBrowserImage(t, repo, id, "session-image")
	assertBrowserThumbnail(p, "#session-feed", v)
	feedAtBottom(p, "#session-feed")
	p.AwaitText(browserImageEntry("#session-feed", v)+` [data-ann=role]`, "Agent")
	p.Await("SSE keeps the session draft focus", `imageDocument && document.activeElement === document.querySelector('#session-form textarea') && document.activeElement.value === 'Unsent session draft'`)
	scrollFeedUp(p)
	rereadFeed(p)
	feedScrollUnchanged(p)
	openBrowserFullImage(p, "#session-feed", v)
}
