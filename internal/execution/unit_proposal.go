package execution

import (
	"fmt"
	"slices"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/trace"
)

// recoveryCard names where the owner decides a move that belongs to the owner
// and has no card of its own.
const recoveryCard = router.RecoveryCard

// proposalHistory is how many earlier moves and rulings of a unit Jev reads
// with a proposal.
const proposalHistory = 10

// proposalPolicy is how the chief of staff's policies set the default of a
// proposal to move a unit, and the history Jev reads beside it.
type proposalPolicy struct {
	// Stay says the default leaves the unit where it is: the chief of staff
	// raised the contest the unit sits in, or ruled on it already, so moving
	// out of it is a departure from the default.
	Stay bool
	// Owner says the move is the owner's: the owner holds the contest the
	// unit sits in, so the proposal waits on a pending owner decision.
	Owner bool
	// Why says, in a sentence, which policy applies.
	Why      string
	Facts    map[string]any
	Records  []router.Entry
	RaisedBy string
}

// unitProposalPolicy reads what bears on a proposal to move the unit to a
// state: the unit's earlier moves and the chief of staff's earlier rulings,
// which Jev reads as evidence, and the contest the unit sits in, if any. The
// chief of staff rules a contest once and does not move a unit out of a contest
// it raised itself; those set the default to leave the unit where it is. A
// contest the owner raised by a move of its own is the owner's to move the unit
// out of.
func unitProposalPolicy(repo *trace.Repository, stream config.WorkstreamID, unit string, state trace.WorkflowState, to string) (proposalPolicy, error) {
	p := proposalPolicy{Facts: map[string]any{"state": state.Value, "target": to}}
	docs, moves, err := recordedMoves(repo, stream, unit)
	if err != nil {
		return p, err
	}
	p.Facts["earlier_moves"] = len(moves)
	for i := max(0, len(moves)-proposalHistory); i < len(moves); i++ {
		m := moves[i]
		p.Records = append(p.Records, router.Entry{Kind: "earlier move", ID: docs[i].ID, Text: fmt.Sprintf("%s moved the unit from %s to %s: %s", m.By, m.From, m.To, m.Note)})
	}
	_, decisions, err := chiefDecisions(repo, stream, unit)
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
	contest, contested, err := unitContest(repo, stream, unit)
	if err != nil || !contested {
		return p, err
	}
	switch {
	case moveContest(contest, unit) && contest.Actor.Kind == ownerActor.Kind:
		p.Owner, p.RaisedBy = true, "owner"
		p.Why = fmt.Sprintf("the owner moved unit %s to contested and holds it", unit)
	case moveContest(contest, unit):
		p.Stay, p.RaisedBy = true, "chief of staff"
		p.Why = fmt.Sprintf("the chief of staff raised the contest of unit %s itself, and by default does not move it out", unit)
	case slices.ContainsFunc(decisions, func(d ChiefContestDecision) bool { return d.Contest == contest.ID }):
		p.Stay, p.RaisedBy = true, "service"
		p.Why = fmt.Sprintf("the chief of staff already ruled on contest %s of unit %s, and by default rules a contest once", contest.ID, unit)
	default:
		p.RaisedBy = "service"
	}
	p.Facts["contest"] = contest.ID
	p.Facts["contest_raised_by"] = p.RaisedBy
	return p, nil
}

// unitDependenciesMerged reports whether every unit the unit depends on has
// merged.
func (s *Controller) unitDependenciesMerged(repo *trace.Repository, stream config.WorkstreamID, unit string) (bool, error) {
	m := &masons{s: s, cfg: s.About(repo), repository: repo}
	b, found, err := m.readSealed(stream)
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

// unitStarted reports whether a unit in the state has started.
func unitStarted(state string) bool { return state != UnitPlanned && state != UnitReady }

// moveLegality returns why a unit in state from does not move to the target
// now, or "". A unit that has not started moves to ready, once every unit it
// depends on has merged, or back to planned while one has not, and its mason
// starts it when it is ready; a started unit moves among the states of its work.
// A target that leaves the unit where it is has no condition.
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

// proposalInput returns the exit of the chief of staff's proposal to move the
// unit to a state. The proposal is the default, except where a policy of the
// chief of staff sets the default to leave the unit where it is: then the
// proposed state is a departure that Jev may take at its bar. A proposal never
// carries the authority to approve a candidate, and a contest the owner holds
// is the owner's to move the unit out of; both leave the move as a pending
// owner decision.
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
