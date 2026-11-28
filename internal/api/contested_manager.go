package api

// ManagerContestDecision records the manager's decision on one contest:
// a ruling it gave, or its escalation to the owner.
type ManagerContestDecision struct {
	Contest  string `json:"contest"`
	Decision string `json:"decision"`
	Note     string `json:"note"`
	// Turn is the manager turn that decided.
	Turn string `json:"turn"`
	// Actor is empty for the manager's own decision, or "jev" with the
	// Jev judgment record it comes from.
	Actor    string `json:"actor,omitempty"`
	Judgment string `json:"judgment,omitempty"`
}
