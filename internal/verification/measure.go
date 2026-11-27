package verification

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// The sources of a measured run: a service rerun of an invocation the mason
// declared, and a verify call the unit's reviewer made.
const (
	SourceRerun    = "rerun"
	SourceReviewer = "reviewer"
)

// What became of a measured run.
const (
	// OutcomeResult is a run that produced a validated or rejected report.
	OutcomeResult = "result"
	// OutcomeLimited is a run that produced no report: it was refused, failed
	// or timed out. It is never a pass.
	OutcomeLimited = "limited"
	// OutcomeSkipped is a declared invocation past the rerun bound.
	OutcomeSkipped = "skipped"
	// OutcomeRefused is a declared invocation the argument rules refused.
	OutcomeRefused = "refused"
)

// MeasuredRun is what a measure reads of one recorded run. Result is only
// meaningful when Outcome is OutcomeResult.
type MeasuredRun struct {
	Source  string
	Outcome string
	Result  Result
}

// SourceMeasure is what one source of runs did for a unit. Runs counts the runs
// that produced a report by their overall status, so a rejected report is
// inconclusive and inconclusive is never part of Pass. Flows holds the flows
// the runs' claims were tagged with, taken only from claim flow tags. A run
// that produced a report with no flow tag at all is an untagged run, and its
// untagged claims are counted apart. A rejected report covers no flow.
type SourceMeasure struct {
	Runs           Counts   `json:"runs"`
	Limited        int      `json:"limited,omitempty"`
	Skipped        int      `json:"skipped,omitempty"`
	Refused        int      `json:"refused,omitempty"`
	Rejected       int      `json:"rejected,omitempty"`
	UntaggedRuns   int      `json:"untagged_runs,omitempty"`
	UntaggedClaims int      `json:"untagged_claims,omitempty"`
	Flows          []string `json:"flows"`
}

// UnitMeasure is the record of what verification did for one unit: the
// service's reruns of the invocations its mason declared, the verify runs of
// its reviewer, and whether its diff touched the lever's source or the map
// directory. Runs counts both sources together by status. Review and
// Candidate say which review and candidate the record was made at.
type UnitMeasure struct {
	Unit         string        `json:"unit"`
	Review       int           `json:"review"`
	Candidate    string        `json:"candidate"`
	Runs         Counts        `json:"runs"`
	Limited      int           `json:"limited,omitempty"`
	Declared     SourceMeasure `json:"declared"`
	Reviewer     SourceMeasure `json:"reviewer"`
	LeverTouched bool          `json:"lever_touched"`
	MapTouched   bool          `json:"map_touched"`
}

