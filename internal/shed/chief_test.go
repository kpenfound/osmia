package shed_test

import (
	"reflect"
	"testing"

	"github.com/kpenfound/osmia/internal/shed"
)

func chief(round int, pin shed.Pin, ds ...shed.ChiefDecision) shed.ChiefDecisions {
	return shed.ChiefDecisions{Version: shed.Version, Round: round, Revision: pin, Decisions: ds}
}

func TestChiefDecisionsRoundTripAndRefuseInvalidContent(t *testing.T) {
	t.Parallel()
	c := chief(1, one, shed.ChiefDecision{Objection: "agent_committee_1-r1-1", Decision: shed.Dismiss, Note: "The reply answers it.", Turn: "events_1"})
	data, err := shed.EncodeChiefDecisions(c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := shed.ParseChiefDecisions(data)
	if err != nil || !reflect.DeepEqual(got, c) {
		t.Fatalf("round trip %+v %v", got, err)
	}
	for name, broken := range map[string]func(*shed.ChiefDecisions){
		"an unknown decision": func(c *shed.ChiefDecisions) { c.Decisions[0].Decision = "sustain" },
		"no note":             func(c *shed.ChiefDecisions) { c.Decisions[0].Note = " " },
		"no objection":        func(c *shed.ChiefDecisions) { c.Decisions[0].Objection = "" },
		"two decisions":       func(c *shed.ChiefDecisions) { c.Decisions = append(c.Decisions, c.Decisions[0]) },
		"no round":            func(c *shed.ChiefDecisions) { c.Round = 0 },
	} {
		next := chief(1, one, shed.ChiefDecision{Objection: "agent_committee_1-r1-1", Decision: shed.Dismiss, Note: "The reply answers it."})
		broken(&next)
		if _, err := shed.EncodeChiefDecisions(next); err == nil {
			t.Fatalf("%s was encoded", name)
		}
	}
	if _, err := shed.Encode(record(1, "chief", one, 1)); err == nil {
		t.Fatal("a member named chief was encoded")
	}
}

func TestChiefDecisionsShapeTheDissentTheOwnerSees(t *testing.T) {
	t.Parallel()
	veto, advice, vetoed := shed.ObjectionID(1, alice, 1), shed.ObjectionID(1, bob, 1), shed.ObjectionID(1, alice, 2)
	a := record(1, alice, one, 2)
	a.Objections[0].Kind, a.Objections[1].Kind = shed.Charter, shed.Charter
	records := []shed.Record{a, record(1, bob, one, 1), ownerRecord(1, one, 1)}

	entries := shed.DissentRecord(records, nil, nil)
	if undecided := shed.Undecided(entries); len(undecided) != 3 || undecided[0].ID != veto || undecided[1].ID != vetoed || undecided[2].ID != advice {
		t.Fatalf("undecided before the chief of staff decided %+v", undecided)
	}

	decided := []shed.ChiefDecisions{chief(1, one,
		shed.ChiefDecision{Objection: veto, Decision: shed.Dismiss, Note: "The charter rule does not apply."},
		shed.ChiefDecision{Objection: vetoed, Decision: shed.Escalate, Note: "It may break rule 2."},
		shed.ChiefDecision{Objection: advice, Decision: shed.Escalate, Note: "A judgement call."},
		shed.ChiefDecision{Objection: "owner-r1-1", Decision: shed.Dismiss, Note: "Not mine to decide."})}
	entries = shed.DissentRecord(records, nil, decided)
	byID := map[string]shed.Entry{}
	for _, e := range entries {
		byID[e.ID] = e
	}
	if e := byID[veto]; e.Blocking || e.Disposition != shed.Dismissed || e.By != shed.ChiefOfStaff || e.Note != "The charter rule does not apply." {
		t.Fatalf("the dismissed veto %+v", e)
	}
	if e := byID[vetoed]; !e.Blocking || e.Disposition != "" || !e.Escalated || e.Escalation != "It may break rule 2." {
		t.Fatalf("the escalated veto %+v", e)
	}
	if e := byID["owner-r1-1"]; !e.Blocking || e.Disposition != "" || e.By != "" || e.Escalated {
		t.Fatalf("the owner's objection %+v", e)
	}
	if undecided := shed.Undecided(entries); len(undecided) != 0 {
		t.Fatalf("undecided once the chief of staff decided %+v", undecided)
	}
	if shown := shed.ForOwner(entries); len(shown) != 3 || shown[0].ID == veto || shown[1].ID == veto || shown[2].ID == veto {
		t.Fatalf("the owner is shown %+v", shown)
	}
	if standing := shed.Standing(entries); len(standing) != 3 {
		t.Fatalf("a dismissed objection is still answered: %+v", standing)
	}
	if r := shed.Ratify(1, one, entries); len(r.Dispositions) != 0 || len(r.Dissent) != 4 {
		t.Fatalf("the ratification %+v", r)
	}

	overruled := shed.DissentRecord(records, []shed.Rulings{rulings(1, one,
		shed.Ruling{Objection: veto, Disposition: shed.Sustained, Note: "It applies."},
		shed.Ruling{Objection: vetoed, Disposition: shed.Overruled})}, decided)
	for _, e := range overruled {
		switch e.ID {
		case veto:
			if !e.Blocking || e.Disposition != shed.Sustained || e.By != "" || e.Note != "It applies." {
				t.Fatalf("the veto the owner sustained over the dismissal %+v", e)
			}
		case vetoed:
			if e.Blocking || e.Disposition != shed.Overruled || !e.Escalated {
				t.Fatalf("the escalated veto the owner overruled %+v", e)
			}
		}
	}
}
