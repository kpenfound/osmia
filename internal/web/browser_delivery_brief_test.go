package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

const deliveryEntry = `{"kind":"delivery","project":"p_fake","workstream":"w_000","number":0,"batch":"","unit":"","amendment":"",` +
	`"revision":3,"question":"Deliver it?","blocked":"Publishing the pull request.","options":["approve"],"recommendation":"",` +
	`"quick_reply":"","opened_at":"2026-01-01T00:00:00Z","asked":[],` +
	`"answer":{"method":"POST","path":"/v1/delivery/w_000","body":{"review":2,"review_revision":3,"commit":"abc123","draft_hash":"hash1"}}}`

// deliveryPresentation is the JSON of a delivery presentation of final
// review 2 whose brief field is brief, left out when brief is empty.
func deliveryPresentation(brief string) string {
	if brief != "" {
		brief = `,"brief":` + brief
	}
	return `{"project":"p_fake","workstream":"w_000","report":{"review":2,"commit":"abc123","summary":"Everything shown.",` +
		`"criteria":[{"criterion":"spec#1","text":"It works","evidence":"Seen working"}]},"review_revision":3,` +
		`"title":"Add the thing","draft":"The description.\n","draft_hash":"hash1","messages":[],"style":"merge"` + brief + `}`
}

// newDeliveryBriefPage opens the page on a fake API whose delivery card for
// w_000 reads presentation, and whose feed for w_000 holds feed.
func newDeliveryBriefPage(t *testing.T, presentation string, feed ...string) *page {
	t.Helper()
	api := newFakeAPI(1)
	api.setInbox(deliveryEntry)
	api.setFeed("w_000", feed...)
	base := api.handler()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/delivery/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, presentation)
	})
	mux.Handle("/", base)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	p := openBrowser(t)
	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate(server.URL+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.click(`[data-select="w_000"]`)
	return p
}

// briefCard is the delivery card's own brief block.
const briefCard = `[data-decision][data-kind="delivery"] [data-field=brief]`

// A recorded brief shows its text and its screenshots as thumbnails that open
// the full image, apart from the report, and, with items, a labelled block of
// them that is always visible.
func TestBrowserDeliveryCardShowsTheRecordedBrief(t *testing.T) {
	p := newDeliveryBriefPage(t, deliveryPresentation(`{"state":"recorded","review":2,"review_revision":3,"commit":"abc123",`+
		`"text":"Owners see the **new** page.","before_you_ship":["Set OSMIA_TOKEN","Restart the service"],"screenshots":["img1","img2"]}`))
	p.awaitVisible("the brief", briefCard)
	p.awaitText(briefCard+" [data-field=brief-text]", "Owners see the new page.")
	var got struct {
		Links  []string `json:"links"`
		Items  []string `json:"items"`
		Label  string   `json:"label"`
		Own    bool     `json:"own"`
		Hidden bool     `json:"hidden"`
		Open   bool     `json:"open"`
	}
	p.eval(`(() => {
  const brief = document.querySelector(`+quote(briefCard)+`);
  const block = brief.querySelector('[data-field=before-you-ship]');
  return {
    links: [...brief.querySelectorAll('a[data-field=image-link]')].map(a => a.getAttribute('href') + '|' + a.target + '|' + !!a.querySelector('img')),
    items: [...block.querySelectorAll('[data-field=before-you-ship-item]')].map(n => n.textContent),
    label: block.querySelector('[data-field=before-you-ship-label]').textContent,
    own: !block.closest('[data-field=brief-text]') && !block.querySelector('[data-field=brief-text]') && !!block.closest('[data-field=brief]'),
    hidden: block.getClientRects().length === 0,
    open: !block.closest('details'),
  };
})()`, &got)
	if want := "/v1/images/w_000/img1|_blank|true,/v1/images/w_000/img2|_blank|true"; strings.Join(got.Links, ",") != want {
		t.Fatalf("screenshots: %v, want %s", got.Links, want)
	}
	if strings.Join(got.Items, "|") != "Set OSMIA_TOKEN|Restart the service" || got.Label != "Before you ship" || !got.Own || got.Hidden || !got.Open {
		t.Fatalf("before you ship block: %+v", got)
	}
	p.awaitText(`[data-decision][data-kind="delivery"] [data-field=summary]`, "Everything shown.")
}

