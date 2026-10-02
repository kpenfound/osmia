// Package checkselect builds the Jev judgment that selects which of a
// project's Dagger checks a change can affect, and reads its answers. It asks
// nothing itself: callers send the request and own the fallback of running
// every check.
package checkselect

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/gitdiff"
	"github.com/kpenfound/osmia/internal/systemone"
)

// Task names the judgment. Bump Version whenever its questions, their
// interpretation or Threshold change.
const (
	Task    = "check-selection"
	Version = 1
)

// Threshold is the probability at or above which a check is selected. It
// favours running a check that may be unaffected over missing one that is.
const Threshold = 0.3

// MaxQuestions bounds the checks asked about in one judgment; a project with
// more is asked about its collections' items, collapsed as Candidates
// describes.
const MaxQuestions = 128

// MaxState bounds the judgment's state in bytes, keeping it and a question
// within Jev's context. The diff is included only when it fits.
const MaxState = 72 * 1024

// Links reads the check links from the output of
// dagger list checks --all --format=link.
func Links(output string) []string {
	var links []string
	for line := range strings.Lines(output) {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "dag+check://") {
			links = append(links, line)
		}
	}
	return links
}

// linkGuide tells Jev how to read a check link.
const linkGuide = "Each question names a Dagger check by its link: dag+check://<module>/<collection path>?<filters>. " +
	"The module and path say what the check is, such as a module's tests or a check that generated files are up to date. " +
	"Filters narrow a collection to some of its items; for Go, go-package names a package directory and go-test a test function. " +
	"A link covers every item its filters leave, so a link without filters covers its whole collection. " +
	"A change can alter a check's result when the check builds or exercises a changed file, or code that depends on one through imports, build configuration or generated files."

// State is the judgment's state: how to read a check link, the changed files
// with their line counts, and the diff when it fits within MaxState. It
// reports false when the changed files alone do not fit.
func State(diff string) (map[string]any, bool) {
	state := map[string]any{"checks": linkGuide, "changed_files": gitdiff.ChangedFiles(diff)}
	size := func() int { data, _ := json.Marshal(state); return len(data) }
	if size() > MaxState {
		return nil, false
	}
	state["diff"] = diff
	if size() > MaxState {
		state["diff"] = "omitted: the diff is too large; judge from the changed files"
	}
	return state, true
}

// Request asks, of state, whether the change can alter each candidate's
// result. Candidate i is asked as Question(i).
func Request(state map[string]any, candidates []string) systemone.Request {
	request := systemone.Request{State: state, Questions: map[string]systemone.Question{}}
	for i, link := range candidates {
		q := systemone.Noul(fmt.Sprintf("Could the change in the state alter the result of the Dagger check %s?", link))
		q.Yes = "The check exercises a changed file, or code, configuration or generated files the change affects."
		q.No = "Nothing the check depends on is touched by the change."
		request.Questions[Question(i)] = q
	}
	return request
}

// Question is the ID of the question about candidate i.
func Question(i int) string { return fmt.Sprintf("check-%d", i) }

// Selected returns the candidates whose answer reaches threshold.
func Selected(r systemone.Response, candidates []string, threshold float64) []string {
	var selected []string
	for i, link := range candidates {
		if a, ok := r.Answers[Question(i)]; ok && a.Kind == systemone.KindNoul && a.Noul >= threshold {
			selected = append(selected, link)
		}
	}
	return selected
}

// Candidates returns the distinct links, narrowed to at most limit by
// standing a collection item's link in for the links within it: the link
// with its last filter removed covers every link that differs from it only
// in that filter. Each step collapses the item that removes the most links,
// so the largest collections are judged by item and small ones keep their
// own links. It reports false when the links cannot be narrowed to limit.
func Candidates(links []string, limit int) ([]string, bool) {
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
