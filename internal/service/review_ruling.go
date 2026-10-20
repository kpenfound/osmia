package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// ContestedRuling records the direction for a contested unit, given by the
// owner or by the chief of staff on the owner's behalf.
type ContestedRuling struct {
	Decision string `json:"decision"`
	Note     string `json:"note"`
	// By names who ruled: "owner", or "chief of staff". Empty is the owner.
	By        string `json:"by,omitempty"`
	Bounces   int    `json:"bounces"`
	Candidate string `json:"candidate"`
	Contest   string `json:"contest,omitempty"`
	ResetTurn uint64 `json:"reset_turn,omitempty"`
	// turn and prompt are the mason turn a move to implementing queues; a
	// ruling's turn and prompt follow from the ruling.
	turn, prompt string
}

type ContestedRulingRequest struct {
	Decision string `json:"decision"`
	Note     string `json:"note"`
}

type ContestedRulingResponse struct {
	Workstream config.WorkstreamID `json:"workstream"`
	Unit       string              `json:"unit"`
	Ruling     ContestedRuling     `json:"ruling"`
}

func contestedRulingPath(unit string, bounces int) string {
	return fmt.Sprintf("units/%s/ruling-%d.json", trace.UnitSubject(unit), bounces)
}

func masonRulingPath(unit string, sequence uint64) string {
	return fmt.Sprintf("units/%s/mason-ruling-%d.json", trace.UnitSubject(unit), sequence)
}

func masonContest(repo *trace.Repository, stream config.WorkstreamID, unit string) (trace.Transition, bool, error) {
	transitions, err := trace.Read[trace.Transition](repo, stream)
	if err != nil {
		return trace.Transition{}, false, err
	}
	for i := len(transitions) - 1; i >= 0; i-- {
		t := transitions[i]
		if t.Subject == trace.UnitSubject(unit) && t.To == UnitContested {
			return t, t.From == UnitImplementing && t.Actor == masonActor, nil
		}
	}
	return trace.Transition{}, false, nil
}

// latestMasonRuling returns the latest direction that resumes the unit's
// mason with a fresh clean-turn allowance: a revise ruling on a mason contest,
// or a move to implementing. Of two that resume after the same turn, the
// later one holds.
func latestMasonRuling(repo *trace.Repository, stream config.WorkstreamID, unit string) (ContestedRuling, bool, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return ContestedRuling{}, false, err
	}
	var latest ContestedRuling
	found := false
	for _, d := range docs {
		if d.Unit != unit {
			continue
		}
		var ruling ContestedRuling
		switch {
		case strings.HasPrefix(d.Path, "units/"+trace.UnitSubject(unit)+"/mason-ruling-"):
			if err := json.Unmarshal([]byte(d.Content), &ruling); err != nil {
				return ContestedRuling{}, false, err
			}
		case strings.HasPrefix(d.Path, movePrefix(unit)):
			var move UnitMove
			if err := json.Unmarshal([]byte(d.Content), &move); err != nil {
				return ContestedRuling{}, false, err
			}
			if move.To != UnitImplementing {
				continue
			}
			ruling = ContestedRuling{Decision: "revise", Note: move.Note, By: move.By, Contest: d.ID, ResetTurn: move.ResetTurn, turn: masonAgent(unit) + "-" + strings.TrimPrefix(d.ID, trace.UnitSubject(unit)+"-"),
				prompt: fmt.Sprintf("The %s moved this unit from %s back to implementing, for you to revise in your existing workspace. Note: %s\nCheck the unit's acceptance, then call done or ask if you need a decision.", move.By, move.From, move.Note)}
		default:
			continue
		}
		if !found || ruling.ResetTurn >= latest.ResetTurn {
			latest, found = ruling, true
		}
	}
	return latest, found, nil
}

func latestContestedRuling(repo *trace.Repository, stream config.WorkstreamID, unit string, bounces int) (ContestedRuling, bool, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return ContestedRuling{}, false, err
	}
	path := contestedRulingPath(unit, bounces)
	for _, d := range docs {
		if d.Path == path {
			var ruling ContestedRuling
			if err := json.Unmarshal([]byte(d.Content), &ruling); err != nil {
				return ContestedRuling{}, false, err
			}
			return ruling, true, nil
		}
	}
	return ContestedRuling{}, false, nil
}

