package telemetry

import (
	"slices"
	"testing"
)

func TestRoutingAttributesAreAllowListed(t *testing.T) {
	for name, a := range map[string]Attr{
		"decided by": DecidedBy("jev_gate"), "default state": DefaultState("in-shed"), "deviated": Deviated(false),
		"trigger": Trigger("unit.checks_failed"), "confidence": Confidence("binding"),
		"judgment": Judgment("judgment_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"),
	} {
		if !a.Valid() {
			t.Errorf("%s is not an allow-listed attribute", name)
		}
	}
	for _, key := range []string{KeyDecidedBy, KeyDefaultState, KeyDeviated, KeyTrigger, KeyConfidence, KeyJudgment} {
		if !slices.Contains(AttributeKeys(), key) {
			t.Errorf("%s is not on the allow-list", key)
		}
	}
	for name, a := range map[string]Attr{
		"decided by as free text":   DecidedBy("Jev thought it through"),
		"trigger as free text":      Trigger("the checks failed"),
		"confidence as a number":    Confidence("0.93 confident"),
		"confidence uppercase":      Confidence("High"),
		"judgment as free text":     Judgment("the judgment said so"),
		"default state with spaces": DefaultState("not a state"),
		"a number for deviated":     Int(KeyDeviated, 1),
		"a string for deviated":     String(KeyDeviated, "true"),
		"a boolean for decided by":  Bool(KeyDecidedBy, true),
		"an empty judgment":         Judgment(""),
	} {
		if a.Valid() {
			t.Errorf("%s was accepted as %q", name, a.Key())
		}
	}
}
