package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/api"
	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/checkselect"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func stageAnswer(answers map[string]systemone.Answer) jevtest.Result {
	return jevtest.Result{Response: systemone.Response{Model: "jev-test", Answers: answers, Usage: systemone.Usage{CostUSD: .01, CostKnown: true}}}
}

func breadthAnswer() jevtest.Result {
	return stageAnswer(map[string]systemone.Answer{"breadth-0": jevtest.Noul(.9), "breadth-1": jevtest.Noul(.01), "breadth-2": jevtest.Noul(.9)})
}

func depthAnswer() jevtest.Result {
	return stageAnswer(map[string]systemone.Answer{"depth-0": jevtest.Choice("go-package", .8, .8, "go-test")})
}

func rankingAnswer(links []string) jevtest.Result {
	answers := map[string]systemone.Answer{}
	for i := range links {
		answers[checkselect.Question(i)] = jevtest.Noul(.9)
	}
	return stageAnswer(answers)
}

func rankingLinks(t *testing.T, request systemone.Request) []string {
	t.Helper()
	links := []string{}
	for i := range len(request.Questions) {
		q := request.Questions[checkselect.Question(i)]
		data, ok := q.Instructions.(map[string]any)
		if !ok {
			t.Fatalf("ranking question: %+v", q)
		}
		link, ok := data["link"].(string)
		if !ok {
			t.Fatalf("ranking instructions: %+v", data)
		}
		links = append(links, link)
	}
	return links
}

func TestUnitChecksBreadthDepthRanking(t *testing.T) {
	t.Parallel()
	checks := fixturekit.SmartChecks()
	f, stream, repo := newChecksFixture(t, "smart-stages", checks)
	want := []string{fixturekit.SmartStandalone, fixturekit.SmartPackageA, fixturekit.SmartPackageB, fixturekit.SmartUncovered}
	slices.Sort(want)
	p := &jevtest.Provider{Results: []jevtest.Result{breadthAnswer(), depthAnswer(), rankingAnswer(want)}}
	boostJev(f.s, p)
	runChecks(t, f.s, repo, stream)
	run := onlyRun(t, repo, stream)
	s := run.Selection
	if run.Status != ChecksPassed || s == nil || s.Mode != checkrun.SelectionSelected || !slices.Equal(s.Links, want) || !slices.Equal(checks.Runs()[0], want) {
		t.Fatalf("run %+v selection %+v runner %q", run, s, checks.Runs())
	}
	if s.BreadthJudgment == "" || s.DepthJudgment == "" || s.Judgment == "" || s.Roots == nil || *s.Roots != 3 || !slices.Equal(s.KeptRoots, []string{fixturekit.SmartStandalone, fixturekit.SmartTestRoot}) || s.Levels[fixturekit.SmartTestRoot] != "go-package" {
		t.Fatalf("stage evidence %+v", s)
	}
	requests := p.Requests()
	if len(requests) != 3 {
		t.Fatalf("requests %d", len(requests))
	}
	if len(requests[0].Questions) != 3 || !strings.Contains(fmtInstructions(requests[0].Questions["breadth-2"]), fixturekit.SmartTestRoot) || !strings.Contains(fmtInstructions(requests[0].Questions["breadth-2"]), "TestOne") || !strings.Contains(fmtInstructions(requests[1].Questions["depth-0"]), fixturekit.SmartTestRoot) || !slices.Equal(rankingLinks(t, requests[2]), want) {
		t.Fatalf("requests %+v", requests)
	}
	for _, request := range requests {
		if !reflect.DeepEqual(request.State, requests[0].State) {
			t.Fatal("stages did not share state")
		}
	}
	docs, err := trace.Read[trace.Document](repo, stream)
	must(t, err)
	for i, id := range []string{s.BreadthJudgment, s.DepthJudgment, s.Judgment} {
		var record jev.Record
		found := false
		for _, d := range docs {
			if d.ID == id && d.Revision == 2 {
				must(t, json.Unmarshal([]byte(d.Content), &record))
				found = true
			}
		}
		if !found || record.Task != []string{"check-breadth", "check-depth", "check-selection"}[i] || record.Version != []int{1, 1, 4}[i] || record.Scope.Role != "checks" {
			t.Fatalf("judgment %d: %+v", i, record)
		}
	}
	costs, err := trace.Read[trace.Cost](repo, stream)
	must(t, err)
	if len(costs) != 3 {
		t.Fatalf("costs %+v", costs)
	}
	for _, cost := range costs {
		if cost.Entry.Scope.Role != "jev" || cost.Entry.Usage.CostUSD != .01 {
			t.Fatalf("cost %+v", cost)
		}
	}
	var start UnitCheckSelection
	must(t, json.Unmarshal([]byte(docContent(t, docs, checkSelectionPath("resume", run.Run))), &start))
	if !reflect.DeepEqual(start.Selection, *s) {
		t.Fatalf("recorded selection %+v, want %+v", start.Selection, s)
	}
	if !strings.Contains(checkrun.DescribeSelection(*s), "within 2 of 3 top-level checks, by go-package") {
		t.Fatal(checkrun.DescribeSelection(*s))
	}
	if !slices.Equal(checks.Calls, []string{"all", "top", fixturekit.SmartStandalone, fixturekit.SmartStaleRoot, fixturekit.SmartTestRoot}) {
		t.Fatalf("listing calls %q", checks.Calls)
	}
	t.Logf("two listings plus %d scoped listings took %s in the fake", len(checks.Roots), checks.ListingTime)

	// A new Judge models a restart; all three identities and answers are durable.
	boostJev(f.s, p)
	c := &checkers{masons: f.controller()}
	ops, err := checkOperations(repo, stream)
	must(t, err)
	g, err := newUnitWorkspaces(f.cfg, repo).Of(stream)
	must(t, err)
	diff, err := g.Diff(context.Background(), run.Base, run.Candidate)
	must(t, err)
	again := c.selectChecks(t.Context(), ops[0].input, t.TempDir(), diff, checks)
	if !reflect.DeepEqual(again, *s) || len(p.Requests()) != 3 {
		t.Fatalf("restart selection %+v requests %d", again, len(p.Requests()))
	}
	// The immutable unit record is restored even when every listing now fails.
	checks.ListErr = errors.New("listing must not run after restore")
	calls := len(checks.Calls)
	restored := UnitCheckRun{Operation: run.Operation}
	saved, err := c.selection(t.Context(), ops[0].input, &restored, t.TempDir(), diff, checks)
	must(t, err)
	if !reflect.DeepEqual(saved, *s) || len(checks.Calls) != calls || len(p.Requests()) != 3 {
		t.Fatalf("restored %+v", saved)
	}
}

