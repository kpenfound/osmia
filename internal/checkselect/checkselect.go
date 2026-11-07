// Package checkselect builds the Jev judgments that select which of a
// project's Dagger checks a change can affect, and reads their answers. It asks
// nothing itself: callers send the requests and run the checks.
package checkselect

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/gitdiff"
)

// Task names the judgment. Bump Version whenever its questions or their
// interpretation change. The threshold and fallback picks are configured;
// each judgment's identity includes the acceptance values it applied.
const (
	Task    = "check-selection"
	Version = 4
)

// MaxQuestions keeps requests within the Jev timeout and per-judgment cost;
// it is not an API limit. Larger lists are narrowed by Candidates.
const MaxQuestions = 1024

// MaxState bounds the judgment's state in bytes, keeping it and a question
// within Jev's context. The diff is included only when it fits.
const MaxState = 72 * 1024

// Links reads the check links from the output of
// dagger list checks [--all] --format=link.
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
const linkGuide = "Each question names a Dagger check by its link: dag+check://<path>?<filters> or dag+check://?check=<name>&<filters>. " +
	"The path or check name says what the check is, such as a module's tests or a check that generated files are up to date. " +
	"Filters narrow a collection to some of its items; for Go, go-package names a package directory and go-test a test function. " +
	"A link covers every item its filters leave; a link without item filters names a standalone check with nothing narrower listed. " +
	"A change can alter a check's result when the check builds or exercises a changed file, or code that depends on one through imports, build configuration or generated files. " +
	"A collection's filters nest in the order they appear in its links. Each level's items contain the items of the levels after it. For Go, `go-module` (when listed) contains `go-package`, which names a package directory, which contains `go-test`, a test function."

// State is shared by all three stages: the link guide, process, changed files
// with their line counts, and diff when it fits within MaxState. It reports
// false when the state without the diff does not fit.
func State(diff string) (map[string]any, bool) {
	state := map[string]any{"checks": linkGuide, "process": process, "changed_files": gitdiff.ChangedFiles(diff)}
	size := func() int { data, _ := json.Marshal(state); return len(data) }
	if size() > MaxState {
		return nil, false
	}
	state["diff"] = diff
	if size() > MaxState {
		state["diff"] = "omitted: the diff is too large; judge from the changed files"
		if size() > MaxState {
			return nil, false
		}
	}
	return state, true
}

// Request asks, of state, whether the change can alter each candidate's
// result. Candidate i is asked as Question(i).
func Request(state map[string]any, candidates []string) systemone.Request {
	request := systemone.Request{State: state, Questions: map[string]systemone.Question{}}
	for i, link := range candidates {
		q := systemone.Noul(map[string]any{"stage": fmt.Sprintf(rankingStage, link), "link": link})
		q.Yes, q.No = rankingYes, rankingNo
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

// FallbackPicks returns up to count scored candidates in descending probability
// order. Equal scores retain candidate order; missing and non-Noul answers do
// not participate. There is no minimum probability for a pick.
func FallbackPicks(r systemone.Response, candidates []string, count int) []string {
	if count <= 0 {
		return nil
	}
	var order []int
	for i := range candidates {
		if a, ok := r.Answers[Question(i)]; ok && a.Kind == systemone.KindNoul {
			order = append(order, i)
		}
	}
	slices.SortStableFunc(order, func(a, b int) int {
		return cmp.Compare(r.Answers[Question(b)].Noul, r.Answers[Question(a)].Noul)
	})
	var picks []string
	for _, i := range order[:min(count, len(order))] {
		picks = append(picks, candidates[i])
	}
	return picks
}

// Candidates excludes collection roots and links holding the same checks as
// a root, while retaining links with nothing narrower listed. Coverage is
// measured against the original listing's leaves, including during narrowing.
// It returns the distinct remaining links, narrowed to at most limit by
// standing a collection item's link in for the links within it: the link
// with its last filter removed covers every link that differs from it only
// in that filter. Each step collapses the item that removes the most links,
// so the largest collections are judged by item and small ones keep their
// own links. No step creates a broad link. It reports false when the links
// cannot be narrowed to limit without one.
func Candidates(links []string, limit int) ([]string, bool) {
	return Narrow(links, links, limit)
}

// Narrow narrows filtered links using coverage against the original expanded
// listing. Dropping another collection or item must not turn a kept link into
// a whole-collection link. A false result is the too_many_checks fallback.
func Narrow(filtered, listed []string, limit int) ([]string, bool) {
	coverage := listedCoverage(listed)
	set := map[string]bool{}
	for _, l := range filtered {
		if !coverage.broad(l) {
			set[l] = true
		}
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
			if coverage.broad(parent) {
				continue
			}
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
