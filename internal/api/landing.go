package api

// UnitLanding is the document units/<unit>/landing.json: the approval a
// unit landed on, with the reviewed candidate and base, the governing spec
// and plan revisions and seal, the criteria the unit serves, and the
// feature branch commit that landed it with its message.
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