func (s *Service) ruleContested(ctx context.Context, rawStream, unit string, req ContestedRulingRequest) (ContestedRulingResponse, *APIError) {
	stream, err := config.ParseWorkstreamID(rawStream)
	if err != nil || unit == "" {
		return ContestedRulingResponse{}, &APIError{Validation, "a workstream ID and unit are required"}
	}
	if req.Decision != "review" && req.Decision != "revise" || strings.TrimSpace(req.Note) == "" {
		return ContestedRulingResponse{}, &APIError{Validation, "a contested ruling requires review or revise and a note"}
	}
	_, projects := s.runtimes()
	if len(projects) == 0 {
		return ContestedRulingResponse{}, &APIError{NoProject, "no active project"}
	}
	repo := projects[0].repository
	for _, p := range projects[1:] {
		streams, err := p.repository.Workstreams()
		if err != nil {
			return ContestedRulingResponse{}, &APIError{Internal, "cannot read the workstream state"}
		}
		if slices.Contains(streams, stream) {
			repo = p.repository
		}
	}
	return s.recordContestedRuling(ctx, repo, stream, unit, req, ownerActor)
}

// rulerName is how a contested ruling's actor is named to the roles it
// resumes.
func rulerName(actor trace.Actor) string {
	if actor.Kind == ownerActor.Kind {
		return "owner"
	}
	return "chief of staff"
}

// ruler returns who gave a recorded ruling.
func (r ContestedRuling) ruler() string {
	if r.By == "" {
		return "owner"
	}
	return r.By
}

// actor returns the trace actor of whoever gave the ruling.
func (r ContestedRuling) actor() trace.Actor {
	if r.ruler() == "owner" {
		return ownerActor
	}
	return chiefActor
}

