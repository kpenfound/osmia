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
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

const moveUnitTool = "move_unit"

// moveSources are the states a unit moves from: started, and not merged.
var moveSources = []string{UnitImplementing, UnitChecking, UnitReviewing, UnitApproved, UnitContested, UnitWaiting}

// moveTargets are the states a unit moves to.
var moveTargets = []string{UnitImplementing, UnitChecking, UnitReviewing, UnitApproved, UnitContested}

// UnitMoveRequest moves a unit to another state with a note for whoever
// takes it from there.
type UnitMoveRequest struct {
	To   string `json:"to"`
	Note string `json:"note"`
}

// UnitMove is the document units/<unit>/move-<version>.json: one move of a
// unit, by the owner or by the chief of staff on the owner's behalf, out of
// the state at version. ResetTurn is the mason turn a move to implementing
// resumes after, and Candidate the recorded candidate a move to checking,
// reviewing or approved takes forward.
type UnitMove struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Note      string `json:"note"`
	By        string `json:"by"`
	Turn      string `json:"turn,omitempty"`
	ResetTurn uint64 `json:"reset_turn,omitempty"`
	Candidate string `json:"candidate,omitempty"`
}

type UnitMoveResponse struct {
	Workstream config.WorkstreamID `json:"workstream"`
	Unit       string              `json:"unit"`
	Move       UnitMove            `json:"move"`
}

func moveDocument(unit string, version uint64) string {
	return fmt.Sprintf("%s-move-%d", trace.UnitSubject(unit), version)
}

func movePath(unit string, version uint64) string {
	return fmt.Sprintf("units/%s/move-%d.json", trace.UnitSubject(unit), version)
}

func movePrefix(unit string) string { return "units/" + trace.UnitSubject(unit) + "/move-" }

// isMove reports whether t is a move of the unit.
func isMove(t trace.Transition, unit string) bool {
	return t.Subject == trace.UnitSubject(unit) && strings.HasPrefix(t.ID, trace.UnitSubject(unit)+"-move-")
}

// moveContest reports whether a contest was raised by a move to contested,
// which raises the unit to the owner.
func moveContest(contest trace.Transition, unit string) bool {
	return contest.To == UnitContested && isMove(contest, unit)
}

// recordedMoves returns the unit's recorded moves with their documents, in the
// order they were recorded.
func recordedMoves(repo *trace.Repository, stream config.WorkstreamID, unit string) ([]trace.Document, []UnitMove, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return nil, nil, err
	}
	var records []trace.Document
	var moves []UnitMove
	for _, d := range docs {
		if d.Unit != unit || !strings.HasPrefix(d.Path, movePrefix(unit)) {
			continue
		}
		var m UnitMove
		if err := json.Unmarshal([]byte(d.Content), &m); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", d.Path, err)
		}
		records, moves = append(records, d), append(moves, m)
	}
	return records, moves, nil
}

// moveNote returns the note of the move a transition records.
func moveNote(repo *trace.Repository, stream config.WorkstreamID, unit string, t trace.Transition) (string, error) {
	records, moves, err := recordedMoves(repo, stream, unit)
	if err != nil {
		return "", err
	}
	if i := slices.IndexFunc(records, func(d trace.Document) bool { return d.ID == t.Cause }); i >= 0 {
		return moves[i].Note, nil
	}
	return "", nil
}

// unitOperation returns the action of a landing or rebase of the unit that
// has no result yet, or "" when none is in flight.
func unitOperation(repo *trace.Repository, stream config.WorkstreamID, unit string) (string, error) {
	records, err := repo.Operations(stream)
	if err != nil {
		return "", err
	}
	for _, r := range records {
		if r.Result != nil || r.Operation.Action != LandAction && r.Operation.Action != RebaseAction {
			continue
		}
		var in struct {
			Unit string `json:"unit"`
		}
		if err := json.Unmarshal(r.Operation.Input, &in); err != nil {
			return "", err
		}
		if in.Unit == unit {
			return r.Operation.Action, nil
		}
	}
	return "", nil
}

