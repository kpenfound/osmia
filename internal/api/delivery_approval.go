package api

import (
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/workspace"
)

// DeliveryPresentation is the final review and the title and description
// presented to the owner. Criteria with a gap precede criteria with
// evidence. A delivered workstream presents the report, approval and
// publication it was delivered with, and no draft.
type DeliveryPresentation struct {
	Project        config.ProjectID            `json:"project"`
	Workstream     config.WorkstreamID         `json:"workstream"`
	Report         FinalReport                 `json:"report"`
	ReviewRevision int                         `json:"review_revision"`
	Title          string                      `json:"title,omitempty"`
	Draft          string                      `json:"draft"`
	DraftHash      string                      `json:"draft_hash"`
	Messages       []workspace.DeliveryMessage `json:"messages"`
	Style          string                      `json:"style"`
	Approval       *DeliveryApproval           `json:"approval,omitempty"`
	// Publication is the latest record of publishing an approval, Merge the
	// owner's own merge of the branch into upstream that delivered it
	// instead, and Delivered whether the workstream is delivered.
	Publication *DeliveryPublication `json:"publication,omitempty"`
	Merge       *DeliveryMerge       `json:"merge,omitempty"`
	Delivered   bool                 `json:"delivered,omitempty"`
	Maintenance bool                 `json:"maintenance,omitempty"`
}

// DeliveryApproval preserves the exact title and text and reviewed identity
// the owner approved. Title is empty in an approval recorded before the
// title became part of the draft; deliveryTitle(Description, stream) is
// then used instead, as it was when that approval was recorded.
// DescriptionHash allows publication to check the text it will send.
type DeliveryApproval struct {
	Review          int                         `json:"review"`
	ReviewRevision  int                         `json:"review_revision"`
	Commit          string                      `json:"commit"`
	Seal            int                         `json:"seal"`
	SpecHash        string                      `json:"spec_hash"`
	Spec            int                         `json:"spec"`
	Plan            int                         `json:"plan"`
	Charter         int                         `json:"charter"`
	DraftHash       string                      `json:"draft_hash"`
	Title           string                      `json:"title,omitempty"`
	Description     string                      `json:"description"`
	DescriptionHash string                      `json:"description_hash"`
	At              time.Time                   `json:"at"`
	Messages        []workspace.DeliveryMessage `json:"messages"`
	Style           string                      `json:"style"`
	// AcceptedBlocking is what the final report found blocking that the
	// owner accepted as it is by approving.
	AcceptedBlocking []string `json:"accepted_blocking,omitempty"`
}

// DeliveryDecision approves the draft as shown, or supplies the owner's edit.
// The review identity and draft hash are required even when the text is edited.
type DeliveryDecision struct {
	Review         int                         `json:"review"`
	ReviewRevision int                         `json:"review_revision"`
	Commit         string                      `json:"commit"`
	DraftHash      string                      `json:"draft_hash"`
	Description    *string                     `json:"description,omitempty"`
	Messages       []workspace.DeliveryMessage `json:"messages,omitempty"`
	// AcceptBlocking accepts what the final report found blocking as it is;
	// a report with blocking findings is approved only with it.
	AcceptBlocking bool `json:"accept_blocking,omitempty"`
}
