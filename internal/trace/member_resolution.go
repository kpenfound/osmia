package trace

import (
	"encoding/json"

	"github.com/kpenfound/osmia/internal/config"
)

// MemberDocumentsActive reports whether a member still needs its own spec,
// slice and plan in active role context. Historical documents are retained.
func MemberDocumentsActive(r *Repository, stream config.WorkstreamID) (bool, error) {
	state, err := r.Workflow(stream, FeatureSubject)
	if err != nil {
		return false, err
	}
	docs, err := Read[Document](r, stream)
	if err != nil {
		return false, err
	}
	evidence, err := MemberResolutionFrom(docs)
	return evidence.Resolution(state.Value) == "unresolved", err
}

// MemberResolutionEvidence contains the facts that retire a member's documents.
// An opened publication counts only when its own current pull request merged.
type MemberResolutionEvidence struct {
	OwnerMerge         bool
	Publication        int
	OutcomePullRequest int
	Outcome            string
}

func (e MemberResolutionEvidence) Merged() bool {
	return e.OwnerMerge || e.Publication > 0 && e.Publication == e.OutcomePullRequest && e.Outcome == "merged"
}

func (e MemberResolutionEvidence) Resolution(state string) string {
	if state == "abandoned" {
		return "cancelled"
	}
	if state == "delivered" && e.Merged() {
		return "merged"
	}
	return "unresolved"
}

// MemberResolutionFrom reads the resolution fields from a document snapshot.
// Other publication fields remain owned by the service's delivery machinery.
func MemberResolutionFrom(docs []Document) (MemberResolutionEvidence, error) {
	var e MemberResolutionEvidence
	latest := map[string]Document{}
	for _, d := range docs {
		if d.Revision > latest[d.ID].Revision {
			latest[d.ID] = d
		}
	}
	for _, d := range latest {
		switch d.ID {
		case "final-merge":
			var merge map[string]json.RawMessage
			if err := json.Unmarshal([]byte(d.Content), &merge); err != nil {
				return e, err
			}
			e.OwnerMerge = true
		case "final-publication":
			var p struct {
				Status      string `json:"status"`
				PullRequest int    `json:"pull_request"`
			}
			if err := json.Unmarshal([]byte(d.Content), &p); err != nil {
				return e, err
			}
			if p.Status == "opened" && p.PullRequest > 0 {
				e.Publication = p.PullRequest
			}
		case "own-pull-request-outcome":
			var o struct {
				PullRequest int    `json:"pull_request"`
				Outcome     string `json:"outcome"`
			}
			if err := json.Unmarshal([]byte(d.Content), &o); err != nil {
				return e, err
			}
			e.OutcomePullRequest, e.Outcome = o.PullRequest, o.Outcome
		}
	}
	return e, nil
}
