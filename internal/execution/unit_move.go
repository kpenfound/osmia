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
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/trace"
)

const MoveUnitTool = "move_unit"

var moveSources = []string{UnitPlanned, UnitReady, UnitImplementing, UnitChecking, UnitReviewing, UnitApproved, UnitContested, UnitWaiting}

var moveTargets = []string{UnitPlanned, UnitReady, UnitImplementing, UnitChecking, UnitReviewing, UnitApproved, UnitContested}

func moveDocument(unit string, version uint64) string {
	return fmt.Sprintf("%s-move-%d", trace.UnitSubject(unit), version)
}

func movePath(unit string, version uint64) string {
	return fmt.Sprintf("units/%s/move-%d.json", trace.UnitSubject(unit), version)
}

func MovePrefix(unit string) string { return "units/" + trace.UnitSubject(unit) + "/move-" }

func IsMove(t trace.Transition, unit string) bool {
	return t.Subject == trace.UnitSubject(unit) && strings.HasPrefix(t.ID, trace.UnitSubject(unit)+"-move-")
}

func MoveContest(contest trace.Transition, unit string) bool {
	return contest.To == UnitContested && IsMove(contest, unit)
}

func recordedMoves(repo *trace.Repository, stream config.WorkstreamID, unit string) ([]trace.Document, []UnitMove, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return nil, nil, err
	}
	var records []trace.Document
	var moves []UnitMove
	for _, d := range docs {
		if d.Unit != unit || !strings.HasPrefix(d.Path, MovePrefix(unit)) {
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

func unitOperation(repo *trace.Repository, stream config.WorkstreamID, unit string) (string, error) {
	return unitOperationExcept(repo, stream, unit, "")
}

func operationUnit(op coreadapter.Operation) (string, error) {
	var in struct {
		Unit string `json:"unit"`
	}
	if err := json.Unmarshal(op.Input, &in); err != nil {
		return "", err
	}
	return in.Unit, nil
}

// MoveUnit moves a unit that has not merged to another state, as actor: the
// owner, or the manager in turn. The move and its document are recorded
// together. A move to implementing gives the unit's mason a turn with the note
// and a fresh clean-turn allowance; to checking, a fresh check run of the
// recorded candidate; to reviewing, a fresh review of it; to approved, an
// approval of it that the foreman lands; to contested, raises the unit to the
// owner; and a unit that has not started moves to ready, which its mason starts
// when its turn comes, or back to planned. A move re-entering the unit's state
// restarts that stage. The owner's move is a pinned move; the manager's
// is a proposal that the router decides.
func (s *Controller) MoveUnit(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, unit string, req UnitMoveRequest, actor trace.Actor, turn string) (UnitMoveResponse, *APIError) {
	return s.moveUnit(ctx, repo, stream, unit, req, actor, turn, "")
}

func (s *Controller) moveUnit(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, unit string, req UnitMoveRequest, actor trace.Actor, turn, judgment string) (UnitMoveResponse, *APIError) {
	note := strings.TrimSpace(req.Note)
	if !slices.Contains(moveTargets, req.To) || note == "" {
		return UnitMoveResponse{}, &APIError{Code: Validation, Message: "a move requires a note and a state: " + strings.Join(moveTargets, ", ")}
	}
	feature, err := repo.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return UnitMoveResponse{}, &APIError{Code: Internal, Message: "cannot read the workstream state"}
	}
	if feature.Value != BuildingState && feature.Value != AssembledState {
		return UnitMoveResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("the workstream is %s; units move while it is building or assembled", featureState(feature.Value))}
	}
	state, err := repo.Workflow(stream, trace.UnitSubject(unit))
	if err != nil {
		return UnitMoveResponse{}, &APIError{Code: Internal, Message: "cannot read the unit state"}
	}
	if state.Value == "" {
		return UnitMoveResponse{}, &APIError{Code: NotFound, Message: fmt.Sprintf("the workstream has no unit %s", unit)}
	}
	if !slices.Contains(moveSources, state.Value) {
		return UnitMoveResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("unit %s is %s; only a started unit that has not merged moves", unit, state.Value)}
	}
	if why, err := s.moveLegality(repo, stream, unit, state.Value, req.To); err != nil {
		return UnitMoveResponse{}, &APIError{Code: Internal, Message: "cannot read the unit's plan"}
	} else if why != "" {
		return UnitMoveResponse{}, &APIError{Code: Conflict, Message: why}
	}
	if state.Value == req.To && (req.To == UnitApproved || req.To == UnitContested) {
		return UnitMoveResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("unit %s is already %s", unit, req.To)}
	}
	if action, err := unitOperation(repo, stream, unit); err != nil {
		return UnitMoveResponse{}, &APIError{Code: Internal, Message: "cannot read the unit's operations"}
	} else if action == LandAction {
		return UnitMoveResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("the foreman is landing unit %s; it moves once the landing ends", unit)}
	} else if action == RebaseAction {
		return UnitMoveResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("the foreman is rebasing unit %s; it moves once the rebase ends", unit)}
	}
	at := s.Now()
	pinned := actor == trace.JevActor || actor.Kind == ownerActor.Kind
	var records unitMoveRecord
	if pinned || req.To != UnitApproved {
		var api *APIError
		if records, api = s.unitMoveRecords(ctx, repo, stream, unit, state, req.To, note, actor, turn, "", "", at); api != nil {
			return UnitMoveResponse{}, api
		}
	}
	cause := fmt.Sprintf("%s-%d", moveDocument(unit, state.Version), at.UnixNano())
	var in UnitExitInput
	switch {
	case actor == trace.JevActor:
		if judgment == "" {
			return UnitMoveResponse{}, &APIError{Code: Validation, Message: "a Jev move names its judgment"}
		}
		in = UnitExitInput{Repo: repo, Stream: stream, Unit: unit, State: state, Default: req.To, Note: note, Turn: turn, Actor: actor, Active: true, Trigger: TriggerJevRuling, Cause: cause,
			Authority: &router.Authority{Kind: router.AuthorityJevGate, Target: req.To, Judgment: judgment}}
	case actor.Kind == ownerActor.Kind:
		in = UnitExitInput{Repo: repo, Stream: stream, Unit: unit, State: state, Default: req.To, Note: note, Turn: turn, Actor: actor, Active: true, Trigger: TriggerMoveOwner, Cause: cause,
			Authority: &router.Authority{Kind: router.AuthorityOwner, Target: req.To}}
	default:
		if in, err = s.proposalInput(repo, stream, unit, state, req.To, note, turn, actor, cause); err != nil {
			return UnitMoveResponse{}, &APIError{Code: Internal, Message: "cannot read the unit's history"}
		}
		if len(in.Gate) == 0 {
			in.Default, in.ID, in.Reason = req.To, "", ""
			if unitStarted(state.Value) {
				in.Targets = nil
			}
			in.Authority = &router.Authority{Kind: router.AuthorityManager, Target: req.To}
		}
	}
	res, err := s.raise(ctx, false, in)
	switch {
	case err != nil:
		return UnitMoveResponse{}, &APIError{Code: Internal, Message: "cannot record the move"}
	case res.Committed && res.Target == TargetWait:
		return UnitMoveResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("unit %s stays %s and was not moved to %s: by default the manager does not move a unit out of a contest it raised or ruled on itself, and Jev judged that the evidence does not support leaving it. The proposal, the earlier moves and rulings, and Jev's judgment are in the trace", unit, state.Value, req.To)}
	case res.Committed:
		move := records.move
		if res.Target != req.To {
			move = UnitMove{From: state.Value, To: res.Target, Note: note, By: trace.DecidedByJev, Turn: turn}
		}
		return UnitMoveResponse{Workstream: stream, Unit: unit, Move: move}, nil
	case res.Outcome == router.OutcomeHeld && res.Hold == router.HoldStale:
		return UnitMoveResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("unit %s moved since it was read; read it again", unit)}
	case res.Pending():
		return UnitMoveResponse{}, &APIError{Code: Conflict, Message: pendingOwnerMove(unit, req.To, res)}
	}
	return UnitMoveResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("unit %s was not moved to %s: the move is held (%s). %s Nothing moved, and the unit keeps its state", unit, req.To, res.Hold, res.Reason)}
}

