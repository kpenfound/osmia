package checkselect

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/agentcore/jev/systemone"
)

func goLinks(root string) []string {
	sep := "?"
	if strings.Contains(root, "?") {
		sep = "&"
	}
	base := root + sep + "go-module=."
	return []string{base + "&go-package=a&go-test=TestA", base + "&go-package=a&go-test=TestB", base + "&go-package=b&go-test=TestC", base + "&go-package=b&go-test=TestD"}
}

func breadthAnswers(plan BreadthPlan, probabilities map[string]float64) systemone.Response {
	r := systemone.Response{Answers: map[string]systemone.Answer{}}
	for i, root := range plan.Roots {
		r.Answers[BreadthQuestion(i)] = systemone.Answer{Kind: systemone.KindNoul, Noul: probabilities[root.Link]}
	}
	return r
}

func allKept(t *testing.T, roots, expanded []string) BreadthResult {
	t.Helper()
	p := Breadth(roots, expanded)
	b, ok := ApplyBreadth(breadthAnswers(p, nil), p, .5)
	if !ok {
		t.Fatal("breadth rejected valid answers")
	}
	return b
}

func TestBreadthRequestWordingAndData(t *testing.T) {
	t.Parallel()
	if BreadthTask != "check-breadth" || BreadthVersion != 1 {
		t.Fatal("incorrect breadth identity")
	}
	root, standalone := "dag+check://tests", "dag+check://version"
	leaves := goLinks(root)
	p := Breadth([]string{standalone, root}, append(slices.Clone(leaves), root, root+"?go-module=.", standalone))
	state, _ := State("")
	r := BreadthRequest(state, p)
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(r.Questions) != 2 {
		t.Fatalf("questions: %+v", r.Questions)
	}
	for i, check := range p.Roots {
		q := r.Questions[BreadthQuestion(i)]
		want := map[string]any{
			"stage": fmt.Sprintf("Stage 1 of 3, breadth. Could the change in the state alter the result of the Dagger check `%s`, or of any of its items?", check.Link),
			"link":  check.Link,
		}
		if check.Collection {
			want["items"], want["examples"] = 4, leaves[:3]
		}
		if q.Kind != systemone.KindNoul || !reflect.DeepEqual(q.Instructions, want) {
			t.Fatalf("question %+v; want %+v", q, want)
		}
		if q.Yes != "The check, or at least one of its items, builds or exercises a changed file, or code, configuration or generated files the change affects. It goes on to stage 2 or 3 for a finer judgment." ||
			q.No != "Nothing the check or any of its items depends on is touched by the change. The check and every item in it are skipped with no further judgment, so answer no only when the change clearly cannot reach it." {
			t.Fatalf("rubric: %+v", q)
		}
	}
	if !reflect.DeepEqual(r.State, state) {
		t.Fatal("breadth changed shared state")
	}
}

func TestBreadthAttributesBothLinkShapesAndAppliesThreshold(t *testing.T) {
	t.Parallel()
	for _, root := range []string{"dag+check://tests", "dag+check://?check=test"} {
		t.Run(root, func(t *testing.T) {
			kept, dropped, uncovered := "dag+check://version", "dag+check://lint", "dag+check://unknown?item=one"
			leaves := goLinks(root)
			listed := append(slices.Clone(leaves), root, kept, dropped, uncovered)
			p := Breadth([]string{root, kept, dropped}, listed)
			if !slices.Equal(p.Uncovered, []string{uncovered}) {
				t.Fatalf("uncovered %q", p.Uncovered)
			}
			for _, c := range p.Roots {
				if c.Link == root && (!c.Collection || !slices.Equal(c.Links, leaves)) {
					t.Fatalf("collection %+v", c)
				}
			}
			b, ok := ApplyBreadth(breadthAnswers(p, map[string]float64{root: .5, kept: .9, dropped: .49}), p, .5)
			if !ok || !slices.Equal(b.KeptRoots, distinct([]string{root, kept})) || len(b.Collections) != 1 || b.Collections[0].Link != root || !slices.Equal(b.Ranking, distinct([]string{kept, uncovered})) {
				t.Fatalf("breadth %+v, usable %t", b, ok)
			}
			b, ok = ApplyBreadth(breadthAnswers(p, map[string]float64{kept: .5}), p, .5)
			if !ok || len(b.Collections) != 0 || !slices.Equal(b.KeptRoots, []string{kept}) || !slices.Equal(Finest(Depth(b)).Links, distinct([]string{kept, uncovered})) {
				t.Fatalf("collection not dropped: %+v", b)
			}
			b, ok = ApplyBreadth(breadthAnswers(p, nil), p, .5)
			if !ok || len(b.KeptRoots) != 3 || len(b.Collections) != 1 {
				t.Fatalf("nothing above threshold did not keep all: %+v", b)
			}
		})
	}
}

