package intake

import (
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// Only a committee objection the chief of staff escalated that blocks
// ratification is Jev's to dispose of. The owner's own objection, a conflict
// between member charters and one already disposed of are not, and neither is
// one the chief of staff kept or dismissed.
func TestOnlyAnEscalatedBlockingCommitteeObjectionIsJevs(t *testing.T) {
	escalated := shed.Entry{Dissent: shed.Dissent{Objection: shed.Objection{ID: "o1", Kind: shed.Charter}, Member: "agent_committee_1"}, Blocking: true, Escalated: true}
	if !objectionIsJevs(escalated) {
		t.Fatal("an escalated blocking committee objection is not Jev's")
	}
	for name, change := range map[string]func(*shed.Entry){
		"the owner's own objection":   func(e *shed.Entry) { e.Member, e.Kind = shed.OwnerMember, shed.Owner },
		"a charter conflict":          func(e *shed.Entry) { e.Kind = shed.CharterConflict },
		"one the chief did not raise": func(e *shed.Entry) { e.Escalated = false },
		"advice that blocks nothing":  func(e *shed.Entry) { e.Blocking = false },
		"one already dismissed":       func(e *shed.Entry) { e.Disposition, e.Blocking = shed.Dismissed, false },
		"one the owner sustained":     func(e *shed.Entry) { e.Disposition = shed.Sustained },
	} {
		e := escalated
		change(&e)
		if objectionIsJevs(e) {
			t.Errorf("%s is Jev's", name)
		}
	}
}

// An escalation whose questions report a conflict between member charters is the
// owner's: no gate is built for it, so Jev is never asked.
func TestAnEscalationOfACharterConflictIsNeverAGate(t *testing.T) {
	question := trace.Question{Header: trace.Header{ID: "1", Workstream: "w_00000000000000000000000000000001"}, AskedBy: trace.Actor{Kind: "agent", ID: "mason1"}, Question: "Which charter wins?", CharterConflict: []string{"p1#1", "p2#2"}}
	entry := trace.InboxEntry{Number: 1, State: trace.QuestionEscalated, Options: []string{"Follow p1.", "Follow p2."}, Recommendation: "Follow p1.", Questions: []trace.QuestionState{{Asked: question, State: trace.QuestionEscalated}}}
	c := New(Dependencies{})
	gate, carried, err := c.escalationGate(nil, entry, InboxView(entry))
	if err != nil || gate != nil || carried != nil {
		t.Fatalf("gate %+v carried %+v: %v", gate, carried, err)
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
