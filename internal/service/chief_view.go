package service

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// chiefDocumentsGuidance tells every chief-of-staff turn what its view holds.
const chiefDocumentsGuidance = "Your read-only view holds the latest revision of every document this workstream records: spec.md, plan.json, what the owner handed in under handed/, the shed rounds under shed/, and the amendment, unit, final review and delivery records once they exist. units/<unit>/activity.json shows each started unit's state, its contest and the rulings the contest takes, its recent transitions and block reasons, and its roles' latest turns with their outcomes and the tool calls the service refused. Call file_read with a path to read a file or list a directory; \".\" lists the whole view. Read the documents a question or event concerns before you answer, escalate or report on it."

// stageChiefDocuments writes the latest revision of every document stream
// records, but its tool-call and inspection records, into workspace, replacing
// what an earlier turn staged there. It returns the top-level paths to select.
func stageChiefDocuments(r *trace.Repository, stream config.WorkstreamID, workspace string) ([]string, error) {
	docs, err := trace.Read[trace.Document](r, stream)
	if err != nil {
		return nil, err
	}
	latest := map[string]trace.Document{}
	for _, doc := range docs {
		if top, _, _ := strings.Cut(doc.Path, "/"); top == "tools" || top == "inspections" {
			continue
		}
		if prior, ok := latest[doc.Path]; !ok || doc.Revision >= prior.Revision {
			latest[doc.Path] = doc
		}
	}
	if err := os.RemoveAll(workspace); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(workspace, 0700); err != nil {
		return nil, err
	}
	var paths []string
	for _, name := range slices.Sorted(maps.Keys(latest)) {
		path := filepath.Join(workspace, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(latest[name].Content), 0600); err != nil {
			return nil, err
		}
		if top, _, _ := strings.Cut(name, "/"); !slices.Contains(paths, top) {
			paths = append(paths, top)
		}
	}
	activity, err := unitActivity(r, stream, docs)
	if err != nil {
		return nil, err
	}
	for unit, a := range activity {
		data, err := json.MarshalIndent(a, "", "  ")
		if err != nil {
			return nil, err
		}
		path := filepath.Join(workspace, "units", unit, "activity.json")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
			return nil, err
		}
		if !slices.Contains(paths, "units") {
			paths = append(paths, "units")
		}
	}
	return paths, nil
}

// activityTurns and activityTransitions bound what activity.json holds of
// each role's turns and of the unit's transitions; activityText bounds each
// report and final response.
const activityTurns, activityTransitions, activityText = 3, 12, 4000

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
// many more the chief of staff may give the unit.
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

func clip(text string) string {
	if len(text) <= activityText {
		return text
	}
	return text[:activityText] + "…"
}

// unitActivity returns the activity of every unit of the workstream that has
// a role thread, by unit.
func unitActivity(r *trace.Repository, stream config.WorkstreamID, docs []trace.Document) (map[string]UnitActivity, error) {
	threads, err := r.Threads(stream)
	if err != nil {
		return nil, err
	}
	transitions, err := trace.Read[trace.Transition](r, stream)
	if err != nil {
		return nil, err
	}
	refused := map[string][]ActivityRefusal{}
	for _, d := range docs {
		if !strings.HasPrefix(d.Path, "tools/") {
			continue
		}
		var call trace.ToolCall
		if json.Unmarshal([]byte(d.Content), &call) != nil || call.Output == nil && !call.Failed {
			continue
		}
		var out struct {
			Recorded *bool  `json:"recorded"`
			Reason   string `json:"reason"`
		}
		switch {
		case call.Failed:
			reason := "the call failed"
			if call.Output != nil && call.Output.Content != "" {
				reason = clip(call.Output.Content)
			}
			refused[call.Scope.Turn] = append(refused[call.Scope.Turn], ActivityRefusal{Tool: call.Name, Reason: reason})
		case json.Unmarshal([]byte(call.Output.Content), &out) == nil && out.Recorded != nil && !*out.Recorded:
			refused[call.Scope.Turn] = append(refused[call.Scope.Turn], ActivityRefusal{Tool: call.Name, Reason: out.Reason})
		}
	}
	out := map[string]UnitActivity{}
	for _, th := range threads {
		unit := th.Identity.Unit
		if unit == "" {
			continue
		}
		a, ok := out[unit]
		if !ok {
			state, err := r.Workflow(stream, trace.UnitSubject(unit))
			if err != nil {
				return nil, err
			}
			a = UnitActivity{Unit: unit, State: state.Value, Transitions: []ActivityTransition{}, Turns: []ActivityTurn{}}
			for _, t := range transitions {
				if t.Unit == unit {
					a.Transitions = append(a.Transitions, ActivityTransition{At: t.At.UTC().Format(time.RFC3339), Subject: t.Subject, From: t.From, To: t.To, Actor: t.Actor.Kind + ":" + t.Actor.ID, Reason: t.Reason})
				}
			}
			a.Transitions = a.Transitions[max(len(a.Transitions)-activityTransitions, 0):]
			if contest, contested, err := unitContest(r, stream, unit); err != nil {
				return nil, err
			} else if contested {
				_, mason, err := masonContest(r, stream, unit)
				if err != nil {
					return nil, err
				}
				left, err := chiefRulingsLeft(r, stream, unit)
				if err != nil {
					return nil, err
				}
				a.Contest = &ActivityContest{ID: contest.ID, Reason: contest.Reason, Rulings: contestOptions(contest, unit, mason), ChiefRulingsLeft: left}
				_, decisions, err := chiefDecisions(r, stream, unit)
				if err != nil {
					return nil, err
				}
				if i := slices.IndexFunc(decisions, func(d ChiefContestDecision) bool { return d.Contest == contest.ID }); i >= 0 {
					a.Contest.Decided = &decisions[i]
				}
			}
		}
		for _, q := range th.Turns[max(len(th.Turns)-activityTurns, 0):] {
			turn := ActivityTurn{Agent: th.Identity.ID, Turn: q.Request.TurnID, Status: q.Status(), Refused: refused[q.Request.TurnID]}
			if q.Response != nil {
				turn.FinalResponse = clip(q.Response.Result.FinalResponse)
				turn.Failure = q.Response.Failure
				if o := q.Response.Result.Outcome; o != nil {
					turn.Outcome, turn.Report = o.Status, clip(o.Report)
				}
			}
			a.Turns = append(a.Turns, turn)
		}
		out[unit] = a
	}
	return out, nil
}
