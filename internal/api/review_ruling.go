package api

import (
	"github.com/kpenfound/osmia/internal/config"
)

// ContestedRuling records the direction for a contested unit, given by the
// owner or by the chief of staff on the owner's behalf.
type ContestedRuling struct {
	Decision string `json:"decision"`
	Note     string `json:"note"`
	// By names who ruled: "owner", or "chief of staff". Empty is the owner.
	By        string `json:"by,omitempty"`
	Bounces   int    `json:"bounces"`
	Candidate string `json:"candidate"`
	Contest   string `json:"contest,omitempty"`
	ResetTurn uint64 `json:"reset_turn,omitempty"`
}

type ContestedRulingRequest struct {
	Decision string `json:"decision"`
	Note     string `json:"note"`
}

type ContestedRulingResponse struct {
	Workstream config.WorkstreamID `json:"workstream"`
	Unit       string              `json:"unit"`
	Ruling     ContestedRuling     `json:"ruling"`
}
