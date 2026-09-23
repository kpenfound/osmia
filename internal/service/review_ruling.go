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

// ContestedRuling records the owner's direction for a contested candidate.
// Review requires another reviewer verdict; revise returns the findings to
// the mason. Neither direction approves the candidate.
type ContestedRuling struct {
	Decision  string `json:"decision"`
	Note      string `json:"note"`
	Bounces   int    `json:"bounces"`
	Candidate string `json:"candidate"`
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
