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

// chiefContestLimit marks delegated recovery as unbounded by a per-unit counter.
// The service budget and loop guard bound execution.
const chiefContestLimit = -1

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
	"Investigate repeated contests with inspect_code and factory_context, commission revised assignments or bounded investigation with route_amendment, and identify what new evidence justifies retrying. When the fix changes approved intent, needs an owner-reserved tradeoff, or requires an unavailable operational capability, call resolve_contested with escalate and a note for the owner: what is stuck, what you found and your recommendation. " +
	"A contest remains an internal engineering blocker until you explicitly escalate it. When the owner rules on a contested unit in a message, call resolve_contested with owner_decided true and the owner's words as the note. " +
	"When a unit is stuck or wrong outside a contest, such as checks that keep failing to complete, a block the service reports, or a stage that has to run again, call move_unit to move it to implementing, checking, reviewing with a note; independent review remains required; the service budget and loop guard bound recovery spending. Move it to contested to hold it for the owner when you cannot fix it. When the owner asks you to move a unit, call move_unit with owner_decided true and the owner's words as the note. " +
	"Hand a unit back to the role that can resolve what stopped it, not to the stage it stopped in. When the service refused its reviews as stale, another review of the same candidate is refused again: move it to implementing so its mason makes a new candidate on the current feature branch, and escalate if its workspace stays behind the feature branch. When its checks did not complete, rule review only once the checks can run again, and escalate when they cannot. When its mason kept leaving conflict markers, rule revise with a note naming the files and what each side holds. When its turns were interrupted again and again, rule review or revise so the work continues, and escalate if the service keeps stopping."

// driftHandbackGuidance tells the chief of staff how to handle a held drift
// rebase. It belongs in the system prompt of every chief-of-staff turn.
const driftHandbackGuidance = "A drift rebase is held when review sent its conflict resolution back too many times, its mason kept leaving conflict markers, or its turns kept being interrupted; drift/rebase.json and the feed say which. When you can say what its drift mason or drift reviewer should do differently, such as a reviewer that misreads an upstream already holding the feature's change, call hand_back_drift with that note: the service asks for another drift rebase, and both receive it. Otherwise leave the held drift rebase to the owner, whose inbox lists it. Record what new evidence or changed approach justifies another attempt; budgets and the loop guard bound recovery. When the owner asks you to hand it back, call hand_back_drift with owner_decided true and the owner's words as the note."

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
