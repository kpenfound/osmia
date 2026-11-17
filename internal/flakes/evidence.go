package flakes

import "time"

// Engines a check run can have run on.
const (
	EngineLocal = "local"
	EngineCloud = "cloud"
)

// Items of the evidence an engine may not expose. Evidence lists the ones its
// engine did not report in Unavailable.
const (
	ItemOutput      = "output"
	ItemDiagnostics = "diagnostics"
	ItemCoRun       = "co_run"
	ItemParallelism = "parallelism"
)

// Evidence is what one unit or final-review check run recorded about its
// flaky checks. It is the neutral value the check-run records convert to and
// the hand-in is written from; it holds the whole run, not one flake group.
type Evidence struct {
	// Engine is the engine that ran the checks: EngineLocal or EngineCloud.
	Engine string `json:"engine,omitempty"`
	// Flaky lists every check of the run that failed and then passed on its
	// rerun: local links, or Dagger Cloud check names.
	Flaky []string `json:"flaky,omitempty"`
	// CoRun lists the other checks that ran in the same invocation.
	CoRun []string `json:"co_run,omitempty"`
	// Parallelism is how many checks the invocation ran at once; zero means
	// not reported.
	Parallelism int `json:"parallelism,omitempty"`
	// Output is the captured output, or an excerpt of it, and OutputPath
	// where the whole of it is recorded.
	Output     string `json:"output,omitempty"`
	OutputPath string `json:"output_path,omitempty"`
	// Diagnostics are the failure diagnostics of each flaky check, marked
	// [passed on rerun].
	Diagnostics []string `json:"diagnostics,omitempty"`
	// Traces are where the run can be read from, such as a Dagger Cloud trace.
	Traces []Trace `json:"traces,omitempty"`
	// Timings are per-check durations, where the runner recorded them.
	Timings []Timing `json:"timings,omitempty"`
	// Unavailable names the items the engine did not expose.
	Unavailable []string `json:"unavailable,omitempty"`
}

// Trace is a place a check's run can be read: a Dagger Cloud trace ID or URL.
type Trace struct {
	// Check is the check the trace shows, and Role what it shows: "failed" for
	// the run that failed, "rerun" for the rerun that passed.
	Check string `json:"check,omitempty"`
	Role  string `json:"role,omitempty"`
	Ref   string `json:"ref"`
}

// Timing is how long one check took.
type Timing struct {
	Check    string        `json:"check"`
	Duration time.Duration `json:"duration"`
}
