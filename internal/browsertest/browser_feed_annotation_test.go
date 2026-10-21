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

// sendAndAnswer sends message to ws's chief of staff through the page and
// has the chief of staff answer with reply, so the feed gains a message
// entry and a response entry, the way a real conversation does.
func (f *pageFixture) sendAndAnswer(t *testing.T, p *page, ws config.WorkstreamID, message, reply string) {
	t.Helper()
	send := `[data-workstream="` + string(ws) + `"] [data-field=send] `
	p.TypeInto(send+"textarea", message)
	p.Click(send + "button")
	p.AwaitText(send+".result", "Sent")
	ctx := context.Background()
	list, err := f.c.Conversation(ctx, ws)
	must(t, err)
	turn := list.Entries[len(list.Entries)-1].Turn
	_, err = service.InspectForTest(f.s).SoleRepository().ClaimTurn(ctx, ws, trace.ChiefOfStaff, "token_"+turn, t.TempDir(), time.Now().UTC())
	must(t, err)
	f.answerChief(t, ws, turn, reply)
}

// On narrow viewports, every feed card's role, action and timestamp stay
// readable: each one is fully visible inside the card and the page, none of
// the three overlaps another, and neither the card nor the page scrolls
// sideways. The fixture's status, session and unit-transition cards cover
// those three kinds; sending a message and its answer on the quiet
// workstream covers the message and response kinds.
func TestBrowserFeedAnnotationFitsAtPhoneWidth(t *testing.T) {
	p := openBrowser(t)
	f := newPageFixture(t)

	p.Run(chromedp.EmulateViewport(375, 812, chromedp.EmulateScale(2), chromedp.EmulateMobile), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)

	streamCard := `[data-workstream="` + string(stream) + `"] `
	p.SelectWorkstream(stream)
	p.AwaitText(streamCard+"[data-field=feed]", "Unit upload: implementing")
	assertFeedAnnotations(t, p, streamCard+".entry", 4)

	quietCard := `[data-workstream="` + string(quiet) + `"] `
	p.SelectWorkstream(quiet)
	p.AwaitText(quietCard+"[data-field=feed]", "Nothing has happened yet.")
	f.sendAndAnswer(t, p, quiet, "Keep the mobile layout narrow.", "Noted; the layout stays narrow.")
	p.AwaitText(quietCard+"[data-kind=response]", "Noted; the layout stays narrow.")
	assertFeedAnnotations(t, p, quietCard+".entry", 2)
}

// At a laptop width, every feed card still shows its role, action and
// timestamp, with the same checks as the phone-width test. This is also
// where the fix is checked not to have changed the desktop layout: the
// annotation still reads left to right on one line because there is room
// for it, since the CSS only wraps the annotation when it does not fit.
func TestBrowserFeedAnnotationShowsAtLaptopWidth(t *testing.T) {
	p := openBrowser(t)
	f := newPageFixture(t)

	p.Run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)

	streamCard := `[data-workstream="` + string(stream) + `"] `
	p.SelectWorkstream(stream)
	p.AwaitText(streamCard+"[data-field=feed]", "Unit upload: implementing")
	assertFeedAnnotations(t, p, streamCard+".entry", 4)

	quietCard := `[data-workstream="` + string(quiet) + `"] `
	p.SelectWorkstream(quiet)
	p.AwaitText(quietCard+"[data-field=feed]", "Nothing has happened yet.")
	f.sendAndAnswer(t, p, quiet, "Keep the desktop layout readable.", "Noted; the desktop layout stays readable.")
	p.AwaitText(quietCard+"[data-kind=response]", "Noted; the desktop layout stays readable.")
	assertFeedAnnotations(t, p, quietCard+".entry", 2)
}
