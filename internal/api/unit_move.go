package api

import (
	"github.com/kpenfound/osmia/internal/config"
)

// UnitMoveRequest moves a unit to another state with a note for whoever
// takes it from there.
type UnitMoveRequest struct {
	To   string `json:"to"`
	Note string `json:"note"`
}

// UnitMove is the document units/<unit>/move-<version>.json: one move of a
// unit, by the owner or by the chief of staff on the owner's behalf, out of
// the state at version. ResetTurn is the mason turn a move to implementing
// resumes after, and Candidate the recorded candidate a move to checking,
// reviewing or approved takes forward.
type UnitMove struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Note      string `json:"note"`
	By        string `json:"by"`
	Turn      string `json:"turn,omitempty"`
	ResetTurn uint64 `json:"reset_turn,omitempty"`
	Candidate string `json:"candidate,omitempty"`
}

type UnitMoveResponse struct {
	Workstream config.WorkstreamID `json:"workstream"`
	Unit       string              `json:"unit"`
	Move       UnitMove            `json:"move"`
}
