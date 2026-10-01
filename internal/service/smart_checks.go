package service

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/systemone"
)

// checkSelectionTask is the Jev judgment that selects the checks a unit's
// candidate runs before review. Bump checkSelectionVersion whenever its
// questions, their interpretation or selectThreshold change.
const (
	checkSelectionTask    = "check-selection"
	checkSelectionVersion = 1
)

// selectThreshold is the probability at or above which a check is selected.
// It favours running a check that may be unaffected over missing one that is.
const selectThreshold = 0.3

// maxCheckQuestions bounds the checks asked about in one judgment; a project
// with more is asked about its collections' items, collapsed as
// checkCandidates describes.
const maxCheckQuestions = 128

// maxCheckState bounds the judgment's state in bytes, keeping it and a
// question within Jev's context. The diff is included only when it fits.
const maxCheckState = 72 * 1024

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
// every check, because of Reason, with Detail.
type CheckSelection struct {
	Mode       string   `json:"mode"`
	Links      []string `json:"links,omitempty"`
	Candidates int      `json:"candidates,omitempty"`
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
	if c.s.jev.Status().Mode == jev.ModeDisabled {
		return fullSelection(string(jev.ReasonDisabled), "")
	}
	links, err := checks.List(ctx, dir)
	if err != nil {
		return fullSelection(selectionUnlisted, err.Error())
	}
	if len(links) == 0 {
		return fullSelection(selectionNoChecks, "the project lists no check links")
	}
	candidates, ok := checkCandidates(links, maxCheckQuestions)
	if !ok {
		return fullSelection(selectionTooMany, fmt.Sprintf("%d check links do not narrow to %d", len(links), maxCheckQuestions))
	}
	state, ok := checkSelectionState(diff)
	if !ok {
		return fullSelection(selectionTooLarge, "the changed files do not fit a judgment")
	}
	request := systemone.Request{State: state, Questions: map[string]systemone.Question{}}
	for i, link := range candidates {
		q := systemone.Noul(fmt.Sprintf("Could the change in the state alter the result of the Dagger check %s?", link))
		q.Yes = "The check exercises a changed file, or code, configuration or generated files the change affects."
		q.No = "Nothing the check depends on is touched by the change."
		request.Questions[checkQuestion(i)] = q
	}
	decision := c.s.jev.Evaluate(ctx, c.repository, jev.Judgment{
		Scope:   c.scope(in),
		Cause:   checkRequestID(in.Unit, in.Run),
		Depth:   1,
		Task:    checkSelectionTask,
		Version: checkSelectionVersion,
		Sources: []jev.Source{{Kind: "unit-report", ID: reportDocument(in.Unit), Revision: in.Report}},
		Request: request,
		Accept: func(r systemone.Response) string {
			if len(selectedChecks(r, candidates)) == 0 {
				return fmt.Sprintf("no check reached probability %.2f", selectThreshold)
			}
			return ""
		},
	})
	if decision.Outcome != jev.Accepted {
		s := fullSelection(string(decision.Reason), decision.Detail)
		s.Judgment = decision.ID
		return s
	}
	return CheckSelection{Mode: selectionSelected, Links: selectedChecks(*decision.Response, candidates), Candidates: len(candidates), Judgment: decision.ID}
}

func checkQuestion(i int) string { return fmt.Sprintf("check-%d", i) }

// selectedChecks returns the candidates whose answer reaches selectThreshold.
func selectedChecks(r systemone.Response, candidates []string) []string {
	var selected []string
	for i, link := range candidates {
		if a, ok := r.Answers[checkQuestion(i)]; ok && a.Kind == systemone.KindNoul && a.Noul >= selectThreshold {
			selected = append(selected, link)
		}
	}
	return selected
}

// checkLinkGuide tells Jev how to read a check link.
const checkLinkGuide = "Each question names a Dagger check by its link: dag+check://<module>/<collection path>?<filters>. " +
	"The module and path say what the check is, such as a module's tests or a check that generated files are up to date. " +
	"Filters narrow a collection to some of its items; for Go, go-package names a package directory and go-test a test function. " +
	"A link covers every item its filters leave, so a link without filters covers its whole collection. " +
	"A change can alter a check's result when the check builds or exercises a changed file, or code that depends on one through imports, build configuration or generated files."

// checkSelectionState is the judgment's state: how to read a check link, the
// changed files with their line counts, and the diff when it fits within
// maxCheckState. It reports false when the changed files alone do not fit.
func checkSelectionState(diff string) (map[string]any, bool) {
	state := map[string]any{"checks": checkLinkGuide, "changed_files": changedFiles(diff)}
	size := func() int { data, _ := json.Marshal(state); return len(data) }
	if size() > maxCheckState {
		return nil, false
	}
	state["diff"] = diff
	if size() > maxCheckState {
		state["diff"] = "omitted: the diff is too large; judge from the changed files"
	}
	return state, true
}

// checkCandidates returns the distinct links, narrowed to at most limit by
// standing a collection item's link in for the links within it: the link
// with its last filter removed covers every link that differs from it only
// in that filter. Each step collapses the item that removes the most links,
// so the largest collections are judged by item and small ones keep their
// own links. It reports false when the links cannot be narrowed to limit.
func checkCandidates(links []string, limit int) ([]string, bool) {
	set := map[string]bool{}
	for _, l := range links {
		set[l] = true
	}
	for len(set) > limit {
		within := map[string][]string{}
		for l := range set {
			if parent, ok := parentLink(l); ok {
				within[parent] = append(within[parent], l)
			}
		}
		best, saved := "", 0
		for parent, children := range within {
			n := len(children)
			if !set[parent] {
				n--
			}
			if n > saved || n == saved && n > 0 && parent < best {
				best, saved = parent, n
			}
		}
		if saved == 0 {
			return nil, false
		}
		for _, l := range within[best] {
			delete(set, l)
		}
		set[best] = true
	}
	out := make([]string, 0, len(set))
	for l := range set {
		out = append(out, l)
	}
	slices.Sort(out)
	return out, true
}

// parentLink returns link without its last filter, and false for a link
// without filters.
func parentLink(link string) (string, bool) {
	base, query, found := strings.Cut(link, "?")
	if !found || query == "" {
		return "", false
	}
	i := strings.LastIndex(query, "&")
	if i < 0 {
		return base, true
	}
	return base + "?" + query[:i], true
}
