// Package followup reads work that supplements a sealed plan: gaps a final
// review found, and criteria an approved amendment changed after the units
// addressing them merged.
package followup

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/trace"
)

const DocumentID = "final-followups"
const Path = "final/followups.json"

// Unit retains the final report, or the amendment, and the criterion that
// caused a follow-up. The sealed spec and plan remain separate, immutable
// inputs to its ordinary unit work.
type Unit struct {
	Review    int       `json:"review"`
	Report    int       `json:"report"`
	Amendment string    `json:"amendment,omitempty"`
	Criterion string    `json:"criterion"`
	Gap       string    `json:"gap"`
	Unit      plan.Unit `json:"unit"`
}

// Read returns every recorded follow-up in review order.
func Read(repo *trace.Repository, stream config.WorkstreamID) ([]Unit, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return nil, err
	}
	var units []Unit
	for _, doc := range docs {
		if doc.ID != DocumentID {
			continue
		}
		var batch []Unit
		if err := json.Unmarshal([]byte(doc.Content), &batch); err != nil {
			return nil, fmt.Errorf("%s revision %d: %w", doc.Path, doc.Revision, err)
		}
		for _, u := range batch {
			units = append(units, u.withTask())
		}
	}
	return units, nil
}

// Find returns the uniquely identified follow-up, if present.
func Find(repo *trace.Repository, stream config.WorkstreamID, id string) (Unit, bool, error) {
	units, err := Read(repo, stream)
	if err != nil {
		return Unit{}, false, err
	}
	i := slices.IndexFunc(units, func(u Unit) bool { return u.Unit.ID == id })
	if i < 0 {
		return Unit{}, false, nil
	}
	return units[i], true, nil
}

// withTask returns the follow-up with a task, acceptance and criterion drawn
// from its gap when its unit records none.
func (u Unit) withTask() Unit {
	if u.Unit.Task == "" {
		u.Unit.Task = u.Gap
	}
	if len(u.Unit.Acceptance) == 0 {
		u.Unit.Acceptance = []string{u.Criterion + " holds: " + u.Gap}
	}
	if len(u.Unit.Criteria) == 0 && u.Criterion != "" {
		u.Unit.Criteria = []string{u.Criterion}
	}
	return u
}
