package api

// UnitRebase is the document units/<unit>/rebase.json: one rebase of a
// unit's workspace. Snapshot is the commit that held the workspace, Base the
// feature branch commit it descended from, Onto the feature branch tip it was
// rebased onto and Commit the rebased commit the workspace's branch is on.
// Conflicts lists the paths the rebase left conflicted, and Report the
// revision of units/<unit>/report.json that is current after the rebase.
type UnitRebase struct {
	Unit      string   `json:"unit"`
	Rebase    int      `json:"rebase"`
	Operation string   `json:"operation"`
	State     string   `json:"state"`
	Branch    string   `json:"branch"`
	Base      string   `json:"base"`
	Onto      string   `json:"onto"`
	Snapshot  string   `json:"snapshot"`
	Commit    string   `json:"commit"`
	Conflicts []string `json:"conflicts"`
	Report    int      `json:"report"`
}