func TestBreadthMappingUsesCoverageAndPreservesUnknownLinks(t *testing.T) {
	t.Parallel()
	root := "dag+check://?check=test"
	leaf := root + "&go-test=T&go-package=a"
	parent := root + "&go-package=a"
	p := Breadth([]string{root, parent}, []string{leaf, "opaque", "dag+check://other/path"})
	if len(p.Roots[0].Links) != 0 || !slices.Equal(p.Roots[1].Links, []string{leaf}) || !slices.Equal(p.Uncovered, []string{"dag+check://other/path", "opaque"}) {
		t.Fatalf("attribution: %+v", p)
	}
	// Repeated check-name filters do not introduce a hierarchy level.
	repeated := Breadth([]string{root + "&check=test"}, []string{leaf})
	if !slices.Equal(repeated.Roots[0].Links, []string{leaf}) {
		t.Fatalf("repeated root: %+v", repeated)
	}
}

func TestDepthRequestHierarchyAndWording(t *testing.T) {
	t.Parallel()
	if DepthTask != "check-depth" || DepthVersion != 1 {
		t.Fatal("incorrect depth identity")
	}
	root := "dag+check://?check=test"
	leaves := goLinks(root)
	p := Depth(allKept(t, []string{root}, leaves))
	state, _ := State("")
	r := DepthRequest(state, p)
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(r.Questions) != 1 {
		t.Fatalf("questions %+v", r.Questions)
	}
	q := r.Questions[DepthQuestion(0)]
	instructions := q.Instructions.(map[string]any)
	if q.Kind != systemone.KindChoice || instructions["stage"] != "Stage 2 of 3, depth. Stage 1 judged that the change could affect the collection `dag+check://?check=test`. Choose the level at which stage 3 should judge its links." {
		t.Fatalf("question %+v", q)
	}
	wantGuidance := "The levels nest: each item contains every item at the levels below it. Choosing a level never drops a check. It decides how finely stage 3 divides this collection, and each link at the chosen level runs everything beneath it. A level that is too broad cannot exclude anything: nearly every link holds code the change touches, so nearly the whole collection runs. A level that is too fine asks stage 3 about items the change does not identify: an individual test's name rarely says what code it exercises, so stage 3 has to guess, and a wrong guess skips an affected test. Choose the level whose items correspond most directly to the files and directories in the change, so each link can be judged from the change itself. For Go collections that is usually `go-package`: a package is a directory that the changed files name, and imports connect it to other packages. `go-module` is too broad and `go-test` too fine. Choose a finer level only when the change itself identifies items at that level. Choose a coarser one only when the change cannot tell items at this level apart."
	if instructions["guidance"] != wantGuidance {
		t.Fatalf("guidance %s", instructions["guidance"])
	}
	wantHierarchy := []map[string]any{
		{"level": "go-module", "links": 1, "example": root + "&go-module=.", "contains": "go-package", "offered": "covers the whole collection; already kept by stage 1"},
		{"level": "go-package", "links": 2, "example": root + "&go-module=.&go-package=a", "contains": "go-test", "offered": true},
		{"level": "go-test", "links": 4, "example": leaves[0], "contains": "none", "offered": true},
	}
	got, _ := json.Marshal(instructions["hierarchy"])
	want, _ := json.Marshal(wantHierarchy)
	// Compare JSON values, including the absence of private execution data.
	var gotValue, wantValue any
	json.Unmarshal(got, &gotValue)
	json.Unmarshal(want, &wantValue)
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("hierarchy %s, want %s", got, want)
	}
	wantOptions := []systemone.Option{
		{Name: "go-package", Description: "Judge each of the `2` `go-package` links, such as `dag+check://?check=test&go-module=.&go-package=a`. Each runs every `go-test` item in it."},
		{Name: "go-test", Description: "Judge each of the `4` `go-test` links, such as `dag+check://?check=test&go-module=.&go-package=a&go-test=TestA`."},
	}
	if !reflect.DeepEqual(q.Options, wantOptions) {
		t.Fatalf("options %+v", q.Options)
	}
	if !reflect.DeepEqual(r.State, state) {
		t.Fatal("depth changed shared state")
	}
}

