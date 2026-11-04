package checks

import (
	"context"
	"fmt"
	"slices"
	"strings"

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
	SelectionRefused  = "refused"
)

// The modes of a check selection.
const (
	SelectionSelected = "selected"
	SelectionFull     = "full"
)

// DescribeSelection says in a sentence which checks ran.
func DescribeSelection(s api.CheckSelection) string {
	if s.Mode == SelectionSelected {
		if s.FallbackPicks > 0 {
			return fmt.Sprintf("no check reached the threshold; the %d highest-scored checks ran out of %d candidate links in judgment %s", len(s.Links), s.Candidates, s.Judgment)
		}
		return fmt.Sprintf("Jev selected %d of %d check links for the changed files in judgment %s", len(s.Links), s.Candidates, s.Judgment)
	}
	if s.Reason == string(jev.ReasonDisabled) {
		return "every check ran: the Jev boost is off"
	}
	if s.Reason == SelectionRefused {
		return "every check ran: Dagger refused the links Jev selected (" + s.Detail + ")"
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

// RunSelection runs selection's checks in the export at dir and returns the
// result with the selection that produced it. Selected links Dagger refuses
// before any check starts run every check instead, under the same ctx, and the
// returned selection says why. Output holds the refused run above the full one,
// and Flaky lists the flaky checks of both.
func RunSelection(ctx context.Context, checks ReviewChecks, dir string, identity Identity, selection api.CheckSelection) (CheckResult, api.CheckSelection, error) {
	refused, err := checks.Check(ctx, dir, identity, selection.Links)
	if err != nil || refused.Refusal == "" || len(selection.Links) == 0 {
		return refused, selection, err
	}
	full := FullSelection(SelectionRefused, refused.Refusal)
	full.Candidates, full.Listed, full.Judgment = selection.Candidates, selection.Listed, selection.Judgment
	result, err := checks.Check(ctx, dir, identity, nil)
	result.Output = fmt.Sprintf("%s\nDagger refused the selected links, so every check runs.\n$ %s\n%s", refused.Output, strings.Join(Command(nil), " "), result.Output)
	result.Truncated = result.Truncated || refused.Truncated
	for _, link := range refused.Flaky {
		if !slices.Contains(result.Flaky, link) {
			result.Flaky = append(result.Flaky, link)
		}
	}
	if result.TraceURL == "" {
		result.TraceURL = refused.TraceURL
	}
	return result, full, err
}

// Select is the service's smart-check selection: it asks Jev which
// of the listed checks a change, described by diff from a disposable export
// at dir, can affect, scoped to scope and keyed by cause for reuse across a
// retry. Every outcome but an accepted judgment runs every check. Listing
// waits for the project's modules to load, so it is skipped while the boost
// is off. Unit, final-review and session check runs all ask through this
// function and use its selection and fallback rules.
func Select(ctx context.Context, j *jev.Judge, repository *trace.Repository, scope coreadapter.Scope, cause string, sources []jev.Source, dir, diff string, identity Identity, checks ReviewChecks) (selection api.CheckSelection) {
	if j.Status().Mode == jev.ModeDisabled {
		return FullSelection(string(jev.ReasonDisabled), "")
	}
	links, err := checks.List(ctx, dir, identity)
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
	settings := j.Config()
	threshold, picks := settings.CheckThreshold, settings.CheckFallbackPicks
	acceptance := fmt.Sprintf("threshold=%g", threshold)
	if picks > 0 {
		acceptance += fmt.Sprintf(" fallback_picks=%d", picks)
	}
	decision := j.Evaluate(ctx, repository, jev.Judgment{
		Scope:      scope,
		Cause:      cause,
		Depth:      1,
		Task:       checkselect.Task,
		Version:    checkselect.Version,
		Acceptance: acceptance,
		Sources:    sources,
		Request:    checkselect.Request(state, candidates),
		Accept: func(r systemone.Response) string {
			if len(checkselect.Selected(r, candidates, threshold)) == 0 && len(checkselect.FallbackPicks(r, candidates, picks)) == 0 {
				return fmt.Sprintf("no check reached probability %.2f", threshold)
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
	selected := checkselect.Selected(*decision.Response, candidates, threshold)
	selection = api.CheckSelection{Mode: SelectionSelected, Links: selected, Candidates: len(candidates), Judgment: decision.ID}
	if len(selected) == 0 {
		selection.Links = checkselect.FallbackPicks(*decision.Response, candidates, picks)
		selection.FallbackPicks = picks
	}
	return selection
}
