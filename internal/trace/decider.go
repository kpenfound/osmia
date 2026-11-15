package trace

// Who decided an ordinary decision record. A record names its decider in
// DecidedBy; an empty value is the one the record's kind has always had: the
// owner for the owner's rulings, ratifications and dispositions, the chief of
// staff for its own decisions. A decision Jev made is the same record with
// DecidedBy "jev" and Judgment naming the judgment record it comes from.
const (
	DecidedByOwner = "owner"
	DecidedByChief = ChiefOfStaff
	DecidedByJev   = "jev"
)

// JevActor is the trace actor of a record Jev's decision wrote.
var JevActor = Actor{Kind: "service", ID: DecidedByJev}

// ValidDecider reports whether by and judgment form a valid decider of a
// record: an empty or known decider, and a judgment key exactly when Jev
// decided.
func ValidDecider(by, judgment string) bool {
	switch by {
	case "", DecidedByOwner, DecidedByChief:
		return judgment == ""
	case DecidedByJev:
		return key(judgment)
	}
	return false
}

// Decider returns the effective decider of a record, the record's own
// default when it names none.
func Decider(by, fallback string) string {
	if by == "" {
		return fallback
	}
	return by
}
