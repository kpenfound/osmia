package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// ContestedRuling records the owner's direction for a contested unit.
type ContestedRuling struct {
	Decision  string `json:"decision"`
	Note      string `json:"note"`
	Bounces   int    `json:"bounces"`
	Candidate string `json:"candidate"`
	Contest   string `json:"contest,omitempty"`
	ResetTurn uint64 `json:"reset_turn,omitempty"`
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

func latestMasonRuling(repo *trace.Repository, stream config.WorkstreamID, unit string) (ContestedRuling, bool, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return ContestedRuling{}, false, err
	}
	var latest ContestedRuling
	found := false
	for _, d := range docs {
		if d.Unit != unit || !strings.HasPrefix(d.Path, "units/"+trace.UnitSubject(unit)+"/mason-ruling-") {
			continue
		}
		var ruling ContestedRuling
		if err := json.Unmarshal([]byte(d.Content), &ruling); err != nil {
			return ContestedRuling{}, false, err
		}
		if !found || ruling.ResetTurn > latest.ResetTurn {
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
	s.mu.Lock()
	active := s.active
	s.mu.Unlock()
	if active == nil {
		return ContestedRulingResponse{}, &APIError{NoProject, "no active project"}
	}
	repo := active.repository
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
	if failedReview(contest, unit) {
		// A failed review turn left no findings, so the unit can only be reviewed again.
		if req.Decision != "review" {
			return ContestedRulingResponse{}, &APIError{Validation, "the reviewer's turn failed and left no findings; use review"}
		}
		ruling := ContestedRuling{Decision: "review", Note: strings.TrimSpace(req.Note), Contest: contest.ID}
		id := trace.EventID(contest.ID, "ruling")
		reason := fmt.Sprintf("owner ruled review on contest %s, raised when the reviewer's turn failed; a new review turn runs: %s", contest.ID, ruling.Note)
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), Workstream: stream, Unit: unit, At: s.now(), Actor: ownerActor, Cause: contest.ID}
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
		ruling := ContestedRuling{Decision: "revise", Note: strings.TrimSpace(req.Note), Contest: contest.ID, ResetTurn: last.Sequence}
		data, _ := json.MarshalIndent(ruling, "", "  ")
		id := fmt.Sprintf("%s-mason-ruling-%d", trace.UnitSubject(unit), last.Sequence)
		doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), Workstream: stream, Unit: unit, At: s.now(), Actor: ownerActor, Cause: contest.ID}, Path: masonRulingPath(unit, last.Sequence), Content: string(data) + "\n"}
		reason := fmt.Sprintf("owner ruled revise on mason contest %s; clean-turn attempts reset to zero: %s", contest.ID, ruling.Note)
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id + "-implementing", Revision: 1, Project: repo.Project(), Workstream: stream, Unit: unit, At: s.now(), Actor: ownerActor, Cause: id}
		tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: trace.UnitSubject(unit), From: UnitContested, To: UnitImplementing, Reason: reason}, Events: []trace.Event{trace.Notice(h.ID, "unit", reason)}}
		if _, err := repo.RecordDocumentsWith(ctx, []trace.Document{doc}, tx); err != nil {
			if errors.Is(err, trace.ErrConflict) {
				return ContestedRulingResponse{}, &APIError{Conflict, "unit already has a ruling"}
			}
			return ContestedRulingResponse{}, &APIError{Internal, "cannot record contested ruling"}
		}
		return ContestedRulingResponse{Workstream: stream, Unit: unit, Ruling: ruling}, nil
	}
	r := &reviewers{masons: &masons{s: s, cfg: s.current(), repository: repo}}
	result, ok, err := r.storedResult(stream, unit, state)
	if err != nil || !ok || result.Verdict.Decision != "material_findings" {
		return ContestedRulingResponse{}, &APIError{Internal, "contested unit has no material review result"}
	}
	if _, found, err := latestContestedRuling(repo, stream, unit, result.Bounces); err != nil {
		return ContestedRulingResponse{}, &APIError{Internal, "cannot read contested ruling"}
	} else if found {
		return ContestedRulingResponse{}, &APIError{Conflict, "unit already has a ruling"}
	}
	ruling := ContestedRuling{Decision: req.Decision, Note: strings.TrimSpace(req.Note), Bounces: result.Bounces, Candidate: result.Identity.Candidate.Revision}
	data, _ := json.MarshalIndent(ruling, "", "  ")
	id := fmt.Sprintf("%s-ruling-%d", trace.UnitSubject(unit), result.Bounces)
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), Workstream: stream, Unit: unit, At: s.now(), Actor: ownerActor, Cause: reviewDocument(unit)}, Path: contestedRulingPath(unit, result.Bounces), Content: string(data) + "\n"}
	if err := repo.RecordDocuments(ctx, []trace.Document{doc}); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			return ContestedRulingResponse{}, &APIError{Conflict, "unit already has a ruling"}
		}
		return ContestedRulingResponse{}, &APIError{Internal, "cannot record contested ruling"}
	}
	return ContestedRulingResponse{Workstream: stream, Unit: unit, Ruling: ruling}, nil
}

func (r *reviewers) resumeContested(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState) error {
	// Only a contest raised by review bounces is resumed by a bounce ruling.
	if contest, _, err := masonContest(r.repository, stream, unit); err != nil || contest.From != UnitReviewing || contest.Actor != reviewerActor || failedReview(contest, unit) {
		return err
	}
	result, ok, err := r.storedResult(stream, unit, state)
	if err != nil || !ok {
		return err
	}
	ruling, found, err := latestContestedRuling(r.repository, stream, unit, result.Bounces)
	if err != nil || !found {
		return err
	}
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
	reason := fmt.Sprintf("owner ruled %s on contested candidate %s after %d material reviews: %s", ruling.Decision, ruling.Candidate, ruling.Bounces, ruling.Note)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: r.repository.Project(), Workstream: stream, Unit: unit, At: r.s.now(), Actor: ownerActor, Cause: fmt.Sprintf("%s-ruling-%d", trace.UnitSubject(unit), result.Bounces)}
	_, err = r.repository.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: trace.UnitSubject(unit), From: UnitContested, To: to, Reason: reason}, Events: []trace.Event{trace.Notice(id, "unit", reason)}})
	if errors.Is(err, trace.ErrConflict) {
		return nil
	}
	if err != nil {
		return err
	}
	if to == UnitImplementing {
		return r.enqueueFindings(ctx, stream, unit, result)
	}
	return nil
}
