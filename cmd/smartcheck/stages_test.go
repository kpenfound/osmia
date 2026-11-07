package main

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/checkselect"
	"github.com/kpenfound/osmia/internal/config"
)

const (
	goRoot    = "dag+check://?check=test"
	goModule  = goRoot + "&go-module=."
	packageA  = goModule + "&go-package=a"
	packageB  = goModule + "&go-package=b"
	lintRoot  = "dag+check://lint"
	docsCheck = "dag+check://docs"
	uncovered = "dag+check://uncovered"
)

func stageFixture() (*fakeDagger, *fakeJev) {
	d := &fakeDagger{
		topLevel: []string{goRoot, lintRoot, release, docsCheck},
		links: []string{
			goRoot, goModule, packageA, packageB,
			packageA + "&go-test=TestA", packageA + "&go-test=TestB",
			packageB + "&go-test=TestA", packageB + "&go-test=TestB",
			lintRoot, lintRoot + "?path=a", lintRoot + "?path=b",
			release, docsCheck, uncovered,
		},
	}
	j := &fakeJev{
		breadth:       map[string]float64{goRoot: .9, lintRoot: .05, release: .75, docsCheck: .1},
		level:         "go-package",
		probabilities: map[string]float64{},
	}
	for _, link := range d.links {
		j.probabilities[link] = .1
	}
	j.probabilities[packageA] = .95
	return d, j
}

func rankingLinks(j *fakeJev) []string {
	request := j.requests[len(j.requests)-1]
	var links []string
	for i := range len(request.Questions) {
		q := request.Questions[checkselect.Question(i)]
		instructions, _ := q.Instructions.(map[string]any)
		link, _ := instructions["link"].(string)
		links = append(links, link)
	}
	return links
}

func assertStages(t *testing.T, j *fakeJev, want ...string) {
	t.Helper()
	var got []string
	for _, request := range j.requests {
		got = append(got, requestStage(request))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("stages %v, want %v", got, want)
	}
}

func TestSmartcheckThreeStagesAndVerboseHierarchy(t *testing.T) {
	t.Parallel()
	dir := newRepository(t)
	write(t, dir, "README.md", "changed\n")
	d, j := stageFixture()
	r := runHarness(t, d, j, withKey, "-C", dir, "-v", "--", "--progress=report")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	assertStages(t, j, checkselect.BreadthTask, checkselect.DepthTask, checkselect.Task)
	wantLists := [][]string{{"list", "checks", "--all", "--format=link"}, {"list", "checks", "--format=link"}}
	for _, root := range d.topLevel {
		wantLists = append(wantLists, []string{"list", "checks", "--all", "--format=link", root})
	}
	if !slices.EqualFunc(d.lists, wantLists, slices.Equal) {
		t.Fatalf("listings %v", d.lists)
	}
	want := []string{packageA, packageB, release, uncovered}
	slices.Sort(want)
	if got := rankingLinks(j); !slices.Equal(got, want) {
		t.Fatalf("ranked %q, want %q", got, want)
	}
	if want := [][]string{{"--fail-fast", "--parallel", "4", "--progress=report", packageA}}; !slices.EqualFunc(d.checks, want, slices.Equal) {
		t.Fatalf("ran %v, want %v", d.checks, want)
	}
	for _, want := range []string{
		"✓ 0.90 " + goRoot, "  0.05 " + lintRoot,
		"✓ 0.75 " + release, "  0.10 " + docsCheck,
		"hierarchy (coarsest first)",
		"go-module: 1 links, contains go-package, not offered: covers the whole collection; already kept by stage 1",
		"go-package: 2 links, contains go-test, offered",
		"go-test: 4 links, contains none, offered",
		"example: " + packageA,
		"chosen go-package (probability 0.83)",
		"jev: jev-1.13.0 answered", "✓ 0.95 " + packageA,
		"selected 1 of 4 at probability 0.50 or more",
	} {
		if !strings.Contains(r.stderr, want) {
			t.Fatalf("verbose output lacks %q:\n%s", want, r.stderr)
		}
	}
	timeout, _ := time.ParseDuration(config.DefaultJevTimeout)
	for i, request := range j.requests {
		if !reflect.DeepEqual(request.State, j.requests[0].State) {
			t.Fatalf("stage %d used a different state", i)
		}
		if j.remaining[i] < timeout-time.Second || j.remaining[i] > timeout {
			t.Fatalf("stage %d got %s, want a fresh %s timeout", i, j.remaining[i], timeout)
		}
		if j.contexts[i].Err() != context.Canceled {
			t.Fatalf("stage %d context was not released", i)
		}
		if i > 0 && j.contexts[i] == j.contexts[i-1] {
			t.Fatal("stages shared a context")
		}
	}
}

