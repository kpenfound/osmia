package api

// CheckResult records the exit status and complete combined stdout/stderr
// emitted by a candidate check. Truncated indicates an incomplete capture.
// CIIdentityFallback explains why a requested Git identity could not be used.
type CheckResult struct {
	ExitCode           int    `json:"exit_code"`
	Output             string `json:"output"`
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
