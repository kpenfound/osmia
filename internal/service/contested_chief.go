package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// chiefActor is the chief of staff acting on the owner's behalf.
var chiefActor = trace.Actor{Kind: "agent", ID: trace.ChiefOfStaff}

const resolveContestedTool = "resolve_contested"

// chiefContestLimit is how many contests of one unit in a row the chief of
// staff may rule on before the unit's contests are the owner's.
const chiefContestLimit = 2

// escalateContest is the chief of staff's decision to raise a contest to the
// owner.
const escalateContest = "escalate"

// ChiefContestDecision records the chief of staff's decision on one contest:
// a ruling it gave, or its escalation to the owner.
type ChiefContestDecision struct {
	Contest  string `json:"contest"`
	Decision string `json:"decision"`
	Note     string `json:"note"`
	// Turn is the chief-of-staff turn that decided.
	Turn string `json:"turn"`
}

func chiefContestPath(unit, contest string) string {
	return fmt.Sprintf("units/%s/chief-%s.json", trace.UnitSubject(unit), contest)
}

// contestGuidance tells the chief of staff how to handle contested units. It
// belongs in the system prompt of every chief-of-staff turn.
const contestGuidance = "A contested unit is stuck until someone rules on it. You rule first, on the owner's behalf: read units/<unit>/activity.json for the contest, the rulings it takes, the unit's recent turns and anything the service refused. " +
	"When you are confident a ruling resolves it, call resolve_contested with review (the reviewer reviews the candidate again) or revise (the mason revises the unit), and a note the resumed role receives saying exactly what to do differently. " +
	"When you cannot tell what is wrong, the fix needs a decision the owner has not made, or the unit has been contested again after your ruling, call resolve_contested with escalate and a note for the owner: what is stuck, what you found and your recommendation. " +
	"A contest you see and do not resolve goes to the owner. When the owner rules on a contested unit in a message, call resolve_contested with owner_decided true and the owner's words as the note. " +
	"When a unit is stuck or wrong outside a contest, such as checks that keep failing to complete, a block the service reports, or a stage that has to run again, call move_unit to move it to implementing, checking, reviewing or approved with a note; your moves share the limit on your rulings. Move it to contested to hold it for the owner when you cannot fix it. When the owner asks you to move a unit, call move_unit with owner_decided true and the owner's words as the note."

// unitContest returns the unit's latest move to contested, and whether the
// unit is contested by it now.
func unitContest(repo *trace.Repository, stream config.WorkstreamID, unit string) (trace.Transition, bool, error) {
	state, err := repo.Workflow(stream, trace.UnitSubject(unit))
	if err != nil || state.Value != UnitContested {
		return trace.Transition{}, false, err
	}
	contest, _, err := masonContest(repo, stream, unit)
	return contest, err == nil && contest.ID != "", err
}

// chiefDecisions returns the chief of staff's contest decisions on the unit,
// in the order they were recorded.
func chiefDecisions(repo *trace.Repository, stream config.WorkstreamID, unit string) ([]trace.Document, []ChiefContestDecision, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return nil, nil, err
	}
	prefix := "units/" + trace.UnitSubject(unit) + "/chief-"
	var records []trace.Document
	var decisions []ChiefContestDecision
	for _, d := range docs {
		if !strings.HasPrefix(d.Path, prefix) {
			continue
		}
		var decision ChiefContestDecision
		if err := json.Unmarshal([]byte(d.Content), &decision); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", d.Path, err)
		}
		records, decisions = append(records, d), append(decisions, decision)
	}
	return records, decisions, nil
}