func pendingOwnerMove(unit, to string, res router.Result) string {
	why := "the owner decides it"
	switch {
	case to == UnitApproved:
		why = "approving a candidate without an independent review is the owner's decision"
	case res.Hold == router.HoldIntent:
		why = "Jev judged that it would change what the owner approved"
	case res.Hold == router.HoldGate:
		why = "the owner holds this unit, and a proposal cannot move it"
	}
	return fmt.Sprintf("unit %s was not moved to %s: %s. The move is pending as an owner decision on %s; nothing moved", unit, to, why, recoveryCard)
}

type unitMoveRecord struct {
	move UnitMove
	docs []trace.Document
	tx   trace.Transaction
}

func (s *Controller) unitMoveRecords(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, unit string, state trace.WorkflowState, to, note string, actor trace.Actor, turn, cause, transition string, at time.Time) (unitMoveRecord, *APIError) {
	by := RulerName(actor)
	move := UnitMove{From: state.Value, To: to, Note: note, By: by, Turn: turn}
	id := moveDocument(unit, state.Version)
	var docs []trace.Document
	reason := fmt.Sprintf("%s moved unit %s from %s to %s", trace.DisplayRuler(by), unit, state.Value, to)
	switch to {
	case UnitImplementing:
		th, err := repo.Thread(stream, masonAgent(unit))
		if err != nil || len(th.Turns) == 0 {
			return unitMoveRecord{}, &APIError{Code: Conflict, Message: fmt.Sprintf("unit %s has no mason thread to resume", unit)}
		}
		move.ResetTurn = th.Turns[len(th.Turns)-1].Sequence
		reason += "; its mason revises it with a fresh clean-turn allowance"
	case UnitChecking, UnitReviewing:
		report, revision, err := unitReport(repo, stream, unit)
		if err != nil {
			return unitMoveRecord{}, &APIError{Code: Internal, Message: "cannot read the unit report"}
		}
		if revision == 0 || report.Candidate == "" {
			return unitMoveRecord{}, &APIError{Code: Conflict, Message: fmt.Sprintf("unit %s has no recorded candidate; move it to implementing", unit)}
		}
		move.Candidate = report.Candidate
		if to == UnitChecking {
			reason += fmt.Sprintf("; checks run again on candidate %s", report.Candidate)
		} else {
			reason += fmt.Sprintf("; its reviewer reviews candidate %s again", report.Candidate)
		}
	case UnitApproved:
		m := &masons{s: s, cfg: s.About(repo), repository: repo}
		_, identity, err := m.CandidateEvidence(ctx, stream, unit)
		if err != nil {
			if ctx.Err() != nil {
				return unitMoveRecord{}, &APIError{Code: Internal, Message: "the move was cancelled"}
			}
			return unitMoveRecord{}, &APIError{Code: Conflict, Message: fmt.Sprintf("unit %s has no candidate to approve: %v", unit, err)}
		}
		move.Candidate = identity.Candidate.Revision
		review, err := approvalRecord(repo, stream, unit, identity, id, by, note, actor, at)
		if err != nil {
			return unitMoveRecord{}, &APIError{Code: Internal, Message: "cannot read the unit review"}
		}
		docs = append(docs, review)
		reason += fmt.Sprintf("; the foreman lands candidate %s", identity.Candidate.Revision)
	case UnitContested:
		reason += "; the owner rules on it"
	case UnitReady:
		reason += "; its mason starts it when its turn comes"
	case UnitPlanned:
		reason += "; it waits until every unit it depends on has merged"
	}
	reason += ": " + note
	data, _ := json.MarshalIndent(move, "", "  ")
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), Workstream: stream, Unit: unit, At: at, Actor: actor, Cause: cause}
	docs = append([]trace.Document{{Header: h, Path: movePath(unit, state.Version), Content: string(data) + "\n"}}, docs...)
	h.Schema, h.ID, h.Cause = "osmia.trace.transition", id+"-"+to, id
	if transition != "" {
		h.ID = transition
	}
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: trace.UnitSubject(unit), From: state.Value, To: to, Reason: reason},
		Events: []trace.Event{trace.Notice(h.ID, "unit", fmt.Sprintf("Unit %s: %s.", unit, reason))}}
	return unitMoveRecord{move: move, docs: docs, tx: tx}, nil
}

