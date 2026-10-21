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
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

const moveWorkstreamTool = "move_workstream"

// workstreamMoveSubject is the workflow subject that orders a workstream's
// moves: moved-<n> once move n is recorded.
const workstreamMoveSubject = "workstream-move"

// workstreamMovePrefix begins the trace record ID of every workstream move.
const workstreamMovePrefix = "workstream-move-"

// workstreamMoveTargets are the feature states a workstream moves to.
var workstreamMoveTargets = []string{SketchedState, InShedState, RatifiedState, BuildingState, AssembledState, DeliveredState, AbandonedState}

// chiefMoveTargets are the feature states the chief of staff moves a
// workstream to on its own judgment. Every other target stands for an owner
// decision, which it only relays.
var chiefMoveTargets = []string{BuildingState, AssembledState}

func workstreamMovePath(n int) string     { return fmt.Sprintf("moves/move-%d.json", n) }
func workstreamMoveDocument(n int) string { return fmt.Sprintf("%s%d", workstreamMovePrefix, n) }

// workstreamMoves returns the workstream's recorded moves in the order they
// were recorded.
func workstreamMoves(repo *trace.Repository, stream config.WorkstreamID) ([]WorkstreamMove, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return nil, err
	}
	var moves []WorkstreamMove
	for _, d := range docs {
		if !strings.HasPrefix(d.ID, workstreamMovePrefix) {
			continue
		}
		var m WorkstreamMove
		if err := json.Unmarshal([]byte(d.Content), &m); err != nil {
			return nil, fmt.Errorf("%s: %w", d.Path, err)
		}
		moves = append(moves, m)
	}
	return moves, nil
}

// reviewMoves returns the workstream's moves to building or assembled, each
// of which asks for a fresh final review once every unit has merged.
func reviewMoves(repo *trace.Repository, stream config.WorkstreamID) ([]WorkstreamMove, error) {
	moves, err := workstreamMoves(repo, stream)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(moves, func(m WorkstreamMove) bool { return m.To != BuildingState && m.To != AssembledState }), nil
}

// reopenHeld reports whether a move to building holds the workstream there:
// its latest move is to building and no amendment has been filed since. The
// foreman reassembles it once a revision filed after the move is decided and
// its units land, or once it is moved to assembled.
func reopenHeld(repo *trace.Repository, stream config.WorkstreamID) (bool, error) {
	records, err := trace.Read[trace.Record](repo, stream)
	if err != nil {
		return false, err
	}
	held := false
	for _, record := range records {
		switch r := record.(type) {
		case trace.Document:
			if !strings.HasPrefix(r.ID, workstreamMovePrefix) {
				continue
			}
			var m WorkstreamMove
			if err := json.Unmarshal([]byte(r.Content), &m); err != nil {
				return false, fmt.Errorf("%s: %w", r.Path, err)
			}
			held = m.To == BuildingState
		case trace.Amendment:
			held = false
		}
	}
	return held, nil
}

