package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/trace"
)

func ContestedRulingPath(unit string, bounces int) string {
	return fmt.Sprintf("units/%s/ruling-%d.json", trace.UnitSubject(unit), bounces)
}

func masonRulingPath(unit string, sequence uint64) string {
	return fmt.Sprintf("units/%s/mason-ruling-%d.json", trace.UnitSubject(unit), sequence)
}

func MasonContest(repo *trace.Repository, stream config.WorkstreamID, unit string) (trace.Transition, bool, error) {
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

type masonRuling struct {
	ContestedRuling
	turn, prompt string
}

func LatestMasonRuling(repo *trace.Repository, stream config.WorkstreamID, unit string) (masonRuling, bool, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return masonRuling{}, false, err
	}
	var latest masonRuling
	found := false
	for _, d := range docs {
		if d.Unit != unit {
			continue
		}
		var ruling masonRuling
		switch {
		case strings.HasPrefix(d.Path, "units/"+trace.UnitSubject(unit)+"/mason-ruling-"):
			if err := json.Unmarshal([]byte(d.Content), &ruling); err != nil {
				return masonRuling{}, false, err
			}
		case strings.HasPrefix(d.Path, MovePrefix(unit)):
			var move UnitMove
			if err := json.Unmarshal([]byte(d.Content), &move); err != nil {
				return masonRuling{}, false, err
			}
			if move.To != UnitImplementing {
				continue
			}
			ruling = masonRuling{ContestedRuling: ContestedRuling{Decision: "revise", Note: move.Note, By: move.By, Contest: d.ID, ResetTurn: move.ResetTurn}, turn: masonAgent(unit) + "-" + strings.TrimPrefix(d.ID, trace.UnitSubject(unit)+"-"),
				prompt: fmt.Sprintf("The %s moved this unit from %s back to implementing, for you to revise in your existing workspace. Note: %s\nCheck the unit's acceptance, then call done or ask if you need a decision.", trace.DisplayRuler(move.By), move.From, move.Note)}
		default:
			continue
		}
		if !found || ruling.ResetTurn >= latest.ResetTurn {
			latest, found = ruling, true
		}
	}
	return latest, found, nil
}

func LatestContestedRuling(repo *trace.Repository, stream config.WorkstreamID, unit string, bounces int) (ContestedRuling, bool, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return ContestedRuling{}, false, err
	}
	path := ContestedRulingPath(unit, bounces)
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

func (s *Controller) RuleContested(ctx context.Context, rawStream, unit string, req ContestedRulingRequest) (ContestedRulingResponse, *APIError) {
	stream, err := config.ParseWorkstreamID(rawStream)
	if err != nil || unit == "" {
		return ContestedRulingResponse{}, &APIError{Code: Validation, Message: "a workstream ID and unit are required"}
	}
	if req.Decision != "review" && req.Decision != "revise" || strings.TrimSpace(req.Note) == "" {
		return ContestedRulingResponse{}, &APIError{Code: Validation, Message: "a contested ruling requires review or revise and a note"}
	}
	_, projects := s.Runtimes()
	if len(projects) == 0 {
		return ContestedRulingResponse{}, &APIError{Code: NoProject, Message: "no active project"}
	}
	repo := projects[0].Repository
	for _, p := range projects[1:] {
		streams, err := p.Repository.Workstreams()
		if err != nil {
			return ContestedRulingResponse{}, &APIError{Code: Internal, Message: "cannot read the workstream state"}
		}
		if slices.Contains(streams, stream) {
			repo = p.Repository
		}
	}
	return s.RecordContestedRuling(ctx, repo, stream, unit, req, ownerActor)
}

func RulerName(actor trace.Actor) string {
	switch {
	case actor.Kind == ownerActor.Kind:
		return "owner"
	case actor == trace.JevActor:
		return trace.DecidedByJev
	case actor.Kind == "service":
		return "service"
	}
	return trace.ManagerRulerName
}

// shownRuling is ruling as the response to its caller reports it: the stored
// ruler name of the manager becomes the manager's display name.
func shownRuling(ruling ContestedRuling) ContestedRuling {
	ruling.By = trace.DisplayRuler(ruling.By)
	return ruling
}

func ContestRuler(r ContestedRuling) string {
	switch r.By {
	case "":
		return "owner"
	case trace.DecidedByJev:
		return "decision maker Jev"
	}
	return trace.DisplayRuler(r.By)
}