// moveUnit moves a started unit that has not merged to another state, as
// actor: the owner, or the chief of staff on the owner's behalf in turn. The
// move and its document are recorded together. A move to implementing gives
// the unit's mason a turn with the note and a fresh clean-turn allowance; to
// checking, a fresh check run of the recorded candidate; to reviewing, a
// fresh review of it; to approved, an approval of it that the foreman lands;
// and to contested, raises the unit to the owner. A move re-entering the
// unit's state restarts that stage.
func (s *Service) moveUnit(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, unit string, req UnitMoveRequest, actor trace.Actor, turn string) (UnitMoveResponse, *APIError) {
	note := strings.TrimSpace(req.Note)
	if !slices.Contains(moveTargets, req.To) || note == "" {
		return UnitMoveResponse{}, &APIError{Validation, "a move requires a note and a state: " + strings.Join(moveTargets, ", ")}
	}
	feature, err := repo.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return UnitMoveResponse{}, &APIError{Internal, "cannot read the workstream state"}
	}
	if feature.Value != BuildingState && feature.Value != AssembledState {
		return UnitMoveResponse{}, &APIError{Conflict, fmt.Sprintf("the workstream is %s; units move while it is building or assembled", featureState(feature.Value))}
	}
	state, err := repo.Workflow(stream, trace.UnitSubject(unit))
	if err != nil {
		return UnitMoveResponse{}, &APIError{Internal, "cannot read the unit state"}
	}
	if state.Value == "" {
		return UnitMoveResponse{}, &APIError{NotFound, fmt.Sprintf("the workstream has no unit %s", unit)}
	}
	if !slices.Contains(moveSources, state.Value) {
		return UnitMoveResponse{}, &APIError{Conflict, fmt.Sprintf("unit %s is %s; only a started unit that has not merged moves", unit, state.Value)}
	}
	if state.Value == req.To && (req.To == UnitApproved || req.To == UnitContested) {
		return UnitMoveResponse{}, &APIError{Conflict, fmt.Sprintf("unit %s is already %s", unit, req.To)}
	}
	if action, err := unitOperation(repo, stream, unit); err != nil {
		return UnitMoveResponse{}, &APIError{Internal, "cannot read the unit's operations"}
	} else if action == LandAction {
		return UnitMoveResponse{}, &APIError{Conflict, fmt.Sprintf("the foreman is landing unit %s; it moves once the landing ends", unit)}
	} else if action == RebaseAction {
		return UnitMoveResponse{}, &APIError{Conflict, fmt.Sprintf("the foreman is rebasing unit %s; it moves once the rebase ends", unit)}
	}
	// A move is caused by the chief-of-staff turn that made it, or else by
	// the transition into the state it leaves.
	cause := turn
	if cause == "" {
		transitions, err := trace.Read[trace.Transition](repo, stream)
		if err != nil {
			return UnitMoveResponse{}, &APIError{Internal, "cannot read the unit's transitions"}
		}
		for _, t := range transitions {
			if t.Subject == trace.UnitSubject(unit) {
				cause = t.ID
			}
		}
	}
	by := rulerName(actor)
	move := UnitMove{From: state.Value, To: req.To, Note: note, By: by, Turn: turn}
	id := moveDocument(unit, state.Version)
	at := s.now()
	var docs []trace.Document
	reason := fmt.Sprintf("%s moved unit %s from %s to %s", by, unit, state.Value, req.To)
	switch req.To {
	case UnitImplementing:
		th, err := repo.Thread(stream, masonAgent(unit))
		if err != nil || len(th.Turns) == 0 {
			return UnitMoveResponse{}, &APIError{Conflict, fmt.Sprintf("unit %s has no mason thread to resume", unit)}
		}
		move.ResetTurn = th.Turns[len(th.Turns)-1].Sequence
		reason += "; its mason revises it with a fresh clean-turn allowance"
	case UnitChecking, UnitReviewing:
		report, revision, err := unitReport(repo, stream, unit)
		if err != nil {
			return UnitMoveResponse{}, &APIError{Internal, "cannot read the unit report"}
		}
		if revision == 0 || report.Candidate == "" {
			return UnitMoveResponse{}, &APIError{Conflict, fmt.Sprintf("unit %s has no recorded candidate; move it to implementing", unit)}
		}
		move.Candidate = report.Candidate
		if req.To == UnitChecking {
			reason += fmt.Sprintf("; checks run again on candidate %s", report.Candidate)
		} else {
			reason += fmt.Sprintf("; its reviewer reviews candidate %s again", report.Candidate)
		}
	case UnitApproved:
		m := &masons{s: s, cfg: s.about(repo), repository: repo}
		_, identity, err := m.candidateEvidence(ctx, stream, unit)
		if err != nil {
			if ctx.Err() != nil {
				return UnitMoveResponse{}, &APIError{Internal, "the move was cancelled"}
			}
			return UnitMoveResponse{}, &APIError{Conflict, fmt.Sprintf("unit %s has no candidate to approve: %v", unit, err)}
		}
		move.Candidate = identity.Candidate.Revision
		review, err := approvalRecord(repo, stream, unit, identity, id, by, note, actor, at)
		if err != nil {
			return UnitMoveResponse{}, &APIError{Internal, "cannot read the unit review"}
		}
		docs = append(docs, review)
		reason += fmt.Sprintf("; the foreman lands candidate %s", identity.Candidate.Revision)
	case UnitContested:
		reason += "; the owner rules on it"
	}
	reason += ": " + note
	data, _ := json.MarshalIndent(move, "", "  ")
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), Workstream: stream, Unit: unit, At: at, Actor: actor, Cause: cause}
	docs = append([]trace.Document{{Header: h, Path: movePath(unit, state.Version), Content: string(data) + "\n"}}, docs...)
	h.Schema, h.ID, h.Cause = "osmia.trace.transition", id+"-"+req.To, id
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: trace.UnitSubject(unit), From: state.Value, To: req.To, Reason: reason},
		Events: []trace.Event{trace.Notice(h.ID, "unit", fmt.Sprintf("Unit %s: %s.", unit, reason))}}
	if _, err := repo.RecordDocumentsWith(ctx, docs, tx); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			return UnitMoveResponse{}, &APIError{Conflict, fmt.Sprintf("unit %s moved since it was read; read it again", unit)}
		}
		return UnitMoveResponse{}, &APIError{Internal, "cannot record the move"}
	}
	return UnitMoveResponse{Workstream: stream, Unit: unit, Move: move}, nil
}