func TestSmartcheckStageRelativeFallbacks(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"breadth timeout", "breadth unusable", "top-level listing", "scoped listing", "depth timeout", "depth unusable", "ranking timeout", "ranking unusable"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := newRepository(t)
			write(t, dir, "README.md", "changed\n")
			d, j := stageFixture()
			// Keep all ranking scores high: this isolates the pre-filter's
			// exclusions from ranking's threshold and fallback picks.
			for link := range j.probabilities {
				j.probabilities[link] = .9
			}
			var want []string
			stages := []string{checkselect.BreadthTask, checkselect.DepthTask, checkselect.Task}
			switch {
			case strings.HasPrefix(name, "breadth"), name == "top-level listing", name == "scoped listing":
				want, _ = checkselect.Candidates(d.links, checkselect.MaxQuestions)
				stages = []string{checkselect.BreadthTask, checkselect.Task}
				if name == "top-level listing" {
					d.topExit = 7
					stages = []string{checkselect.Task}
				} else if name == "scoped listing" {
					d.scopeExit = 7
					stages = []string{checkselect.Task}
				} else if strings.HasSuffix(name, "timeout") {
					j.failStage = checkselect.BreadthTask
				} else {
					j.unusableStage = checkselect.BreadthTask
				}
			case strings.HasPrefix(name, "depth"):
				want = []string{packageA + "&go-test=TestA", packageA + "&go-test=TestB", packageB + "&go-test=TestA", packageB + "&go-test=TestB", release, uncovered}
				slices.Sort(want)
				if strings.HasSuffix(name, "timeout") {
					j.failStage = checkselect.DepthTask
				} else {
					j.unusableStage = checkselect.DepthTask
				}
			default:
				want = []string{packageA, packageB, release, uncovered}
				slices.Sort(want)
				if strings.HasSuffix(name, "timeout") {
					j.failStage = checkselect.Task
				} else {
					j.unusableStage = checkselect.Task
				}
			}
			r := runHarness(t, d, j, withKey, "-C", dir, "-v", "--fallback-picks=2")
			if r.code != 0 {
				t.Fatalf("exit %d: %s", r.code, r.stderr)
			}
			assertStages(t, j, stages...)
			if got := rankingLinks(j); !slices.Equal(got, want) {
				t.Fatalf("ranked %q, want %q", got, want)
			}
			if strings.HasPrefix(name, "ranking") {
				if !strings.Contains(r.stderr, "smartcheck: running every check: Jev did not answer:") || !slices.EqualFunc(d.checks, [][]string{{"--fail-fast", "--parallel", "4"}}, slices.Equal) {
					t.Fatalf("ranking fallback: %+v, ran %v", r, d.checks)
				}
			} else {
				for _, args := range d.checks {
					if !slices.Equal(args[:3], []string{"--fail-fast", "--parallel", "4"}) || len(args) == 3 {
						t.Fatalf("not a selected run: %q", args)
					}
				}
			}
		})
	}
}

func TestSmartcheckSuppliesScopedGoMembership(t *testing.T) {
	t.Parallel()
	dir := newRepository(t)
	write(t, dir, "README.md", "changed\n")
	const testRoot = "dag+check://?check=go/modules/packages/tests/test"
	const staleRoot = "dag+check://?check=go/modules/packages/generate/stale"
	d, j := stageFixture()
	d.topLevel = []string{testRoot, staleRoot, release}
	d.scoped = map[string][]string{
		testRoot:  {packageA + "&go-test=TestA", packageA + "&go-test=TestB", packageB + "&go-test=TestA", packageB + "&go-test=TestB"},
		staleRoot: {"dag+check://?check=stale&go-module=.&go-package=a", "dag+check://?check=stale&go-module=.&go-package=b"},
		release:   {release},
	}
	d.links = nil
	for _, members := range d.scoped {
		d.links = append(d.links, members...)
	}
	j.breadth = map[string]float64{testRoot: .9, staleRoot: .1, release: .8}
	r := runHarness(t, d, j, withKey, "-C", dir, "--dry-run", "-v")
	if r.code != 0 || r.stdout != packageA+"\n" {
		t.Fatalf("scoped selection: %+v", r)
	}
	assertStages(t, j, checkselect.BreadthTask, checkselect.DepthTask, checkselect.Task)
	if got := rankingLinks(j); !slices.Equal(got, []string{packageA, packageB, release}) {
		t.Fatalf("ranked %q", got)
	}
	for _, root := range d.topLevel {
		if !slices.ContainsFunc(d.lists, func(args []string) bool {
			return slices.Equal(args, []string{"list", "checks", "--all", "--format=link", root})
		}) {
			t.Fatalf("no scoped listing for %s: %q", root, d.lists)
		}
	}
}

func TestSmartcheckUsesFinestDepthChoice(t *testing.T) {
	t.Parallel()
	dir := newRepository(t)
	write(t, dir, "README.md", "changed\n")
	d, j := stageFixture()
	j.level = "go-test"
	j.probabilities[packageA+"&go-test=TestA"] = .95
	r := runHarness(t, d, j, withKey, "-C", dir, "--dry-run", "-v")
	if r.code != 0 || r.stdout != packageA+"&go-test=TestA\n" || !strings.Contains(r.stderr, "chosen go-test (probability 0.83)") || len(d.checks) != 0 {
		t.Fatalf("finest choice: %+v, checks %v", r, d.checks)
	}
	assertStages(t, j, checkselect.BreadthTask, checkselect.DepthTask, checkselect.Task)
	if got := rankingLinks(j); slices.Contains(got, packageA) || slices.Contains(got, packageB) || len(got) != 6 {
		t.Fatalf("ranked %q", got)
	}
}

func TestSmartcheckSkipsDepthWithoutAChoice(t *testing.T) {
	t.Parallel()
	dir := newRepository(t)
	write(t, dir, "README.md", "changed\n")
	d := &fakeDagger{topLevel: []string{lintRoot}, links: []string{lintRoot, lintRoot + "?path=a", lintRoot + "?path=b"}}
	j := &fakeJev{breadth: map[string]float64{lintRoot: .8}, probabilities: map[string]float64{lintRoot + "?path=a": .9, lintRoot + "?path=b": .1}}
	r := runHarness(t, d, j, withKey, "-C", dir, "--dry-run", "-v")
	if r.code != 0 || r.stdout != lintRoot+"?path=a\n" || !strings.Contains(r.stderr, "chosen path (no judgment needed)") {
		t.Fatalf("single level: %+v", r)
	}
	assertStages(t, j, checkselect.BreadthTask, checkselect.Task)
}
