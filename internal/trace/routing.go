package trace

// Who decided a state change that went through the transition router. A
// transition names its decider in DecidedBy; an empty value is a transition no
// router chose, such as one of a step journal.
//
//   - DecidedByCode: the router committed the default, either because it was
//     the only legal target or because Jev's answers kept it.
//   - DecidedByJev: Jev departed from the default; DefaultTo names the default
//     and Judgment the judgment record. It is the only value that reads as a
//     departure.
//   - DecidedByFallback: Jev could not answer, so the default was committed.
//   - DecidedByOwner: an owner decision, applied as it was given.
//   - DecidedByJevGate: one of Jev's existing gate decisions, applied as it was
//     given; Judgment names the judgment record.
//   - DecidedByReviewer: a satisfactory reviewer verdict for the current
//     candidate authorised the move.
//   - DecidedByChief: the chief of staff's own move or ruling, applied as it
//     was given.
const (
	DecidedByCode     = "code"
	DecidedByFallback = "fallback"
	DecidedByJevGate  = "jev_gate"
	DecidedByReviewer = "reviewer"
)

// Routing is how a transition came to be chosen: who decided it, the Jev
// judgment record behind it, and the state the default would have been.
type Routing struct {
	DecidedBy string `json:"decided_by,omitempty"`
	Judgment  string `json:"judgment,omitempty"`
	DefaultTo string `json:"default_to,omitempty"`
}

// Routing returns how t was chosen; the zero Routing for a transition no
// router chose.
func (t Transition) Routing() Routing {
	return Routing{DecidedBy: t.DecidedBy, Judgment: t.Judgment, DefaultTo: t.DefaultTo}
}

func (t Transition) withRouting(r Routing) Transition {
	t.DecidedBy, t.Judgment, t.DefaultTo = r.DecidedBy, r.Judgment, r.DefaultTo
	return t
}

// Deviated reports whether t entered a state other than its default, and
// whether t records a default at all.
func (t Transition) Deviated() (deviated, known bool) {
	return t.DefaultTo != "" && t.DefaultTo != t.To, t.DefaultTo != ""
}

// Departed reports whether Jev departed from the default to commit t: the one
// reading of a transition that a surface labels as decided by Jev. A followed,
// fallback, pinned or reviewer-authorised transition is never one.
func (t Transition) Departed() bool {
	deviated, _ := t.Deviated()
	return t.DecidedBy == DecidedByJev && deviated
}

// ValidRouting reports whether r is a valid way to have chosen a transition to
// state to. Nothing recorded is valid. Otherwise the decider is one of the
// values above, a judgment is a record key and is present exactly when Jev
// decided (a departure or a gate decision) or a code or reviewer decision
// cites the judgment that kept it, and a departure names a default it departs
// from.
func ValidRouting(to string, r Routing) bool {
	if r == (Routing{}) {
		return true
	}
	if r.DefaultTo != "" && !key(r.DefaultTo) || r.Judgment != "" && !key(r.Judgment) {
		return false
	}
	switch r.DecidedBy {
	case DecidedByJev:
		return r.Judgment != "" && r.DefaultTo != "" && r.DefaultTo != to
	case DecidedByJevGate:
		return r.Judgment != ""
	case DecidedByOwner, DecidedByFallback, DecidedByChief:
		return r.Judgment == ""
	case DecidedByCode, DecidedByReviewer:
		return true
	}
	return false
}
