package service

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// driftEntry is the workstream's latest drift rebase while it is held for
// the owner and the owner has not asked for another, opened when it was
// held. It is answered by asking for a drift rebase of the project, or by a
// handback of it.
func driftEntry(repository *trace.Repository, w trace.WorkstreamStatus) (InboxEntry, bool, error) {
	value := w.Subjects[driftSubject].Value
	if !strings.HasPrefix(value, driftHeld+"-") {
		return InboxEntry{}, false, nil
	}
	k, err := driftNumber(value)
	if err != nil {
		return InboxEntry{}, false, err
	}
	if requested, err := driftNumber(w.Subjects[driftRequestSubject].Value); err != nil || requested > k {
		return InboxEntry{}, false, err
	}
	transitions, err := trace.Read[trace.Transition](repository, w.Workstream)
	if err != nil {
		return InboxEntry{}, false, err
	}
	transition, _ := driftIDs(k)
	i := slices.IndexFunc(transitions, func(t trace.Transition) bool { return t.ID == transition+"-"+driftHeld })
	if i < 0 {
		return InboxEntry{}, false, nil
	}
	held := transitions[i]
	e := decision(InboxDrift, w.Workstream, held.At, "/rebase/"+string(w.Workstream), map[string]any{})
	e.Revision = k
	e.Question = fmt.Sprintf("Drift rebase %d is held: %s.", k, held.Reason)
	e.Blocked = "Rebasing the feature branch onto upstream. No drift rebase is asked for on the upstream_rebase cadence until you ask for one with osmia rebase " + string(w.Workstream) + ", or ask the chief of staff in a message to hand it back with what its drift mason or reviewer should do differently."
	return e, true, nil
}

// baseEntry is the workstream's park on an unavailable base, opened when it
// was parked. When its own base workstream was abandoned, the owner can move
// it onto upstream; otherwise it takes no decision here, and leaves the inbox
// once the base is available again or the workstream is abandoned.
func baseEntry(repository *trace.Repository, w trace.WorkstreamStatus) (InboxEntry, bool, error) {
	if w.Subjects[baseWaitSubject].Value != "waiting" {
		return InboxEntry{}, false, nil
	}
	transitions, err := trace.Read[trace.Transition](repository, w.Workstream)
	if err != nil {
		return InboxEntry{}, false, err
	}
	var parked trace.Transition
	for _, t := range transitions {
		if t.Subject == baseWaitSubject && t.To == "waiting" {
			parked = t
		}
	}
	dependency, err := repository.WorkstreamBase(w.Workstream)
	if err != nil {
		return InboxEntry{}, false, err
	}
	var parent trace.WorkflowState
	if dependency.Base != "" {
		if parent, err = repository.Workflow(dependency.Base, trace.FeatureSubject); err != nil {
			return InboxEntry{}, false, err
		}
	}
	path := fmt.Sprintf("/base/%s/upstream", w.Workstream)
	e := decision(InboxBase, w.Workstream, parked.At, path, map[string]any{"base": string(dependency.Base)})
	e.Question = fmt.Sprintf("The workstream is parked: %s.", parked.Reason)
	e.Blocked = "Everything on the workstream but the chief of staff and the check of its base."
	if parent.Value == AbandonedState {
		e.Options = []string{BaseDecisionUpstream}
		e.Question += fmt.Sprintf(" If upstream already holds the changes of %s this workstream builds on, it can continue from upstream: the service moves it there only once merging them into upstream changes nothing, and its next drift rebase replays its own commits onto upstream. Otherwise abandon it.", dependency.Base)
	} else {
		e.Answer = InboxAnswer{Method: http.MethodPost, Path: Prefix + "/abandon/" + string(w.Workstream), Body: map[string]any{}}
		e.Question += " It continues once its base is available again; otherwise abandon it."
	}
	return e, true, nil
}

// contestOptions returns the rulings a contest takes: review alone after a
// failed review turn, revise alone after a mason contest, and either after
// send-backs by review or failed checks.
func contestOptions(contest trace.Transition, unit string, mason bool) []string {
	switch {
	case failedReview(contest, unit):
		return []string{"review"}
	case mason:
		return []string{"revise"}
	}
	return []string{"review", "revise"}
}

// contestedEntry is a contested unit the chief of staff raised to the owner
// that has no ruling yet, with the rulings its contest takes and the chief of
// staff's note.
func (s *Service) contestedEntry(repository *trace.Repository, stream config.WorkstreamID, unit string) (InboxEntry, bool, error) {
	state, err := repository.Workflow(stream, trace.UnitSubject(unit))
	if err != nil || state.Value != UnitContested {
		return InboxEntry{}, false, err
	}
	contest, mason, err := masonContest(repository, stream, unit)
	if err != nil {
		return InboxEntry{}, false, err
	}
	// The chief of staff rules on a contest first; it is the owner's once
	// raised.
	raised, note, err := contestRaised(repository, stream, unit, contest)
	if err != nil || !raised {
		return InboxEntry{}, false, err
	}
	options := contestOptions(contest, unit, mason)
	switch {
	case failedReview(contest, unit), mason, moveContest(contest, unit), checksContest(contest, unit):
	default:
		r := &reviewers{masons: &masons{s: s, cfg: s.about(repository), repository: repository}}
		result, ok, err := r.storedResult(stream, unit, state)
		if err != nil {
			return InboxEntry{}, false, err
		}
		if ok {
			if _, ruled, err := latestContestedRuling(repository, stream, unit, result.Bounces); err != nil || ruled {
				return InboxEntry{}, false, err
			}
		}
	}
	e := decision(InboxContested, stream, contest.At, "/contested/"+string(stream)+"/"+unit, map[string]any{})
	e.Unit = unit
	e.Question = fmt.Sprintf("Unit %s is contested: %s", unit, contest.Reason)
	// A move's reason ends with its note, which the entry already carries.
	if moveContest(contest, unit) {
		e.Question = fmt.Sprintf("Unit %s is contested: you moved it here from %s.", unit, contest.From)
		if rulerName(contest.Actor) != "owner" {
			e.Question = fmt.Sprintf("Unit %s is contested: the chief of staff moved it here from %s for you to rule on.", unit, contest.From)
		}
	}
	e.Blocked = fmt.Sprintf("Unit %s.", unit)
	e.Options = options
	e.Recommendation = note
	return e, true, nil
}