func fmtInstructions(q systemone.Question) string {
	b, _ := json.Marshal(q.Instructions)
	return string(b)
}

func docContent(t *testing.T, docs []trace.Document, path string) string {
	t.Helper()
	for _, d := range docs {
		if d.Path == path {
			return d.Content
		}
	}
	t.Fatalf("missing document %s", path)
	return ""
}

func TestUnitChecksStageFallbacks(t *testing.T) {
	t.Parallel()
	timeout := jevtest.Result{Err: &systemone.Error{Kind: systemone.KindTimeout, Message: "deadline"}}
	unusable := stageAnswer(nil)
	for _, scenario := range []string{"breadth timeout", "breadth unusable", "top listing failed", "scoped listing failed", "missing scoped support", "depth timeout", "depth unusable", "ranking timeout", "cool-down", "all listing failed"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			checks := fixturekit.SmartChecks()
			var runner ReviewChecks = checks
			full, ok := checkselect.Candidates(checks.Links, checkselect.MaxQuestions)
			if !ok {
				t.Fatal("bad fixture")
			}
			kept := append([]string{fixturekit.SmartStandalone, fixturekit.SmartUncovered}, checks.Members[fixturekit.SmartTestRoot]...)
			slices.Sort(kept)
			filtered := []string{fixturekit.SmartStandalone, fixturekit.SmartPackageA, fixturekit.SmartPackageB, fixturekit.SmartUncovered}
			slices.Sort(filtered)
			want, results, rankingIndex, reason := full, []jevtest.Result{rankingAnswer(full)}, 0, ""
			switch scenario {
			case "breadth timeout":
				results = []jevtest.Result{timeout, timeout, rankingAnswer(full)}
				rankingIndex = 2
			case "breadth unusable":
				results = []jevtest.Result{unusable, rankingAnswer(full)}
				rankingIndex = 1
			case "top listing failed":
				checks.TopErr = errors.New("top listing failed")
			case "scoped listing failed":
				checks.ScopedErr = errors.New("scoped listing failed")
			case "missing scoped support":
				runner = &topOnlyChecks{fakeChecks: checks.FakeChecks, roots: checks.Roots}
			case "depth timeout":
				want = kept
				results = []jevtest.Result{breadthAnswer(), timeout, timeout, rankingAnswer(kept)}
				rankingIndex = 3
			case "depth unusable":
				want = kept
				results = []jevtest.Result{breadthAnswer(), unusable, rankingAnswer(kept)}
				rankingIndex = 2
			case "ranking timeout":
				want = nil
				results = []jevtest.Result{breadthAnswer(), depthAnswer(), timeout}
				rankingIndex = 2
				reason = string(jev.ReasonTimeout)
			case "cool-down":
				want = nil
				results = []jevtest.Result{timeout, timeout, timeout}
				reason = string(jev.ReasonCoolingDown)
			case "all listing failed":
				want = nil
				checks.ListErr = errors.New("engine unavailable")
				results = nil
				reason = checkrun.SelectionUnlisted
			}
			f, stream, repo := newChecksFixture(t, "stage-fallback", runner)
			p := &jevtest.Provider{Results: results}
			boostJev(f.s, p)
			if scenario == "cool-down" {
				// Three consecutive failures put the shared provider into cool-down.
				for i := range 3 {
					f.s.Jev.Evaluate(t.Context(), repo, jev.Judgment{Scope: coreadapter.Scope{Project: string(f.cfg.Project.ID), Workstream: string(stream), Thread: "checks", Turn: "warm", Role: "checks"}, Cause: []string{"warm-1", "warm-2", "warm-3"}[i], Task: "warm", Version: 1, Request: checkselect.Request(map[string]any{}, []string{"dag+check://warm"})})
				}
				if f.s.Jev.Status().CoolingUntil == nil {
					t.Fatal("provider did not cool down")
				}
			}
			before := len(p.Requests())
			runChecks(t, f.s, repo, stream)
			run := onlyRun(t, repo, stream)
			s := run.Selection
			if s == nil || run.Status != ChecksPassed || !slices.Equal(checks.Runs()[0], want) || !slices.Equal(s.Links, want) {
				t.Fatalf("selection %+v, ran %q want %q", s, checks.Runs(), want)
			}
			if reason != "" {
				if s.Mode != checkrun.SelectionFull || s.Reason != reason {
					t.Fatalf("selection %+v want %s", s, reason)
				}
			} else if s.Mode != checkrun.SelectionSelected {
				t.Fatalf("selection %+v", s)
			}
			requests := p.Requests()
			if scenario == "cool-down" || scenario == "all listing failed" {
				if len(requests) != before {
					t.Fatalf("fallback sent %d requests", len(requests)-before)
				}
				if scenario == "all listing failed" && s.Listed != nil {
					t.Fatal("failed all listing has a count")
				}
			} else {
				expectedCount := rankingIndex + 1
				if scenario == "ranking timeout" {
					expectedCount++
				}
				if len(requests) != expectedCount {
					t.Fatalf("requests %d want %d", len(requests), expectedCount)
				}
				expected := want
				if scenario == "ranking timeout" {
					expected = filtered
				}
				if !slices.Equal(rankingLinks(t, requests[rankingIndex]), expected) {
					t.Fatalf("ranking got %q want %q", rankingLinks(t, requests[rankingIndex]), expected)
				}
			}
			if strings.HasPrefix(scenario, "depth") && (s.Roots == nil || len(s.KeptRoots) != 2 || s.Levels[fixturekit.SmartTestRoot] != "go-test") {
				t.Fatalf("depth fallback lost breadth %+v", s)
			}
		})
	}
}

