package api

import (
	"strings"

	"github.com/kpenfound/osmia/internal/seal"
)

// FinalReport is the document final/report.json: one final review of an
// assembled workstream. A reviewed report names the exact branch commit the
// committee member read, the governing seal, spec hash, spec, plan and
// charter revisions, the coverage of every criterion of the sealed spec by
// merged units, and the reader's findings on the branch as a whole. A failed
// report says why no such report exists; it never authorises approval or
// delivery.
type FinalReport struct {
	Maintenance  bool             `json:"maintenance,omitempty"`
	BaseCheck    int              `json:"base_check,omitempty"`
	EdgeResolved string           `json:"edge_resolved,omitempty"`
	Move         int              `json:"move,omitempty"`
	Review       int              `json:"review"`
	Operation    string           `json:"operation"`
	Outcome      string           `json:"outcome"`
	Failure      string           `json:"failure,omitempty"`
	Branch       string           `json:"branch"`
	Before       string           `json:"before"`
	Commit       string           `json:"commit,omitempty"`
	Upstream     *seal.Base       `json:"upstream,omitempty"`
	Conflicts    []string         `json:"conflicts,omitempty"`
	Seal         int              `json:"seal"`
	SpecHash     string           `json:"spec_hash"`
	Spec         int              `json:"spec"`
	Plan         int              `json:"plan"`
	Charter      int              `json:"charter"`
	Reader       string           `json:"reader,omitempty"`
	Turn         string           `json:"turn,omitempty"`
	Summary      string           `json:"summary,omitempty"`
	Criteria     []FinalCriterion `json:"criteria"`
	Findings     []FinalFinding   `json:"findings,omitempty"`
}

// FinalCriterion is one sealed criterion's coverage: Evidence names the
// merged units that serve it, or Gap says that none does.
type FinalCriterion struct {
	Criterion string `json:"criterion"`
	Text      string `json:"text"`
	Evidence  string `json:"evidence,omitempty"`
	Gap       string `json:"gap,omitempty"`
}

// FinalFinding is one problem the final reader found with the branch as a
// whole, the criteria it concerns, and the action it asks for.
type FinalFinding struct {
	Severity string   `json:"severity"`
	Criteria []string `json:"criteria,omitempty"`
	Finding  string   `json:"finding"`
	Action   string   `json:"action,omitempty"`
}

// Describe is the finding on one line with the criteria it concerns.
func (f FinalFinding) Describe() string {
	out := f.Finding
	if len(f.Criteria) > 0 {
		out = strings.Join(f.Criteria, ", ") + ": " + out
	}
	if f.Action != "" {
		out += " Action: " + f.Action
	}
	return out
}
