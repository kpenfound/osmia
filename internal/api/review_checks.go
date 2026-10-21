package api

// CheckResult records the exit status and complete combined stdout/stderr
// emitted by a candidate check. Truncated indicates an incomplete capture.
type CheckResult struct {
	ExitCode  int    `json:"exit_code"`
	Output    string `json:"output"`
	Truncated bool   `json:"truncated"`
}
