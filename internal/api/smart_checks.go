package api

// CheckSelection is which checks a run ran and why. A selected run ran Links,
// chosen by Jev judgment Judgment from Candidates check links. A full run ran
// every check, because of Reason, with Detail. Listed is the number of links
// the project listed, before narrowing, or nil when listing did not complete.
// Covered is how many of the listed checks the run covered, or nil when the
// listing did not complete or no count was recorded.
type CheckSelection struct {
	// FallbackPicks is the configured count when no check reached the threshold.
	FallbackPicks   int               `json:"fallback_picks,omitempty"`
	Mode            string            `json:"mode"`
	Links           []string          `json:"links,omitempty"`
	Candidates      int               `json:"candidates,omitempty"`
	Listed          *int              `json:"listed,omitempty"`
	Covered         *int              `json:"covered,omitempty"`
	Judgment        string            `json:"judgment,omitempty"`
	BreadthJudgment string            `json:"breadth_judgment,omitempty"`
	DepthJudgment   string            `json:"depth_judgment,omitempty"`
	Roots           *int              `json:"roots,omitempty"`
	KeptRoots       []string          `json:"kept_roots,omitempty"`
	Levels          map[string]string `json:"levels,omitempty"`
	Reason          string            `json:"reason,omitempty"`
	Detail          string            `json:"detail,omitempty"`
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

// CheckEngineMixed names a check operation that ran some invocations locally
// and others on the cloud engine.
const CheckEngineMixed = "local and cloud"

// CheckEvidence is what a check run's record holds about how it ran, so a
// flake can be written up from the record alone. It is the neutral value both
// kinds of run record convert to: a unit's local run and a final review's
// Dagger Cloud statuses.
//
// Engine is "local", "cloud" or "local and cloud" (see CheckEngineMixed);
// Engines says which engine ran each invocation of a unit run. CoRun lists the
// checks the run ran or reported, and EveryCheck says the run was not narrowed
// to selected links. Parallelism is the number of checks the invocation ran at
// once, zero when the engine did not report it. OutputPath and OutputRevision
// name the captured output. Diagnostics are the failure reports of the checks
// that passed on their rerun, Traces say where else to read a run, and Timings
// are the durations the engine reported. Flaky is the run's checks that failed
// and then passed on their rerun, set when a record is converted. Every item
// the engine did not expose, and where to read it instead, is in Unavailable.
type CheckEvidence struct {
	Engine         string             `json:"engine"`
	Engines        *CheckAttribution  `json:"engines,omitempty"`
	Flaky          []string           `json:"flaky,omitempty"`
	CoRun          []string           `json:"co_run,omitempty"`
	EveryCheck     bool               `json:"every_check,omitempty"`
	Parallelism    int                `json:"parallelism,omitempty"`
	OutputPath     string             `json:"output_path,omitempty"`
	OutputRevision int                `json:"output_revision,omitempty"`
	Diagnostics    []CheckDiagnostic  `json:"diagnostics,omitempty"`
	Traces         []CheckTraceRef    `json:"traces,omitempty"`
	Timings        []CheckTiming      `json:"timings,omitempty"`
	Unavailable    []CheckUnavailable `json:"unavailable,omitempty"`
}

// The kinds of trace reference in a CheckEvidence.
const (
	CheckTraceRun    = "run"
	CheckTraceFailed = "failed"
	CheckTraceRerun  = "rerun"
)

// The items a CheckEvidence marks unavailable.
const (
	CheckItemOutput      = "output"
	CheckItemParallelism = "parallelism"
	CheckItemDiagnostics = "diagnostics"
)

// CheckDiagnostic is the failure report of a check that passed on its rerun.
type CheckDiagnostic struct {
	Check string `json:"check"`
	Text  string `json:"text"`
}

// CheckTraceRef says where to read a check or a run: a `dagger trace` ID, a
// URL, or both. Kind is one of the CheckTrace constants (CheckTraceRun, CheckTraceFailed and CheckTraceRerun); Check is empty for a
// reference to the whole run.
type CheckTraceRef struct {
	Kind    string `json:"kind"`
	Check   string `json:"check,omitempty"`
	TraceID string `json:"trace_id,omitempty"`
	URL     string `json:"url,omitempty"`
}

// CheckTiming is the duration an engine reported for a check. State is passed,
// failed or cancelled.
type CheckTiming struct {
	Check    string `json:"check"`
	State    string `json:"state,omitempty"`
	Duration string `json:"duration"`
}

// CheckUnavailable marks an item of evidence the engine did not expose, with
// where to read it instead.
type CheckUnavailable struct {
	Item   string `json:"item"`
	Engine string `json:"engine"`
	Where  string `json:"where,omitempty"`
}
