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
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/trace"
)

// stageChiefDocuments stages current documents, excluding tool calls,
// inspections and a resolved member's own spec and plan. Historical revisions
// remain available through factory_context. Each staging replaces the last.
func stageChiefDocuments(r *trace.Repository, stream config.WorkstreamID, workspace string) ([]string, error) {
	docs, err := trace.Read[trace.Document](r, stream)
	if err != nil {
		return nil, err
	}
	active, err := trace.MemberDocumentsActive(r, stream)
	if err != nil {
		return nil, err
	}
	latest := map[string]trace.Document{}
	for _, doc := range docs {
		if !active && (doc.Path == plan.SpecPath || doc.Path == plan.PlanPath) {
			continue
		}
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