// chiefRulingsLeft returns how many more times the chief of staff may rule on
// a contest of the unit or move it: chiefContestLimit less its rulings and
// moves since the owner last ruled on or moved the unit. Escalations and moves
// to contested raise the unit to the owner and use none.
func chiefRulingsLeft(repo *trace.Repository, stream config.WorkstreamID, unit string) (int, error) {
	transitions, err := trace.Read[trace.Transition](repo, stream)
	if err != nil {
		return 0, err
	}
	var owner trace.Transition
	for _, t := range transitions {
		if t.Subject == trace.UnitSubject(unit) && t.Actor == ownerActor {
			owner = t
		}
	}
	records, decisions, err := chiefDecisions(repo, stream, unit)
	if err != nil {
		return 0, err
	}
	used := 0
	for i, d := range decisions {
		if d.Decision != escalateContest && records[i].At.After(owner.At) {
			used++
		}
	}
	moved, moves, err := recordedMoves(repo, stream, unit)
	if err != nil {
		return 0, err
	}
	for i, m := range moves {
		if m.By == rulerName(chiefActor) && m.To != UnitContested && moved[i].At.After(owner.At) {
			used++
		}
	}
	return max(chiefContestLimit-used, 0), nil
}

// contestRaised reports whether a contest is the owner's to rule, and the
// chief of staff's note when it escalated it. It is the owner's when a move
// to contested raised it, when the chief of staff escalated it, has no
// rulings left for the unit, or has seen it and left it undecided: its event
// was acknowledged after a completed chief-of-staff turn or failed to be
// delivered, or it raised no event.
func contestRaised(repo *trace.Repository, stream config.WorkstreamID, unit string, contest trace.Transition) (bool, string, error) {
	if moveContest(contest, unit) {
		note, err := moveNote(repo, stream, unit, contest)
		return err == nil, note, err
	}
	_, decisions, err := chiefDecisions(repo, stream, unit)
	if err != nil {
		return false, "", err
	}
	if i := slices.IndexFunc(decisions, func(d ChiefContestDecision) bool { return d.Contest == contest.ID }); i >= 0 {
		// A ruling that left the unit on this contest did not resolve it.
		return true, decisions[i].Note, nil
	}
	if left, err := chiefRulingsLeft(repo, stream, unit); err != nil || left == 0 {
		return err == nil, "", err
	}
	entries, err := repo.Outbox(stream)
	if err != nil {
		return false, "", err
	}
	seen := true
	for _, e := range entries {
		if e.TransitionID != contest.ID {
			continue
		}
		failed := slices.ContainsFunc(e.History, func(a trace.DeliveryAction) bool { return a.Kind == "release" })
		seen = e.Acknowledged || failed
		if !seen {
			break
		}
	}
	return seen, "", nil
}

