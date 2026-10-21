package checks

import (
	"context"
	"fmt"

	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/checkselect"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/trace"
)

// The reasons a selection runs every check without a Jev decision. A Jev
// fallback records the judgment's own reason.
const (
	SelectionUnlisted = "unlisted"
	SelectionNoChecks = "no_checks"
	SelectionTooMany  = "too_many_checks"
	SelectionTooLarge = "change_too_large"
)

// The modes of a check selection.
const (
	SelectionSelected = "selected"
	SelectionFull     = "full"
)

// DescribeSelection says in a sentence which checks ran.
func DescribeSelection(s api.CheckSelection) string {
	if s.Mode == SelectionSelected {
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

// FullSelection runs every check for reason.
func FullSelection(reason, detail string) api.CheckSelection {
	return api.CheckSelection{Mode: SelectionFull, Reason: reason, Detail: detail}
}

// Select is the service's smart-check selection: it asks Jev which
// of the listed checks a change, described by diff from a disposable export
// at dir, can affect, scoped to scope and keyed by cause for reuse across a
// retry. Every outcome but an accepted judgment runs every check. Listing
// waits for the project's modules to load, so it is skipped while the boost
// is off. Unit, final-review and session check runs all ask through this
// function and use its selection and fallback rules.
func Select(ctx context.Context, j *jev.Judge, repository *trace.Repository, scope coreadapter.Scope, cause string, sources []jev.Source, dir, diff string, checks ReviewChecks) (selection api.CheckSelection) {
	if j.Status().Mode == jev.ModeDisabled {
		return FullSelection(string(jev.ReasonDisabled), "")
	}
	links, err := checks.List(ctx, dir)
	if err != nil {
		return FullSelection(SelectionUnlisted, err.Error())
	}
	listed := len(links)
	defer func() { selection.Listed = &listed }()
	if len(links) == 0 {
		return FullSelection(SelectionNoChecks, "the project lists no check links")
	}
	candidates, ok := checkselect.Candidates(links, checkselect.MaxQuestions)
	if !ok {
		return FullSelection(SelectionTooMany, fmt.Sprintf("%d check links do not narrow to %d", len(links), checkselect.MaxQuestions))
	}
	state, ok := checkselect.State(diff)
	if !ok {
		return FullSelection(SelectionTooLarge, "the changed files do not fit a judgment")
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
		s := FullSelection(string(decision.Reason), decision.Detail)
		s.Judgment = decision.ID
		s.Candidates = len(candidates)
		return s
	}
	return api.CheckSelection{Mode: SelectionSelected, Links: checkselect.Selected(*decision.Response, candidates, checkselect.Threshold), Candidates: len(candidates), Judgment: decision.ID}
}
