package verification

import "sort"

// FlowStatus is the status one or more runs gave a flow.
type FlowStatus struct {
	Flow   string `json:"flow"`
	Status Status `json:"status"`
	// Unlisted is set when the lever's list was given and does not name the
	// flow.
	Unlisted bool `json:"unlisted,omitempty"`
}

// Coverage is what one run covered. Flows come only from claim flow tags, in
// name order. Untagged holds the claims with no flow tag: they count toward the
// run's status and cover no flow. A rejected run covers no flow.
type Coverage struct {
	Status   Status       `json:"status"`
	Flows    []FlowStatus `json:"flows,omitempty"`
	Untagged []Claim      `json:"untagged,omitempty"`
	Rejected string       `json:"rejected,omitempty"`
}

// Attribute computes the coverage of one run. A flow's status is fail if any
// claim tagged with it failed, else inconclusive if any is inconclusive, else
// pass. listed is the flow names the lever's list returned: a tag it does not
// name is marked unlisted. A nil listed means the list is unknown and marks
// nothing; an empty non-nil one means the lever lists no flows.
func Attribute(r Result, listed []string) Coverage {
	cov := Coverage{Status: r.Status(), Rejected: r.Rejected}
	if r.Rejected != "" {
		return cov
	}
	known := map[string]bool{}
	for _, name := range listed {
		known[name] = true
	}
	statuses := map[string]Status{}
	for _, c := range r.Claims {
		if c.Flow == "" {
			cov.Untagged = append(cov.Untagged, c)
			continue
		}
		if prior, ok := statuses[c.Flow]; ok {
			statuses[c.Flow] = Worse(prior, c.Status)
		} else {
			statuses[c.Flow] = Worse(Pass, c.Status)
		}
	}
	for flow, status := range statuses {
		cov.Flows = append(cov.Flows, FlowStatus{Flow: flow, Status: status, Unlisted: listed != nil && !known[flow]})
	}
	sort.Slice(cov.Flows, func(i, j int) bool { return cov.Flows[i].Flow < cov.Flows[j].Flow })
	return cov
}

// Combine merges the flow statuses of several runs of one commit: each flow
// takes the worst status any run gave it, fail then inconclusive then pass.
// A flow is unlisted when any run marked it so. A run that covers no flow adds
// nothing, and a flow no run covered is absent from the result.
func Combine(runs ...Coverage) []FlowStatus {
	merged := map[string]FlowStatus{}
	for _, run := range runs {
		for _, f := range run.Flows {
			if prior, ok := merged[f.Flow]; ok {
				f.Status = Worse(prior.Status, f.Status)
				f.Unlisted = f.Unlisted || prior.Unlisted
			}
			merged[f.Flow] = f
		}
	}
	out := make([]FlowStatus, 0, len(merged))
	for _, f := range merged {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Flow < out[j].Flow })
	return out
}

// Counts tallies statuses. Inconclusive is its own count and is never part of
// Pass.
type Counts struct {
	Pass         int `json:"pass"`
	Fail         int `json:"fail"`
	Inconclusive int `json:"inconclusive"`
}

// Add counts one status. A status that is not valid counts as inconclusive.
func (c *Counts) Add(s Status) {
	switch s {
	case Pass:
		c.Pass++
	case Fail:
		c.Fail++
	default:
		c.Inconclusive++
	}
}

// Total is the number of statuses counted.
func (c Counts) Total() int { return c.Pass + c.Fail + c.Inconclusive }

// CountRuns tallies the overall status of each run.
func CountRuns(runs ...Result) Counts {
	var c Counts
	for _, r := range runs {
		c.Add(r.Status())
	}
	return c
}

// CountClaims tallies the claims of a run. A rejected report has no claims.
func CountClaims(r Result) Counts {
	var c Counts
	for _, claim := range r.Claims {
		c.Add(claim.Status)
	}
	return c
}

// CountFlows tallies flow statuses.
func CountFlows(flows []FlowStatus) Counts {
	var c Counts
	for _, f := range flows {
		c.Add(f.Status)
	}
	return c
}