// resolveContested returns the resolve_contested tool of one claimed
// chief-of-staff turn. The chief of staff rules on a contested unit of its
// workstream on the owner's behalf, within chiefContestLimit, or escalates it
// to the owner; in a turn answering the owner, it records the owner's own
// ruling. A decision the service refuses is an ordinary result,
// {"recorded":false,"reason":...}, and records nothing.
func (c *runtimeControls) resolveContested(repository *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	tool := coreadapter.Tool{Name: resolveContestedTool, Effect: coreadapter.ToolMemory,
		Description: "Resolve a contested unit of this workstream. unit: its ID; decision: review (its reviewer reviews the candidate again), revise (its mason revises the unit) or escalate (the owner rules); note: for review and revise, what the resumed role must do differently, which it receives; for escalate, what is stuck, what you found and your recommendation. " +
			"Rule only when you are confident the ruling resolves the contest; otherwise escalate. owner_decided: true only when the owner ruled in the message this turn answers, with the owner's words as the note. The rulings a contest takes are in units/<unit>/activity.json.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"unit":{"type":"string"},"decision":{"type":"string","enum":["review","revise","escalate"]},"note":{"type":"string"},"owner_decided":{"type":"boolean"}},"required":["unit","decision","note"],"additionalProperties":false}`)}
	tool.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input struct {
			Unit         string `json:"unit"`
			Decision     string `json:"decision"`
			Note         string `json:"note"`
			OwnerDecided bool   `json:"owner_decided"`
		}
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if err := d.Decode(&input); err != nil {
			return nil, fmt.Errorf("tool input: %w", err)
		}
		if err := d.Decode(new(any)); err != io.EOF {
			return nil, errors.New("tool input must be one object")
		}
		s := c.service.Load()
		if s == nil {
			return nil, errors.New("contested rulings are unavailable")
		}
		if err := repository.ActiveTurn(scope); err != nil {
			return nil, err
		}
		stream := config.WorkstreamID(scope.Workstream)
		note := strings.TrimSpace(input.Note)
		if note == "" {
			return priorityRefusal("a note is required")
		}
		contest, contested, err := unitContest(repository, stream, input.Unit)
		if err != nil {
			return nil, err
		}
		if !contested {
			return priorityRefusal(fmt.Sprintf("unit %s is not contested; move_unit moves a unit that is not", input.Unit))
		}
		request := ContestedRulingRequest{Decision: input.Decision, Note: note}
		if input.OwnerDecided {
			turn, owner, err := repository.OwnerTurn(trace.ChiefOfStaff, scope)
			if err != nil {
				return nil, err
			}
			if !owner {
				return priorityRefusal("owner_decided is only for a ruling the owner gave in the message this turn answers")
			}
			if input.Decision == escalateContest {
				return priorityRefusal("the owner rules review or revise")
			}
			return rulingResult(s.recordContestedRuling(ctx, repository, stream, input.Unit, request, turn.Actor))
		}
		if moveContest(contest, input.Unit) {
			return priorityRefusal(fmt.Sprintf("unit %s was moved to contested for the owner; the owner rules on it", input.Unit))
		}
		_, decisions, err := chiefDecisions(repository, stream, input.Unit)
		if err != nil {
			return nil, err
		}
		if slices.ContainsFunc(decisions, func(d ChiefContestDecision) bool { return d.Contest == contest.ID }) {
			return priorityRefusal(fmt.Sprintf("you already decided contest %s of unit %s; the owner rules on it", contest.ID, input.Unit))
		}
		if input.Decision != escalateContest {
			if left, err := chiefRulingsLeft(repository, stream, input.Unit); err != nil {
				return nil, err
			} else if left == 0 {
				return priorityRefusal(fmt.Sprintf("you ruled on unit %s's last %d contests and it is contested again; escalate it to the owner", input.Unit, chiefContestLimit))
			}
		}
		record := func() error {
			data, _ := json.MarshalIndent(ChiefContestDecision{Contest: contest.ID, Decision: input.Decision, Note: note, Turn: scope.Turn}, "", "  ")
			h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: trace.UnitSubject(input.Unit) + "-chief-" + contest.ID, Revision: 1, Project: repository.Project(), Workstream: stream, Unit: input.Unit, At: s.now(), Actor: chiefActor, Cause: contest.ID}
			return repository.RecordDocuments(ctx, []trace.Document{{Header: h, Path: chiefContestPath(input.Unit, contest.ID), Content: string(data) + "\n"}})
		}
		if input.Decision == escalateContest {
			if err := record(); err != nil {
				return nil, err
			}
			return json.Marshal(struct {
				Recorded bool   `json:"recorded"`
				Unit     string `json:"unit"`
				Detail   string `json:"detail"`
			}{true, input.Unit, "the contest is in the owner's inbox with your note"})
		}
		out, api := s.recordContestedRuling(ctx, repository, stream, input.Unit, request, chiefActor)
		if api == nil {
			if err := record(); err != nil {
				return nil, err
			}
		}
		return rulingResult(out, api)
	}
	return tool
}

func rulingResult(out ContestedRulingResponse, api *APIError) (json.RawMessage, error) {
	if api != nil {
		if api.Code == Internal {
			return nil, errors.New(api.Message)
		}
		return priorityRefusal(api.Message)
	}
	return json.Marshal(struct {
		Recorded bool            `json:"recorded"`
		Unit     string          `json:"unit"`
		Ruling   ContestedRuling `json:"ruling"`
	}{true, out.Unit, out.Ruling})
}
