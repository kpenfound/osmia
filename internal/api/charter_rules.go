package api

import (
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

// CharterDecisionRequest is the owner's decision on a charter proposal:
// ratify or decline, with an optional note.
type CharterDecisionRequest struct {
	Decision string `json:"decision"`
	Note     string `json:"note,omitempty"`
}

// CharterProposalView is one charter proposal. Number is the number the rule
// would take when it was proposed, and the number it took once State is
// chartered; Charter is then the charter.md revision that records it. Ruling
// and RulingRevision name the owner's ruling it comes from.
type CharterProposalView struct {
	Workstream     config.WorkstreamID `json:"workstream"`
	Question       string              `json:"question"`
	State          string              `json:"state"`
	Rule           string              `json:"rule"`
	Number         int                 `json:"number"`
	Ruling         string              `json:"ruling"`
	RulingRevision int                 `json:"ruling_revision"`
	OwnerResponse  string              `json:"owner_response"`
	ProposedAt     time.Time           `json:"proposed_at"`
	Decision       string              `json:"decision,omitempty"`
	Note           string              `json:"note,omitempty"`
	Charter        int                 `json:"charter,omitempty"`
	Detail         string              `json:"detail,omitempty"`
}

// CharterProposalsResponse lists the charter proposals waiting for the
// owner's decision, oldest first.
type CharterProposalsResponse struct {
	Proposals []CharterProposalView `json:"proposals"`
}
