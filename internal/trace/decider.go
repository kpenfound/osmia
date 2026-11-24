package trace

import (
	"encoding/json"
	"regexp"
)

// Who decided an ordinary decision record. A record names its decider in
// DecidedBy; an empty value means the decider of the record's kind: the owner
// for the owner's rulings, ratifications and dispositions, the chief of staff
// for its own decisions. A decision Jev made is the same record with DecidedBy
// "jev" and Judgment naming the judgment record it comes from.
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

func validRulingActor(v Ruling) bool {
	if v.DecidedBy != DecidedByJev {
		return v.Actor != JevActor
	}
	relay := v.Revision > 1 && present(v.ReturnedAnswer) && v.Actor.Kind == "agent"
	return v.Actor == JevActor || relay
}

var (
	ratificationPath      = regexp.MustCompile(`^shed/round-[0-9]+/ratification\.json$`)
	amendmentDecisionPath = regexp.MustCompile(`^amendments/[^/]+/decision\.json$`)
	charterDecisionPath   = regexp.MustCompile(`^questions/[^/]+/charter\.json$`)
)

const deliveryApprovalPath = "final/delivery.json"

// ValidDecisionActor reports whether the header actor of a document that
// records an owner-gate decision agrees with the decider its content names: a
// ratification, a delivery approval and a charter proposal name their decider
// in decided_by and an amendment decision in actor, and Jev's decision is
// written by the Jev actor and nobody else's is. A charter proposal's later
// revisions are the service's, so the Jev actor writes only one Jev decided.
// Other documents are not decisions.
func ValidDecisionActor(d Document) bool {
	var by string
	switch {
	case d.Path == deliveryApprovalPath:
		var c struct {
			DecidedBy string `json:"decided_by"`
		}
		if json.Unmarshal([]byte(d.Content), &c) != nil {
			return true
		}
		by = c.DecidedBy
	case charterDecisionPath.MatchString(d.Path):
		var c struct {
			Decision  string `json:"decision"`
			DecidedBy string `json:"decided_by"`
		}
		if json.Unmarshal([]byte(d.Content), &c) != nil {
			return true
		}
		return d.Actor != JevActor || c.Decision != "" && c.DecidedBy == DecidedByJev
	case ratificationPath.MatchString(d.Path):
		var c struct {
			DecidedBy string `json:"decided_by"`
		}
		if json.Unmarshal([]byte(d.Content), &c) != nil {
			return true
		}
		by = c.DecidedBy
	case amendmentDecisionPath.MatchString(d.Path):
		var c struct {
			Actor string `json:"actor"`
		}
		if json.Unmarshal([]byte(d.Content), &c) != nil {
			return true
		}
		by = c.Actor
	default:
		return true
	}
	return (by == DecidedByJev) == (d.Actor == JevActor)
}
