package api

import (
	"github.com/kpenfound/osmia/internal/config"
)

// DeliveryPublication is the document final/publication.json: the delivery
// of one owner approval. Reviewed is the reviewed branch commit and Commit
// the commit pushed to Branch of Fork through the clone's Remote; with the
// squash style it is one commit holding Reviewed's tree. PullRequest and URL
// name the pull request opened against Base of Upstream with Title and the
// approved Description once Status is opened. Fork is the resolved push
// repository, including upstream when the project has no fork. BaseWorkstream
// identifies a dependent request; it is empty for the canonical base branch.
type DeliveryPublication struct {
	BaseWorkstream config.WorkstreamID `json:"base_workstream,omitempty"`
	PriorURL       string              `json:"prior_url,omitempty"`
	Approval       int                 `json:"approval"`
	Operation      string              `json:"operation"`
	Status         string              `json:"status"`
	// Draft records whether the recorded pull request opened, or stays,
	// a draft: see publishInput.Draft.
	Draft           bool   `json:"draft,omitempty"`
	Style           string `json:"style"`
	Fork            string `json:"fork"`
	Remote          string `json:"remote"`
	Branch          string `json:"branch"`
	Reviewed        string `json:"reviewed"`
	Commit          string `json:"commit"`
	Upstream        string `json:"upstream"`
	Base            string `json:"base"`
	PullRequest     int    `json:"pull_request,omitempty"`
	URL             string `json:"url,omitempty"`
	Title           string `json:"title"`
	Description     string `json:"description"`
	DescriptionHash string `json:"description_hash"`
}