// recordContestedRuling rules on a contested unit as actor: the owner, or the
// chief of staff on the owner's behalf. The rulings each contest takes, and
// what they resume, do not depend on who gives them.
func (s *Service) recordContestedRuling(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, unit string, req ContestedRulingRequest, actor trace.Actor, decisions ...trace.Document) (ContestedRulingResponse, *APIError) {
	if req.Decision != "review" && req.Decision != "revise" || strings.TrimSpace(req.Note) == "" {
		return ContestedRulingResponse{}, &APIError{Validation, "a contested ruling requires review or revise and a note"}
	}
	by := rulerName(actor)
	if gone, err := abandoned(repo, stream); err != nil {
		return ContestedRulingResponse{}, &APIError{Internal, "cannot read the workstream state"}
	} else if gone {
		return ContestedRulingResponse{}, &APIError{Conflict, "abandoned workstream takes no contested ruling"}
	}
	state, err := repo.Workflow(stream, trace.UnitSubject(unit))
	if err != nil {
		return ContestedRulingResponse{}, &APIError{Internal, "cannot read the contested unit"}
	}
	if state.Value != UnitContested {
		return ContestedRulingResponse{}, &APIError{Conflict, "unit is not contested"}
	}
	contest, mason, err := masonContest(repo, stream, unit)
	if err != nil {
		return ContestedRulingResponse{}, &APIError{Internal, "cannot read contested transition"}
	}
	if moveContest(contest, unit) || checksContest(contest, unit) || contest.Cause == questionWaitContestCause {
		// A move, incomplete checks or an uncertain question wait leaves no
		// findings to rule on. A ruling moves the unit on, and a move to
		// reviewing runs its checks before review.
		to := map[string]string{"review": UnitReviewing, "revise": UnitImplementing}[req.Decision]
		out, api := s.moveUnit(ctx, repo, stream, unit, UnitMoveRequest{To: to, Note: req.Note}, actor, "")
		if api != nil {
			return ContestedRulingResponse{}, api
		}
		return ContestedRulingResponse{Workstream: stream, Unit: unit, Ruling: ContestedRuling{Decision: req.Decision, Note: out.Move.Note, By: by, Contest: contest.ID}}, nil
	}
	if failedReview(contest, unit) {
		// A failed review turn left no findings, so the unit can only be reviewed again.
		if req.Decision != "review" {
			return ContestedRulingResponse{}, &APIError{Validation, "the reviewer's turn failed and left no findings; use review"}
		}
		ruling := ContestedRuling{Decision: "review", Note: strings.TrimSpace(req.Note), By: by, Contest: contest.ID}
		id := trace.EventID(contest.ID, "ruling")
		reason := fmt.Sprintf("%s ruled review on contest %s, raised when the reviewer's turn failed; a new review turn runs: %s", by, contest.ID, ruling.Note)
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), Workstream: stream, Unit: unit, At: s.now(), Actor: actor, Cause: contest.ID}
		tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: trace.UnitSubject(unit), From: UnitContested, To: UnitReviewing, Reason: reason}, Events: []trace.Event{trace.Notice(id, "unit", reason)}}
		if _, err := repo.Transact(ctx, tx); err != nil {
			if errors.Is(err, trace.ErrConflict) {
				return ContestedRulingResponse{}, &APIError{Conflict, "unit already has a ruling"}
			}
			return ContestedRulingResponse{}, &APIError{Internal, "cannot record contested ruling"}
		}
		return ContestedRulingResponse{Workstream: stream, Unit: unit, Ruling: ruling}, nil
	}
	if mason {
		if req.Decision == "review" {
			return ContestedRulingResponse{}, &APIError{Validation, "no candidate under review; use revise"}
		}
		th, err := repo.Thread(stream, masonAgent(unit))
		if err != nil || len(th.Turns) == 0 {
			return ContestedRulingResponse{}, &APIError{Internal, "contested mason thread is missing"}
		}
		last := th.Turns[len(th.Turns)-1]
		if last.Response == nil || last.Response.ID != contest.Cause {
			return ContestedRulingResponse{}, &APIError{Internal, "contested mason turn does not match transition"}
		}
		ruling := ContestedRuling{Decision: "revise", Note: strings.TrimSpace(req.Note), By: by, Contest: contest.ID, ResetTurn: last.Sequence}
		data, _ := json.MarshalIndent(ruling, "", "  ")
		id := fmt.Sprintf("%s-mason-ruling-%d", trace.UnitSubject(unit), last.Sequence)
		doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), Workstream: stream, Unit: unit, At: s.now(), Actor: actor, Cause: contest.ID}, Path: masonRulingPath(unit, last.Sequence), Content: string(data) + "\n"}
		reason := fmt.Sprintf("%s ruled revise on mason contest %s; clean-turn attempts reset to zero: %s", by, contest.ID, ruling.Note)
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id + "-implementing", Revision: 1, Project: repo.Project(), Workstream: stream, Unit: unit, At: s.now(), Actor: actor, Cause: id}
		tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: trace.UnitSubject(unit), From: UnitContested, To: UnitImplementing, Reason: reason}, Events: []trace.Event{trace.Notice(h.ID, "unit", reason)}}
		if _, err := repo.RecordDocumentsWith(ctx, []trace.Document{doc}, tx); err != nil {
			if errors.Is(err, trace.ErrConflict) {
				return ContestedRulingResponse{}, &APIError{Conflict, "unit already has a ruling"}
			}
			return ContestedRulingResponse{}, &APIError{Internal, "cannot record contested ruling"}
		}
		return ContestedRulingResponse{Workstream: stream, Unit: unit, Ruling: ruling}, nil
	}
	r := &reviewers{masons: &masons{s: s, cfg: s.about(repo), repository: repo}}
	result, ok, err := r.storedResult(stream, unit, state)
	if err != nil || !ok || result.Verdict.Decision != "material_findings" {
		return ContestedRulingResponse{}, &APIError{Internal, "contested unit has no material review result"}
	}
	ruling, found, err := latestContestedRuling(repo, stream, unit, result.Bounces)
	if err != nil {
		return ContestedRulingResponse{}, &APIError{Internal, "cannot read contested ruling"}
	}
	docs := slices.Clone(decisions)
	if found {
		// A saved ruling is authoritative. A chief retry records the
		// decision being applied, including its original direction and note.
		for i := range docs {
			var decision ChiefContestDecision
			if err := json.Unmarshal([]byte(docs[i].Content), &decision); err != nil {
				return ContestedRulingResponse{}, &APIError{Internal, "cannot read chief decision"}
			}
			decision.Decision, decision.Note = ruling.Decision, ruling.Note
			data, _ := json.MarshalIndent(decision, "", "  ")
			docs[i].Content = string(data) + "\n"
		}
	}
	if !found {
		ruling = ContestedRuling{Decision: req.Decision, Note: strings.TrimSpace(req.Note), By: by, Bounces: result.Bounces, Candidate: result.Identity.Candidate.Revision}
		data, _ := json.MarshalIndent(ruling, "", "  ")
		id := fmt.Sprintf("%s-ruling-%d", trace.UnitSubject(unit), result.Bounces)
		docs = append(docs, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), Workstream: stream, Unit: unit, At: s.now(), Actor: actor, Cause: reviewDocument(unit)}, Path: contestedRulingPath(unit, result.Bounces), Content: string(data) + "\n"})
	}
	if err := r.applyBounceRuling(ctx, stream, unit, state, result, ruling, docs); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			return ContestedRulingResponse{}, &APIError{Conflict, "unit already has a ruling"}
		}
		return ContestedRulingResponse{}, &APIError{Internal, "cannot record contested ruling"}
	}
	return ContestedRulingResponse{Workstream: stream, Unit: unit, Ruling: ruling}, nil
}

