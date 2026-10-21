package service

import (
	"encoding/json"
	"fmt"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/followup"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

// unitStates returns states, latest cards, landings and deferrals, taken from
// the given workflow states, reports, landing and dispatch documents, of the
// units of the workstream's sealed plan and final-review follow-ups, in
// order: none before their unit states are recorded.
func unitStates(repository *trace.Repository, stream config.WorkstreamID, states map[string]trace.WorkflowState) ([]UnitStatus, error) {
	latest, _, found, err := seal.Latest(repository, stream)
	if err != nil || !found {
		return nil, err
	}
	graph, err := sealedPlan(repository, stream, latest.Revision.Plan)
	if err != nil {
		return nil, err
	}
	p, err := plan.Parse([]byte(graph.Content))
	if err != nil {
		return nil, err
	}
	added, err := followup.Read(repository, stream)
	if err != nil {
		return nil, err
	}
	for _, u := range added {
		p.Units = append(p.Units, u.Unit)
	}
	documents, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return nil, err
	}
	cards := map[string]*coreadapter.Card{}
	landings := map[string]*UnitLanding{}
	dispatches := map[string]trace.Document{}
	for _, document := range documents {
		if document.Unit != "" && document.ID == dispatchDocument(document.Unit) {
			dispatches[document.Unit] = document
			continue
		}
		if document.Unit != "" && document.ID == landingDocument(document.Unit) {
			var landing UnitLanding
			if err := json.Unmarshal([]byte(document.Content), &landing); err != nil {
				return nil, fmt.Errorf("unit %s landing: %w", document.Unit, err)
			}
			landings[document.Unit] = &landing
			continue
		}
		if document.Unit == "" || document.ID != reportDocument(document.Unit) {
			continue
		}
		var report UnitReport
		if err := json.Unmarshal([]byte(document.Content), &report); err != nil {
			return nil, fmt.Errorf("unit %s report: %w", document.Unit, err)
		}
		if report.Card != nil {
			cards[document.Unit] = report.Card
		}
	}
	var out []UnitStatus
	for _, u := range p.Units {
		state := states[trace.UnitSubject(u.ID)]
		if state.Value == "" {
			continue
		}
		status := UnitStatus{Unit: u.ID, State: state.Value, Card: cards[u.ID], Landing: landings[u.ID]}
		if status.Deferral, err = currentDeferral(dispatches[u.ID], state); err != nil {
			return nil, err
		} else if status.Deferral != nil {
			status.Reason = status.Deferral.Message
		}
		if state.Value == UnitContested {
			contest, _, err := unitContest(repository, stream, u.ID)
			if err != nil {
				return nil, err
			}
			status.Reason = contest.Reason
		}
		out = append(out, status)
	}
	return out, nil
}