func approvalRecord(repo *trace.Repository, stream config.WorkstreamID, unit string, identity UnitReviewIdentity, move, by, note string, actor trace.Actor, at time.Time) (trace.Document, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return trace.Document{}, err
	}
	var latest trace.Document
	for _, d := range docs {
		if d.ID == ReviewDocument(unit) {
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
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: ReviewDocument(unit), Revision: latest.Revision + 1, Project: repo.Project(), Workstream: stream, Unit: unit, At: at, Actor: actor, Cause: move}
	return trace.Document{Header: h, Path: fmt.Sprintf("units/%s/review.json", unit), Content: string(content) + "\n"}, nil
}

// OwnerMove moves a unit as the owner.
func (s *Controller) OwnerMove(ctx context.Context, raw, unit string, req UnitMoveRequest) (UnitMoveResponse, *APIError) {
	_, stream, repo, api := s.ConversationTrace(raw)
	if api != nil {
		return UnitMoveResponse{}, api
	}
	if unit == "" {
		return UnitMoveResponse{}, &APIError{Code: Validation, Message: "a move requires a unit"}
	}
	if gone, err := abandoned(repo, stream); err != nil {
		return UnitMoveResponse{}, &APIError{Code: Internal, Message: "cannot read the workstream state"}
	} else if gone {
		return UnitMoveResponse{}, &APIError{Code: Conflict, Message: "an abandoned workstream's units do not move"}
	}
	return s.MoveUnit(ctx, repo, stream, unit, req, ownerActor, "")
}

