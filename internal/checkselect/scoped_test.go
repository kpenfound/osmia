package checkselect

import (
	"reflect"
	"slices"
	"testing"

	"github.com/kpenfound/agentcore/jev/systemone"
)

const (
	realTestRoot  = "dag+check://?check=go/modules/packages/tests/test"
	realStaleRoot = "dag+check://?check=go/modules/packages/generate/stale"
)

func realShapeMembership() map[string][]string {
	return map[string][]string{
		realTestRoot: goLinks("dag+check://?check=test"),
		realStaleRoot: {
			"dag+check://?check=stale&go-module=.&go-package=a",
			"dag+check://?check=stale&go-module=.&go-package=b",
		},
		"dag+check://?check=fixture/standalone":      {"dag+check://?check=fixture/standalone"},
		"dag+check://?check=dang-sdk/generate/stale": {"dag+check://?check=dang-sdk/generate/stale"},
	}
}

func scopedFixture(t *testing.T, membership map[string][]string) (BreadthPlan, []string) {
	t.Helper()
	var roots, expanded []string
	for root, links := range membership {
		roots = append(roots, root)
		expanded = append(expanded, links...)
	}
	p, err := BreadthScoped(roots, expanded, membership)
	if err != nil {
		t.Fatal(err)
	}
	return p, distinct(expanded)
}

func TestBreadthScopedRealDaggerShapes(t *testing.T) {
	t.Parallel()
	membership := realShapeMembership()
	p, expanded := scopedFixture(t, membership)
	r := BreadthRequest(nil, p)
	if len(r.Questions) != 4 || len(p.Uncovered) != 0 {
		t.Fatalf("questions %+v, uncovered %q", r.Questions, p.Uncovered)
	}
	var attributed []string
	for i, root := range p.Roots {
		want := membership[root.Link]
		collection := root.Link == realTestRoot || root.Link == realStaleRoot
		if root.Collection != collection || !slices.Equal(root.Links, want) {
			t.Fatalf("root %+v, want %q, collection %t", root, want, collection)
		}
		data := r.Questions[BreadthQuestion(i)].Instructions.(map[string]any)
		if collection {
			if data["items"] != len(want) || !reflect.DeepEqual(data["examples"], want[:min(3, len(want))]) {
				t.Fatalf("collection data %+v", data)
			}
		} else if data["items"] != nil || data["examples"] != nil {
			t.Fatalf("standalone data %+v", data)
		}
		attributed = append(attributed, root.Links...)
	}
	if !slices.Equal(distinct(attributed), expanded) || len(attributed) != len(expanded) {
		t.Fatalf("attributed %q, expanded %q", attributed, expanded)
	}
	// A collection remains a collection when it has only one expanded item.
	membership[realTestRoot] = membership[realTestRoot][:1]
	p, _ = scopedFixture(t, membership)
	for _, root := range p.Roots {
		if root.Link == realTestRoot && !root.Collection {
			t.Fatalf("single-item collection %+v", root)
		}
	}
}

func TestBreadthScopedAmbiguousAndUnmatchedReachRanking(t *testing.T) {
	t.Parallel()
	const (
		a          = "dag+check://?check=go/first/test"
		b          = "dag+check://?check=go/second/test"
		standalone = "dag+check://?check=fixture/standalone"
		ambiguous  = "dag+check://?check=test&go-module=.&go-package=a&go-test=Shared"
		unmatched  = "dag+check://?check=test&go-module=.&go-package=b&go-test=Unknown"
	)
	expanded := []string{ambiguous, unmatched, standalone}
	scoped := map[string][]string{a: {ambiguous, ambiguous}, b: {ambiguous}, standalone: {standalone}}
	// Membership outside the unexpanded root listing cannot invent a root.
	scoped["dag+check://?check=unlisted/root"] = []string{unmatched}
	p, err := BreadthScoped([]string{b, a, standalone, a}, expanded, scoped)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.Uncovered, []string{ambiguous, unmatched}) || len(BreadthRequest(nil, p).Questions) != 3 {
		t.Fatalf("attribution %+v", p)
	}
	// Dropping both roots that report the shared item must not drop that item.
	breadth, ok := ApplyBreadth(breadthAnswers(p, map[string]float64{standalone: .9}), p, .5)
	if !ok || !slices.Equal(breadth.KeptRoots, []string{standalone}) || len(breadth.Collections) != 0 {
		t.Fatalf("breadth %+v, usable %t", breadth, ok)
	}
	ranking := Finest(Depth(breadth)).Links
	if !slices.Equal(ranking, distinct(expanded)) || len(Request(nil, ranking).Questions) != len(expanded) {
		t.Fatalf("ranking %q, expanded %q", ranking, expanded)
	}
}

