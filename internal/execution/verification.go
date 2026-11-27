package execution

import (
	"encoding/json"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/verifytool"
)

// PlaceReviewerVerification puts into view the retained files of the verify
// runs the unit's reviewer made on the candidate its latest review returned.
// It runs no lever. A unit without a review result, or whose reviewer ran
// nothing, places nothing.
func PlaceReviewerVerification(repo *trace.Repository, stream config.WorkstreamID, unit string, view isolation.View) error {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return err
	}
	var latest trace.Document
	for _, d := range docs {
		if d.ID == ReviewDocument(unit) && d.Revision >= latest.Revision {
			latest = d
		}
	}
	var result UnitReviewResult
	if latest.Revision == 0 || json.Unmarshal([]byte(latest.Content), &result) != nil || result.Identity.Candidate.Revision == "" {
		return nil
	}
	runs, err := verifytool.ReviewerRuns(repo, stream, unit, result.Identity.Candidate.Revision)
	if err != nil {
		return err
	}
	for _, op := range runs {
		if _, err := verifytool.Place(repo, op.Workstream, op.RunID, view, false); err != nil {
			return err
		}
	}
	return nil
}
