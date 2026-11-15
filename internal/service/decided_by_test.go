package service

import (
	"testing"

	"github.com/kpenfound/osmia/internal/trace"
)

// The trace walk names who decided a ruling: the record's own decider unless
// it names one, so a ruling Jev decided shows as Jev's with its judgment.
func TestTraceRulingsNameTheirDecider(t *testing.T) {
	w := &traceWalk{}
	for _, c := range []struct {
		name    string
		ruling  trace.Ruling
		decider string
	}{
		{"the owner's ruling", trace.Ruling{Decision: trace.DecisionRuling, OwnerResponse: "Files."}, trace.DecidedByOwner},
		{"the chief of staff's answer", trace.Ruling{Decision: trace.DecisionAnswer, ReturnedAnswer: "Files."}, trace.DecidedByChief},
		{"Jev's ruling", trace.Ruling{Decision: trace.DecisionRuling, OwnerResponse: "Files.", DecidedBy: trace.DecidedByJev, Judgment: "judgment_ab12"}, trace.DecidedByJev},
	} {
		got := w.questionRuling(c.ruling)
		if got.DecidedBy != c.decider || got.Judgment != c.ruling.Judgment {
			t.Errorf("%s: decided by %q with judgment %q, want %q and %q", c.name, got.DecidedBy, got.Judgment, c.decider, c.ruling.Judgment)
		}
	}
	for by, want := range map[string]string{"": trace.DecidedByOwner, "owner": trace.DecidedByOwner, "chief of staff": trace.DecidedByChief, "jev": trace.DecidedByJev} {
		if got := contestDecider(by); got != want {
			t.Errorf("contestDecider(%q) = %q, want %q", by, got, want)
		}
	}
}
