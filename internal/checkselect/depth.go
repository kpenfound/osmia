package checkselect

import (
	"fmt"
	"slices"

	"github.com/kpenfound/agentcore/jev/systemone"
)

const (
	DepthTask    = "check-depth"
	DepthVersion = 1
)

// Level describes one level in a collection's hierarchy. Offered is true or
// a string explaining why the level cannot be chosen.
type Level struct {
	Level    string `json:"level"`
	Links    int    `json:"links"`
	Example  string `json:"example"`
	Contains string `json:"contains"`
	Offered  any    `json:"offered"`
	links    []string
}

// CollectionDepth is a kept collection's coarsest-first hierarchy and original
// finest links. Question is empty when no Choice is needed; an inconsistent
// filter order leaves Hierarchy empty and uses Finest unchanged.
type CollectionDepth struct {
	Root      string
	Hierarchy []Level
	Finest    []string
	Question  string
}

// DepthPlan describes the collections retained by breadth and the links that
// bypass depth. It can build a request or supply the depth-failure path.
type DepthPlan struct {
	Collections []CollectionDepth
	Ranking     []string
}

// Depth derives filter levels without choosing a granularity. Broadness is
// measured against the original expanded listing, including dropped checks.
func Depth(breadth BreadthResult) DepthPlan {
	p := DepthPlan{Ranking: slices.Clone(breadth.Ranking)}
	coverage := listedCoverage(breadth.listed)
	for _, root := range breadth.Collections {
		c := CollectionDepth{Root: root.Link, Finest: finest(root.Links)}
		keys, consistent := filterOrder(c.Finest)
		// Intermediate listed items must agree with the leaves' filter order.
		for _, link := range root.Links {
			order := itemFilters(parseAddress(link))
			if len(order) > len(keys) || !slices.Equal(order, keys[:min(len(order), len(keys))]) {
				consistent = false
			}
		}
		if consistent {
			for k, key := range keys {
				links := cutLinks(c.Finest, k+1)
				if k == len(keys)-1 {
					links = slices.Clone(c.Finest)
				}
				level := Level{Level: key, Links: len(links), Example: links[0], Contains: "none", Offered: true, links: links}
				if k+1 < len(keys) {
					level.Contains = keys[k+1]
				}
				if slices.ContainsFunc(links, coverage.broad) {
					level.Offered = broadReason
				}
				c.Hierarchy = append(c.Hierarchy, level)
			}
		}
		if len(offered(c)) >= 2 {
			c.Question = DepthQuestion(len(p.Collections))
		}
		p.Collections = append(p.Collections, c)
	}
	return p
}

// finest retains leaves only, including opaque links whose coverage is unknown.
func finest(links []string) []string {
	var out []string
	for _, link := range links {
		a := parseAddress(link)
		if a.opaque || !slices.ContainsFunc(links, func(other string) bool {
			b := parseAddress(other)
			return !b.opaque && a.covers(b) && !b.covers(a)
		}) {
			out = append(out, link)
		}
	}
	return distinct(out)
}

// filterOrder requires every leaf to name the same filters in the same order.
// The check-name filter identifies the collection and is not an item level.
func filterOrder(links []string) ([]string, bool) {
	var keys []string
	for i, link := range links {
		a := parseAddress(link)
		if a.opaque {
			return nil, false
		}
		order := itemFilters(a)
		if i == 0 {
			keys = order
		} else if !slices.Equal(keys, order) {
			return nil, false
		}
	}
	return keys, len(keys) > 0
}

func itemFilters(a *address) []string {
	var keys []string
	for _, f := range a.filters {
		if f.key != "check" {
			keys = append(keys, f.key)
		}
	}
	return keys
}

func cutLinks(links []string, n int) []string {
	var out []string
	for _, link := range links {
		a := parseAddress(link)
		cut := &address{base: a.base}
		items := 0
		for _, f := range a.filters {
			if f.key == "check" || items < n {
				cut.filters = append(cut.filters, f)
			}
			if f.key != "check" {
				items++
			}
		}
		out = append(out, cut.String())
	}
	return distinct(out)
}

func offered(c CollectionDepth) []Level {
	var levels []Level
	for _, l := range c.Hierarchy {
		if l.Offered == true {
			levels = append(levels, l)
		}
	}
	return levels
}

// DepthQuestion is the question ID for Collections[i], when it needs a Choice.
func DepthQuestion(i int) string { return fmt.Sprintf("depth-%d", i) }

// DepthRequest builds check-depth v1. When Questions is empty, callers apply
// an empty response without sending a judgment.
func DepthRequest(state map[string]any, plan DepthPlan) systemone.Request {
	r := systemone.Request{State: state, Questions: map[string]systemone.Question{}}
	for _, c := range plan.Collections {
		if c.Question == "" {
			continue
		}
		var options []systemone.Option
		for _, l := range offered(c) {
			description := fmt.Sprintf(depthOption, l.Links, l.Level, l.Example)
			if l.Contains != "none" {
				description += fmt.Sprintf(depthContains, l.Contains)
			}
			options = append(options, systemone.Option{Name: l.Level, Description: description})
		}
		r.Questions[c.Question] = systemone.Choice(map[string]any{
			"stage": fmt.Sprintf(depthStage, c.Root), "hierarchy": c.Hierarchy, "guidance": depthGuidance,
		}, options...)
	}
	return r
}

// DepthResult is the filtered ranking list and chosen level per collection.
// "finest" labels a collection without a consistent named hierarchy.
type DepthResult struct {
	Links  []string
	Levels map[string]string
}

// Finest is the depth-failure path: every leaf of breadth-kept collections,
// kept standalone checks and uncovered links. Dropped roots stay dropped.
func Finest(plan DepthPlan) DepthResult {
	r := DepthResult{Links: slices.Clone(plan.Ranking), Levels: map[string]string{}}
	for _, c := range plan.Collections {
		r.Links = append(r.Links, c.Finest...)
		r.Levels[c.Root] = "finest"
		if len(c.Hierarchy) > 0 {
			r.Levels[c.Root] = c.Hierarchy[len(c.Hierarchy)-1].Level
		}
	}
	r.Links = distinct(r.Links)
	return r
}

// ApplyDepth uses the returned Choice as given, with no probability threshold.
// Missing, extra, wrong-kind and unoffered choices are unusable. Distributions
// are validated by systemone's transport. On false the caller uses Finest.
func ApplyDepth(response systemone.Response, plan DepthPlan) (DepthResult, bool) {
	r := DepthResult{Links: slices.Clone(plan.Ranking), Levels: map[string]string{}}
	questions := 0
	for _, c := range plan.Collections {
		levels := offered(c)
		chosen := -1
		if c.Question != "" {
			questions++
			a, ok := response.Answers[c.Question]
			if !ok || a.Kind != systemone.KindChoice {
				return DepthResult{}, false
			}
			chosen = slices.IndexFunc(levels, func(l Level) bool { return l.Level == a.Choice })
			if chosen < 0 {
				return DepthResult{}, false
			}
		} else if len(levels) == 1 {
			chosen = 0
		}
		if chosen >= 0 {
			r.Links = append(r.Links, levels[chosen].links...)
			r.Levels[c.Root] = levels[chosen].Level
		} else {
			r.Links = append(r.Links, c.Finest...)
			r.Levels[c.Root] = "finest"
		}
	}
	if len(response.Answers) != questions {
		return DepthResult{}, false
	}
	r.Links = distinct(r.Links)
	return r, true
}
