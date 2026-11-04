package api

// CheckResult records the exit status and complete combined stdout/stderr
// emitted by a candidate check. Truncated indicates an incomplete capture.
// TraceURL is the Dagger Cloud trace link printed by the CLI, when present.
// CIIdentityFallback explains why a requested Git identity could not be used.
// Refusal is Dagger's error when the run that decided the result was refused
// before any check started. Flaky lists the links of checks that failed and then
// passed on their rerun.
type CheckResult struct {
	// Command records each command actually started, separated by &&.
	// Callers persist it in the run record alongside the result.
	Command            []string `json:"-"`
	ExitCode           int      `json:"exit_code"`
	Output             string   `json:"output"`
	TraceURL           string   `json:"trace_url,omitempty"`
	CIIdentityFallback string   `json:"ci_identity_fallback,omitempty"`
	Truncated          bool     `json:"truncated"`
	Flaky              []string `json:"flaky,omitempty"`
	Refusal            string   `json:"-"`
}

// CheckIdentity identifies the pinned commit and its service-owned source clone.
// Remote is the configured upstream's credential-free HTTPS URL.
type CheckIdentity struct {
	Candidate  string
	Branch     string
	Remote     string
	Repository string
}
