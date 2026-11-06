package api

// CheckSelection is which checks a run ran and why. A selected run ran Links,
// chosen by Jev judgment Judgment from Candidates check links. A full run ran
// every check, because of Reason, with Detail. Listed is the number of links
// the project listed, before narrowing, or nil when listing did not complete.
type CheckSelection struct {
	// FallbackPicks is the configured count when no check reached the threshold.
	FallbackPicks int      `json:"fallback_picks,omitempty"`
	Mode          string   `json:"mode"`
	Links         []string `json:"links,omitempty"`
	Candidates    int      `json:"candidates,omitempty"`
	Listed        *int     `json:"listed,omitempty"`
	Judgment      string   `json:"judgment,omitempty"`
	Reason        string   `json:"reason,omitempty"`
	Detail        string   `json:"detail,omitempty"`
}

// The engines a check run can use, and the reasons a run used the cloud one.
const (
	CheckEngineLocal = "local"
	CheckEngineCloud = "cloud"

	// CloudAfterTimeout: the local attempt reached its time budget.
	CloudAfterTimeout = "timeout"
	// CloudAfterEngineFailure: the local attempt lost its engine session, or
	// Dagger exited non-zero without reporting a failed check.
	CloudAfterEngineFailure = "engine_failure"
	// CloudByMemory: an earlier run of the workstream needed cloud for these
	// links, so no local attempt was made.
	CloudByMemory = "memory"
)

// CheckEngineRun is which engine ran one invocation of a check operation: the
// run of Links, or of every check when EveryCheck is set. Reason says why a
// run used the cloud engine and is empty for local runs. PassedLocally counts
// the checks of an escalated invocation that the local attempt had already
// passed and the cloud attempt left out.
type CheckEngineRun struct {
	Engine        string   `json:"engine"`
	Links         []string `json:"links,omitempty"`
	EveryCheck    bool     `json:"every_check,omitempty"`
	Reason        string   `json:"reason,omitempty"`
	PassedLocally int      `json:"passed_locally,omitempty"`
}

// CheckAttribution records which engine ran each invocation of a check
// operation. The zero value, and an absent attribution, mean everything ran
// locally.
type CheckAttribution struct {
	Runs []CheckEngineRun `json:"runs,omitempty"`
}

// CloudRuns returns the runs that used the cloud engine. It is safe on nil.
func (a *CheckAttribution) CloudRuns() []CheckEngineRun {
	if a == nil {
		return nil
	}
	var runs []CheckEngineRun
	for _, run := range a.Runs {
		if run.Engine == CheckEngineCloud {
			runs = append(runs, run)
		}
	}
	return runs
}

// UsedCloud reports whether any invocation ran on the cloud engine.
func (a *CheckAttribution) UsedCloud() bool { return len(a.CloudRuns()) > 0 }