func contestActor(r ContestedRuling) trace.Actor {
	switch r.By {
	case "", "owner":
		return ownerActor
	case trace.DecidedByJev:
		return trace.JevActor
	}
	return managerActor
}

// RecordContestedRuling rules on a contested unit as actor: the owner, or the
// manager on the owner's behalf. The rulings each contest takes, and
// what they resume, do not depend on who gives them.
func (s *Controller) RecordContestedRuling(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, unit string, req ContestedRulingRequest, actor trace.Actor, decisions ...trace.Document) (ContestedRulingResponse, *APIError) {
	return s.RecordContestedRulingJudged(ctx, repo, stream, unit, req, actor, "", decisions...)
}

// RecordContestedRulingJudged is RecordContestedRuling with the Jev judgment
// the ruling comes from. A non-empty judgment is Jev deciding an owner gate:
// the actor must be the Jev actor, the ruling names the judgment, and the
// owner's rules apply, not the manager's.
func (s *Controller) RecordContestedRulingJudged(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, unit string, req ContestedRulingRequest, actor trace.Actor, judgment string, decisions ...trace.Document) (ContestedRulingResponse, *APIError) {
	if judgment != "" && actor != trace.JevActor || judgment == "" && actor == trace.JevActor {
		return ContestedRulingResponse{}, &APIError{Code: Validation, Message: "a Jev ruling is written by Jev and names its judgment"}
	}
	if req.Decision != "review" && req.Decision != "revise" || strings.TrimSpace(req.Note) == "" {
		return ContestedRulingResponse{}, &APIError{Code: Validation, Message: "a contested ruling requires review or revise and a note"}
	}
	by := RulerName(actor)
	if gone, err := abandoned(repo, stream); err != nil {
		return ContestedRulingResponse{}, &APIError{Code: Internal, Message: "cannot read the workstream state"}
	} else if gone {
		return ContestedRulingResponse{}, &APIError{Code: Conflict, Message: "abandoned workstream takes no contested ruling"}
	}
	state, err := repo.Workflow(stream, trace.UnitSubject(unit))
	if err != nil {
		return ContestedRulingResponse{}, &APIError{Code: Internal, Message: "cannot read the contested unit"}
	}
	if state.Value != UnitContested {
		return ContestedRulingResponse{}, &APIError{Code: Conflict, Message: "unit is not contested"}
	}
	contest, mason, err := MasonContest(repo, stream, unit)
	if err != nil {
		return ContestedRulingResponse{}, &APIError{Code: Internal, Message: "cannot read contested transition"}
	}
	if MoveContest(contest, unit) || ChecksContest(contest, unit) || RebaseContest(contest, unit) || contest.Cause == questionWaitContestCause {
		to := map[string]string{"review": UnitReviewing, "revise": UnitImplementing}[req.Decision]
		out, api := s.moveUnit(ctx, repo, stream, unit, UnitMoveRequest{To: to, Note: req.Note}, actor, "", judgment)
		if api != nil {
			return ContestedRulingResponse{}, api
		}
		return ContestedRulingResponse{Workstream: stream, Unit: unit, Ruling: ContestedRuling{Decision: req.Decision, Note: out.Move.Note, By: trace.DisplayRuler(by), Judgment: judgment, Contest: contest.ID}}, nil
	}
	if FailedReview(contest, unit) {
		if req.Decision != "review" {
			return ContestedRulingResponse{}, &APIError{Code: Validation, Message: "the reviewer's turn failed and left no findings; use review"}
		}
		ruling := ContestedRuling{Decision: "review", Note: strings.TrimSpace(req.Note), By: by, Judgment: judgment, Contest: contest.ID}
		id := trace.EventID(contest.ID, "ruling")
		reason := fmt.Sprintf("%s ruled review on contest %s, raised when the reviewer's turn failed; a new review turn runs: %s", trace.DisplayRuler(by), contest.ID, ruling.Note)
		if api := s.routeRuling(ctx, false, UnitExitInput{Repo: repo, Stream: stream, Unit: unit, State: state, Cause: contest.ID, Default: UnitReviewing, Targets: []string{UnitReviewing, UnitImplementing},
			Actor: actor, Note: ruling.Note, ID: id, HeaderCause: contest.ID, Reason: reason, Events: []trace.Event{trace.Notice(id, "unit", reason)}}, ruling); api != nil {
			return ContestedRulingResponse{}, api
		}
		return ContestedRulingResponse{Workstream: stream, Unit: unit, Ruling: shownRuling(ruling)}, nil
	}
	if mason {
		if req.Decision == "review" {
			return ContestedRulingResponse{}, &APIError{Code: Validation, Message: "no candidate under review; use revise"}
		}
		th, err := repo.Thread(stream, masonAgent(unit))
		if err != nil || len(th.Turns) == 0 {
			return ContestedRulingResponse{}, &APIError{Code: Internal, Message: "contested mason thread is missing"}
		}
		last := th.Turns[len(th.Turns)-1]
		if last.Response == nil || last.Response.ID != contest.Cause {
			return ContestedRulingResponse{}, &APIError{Code: Internal, Message: "contested mason turn does not match transition"}
		}
		ruling := ContestedRuling{Decision: "revise", Note: strings.TrimSpace(req.Note), By: by, Judgment: judgment, Contest: contest.ID, ResetTurn: last.Sequence}
		data, _ := json.MarshalIndent(ruling, "", "  ")
		id := fmt.Sprintf("%s-mason-ruling-%d", trace.UnitSubject(unit), last.Sequence)
		doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), Workstream: stream, Unit: unit, At: s.Now(), Actor: actor, Cause: contest.ID}, Path: masonRulingPath(unit, last.Sequence), Content: string(data) + "\n"}
		reason := fmt.Sprintf("%s ruled revise on mason contest %s; clean-turn attempts reset to zero: %s", trace.DisplayRuler(by), contest.ID, ruling.Note)
		tid := id + "-implementing"
		if api := s.routeRuling(ctx, false, UnitExitInput{Repo: repo, Stream: stream, Unit: unit, State: state, Cause: contest.ID, Default: UnitImplementing, Targets: []string{UnitImplementing, UnitReviewing},
			Actor: actor, Note: ruling.Note, ID: tid, HeaderCause: id, Reason: reason, Events: []trace.Event{trace.Notice(tid, "unit", reason)}, Documents: []trace.Document{doc}}, ruling); api != nil {
			return ContestedRulingResponse{}, api
		}
		return ContestedRulingResponse{Workstream: stream, Unit: unit, Ruling: shownRuling(ruling)}, nil
	}
	r := &reviewers{masons: &masons{s: s, cfg: s.About(repo), repository: repo}}
	result, ok, err := r.StoredResult(stream, unit, state)
	if err != nil || !ok || result.Verdict.Decision != "material_findings" {
		return ContestedRulingResponse{}, &APIError{Code: Internal, Message: "contested unit has no material review result"}
	}
	ruling, found, err := LatestContestedRuling(repo, stream, unit, result.Bounces)
	if err != nil {
		return ContestedRulingResponse{}, &APIError{Code: Internal, Message: "cannot read contested ruling"}
	}
	docs := slices.Clone(decisions)
	if found {
		for i := range docs {
			var decision ManagerContestDecision
			if err := json.Unmarshal([]byte(docs[i].Content), &decision); err != nil {
				return ContestedRulingResponse{}, &APIError{Code: Internal, Message: "cannot read manager decision"}
			}
			decision.Decision, decision.Note = ruling.Decision, ruling.Note
			data, _ := json.MarshalIndent(decision, "", "  ")
			docs[i].Content = string(data) + "\n"
		}
	}
	if !found {
		ruling = ContestedRuling{Decision: req.Decision, Note: strings.TrimSpace(req.Note), By: by, Judgment: judgment, Bounces: result.Bounces, Candidate: result.Identity.Candidate.Revision}
		data, _ := json.MarshalIndent(ruling, "", "  ")
		id := fmt.Sprintf("%s-ruling-%d", trace.UnitSubject(unit), result.Bounces)
		docs = append(docs, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), Workstream: stream, Unit: unit, At: s.Now(), Actor: actor, Cause: ReviewDocument(unit)}, Path: ContestedRulingPath(unit, result.Bounces), Content: string(data) + "\n"})
	}
	if err := r.applyBounceRuling(ctx, false, stream, unit, state, result, ruling, docs); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			return ContestedRulingResponse{}, &APIError{Code: Conflict, Message: "unit already has a ruling"}
		}
		return ContestedRulingResponse{}, &APIError{Code: Internal, Message: "cannot record contested ruling"}
	}
	return ContestedRulingResponse{Workstream: stream, Unit: unit, Ruling: shownRuling(ruling)}, nil
}

