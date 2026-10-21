package api

// UnitActivity is what the chief of staff reads of a started unit to see why
// it is where it is.
type UnitActivity struct {
	Unit        string               `json:"unit"`
	State       string               `json:"state"`
	Contest     *ActivityContest     `json:"contest,omitempty"`
	Transitions []ActivityTransition `json:"transitions"`
	Turns       []ActivityTurn       `json:"turns"`
}

// ActivityContest is the contest a unit is in, the rulings it takes and how
// many more the chief of staff may give the unit (-1 means no count gate).
type ActivityContest struct {
	ID               string                `json:"id"`
	Reason           string                `json:"reason"`
	Rulings          []string              `json:"rulings"`
	ChiefRulingsLeft int                   `json:"chief_rulings_left"`
	Decided          *ChiefContestDecision `json:"decided,omitempty"`
}

type ActivityTransition struct {
	At      string `json:"at"`
	Subject string `json:"subject"`
	From    string `json:"from"`
	To      string `json:"to"`
	Actor   string `json:"actor"`
	Reason  string `json:"reason"`
}

// ActivityTurn is one of a role's latest turns on the unit.
type ActivityTurn struct {
	Agent         string            `json:"agent"`
	Turn          string            `json:"turn"`
	Status        string            `json:"status"`
	Outcome       string            `json:"outcome,omitempty"`
	Report        string            `json:"report,omitempty"`
	FinalResponse string            `json:"final_response,omitempty"`
	Failure       string            `json:"failure,omitempty"`
	Refused       []ActivityRefusal `json:"refused,omitempty"`
}

// ActivityRefusal is a tool call of a turn that the service refused or that
// failed.
type ActivityRefusal struct {
	Tool   string `json:"tool"`
	Reason string `json:"reason"`
}
