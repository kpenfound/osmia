package checkselect

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

func TestInvocationsGroupLinksThatShareOneFilteredView(t *testing.T) {
	t.Parallel()
	const test = "dag+check://?check=test&go-module=.&go-package="
	for name, c := range map[string]struct {
		links []string
		want  []string
	}{
		"one link":      {[]string{test + "a"}, []string{test + "a"}},
		"identical":     {[]string{test + "a", test + "a"}, []string{test + "a"}},
		"packages":      {[]string{test + "a", test + "b"}, []string{test + "a&go-package=b"}},
		"package tests": {[]string{test + "a&go-test=T1", test + "a&go-test=T2"}, []string{test + "a&go-test=T1&go-test=T2"}},
		"whole packages and a test elsewhere": {
			[]string{test + "cmd", test + "checks", test + "select&go-test=T", test + "delivery"},
			[]string{test + "cmd&go-package=checks&go-package=delivery", test + "select&go-test=T"},
		},
		"correlated tests stay apart": {
			[]string{test + "a&go-test=T1", test + "b&go-test=T2"},
			[]string{test + "a&go-test=T1", test + "b&go-test=T2"},
		},
		"complete cross-product": {
			[]string{test + "a&go-test=T1", test + "a&go-test=T2", test + "b&go-test=T1", test + "b&go-test=T2"},
			[]string{test + "a&go-package=b&go-test=T1&go-test=T2"},
		},
		"different checks": {
			[]string{"dag+check://?check=test", "dag+check://?check=stale"},
			[]string{"dag+check://?check=test&check=stale"},
		},
		"a filter one check lacks": {
			[]string{"dag+check://?check=test", "dag+check://?check=lint&staticcheck-project=."},
			[]string{"dag+check://?check=test", "dag+check://?check=lint&staticcheck-project=."},
		},
		"parent covers items": {
			[]string{test + "a&go-test=T", "dag+check://?check=test", test + "b"},
			[]string{"dag+check://?check=test"},
		},
		"every check covers the scheme": {
			[]string{"dag+check://?check=stale&go-package=a", "dag+check://", "dag+check://release/version", "dag://?check=lint"},
			[]string{"dag+check://", "dag://?check=lint"},
		},
		"different paths": {
			[]string{"dag+check://go/test?go-package=a", "dag+check://release/version", "dag+check://go/test?go-package=b"},
			[]string{"dag+check://go/test?go-package=a&go-package=b", "dag+check://release/version"},
		},
		"filter without a value": {
			[]string{"dag+check://?go-package", "dag+check://?go-package=a", "dag+check://?go-package"},
			[]string{"dag+check://?go-package", "dag+check://?go-package=a"},
		},
		"unreadable link": {[]string{"not-a-link", "not-a-link", test + "a"}, []string{"not-a-link", test + "a"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := Invocations(c.links)
			want := make([][]string, len(c.want))
			for i, address := range c.want {
				want[i] = []string{address}
			}
			if !slices.EqualFunc(got, want, slices.Equal) {
				t.Fatalf("invocations %q, want %q", got, want)
			}
		})
	}
	if got := Invocations(nil); len(got) != 1 || len(got[0]) != 0 {
		t.Fatalf("no links run every check once: %q", got)
	}
}

// Dagger ANDs an address's filters and ORs the values of one filter, so an
// address selects a cross-product. Grouped addresses must select exactly the
// checks the links select: a wider grouping runs checks nobody selected, and a
// narrower one reports a pass without running a selected check.
func TestInvocationsSelectExactlyWhatTheLinksSelect(t *testing.T) {
	t.Parallel()
	dimensions := []filter{
		{"check", []string{"test", "stale"}},
		{"go-package", []string{"a", "b", "c"}},
		{"go-test", []string{"T1", "T2", "T3"}},
	}
	var leaves []map[string]string
	var expand func(i int, leaf map[string]string)
	expand = func(i int, leaf map[string]string) {
		if i == len(dimensions) {
			leaves = append(leaves, leaf)
			return
		}
		for _, value := range dimensions[i].values {
			next := map[string]string{dimensions[i].key: value}
			for k, v := range leaf {
				next[k] = v
			}
			expand(i+1, next)
		}
	}
	expand(0, nil)
	selects := func(a *address, leaf map[string]string) bool {
		for _, f := range a.filters {
			if !slices.Contains(f.values, leaf[f.key]) {
				return false
			}
		}
		return true
	}
	selection := func(links []string) []bool {
		selected := make([]bool, len(leaves))
		for _, link := range links {
			a := parseAddress(link)
			for i, leaf := range leaves {
				selected[i] = selected[i] || selects(a, leaf)
			}
		}
		return selected
	}
	random := rand.New(rand.NewPCG(1, 2))
	for range 2000 {
		var links []string
		for range 1 + random.IntN(12) {
			var pairs []string
			for _, d := range dimensions {
				if random.IntN(4) > 0 {
					pairs = append(pairs, d.key+"="+d.values[random.IntN(len(d.values))])
				}
			}
			links = append(links, "dag+check://?"+strings.Join(pairs, "&"))
		}
		var addresses []string
		for _, invocation := range Invocations(links) {
			addresses = append(addresses, invocation...)
		}
		if !slices.Equal(selection(addresses), selection(links)) {
			t.Fatalf("links %q grouped as %q select different checks", links, addresses)
		}
		if len(addresses) > len(links) {
			t.Fatalf("links %q need %d invocations", links, len(addresses))
		}
	}
}

// One-link commands preserve the exact address; unfamiliar forms remain
// independent rather than acquiring query semantics from their spelling.
func TestInvocationsPreserveUnparsedLinks(t *testing.T) {
	t.Parallel()
	for _, link := range []string{
		"dag+check://?", "dag://?go-test=T&go-package=a&go-test=T",
		"dag+check://?go-package=a%2Fb&go-test=T+One",
		"future://?go-package=a", "dag+check://?go-package=a#fragment",
	} {
		if got := Invocations([]string{link}); len(got) != 1 || !slices.Equal(got[0], []string{link}) {
			t.Fatalf("link %q became %q", link, got)
		}
	}
	if got := Invocations([]string{"future://?go-package=a", "future://?go-package=b"}); len(got) != 2 {
		t.Fatalf("unknown forms combined: %q", got)
	}
}
