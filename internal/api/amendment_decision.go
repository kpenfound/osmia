package api

import (
	"encoding/json"

	"github.com/kpenfound/osmia/internal/config"
)

// AmendmentDecision is one attributed decision on a presented amendment, recorded
// as one revision of amendments/<n>/decision.json. Round is the debate round
// the decided packet followed, Packet the revision of packet.json considered, Spec and Plan the revisions of the proposed amendments/<n>/spec.md
// and plan.json, and SealRevision the revision of seal.json in force.
// Overruled names the objections an overrule set aside.
type AmendmentDecision struct {
	Decision     string   `json:"decision"`
	Note         string   `json:"note,omitempty"`
	Round        int      `json:"round"`
	Packet       int      `json:"packet"`
	Spec         int      `json:"spec"`
	Plan         int      `json:"plan"`
	SealRevision int      `json:"seal_revision"`
	Overruled    []string `json:"overruled,omitempty"`
	Actor        string   `json:"actor,omitempty"`
}

// AmendmentDecisionRequest is the owner's decision on the named revision of
// an amendment's packet.
type AmendmentDecisionRequest struct {
	Decision string `json:"decision"`
	Note     string `json:"note,omitempty"`
	Packet   int    `json:"packet"`
}

// AmendmentResponse describes one amendment: its state, the debate round it
// reached, its latest packet and revision, and its latest decision.
type AmendmentResponse struct {
	Project    config.ProjectID    `json:"project"`
	Workstream config.WorkstreamID `json:"workstream"`
	Amendment  string              `json:"amendment"`
	State      string              `json:"state"`
	Round      int                 `json:"round"`
	Packet     json.RawMessage     `json:"packet,omitempty"`
	Revision   int                 `json:"revision,omitempty"`
	Decision   *AmendmentDecision  `json:"decision,omitempty"`
	Detail     string              `json:"detail,omitempty"`
}
