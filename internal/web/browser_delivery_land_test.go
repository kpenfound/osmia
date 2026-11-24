package web

import (
	"fmt"
	"strings"
	"testing"
)

func landEntryJSON(pins deliveryPins) string {
	entry := deliveryEntryJSON(pins, false)
	entry = strings.Replace(entry, `"options":["approve"]`, `"options":["approve","land"]`, 1)
	land := fmt.Sprintf(`"land":{"method":"POST","path":"/v1/delivery/w_000/land","body":{"review":%d,"review_revision":%d,"commit":%q,"draft_hash":%q}},`, pins.review, pins.revision, pins.commit, pins.hash)
	return strings.Replace(entry, `"asked":[],`, `"asked":[],`+land, 1)
}

const landButton = deliveryCard + "[data-field=land]"

func TestBrowserDeliveryCardApproveAndLandSendsThePinnedIdentityToTheLandingEndpoint(t *testing.T) {
	t.Parallel()
	f := newDecisionButtonFixture(t, landEntryJSON(testPins), deliveryPresentationJSON(testPins, "one", "", `"can_land":true`))
	submit := deliveryCard + "button[type=submit]"
	f.p.await("both buttons", `document.querySelector(`+quote(landButton)+`)?.disabled === false && document.querySelector(`+quote(submit)+`)?.disabled === false`)
	var shown struct {
		Label, Mark     string
		Beside, Primary bool
		Note            string
	}
	f.p.eval(`(() => {
  const land = document.querySelector(`+quote(landButton)+`);
  const mark = document.querySelector(`+quote(submit)+`);
  return {
    Label: land.textContent,
    Mark: mark.textContent,
    Beside: land.parentElement === mark.parentElement && !!(mark.compareDocumentPosition(land) & Node.DOCUMENT_POSITION_FOLLOWING),
    Primary: mark.classList.contains('primary') && land.classList.contains('secondary'),
    Note: document.querySelector(`+quote(deliveryCard+"[data-field=authorization]")+`).textContent,
  };
})()`, &shown)
	if shown.Label != "Approve and land" || shown.Mark != "Mark ready for review" || !shown.Beside || !shown.Primary || !strings.Contains(shown.Note, "Approve and land moves the base branch") {
		t.Fatalf("the delivery card: %+v", shown)
	}
	f.p.click(landButton)
	f.p.awaitGone("the landed delivery", deliveryCard)
	f.p.awaitOutcome("Landed")
	assertDecisionRequest(t, f, "/v1/delivery/w_000/land", map[string]any{"review": float64(1), "review_revision": float64(2), "commit": testPins.commit, "draft_hash": testPins.hash})
}

func TestBrowserDeliveryCardOffersNoLandingOnAProjectThatDeliversByPullRequest(t *testing.T) {
	t.Parallel()
	f := newDeliveryFixture(t, false, deliveryPresentationJSON(testPins, "one", "", ""))
	submit := deliveryCard + "button[type=submit]"
	f.p.await("the button", `document.querySelector(`+quote(submit)+`)?.disabled === false`)
	var shown struct {
		Lands int
		Note  string
	}
	f.p.eval(`({
  Lands: document.querySelectorAll(`+quote(deliveryCard+"[data-field=land]")+`).length + [...document.querySelectorAll(`+quote(deliveryCard+"button")+`)].filter(b => b.textContent.includes('land')).length,
  Note: document.querySelector(`+quote(deliveryCard+"[data-field=authorization]")+`).textContent,
})`, &shown)
	if shown.Lands != 0 || strings.Contains(shown.Note, "land") {
		t.Fatalf("the card offers landing: %+v", shown)
	}
}

func TestBrowserDeliveryCardMakesLandingPrimaryWhenJevRecommendsIt(t *testing.T) {
	t.Parallel()
	f := newDecisionButtonFixture(t, landEntryJSON(testPins), deliveryPresentationJSON(testPins, "one", "", `"can_land":true,"recommendation":"land"`))
	f.p.await("the landing button", `document.querySelector(`+quote(landButton)+`)?.classList.contains('primary')`)
	var markPrimary bool
	f.p.eval(`document.querySelector(`+quote(deliveryCard+"button[type=submit]")+`).classList.contains('primary')`, &markPrimary)
	if markPrimary {
		t.Fatal("marking ready is still the primary action")
	}
}
