package service

import (
	"context"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// annotationBox is the measured position of one role, action or timestamp
// element within a feed card.
type annotationBox struct {
	Ann                      string
	Left, Right, Top, Bottom float64
}

// width and height report the box's size, after rounding away sub-pixel
// noise that would otherwise make touching edges look like an overlap.
func (b annotationBox) width() float64  { return b.Right - b.Left }
func (b annotationBox) height() float64 { return b.Bottom - b.Top }

// overlaps reports whether b and other's boxes share more than a sliver of
// area, which would mean one annotation sits on top of the other.
func (b annotationBox) overlaps(other annotationBox) bool {
	const slack = 0.5
	if b.Right <= other.Left+slack || other.Right <= b.Left+slack {
		return false
	}
	if b.Bottom <= other.Top+slack || other.Bottom <= b.Top+slack {
		return false
	}
	return true
}

// annotationEntry is one feed card's own box and the role, action and
// timestamp annotation boxes chromedp found inside it.
type annotationEntry struct {
	Kind                  string
	EntryLeft, EntryRight float64
	Anns                  []annotationBox
}

// annotationLayout is everything feedAnnotationLayout measures on the page.
type annotationLayout struct {
	ViewportWidth    float64
	DocumentOverflow bool
	Entries          []annotationEntry
}

// feedAnnotationLayout measures, for every feed card matching selector, the
// card's own box and the box of each of its role, action and timestamp
// annotation elements (marked with data-ann in app.js), plus whether the
// document itself scrolls sideways at the current viewport width.
func (p *page) feedAnnotationLayout(selector string) annotationLayout {
	p.t.Helper()
	var layout annotationLayout
	expr := `(() => {
		const width = document.documentElement.clientWidth;
		const entries = [...document.querySelectorAll(` + quote(selector) + `)].map((entry) => {
			const box = entry.getBoundingClientRect();
			const anns = [...entry.querySelectorAll('[data-ann]')].map((n) => {
				const r = n.getBoundingClientRect();
				return { ann: n.dataset.ann, left: r.left, right: r.right, top: r.top, bottom: r.bottom };
			});
			return { kind: entry.dataset.kind, entryLeft: box.left, entryRight: box.right, anns };
		});
		return {
			viewportWidth: width,
			documentOverflow: document.documentElement.scrollWidth > width,
			entries,
		};
	})()`
	p.eval(expr, &layout)
	return layout
}

// assertFeedAnnotations fails the test unless every feed card selector
// matches shows a role, action and timestamp annotation that is each fully
// visible inside the card and the viewport, and none of the three
// intersects another, and neither the cards nor the page scroll sideways.
// It requires at least minEntries cards, so a selector typo or an empty
// feed cannot pass by matching nothing.
func assertFeedAnnotations(t *testing.T, p *page, selector string, minEntries int) {
	t.Helper()
	layout := p.feedAnnotationLayout(selector)
	if len(layout.Entries) < minEntries {
		t.Fatalf("found %d feed cards matching %s, want at least %d: %+v", len(layout.Entries), selector, minEntries, layout.Entries)
	}
	if layout.DocumentOverflow {
		t.Fatalf("the page scrolls horizontally at %vpx wide", layout.ViewportWidth)
	}
	for _, entry := range layout.Entries {
		if entry.EntryLeft < -0.5 || entry.EntryRight > layout.ViewportWidth+0.5 {
			t.Fatalf("a %s card scrolls horizontally: left %v right %v, viewport %v", entry.Kind, entry.EntryLeft, entry.EntryRight, layout.ViewportWidth)
		}
		byAnn := map[string]annotationBox{}
		for _, a := range entry.Anns {
			byAnn[a.Ann] = a
		}
		for _, want := range []string{"role", "action", "when"} {
			box, ok := byAnn[want]
			if !ok || box.width() <= 0 || box.height() <= 0 {
				t.Fatalf("a %s card's %q annotation is not fully visible: %+v", entry.Kind, want, box)
			}
			if box.Left < entry.EntryLeft-0.5 || box.Right > entry.EntryRight+0.5 {
				t.Fatalf("a %s card's %q annotation overflows the card: %+v outside %v..%v", entry.Kind, want, box, entry.EntryLeft, entry.EntryRight)
			}
			if box.Left < -0.5 || box.Right > layout.ViewportWidth+0.5 {
				t.Fatalf("a %s card's %q annotation overflows the viewport: %+v", entry.Kind, want, box)
			}
		}
		for i := range entry.Anns {
			for j := i + 1; j < len(entry.Anns); j++ {
				if entry.Anns[i].overlaps(entry.Anns[j]) {
					t.Fatalf("a %s card's %q and %q annotations overlap: %+v and %+v", entry.Kind, entry.Anns[i].Ann, entry.Anns[j].Ann, entry.Anns[i], entry.Anns[j])
				}
			}
		}
	}
}

// sendAndAnswer sends message to ws's chief of staff through the page and
// has the chief of staff answer with reply, so the feed gains a message
// entry and a response entry, the way a real conversation does.
func (f *pageFixture) sendAndAnswer(t *testing.T, p *page, ws config.WorkstreamID, message, reply string) {
	t.Helper()
	send := `[data-workstream="` + string(ws) + `"] [data-field=send] `
	p.typeInto(send+"textarea", message)
	p.click(send + "button")
	p.awaitText(send+".result", "Sent")
	ctx := context.Background()
	list, err := f.c.Conversation(ctx, ws)
	must(t, err)
	turn := list.Entries[len(list.Entries)-1].Turn
	_, err = f.s.sole().repository.ClaimTurn(ctx, ws, trace.ChiefOfStaff, "token_"+turn, t.TempDir(), time.Now().UTC())
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

	p.run(chromedp.EmulateViewport(375, 812, chromedp.EmulateScale(2), chromedp.EmulateMobile), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)

	streamCard := `[data-workstream="` + string(stream) + `"] `
	p.selectWorkstream(stream)
	p.awaitText(streamCard+"[data-field=feed]", "Unit upload: implementing")
	assertFeedAnnotations(t, p, streamCard+".entry", 4)

	quietCard := `[data-workstream="` + string(quiet) + `"] `
	p.selectWorkstream(quiet)
	p.awaitText(quietCard+"[data-field=feed]", "Nothing has happened yet.")
	f.sendAndAnswer(t, p, quiet, "Keep the mobile layout narrow.", "Noted; the layout stays narrow.")
	p.awaitText(quietCard+"[data-kind=response]", "Noted; the layout stays narrow.")
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

	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)

	streamCard := `[data-workstream="` + string(stream) + `"] `
	p.selectWorkstream(stream)
	p.awaitText(streamCard+"[data-field=feed]", "Unit upload: implementing")
	assertFeedAnnotations(t, p, streamCard+".entry", 4)

	quietCard := `[data-workstream="` + string(quiet) + `"] `
	p.selectWorkstream(quiet)
	p.awaitText(quietCard+"[data-field=feed]", "Nothing has happened yet.")
	f.sendAndAnswer(t, p, quiet, "Keep the desktop layout readable.", "Noted; the desktop layout stays readable.")
	p.awaitText(quietCard+"[data-kind=response]", "Noted; the desktop layout stays readable.")
	assertFeedAnnotations(t, p, quietCard+".entry", 2)
}
