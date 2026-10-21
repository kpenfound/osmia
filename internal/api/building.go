package api

import (
	"github.com/kpenfound/osmia/internal/coreadapter"
)

// UnitStatus is the recorded state, latest card and landing of one unit of
// the sealed plan or a final-review follow-up. A ready unit the mason
// controller deferred carries the deferral, and its message as Reason.
type UnitStatus struct {
	Unit     string            `json:"unit"`
	State    string            `json:"state"`
	Reason   string            `json:"reason,omitempty"`
	Deferral *UnitDispatch     `json:"deferral,omitempty"`
	Card     *coreadapter.Card `json:"card,omitempty"`
	Landing  *UnitLanding      `json:"landing,omitempty"`
}
