package checkselect

import (
	"errors"
	"testing"
)

const (
	rootTest    = "dag+check://?check=test"
	pkgA        = rootTest + "&go-module=.&go-package=internal/a"
	pkgB        = rootTest + "&go-module=.&go-package=internal/b"
	lintLink    = "dag+check://?check=lint&staticcheck-project=."
	versionLink = "dag+check://?check=release/version-round-trip"
)

var flakeKnown = []string{
	rootTest, rootTest + "&go-module=.",
	pkgA, pkgA + "&go-test=TestOne", pkgA + "&go-test=TestTwo",
	pkgB, pkgB + "&go-test=TestOne",
	lintLink, versionLink,
}

func TestFlakeGroupOfLinks(t *testing.T) {
	t.Parallel()
	r := NewFlakeResolver(flakeKnown)
	for name, tc := range map[string]struct{ link, want string }{
		"go test groups with its package":    {pkgA + "&go-test=TestOne", "dag+check://?check=test&go-module=.&go-package=internal/a"},
		"go package is its own group":        {pkgA, "dag+check://?check=test&go-module=.&go-package=internal/a"},
		"standalone check is its own group":  {versionLink, versionLink},
		"standalone with an argument":        {lintLink, lintLink},
		"filter order does not matter":       {rootTest + "&go-test=TestOne&go-package=internal/a&go-module=.", "dag+check://?check=test&go-module=.&go-package=internal/a"},
		"path form groups with its package":  {"dag://go/packages/tests/test?go-package=x&go-test=T", "dag://go/packages/tests/test?go-package=x"},
		"path form package is its own group": {"dag://go/packages/tests/test?go-package=x", "dag://go/packages/tests/test?go-package=x"},
		"unreadable link is its own group":   {"something else", "something else"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := r.Link(tc.link)
			if err != nil || got.Key != tc.want || got.Unresolved {
				t.Fatalf("group %+v, %v; want %q", got, err, tc.want)
			}
		})
	}
	siblings := []string{pkgA + "&go-test=TestOne", pkgA + "&go-test=TestTwo", pkgA}
	first, _ := r.Link(siblings[0])
	for _, link := range siblings[1:] {
		if got, _ := r.Link(link); got != first {
			t.Fatalf("%s is in group %+v, not its sibling's %+v", link, got, first)
		}
	}
	if other, _ := r.Link(pkgB + "&go-test=TestOne"); other == first {
		t.Fatalf("a test of another package shares group %+v", first)
	}
}

func TestFlakeGroupNeverIsModuleWideOrCollectionRoot(t *testing.T) {
	t.Parallel()
	r := NewFlakeResolver(flakeKnown)
	for _, link := range []string{rootTest, rootTest + "&go-module=.", "dag+check://", rootTest + "&go-test=TestOne"} {
		if got, err := r.Link(link); !errors.Is(err, ErrBroadFlakeCheck) {
			t.Errorf("%s: group %+v, %v; want ErrBroadFlakeCheck", link, got, err)
		}
	}
	if got, err := r.Link(rootTest + "&go-module=.&go-package=internal/a&go-package=internal/b"); !errors.Is(err, ErrBroadFlakeCheck) {
		t.Errorf("two packages: group %+v, %v", got, err)
	}
	only := NewFlakeResolver([]string{rootTest + "&go-module=other"})
	if got, err := only.Link(rootTest + "&go-module=other"); err != nil || got.Key != rootTest+"&go-module=other" {
		t.Errorf("module without packages: %+v, %v", got, err)
	}
}

func TestCloudNameGroupsWithTheLocalLinkOfTheSameCheck(t *testing.T) {
	t.Parallel()
	r := NewFlakeResolver(flakeKnown)
	local := func(link string) FlakeGroup {
		t.Helper()
		g, err := r.Link(link)
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	for name, tc := range map[string]struct{ cloud, link string }{
		"test in a package":           {"test/internal/a/TestOne", pkgA + "&go-test=TestOne"},
		"test with the module":        {"test/./internal/a/TestTwo", pkgA + "&go-test=TestTwo"},
		"sibling test":                {"test/internal/a/TestTwo", pkgA + "&go-test=TestOne"},
		"package":                     {"test/internal/a", pkgA},
		"other package":               {"test/internal/b/TestOne", pkgB},
		"full check path":             {"test/internal/b", pkgB},
		"standalone check":            {"release/version-round-trip", versionLink},
		"standalone by trailing name": {"version-round-trip", versionLink},
		"check with an argument":      {"lint", lintLink},
		"colon separators":            {"test:internal:a:TestOne", pkgA},
		"surrounding space":           {"  test/internal/a/TestOne ", pkgA},
		"cloud name given as a link":  {pkgA + "&go-test=TestOne", pkgA},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := r.CloudName(tc.cloud)
			if want := local(tc.link); got != want || got.Unresolved {
				t.Fatalf("cloud name %q is in group %+v, want the group of %s: %+v", tc.cloud, got, tc.link, want)
			}
		})
	}
}

func TestCloudNameMatchesChecksNamedByAPath(t *testing.T) {
	t.Parallel()
	r := NewFlakeResolver([]string{
		"dag://go/packages/tests/test?go-package=x&go-test=T1",
		"dag://go/packages/tests/test?go-package=x&go-test=T2",
		"dag://go/packages/tests/test?go-package=y&go-test=T1",
	})
	want, _ := r.Link("dag://go/packages/tests/test?go-package=x&go-test=T2")
	for _, name := range []string{"go/packages/tests/test/x/T1", "tests/test/x/T2", "test/x", "go/packages/tests/test/x"} {
		if got := r.CloudName(name); got != want {
			t.Errorf("%q is in group %+v, want %+v", name, got, want)
		}
	}
}

func TestCloudNameTiedToNoKnownCheckIsItsOwnUnresolvedGroup(t *testing.T) {
	t.Parallel()
	r := NewFlakeResolver(flakeKnown)
	for _, name := range []string{
		"test-split/test-cli-engine",
		"test/internal/c/TestOne",
		"test/internal/a/TestNone",
		"test",
		"test/.",
		"docs",
	} {
		if got := r.CloudName(name); !got.Unresolved || got.Key != name {
			t.Errorf("%q: group %+v, want its own unresolved group", name, got)
		}
	}
	a, b := r.CloudName("docs"), r.CloudName("docs-lint")
	if a == b {
		t.Fatalf("different unknown names share %+v", a)
	}
	if same := r.CloudName("docs"); same != a {
		t.Fatalf("the same unknown name moved from %+v to %+v", a, same)
	}
}

func TestCloudNameWithoutKnownChecksIsUnresolved(t *testing.T) {
	t.Parallel()
	got := NewFlakeResolver(nil).CloudName("test/a/T")
	if !got.Unresolved || got.Key != "test/a/T" {
		t.Fatalf("group %+v", got)
	}
}
