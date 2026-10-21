package api

// ReviewFinding is one thing the mason must change before the unit's
// acceptance holds.
type ReviewFinding struct {
	Severity string `json:"severity"`
	Evidence string `json:"evidence"`
	Action   string `json:"action"`
}
