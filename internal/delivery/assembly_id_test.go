package delivery

import (
	"testing"

	"github.com/kpenfound/osmia/internal/trace"
)

func TestAssemblyIDIsUniquePerAssemblyAndStableAcrossRetries(t *testing.T) {
	t.Parallel()
	move := func(id, from, to string) trace.Transition {
		return trace.Transition{Header: trace.Header{ID: id}, Subject: trace.FeatureSubject, From: from, To: to}
	}
	var transitions []trace.Transition
	assemble := func(want string) {
		t.Helper()
		id := AssemblyID(transitions)
		if retry := AssemblyID(transitions); id != want || retry != id {
			t.Fatalf("assembly ID %q, retried %q, want %q", id, retry, want)
		}
		transitions = append(transitions, move(id, BuildingState, AssembledState))
	}
	transitions = append(transitions, move("building", "ratified", BuildingState))
	assemble(AssembledState)
	for _, want := range []string{"assembled-2", "assembled-3"} {
		transitions = append(transitions, move("reopen-"+want, AssembledState, BuildingState))
		assemble(want)
	}
}

func TestAssemblyIDSkipsIDsTheTraceAlreadyHolds(t *testing.T) {
	t.Parallel()
	move := func(id, from, to string) trace.Transition {
		return trace.Transition{Header: trace.Header{ID: id}, Subject: trace.FeatureSubject, From: from, To: to}
	}
	held := []trace.Transition{
		move(AssembledState, BuildingState, AssembledState),
		move("reopen-1", AssembledState, BuildingState),
		move("assembled-seal-1", BuildingState, AssembledState),
		move("reopen-2", AssembledState, BuildingState),
		{Header: trace.Header{ID: "assembled-3"}, Subject: trace.UnitSubject("resume"), To: UnitMerged},
	}
	if id := AssemblyID(held); id != "assembled-4" {
		t.Fatalf("assembly ID %q, want assembled-4", id)
	}
}
