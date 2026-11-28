package api

import (
	"github.com/kpenfound/osmia/internal/config"
)

// ContestedRuling records the direction for a contested unit, given by the
// owner or by the manager on the owner's behalf.
type ContestedRuling struct {
	Decision string `json:"decision"`
	Note     string `json:"note"`
	// By names who ruled: "owner", "manager" or "jev". Empty is the
	// owner. Judgment names the Jev judgment record a Jev ruling comes from.
	By        string `json:"by,omitempty"`
	Judgment  string `json:"judgment,omitempty"`
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
