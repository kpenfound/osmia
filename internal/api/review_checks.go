package api

// CheckResult records the exit status and complete combined stdout/stderr
// emitted by a candidate check. Truncated indicates an incomplete capture.
// TraceURL is the Dagger Cloud trace link printed by the CLI, when present.
// CIIdentityFallback explains why a requested Git identity could not be used.
type CheckResult struct {
	ExitCode           int    `json:"exit_code"`
	Output             string `json:"output"`
	TraceURL           string `json:"trace_url,omitempty"`
	CIIdentityFallback string `json:"ci_identity_fallback,omitempty"`
	Truncated          bool   `json:"truncated"`
}

// CheckIdentity identifies the pinned commit and its service-owned source clone.
// Remote is the configured upstream's credential-free HTTPS URL.
type CheckIdentity struct {
	Candidate  string
	Branch     string
	Remote     string
	Repository string
}