func (r *reviewers) resumeContested(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, contests ...trace.Transition) error {
	return r.resumeContestedBy(ctx, false, stream, unit, state, contests...)
}

func (r *reviewers) resumeContestedBy(ctx context.Context, scheduled bool, stream config.WorkstreamID, unit string, state trace.WorkflowState, contests ...trace.Transition) error {
	var contest trace.Transition
	if len(contests) > 0 {
		contest = contests[0]
	} else {
		var err error
		contest, _, err = MasonContest(r.repository, stream, unit)
		if err != nil {
			return err
		}
	}
	if !BounceContest(contest, unit) {
		return nil
	}
	result, ok, err := r.StoredResult(stream, unit, state)
	if err != nil || !ok {
		return err
	}
	ruling, found, err := LatestContestedRuling(r.repository, stream, unit, result.Bounces)
	if err != nil || !found {
		return err
	}
	if ruling.Candidate != result.Identity.Candidate.Revision || ruling.Bounces != result.Bounces {
		return nil
	}
	return r.applyBounceRuling(ctx, scheduled, stream, unit, state, result, ruling, nil)
}

func (r *reviewers) applyBounceRuling(ctx context.Context, scheduled bool, stream config.WorkstreamID, unit string, state trace.WorkflowState, result UnitReviewResult, ruling ContestedRuling, docs []trace.Document) error {
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
	reason := fmt.Sprintf("%s ruled %s on contested candidate %s after %d send-backs: %s", ContestRuler(ruling), ruling.Decision, ruling.Candidate, ruling.Bounces, ruling.Note)
	if err := r.s.Step("bounce-ruling-recording:" + string(stream) + ":" + unit); err != nil {
		return err
	}
	in := UnitExitInput{Repo: r.repository, Stream: stream, Unit: unit, State: state, Cause: fmt.Sprintf("%s-ruling-%d", trace.UnitSubject(unit), result.Bounces), Default: to,
		Targets: []string{UnitReviewing, UnitImplementing}, Actor: contestActor(ruling), Note: ruling.Note,
		ID: id, HeaderCause: fmt.Sprintf("%s-ruling-%d", trace.UnitSubject(unit), result.Bounces), Reason: reason, Events: []trace.Event{trace.Notice(id, "unit", reason)}, Documents: docs}
	if api := r.s.routeRuling(ctx, scheduled, in, ruling); api != nil {
		if api.Code == Conflict {
			return trace.ErrConflict
		}
		return errors.New(api.Message)
	}
	_ = r.s.Step("bounce-ruling-committed:" + string(stream) + ":" + unit)
	return nil
}

