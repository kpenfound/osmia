package api

import (
	"github.com/kpenfound/osmia/internal/config"
)

// WorkstreamMoveRequest moves a workstream to another feature state with a
// note for whoever takes it from there.
type WorkstreamMoveRequest struct {
	To   string `json:"to"`
	Note string `json:"note"`
}

// WorkstreamMove is the document moves/move-<n>.json: one move of a
// workstream to building, assembled or back to delivered, by the owner or by
// the chief of staff. From is the feature state it left; a move into the
// state the workstream is in restarts that stage.
type WorkstreamMove struct {
	Move int    `json:"move"`
	From string `json:"from"`
	To   string `json:"to"`
	Note string `json:"note"`
	By   string `json:"by"`
	Turn string `json:"turn,omitempty"`
}

// WorkstreamMoveResponse reports a workstream move: the feature state the
// workstream is in after it, what the move did, and the recorded move when
// the move is not one of the owner's own shed, ratification or abandonment
// actions, which record themselves.
type WorkstreamMoveResponse struct {
	Workstream config.WorkstreamID `json:"workstream"`
	State      string              `json:"state"`
	Detail     string              `json:"detail"`
	Move       *WorkstreamMove     `json:"move,omitempty"`
}
