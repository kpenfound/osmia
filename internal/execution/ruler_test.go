package execution

import (
	"testing"

	"github.com/kpenfound/osmia/internal/trace"
)

func TestContestedRulingByJev(t *testing.T) {
	if got := RulerName(trace.JevActor); got != trace.DecidedByJev {
		t.Fatalf("rulerName(Jev) = %q", got)
	}
	if RulerName(managerActor) != "chief of staff" || RulerName(ownerActor) != "owner" {
		t.Fatal("rulerName changed for the owner or the chief of staff")
	}
	jev := ContestedRuling{Decision: "review", By: trace.DecidedByJev, Judgment: "judgment_ab12"}
	if contestActor(jev) != trace.JevActor || ContestRuler(jev) != "decision maker Jev" {
		t.Fatalf("Jev's ruling: actor %+v, ruler %q", contestActor(jev), ContestRuler(jev))
	}
	if contestActor(ContestedRuling{}) != ownerActor || ContestRuler(ContestedRuling{}) != "owner" {
		t.Fatal("an unattributed ruling is not the owner's")
	}
	if contestActor(ContestedRuling{By: "chief of staff"}) != managerActor {
		t.Fatal("the chief of staff's ruling is not the chief's")
	}
}
