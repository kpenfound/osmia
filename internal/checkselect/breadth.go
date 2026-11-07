package checkselect

import (
	"fmt"
	"slices"

	"github.com/kpenfound/agentcore/jev/systemone"
)

const (
	BreadthTask    = "check-breadth"
	BreadthVersion = 1
)

// Root is a Dagger-listed top-level check and the non-broad expanded links it
// covers. Collection distinguishes a collection from a standalone check even
// when the collection expands to only one item.
type Root struct {
	Link       string
	Links      []string
	Collection bool
}

// BreadthPlan attributes expanded links to Dagger's top-level checks. Roots,
// their links and Uncovered are sorted and distinct.
type BreadthPlan struct {
	Roots     []Root
	Uncovered []string
	listed    []string
}

// Breadth maps each non-broad expanded link to its most specific covering
// top-level check, using lexical order to break ties. Uncovered links bypass
// breadth and always reach ranking. It never invents top-level checks.
func Breadth(topLevel, expanded []string) BreadthPlan {
	plan := BreadthPlan{listed: slices.Clone(expanded)}
	for _, link := range distinct(topLevel) {
		plan.Roots = append(plan.Roots, Root{Link: link})
	}
	links, _ := Candidates(expanded, len(expanded))
	for _, link := range links {
		a := parseAddress(link)
		owner := -1
		for i, root := range plan.Roots {
			r := parseAddress(root.Link)
			if !r.covers(a) {
				continue
			}
			if owner < 0 || parseAddress(plan.Roots[owner].Link).covers(r) && !r.covers(parseAddress(plan.Roots[owner].Link)) {
				owner = i
			}
		}
		if owner < 0 {
			plan.Uncovered = append(plan.Uncovered, link)
			continue
		}
		root := &plan.Roots[owner]
		root.Links = append(root.Links, link)
		root.Collection = root.Collection || !a.covers(parseAddress(root.Link))
	}
	return plan
}

// BreadthQuestion is the question ID for Roots[i].
func BreadthQuestion(i int) string { return fmt.Sprintf("breadth-%d", i) }

// BreadthRequest builds check-breadth v1. An empty plan needs no judgment.
func BreadthRequest(state map[string]any, plan BreadthPlan) systemone.Request {
	r := systemone.Request{State: state, Questions: map[string]systemone.Question{}}
	for i, root := range plan.Roots {
		instructions := map[string]any{"stage": fmt.Sprintf(breadthStage, root.Link), "link": root.Link}
		if root.Collection {
			instructions["items"] = len(root.Links)
			instructions["examples"] = slices.Clone(root.Links[:min(3, len(root.Links))])
		}
		q := systemone.Noul(instructions)
		q.Yes, q.No = breadthYes, breadthNo
		r.Questions[BreadthQuestion(i)] = q
	}
	return r
}

// BreadthResult keeps collections for depth and standalone and uncovered links
// for ranking. KeptRoots includes both kept collections and standalone checks.
type BreadthResult struct {
	KeptRoots   []string
	Collections []Root
	Ranking     []string
	listed      []string
}

// ApplyBreadth applies the threshold, retaining all roots when none reaches
// it. A missing, extra, wrong-kind or invalid probability answer is unusable;
// false tells the caller to restore the full candidate path.
func ApplyBreadth(r systemone.Response, plan BreadthPlan, threshold float64) (BreadthResult, bool) {
	if !usableNouls(r, len(plan.Roots), BreadthQuestion) {
		return BreadthResult{}, false
	}
	keepAll := true
	for i := range plan.Roots {
		if r.Answers[BreadthQuestion(i)].Noul >= threshold {
			keepAll = false
			break
		}
	}
	result := BreadthResult{Ranking: slices.Clone(plan.Uncovered), listed: plan.listed}
	for i, root := range plan.Roots {
		if !keepAll && r.Answers[BreadthQuestion(i)].Noul < threshold {
			continue
		}
		result.KeptRoots = append(result.KeptRoots, root.Link)
		if root.Collection {
			result.Collections = append(result.Collections, root)
		} else {
			result.Ranking = append(result.Ranking, root.Links...)
		}
	}
	result.Ranking = distinct(result.Ranking)
	return result, true
}

func distinct(links []string) []string {
	out := slices.Clone(links)
	slices.Sort(out)
	return slices.Compact(out)
}
