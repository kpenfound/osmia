package checkselect

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/systemone"
)

func TestCandidatesCollapseTheLargestCollections(t *testing.T) {
	t.Parallel()
	tests := "dag+check://go/packages/tests/test"
	stale := "dag+check://go/packages/generate/stale"
	release := "dag+check://release/version"
	links := []string{release, stale + "?go-package=a", stale + "?go-package=b", tests + "?go-package=b&go-test=TestX", tests + "?go-package=b&go-test=TestY"}
	for i := range 5 {
		links = append(links, fmt.Sprintf("%s?go-package=a&go-test=Test%d", tests, i))
	}
	for _, c := range []struct {
		limit int
		want  []string
	}{
		{10, slices.Sorted(slices.Values(links))},
		{7, []string{stale + "?go-package=a", stale + "?go-package=b", tests + "?go-package=a", tests + "?go-package=b&go-test=TestX", tests + "?go-package=b&go-test=TestY", release}},
		{4, []string{stale, tests + "?go-package=a", tests + "?go-package=b", release}},
		{3, []string{stale, tests, release}},
	} {
		got, ok := Candidates(links, c.limit)
		if !ok || !slices.Equal(got, c.want) {
			t.Fatalf("limit %d: %q %t, want %q", c.limit, got, ok, c.want)
		}
	}
	if got, ok := Candidates(links, 2); ok {
		t.Fatalf("links narrowed below their collections: %q", got)
	}
	for link, want := range map[string]string{tests + "?go-package=a&go-test=T": tests + "?go-package=a", tests + "?go-package=a": tests, tests: ""} {
		if got, _ := parentLink(link); got != want {
			t.Fatalf("parent of %s is %q, want %q", link, got, want)
		}
	}
}

func TestStateKeepsWithinItsBound(t *testing.T) {
	t.Parallel()
	diff := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+new\n"
	state, ok := State(diff)
	if !ok || state["diff"] != diff || !strings.Contains(state["changed_files"].(string), "a.go (+1 -1)") {
		t.Fatalf("state %+v", state)
	}
	large := diff + "+" + strings.Repeat("x", MaxState) + "\n"
	if state, ok := State(large); !ok || !strings.HasPrefix(state["diff"].(string), "omitted") {
		t.Fatalf("a large diff was kept: %t", ok)
	}
	var many strings.Builder
	for i := range MaxState / 10 {
		fmt.Fprintf(&many, "diff --git a/f%05d.go b/f%05d.go\n--- a/f%05d.go\n+++ b/f%05d.go\n@@ -1 +1 @@\n-a\n+b\n", i, i, i, i)
	}
	if _, ok := State(many.String()); ok {
		t.Fatal("changed files beyond the bound were judged")
	}
}

func TestLinksReadsAListing(t *testing.T) {
	t.Parallel()
	output := "Loading modules\n  dag+check://go/packages/tests/test?go-package=a  \nnot a link\ndag+check://release/version\n"
	if got, want := Links(output), []string{"dag+check://go/packages/tests/test?go-package=a", "dag+check://release/version"}; !slices.Equal(got, want) {
		t.Fatalf("links %q, want %q", got, want)
	}
}

func TestSelectedAppliesTheThreshold(t *testing.T) {
	t.Parallel()
	candidates := []string{"dag+check://a", "dag+check://b", "dag+check://c", "dag+check://d"}
	request := Request(map[string]any{"changed_files": "- a.go (+1 -0)\n"}, candidates)
	if err := request.Validate(); err != nil || len(request.Questions) != len(candidates) {
		t.Fatalf("request %+v: %v", request, err)
	}
	for i, link := range candidates {
		if q := request.Questions[Question(i)]; q.Kind != systemone.KindNoul || !strings.Contains(q.Instructions.(string), link+"?") {
			t.Fatalf("question %d %+v does not ask about %s", i, q, link)
		}
	}
	response := systemone.Response{Answers: map[string]systemone.Answer{
		Question(0): {Kind: systemone.KindNoul, Noul: 0.9},
		Question(1): {Kind: systemone.KindNoul, Noul: Threshold},
		Question(2): {Kind: systemone.KindNoul, Noul: 0.29},
	}}
	if got, want := Selected(response, candidates, Threshold), candidates[:2]; !slices.Equal(got, want) {
		t.Fatalf("selected %q, want %q", got, want)
	}
	if got := Selected(response, candidates, 0.95); got != nil {
		t.Fatalf("selected %q above every answer", got)
	}
}
