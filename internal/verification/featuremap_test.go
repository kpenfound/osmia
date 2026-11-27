package verification

import (
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

const checkoutFeature = `---
feature: Checkout
paths:
  - internal/cart/**
  - "web/checkout/*.html"
flows: [checkout, "coupon"]
---

# Checkout

## What exists
`

const adminFeature = `---
feature: Admin
paths: [internal/admin/**]
flows:
  - admin-login
  - checkout
---
`

func newMap() fstest.MapFS {
	return fstest.MapFS{
		"docs/verification/README.md":   {Data: []byte("# Features\n- checkout\n- admin\n")},
		"docs/verification/checkout.md": {Data: []byte(checkoutFeature)},
		"docs/verification/admin.md":    {Data: []byte(adminFeature)},
		"docs/verification/notes.txt":   {Data: []byte("---\nfeature: nope\n---")},
		"docs/verification/sub/deep.md": {Data: []byte(adminFeature)},
	}
}

func names(features []Feature) []string {
	var out []string
	for _, f := range features {
		out = append(out, f.Name)
	}
	return out
}

func TestReadMapReturnsHeadersAndSkipsTheReadme(t *testing.T) {
	m := ReadMap(newMap(), "docs/verification")
	if len(m.Notes) != 0 || !slices.Equal(names(m.Features), []string{"Admin", "Checkout"}) {
		t.Fatalf("%+v", m)
	}
	checkout := m.Features[1]
	if checkout.File != "docs/verification/checkout.md" || !slices.Equal(checkout.Paths, []string{"internal/cart/**", "web/checkout/*.html"}) || !slices.Equal(checkout.Flows, []string{"checkout", "coupon"}) {
		t.Fatalf("%+v", checkout)
	}
}

func TestMatchFilesUsesPlainGlobMatching(t *testing.T) {
	m := ReadMap(newMap(), "docs/verification")
	for files, want := range map[string][]string{
		"internal/cart/total.go":      {"Checkout"},
		"internal/cart/sub/deep.go":   {"Checkout"},
		"web/checkout/index.html":     {"Checkout"},
		"web/checkout/sub/index.html": nil,
		"internal/admin/x.go":         {"Admin"},
		"internal/other/x.go":         nil,
		"internal/cartx/y.go":         nil,
		"web/checkout/app.js":         nil,
	} {
		if got := names(m.MatchFiles([]string{files})); !slices.Equal(got, want) {
			t.Errorf("%s: %q, want %q", files, got, want)
		}
	}
	if got := names(m.MatchFiles([]string{"internal/cart/a.go", "internal/admin/b.go"})); !slices.Equal(got, []string{"Admin", "Checkout"}) {
		t.Fatalf("both: %q", got)
	}
}

func TestMatchPatternsNamesAFeatureThatSharesALiteralPrefix(t *testing.T) {
	m := ReadMap(newMap(), "docs/verification")
	for pattern, want := range map[string][]string{
		"internal/cart/newpkg/**": {"Checkout"},
		"internal/cart/**":        {"Checkout"},
		"internal/**":             {"Admin", "Checkout"},
		"**":                      {"Admin", "Checkout"},
		"internal/car*":           {"Checkout"},
		"web/**":                  {"Checkout"},
		"cmd/tool/**":             nil,
		"internal/billing/**":     nil,
		"internal/[":              nil,
	} {
		if got := names(m.MatchPatterns([]string{pattern})); !slices.Equal(got, want) {
			t.Errorf("%s: %q, want %q", pattern, got, want)
		}
	}
}

func TestPatternsMayOverlapComparesLiteralPrefixes(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"a/b/**", "a/b/c/*.go", true},
		{"a/b/c/*.go", "a/b/**", true},
		{"a/b*", "a/bc/d", true},
		{"a/**", "b/**", false},
		{"a/b/c", "a/b/d", false},
		{"**", "x/y", true},
	} {
		if got := PatternsMayOverlap(tc.a, tc.b); got != tc.want {
			t.Errorf("%s %s: %v", tc.a, tc.b, got)
		}
	}
	if LiteralPrefix("a/b/*.go") != "a/b/" || LiteralPrefix("a/b") != "a/b" || LiteralPrefix("**") != "" {
		t.Fatal("LiteralPrefix")
	}
}

func TestUnionFlowsListsEachFlowOnce(t *testing.T) {
	m := ReadMap(newMap(), "docs/verification")
	if got := m.Flows(); !slices.Equal(got, []string{"admin-login", "checkout", "coupon"}) {
		t.Fatalf("flows %q", got)
	}
}

