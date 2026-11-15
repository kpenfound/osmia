package execution

import (
	"testing"

	"github.com/kpenfound/osmia/internal/trace"
)

// A contested ruling Jev gave is recorded with Jev as its ruler and actor, and
// the roles it resumes are told who ruled.
func TestContestedRulingByJev(t *testing.T) {
	if got := rulerName(trace.JevActor); got != trace.DecidedByJev {
		t.Fatalf("rulerName(Jev) = %q", got)
	}
	if rulerName(chiefActor) != "chief of staff" || rulerName(ownerActor) != "owner" {
		t.Fatal("rulerName changed for the owner or the chief of staff")
	}
	jev := ContestedRuling{Decision: "review", By: trace.DecidedByJev, Judgment: "judgment_ab12"}
	if contestActor(jev) != trace.JevActor || contestRuler(jev) != "decision maker Jev" {
		t.Fatalf("Jev's ruling: actor %+v, ruler %q", contestActor(jev), contestRuler(jev))
	}
	if contestActor(ContestedRuling{}) != ownerActor || contestRuler(ContestedRuling{}) != "owner" {
		t.Fatal("an unattributed ruling is not the owner's")
	}
	if contestActor(ContestedRuling{By: "chief of staff"}) != chiefActor {
		t.Fatal("the chief of staff's ruling is not the chief's")
	}
}