func (s *Controller) routeRuling(ctx context.Context, scheduled bool, in UnitExitInput, ruling ContestedRuling) *APIError {
	in.Trigger = TriggerManagerRuling
	in.Authority = &router.Authority{Kind: router.AuthorityManager, Target: in.Default}
	switch ruling.By {
	case "", "owner":
		in.Trigger, in.Authority = TriggerOwnerRuling, &router.Authority{Kind: router.AuthorityOwner, Target: in.Default}
	case trace.DecidedByJev:
		if ruling.Judgment == "" {
			return &APIError{Code: Validation, Message: "a Jev ruling names its judgment"}
		}
		in.Trigger, in.Authority = TriggerJevRuling, &router.Authority{Kind: router.AuthorityJevGate, Target: in.Default, Judgment: ruling.Judgment}
	}
	res, err := s.raise(ctx, scheduled, in)
	switch {
	case err != nil:
		return &APIError{Code: Internal, Message: "cannot record contested ruling"}
	case scheduled || res.Committed:
		return nil
	}
	return &APIError{Code: Conflict, Message: "unit already has a ruling"}
}

func bounceRevisionTransition(t trace.Transition) bool {
	return t.From == UnitContested && t.To == UnitImplementing && strings.HasPrefix(t.Cause, t.Subject+"-ruling-")
}

func (m *masons) ResumeBounceRevision(ctx context.Context, stream config.WorkstreamID, unit string, t trace.Transition) error {
	if !bounceRevisionTransition(t) {
		return nil
	}
	r := &reviewers{masons: m}
	result, ok, err := r.StoredResult(stream, unit, trace.WorkflowState{})
	if err != nil || !ok {
		return err
	}
	if t.Cause != fmt.Sprintf("%s-ruling-%d", trace.UnitSubject(unit), result.Bounces) {
		return nil
	}
	return r.enqueueFindings(ctx, stream, unit, result)
}
