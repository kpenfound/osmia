package jev

import (
	"fmt"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// Require is the precondition of the chief of staff's outcome tool for a
// subject occurrence of a decision kind: the chief's decision is recorded only
// when a decide result exists for that same occurrence. It reads the
// occurrence's recorded result and never asks Jev.
//
// It returns why the outcome must be refused, or "". With a binding pick it
// also returns the result when outcome is the picked one, which the caller
// records the decision under; any other outcome is refused naming the pick. A
// below-threshold answer, a deferral, a fallback and a pick the service already
// refused permit any outcome, with no result returned. subject names the
// occurrence in the reason.
func Require(repo *trace.Repository, stream config.WorkstreamID, kind DecisionKind, key, outcome, subject string) (bound *Result, refusal string, err error) {
	res, err := Recorded(repo, stream, kind, key)
	if err != nil {
		return nil, "", err
	}
	switch res.Status {
	case None:
		return nil, fmt.Sprintf("call decide for %s first (kind %s); the decision is recorded only after Jev's decision on it exists", subject, kind), nil
	case Binding:
		if res.Refused != "" {
			return nil, "", nil
		}
		if outcome != res.Pick {
			return nil, fmt.Sprintf("Jev decided %s for %s with confidence %.2f (judgment %s); that decision is binding, so %q cannot be recorded. Record %s with your own text", res.Pick, subject, res.Confidence, res.Judgment, outcome, res.Pick), nil
		}
		return &res, "", nil
	}
	return nil, "", nil
}