// moveWorkstream moves a workstream to another feature state, as actor: the
// owner, or the chief of staff on the owner's behalf in turn.
//
// A move to sketched asks the architect for a redraft with the note, to
// in-shed for one more round of debate, to ratified ratifies the latest
// ratification packet, and to abandoned abandons the workstream; each is the
// owner's own shed, ratification or abandonment action and records itself.
// A move to building reopens an assembled workstream, or a delivered one
// whose pull request is open, on its existing feature branch for repair
// units; to assembled asks for a fresh final review of the branch as it is;
// and to delivered returns a reopened workstream whose branch is still the
// one its open pull request carries to delivered. Those three record the
// move and its note together with the move of the feature state.
func (s *Service) moveWorkstream(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, req WorkstreamMoveRequest, actor trace.Actor, turn string) (WorkstreamMoveResponse, *APIError) {
	note := strings.TrimSpace(req.Note)
	if !slices.Contains(workstreamMoveTargets, req.To) || note == "" {
		return WorkstreamMoveResponse{}, &APIError{Code: Validation, Message: "a workstream move requires a note and a state: " + strings.Join(workstreamMoveTargets, ", ")}
	}
	if session, err := isSession(repo, stream); err != nil {
		return WorkstreamMoveResponse{}, &APIError{Code: Internal, Message: "cannot read the workstream kind"}
	} else if session {
		return WorkstreamMoveResponse{}, &APIError{Code: Conflict, Message: "a session has no feature states to move"}
	}
	if held, err := changeHeld(repo, stream); err != nil {
		return WorkstreamMoveResponse{}, &APIError{Code: Internal, Message: "cannot read the workgroup change"}
	} else if held {
		return WorkstreamMoveResponse{}, &APIError{Code: Conflict, Message: "a workgroup change holds the workstream; it moves once the change is applied"}
	}
	feature, err := repo.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return WorkstreamMoveResponse{}, &APIError{Code: Internal, Message: "cannot read the workstream state"}
	}
	respond := func(detail string) (WorkstreamMoveResponse, *APIError) {
		after, err := repo.Workflow(stream, trace.FeatureSubject)
		if err != nil {
			return WorkstreamMoveResponse{}, &APIError{Code: Internal, Message: "cannot read the workstream state"}
		}
		return WorkstreamMoveResponse{Workstream: stream, State: after.Value, Detail: detail}, nil
	}
	raw := string(stream)
	switch req.To {
	case SketchedState:
		out, api := s.shedRedraft(ctx, raw, ShedRedraftRequest{Note: note})
		if api != nil {
			return WorkstreamMoveResponse{}, api
		}
		return respond(out.Detail)
	case InShedState:
		out, api := s.shedMore(ctx, raw, ShedMoreRequest{Rounds: 1})
		if api != nil {
			return WorkstreamMoveResponse{}, api
		}
		return respond(out.Detail)
	case RatifiedState:
		packet, _, found, err := shed.LatestPacket(repo, stream)
		if err != nil {
			return WorkstreamMoveResponse{}, &APIError{Code: Internal, Message: "cannot read the ratification packet"}
		}
		if !found {
			return WorkstreamMoveResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s has no ratification packet to ratify", stream)}
		}
		out, api := s.ratify(ctx, raw, RatifyRequest{Spec: packet.Revision.Spec, Plan: packet.Revision.Plan})
		if api != nil {
			return WorkstreamMoveResponse{}, api
		}
		return respond(out.Detail)
	case AbandonedState:
		if _, api := s.abandon(ctx, raw, AbandonRequest{Reason: note}); api != nil {
			return WorkstreamMoveResponse{}, api
		}
		return respond("the owner abandoned the workstream: " + note)
	}
	if action, err := publicationInFlight(repo, stream); err != nil {
		return WorkstreamMoveResponse{}, &APIError{Code: Internal, Message: "cannot read the workstream's operations"}
	} else if action != "" {
		return WorkstreamMoveResponse{}, &APIError{Code: Conflict, Message: "the workstream's publication is in flight; it moves once the publication has an outcome"}
	}
	by := "the " + rulerName(actor)
	var reason string
	switch req.To {
	case BuildingState:
		switch feature.Value {
		case AssembledState:
			reason = fmt.Sprintf("%s moved the workstream from assembled to building on %s", by, featureBranch(stream))
		case DeliveredState:
			pr, api := openPullRequest(repo, stream)
			if api != nil {
				return WorkstreamMoveResponse{}, api
			}
			reason = fmt.Sprintf("%s reopened the delivered workstream on %s, where pull request #%d stays open; it is updated only after a fresh final review and the owner's delivery approval", by, featureBranch(stream), pr.PullRequest)
		case BuildingState:
			return WorkstreamMoveResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s is already building", stream)}
		default:
			return WorkstreamMoveResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("the workstream is %s; it moves to building once it is assembled or delivered with an open pull request", featureState(feature.Value))}
		}
		reason += "; it stays building until a revision filed after this move is decided and its units land, or until it is moved to assembled, and its final review then runs again"
	case AssembledState:
		switch feature.Value {
		case AssembledState:
			reason = fmt.Sprintf("%s asked for a fresh final review of the assembled branch", by)
		case DeliveredState:
			pr, api := openPullRequest(repo, stream)
			if api != nil {
				return WorkstreamMoveResponse{}, api
			}
			reason = fmt.Sprintf("%s returned the delivered workstream to assembled for a fresh final review; pull request #%d stays open and is updated only after the owner's delivery approval", by, pr.PullRequest)
		case BuildingState:
			if api := s.assemblable(repo, stream); api != nil {
				return WorkstreamMoveResponse{}, api
			}
			reason = fmt.Sprintf("%s moved the workstream from building to assembled for a fresh final review", by)
		default:
			return WorkstreamMoveResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("the workstream is %s; it moves to assembled once it is building, assembled or delivered with an open pull request", featureState(feature.Value))}
		}
	case DeliveredState:
		if feature.Value != BuildingState && feature.Value != AssembledState {
			return WorkstreamMoveResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("the workstream is %s; only a reopened workstream moves back to delivered", featureState(feature.Value))}
		}
		pr, api := openPullRequest(repo, stream)
		if api != nil {
			return WorkstreamMoveResponse{}, api
		}
		if api := s.assemblable(repo, stream); api != nil {
			return WorkstreamMoveResponse{}, api
		}
		tip, api := s.featureTip(ctx, repo, stream)
		if api != nil {
			return WorkstreamMoveResponse{}, api
		}
		if tip != pr.Reviewed && tip != pr.Commit {
			return WorkstreamMoveResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("feature branch %s moved to %s since pull request #%d was published; it is delivered through a fresh final review and the owner's delivery approval", featureBranch(stream), tip, pr.PullRequest)}
		}
		reason = fmt.Sprintf("%s returned the reopened workstream to delivered; feature branch %s is still what pull request #%d carries", by, featureBranch(stream), pr.PullRequest)
	}
	reason += ": " + note
	moves, err := workstreamMoves(repo, stream)
	if err != nil {
		return WorkstreamMoveResponse{}, &APIError{Code: Internal, Message: "cannot read the workstream's moves"}
	}
	subject, err := repo.Workflow(stream, workstreamMoveSubject)
	if err != nil {
		return WorkstreamMoveResponse{}, &APIError{Code: Internal, Message: "cannot read the workstream's moves"}
	}
	n := len(moves) + 1
	move := WorkstreamMove{Move: n, From: feature.Value, To: req.To, Note: note, By: rulerName(actor), Turn: turn}
	data, err := json.MarshalIndent(move, "", "  ")
	if err != nil {
		return WorkstreamMoveResponse{}, &APIError{Code: Internal, Message: err.Error()}
	}
	cause := turn
	if cause == "" {
		cause = trace.FeatureSubject + "-" + feature.Value
	}
	id := workstreamMoveDocument(n)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), Workstream: stream, At: s.now(), Actor: actor, Cause: cause}
	doc := trace.Document{Header: h, Path: workstreamMovePath(n), Content: string(data) + "\n"}
	h.Schema, h.ID, h.Cause = "osmia.trace.transition", id+"-recorded", id
	txs := []trace.Transaction{{ExpectedVersion: subject.Version,
		Transition: trace.Transition{Header: h, Subject: workstreamMoveSubject, From: subject.Value, To: fmt.Sprintf("moved-%d", n), Reason: reason},
		Events:     []trace.Event{trace.Notice(h.ID, "workstream", fmt.Sprintf("Workstream %s: %s.", stream, strings.TrimSuffix(reason, ".")))}}}
	if feature.Value != req.To {
		fh := h
		fh.ID = id + "-" + req.To
		txs = append(txs, trace.Transaction{ExpectedVersion: feature.Version, Transition: trace.Transition{Header: fh, Subject: trace.FeatureSubject, From: feature.Value, To: req.To, Reason: reason}})
	}
	if _, err := repo.RecordDocumentsWith(ctx, []trace.Document{doc}, txs...); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			return WorkstreamMoveResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s changed since it was read; read it again", stream)}
		}
		return WorkstreamMoveResponse{}, &APIError{Code: Internal, Message: "cannot record the move"}
	}
	return WorkstreamMoveResponse{Workstream: stream, State: req.To, Detail: reason, Move: &move}, nil
}

