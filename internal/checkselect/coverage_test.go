package checkselect

import (
	"fmt"
	"slices"
	"testing"
)

func TestCandidatesExcludeWholeCollections(t *testing.T) {
	t.Parallel()
	for _, root := range []string{"dag+check://?check=test", "dag+check://go/packages/tests/test"} {
		t.Run(root, func(t *testing.T) {
			item := func(filters string) string {
				separator := "?"
				if root == "dag+check://?check=test" {
					separator = "&"
				}
				return root + separator + filters
			}
			module := item("go-module=.")
			a, b := item("go-module=.&go-package=a"), item("go-module=.&go-package=b")
			a1, a2, b1 := a+"&go-test=T1", a+"&go-test=T2", b+"&go-test=T1"
			other := item("go-module=other&go-package=c&go-test=T3")
			for _, tc := range []struct {
				name   string
				listed []string
				want   []string
			}{
				{"listed roots and parents", []string{root, module, a, b, a1, a2, b1}, []string{a, b, a1, a2, b1}},
				{"unlisted root", []string{module, a, b, a1, a2, b1}, []string{a, b, a1, a2, b1}},
				{"flat packages", []string{root, module, a, b}, []string{a, b}},
				{"only package", []string{root, module, a, a1, a2}, []string{a1, a2}},
				{"only package without listed root", []string{a, a1, a2}, []string{a1, a2}},
				{"multiple modules", []string{root, module, a1, a2, other}, []string{module, a1, a2, other}},
				{"standalone root", []string{root}, []string{root}},
				{"single leaf", []string{root, module, a, a1}, []string{a1}},
				{"equivalent leaves", []string{a1, item("go-test=T1&go-package=a&go-module=.")}, []string{a1, item("go-test=T1&go-package=a&go-module=.")}},
				{"equivalent roots", []string{root, root + itemSeparator(root)}, []string{root, root + itemSeparator(root)}},
				{"equivalent broad roots", []string{root, root + itemSeparator(root), a, a1, a2}, []string{a1, a2}},
				{"reordered broad parent", []string{root, item("go-package=a&go-module=."), a1, a2}, []string{a1, a2}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					got, ok := Candidates(tc.listed, MaxQuestions)
					want := slices.Sorted(slices.Values(tc.want))
					if !ok || !slices.Equal(got, want) {
						t.Fatalf("candidates %q, %t; want %q", got, ok, want)
					}
					assertLeavesCovered(t, tc.listed, got)
				})
			}
		})
	}
}

func TestCandidatesNarrowLargeCollectionsToPackages(t *testing.T) {
	t.Parallel()
	for _, root := range []string{"dag+check://?check=test", "dag+check://tests"} {
		for _, parentsListed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/parents=%t", root, parentsListed), func(t *testing.T) {
				separator := "?"
				if root == "dag+check://?check=test" {
					separator = "&"
				}
				var links, want []string
				for _, pkg := range []string{"a", "b"} {
					packageLink := root + separator + "go-module=.&go-package=" + pkg
					want = append(want, packageLink)
					if parentsListed {
						links = append(links, packageLink)
					}
					for i := range MaxQuestions + 1 {
						links = append(links, fmt.Sprintf("%s&go-test=T%d", packageLink, i))
					}
				}
				if parentsListed {
					links = append(links, root, root+separator+"go-module=.")
				}
				got, ok := Candidates(links, MaxQuestions)
				if !ok || !slices.Equal(got, want) {
					t.Fatalf("candidates %q, %t; want %q", got, ok, want)
				}
				assertLeavesCovered(t, links, got)
			})
		}
	}
}

// An empty query or repeated check-name filter represents the same root.
func itemSeparator(root string) string {
	if root == "dag+check://?check=test" {
		return "&check=test"
	}
	return "?"
}

func assertLeavesCovered(t *testing.T, listed, candidates []string) {
	t.Helper()
	for _, link := range listed {
		a := parseAddress(link)
		leaf := true
		for _, other := range listed {
			b := parseAddress(other)
			if a.covers(b) && !b.covers(a) {
				leaf = false
			}
		}
		if leaf && !slices.ContainsFunc(candidates, func(candidate string) bool { return parseAddress(candidate).covers(a) }) {
			t.Errorf("leaf %s is not covered by %q", link, candidates)
		}
	}
}

func TestCandidatesKeepOpaqueAndIndependentPaths(t *testing.T) {
	t.Parallel()
	links := []string{"opaque", "dag+check://tests?invalid", "dag+check://tests#fragment", "dag+check://tests", "dag+check://tests/longer"}
	got, ok := Candidates(links, MaxQuestions)
	if !ok || !slices.Equal(got, slices.Sorted(slices.Values(links))) {
		t.Fatalf("candidates %q, %t", got, ok)
	}
}

func TestCandidatesCannotNarrowThroughRoots(t *testing.T) {
	t.Parallel()
	for _, root := range []string{"dag+check://?check=test", "dag+check://tests"} {
		for _, parentsListed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/parents=%t", root, parentsListed), func(t *testing.T) {
				separator := "?"
				if root == "dag+check://?check=test" {
					separator = "&"
				}
				packageLink := root + separator + "go-module=.&go-package=only"
				var links []string
				for i := range MaxQuestions + 1 {
					links = append(links, fmt.Sprintf("%s&go-test=T%d", packageLink, i))
				}
				if parentsListed {
					links = append(links, root, root+separator+"go-module=.", packageLink)
				}
				if got, ok := Candidates(links, MaxQuestions); ok {
					t.Fatalf("narrowed through a whole collection: %q", got)
				}
			})
		}
	}
	var standalone []string
	for i := range MaxQuestions + 1 {
		standalone = append(standalone, fmt.Sprintf("dag+check://?check=standalone%d", i))
	}
	if got, ok := Candidates(standalone, MaxQuestions); ok {
		t.Fatalf("standalone checks collapsed to bare scheme: %q", got)
	}
}

func TestCandidatesSkipForbiddenLargestParent(t *testing.T) {
	t.Parallel()
	links := []string{"dag+check://?check=a", "dag+check://?check=b", "dag+check://?check=c", "dag+check://?check=d", "dag+check://?check=e"}
	root := "dag+check://?check=test&go-module=."
	for _, pkg := range []string{"a", "b"} {
		for i := range 3 {
			links = append(links, fmt.Sprintf("%s&go-package=%s&go-test=T%d", root, pkg, i))
		}
	}
	got, ok := Candidates(links, 9)
	want := append(slices.Clone(links[:5]), root+"&go-package=a")
	want = append(want, links[8:]...)
	slices.Sort(want)
	if !ok || !slices.Equal(got, want) {
		t.Fatalf("candidates %q, %t; want %q", got, ok, want)
	}
	assertLeavesCovered(t, links, got)
}
