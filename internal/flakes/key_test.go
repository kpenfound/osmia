package flakes

import (
	"testing"

	"github.com/kpenfound/osmia/internal/intake"
)

func TestHandInKeyIsDeterministicAndDistinct(t *testing.T) {
	t.Parallel()
	key := HandInKey(project, groupA, 0)
	if again := HandInKey(project, groupA, 0); again != key {
		t.Fatalf("key changed: %q, %q", key, again)
	}
	seen := map[string]string{key: "base"}
	for name, other := range map[string]string{
		"other project":    HandInKey("p_00000000000000000000000000000002", groupA, 0),
		"other group":      HandInKey(project, groupB, 0),
		"next generation":  HandInKey(project, groupA, 1),
		"later generation": HandInKey(project, groupA, 2),
	} {
		if prior, dup := seen[other]; dup {
			t.Errorf("%s has the key of %s: %q", name, prior, other)
		}
		seen[other] = name
	}
}

func TestHandInKeyIsValidForIntake(t *testing.T) {
	t.Parallel()
	for _, group := range []string{groupA, "test-split/test-cli-engine", "ünïcode group", "a/b?c=d&e=f#g", string(make([]byte, 4096))} {
		for generation := range 3 {
			key := HandInKey(project, group, generation)
			if !intake.HandInKey.MatchString(key) {
				t.Errorf("key %q for %q generation %d is not a valid hand-in key", key, group, generation)
			}
		}
	}
	a, b := intake.HandInWorkstream(project, HandInKey(project, groupA, 0)), intake.HandInWorkstream(project, HandInKey(project, groupA, 1))
	if a == b {
		t.Fatalf("generations share workstream %s", a)
	}
}

func TestGroupAndOccurrenceIDsAreOnePerGroupAndPerRunAndGroup(t *testing.T) {
	t.Parallel()
	if GroupID(groupA) == GroupID(groupB) {
		t.Fatal("groups share an ID")
	}
	ids := map[string]string{}
	for name, id := range map[string]string{
		"run 1 group a": OccurrenceID("r1", groupA), "run 2 group a": OccurrenceID("r2", groupA), "run 1 group b": OccurrenceID("r1", groupB),
	} {
		if prior, dup := ids[id]; dup {
			t.Errorf("%s has the ID of %s", name, prior)
		}
		ids[id] = name
	}
	again := []string{GroupID(groupA), OccurrenceID("r1", groupA)}
	if again[0] != GroupID(groupA) || again[1] != OccurrenceID("r1", groupA) {
		t.Fatal("IDs change between calls")
	}
	if !intake.HandInKey.MatchString(GroupID(groupA)) {
		t.Fatalf("group ID %q is not a trace key", GroupID(groupA))
	}
}