func (r *reviewers) resumeContested(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, contests ...trace.Transition) error {
	// Only a contest raised by send-backs is resumed by a bounce ruling.
	var contest trace.Transition
	if len(contests) > 0 {
		contest = contests[0]
	} else {
		var err error
		contest, _, err = masonContest(r.repository, stream, unit)
		if err != nil {
			return err
		}
	}
	if !bounceContest(contest, unit) {
		return nil
	}
	result, ok, err := r.storedResult(stream, unit, state)
	if err != nil || !ok {
		return err
	}
	ruling, found, err := latestContestedRuling(r.repository, stream, unit, result.Bounces)
	if err != nil || !found {
		return err
	}
	// A ruling for different evidence cannot resume this contest. Keep it in
	// the trace without stopping recovery of the stream's other units.
	if ruling.Candidate != result.Identity.Candidate.Revision || ruling.Bounces != result.Bounces {
		return nil
	}
	return r.applyBounceRuling(ctx, stream, unit, state, result, ruling, nil)
}

// applyBounceRuling commits the ruling and its move together. The result and
// transition retain everything needed to queue findings after interruption.
func (r *reviewers) applyBounceRuling(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, result UnitReviewResult, ruling ContestedRuling, docs []trace.Document) error {
	if ruling.Candidate != result.Identity.Candidate.Revision || ruling.Bounces != result.Bounces {
		return errors.New("contested ruling does not match candidate and bounce count")
	}
	to := UnitReviewing
	if ruling.Decision == "revise" {
		to = UnitImplementing
	} else if ruling.Decision != "review" {
		return errors.New("invalid contested ruling")
	}
	id := fmt.Sprintf("%s-%s-ruling-%d", trace.UnitSubject(unit), to, result.Bounces)
	reason := fmt.Sprintf("%s ruled %s on contested candidate %s after %d send-backs: %s", ruling.ruler(), ruling.Decision, ruling.Candidate, ruling.Bounces, ruling.Note)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: r.repository.Project(), Workstream: stream, Unit: unit, At: r.s.now(), Actor: ruling.actor(), Cause: fmt.Sprintf("%s-ruling-%d", trace.UnitSubject(unit), result.Bounces)}
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: trace.UnitSubject(unit), From: UnitContested, To: to, Reason: reason}, Events: []trace.Event{trace.Notice(id, "unit", reason)}}
	if err := r.s.step("bounce-ruling-recording:" + string(stream) + ":" + unit); err != nil {
		return err
	}
	var err error
	if len(docs) == 0 {
		_, err = r.repository.Transact(ctx, tx)
	} else {
		_, err = r.repository.RecordDocumentsWith(ctx, docs, tx)
	}
	if err != nil {
		// Publication can return an error after committing, and a controller
		// may have read contested before another ruler committed the move.
		// Reconcile its identity and decision before reporting failure.
		transitions, readErr := trace.Read[trace.Transition](r.repository, stream)
		if readErr != nil || !slices.ContainsFunc(transitions, func(t trace.Transition) bool {
			// The controller's attempt can have a later timestamp than the
			// committed move it is reconciling.
			t.At = tx.Transition.At
			return t == tx.Transition
		}) {
			return err
		}
		stored, readErr := trace.Read[trace.Document](r.repository, stream)
		if readErr != nil || slices.ContainsFunc(docs, func(d trace.Document) bool {
			return !slices.ContainsFunc(stored, func(s trace.Document) bool {
				at := s.At
				s.At = d.At
				return at.Equal(s.At) && s == d
			})
		}) {
			return err
		}
	}
	// The commit is authoritative even when interruption prevents the next
	// controller pass. Its findings are reconciled from the recorded result.
	_ = r.s.step("bounce-ruling-committed:" + string(stream) + ":" + unit)
	return nil
}

// bounceRevisionTransition identifies the move caused by a revise ruling.
func bounceRevisionTransition(t trace.Transition) bool {
	return t.From == UnitContested && t.To == UnitImplementing && strings.HasPrefix(t.Cause, t.Subject+"-ruling-")
}

// resumeBounceRevision closes the interval between a ruling's move and its
// findings turn. Both controllers call it: a ruling may arrive after the
// review pass but before the mason pass. The old done turn must not be used
// to send the candidate to checking again before the revision is queued.
func (m *masons) resumeBounceRevision(ctx context.Context, stream config.WorkstreamID, unit string, t trace.Transition) error {
	// The pass already read transitions for its building snapshot. Units
	// without a bounce ruling need no additional trace read.
	if !bounceRevisionTransition(t) {
		return nil
	}
	r := &reviewers{masons: m}
	result, ok, err := r.storedResult(stream, unit, trace.WorkflowState{})
	if err != nil || !ok {
		return err
	}
	if t.Cause != fmt.Sprintf("%s-ruling-%d", trace.UnitSubject(unit), result.Bounces) {
		return nil
	}
	return r.enqueueFindings(ctx, stream, unit, result)
}