func TestBreadthScopedPreservesCoverageOwners(t *testing.T) {
	t.Parallel()
	const root = "dag+check://?check=test"
	const parent = root + "&go-package=a"
	leaves := []string{parent + "&go-test=One", root + "&go-package=b&go-test=Two"}
	roots := []string{root, parent}
	old := Breadth(roots, leaves)
	// Membership agrees with the most-specific covering root. Membership is
	// authoritative when multiple roots actually return the same link instead.
	p, err := BreadthScoped(roots, leaves, map[string][]string{root: {leaves[1]}, parent: {leaves[0]}})
	if err != nil || !reflect.DeepEqual(p, old) {
		t.Fatalf("scoped %+v, error %v; coverage %+v", p, err, old)
	}
}

func TestBreadthScopedRequiresEveryListing(t *testing.T) {
	t.Parallel()
	if _, err := BreadthScoped([]string{realTestRoot, realStaleRoot}, nil, map[string][]string{realTestRoot: nil}); err == nil {
		t.Fatal("missing scoped listing did not report breadth failure")
	}
	p, err := BreadthScoped([]string{realTestRoot}, nil, map[string][]string{realTestRoot: nil})
	if err != nil || len(p.Roots) != 1 || len(p.Roots[0].Links) != 0 {
		t.Fatalf("successful empty listing: %+v, %v", p, err)
	}
}

func TestDepthScopedGoHierarchyAndPackageCoverage(t *testing.T) {
	t.Parallel()
	p, _ := scopedFixture(t, realShapeMembership())
	breadth, ok := ApplyBreadth(breadthAnswers(p, map[string]float64{realTestRoot: .9}), p, .5)
	if !ok {
		t.Fatal("breadth failed")
	}
	depth := Depth(breadth)
	request := DepthRequest(nil, depth)
	if len(depth.Collections) != 1 || len(request.Questions) != 1 {
		t.Fatalf("depth %+v, questions %+v", depth, request.Questions)
	}
	c := depth.Collections[0]
	hierarchy := request.Questions[c.Question].Instructions.(map[string]any)["hierarchy"].([]Level)
	if len(hierarchy) != 3 || hierarchy[0].Level != "go-module" || hierarchy[0].Offered != broadReason || hierarchy[1].Level != "go-package" || hierarchy[1].Offered != true || hierarchy[2].Level != "go-test" || hierarchy[2].Offered != true {
		t.Fatalf("hierarchy %+v", hierarchy)
	}
	result, ok := ApplyDepth(systemone.Response{Answers: map[string]systemone.Answer{c.Question: {Kind: systemone.KindChoice, Choice: "go-package"}}}, depth)
	want := []string{"dag+check://?check=test&go-module=.&go-package=a", "dag+check://?check=test&go-module=.&go-package=b"}
	if !ok || !slices.Equal(result.Links, want) || result.Levels[realTestRoot] != "go-package" {
		t.Fatalf("depth result %+v, usable %t", result, ok)
	}
	assertLeavesCovered(t, c.Finest, result.Links)
}

func TestDepthBroadnessUsesScopedCollection(t *testing.T) {
	t.Parallel()
	first := goLinks("dag+check://?check=test")
	second := []string{"dag+check://?check=test&go-module=other&go-package=c&go-test=Other"}
	p, _ := scopedFixture(t, map[string][]string{realTestRoot: first, "dag+check://?check=other/tests/test": second})
	breadth, ok := ApplyBreadth(breadthAnswers(p, map[string]float64{realTestRoot: .9}), p, .5)
	if !ok {
		t.Fatal("breadth failed")
	}
	c := Depth(breadth).Collections[0]
	if c.Hierarchy[0].Offered != broadReason || c.Hierarchy[1].Offered != true {
		t.Fatalf("other root's leaves changed offered levels: %+v", c.Hierarchy)
	}
}
