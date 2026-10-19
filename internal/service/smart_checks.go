package service

import (
	"context"
	"fmt"

	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/checkselect"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/trace"
)

// The reasons a selection runs every check without a Jev decision. A Jev
// fallback records the judgment's own reason.
const (
	selectionUnlisted   = "unlisted"
	selectionNoChecks   = "no_checks"
	selectionTooMany    = "too_many_checks"
	selectionTooLarge   = "change_too_large"
	selectionUnreadable = "diff_unreadable"
)

// The modes of a check selection.
const (
	selectionSelected = "selected"
	selectionFull     = "full"
)

// CheckSelection is which checks a run ran and why. A selected run ran Links,
// chosen by Jev judgment Judgment from Candidates check links. A full run ran
// every check, because of Reason, with Detail. Listed is the number of links
// the project listed, before narrowing, or nil when listing did not complete.
type CheckSelection struct {
	Mode       string   `json:"mode"`
	Links      []string `json:"links,omitempty"`
	Candidates int      `json:"candidates,omitempty"`
	Listed     *int     `json:"listed,omitempty"`
	Judgment   string   `json:"judgment,omitempty"`
	Reason     string   `json:"reason,omitempty"`
	Detail     string   `json:"detail,omitempty"`
}

// describe says in a sentence which checks ran.
func (s CheckSelection) describe() string {
	if s.Mode == selectionSelected {
		return fmt.Sprintf("Jev selected %d of %d check links for the changed files in judgment %s", len(s.Links), s.Candidates, s.Judgment)
	}
	if s.Reason == string(jev.ReasonDisabled) {
		return "every check ran: the Jev boost is off"
	}
	why := s.Reason
	if s.Detail != "" {
		why += ": " + s.Detail
	}
	return "every check ran: Jev did not select them (" + why + ")"
}

// fullSelection runs every check for reason.
func fullSelection(reason, detail string) CheckSelection {
	return CheckSelection{Mode: selectionFull, Reason: reason, Detail: detail}
}

// selectChecks asks Jev which of the listed checks the change can affect.
// Every outcome but an accepted judgment runs every check. Listing waits for
// the project's modules to load, so it is skipped while the boost is off.
func (c *checkers) selectChecks(ctx context.Context, in checkInput, dir, diff string, checks ReviewChecks) CheckSelection {
	return selectChecksFor(ctx, c.s.jev, c.repository, c.scope(in), checkRequestID(in.Unit, in.Run),
		[]jev.Source{{Kind: "unit-report", ID: reportDocument(in.Unit), Revision: in.Report}}, dir, diff, checks)
}

// selectChecksFor is the service's smart-check selection: it asks Jev which
// of the listed checks a change, described by diff from a disposable export
// at dir, can affect, scoped to scope and keyed by cause for reuse across a
// retry. Every outcome but an accepted judgment runs every check. Listing
// waits for the project's modules to load, so it is skipped while the boost
// is off. The unit check run and the session check control both ask through
// this one function, so either reuses exactly the same selection.
func selectChecksFor(ctx context.Context, j *jev.Judge, repository *trace.Repository, scope coreadapter.Scope, cause string, sources []jev.Source, dir, diff string, checks ReviewChecks) (selection CheckSelection) {
	if j.Status().Mode == jev.ModeDisabled {
		return fullSelection(string(jev.ReasonDisabled), "")
	}
	links, err := checks.List(ctx, dir)
	if err != nil {
		return fullSelection(selectionUnlisted, err.Error())
	}
	listed := len(links)
	defer func() { selection.Listed = &listed }()
	if len(links) == 0 {
		return fullSelection(selectionNoChecks, "the project lists no check links")
	}
	candidates, ok := checkselect.Candidates(links, checkselect.MaxQuestions)
	if !ok {
		return fullSelection(selectionTooMany, fmt.Sprintf("%d check links do not narrow to %d", len(links), checkselect.MaxQuestions))
	}
	state, ok := checkselect.State(diff)
	if !ok {
		return fullSelection(selectionTooLarge, "the changed files do not fit a judgment")
	}
	decision := j.Evaluate(ctx, repository, jev.Judgment{
		Scope:   scope,
		Cause:   cause,
		Depth:   1,
		Task:    checkselect.Task,
		Version: checkselect.Version,
		Sources: sources,
		Request: checkselect.Request(state, candidates),
		Accept: func(r systemone.Response) string {
			if len(checkselect.Selected(r, candidates, checkselect.Threshold)) == 0 {
				return fmt.Sprintf("no check reached probability %.2f", checkselect.Threshold)
			}
			return ""
		},
	})
	if decision.Outcome != jev.Accepted {
		s := fullSelection(string(decision.Reason), decision.Detail)
		s.Judgment = decision.ID
		s.Candidates = len(candidates)
		return s
	}
	return CheckSelection{Mode: selectionSelected, Links: checkselect.Selected(*decision.Response, candidates, checkselect.Threshold), Candidates: len(candidates), Judgment: decision.ID}
}
