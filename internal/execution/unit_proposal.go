package execution

import (
	"fmt"
	"slices"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/trace"
)

const recoveryCard = router.RecoveryCard

const proposalHistory = 10

type proposalPolicy struct {
	Stay     bool
	Owner    bool
	Why      string
	Facts    map[string]any
	Records  []router.Entry
	RaisedBy string
}

func unitProposalPolicy(repo *trace.Repository, stream config.WorkstreamID, unit string, state trace.WorkflowState, to string) (proposalPolicy, error) {
	p := proposalPolicy{Facts: map[string]any{"state": state.Value, "target": to}}
	docs, moves, err := recordedMoves(repo, stream, unit)
	if err != nil {
		return p, err
	}
	p.Facts["earlier_moves"] = len(moves)
	for i := max(0, len(moves)-proposalHistory); i < len(moves); i++ {
		m := moves[i]
		p.Records = append(p.Records, router.Entry{Kind: "earlier move", ID: docs[i].ID, Text: fmt.Sprintf("%s moved the unit from %s to %s: %s", trace.DisplayRuler(m.By), m.From, m.To, m.Note)})
	}
	_, decisions, err := ManagerDecisions(repo, stream, unit)
	if err != nil {
		return p, err
	}
	p.Facts["earlier_rulings"] = len(decisions)
	for i := max(0, len(decisions)-proposalHistory); i < len(decisions); i++ {
		d := decisions[i]
		p.Records = append(p.Records, router.Entry{Kind: "earlier ruling", ID: d.Contest, Text: fmt.Sprintf("the chief of staff ruled %s on contest %s: %s", d.Decision, d.Contest, d.Note)})
	}
	if state.Value != UnitContested {
		return p, nil
	}
	contest, contested, err := UnitContest(repo, stream, unit)
	if err != nil || !contested {
		return p, err
	}
	switch {
	case MoveContest(contest, unit) && contest.Actor.Kind == ownerActor.Kind:
		p.Owner, p.RaisedBy = true, "owner"
		p.Why = fmt.Sprintf("the owner moved unit %s to contested and holds it", unit)
	case MoveContest(contest, unit):
		p.Stay, p.RaisedBy = true, "chief of staff"
		p.Why = fmt.Sprintf("the chief of staff raised the contest of unit %s itself, and by default does not move it out", unit)
	case slices.ContainsFunc(decisions, func(d ManagerContestDecision) bool { return d.Contest == contest.ID }):
		p.Stay, p.RaisedBy = true, "service"
		p.Why = fmt.Sprintf("the chief of staff already ruled on contest %s of unit %s, and by default rules a contest once", contest.ID, unit)
	default:
		p.RaisedBy = "service"
	}
	p.Facts["contest"] = contest.ID
	p.Facts["contest_raised_by"] = p.RaisedBy
	return p, nil
}

func (s *Controller) unitDependenciesMerged(repo *trace.Repository, stream config.WorkstreamID, unit string) (bool, error) {
	m := &masons{s: s, cfg: s.About(repo), repository: repo}
	b, found, err := m.ReadSealed(stream)
	if err != nil || !found {
		return false, err
	}
	for _, u := range b.plan.Units {
		if u.ID == unit {
			return unitDependenciesMerged(b, u), nil
		}
	}
	return false, fmt.Errorf("the sealed plan has no unit %s", unit)
}

func unitStarted(state string) bool { return state != UnitPlanned && state != UnitReady }

func (s *Controller) moveLegality(repo *trace.Repository, stream config.WorkstreamID, unit, from, to string) (string, error) {
	if to == TargetWait {
		return "", nil
	}
	notStarted := to == UnitPlanned || to == UnitReady
	switch {
	case unitStarted(from) && notStarted:
		return fmt.Sprintf("unit %s has started; it does not return to %s", unit, to), nil
	case !unitStarted(from) && !notStarted:
		return fmt.Sprintf("unit %s is %s; only a started unit that has not merged moves to %s", unit, from, to), nil
	case !notStarted:
		return "", nil
	}
	merged, err := s.unitDependenciesMerged(repo, stream, unit)
	if err != nil {
		return "", err
	}
	switch {
	case to == UnitReady && !merged:
		return fmt.Sprintf("a unit that %s depends on has not merged; it is ready once they all have", unit), nil
	case to == UnitPlanned && merged:
		return fmt.Sprintf("every unit that %s depends on has merged, so it is ready", unit), nil
	}
	return "", nil
}

func (s *Controller) proposalInput(repo *trace.Repository, stream config.WorkstreamID, unit string, state trace.WorkflowState, to, note, turn string, actor trace.Actor, cause string) (UnitExitInput, error) {
	policy, err := unitProposalPolicy(repo, stream, unit, state, to)
	if err != nil {
		return UnitExitInput{}, err
	}
	in := UnitExitInput{Repo: repo, Stream: stream, Unit: unit, State: state, Default: to, Proposed: to, Note: note, Turn: turn, Actor: actor, Active: true,
		Trigger: TriggerMoveProposal, Cause: cause, Evidence: router.Evidence{Facts: policy.Facts, Records: policy.Records},
		Legal: func(target string) (string, error) { return s.moveLegality(repo, stream, unit, state.Value, target) }}
	switch {
	case !unitStarted(state.Value):
		in.Targets = []string{UnitReady, UnitPlanned}
	case to == UnitApproved || policy.Owner:
		in.Gate, in.Targets = []string{to}, []string{to}
	case policy.Stay:
		in.Default, in.Targets = TargetWait, []string{TargetWait, to}
		in.ID = cause + "-stay"
		in.Reason = fmt.Sprintf("unit %s stays %s: %s; the proposal to move it to %s was not made: %s", unit, state.Value, policy.Why, to, note)
	}
	return in, nil
}
