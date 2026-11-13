package lifecycle_test

import (
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

// The page shows no delivery card while the chief of staff's turn for the
// final review notice has recorded no brief, and then shows the card with
// the brief text, the screenshot as a thumbnail that opens the full image,
// and the Before you ship items in their own labelled block.
func TestBrowserDeliveryCardIsHeldUntilTheBriefThenShowsIt(t *testing.T) {
	t.Parallel()
	j := newBriefJourney(t, listenWeb)
	stream := j.stream
	project := j.repository.Project()
	screenshot := j.capture(t, "uploads", j.report.Commit)
	items := []string{briefShipFirst, briefShipNext}
	j.chief.plan(1, briefPlan{Text: briefText, Items: items, Screenshots: []string{screenshot}})
	j.chief.hold()
	j.run(t)
	j.awaitStarts(t, 1)

	p := openBrowser(t)
	p.Run(chromedp.EmulateViewport(1280, 900), chromedp.Navigate("http://"+j.s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.SelectWorkstream(stream)
	card := intakefixture.DecisionCard(api.InboxEntry{Kind: InboxDelivery, Project: project, Workstream: stream})
	cardSelector := strings.TrimSpace(card)
	if entries := j.entries(t, stream); len(entries) != 0 {
		t.Fatalf("the delivery entry is listed while the brief is pending: %+v", entries)
	}
	p.Await("no delivery card while the chief of staff writes the brief", `document.querySelector(`+quote(cardSelector)+`) === null`)

	// The brief is recorded: the card appears with it.
	j.chief.release()
	p.Await("the delivery card", `document.querySelector(`+quote(cardSelector)+`) !== null`)
	brief := card + "[data-field=brief] "
	p.AwaitText(brief+"[data-field=brief-text]", briefText)
	p.AwaitText(brief+"[data-field=before-you-ship-label]", "Before you ship")
	var shown struct {
		Items   []string `json:"items"`
		Visible bool     `json:"visible"`
		Own     bool     `json:"own"`
		Links   []string `json:"links"`
	}
	p.Eval(`(() => {
  const brief = document.querySelector(`+quote(strings.TrimSpace(brief))+`);
  const block = brief.querySelector('[data-field=before-you-ship]');
  return {
    items: [...block.querySelectorAll('[data-field=before-you-ship-item]')].map(n => n.textContent),
    visible: block.getClientRects().length > 0 && !block.closest('details'),
    own: !block.closest('[data-field=brief-text]') && !!block.closest('[data-field=brief]'),
    links: [...brief.querySelectorAll('a[data-field=image-link]')].map(a => a.getAttribute('href') + '|' + a.target),
  };
})()`, &shown)
	if strings.Join(shown.Items, "|") != strings.Join(items, "|") || !shown.Visible || !shown.Own {
		t.Fatalf("the Before you ship block: %+v", shown)
	}
	if want := "/v1/images/" + string(stream) + "/" + screenshot + "|_blank"; len(shown.Links) != 1 || shown.Links[0] != want {
		t.Fatalf("the screenshot links %v, want %s", shown.Links, want)
	}
	p.Await("the screenshot thumbnail", `(() => {
  const img = document.querySelector(`+quote(strings.TrimSpace(brief)+" a[data-field=image-link] img")+`);
  return !!img && img.complete && img.naturalWidth > 0;
})()`)
	p.AwaitText(card+"[data-field=pins]", "final review 1")
}