// MoveUnit returns the move_unit tool of one claimed manager turn. The
// manager proposes a move of a unit of its workstream: the transition
// router commits it, keeps the unit where it is, or leaves the move as a pending
// owner decision, and the result says which. A contest the manager raised
// or ruled on itself keeps the unit there by default, and Jev may depart from
// that; a proposal to approve a candidate, and a move out of a contest the
// owner holds, are the owner's to decide and are left pending on the recovery
// card. In a turn answering the owner, it records the owner's own move. A move
// that is not made is an ordinary result, {"recorded":false,"reason":...},
// which says what became of it. A shared manager names the member whose
// unit moves.
func (c *Controls) MoveUnit(repository *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	tool := coreadapter.Tool{Name: MoveUnitTool, Effect: coreadapter.ToolMemory,
		Description: "Move a unit of this workstream to another state when the state machine leaves it stuck or wrong. The move is your decision: the service makes it unless a hard prerequisite refuses it (a landing or rebase in flight, a pause, the budget, a missing candidate), and the result says what happened, or that the move belongs to the owner and is pending on a card. " +
			"unit: its ID; to: ready (kick a ready unit, or make a planned unit ready once every unit it depends on has merged), planned (hold a unit that has not started until its dependencies merge), implementing (its mason revises the unit in its workspace, with a fresh clean-turn allowance), checking (the service runs the checks again on the recorded candidate), reviewing (its reviewer reviews the recorded candidate again), approved (a candidate approved without independent review is the owner's decision, so the move waits as a pending owner decision) or contested (hold the unit for the owner); " +
			"note: for the mason or reviewer, what to do differently, which it receives; for contested, in a few short sentences for the owner, what is stuck, the finding that matters and what you recommend they do. Moving a unit to the state it is in restarts that stage. " +
			"You may move a unit out of any contest except one the owner holds. Report to the owner what the result says happened. owner_decided: true only when the owner asked for the move in the message this turn answers, with the owner's words as the note. The unit's state and recent transitions are in units/<unit>/activity.json. workstream: a member workstream ID of your workgroup to act on; omit for this conversation's own workstream.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"unit":{"type":"string"},"to":{"type":"string","enum":["planned","ready","implementing","checking","reviewing","approved","contested"]},"note":{"type":"string"},"owner_decided":{"type":"boolean"},"workstream":{"type":"string"}},"required":["unit","to","note"],"additionalProperties":false}`)}
	tool.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input struct {
			Unit         string              `json:"unit"`
			To           string              `json:"to"`
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
			return nil, errors.New("unit moves are unavailable")
		}
		if err := repository.ActiveTurn(scope); err != nil {
			return nil, err
		}
		target, stream, refusal, err := repository.ManagerMember(config.WorkstreamID(scope.Workstream), input.Workstream)
		if err != nil {
			return nil, err
		}
		if refusal != "" {
			return priorityRefusal(refusal)
		}
		request := UnitMoveRequest{To: input.To, Note: input.Note}
		if gone, err := abandoned(target, stream); err != nil {
			return nil, err
		} else if gone {
			return priorityRefusal("an abandoned workstream's units do not move")
		}
		if input.OwnerDecided {
			turn, owner, err := repository.OwnerTurn(trace.Manager, scope)
			if err != nil {
				return nil, err
			}
			if !owner {
				return priorityRefusal("owner_decided is only for a move the owner asked for in the message this turn answers")
			}
			return moveResult(s.MoveUnit(ctx, target, stream, input.Unit, request, turn.Actor, scope.Turn))
		}
		return moveResult(s.MoveUnit(ctx, target, stream, input.Unit, request, managerActor, scope.Turn))
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
