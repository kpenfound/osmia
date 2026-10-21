package api

// CheckSelection is which checks a run ran and why. A selected run ran Links,
// chosen by Jev judgment Judgment from Candidates check links. A full run ran
// every check, because of Reason, with Detail. Listed is the number of links
// the project listed, before narrowing, or nil when listing did not complete.
type CheckSelection struct {
	Mode       string   `json:"mode"`
	Links      []string `json:"links,omitempty"`
	Candidates int      `json:"candidates,omitempty"`
	Listed     *int     `json:"listed,omitempty"`
	Judgment   string   `json:"judgment,omitempty"`
	Reason     string   `json:"reason,omitempty"`
	Detail     string   `json:"detail,omitempty"`
}