func TestDepthAppliesChosenLevelWithoutOverride(t *testing.T) {
	t.Parallel()
	for _, root := range []string{"dag+check://tests", "dag+check://?check=test"} {
		leaves := goLinks(root)
		// Listed intermediate links must not survive a choice of the finest level.
		listed := append(slices.Clone(leaves), cutLinks(leaves, 2)...)
		p := Depth(allKept(t, []string{root}, listed))
		for _, level := range []string{"go-package", "go-test"} {
			r := systemone.Response{Answers: map[string]systemone.Answer{DepthQuestion(0): {Kind: systemone.KindChoice, Choice: level, Confidence: .01}}}
			got, ok := ApplyDepth(r, p)
			want := leaves
			if level == "go-package" {
				want = cutLinks(leaves, 2)
			}
			if !ok || !slices.Equal(got.Links, want) || got.Levels[root] != level {
				t.Fatalf("%s: %+v, usable %t; want %q", level, got, ok, want)
			}
			assertLeavesCovered(t, listed, got.Links)
		}
	}
}

func TestDepthSkipsInconsistentAndSingleOfferableLevels(t *testing.T) {
	t.Parallel()
	root := "dag+check://?check=test"
	for _, tc := range []struct {
		name  string
		links []string
		level string
	}{
		{"inconsistent order", []string{root + "&go-module=.&go-package=a&go-test=A", root + "&go-module=.&go-test=B&go-package=b"}, "finest"},
		{"different dimensions", []string{root + "&package=a", root + "&suite=b"}, "finest"},
		{"one offered", []string{root + "&go-module=.&go-package=a", root + "&go-module=.&go-package=b"}, "go-package"},
		{"one package", []string{root + "&go-module=.&go-package=a&go-test=A", root + "&go-module=.&go-package=a&go-test=B"}, "go-test"},
		{"one leaf", []string{root + "&go-module=."}, "go-module"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := Depth(allKept(t, []string{root}, tc.links))
			if len(DepthRequest(map[string]any{"diff": "x"}, p).Questions) != 0 {
				t.Fatal("unnecessary depth question")
			}
			got, ok := ApplyDepth(systemone.Response{}, p)
			if !ok || !slices.Equal(got.Links, distinct(tc.links)) || got.Levels[root] != tc.level {
				t.Fatalf("got %+v, %t", got, ok)
			}
			assertLeavesCovered(t, tc.links, got.Links)
		})
	}
}

