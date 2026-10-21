package api

import (
	"github.com/kpenfound/osmia/internal/config"
)

// TraceSummary identifies the sealed revisions, criteria and units of a
// workstream. Unit walks retain their evidence and explicit missing links.
type TraceSummary struct {
	Project    config.ProjectID    `json:"project"`
	Workstream config.WorkstreamID `json:"workstream"`
	Feature    string              `json:"feature"`
	Seal       *TraceSeal          `json:"seal,omitempty"`
	Spec       *TraceRef           `json:"spec,omitempty"`
	Plan       *TraceRef           `json:"plan,omitempty"`
	Criteria   []TraceCriterion    `json:"criteria"`
	Units      []UnitTrace         `json:"units"`
	Delivery   *TraceDelivery      `json:"delivery,omitempty"`
	Gaps       []TraceGap          `json:"gaps"`
	Complete   bool                `json:"complete"`
}