// approvalRecord returns the next revision of the unit's review.json: a
// satisfactory decision on identity's candidate by whoever moved the unit to
// approved, which is the approval the foreman lands. Send-backs counted so far
// stay counted.
func approvalRecord(repo *trace.Repository, stream config.WorkstreamID, unit string, identity UnitReviewIdentity, move, by, note string, actor trace.Actor, at time.Time) (trace.Document, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return trace.Document{}, err
	}
	var latest trace.Document
	for _, d := range docs {
		if d.ID == reviewDocument(unit) {
			latest = d
		}
	}
	result := UnitReviewResult{Identity: identity, Turn: move, Verdict: UnitVerdict{Decision: "satisfactory", Summary: fmt.Sprintf("The %s approved candidate %s by moving the unit to approved: %s", by, identity.Candidate.Revision, note), Findings: []ReviewFinding{}}}
	var prior UnitReviewResult
	if latest.Revision > 0 && json.Unmarshal([]byte(latest.Content), &prior) == nil {
		result.Bounces = prior.Bounces
	}
	content, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return trace.Document{}, err
	}
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: reviewDocument(unit), Revision: latest.Revision + 1, Project: repo.Project(), Workstream: stream, Unit: unit, At: at, Actor: actor, Cause: move}
	return trace.Document{Header: h, Path: fmt.Sprintf("units/%s/review.json", unit), Content: string(content) + "\n"}, nil
}

// ownerMove moves a unit as the owner.
func (s *Service) ownerMove(ctx context.Context, raw, unit string, req UnitMoveRequest) (UnitMoveResponse, *APIError) {
	_, stream, repo, api := s.conversationTrace(raw)
	if api != nil {
		return UnitMoveResponse{}, api
	}
	if unit == "" {
		return UnitMoveResponse{}, &APIError{Validation, "a move requires a unit"}
	}
	if gone, err := abandoned(repo, stream); err != nil {
		return UnitMoveResponse{}, &APIError{Internal, "cannot read the workstream state"}
	} else if gone {
		return UnitMoveResponse{}, &APIError{Conflict, "an abandoned workstream's units do not move"}
	}
	return s.moveUnit(ctx, repo, stream, unit, req, ownerActor, "")
}