func TestDepthFailurePreservesBreadthAtFinest(t *testing.T) {
	t.Parallel()
	a, b, standalone, uncovered := "dag+check://a", "dag+check://b", "dag+check://version", "dag+check://uncovered?item=one"
	aLeaves, bLeaves := goLinks(a), goLinks(b)
	listed := append(append(slices.Clone(aLeaves), bLeaves...), standalone, uncovered)
	p := Breadth([]string{a, b, standalone}, listed)
	breadth, ok := ApplyBreadth(breadthAnswers(p, map[string]float64{a: .9, standalone: .5}), p, .5)
	if !ok {
		t.Fatal("unusable breadth")
	}
	depth := Depth(breadth)
	if _, ok := ApplyDepth(systemone.Response{}, depth); ok {
		t.Fatal("missing choice was usable")
	}
	got := Finest(depth)
	want := distinct(append(slices.Clone(aLeaves), standalone, uncovered))
	if !slices.Equal(got.Links, want) || got.Levels[a] != "go-test" {
		t.Fatalf("fallback %+v, want %q", got, want)
	}
	assertLeavesCovered(t, want, got.Links)
}

func TestDepthDerivesArbitraryFiltersAndMultipleModules(t *testing.T) {
	t.Parallel()
	root := "dag+check://collection"
	var leaves []string
	for _, group := range []string{"a", "b"} {
		for _, item := range []string{"x", "y"} {
			leaves = append(leaves, root+"?group="+group+"&item="+item)
		}
	}
	p := Depth(allKept(t, []string{root}, leaves))
	c := p.Collections[0]
	if len(c.Hierarchy) != 2 || c.Hierarchy[0].Level != "group" || c.Hierarchy[0].Offered != true || c.Hierarchy[1].Level != "item" {
		t.Fatalf("hierarchy %+v", c.Hierarchy)
	}
	response := systemone.Response{Answers: map[string]systemone.Answer{c.Question: {Kind: systemone.KindChoice, Choice: "group"}}}
	got, ok := ApplyDepth(response, p)
	if !ok || !slices.Equal(got.Links, []string{root + "?group=a", root + "?group=b"}) {
		t.Fatalf("chosen group %+v, %t", got, ok)
	}
	assertLeavesCovered(t, leaves, got.Links)

	goRoot := "dag+check://tests"
	goLeaves := append(goLinks(goRoot), strings.Replace(goLinks(goRoot)[0], "go-module=.", "go-module=other", 1))
	goDepth := Depth(allKept(t, []string{goRoot}, goLeaves))
	if goDepth.Collections[0].Hierarchy[0].Offered != true {
		t.Fatal("multiple modules should offer the module level")
	}
}

func TestDepthRejectsInconsistentIntermediateOrder(t *testing.T) {
	t.Parallel()
	root := "dag+check://tests"
	leaves := goLinks(root)
	intermediate := root + "?go-package=a&go-module=."
	p := Depth(allKept(t, []string{root}, append(slices.Clone(leaves), intermediate)))
	if len(DepthRequest(map[string]any{"diff": "x"}, p).Questions) != 0 {
		t.Fatal("inconsistent intermediate filter order was offered")
	}
	got, ok := ApplyDepth(systemone.Response{}, p)
	if !ok || !slices.Equal(got.Links, leaves) {
		t.Fatalf("finest fallback %+v, %t", got, ok)
	}
}

