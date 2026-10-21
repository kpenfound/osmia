package browsertest

import (
	"strings"
	"sync"
	"testing"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/config"
)

// annotationBox is the measured position of one role, action or timestamp
// element within a feed card.
type annotationBox struct {
	Ann                      string
	Left, Right, Top, Bottom float64
}

// width and height report the box's size, after rounding away sub-pixel
// noise that would otherwise make touching edges look like an overlap.
func (b annotationBox) width() float64 { return b.Right - b.Left }

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
func (p *Page) FeedAnnotationLayout(selector string) annotationLayout {
	p.T.Helper()
	var layout annotationLayout
	expr := `(() => {
		const width = document.documentElement.clientWidth;
		const entries = [...document.querySelectorAll(` + Quote(selector) + `)].map((entry) => {
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
	p.Eval(expr, &layout)
	return layout
}

// assertFeedAnnotations fails the test unless every feed card selector
// matches shows a role, action and timestamp annotation that is each fully
// visible inside the card and the viewport, and none of the three
// intersects another, and neither the cards nor the page scroll sideways.
// It requires at least minEntries cards, so a selector typo or an empty
// feed cannot pass by matching nothing.
func AssertFeedAnnotations(t *testing.T, p *Page, selector string, minEntries int) {
	t.Helper()
	layout := p.FeedAnnotationLayout(selector)
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

// awaitCleared waits until the field selector names shows no value, either
// because its card cleared it after a successful submission or because the
// resolved entry it answered already took the card out of the inbox; it
// fails with the page's text if neither happens.
func (p *Page) AwaitCleared(selector string) {
	p.T.Helper()
	p.Await(selector+" cleared", `(function(){var e=document.querySelector(`+Quote(selector)+`);return e===null||e.value==='';})()`)
}

// requestGate fails the page's requests of one kind while it is closed, so
// the page keeps what it last read of them until the gate opens again.
type RequestGate struct {
	mu     sync.Mutex
	closed bool
}

func (g *RequestGate) Set(closed bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = closed
}

// gate intercepts the page's requests whose URL matches pattern. A page has
// one gate at a time.
func (p *Page) Gate(pattern string) *RequestGate {
	p.T.Helper()
	g := &RequestGate{}
	chromedp.ListenTarget(p.Ctx, func(event any) {
		e, ok := event.(*fetch.EventRequestPaused)
		if !ok {
			return
		}
		g.mu.Lock()
		closed := g.closed
		g.mu.Unlock()
		go func() {
			ctx := cdp.WithExecutor(p.Ctx, chromedp.FromContext(p.Ctx).Target)
			if closed {
				fetch.FailRequest(e.RequestID, network.ErrorReasonConnectionRefused).Do(ctx)
			} else {
				fetch.ContinueRequest(e.RequestID).Do(ctx)
			}
		}()
	})
	p.Run(fetch.Enable().WithPatterns([]*fetch.RequestPattern{{URLPattern: pattern}}))
	return g
}

// awaitGone waits until the page shows no card for the entry.
func (p *Page) AwaitGone(what, card string) {
	p.T.Helper()
	p.Await(what+" to leave the inbox", `document.querySelector(`+Quote(strings.TrimSpace(card))+`) === null`)
}

// setValue replaces the value of the first field selector matches.
func (p *Page) SetValue(selector, value string) {
	p.T.Helper()
	p.Eval(`document.querySelector(`+Quote(selector)+`).value = `+Quote(value), nil)
}

// selectSession picks the session with id from the sidebar the way
// selectWorkstream does for an ordinary workstream, but waits for the
// session view's own content (there is no #workstream-head for a session).
func (p *Page) SelectSession(id config.WorkstreamID) {
	p.T.Helper()
	row := `[data-select="` + string(id) + `"]`
	p.Await("the session "+string(id)+" in the list", `document.querySelector(`+Quote(row)+`) !== null`)
	var collapsed bool
	p.Eval(`getComputedStyle(document.getElementById('sidebar')).display === 'none'`, &collapsed)
	if collapsed {
		p.Click("#picker-toggle")
	}
	p.Run(selectSidebarRow(row, `!document.querySelector('.view[data-view="session"]').hidden &&
		document.getElementById('session-meta').textContent.includes(`+Quote(string(id))+`)`))
}

func AwaitFeedEvent(p *Page, stream config.WorkstreamID) {
	p.T.Helper()
	p.Await("the workstream notification on the page's EventSource", `window.feedEvents.some(event => event.workstream === `+Quote(string(stream))+`)`)
	p.Await("the same document", `window.sameFeedDocument === true`)
	p.Eval(`window.feedEvents.length = 0`, nil)
}