// publicationInFlight returns the action of a publication of the workstream
// that has no result yet, or "" when none is in flight.
func publicationInFlight(repo *trace.Repository, stream config.WorkstreamID) (string, error) {
	records, err := repo.Operations(stream)
	if err != nil {
		return "", err
	}
	for _, r := range records {
		if r.Result == nil && (r.Operation.Action == PublishAction || r.Operation.Action == publishUpstreamAction) {
			return r.Operation.Action, nil
		}
	}
	return "", nil
}

// openPullRequest returns the workstream's current publication when its pull
// request is open: recorded, neither merged nor closed, and the workstream
// not delivered by an owner-recorded merge.
func openPullRequest(repo *trace.Repository, stream config.WorkstreamID) (DeliveryPublication, *APIError) {
	if _, merged, err := deliveryMerge(repo, stream); err != nil {
		return DeliveryPublication{}, &APIError{Code: Internal, Message: "cannot read the workstream's delivery"}
	} else if merged {
		return DeliveryPublication{}, &APIError{Code: Conflict, Message: "the owner recorded the workstream's branch merged upstream; it does not reopen"}
	}
	pr, found, err := currentOwnPublication(repo, stream)
	if err != nil {
		return DeliveryPublication{}, &APIError{Code: Internal, Message: "cannot read the workstream's publications"}
	}
	if !found {
		return DeliveryPublication{}, &APIError{Code: Conflict, Message: "the workstream has no pull request of its own; it is delivered through the owner's delivery approval"}
	}
	if finished, err := ownPullRequestFinished(repo, stream); err != nil {
		return DeliveryPublication{}, &APIError{Code: Internal, Message: "cannot read the pull request outcome"}
	} else if finished {
		return DeliveryPublication{}, &APIError{Code: Conflict, Message: fmt.Sprintf("pull request #%d is merged or closed; the workstream does not reopen", pr.PullRequest)}
	}
	return pr, nil
}

