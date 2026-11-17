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

// routedTransitionEntry is the JSON of a feed entry for a workstream
// transition the transition router chose: who decided it, the default it
// would have entered, and whether Jev departed from that default to make it.
func routedTransitionEntry(at, to, actor, decidedBy, defaultTo string, departed bool) string {
	return fmt.Sprintf(`{"kind":"transition","at":%s,"transition":{"subject":"feature","from":"planning","to":%s,"actor":%s,"reason":"","decided_by":%s,"default_to":%s,"departed":%t}}`,
		quote(at), quote(to), quote(actor), quote(decidedBy), quote(defaultTo), departed)
}

// Only a transition on which Jev departed from the default is labelled
// "decided by Jev" in Activity. A followed default, a fallback, an owner's
// pinned move, one of Jev's gate decisions, a reviewer's verdict and a
// transition no router chose carry no such label, whoever recorded them.
func TestBrowserActivityLabelsOnlyJevDepartures(t *testing.T) {
	f := newOutcomeFeedFixture(t, 1, func(api *fakeAPI) {
		api.setFeed("w_000",
			routedTransitionEntry("2026-01-01T00:00:01Z", "departed", "service:jev", "jev", "ratified", true),
			routedTransitionEntry("2026-01-01T00:00:02Z", "followed", "service:osmia", "code", "followed", false),
			routedTransitionEntry("2026-01-01T00:00:03Z", "fallback", "service:osmia", "fallback", "fallback", false),
			routedTransitionEntry("2026-01-01T00:00:04Z", "pinned", "owner:local", "owner", "ratified", false),
			routedTransitionEntry("2026-01-01T00:00:05Z", "gate", "service:jev", "jev_gate", "gate", false),
			routedTransitionEntry("2026-01-01T00:00:06Z", "verdict", "service:reviewer", "reviewer", "verdict", false),
			actorTransitionEntry("2026-01-01T00:00:07Z", "unrouted", "service:jev"),
			actorTransitionEntry("2026-01-01T00:00:08Z", "building", "owner:local"),
			actorTransitionEntry("2026-01-01T00:00:09Z", "assembled", "agent:chief_of_staff"))
	})
	p := f.p
	p.openActivity()
	p.await("the nine transitions", `document.querySelectorAll('#workstream-activity [data-kind=transition]').length === 9`)
	var labels string
	p.eval(`[...document.querySelectorAll('#workstream-activity [data-kind=transition]')].map(n => n.dataset.to + '=' + [...n.querySelectorAll('[data-field=decided-by]')].map(l => l.textContent).join('')).join(',')`, &labels)
	if want := "departed=decided by Jev,followed=,fallback=,pinned=,gate=,verdict=,unrouted=,building=,assembled="; labels != want {
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
