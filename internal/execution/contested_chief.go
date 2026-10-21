package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

const resolveContestedTool = "resolve_contested"

// chiefContestLimit marks delegated recovery as unbounded by a per-unit counter.
// The service budget and loop guard bound execution.
const chiefContestLimit = -1

// escalateContest is the chief of staff's decision to raise a contest to the
// owner.
const escalateContest = "escalate"

func chiefContestPath(unit, contest string) string {
	return fmt.Sprintf("units/%s/chief-%s.json", trace.UnitSubject(unit), trace.EventID(contest, "chief"))
}

// chiefDecisionDocument uses a bounded stable key; the contest already names
// the unit, so repeating the subject could exceed the trace's key limit.
func chiefDecisionDocument(repo *trace.Repository, stream config.WorkstreamID, unit, contest, decision, note, turn string, at time.Time) trace.Document {
	data, _ := json.MarshalIndent(ChiefContestDecision{Contest: contest, Decision: decision, Note: note, Turn: turn}, "", "  ")
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: trace.EventID(contest, "chief"), Revision: 1, Project: repo.Project(), Workstream: stream, Unit: unit, At: at, Actor: chiefActor, Cause: contest}
	return trace.Document{Header: h, Path: chiefContestPath(unit, contest), Content: string(data) + "\n"}
}

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

// chiefRulingsLeft returns -1: engineering recovery is not an owner gate.
func chiefRulingsLeft(repo *trace.Repository, stream config.WorkstreamID, unit string) (int, error) {
	return -1, nil
}

// contestRaised reports explicit escalation or an owner-held contest.
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
	return false, "", nil
}

// resolveContested returns the resolve_contested tool of one claimed
// chief-of-staff turn. The chief of staff rules on a contested unit of its
// workstream on the owner's behalf, or explicitly escalates it
// to the owner; in a turn answering the owner, it records the owner's own
// ruling. A decision the service refuses is an ordinary result,
// {"recorded":false,"reason":...}, and records nothing. A shared chief of
// staff names the member whose unit is contested.
func (c *Controls) resolveContested(repository *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	tool := coreadapter.Tool{Name: resolveContestedTool, Effect: coreadapter.ToolMemory,
		Description: "Resolve a contested unit of this workstream. unit: its ID; decision: review (its reviewer reviews the candidate again), revise (its mason revises the unit) or escalate (the owner rules); note: for review and revise, what the resumed role must do differently, which it receives; for escalate, in a few short sentences for the owner, what is stuck, the finding that matters and what you recommend they rule. " +
			"Rule only when you are confident the ruling resolves the contest; otherwise escalate. owner_decided: true only when the owner ruled in the message this turn answers, with the owner's words as the note. The rulings a contest takes are in units/<unit>/activity.json. workstream: a member workstream ID of your workgroup to act on; omit for this conversation's own workstream.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"unit":{"type":"string"},"decision":{"type":"string","enum":["review","revise","escalate"]},"note":{"type":"string"},"owner_decided":{"type":"boolean"},"workstream":{"type":"string"}},"required":["unit","decision","note"],"additionalProperties":false}`)}
	tool.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input struct {
			Unit         string              `json:"unit"`
			Decision     string              `json:"decision"`
			Note         string              `json:"note"`
			OwnerDecided bool                `json:"owner_decided"`
			Workstream   config.WorkstreamID `json:"workstream"`
		}
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if err := d.Decode(&input); err != nil {
			return nil, fmt.Errorf("tool input: %w", err)
		}
		if err := d.Decode(new(any)); err != io.EOF {
			return nil, errors.New("tool input must be one object")
		}
		s := c.Load()
		if s == nil {
			return nil, errors.New("contested rulings are unavailable")
		}
		if err := repository.ActiveTurn(scope); err != nil {
			return nil, err
		}
		target, stream, refusal, err := repository.ChiefMember(config.WorkstreamID(scope.Workstream), input.Workstream)
		if err != nil {
			return nil, err
		}
		if refusal != "" {
			return priorityRefusal(refusal)
		}
		note := strings.TrimSpace(input.Note)
		if note == "" {
			return priorityRefusal("a note is required")
		}
		contest, contested, err := unitContest(target, stream, input.Unit)
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
			if bounceContest(contest, input.Unit) {
				decision := chiefDecisionDocument(target, stream, input.Unit, contest.ID, input.Decision, note, scope.Turn, s.Now())
				// The owner may be answering this chief's escalation. Retain
				// that decision's identity and append its ruling revision.
				docs, prior, err := chiefDecisions(target, stream, input.Unit)
				if err != nil {
					return nil, err
				}
				for i, p := range prior {
					if p.Contest == contest.ID {
						decision.ID, decision.Path, decision.Revision = docs[i].ID, docs[i].Path, docs[i].Revision+1
					}
				}
				return rulingResult(s.RecordContestedRuling(ctx, target, stream, input.Unit, request, turn.Actor, decision))
			}
			return rulingResult(s.RecordContestedRuling(ctx, target, stream, input.Unit, request, turn.Actor))
		}
		if moveContest(contest, input.Unit) {
			return priorityRefusal(fmt.Sprintf("unit %s was moved to contested for the owner; the owner rules on it", input.Unit))
		}
		_, decisions, err := chiefDecisions(target, stream, input.Unit)
		if err != nil {
			return nil, err
		}
		if slices.ContainsFunc(decisions, func(d ChiefContestDecision) bool { return d.Contest == contest.ID }) {
			return priorityRefusal(fmt.Sprintf("you already decided contest %s of unit %s; the owner rules on it", contest.ID, input.Unit))
		}

		decision := chiefDecisionDocument(target, stream, input.Unit, contest.ID, input.Decision, note, scope.Turn, s.Now())
		record := func() error {
			if bounceContest(contest, input.Unit) {
				_, err := target.RecordDocumentsWith(ctx, []trace.Document{decision})
				return err
			}
			return target.RecordDocuments(ctx, []trace.Document{decision})
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
		if bounceContest(contest, input.Unit) {
			return rulingResult(s.RecordContestedRuling(ctx, target, stream, input.Unit, request, chiefActor, decision))
		}
		out, api := s.RecordContestedRuling(ctx, target, stream, input.Unit, request, chiefActor)
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
