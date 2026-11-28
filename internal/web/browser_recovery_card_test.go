package web

import (
	"fmt"
	"testing"
)

const recoveryOccurrence = "unit-index@4/unit.stalled/stalled-20260101T000000Z"

func recoveryMoveJSON(origin string) string {
	return fmt.Sprintf(`{"occurrence":%q,"machine":"unit","subject":"unit-index","unit":"index","from":"checking","version":4,"target":"checking","origin":%q,"reason":"unit index has made no progress and neither Jev nor the manager cleared the stall within a further 45 minutes, so starting checking again is yours to approve"}`, recoveryOccurrence, origin)
}

func recoveryEntry() string {
	body := fmt.Sprintf(`{"occurrence":%q,"subject":"unit-index","unit":"index","from":"checking","version":4,"target":"checking"}`, recoveryOccurrence)
	return `{"kind":"recovery","project":"p_fake","workstream":"w_000","unit":"index","options":["approve"],"asked":[],"question":"Approve this recovery move: unit index goes from checking (version 4) to checking.","blocked":"Unit index.","recovery":` + recoveryMoveJSON("stall") + `,"answer":{"method":"POST","path":"/v1/recovery/w_000","body":` + body + `}}`
}

func TestBrowserRecoveryCardShowsAndSendsThePinnedMove(t *testing.T) {
	t.Parallel()
	f := newDecisionButtonFixture(t, recoveryEntry())
	card := `[data-decision][data-kind="recovery"] `
	assertDecisionButtons(f.p, card, map[string]string{"approve": "Approve this move"})
	assertButtonStyles(f.p, card, "approve")
	f.p.awaitText(card+"[data-field=kind]", "Approve this recovery move")
	for field, want := range map[string]string{"subject": "Unit index", "from": "checking, version 4", "target": "checking"} {
		f.p.awaitText(card+"[data-field=move] [data-field="+field+"]", want)
	}
	f.p.awaitText(card+"[data-field=move] [data-field=reason]", "neither Jev nor the manager cleared the stall")
	f.p.awaitText(card+"[data-field=pins]", "version 4")
	f.p.click(card + `[data-decision-choice="approve"]`)
	f.p.awaitGone("the approved card", card)
	f.p.await("the recorded outcome", `document.querySelectorAll('#workstreams [data-field=feed] [data-kind=outcome][data-outcome=ok]').length === 1`)
	assertDecisionRequest(t, f, "/v1/recovery/w_000", map[string]any{"occurrence": recoveryOccurrence, "subject": "unit-index", "unit": "index", "from": "checking", "version": float64(4), "target": "checking"})
}

func TestBrowserRecoveryCardsOfOneWorkstreamStayApart(t *testing.T) {
	t.Parallel()
	other := `{"kind":"recovery","project":"p_fake","workstream":"w_000","unit":"","options":["approve"],"asked":[],"question":"Approve this recovery move: workstream w_000 goes from assembled (version 7) to assembled.","recovery":{"occurrence":"feature@7/ws.stalled/stalled-1","machine":"ws","subject":"feature","from":"assembled","version":7,"target":"assembled","origin":"stall","reason":"the workstream has made no progress"},"answer":{"method":"POST","path":"/v1/recovery/w_000","body":{"occurrence":"feature@7/ws.stalled/stalled-1","subject":"feature","from":"assembled","version":7,"target":"assembled"}}}`
	f := newDecisionButtonFixture(t, recoveryEntry()+","+other)
	f.p.await("two recovery cards", `document.querySelectorAll('[data-decision][data-kind=recovery]').length === 2`)
}

func TestBrowserActivityShowsARecoveryCardAndKeepsItsIdentity(t *testing.T) {
	entry := func(state string) string {
		return `{"kind":"recovery","at":"2026-01-01T00:00:01Z","recovery":{"occurrence":"` + recoveryOccurrence + `","machine":"unit","subject":"unit-index","unit":"index","from":"checking","version":4,"target":"checking","origin":"stall","reason":"no progress","state":"` + state + `"}}`
	}
	f := newOutcomeFeedFixture(t, 1, func(api *fakeAPI) { api.setFeed("w_000", entry("open")) })
	p := f.p
	p.openActivity()
	line := "#workstream-activity [data-kind=recovery]"
	p.await("the recovery line", `document.querySelectorAll('`+line+`[data-state=open]').length === 1`)
	p.awaitText(line+" [data-field=state]", "waiting for you")
	p.awaitText(line+" [data-field=subject]", "Unit index")
	var before string
	p.eval(`document.querySelector('`+line+`').dataset.entry`, &before)

	f.api.setFeed("w_000", entry("approved"))
	f.api.pushEvent("resync")
	p.await("the approved line", `document.querySelectorAll('`+line+`[data-state=approved]').length === 1`)
	var after string
	p.eval(`document.querySelector('`+line+`').dataset.entry`, &after)
	if before == "" || before != after {
		t.Fatalf("the recovery entry was named %q open and %q approved", before, after)
	}
}
