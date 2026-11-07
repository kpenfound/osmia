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
	clause := ""
	if s.Roots != nil {
		clause = fmt.Sprintf(" within %d of %d top-level checks", len(s.KeptRoots), *s.Roots)
		var levels []string
		for _, level := range s.Levels {
			levels = append(levels, level)
		}
		slices.Sort(levels)
		levels = slices.Compact(levels)
		if len(levels) > 0 {
			clause += ", by " + strings.Join(levels, ", ")
		}
	}
	if s.Mode == SelectionSelected {
		if s.FallbackPicks > 0 {
			return fmt.Sprintf("no check reached the threshold; the %d highest-scored checks ran out of %d candidate links in judgment %s", len(s.Links), s.Candidates, s.Judgment) + clause
		}
		return fmt.Sprintf("Jev selected %d of %d check links for the changed files in judgment %s", len(s.Links), s.Candidates, s.Judgment) + clause
	}
	if clause != "" {
		clause = "; ranking considered links" + clause
	}
	if s.Reason == string(jev.ReasonDisabled) {
		return "every check ran: the Jev boost is off" + clause
	}
	if s.Reason == SelectionRefused {
		return "every check ran: Dagger refused the links Jev selected (" + s.Detail + ")" + clause
	}
	why := s.Reason
	if s.Detail != "" {
		why += ": " + s.Detail
	}
	return "every check ran: Jev did not select them (" + why + ")" + clause
}

// FullSelection runs every check for reason.
func FullSelection(reason, detail string) api.CheckSelection {
	return api.CheckSelection{Mode: SelectionFull, Reason: reason, Detail: detail}
}

// RunSelection runs selection's checks in the export at dir and returns the
// result with the selection that produced it. Selected links Dagger refuses
// before that command starts any check run every check instead, under the same
// ctx, and the returned selection says why. Output holds the refused run above the full one,
// and Flaky lists the flaky checks of both.
func RunSelection(ctx context.Context, checks ReviewChecks, dir string, identity Identity, selection api.CheckSelection) (CheckResult, api.CheckSelection, error) {
	refused, err := checks.Check(ctx, dir, identity, selection.Links)
	if err != nil || refused.Refusal == "" || len(selection.Links) == 0 {
		return refused, selection, err
	}
	full := FullSelection(SelectionRefused, refused.Refusal)
	full.Candidates, full.Listed, full.Judgment = selection.Candidates, selection.Listed, selection.Judgment
	full.BreadthJudgment, full.DepthJudgment = selection.BreadthJudgment, selection.DepthJudgment
	full.Roots, full.KeptRoots, full.Levels = selection.Roots, selection.KeptRoots, selection.Levels
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
// retry. Breadth and depth failures retain the earlier stages; a ranking
// failure runs every check. Listing waits for the project's modules to load, so it is skipped while the boost
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
	state, ok := checkselect.State(diff)
	if !ok {
		return FullSelection(SelectionTooLarge, "the changed files do not fit a judgment")
	}
	settings := j.Config()
	threshold, picks := settings.CheckThreshold, settings.CheckFallbackPicks
	filtered, prefilter := prefilterChecks(ctx, j, repository, scope, cause, sources, dir, identity, checks, links, state, threshold)
	defer func() {
		selection.BreadthJudgment, selection.DepthJudgment = prefilter.BreadthJudgment, prefilter.DepthJudgment
		selection.Roots, selection.KeptRoots, selection.Levels = prefilter.Roots, prefilter.KeptRoots, prefilter.Levels
	}()
	candidates, ok := checkselect.Narrow(filtered, links, checkselect.MaxQuestions)
	if !ok {
		return FullSelection(SelectionTooMany, fmt.Sprintf("%d check links do not narrow to %d", len(filtered), checkselect.MaxQuestions))
	}
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

// prefilterChecks falls back at the failed stage, retaining every earlier answer.
func prefilterChecks(ctx context.Context, j *jev.Judge, repository *trace.Repository, scope coreadapter.Scope, cause string, sources []jev.Source, dir string, identity Identity, checks ReviewChecks, links []string, state map[string]any, threshold float64) ([]string, api.CheckSelection) {
	var evidence api.CheckSelection
	top, ok := checks.(TopLevelChecks)
	if !ok {
		return links, evidence
	}
	scoped, ok := checks.(ScopedChecks)
	if !ok {
		return links, evidence
	}
	roots, err := top.ListTopLevel(ctx, dir, identity)
	if err != nil {
		return links, evidence
	}
	members := map[string][]string{}
	for _, root := range roots {
		if _, exists := members[root]; exists {
			continue
		}
		members[root], err = scoped.ListScoped(ctx, dir, identity, root)
		if err != nil {
			return links, evidence
		}
	}
	plan, err := checkselect.BreadthScoped(roots, links, members)
	if err != nil || len(plan.Roots) == 0 {
		return links, evidence
	}
	breadth := j.Evaluate(ctx, repository, jev.Judgment{
		Scope: scope, Cause: cause, Depth: 1, Sources: sources,
		Task: checkselect.BreadthTask, Version: checkselect.BreadthVersion,
		Acceptance: fmt.Sprintf("threshold=%g", threshold),
		Request:    checkselect.BreadthRequest(state, plan),
		Accept: func(r systemone.Response) string {
			if _, ok := checkselect.ApplyBreadth(r, plan, threshold); !ok {
				return "unusable breadth answer"
			}
			return ""
		},
	})
	evidence.BreadthJudgment = breadth.ID
	if breadth.Outcome != jev.Accepted {
		return links, evidence
	}
	kept, _ := checkselect.ApplyBreadth(*breadth.Response, plan, threshold)
	count := len(plan.Roots)
	evidence.Roots, evidence.KeptRoots = &count, kept.KeptRoots
	depthPlan := checkselect.Depth(kept)
	request := checkselect.DepthRequest(state, depthPlan)
	result := checkselect.Finest(depthPlan)
	if len(request.Questions) > 0 {
		depth := j.Evaluate(ctx, repository, jev.Judgment{
			Scope: scope, Cause: cause, Depth: 1, Sources: sources,
			Task: checkselect.DepthTask, Version: checkselect.DepthVersion, Request: request,
			Accept: func(r systemone.Response) string {
				if _, ok := checkselect.ApplyDepth(r, depthPlan); !ok {
					return "unusable depth answer"
				}
				return ""
			},
		})
		evidence.DepthJudgment = depth.ID
		if depth.Outcome == jev.Accepted {
			result, _ = checkselect.ApplyDepth(*depth.Response, depthPlan)
		}
	} else {
		result, _ = checkselect.ApplyDepth(systemone.Response{}, depthPlan)
	}
	evidence.Levels = result.Levels
	return result.Links, evidence
}
