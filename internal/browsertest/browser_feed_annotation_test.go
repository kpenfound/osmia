package browsertest

import (
	"context"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/trace"
)

func (f *pageFixture) sendAndAnswer(t *testing.T, p *page, ws config.WorkstreamID, message, reply string) {
	t.Helper()
	send := `[data-workstream="` + string(ws) + `"] [data-field=send] `
	p.TypeInto(send+"textarea", message)
	p.Click(send + "button[type=submit]")
	p.AwaitText(send+".result", "Sent")
	ctx := context.Background()
	list, err := f.c.Conversation(ctx, ws)
	must(t, err)
	turn := list.Entries[len(list.Entries)-1].Turn
	_, err = service.InspectForTest(f.s).SoleRepository().ClaimTurn(ctx, ws, trace.Manager, "token_"+turn, t.TempDir(), time.Now().UTC())
	must(t, err)
	f.answerChief(t, ws, turn, reply)
}

func TestBrowserEntryAnnotationFitsAtPhoneWidth(t *testing.T) {
	t.Parallel()
	p := openBrowser(t)
	f := newPageFixture(t)

	p.Run(chromedp.EmulateViewport(375, 812, chromedp.EmulateScale(2), chromedp.EmulateMobile), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)

	streamCard := `[data-workstream="` + string(stream) + `"] `
	p.SelectWorkstream(stream)
	p.AwaitText(streamCard+"[data-field=feed]", "Workstream")
	assertFeedAnnotations(t, p, streamCard+"[data-field=feed] .entry", 2)
	p.OpenActivity()
	p.AwaitText(streamCard+"[data-field=activity-log]", "Unit upload: implementing")
	assertFeedAnnotations(t, p, streamCard+"[data-field=activity-log] .entry", 4)
	p.OpenConversation()

	quietCard := `[data-workstream="` + string(quiet) + `"] `
	p.SelectWorkstream(quiet)
	p.AwaitText(quietCard+"[data-field=feed]", "Nothing here yet.")
	f.sendAndAnswer(t, p, quiet, "Keep the mobile layout narrow.", "Noted; the layout stays narrow.")
	p.AwaitText(quietCard+"[data-kind=response]", "Noted; the layout stays narrow.")
	assertFeedAnnotations(t, p, quietCard+"[data-field=feed] .entry", 2)
}

func TestBrowserEntryAnnotationShowsAtLaptopWidth(t *testing.T) {
	t.Parallel()
	p := openBrowser(t)
	f := newPageFixture(t)

	p.Run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)

	streamCard := `[data-workstream="` + string(stream) + `"] `
	p.SelectWorkstream(stream)
	p.AwaitText(streamCard+"[data-field=feed]", "Workstream")
	assertFeedAnnotations(t, p, streamCard+"[data-field=feed] .entry", 2)
	p.OpenActivity()
	p.AwaitText(streamCard+"[data-field=activity-log]", "Unit upload: implementing")
	assertFeedAnnotations(t, p, streamCard+"[data-field=activity-log] .entry", 4)
	p.OpenConversation()

	quietCard := `[data-workstream="` + string(quiet) + `"] `
	p.SelectWorkstream(quiet)
	p.AwaitText(quietCard+"[data-field=feed]", "Nothing here yet.")
	f.sendAndAnswer(t, p, quiet, "Keep the desktop layout readable.", "Noted; the desktop layout stays readable.")
	p.AwaitText(quietCard+"[data-kind=response]", "Noted; the desktop layout stays readable.")
	assertFeedAnnotations(t, p, quietCard+"[data-field=feed] .entry", 2)
}
