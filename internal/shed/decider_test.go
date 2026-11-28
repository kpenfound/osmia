package shed_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestGateRecordsDecidedByJevRoundTripWithTheirJudgment(t *testing.T) {
	t.Parallel()
	ratification := shed.Ratify(3, one, []shed.Entry{entry("a-r1-1", shed.Fit, "")})
	ratification.DecidedBy, ratification.Judgment = trace.DecidedByJev, "judgment_ab12"
	data, err := shed.EncodeRatification(ratification)
	if err != nil {
		t.Fatal(err)
	}
	got, err := shed.ParseRatification(data)
	if err != nil || !reflect.DeepEqual(got, ratification) {
		t.Fatalf("ratification round trip %+v %v", got, err)
	}
	owner, err := shed.EncodeRatification(shed.Ratify(3, one, nil))
	if err != nil || string(owner) == string(data) || strings.Contains(string(owner), "decided_by") || strings.Contains(string(owner), "judgment") {
		t.Fatalf("the owner's ratification carries a decider: %s %v", owner, err)
	}
	for name, broken := range map[string]func(*shed.Ratification){
		"Jev without a judgment": func(r *shed.Ratification) { r.Judgment = "" },
		"a judgment without Jev": func(r *shed.Ratification) { r.DecidedBy = "" },
		"the manager":     func(r *shed.Ratification) { r.DecidedBy, r.Judgment = trace.DecidedByManager, "" },
	} {
		next := ratification
		broken(&next)
		if _, err := shed.EncodeRatification(next); err == nil {
			t.Fatalf("a ratification with %s was encoded", name)
		}
	}

	ruled := rulings(1, one, shed.Ruling{Objection: "a-r1-1", Disposition: shed.Dismissed, Note: "Settled.", DecidedBy: trace.DecidedByJev, Judgment: "judgment_ab12"})
	data, err = shed.EncodeRulings(ruled)
	if err != nil {
		t.Fatal(err)
	}
	if back, err := shed.ParseRulings(data); err != nil || !reflect.DeepEqual(back, ruled) {
		t.Fatalf("rulings round trip %+v %v", back, err)
	}
	ruled.Rulings[0].DecidedBy = trace.DecidedByManager
	if _, err := shed.EncodeRulings(ruled); err == nil {
		t.Fatal("an owner ruling decided by the manager was encoded")
	}

	decisions := manager(1, one, shed.ManagerDecision{Objection: "a-r1-1", Decision: shed.Dismiss, Note: "Answered.", DecidedBy: trace.DecidedByJev, Judgment: "judgment_ab12"})
	data, err = shed.EncodeManagerDecisions(decisions)
	if err != nil {
		t.Fatal(err)
	}
	if back, err := shed.ParseManagerDecisions(data); err != nil || !reflect.DeepEqual(back, decisions) {
		t.Fatalf("chief decisions round trip %+v %v", back, err)
	}
	decisions.Decisions[0].Judgment = ""
	if _, err := shed.EncodeManagerDecisions(decisions); err == nil {
		t.Fatal("a Jev decision without a judgment was encoded")
	}
}