// MeasureUnit tallies the runs recorded for unit: the service reruns of its
// declared invocations as Declared and its reviewer's verify runs as Reviewer.
// A run of any other source is not counted. It never maps a flow from
// arguments: flows come only from claim tags.
func MeasureUnit(unit string, runs []MeasuredRun) UnitMeasure {
	m := UnitMeasure{Unit: unit}
	flows := map[string]map[string]bool{SourceRerun: {}, SourceReviewer: {}}
	for _, run := range runs {
		var part *SourceMeasure
		switch run.Source {
		case SourceRerun:
			part = &m.Declared
		case SourceReviewer:
			part = &m.Reviewer
		default:
			continue
		}
		switch run.Outcome {
		case OutcomeSkipped:
			part.Skipped++
			continue
		case OutcomeRefused:
			part.Refused++
			continue
		case OutcomeLimited:
			part.Limited++
			m.Limited++
			continue
		}
		status := run.Result.Status()
		part.Runs.Add(status)
		m.Runs.Add(status)
		if run.Result.Rejected != "" {
			part.Rejected++
			continue
		}
		cov := Attribute(run.Result, nil)
		if len(cov.Flows) == 0 {
			part.UntaggedRuns++
		}
		part.UntaggedClaims += len(cov.Untagged)
		for _, f := range cov.Flows {
			flows[run.Source][f.Flow] = true
		}
	}
	m.Declared.Flows = sortedKeys(flows[SourceRerun])
	m.Reviewer.Flows = sortedKeys(flows[SourceReviewer])
	return m
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Association is the flows an approved unit's feature association names.
type Association struct {
	Unit  string   `json:"unit"`
	Flows []string `json:"flows"`
}

// FlowAttribution is one flow's status at a final review and the approved
// units it is attributed to. Units lists every approved unit whose association
// names the flow, and Shared is set when there is more than one, so the flow
// still counts once. Unattributed is set when no unit's association names it.
type FlowAttribution struct {
	Flow         string   `json:"flow"`
	Status       Status   `json:"status"`
	Units        []string `json:"units,omitempty"`
	Shared       bool     `json:"shared,omitempty"`
	Unattributed bool     `json:"unattributed,omitempty"`
}

// FinalMeasure is the record of what verification found at one final review
// round: each flow's status on the reviewed head across the round's verify
// runs, the failed and the inconclusive flows with their attribution, and the
// failing untagged claims, which no unit is attributed.
//
// Failed counts a failed flow once, however many units share it. Inconclusive
// is a separate measure that never counts as a failure. UnattributedClaims
// names the failing claims that carry no flow tag. UnitsFailing lists the
// units that passed unit review and have at least one failed flow attributed
// to them at this round, and UnitsInconclusive the units attributed an
// inconclusive flow.
type FinalMeasure struct {
	Review             int               `json:"review"`
	Commit             string            `json:"commit"`
	Runs               Counts            `json:"runs"`
	Limited            int               `json:"limited,omitempty"`
	Flows              []FlowStatus      `json:"flows"`
	Failed             []FlowAttribution `json:"failed"`
	Inconclusive       []FlowAttribution `json:"inconclusive"`
	UnattributedClaims []string          `json:"unattributed_claims"`
	UnitsFailing       []string          `json:"units_failing"`
	UnitsInconclusive  []string          `json:"units_inconclusive"`
}

// Unattributed counts the unattributed final failures: the failed flows no
// unit's association names and the failing untagged claims.
func (m FinalMeasure) Unattributed() int {
	n := len(m.UnattributedClaims)
	for _, f := range m.Failed {
		if f.Unattributed {
			n++
		}
	}
	return n
}

// AttributeFinal computes the measure of one final review round from the
// results of the round's verify runs on the reviewed head, limited being the
// number of runs that produced no report, and the associations of the approved
// units. Each flow's status is the worst any run gave it, by Combine. A failed
// flow is attributed to every unit whose association names it, a flow no unit
// names is unattributed, and a failing untagged claim is unattributed. An
// inconclusive flow is attributed the same way into its own list, and a run or
// a flow that is inconclusive is never a pass and never a failure.
func AttributeFinal(review int, commit string, runs []Result, limited int, associations []Association) FinalMeasure {
	m := FinalMeasure{Review: review, Commit: commit, Limited: limited,
		Failed: []FlowAttribution{}, Inconclusive: []FlowAttribution{}, UnattributedClaims: []string{}, UnitsFailing: []string{}, UnitsInconclusive: []string{}}
	var coverage []Coverage
	for _, r := range runs {
		m.Runs.Add(r.Status())
		cov := Attribute(r, nil)
		coverage = append(coverage, cov)
		for _, c := range cov.Untagged {
			if c.Status == Fail {
				m.UnattributedClaims = append(m.UnattributedClaims, c.Name)
			}
		}
	}
	m.Flows = Combine(coverage...)
	failing, inconclusive := map[string]bool{}, map[string]bool{}
	for _, f := range m.Flows {
		var units []string
		for _, a := range associations {
			if slices.Contains(a.Flows, f.Flow) && !slices.Contains(units, a.Unit) {
				units = append(units, a.Unit)
			}
		}
		sort.Strings(units)
		record := FlowAttribution{Flow: f.Flow, Status: f.Status, Units: units, Shared: len(units) > 1, Unattributed: len(units) == 0}
		switch f.Status {
		case Fail:
			m.Failed = append(m.Failed, record)
			for _, u := range units {
				failing[u] = true
			}
		case Pass:
		default:
			m.Inconclusive = append(m.Inconclusive, record)
			for _, u := range units {
				inconclusive[u] = true
			}
		}
	}
	m.UnitsFailing = append(m.UnitsFailing[:0], sortedKeys(failing)...)
	m.UnitsInconclusive = append(m.UnitsInconclusive[:0], sortedKeys(inconclusive)...)
	return m
}

// UnitMeasurePath is the trace document path of a unit's measure.
func UnitMeasurePath(unit string) string { return "units/" + unit + "/verification-measure.json" }

// ParseUnitMeasurePath reports whether path is a unit measure's and names the
// unit.
func ParseUnitMeasurePath(path string) (unit string, ok bool) {
	rest, ok := strings.CutPrefix(path, "units/")
	if !ok {
		return "", false
	}
	unit, ok = strings.CutSuffix(rest, "/verification-measure.json")
	return unit, ok && unit != "" && !strings.Contains(unit, "/")
}

// FinalMeasurePath is the trace document path of final review k's measure.
func FinalMeasurePath(k int) string { return fmt.Sprintf("final/verification-%d.json", k) }

// IsFinalMeasurePath reports whether path is a final review measure's.
func IsFinalMeasurePath(path string) bool {
	rest, ok := strings.CutPrefix(path, "final/verification-")
	if !ok {
		return false
	}
	n, ok := strings.CutSuffix(rest, ".json")
	if !ok {
		return false
	}
	_, err := strconv.Atoi(n)
	return err == nil
}
