package kb

import (
	"reflect"
	"testing"
)

func resolutionMap() Map {
	return Map{Version: Version, Entities: []Entity{
		{ID: "internal", Name: "internal", Paths: []string{"internal"}},
		{ID: "internal.trace", Name: "trace", Aliases: []string{"Trace"}, Paths: []string{"internal/trace"}, PartOf: []string{"internal"}},
		{ID: "internal.trace.git", Name: "git", Paths: []string{"internal/trace/git.go"}, PartOf: []string{"internal.trace"}},
		{ID: "docs", Name: "docs", Aliases: []string{"documentation"}, Paths: []string{"docs", "README.md"}},
		{ID: "go-tests", Name: "tests", Paths: []string{"**/*_test.go"}},
		{ID: "trace-tests", Name: "trace tests", Paths: []string{"internal/trace/*_test.go"}, PartOf: []string{"internal.trace"}},
		{ID: "trace-docs", Name: "trace docs", Paths: []string{"docs/trace.md"}},
		{ID: "trace-doc-owners", Name: "trace doc owners", Paths: []string{"docs/trace.md"}},
		{ID: "guide", Name: "guide", Paths: []string{"**/*.md", "docs/guide"}},
		{ID: "team", Name: "team"},
		{ID: "team.member", Name: "member", PartOf: []string{"team"}},
	}}
}

func TestResolveEntities(t *testing.T) {
	m := resolutionMap()
	if err := Validate(m); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		names []string
		want  Footprint
	}{
		{[]string{"internal.trace.git"}, Footprint{Entities: []string{"internal.trace.git"}, Paths: []string{"internal/trace/git.go"}, Unresolved: []string{}}},
		{[]string{"INTERNAL.Trace.Git"}, Footprint{Entities: []string{"internal.trace.git"}, Paths: []string{"internal/trace/git.go"}, Unresolved: []string{}}},
		{[]string{"TRACE"}, Footprint{Entities: []string{"internal.trace", "internal.trace.git", "trace-tests"}, Paths: []string{"internal/trace", "internal/trace/*_test.go", "internal/trace/git.go"}, Unresolved: []string{}}},
		{[]string{"internal"}, Footprint{Entities: []string{"internal", "internal.trace", "internal.trace.git", "trace-tests"}, Paths: []string{"internal", "internal/trace", "internal/trace/*_test.go", "internal/trace/git.go"}, Unresolved: []string{}}},
		{[]string{"Documentation", "nope", "go-tests", "team", "missing"}, Footprint{Entities: []string{"docs", "go-tests"}, Paths: []string{"**/*_test.go", "README.md", "docs"}, Unresolved: []string{"nope", "team", "missing"}}},
		{nil, Footprint{Entities: []string{}, Paths: []string{}, Unresolved: []string{}}},
	}
	for _, c := range cases {
		if got := m.ResolveEntities(c.names); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%v:\ngot  %#v\nwant %#v", c.names, got, c.want)
		}
	}
}

func TestResolvePaths(t *testing.T) {
	m := resolutionMap()
	got := m.ResolvePaths([]string{
		"internal/trace/git.go",
		"internal/trace/files.go",
		"internal/trace/files_test.go",
		"internal/kb/kb_test.go",
		"internal/kb/kb.go",
		"docs/trace.md",
		"docs/guide/intro.md",
		"README.md",
		"cmd/osmia/main.go",
		"/etc/passwd",
		"../outside",
		"internal/../x",
		".",
		"",
		"internal",
	})
	want := Location{
		Matches: []PathMatch{
			{"internal/trace/git.go", []string{"internal.trace.git"}},
			{"internal/trace/files.go", []string{"internal.trace"}},
			{"internal/trace/files_test.go", []string{"trace-tests"}},
			{"internal/kb/kb_test.go", []string{"internal"}},
			{"internal/kb/kb.go", []string{"internal"}},
			{"docs/trace.md", []string{"trace-doc-owners", "trace-docs"}},
			{"docs/guide/intro.md", []string{"guide"}},
			{"README.md", []string{"docs"}},
			{"internal", []string{"internal"}},
		},
		Unresolved: []string{"cmd/osmia/main.go", "/etc/passwd", "../outside", "internal/../x", ".", ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %#v\nwant %#v", got, want)
	}
	if got := (Map{Version: Version}).ResolvePaths([]string{"a"}); !reflect.DeepEqual(got, Location{Matches: []PathMatch{}, Unresolved: []string{"a"}}) {
		t.Fatalf("empty map: %#v", got)
	}
	everything := Map{Version: Version, Entities: []Entity{{ID: "all", Name: "all", Paths: []string{"**"}}}}
	if got := everything.ResolvePaths([]string{".", "a"}); !reflect.DeepEqual(got, Location{Matches: []PathMatch{{"a", []string{"all"}}}, Unresolved: []string{"."}}) {
		t.Fatalf("repository root: %#v", got)
	}
}

func TestMatch(t *testing.T) {
	for _, c := range []struct {
		pattern, path string
		want          bool
	}{
		{"internal", "internal/trace/x.go", true},
		{"internal", "internals/x.go", false},
		{"internal/trace", "internal", false},
		{"**", "a/b", true},
		{"**/logs", "logs", true},
		{"**/logs", "a/b/logs/x", true},
		{"src/**/test.go", "src/test.go", true},
		{"src/**/test.go", "src/a/b/test.go", true},
		{"src/**/test.go", "src/a/b/other.go", false},
		{"*.md", "README.md", true},
		{"*.md", "docs/x.md", false},
	} {
		if got := match(split(c.pattern), split(c.path)); got != c.want {
			t.Errorf("match(%q, %q) = %v", c.pattern, c.path, got)
		}
	}
}

func split(p string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(p); i++ {
		if i == len(p) || p[i] == '/' {
			out = append(out, p[start:i])
			start = i + 1
		}
	}
	return out
}
