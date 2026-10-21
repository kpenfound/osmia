package api

// ChiefContestDecision records the chief of staff's decision on one contest:
// a ruling it gave, or its escalation to the owner.
type ChiefContestDecision struct {
	Contest  string `json:"contest"`
	Decision string `json:"decision"`
	Note     string `json:"note"`
	// Turn is the chief-of-staff turn that decided.
	Turn string `json:"turn"`
}
