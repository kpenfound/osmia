package service

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/trace"
)

func (s *Service) contestedGate(repository *trace.Repository, stream config.WorkstreamID, unit string) (*intake.Gate, *jev.Result, error) {
	e, open, err := s.contestedEntry(repository, stream, unit)
	if err != nil || !open {
		return nil, nil, err
	}
	contest, ok, err := unitContest(repository, stream, unit)
	if err != nil || !ok {
		return nil, nil, err
	}
	_, escalated, err := execution.ContestEscalated(repository, stream, unit, contest)
	if err != nil || !escalated {
		return nil, nil, err
	}
	key := jev.ContestKey(contest.ID)
	if res, ok := s.intake().CarriedJudgment(repository, stream, jev.KindContestedRuling, key); ok {
		return nil, &res, nil
	}
	if !slices.Contains(e.Options, "review") {
		return nil, nil, nil
	}
	project := repository.Project()
	g := &intake.Gate{Kind: jev.KindEscalatedContest, Project: project, Stream: stream, Key: key,
		Label: fmt.Sprintf("contested unit %s of workstream %s", unit, stream), Cause: contest.ID, Reached: contest.At,
		Options: []jev.DecisionOption{{Name: "review", Description: "Have the unit's candidate reviewed again; the unit continues from that review."}}}
	g.State = func(ctx context.Context) (string, []jev.Source, error) {
		var b strings.Builder
		intake.GateSection(&b, "Decision", fmt.Sprintf("Unit %s of workstream %s is contested and the chief of staff escalated it to the owner. The owner may have it reviewed again, or direct its mason to revise it. Revising needs the owner's own words on what to change and is not offered here.", unit, stream), 0)
		intake.GateSection(&b, "Card", e.Question+"\nBlocked: "+e.Blocked, 0)
		intake.GateSection(&b, "Chief of staff's note", e.Recommendation, 0)
		if u, err := sealedUnit(repository, coreadapter.Scope{Workstream: string(stream), Unit: unit}); err == nil {
			intake.GateSection(&b, "The unit", fmt.Sprintf("%s: %s\nAcceptance:\n- %s", u.ID, u.Task, strings.Join(u.Acceptance, "\n- ")), 0)
		}
		if doc, _, err := s.loadCharter(ctx, project); err == nil {
			intake.GateSection(&b, "Charter", doc.Content, 8<<10)
		}
		return b.String(), []jev.Source{{Kind: "contest", ID: contest.ID}}, nil
	}
	g.Apply = func(ctx context.Context, res jev.Result) *APIError {
		if res.Pick != "review" {
			return &APIError{Code: Conflict, Message: fmt.Sprintf("%q is not a ruling Jev gives on a contest", res.Pick)}
		}
		note := fmt.Sprintf("Decided by Jev with confidence %.2f (judgment %s): the candidate is reviewed again.", res.Confidence, res.Judgment)
		_, api := s.execution().RecordContestedRulingJudged(ctx, repository, stream, unit, ContestedRulingRequest{Decision: "review", Note: note}, trace.JevActor, res.Judgment)
		return api
	}
	g.Decision = func() (intake.GateDecision, error) {
		state, err := repository.Workflow(stream, trace.UnitSubject(unit))
		if err != nil {
			return intake.GateDecision{}, err
		}
		latest, still, err := unitContest(repository, stream, unit)
		if err != nil {
			return intake.GateDecision{}, err
		}
		if state.Value == UnitContested && still && latest.ID == contest.ID {
			return intake.GateDecision{}, nil
		}
		transitions, err := trace.Read[trace.Transition](repository, stream)
		if err != nil {
			return intake.GateDecision{}, err
		}
		d := intake.GateDecision{Recorded: true}
		for _, t := range transitions {
			if t.Subject == trace.UnitSubject(unit) && t.From == UnitContested && t.At.After(contest.At) {
				d.ByJev = t.Actor == trace.JevActor
				break
			}
		}
		return d, nil
	}
	return g, nil, nil
}
