package intake

import "github.com/kpenfound/osmia/internal/trace"

// Deciding says who makes an owner-gate decision through the owner commands'
// path: the zero value is the owner, and a Judgment names the Jev judgment
// record when Jev decides the gate. The decision is the ordinary record of its
// kind either way, with Jev's marked as Jev's.
type Deciding struct{ Judgment string }

// Jev reports whether Jev makes the decision.
func (d Deciding) Jev() bool { return d.Judgment != "" }

// Actor is the trace actor of the records the decision writes.
func (d Deciding) Actor() trace.Actor {
	if d.Jev() {
		return trace.JevActor
	}
	return OwnerActor
}

// By is the decider the decision's record names: empty for the owner.
func (d Deciding) By() string {
	if d.Jev() {
		return trace.DecidedByJev
	}
	return ""
}

// Name is who decided, as the decision's reason says it.
func (d Deciding) Name() string {
	if d.Jev() {
		return "Jev"
	}
	return "the owner"
}

// deciding returns who a recorded decider was, from the decider and judgment
// its record names.
func decidingOf(by, judgment string) Deciding {
	if by == trace.DecidedByJev {
		return Deciding{Judgment: judgment}
	}
	return Deciding{}
}