// moveUnit returns the move_unit tool of one claimed chief-of-staff turn.
// The chief of staff moves a unit of its workstream on the owner's behalf,
// sharing chiefContestLimit with its contest rulings; a move to contested
// raises the unit to the owner and is always open to it. A contest the owner
// holds, or one it already decided, is the owner's to move. In a turn
// answering the owner, it records the owner's own move. A move the service
// refuses is an ordinary result, {"recorded":false,"reason":...}, and records
// nothing.
func (c *runtimeControls) moveUnit(repository *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	tool := coreadapter.Tool{Name: moveUnitTool, Effect: coreadapter.ToolMemory,
		Description: "Move a unit of this workstream to another state when the state machine leaves it stuck or wrong. unit: its ID; to: implementing (its mason revises the unit in its workspace, with a fresh clean-turn allowance), checking (the service runs the checks again on the recorded candidate), reviewing (its reviewer reviews the recorded candidate again), approved (you approve the recorded candidate and the foreman lands it) or contested (hold the unit for the owner); " +
			"note: for the mason or reviewer, what to do differently, which it receives; for approved, why the candidate holds; for contested, what is stuck, what you found and your recommendation. Moving a unit to the state it is in restarts that stage. " +
			"Move only when you are confident the move resolves the problem; otherwise move it to contested. owner_decided: true only when the owner asked for the move in the message this turn answers, with the owner's words as the note. The unit's state and recent transitions are in units/<unit>/activity.json.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"unit":{"type":"string"},"to":{"type":"string","enum":["implementing","checking","reviewing","approved","contested"]},"note":{"type":"string"},"owner_decided":{"type":"boolean"}},"required":["unit","to","note"],"additionalProperties":false}`)}
	tool.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input struct {
			Unit         string `json:"unit"`
			To           string `json:"to"`
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
			return nil, errors.New("unit moves are unavailable")
		}
		if err := repository.ActiveTurn(scope); err != nil {
			return nil, err
		}
		stream := config.WorkstreamID(scope.Workstream)
		request := UnitMoveRequest{To: input.To, Note: input.Note}
		if gone, err := abandoned(repository, stream); err != nil {
			return nil, err
		} else if gone {
			return priorityRefusal("an abandoned workstream's units do not move")
		}
		if input.OwnerDecided {
			turn, owner, err := repository.OwnerTurn(trace.ChiefOfStaff, scope)
			if err != nil {
				return nil, err
			}
			if !owner {
				return priorityRefusal("owner_decided is only for a move the owner asked for in the message this turn answers")
			}
			return moveResult(s.moveUnit(ctx, repository, stream, input.Unit, request, turn.Actor, scope.Turn))
		}
		if contest, contested, err := unitContest(repository, stream, input.Unit); err != nil {
			return nil, err
		} else if contested {
			if moveContest(contest, input.Unit) {
				return priorityRefusal(fmt.Sprintf("unit %s was moved to contested for the owner; the owner moves it", input.Unit))
			}
			_, decisions, err := chiefDecisions(repository, stream, input.Unit)
			if err != nil {
				return nil, err
			}
			if slices.ContainsFunc(decisions, func(d ChiefContestDecision) bool { return d.Contest == contest.ID }) {
				return priorityRefusal(fmt.Sprintf("you already decided contest %s of unit %s; the owner moves it", contest.ID, input.Unit))
			}
		}
		if input.To != UnitContested {
			if left, err := chiefRulingsLeft(repository, stream, input.Unit); err != nil {
				return nil, err
			} else if left == 0 {
				return priorityRefusal(fmt.Sprintf("you moved or ruled on unit %s %d times since the owner last acted on it; move it to contested for the owner", input.Unit, chiefContestLimit))
			}
		}
		return moveResult(s.moveUnit(ctx, repository, stream, input.Unit, request, chiefActor, scope.Turn))
	}
	return tool
}

func moveResult(out UnitMoveResponse, api *APIError) (json.RawMessage, error) {
	if api != nil {
		if api.Code == Internal {
			return nil, errors.New(api.Message)
		}
		return priorityRefusal(api.Message)
	}
	return json.Marshal(struct {
		Recorded bool     `json:"recorded"`
		Unit     string   `json:"unit"`
		Move     UnitMove `json:"move"`
	}{true, out.Unit, out.Move})
}