func TestStagesAreDeterministic(t *testing.T) {
	t.Parallel()
	roots := []string{"dag+check://z", "dag+check://a", "dag+check://standalone"}
	listed := append(append(goLinks(roots[0]), goLinks(roots[1])...), roots[2])
	state, _ := State("")
	var previousBreadth, previousDepth, previousRanking []byte
	for range 5 {
		p := Breadth(roots, listed)
		b, _ := ApplyBreadth(breadthAnswers(p, nil), p, .5)
		d := Depth(b)
		r := systemone.Response{Answers: map[string]systemone.Answer{}}
		for _, c := range d.Collections {
			r.Answers[c.Question] = systemone.Answer{Kind: systemone.KindChoice, Choice: "go-package"}
		}
		filtered, ok := ApplyDepth(r, d)
		if !ok {
			t.Fatal("depth failed")
		}
		breadthBody, _ := BreadthRequest(state, p).Body("model")
		depthBody, _ := DepthRequest(state, d).Body("model")
		rankingBody, _ := Request(state, filtered.Links).Body("model")
		if previousBreadth != nil && (!slices.Equal(breadthBody, previousBreadth) || !slices.Equal(depthBody, previousDepth) || !slices.Equal(rankingBody, previousRanking)) {
			t.Fatal("stage identity changed with listing order")
		}
		previousBreadth, previousDepth, previousRanking = breadthBody, depthBody, rankingBody
		slices.Reverse(roots)
		slices.Reverse(listed)
	}
}

func TestStageAnswersMustBeUsable(t *testing.T) {
	t.Parallel()
	root := "dag+check://tests"
	p := Breadth([]string{root}, goLinks(root))
	for _, a := range []systemone.Answer{{Kind: systemone.KindChoice}, {Kind: systemone.KindNoul, Noul: -.1}, {Kind: systemone.KindNoul, Noul: 1.1}, {Kind: systemone.KindNoul, Noul: math.NaN()}, {Kind: systemone.KindNoul, Noul: math.Inf(1)}} {
		if _, ok := ApplyBreadth(systemone.Response{Answers: map[string]systemone.Answer{BreadthQuestion(0): a}}, p, .5); ok {
			t.Fatalf("usable invalid breadth answer %+v", a)
		}
	}
	if _, ok := ApplyBreadth(systemone.Response{}, p, .5); ok {
		t.Fatal("usable missing breadth answer")
	}
	extra := breadthAnswers(p, nil)
	extra.Answers["extra"] = systemone.Answer{Kind: systemone.KindNoul}
	if _, ok := ApplyBreadth(extra, p, .5); ok {
		t.Fatal("usable extra breadth answer")
	}
	d := Depth(allKept(t, []string{root}, goLinks(root)))
	for _, a := range []systemone.Answer{{Kind: systemone.KindNoul}, {Kind: systemone.KindChoice, Choice: "go-module"}, {Kind: systemone.KindChoice, Choice: "unknown"}} {
		if _, ok := ApplyDepth(systemone.Response{Answers: map[string]systemone.Answer{DepthQuestion(0): a}}, d); ok {
			t.Fatalf("usable invalid depth answer %+v", a)
		}
	}
	r := systemone.Response{Answers: map[string]systemone.Answer{DepthQuestion(0): {Kind: systemone.KindChoice, Choice: "go-test"}, "extra": {Kind: systemone.KindChoice, Choice: "go-test"}}}
	if _, ok := ApplyDepth(r, d); ok {
		t.Fatal("usable extra depth answer")
	}
	if reason := RankingAcceptance(systemone.Response{}, []string{root}, .5, 0); reason == "" {
		t.Fatal("usable missing ranking answer")
	}
	for _, tc := range []struct {
		probability float64
		picks       int
		accepted    bool
	}{{.5, 0, true}, {.49, 0, false}, {.1, 1, true}} {
		r := systemone.Response{Answers: map[string]systemone.Answer{Question(0): {Kind: systemone.KindNoul, Noul: tc.probability}}}
		if got := RankingAcceptance(r, []string{root}, .5, tc.picks); (got == "") != tc.accepted {
			t.Fatalf("ranking acceptance %q for %+v", got, tc)
		}
	}
}

