package api

// UnitLanding is a unit's landing.json: the candidate that landed on the
// feature branch, the commit it landed as and the approval, turn and sealed
// identity it landed under.
type UnitLanding struct {
	Unit      string   `json:"unit"`
	Operation string   `json:"operation"`
	Approval  string   `json:"approval"`
	Turn      string   `json:"turn"`
	Candidate string   `json:"candidate"`
	Base      string   `json:"base"`
	Spec      string   `json:"spec"`
	Plan      string   `json:"plan"`
	Seal      int      `json:"seal"`
	Criteria  []string `json:"criteria"`
	Branch    string   `json:"branch"`
	Commit    string   `json:"commit"`
	Message   string   `json:"message"`
}