// assemblable refuses a workstream with an unmerged unit of its sealed plan,
// an open discovery or a pending revision.
func (s *Service) assemblable(repo *trace.Repository, stream config.WorkstreamID) *APIError {
	b, found, err := (&masons{s: s, cfg: s.about(repo), repository: repo}).readSealed(stream)
	if err != nil {
		return &APIError{Code: Internal, Message: "cannot read the sealed plan"}
	}
	if !found || !allMerged(b) {
		var open []string
		if found {
			for _, u := range b.plan.Units {
				if b.states[trace.UnitSubject(u.ID)].Value != UnitMerged {
					open = append(open, u.ID)
				}
			}
		}
		return &APIError{Code: Conflict, Message: fmt.Sprintf("units %s have not merged; move them, or wait for them to land", strings.Join(open, ", "))}
	}
	if pending, err := unresolvedWork(repo, stream); err != nil {
		return &APIError{Code: Internal, Message: "cannot read the workstream's discoveries and revisions"}
	} else if pending {
		return &APIError{Code: Conflict, Message: "an open discovery or a pending revision prevents assembly"}
	}
	return nil
}

// featureTip returns the commit the workstream's feature branch is at.
func (s *Service) featureTip(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID) (string, *APIError) {
	g, err := featureWorkspaces(s.about(repo), repo).of(stream)
	if err != nil {
		return "", &APIError{Code: Internal, Message: "cannot open the workstream's feature branch"}
	}
	tip, exists, err := g.Branch(ctx, featureBranch(stream))
	if err != nil {
		return "", &APIError{Code: Internal, Message: "cannot read the workstream's feature branch"}
	}
	if !exists {
		return "", &APIError{Code: Conflict, Message: fmt.Sprintf("the clone has no feature branch %s", featureBranch(stream))}
	}
	return tip, nil
}

// ownerMoveWorkstream moves a workstream as the owner.
func (s *Service) ownerMoveWorkstream(ctx context.Context, raw string, req WorkstreamMoveRequest) (WorkstreamMoveResponse, *APIError) {
	_, stream, repo, api := s.conversationTrace(raw)
	if api != nil {
		return WorkstreamMoveResponse{}, api
	}
	if gone, err := abandoned(repo, stream); err != nil {
		return WorkstreamMoveResponse{}, &APIError{Code: Internal, Message: "cannot read the workstream state"}
	} else if gone {
		return WorkstreamMoveResponse{}, &APIError{Code: Conflict, Message: "an abandoned workstream does not move"}
	}
	return s.moveWorkstream(ctx, repo, stream, req, ownerActor, "")
}