func TestNarrowFilteredLinksAgainstOriginalLeaves(t *testing.T) {
	t.Parallel()
	if MaxQuestions != 1024 {
		t.Fatalf("cap %d", MaxQuestions)
	}
	root := "dag+check://tests?go-module=."
	var filtered []string
	for i := range MaxQuestions + 1 {
		filtered = append(filtered, fmt.Sprintf("%s&go-package=a&go-test=T%d", root, i))
	}
	listed := append(slices.Clone(filtered), root+"&go-package=b&go-test=Other")
	got, ok := Narrow(filtered, listed, MaxQuestions)
	if !ok || !slices.Equal(got, []string{root + "&go-package=a"}) {
		t.Fatalf("narrowed %q, %t", got, ok)
	}
	assertLeavesCovered(t, filtered, got)
	if _, ok := Candidates(filtered, MaxQuestions); ok {
		t.Fatal("whole-collection narrowing should fail without the original other package")
	}
	// A chosen parent must also survive broad-link removal against the original.
	if got, ok := Narrow([]string{root + "&go-package=a"}, listed, MaxQuestions); !ok || len(got) != 1 {
		t.Fatalf("filtered parent lost: %q, %t", got, ok)
	}
}

func TestRankingRequestWording(t *testing.T) {
	t.Parallel()
	if Task != "check-selection" || Version != 4 {
		t.Fatal("incorrect ranking identity")
	}
	state, _ := State("")
	link := "dag+check://tests?go-package=a"
	r := Request(state, []string{link})
	q := r.Questions["check-0"]
	want := map[string]any{"stage": "Stage 3 of 3, ranking. Earlier stages kept this check and chose how finely to divide its collection. Could the change in the state alter the result of the Dagger check `dag+check://tests?go-package=a`?", "link": link}
	if len(r.Questions) != 1 || q.Kind != systemone.KindNoul || !reflect.DeepEqual(q.Instructions, want) || q.Yes != "The check exercises a changed file, or code, configuration or generated files the change affects. It runs." || q.No != "Nothing the check depends on is touched by the change. It is skipped." {
		t.Fatalf("ranking %+v", r)
	}
	if !reflect.DeepEqual(r.State, state) {
		t.Fatal("ranking changed shared state")
	}
}

func TestStateCountsProcessAndExtendedGuide(t *testing.T) {
	t.Parallel()
	state, ok := State("")
	wantProcess := "Osmia is choosing which of a project's Dagger checks to run on a change, in three stages. Stage 1, breadth: for each top-level check, could the change affect it or any of its items at all? A no skips the check and everything in it. Stage 2, depth: for each collection kept by stage 1, at which level of its hierarchy should stage 3 judge it? Stage 3, ranking: for each remaining link, could the change alter its result? A yes runs it. Missing an affected check can let a regression through; running an unaffected one only costs time."
	wantGuideSuffix := "A collection's filters nest in the order they appear in its links. Each level's items contain the items of the levels after it. For Go, `go-module` (when listed) contains `go-package`, which names a package directory, which contains `go-test`, a test function."
	if !ok || len(state) != 4 || state["process"] != wantProcess || !strings.HasSuffix(state["checks"].(string), wantGuideSuffix) || state["changed_files"] == nil {
		t.Fatalf("state %+v", state)
	}
	if MaxState != 72*1024 {
		t.Fatalf("state bound %d", MaxState)
	}
	base, _ := json.Marshal(state)
	fitting := strings.Repeat("x", MaxState-len(base))
	state, ok = State(fitting)
	body, _ := json.Marshal(state)
	if !ok || len(body) != MaxState || state["diff"] != fitting {
		t.Fatalf("boundary diff did not fit: %d, %t", len(body), ok)
	}
	state, ok = State(fitting + "x")
	body, _ = json.Marshal(state)
	if !ok || len(body) > MaxState || !strings.HasPrefix(state["diff"].(string), "omitted:") {
		t.Fatal("oversized state kept diff")
	}
	// Without process that same diff would fit; process must count toward the cap.
	delete(state, "process")
	state["diff"] = fitting + "x"
	body, _ = json.Marshal(state)
	if len(body) >= MaxState {
		t.Fatal("fixture does not isolate process cost")
	}
}
