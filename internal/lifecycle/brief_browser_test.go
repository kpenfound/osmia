package lifecycle_test

import (
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/browsertest"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

func TestBrowserDeliveryCardIsHeldUntilTheBriefThenShowsIt(t *testing.T) {
	t.Parallel()
	j := newBriefJourney(t, journey.ListenWeb)
	stream := j.Stream
	project := j.Repository.Project()
	screenshot := j.capture(t, "uploads", j.Report.Commit)
	items := []string{briefShipFirst, briefShipNext}
	j.manager.plan(1, briefPlan{Text: briefText, Items: items, Screenshots: []string{screenshot}})
	j.manager.hold()
	j.run(t)
	j.awaitStarts(t, 1)

	p := browsertest.OpenBrowser(t)
	p.Run(chromedp.EmulateViewport(1280, 900), chromedp.Navigate("http://"+j.S.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.SelectWorkstream(stream)
	card := intakefixture.DecisionCard(api.InboxEntry{Kind: api.InboxDelivery, Project: project, Workstream: stream})
	cardSelector := strings.TrimSpace(card)
	if entries := j.entries(t, stream); len(entries) != 0 {
		t.Fatalf("the delivery entry is listed while the brief is pending: %+v", entries)
	}
	p.Await("no delivery card while the manager writes the brief", `document.querySelector(`+browsertest.Quote(cardSelector)+`) === null`)

	j.manager.release()
	p.Await("the delivery card", `document.querySelector(`+browsertest.Quote(cardSelector)+`) !== null`)
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
  const brief = document.querySelector(`+browsertest.Quote(strings.TrimSpace(brief))+`);
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
  const img = document.querySelector(`+browsertest.Quote(strings.TrimSpace(brief)+" a[data-field=image-link] img")+`);
  return !!img && img.complete && img.naturalWidth > 0;
})()`)
	p.AwaitText(card+"[data-field=pins]", "final review 1")
}
