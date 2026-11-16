package web

import (
	"fmt"
	"testing"
)

// actorTransitionEntry is the JSON of a feed entry for a workstream
// transition recorded by actor.
func actorTransitionEntry(at, to, actor string) string {
	return fmt.Sprintf(`{"kind":"transition","at":%s,"transition":{"subject":"feature","from":"planning","to":%s,"actor":%s,"reason":""}}`,
		quote(at), quote(to), quote(actor))
}

// A transition Jev recorded is labelled "decided by Jev" in Activity; the
// owner's and the chief of staff's transitions carry no such label.
// Conversation notes each as a milestone line.
func TestBrowserActivityLabelsJevDecisions(t *testing.T) {
	f := newOutcomeFeedFixture(t, 1, func(api *fakeAPI) {
		api.setFeed("w_000",
			actorTransitionEntry("2026-01-01T00:00:01Z", "ratified", "service:jev"),
			actorTransitionEntry("2026-01-01T00:00:02Z", "building", "owner:local"),
			actorTransitionEntry("2026-01-01T00:00:03Z", "assembled", "agent:chief_of_staff"))
	})
	p := f.p
	p.awaitFeedKinds("milestone,milestone,milestone")
	p.openActivity()
	var labels string
	p.eval(`[...document.querySelectorAll('#workstream-activity [data-kind=transition]')].map(n => n.dataset.to + '=' + [...n.querySelectorAll('[data-field=decided-by]')].map(l => l.textContent).join('')).join(',')`, &labels)
	if want := "ratified=decided by Jev,building=,assembled="; labels != want {
		t.Fatalf("decision labels = %q, want %q", labels, want)
	}
	// The label is a footnote, the same kind of note as a card's "What was
	// asked".
	var footnotes int
	p.eval(`document.querySelectorAll('#workstream-activity [data-field=decided-by].footnote').length`, &footnotes)
	if footnotes != 1 {
		t.Fatalf("%d decided by labels are footnotes, want 1", footnotes)
	}
}
