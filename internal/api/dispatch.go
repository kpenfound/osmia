package api

import (
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/runtime"
)

// UnitDispatch is the document units/<unit>/dispatch.json: the mason
// controller's latest decision on a ready unit. Version is the version of the
// unit's workflow state the decision was taken on, so a decision on a unit
// that has since moved no longer applies. Reason is set on a deferral, with
// Blockers for an entanglement, Blocked naming the blocked unit, Pause the
// pause in force, Workstreams the higher-priority workstreams waiting to
// start a unit, and Limit the capacity in force. Message says the same in
// plain language.
type UnitDispatch struct {
	Unit        string                `json:"unit"`
	Version     uint64                `json:"version"`
	Decision    string                `json:"decision"`
	Reason      string                `json:"reason,omitempty"`
	Blockers    []plan.StartBlocker   `json:"blockers,omitempty"`
	Blocked     string                `json:"blocked,omitempty"`
	Pause       *runtime.Pause        `json:"pause,omitempty"`
	Workstreams []config.WorkstreamID `json:"workstreams,omitempty"`
	Limit       int                   `json:"limit,omitempty"`
	Message     string                `json:"message"`
}