// moveWorkstream returns the move_workstream tool of one claimed
// chief-of-staff turn. The chief of staff moves its workstream to building or
// assembled on its own judgment. Every other target stands for an owner
// decision: in a turn answering the owner, it records the owner's own move. A
// move the service refuses is an ordinary result,
// {"recorded":false,"reason":...}, and records nothing.
func (c *runtimeControls) moveWorkstream(repository *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	tool := coreadapter.Tool{Name: moveWorkstreamTool, Effect: coreadapter.ToolMemory,
		Description: "Move this workstream to another feature state when the state machine leaves it stuck or wrong. to: building (reopen an assembled workstream, or a delivered one whose pull request is open, on its existing feature branch; it stays building until a revision you file afterwards with route_amendment is decided and its units land, so file the revision that repairs it next), " +
			"assembled (run a fresh final review of the branch as it is: after a failed or stuck final review, after a drift rebase, or to return a delivered workstream with an open pull request to final review), " +
			"or, only to record an owner decision, sketched (the architect redrafts with the note), in-shed (one more round of debate), ratified (ratify the latest packet), delivered (return a reopened workstream whose branch is unchanged since its pull request was published) or abandoned. " +
			"A delivered workstream's open pull request is updated only after a fresh final review and the owner's delivery approval. note: what is stuck or wrong and what should happen next; the final reviewer receives it. owner_decided: true only when the owner asked for the move in the message this turn answers, with the owner's words as the note.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"to":{"type":"string","enum":["sketched","in-shed","ratified","building","assembled","delivered","abandoned"]},"note":{"type":"string"},"owner_decided":{"type":"boolean"}},"required":["to","note"],"additionalProperties":false}`)}
	tool.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input struct {
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
			return nil, errors.New("workstream moves are unavailable")
		}
		if err := repository.ActiveTurn(scope); err != nil {
			return nil, err
		}
		stream := config.WorkstreamID(scope.Workstream)
		request := WorkstreamMoveRequest{To: input.To, Note: input.Note}
		if gone, err := abandoned(repository, stream); err != nil {
			return nil, err
		} else if gone {
			return priorityRefusal("an abandoned workstream does not move")
		}
		if input.OwnerDecided {
			turn, owner, err := repository.OwnerTurn(trace.ChiefOfStaff, scope)
			if err != nil {
				return nil, err
			}
			if !owner {
				return priorityRefusal("owner_decided is only for a move the owner asked for in the message this turn answers")
			}
			return workstreamMoveResult(s.moveWorkstream(ctx, repository, stream, request, turn.Actor, scope.Turn))
		}
		if !slices.Contains(chiefMoveTargets, input.To) {
			return priorityRefusal(fmt.Sprintf("a move to %s is the owner's decision; present it to the owner, and record it with owner_decided once the owner asks for it", input.To))
		}
		if input.To == BuildingState {
			rounds, err := finalRoundsWithBlocking(repository, stream)
			if err != nil {
				return nil, err
			}
			if rounds >= maxFinalRounds {
				return priorityRefusal(fmt.Sprintf("final reviews have found blocking problems %d times; reopening the workstream for further revisions is the owner's decision", rounds))
			}
		}
		return workstreamMoveResult(s.moveWorkstream(ctx, repository, stream, request, chiefActor, scope.Turn))
	}
	return tool
}

func workstreamMoveResult(out WorkstreamMoveResponse, api *APIError) (json.RawMessage, error) {
	if api != nil {
		if api.Code == Internal {
			return nil, errors.New(api.Message)
		}
		return priorityRefusal(api.Message)
	}
	return json.Marshal(struct {
		Recorded bool   `json:"recorded"`
		State    string `json:"state"`
		Detail   string `json:"detail"`
	}{true, out.State, out.Detail})
}