// A brief with no items shows no deployment block.
func TestBrowserDeliveryCardShowsNoBeforeYouShipWithoutItems(t *testing.T) {
	p := newDeliveryBriefPage(t, deliveryPresentation(`{"state":"recorded","review":2,"review_revision":3,"commit":"abc123","text":"Text only."}`))
	p.awaitText(briefCard+" [data-field=brief-text]", "Text only.")
	var shown int
	p.eval(`document.querySelectorAll('[data-decision][data-kind="delivery"] [data-field=before-you-ship], [data-decision][data-kind="delivery"] [data-field=brief-screenshots]').length`, &shown)
	if shown != 0 {
		t.Fatalf("%d empty brief blocks shown", shown)
	}
}

// With no brief recorded for the review, the card says so.
func TestBrowserDeliveryCardSaysNoBriefWasRecorded(t *testing.T) {
	p := newDeliveryBriefPage(t, deliveryPresentation(`{"state":"none_recorded","review":2,"review_revision":3,"commit":"abc123"}`))
	p.awaitText(briefCard+" [data-field=brief-state]", "No brief was recorded for this review")
	var text int
	p.eval(`document.querySelectorAll('[data-decision][data-kind="delivery"] [data-field=brief-text]').length`, &text)
	if text != 0 {
		t.Fatal("the fallback card shows brief text")
	}
}

// A presentation without a brief field has no brief block.
func TestBrowserDeliveryCardWithoutBriefFieldShowsNoBlock(t *testing.T) {
	p := newDeliveryBriefPage(t, deliveryPresentation(""))
	p.awaitText(`[data-decision][data-kind="delivery"] [data-field=summary]`, "Everything shown.")
	var shown int
	p.eval(`document.querySelectorAll(`+quote(briefCard)+`).length`, &shown)
	if shown != 0 {
		t.Fatal("a brief block is shown without a brief")
	}
}

// The brief is its own entry of the workstream's feed, with its text, items and
// screenshots, and the conversation composer stays under it.
func TestBrowserFeedShowsTheBriefAsItsOwnEntry(t *testing.T) {
	brief := `{"kind":"brief","at":"2026-01-01T00:00:05Z","brief":{"review":2,"review_revision":3,"commit":"abc123",`+
		`"text":"Owners see the new page.","before_you_ship":["Set OSMIA_TOKEN"],`+
		`"screenshots":[{"id":"img1","url":"/v1/images/w_000/img1","content_type":"image/png"},{"id":"img2","placeholder":"image img2 is no longer stored"}]}}`
	p := newDeliveryBriefPage(t, deliveryPresentation(""), transitionEntry("2026-01-01T00:00:01Z", "assembled"), brief)
	p.awaitFeedKinds("transition,brief")
	const entry = `#workstreams [data-field=feed] li[data-kind=brief]`
	p.awaitText(entry+" [data-field=brief-text]", "Owners see the new page.")
	var got struct {
		Review, Link, Items, Placeholder string
		Composer                         bool
	}
	p.eval(`(() => {
  const entry = document.querySelector(`+quote(entry)+`);
  const form = document.querySelector('#workstreams [data-field=send]');
  return {
    review: entry.dataset.review,
    link: entry.querySelector('a[data-field=image-link]').getAttribute('href'),
    items: [...entry.querySelectorAll('[data-field=before-you-ship-item]')].map(n => n.textContent).join('|'),
    placeholder: entry.querySelector('[data-field=placeholder]').textContent,
    composer: !form.hidden && !!(entry.compareDocumentPosition(form) & Node.DOCUMENT_POSITION_FOLLOWING) && !form.querySelector('textarea').disabled,
  };
})()`, &got)
	if got.Review != "2" || got.Link != "/v1/images/w_000/img1" || got.Items != "Set OSMIA_TOKEN" || got.Placeholder != "image img2 is no longer stored" || !got.Composer {
		t.Fatalf("brief entry: %+v", got)
	}
}