func TestStructureCheckReportsProblemsAsNotesAndNeverFails(t *testing.T) {
	fsys := newMap()
	fsys["docs/verification/broken.md"] = &fstest.MapFile{Data: []byte("no header here\n")}
	fsys["docs/verification/open.md"] = &fstest.MapFile{Data: []byte("---\nfeature: Open\npaths: [a/**]\n")}
	fsys["docs/verification/nopaths.md"] = &fstest.MapFile{Data: []byte("---\nfeature: X\nflows: [a]\n---\n")}
	fsys["docs/verification/badglob.md"] = &fstest.MapFile{Data: []byte("---\nfeature: X\npaths: [\"../x\"]\n---\n")}
	m := ReadMap(fsys, "docs/verification")
	if !slices.Equal(names(m.Features), []string{"Admin", "Checkout"}) {
		t.Fatalf("features %q", names(m.Features))
	}
	notes := m.Check([]string{"checkout", "admin-login"})
	var text []string
	for _, n := range notes {
		text = append(text, n.String())
	}
	joined := strings.Join(text, "\n")
	for _, want := range []string{"broken.md: the header does not parse", "open.md: the header does not parse", "nopaths.md: the header does not parse", "badglob.md: the header does not parse", `checkout.md: flow "coupon" is not in the lever's list`} {
		if !strings.Contains(joined, want) {
			t.Errorf("notes lack %q:\n%s", want, joined)
		}
	}
	if len(notes) != 5 {
		t.Fatalf("notes:\n%s", joined)
	}
	if got := m.Check(nil); len(got) != 4 {
		t.Fatalf("with no list, only parse notes remain: %v", got)
	}
}

func TestReadMapOfAMissingDirectoryIsANote(t *testing.T) {
	m := ReadMap(fstest.MapFS{}, "docs/verification")
	if len(m.Features) != 0 || len(m.Notes) != 1 || len(m.Check([]string{"a"})) != 1 {
		t.Fatalf("%+v", m)
	}
}

func TestParseHeaderAcceptsTheCommonYAMLShapes(t *testing.T) {
	f, err := ParseHeader([]byte("\ufeff---\r\nfeature: 'Login'\r\n# comment\r\npaths:\r\n- a/**\r\n- b/*.go\r\nflows: []\r\nextra: ignored\r\n---\r\nbody"))
	if err != nil || f.Name != "Login" || !slices.Equal(f.Paths, []string{"a/**", "b/*.go"}) || len(f.Flows) != 0 {
		t.Fatalf("%+v %v", f, err)
	}
	for name, text := range map[string]string{
		"no feature":  "---\npaths: [a]\n---\n",
		"list name":   "---\nfeature: [a]\npaths: [a]\n---\n",
		"dup key":     "---\nfeature: a\nfeature: b\npaths: [a]\n---\n",
		"stray item":  "---\n- a\nfeature: a\npaths: [a]\n---\n",
		"not closed":  "---\nfeature: a\npaths: [a\n---\n",
		"bad quote":   "---\nfeature: a\npaths: [\"a]\n---\n",
		"empty flow":  "---\nfeature: a\npaths: [a]\nflows:\n  - \"\"\n---\n",
		"no colon":    "---\nfeature a\npaths: [a]\n---\n",
		"binary":      "---\nfeature: a\x00\n---\n",
		"item of str": "---\nfeature: a\npaths: b\n  - c\n---\n",
	} {
		if _, err := ParseHeader([]byte(text)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

func TestParseFlowList(t *testing.T) {
	got, err := ParseFlowList([]byte(" [\"checkout\", \"login\"]\n"))
	if err != nil || !slices.Equal(got, []string{"checkout", "login"}) {
		t.Fatalf("%q %v", got, err)
	}
	got, err = ParseFlowList([]byte(`{"flows":["a"]}`))
	if err != nil || !slices.Equal(got, []string{"a"}) {
		t.Fatalf("%q %v", got, err)
	}
	if got, err = ParseFlowList([]byte("[]")); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty list: %#v %v", got, err)
	}
	for _, bad := range []string{"", "null", "checkout\nlogin", `["a",1]`, `["a","a"]`, `[""]`, `["a\nb"]`, `{"flows":null}`, `{"other":[]}`, `"a"`} {
		if _, err := ParseFlowList([]byte(bad)); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}
