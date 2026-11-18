package intake

import (
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// A committee objection the chief of staff escalated that blocks ratification
// is an escalated-objection gate. The owner's own objection and a conflict
// between member charters that block are the owner's own decisions, which Jev
// decides at the owner threshold. One already disposed of, advice and one the
// chief of staff kept are nobody's gate.
func TestTheObjectionsJevDecidesAtTheRatificationGate(t *testing.T) {
	escalated := shed.Entry{Dissent: shed.Dissent{Objection: shed.Objection{ID: "o1", Kind: shed.Charter}, Member: "agent_committee_1"}, Blocking: true, Escalated: true}
	if kind, ok := objectionKind(escalated); !ok || kind != jev.KindEscalatedObjection || !objectionIsJevs(escalated) {
		t.Fatalf("an escalated blocking committee objection is %q %v", kind, ok)
	}
	for name, c := range map[string]struct {
		change func(*shed.Entry)
		kind   jev.DecisionKind
	}{
		"the owner's own objection":   {func(e *shed.Entry) { e.Member, e.Kind, e.Escalated = shed.OwnerMember, shed.Owner, false }, jev.KindOwnerObjection},
		"a charter conflict":          {func(e *shed.Entry) { e.Kind = shed.CharterConflict }, jev.KindCharterConflict},
		"one the chief did not raise": {func(e *shed.Entry) { e.Escalated = false }, ""},
		"advice that blocks nothing":  {func(e *shed.Entry) { e.Blocking = false }, ""},
		"one already dismissed":       {func(e *shed.Entry) { e.Disposition, e.Blocking = shed.Dismissed, false }, ""},
		"one the owner sustained":     {func(e *shed.Entry) { e.Disposition = shed.Sustained }, ""},
	} {
		e := escalated
		c.change(&e)
		kind, ok := objectionKind(e)
		if ok != (c.kind != "") || kind != c.kind {
			t.Errorf("%s is %q %v, want %q", name, kind, ok, c.kind)
		}
		if c.kind != "" && !c.kind.Owner() {
			t.Errorf("%s is not one of the owner's own decisions", name)
		}
	}
	if jev.KindEscalatedObjection.Owner() {
		t.Error("an escalated objection is held to the owner threshold")
	}
}

// An escalation whose questions report a conflict between member charters is
// one of the owner's own decisions: its gate is of the charter-conflict kind and
// offers the options the chief of staff wrote.
func TestAnEscalationOfACharterConflictIsAnOwnerGate(t *testing.T) {
	_, repo, _ := fixturekit.HardPauseRepository(t, 0)
	question := trace.Question{Header: trace.Header{ID: "1", Workstream: fixturekit.Stream}, AskedBy: trace.Actor{Kind: "agent", ID: "mason1"}, Question: "Which charter wins?", CharterConflict: []string{"p1#1", "p2#2"}}
	entry := trace.InboxEntry{Number: 1, State: trace.QuestionEscalated, Options: []string{"Follow p1.", "Follow p2."}, Recommendation: "Follow p1.", Questions: []trace.QuestionState{{Asked: question, State: trace.QuestionEscalated}}}
	c := New(Dependencies{})
	gate, carried, err := c.escalationGate(repo, entry, InboxView(entry))
	if err != nil || gate == nil || carried != nil || gate.Kind != jev.KindCharterConflict || len(gate.Options) != 2 {
		t.Fatalf("gate %+v carried %+v: %v", gate, carried, err)
	}
	entry.Options = nil
	if gate, _, err := c.escalationGate(repo, entry, InboxView(entry)); err != nil || gate != nil {
		t.Fatalf("an escalation without the chief's options is a gate: %+v %v", gate, err)
	}
}

// Jev never writes text: an option that is carried out with text is offered
// only when the chief of staff wrote that text, once for each distinct text.
func TestAnOptionThatNeedsTextIsOfferedOnlyWithTheChiefsText(t *testing.T) {
	options := TextOptions([]string{"Keep the project.", "  ", "", "Drop it.", "Keep the project."})
	if len(options) != 2 || options[0] != (jev.DecisionOption{Name: "option-1", Description: "Keep the project."}) || options[1] != (jev.DecisionOption{Name: "option-4", Description: "Drop it."}) {
		t.Fatalf("options %+v", options)
	}
	if options := TextOptions(nil); len(options) != 0 {
		t.Fatalf("options without text %+v", options)
	}
}

// A card is decorated with Jev's result only when Jev gave one, and a
// deferral keeps the chief of staff's recommendation behind Jev's word.
func TestAJevCardKeepsTheChiefsRecommendationOnlyWhenJevGaveNoPick(t *testing.T) {
	card := func() InboxEntry {
		return InboxEntry{Recommendation: "ratify: nothing blocks.", QuickReply: "ratify: nothing blocks."}
	}
	e := card()
	applyJevCard(&e, "", nil)
	if e.Recommendation != "ratify: nothing blocks." || e.Jev != nil {
		t.Fatalf("a result Jev did not give changed the card: %+v", e)
	}
	e = card()
	applyJevCard(&e, "Jev recommends ratify.", &api.InboxJev{Pick: "ratify", Confidence: 0.7, Threshold: 0.9})
	if e.Recommendation != "Jev recommends ratify." || e.Jev == nil || e.QuickReply != "" {
		t.Fatalf("a below-threshold pick: %+v", e)
	}
	e = card()
	applyJevCard(&e, "Jev deferred this decision to you.", &api.InboxJev{Deferred: true})
	if e.Recommendation != "Jev deferred this decision to you. ratify: nothing blocks." || !e.Jev.Deferred {
		t.Fatalf("a deferral: %+v", e)
	}
}

// A pick of an option the chief of staff wrote is named to the owner by its
// text, and the other options by their names.
func TestAPickIsNamedByTheChiefsTextForAnEscalationAndByNameOtherwise(t *testing.T) {
	options := []jev.DecisionOption{{Name: "option-1", Description: "Resume from the last chunk."}, {Name: "option-2", Description: "Restart."}}
	if got := optionLabel(options, "option-2"); got != "Restart." {
		t.Fatalf("label %q", got)
	}
	if got := optionLabel([]jev.DecisionOption{{Name: "ratify", Description: "Ratify it."}}, "ratify"); got != "ratify" {
		t.Fatalf("label %q", got)
	}
}