// topOnlyChecks represents a runner which can list roots but lacks scoped listing.
type topOnlyChecks struct {
	*fakeChecks
	roots []string
}

func (c *topOnlyChecks) ListTopLevel(context.Context, string, api.CheckIdentity) ([]string, error) {
	return c.roots, nil
}

func TestUnitChecksNarrowAfterDepthFailure(t *testing.T) {
	t.Parallel()
	checks := fixturekit.SmartChecks()
	leaves := []string{fixturekit.SmartPackageB + "&go-test=TestOne", fixturekit.SmartPackageB + "&go-test=TestTwo"}
	for i := range checkselect.MaxQuestions + 1 {
		leaves = append(leaves, fmt.Sprintf("%s&go-test=Test%d", fixturekit.SmartPackageA, i))
	}
	checks.Members[fixturekit.SmartTestRoot] = leaves
	checks.Links = append(append([]string{fixturekit.SmartStandalone, fixturekit.SmartUncovered}, checks.Members[fixturekit.SmartStaleRoot]...), leaves...)
	want := []string{fixturekit.SmartStandalone, fixturekit.SmartUncovered, fixturekit.SmartPackageA, fixturekit.SmartPackageB + "&go-test=TestOne", fixturekit.SmartPackageB + "&go-test=TestTwo"}
	slices.Sort(want)
	timeout := jevtest.Result{Err: &systemone.Error{Kind: systemone.KindTimeout, Message: "deadline"}}
	p := &jevtest.Provider{Results: []jevtest.Result{breadthAnswer(), timeout, timeout, rankingAnswer(want)}}
	f, stream, repo := newChecksFixture(t, "depth-narrow", checks)
	boostJev(f.s, p)
	runChecks(t, f.s, repo, stream)
	run := onlyRun(t, repo, stream)
	if run.Status != ChecksPassed || !slices.Equal(run.Selection.Links, want) || !slices.Equal(checks.Runs()[0], want) || run.Selection.Candidates != len(want) || run.Selection.Levels[fixturekit.SmartTestRoot] != "go-test" || len(p.Requests()) != 4 {
		t.Fatalf("run %+v selection %+v runner %q", run, run.Selection, checks.Runs())
	}
	if got := rankingLinks(t, p.Requests()[3]); !slices.Equal(got, want) {
		t.Fatalf("ranking links %q want %q", got, want)
	}
}
