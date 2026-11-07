package checks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/checkselect"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

// scriptedChecks returns one scripted result for each Check and records its links.
type scriptedChecks struct {
	results []CheckResult
	errs    []error
	ran     [][]string
}

func (s *scriptedChecks) List(context.Context, string, Identity) ([]string, error) { return nil, nil }

func (s *scriptedChecks) Check(_ context.Context, _ string, _ Identity, links []string) (CheckResult, error) {
	i := len(s.ran)
	s.ran = append(s.ran, links)
	var err error
	if i < len(s.errs) {
		err = s.errs[i]
	}
	return s.results[i], err
}

func TestRunSelection(t *testing.T) {
	t.Parallel()
	listed := 9
	selected := api.CheckSelection{Mode: SelectionSelected, Links: []string{"dag+check://x"}, Candidates: 3, Listed: &listed, Judgment: "judgment_1"}
	refused := CheckResult{ExitCode: 1, Output: "Error: no checks matched\n", Refusal: "no checks matched", TraceURL: "https://dagger.cloud/refused"}
	passed := CheckResult{Output: "CHECKS  ✔ 2 passed\n"}

	for _, picks := range []int{0, 2} {
		t.Run(fmt.Sprintf("a refused selection with %d fallback picks runs every check and says why", picks), func(t *testing.T) {
			t.Parallel()
			selected := selected
			selected.FallbackPicks = picks
			checks := &scriptedChecks{results: []CheckResult{refused, {ExitCode: 3, Output: "CHECKS  ✘ 1 failed\n✘ dag://failed 1s ERROR\n    wrong\n", TraceURL: "https://dagger.cloud/full"}}}
			result, ran, err := RunSelection(context.Background(), checks, t.TempDir(), Identity{}, selected)
			if err != nil {
				t.Fatal(err)
			}
			if len(checks.ran) != 2 || !slices.Equal(checks.ran[0], selected.Links) || checks.ran[1] != nil {
				t.Fatalf("ran %q", checks.ran)
			}
			if ran.Mode != SelectionFull || ran.FallbackPicks != 0 || ran.Reason != SelectionRefused || ran.Detail != "no checks matched" || len(ran.Links) != 0 || ran.Judgment != "judgment_1" || ran.Candidates != 3 || ran.Listed != &listed {
				t.Fatalf("selection %+v", ran)
			}
			if result.ExitCode != 3 || result.TraceURL != "https://dagger.cloud/full" || result.Refusal != "" {
				t.Fatalf("result %+v", result)
			}
			if !strings.Contains(result.Output, "Error: no checks matched\n") || !strings.Contains(result.Output, "$ dagger check --progress=report --fail-fast --parallel 4\n") {
				t.Fatalf("output lacks the refused run or the full command:\n%s", result.Output)
			}
			if status, failed, _ := ClassifyChecks(result, nil); status != ChecksFailed || !slices.Equal(failed, []string{"dag://failed"}) {
				t.Fatalf("classified %s, %q", status, failed)
			}
			if want := "every check ran: Dagger refused the links Jev selected (no checks matched)"; DescribeSelection(ran) != want {
				t.Fatalf("description %q", DescribeSelection(ran))
			}
		})
	}
	t.Run("a refused selection keeps the flaky checks of both runs", func(t *testing.T) {
		t.Parallel()
		before, after := refused, passed
		before.Flaky, after.Flaky = []string{"dag://a", "dag://b"}, []string{"dag://b", "dag://c"}
		checks := &scriptedChecks{results: []CheckResult{before, after}}
		result, _, err := RunSelection(context.Background(), checks, t.TempDir(), Identity{}, selected)
		if err != nil || !slices.Equal(result.Flaky, []string{"dag://b", "dag://c", "dag://a"}) {
			t.Fatalf("flaky %q, err %v", result.Flaky, err)
		}
	})
	t.Run("a refused full run is not repeated", func(t *testing.T) {
		t.Parallel()
		full := FullSelection(SelectionUnlisted, "listing failed")
		checks := &scriptedChecks{results: []CheckResult{refused}}
		result, ran, err := RunSelection(context.Background(), checks, t.TempDir(), Identity{}, full)
		if err != nil || len(checks.ran) != 1 || ran.Reason != SelectionUnlisted || result.ExitCode != 1 {
			t.Fatalf("result %+v, selection %+v, ran %q, err %v", result, ran, checks.ran, err)
		}
	})
	t.Run("a run that was not refused keeps its selection", func(t *testing.T) {
		t.Parallel()
		for _, first := range []CheckResult{passed, {ExitCode: 1, Output: "Error: Post \"http://dagger/query\": unexpected EOF\n"}} {
			checks := &scriptedChecks{results: []CheckResult{first}}
			_, ran, err := RunSelection(context.Background(), checks, t.TempDir(), Identity{}, selected)
			if err != nil || len(checks.ran) != 1 || ran.Mode != SelectionSelected {
				t.Fatalf("selection %+v, ran %q, err %v", ran, checks.ran, err)
			}
		}
	})
	t.Run("an error stops the selection", func(t *testing.T) {
		t.Parallel()
		cause := errors.New("timed out")
		checks := &scriptedChecks{results: []CheckResult{refused}, errs: []error{cause}}
		if _, ran, err := RunSelection(context.Background(), checks, t.TempDir(), Identity{}, selected); !errors.Is(err, cause) || len(checks.ran) != 1 || ran.Mode != SelectionSelected {
			t.Fatalf("selection %+v, ran %q, err %v", ran, checks.ran, err)
		}
	})
	t.Run("a full run refused in turn stays incomplete with its error", func(t *testing.T) {
		t.Parallel()
		again := CheckResult{ExitCode: 1, Output: "Error: load workspace: broken\n", Refusal: "load workspace: broken"}
		checks := &scriptedChecks{results: []CheckResult{refused, again}}
		result, ran, err := RunSelection(context.Background(), checks, t.TempDir(), Identity{}, selected)
		if err != nil || len(checks.ran) != 2 || ran.Reason != SelectionRefused {
			t.Fatalf("selection %+v, ran %q, err %v", ran, checks.ran, err)
		}
		if status, _, reason := ClassifyChecks(result, nil); status != ChecksIncomplete || reason == nil || !strings.Contains(reason.Error(), "load workspace: broken") {
			t.Fatalf("classified %s, %v", status, reason)
		}
	})
}

func TestFallbackPicksPreserveThresholdAndFullRunPaths(t *testing.T) {
	t.Parallel()
	var large strings.Builder
	for i := range checkselect.MaxState / 10 {
		fmt.Fprintf(&large, "diff --git a/f%05d.go b/f%05d.go\n--- a/f%05d.go\n+++ b/f%05d.go\n@@ -1 +1 @@\n-a\n+b\n", i, i, i, i)
	}
	for _, tc := range []struct {
		name    string
		enabled bool
		diff    string
		result  jevtest.Result
		reason  string
	}{
		{name: "threshold", enabled: true, result: jevtest.Result{Response: systemone.Response{Model: "jev-test", Answers: map[string]systemone.Answer{"check-0": jevtest.Noul(.5), "check-1": jevtest.Noul(.1)}}}},
		{name: "threshold zero picks", enabled: true, result: jevtest.Result{Response: systemone.Response{Model: "jev-test", Answers: map[string]systemone.Answer{"check-0": jevtest.Noul(.5), "check-1": jevtest.Noul(.1)}}}},
		{name: "threshold oversized picks", enabled: true, result: jevtest.Result{Response: systemone.Response{Model: "jev-test", Answers: map[string]systemone.Answer{"check-0": jevtest.Noul(.5), "check-1": jevtest.Noul(.1)}}}},
		{name: "disabled", reason: string(jev.ReasonDisabled)},
		{name: "large change", enabled: true, diff: large.String(), reason: SelectionTooLarge},
		{name: "no usable scores", enabled: true, result: jevtest.Result{Response: systemone.Response{Model: "jev-test"}}, reason: string(jev.ReasonDeclined)},
		{name: "timeout", enabled: true, result: jevtest.Result{Err: &systemone.Error{Kind: systemone.KindTimeout, Message: "deadline"}}, reason: string(jev.ReasonTimeout)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Keep the trace socket and clone paths short on every platform.
			home, err := os.MkdirTemp("", "picks-")
			must(t, err)
			t.Cleanup(func() { os.RemoveAll(home) })
			cfg, err := config.Load(fixturekit.ConfigAt(t, home))
			must(t, err)
			repository, err := trace.Create(t.Context(), cfg.Root, cfg.Project, fixturekit.DemoStart, trace.Actor{Kind: "service", ID: "fixture"})
			must(t, err)
			t.Cleanup(func() { repository.Close() })
			stream := fixturekit.Stream
			must(t, repository.CreateWorkstream(t.Context(), stream, fixturekit.DemoStart, trace.Actor{Kind: "service", ID: "fixture"}))
			settings := cfg.Jev
			settings.Enabled = tc.enabled
			settings.CheckFallbackPicks = 2
			if tc.name == "threshold zero picks" {
				settings.CheckFallbackPicks = 0
			}
			if tc.name == "threshold oversized picks" {
				settings.CheckFallbackPicks = 9
			}
			settings.Timeout = "1s"
			p := &jevtest.Provider{Results: []jevtest.Result{tc.result}}
			j := &jev.Judge{Config: func() config.Jev { return settings }, Provider: p.Factory(), Getenv: func(string) string { return "key" }, Now: func() time.Time { return fixturekit.DemoStart }}
			checks := &fixturekit.FakeChecks{Links: []string{"dag+check://a", "dag+check://b"}, Outcomes: fixturekit.Passing()}
			scope := coreadapter.Scope{Project: string(cfg.Project.ID), Workstream: string(stream), Thread: "checks", Turn: "check-1", Role: "checks"}
			selection := Select(t.Context(), j, repository, scope, "check-1", nil, t.TempDir(), tc.diff, Identity{}, checks)
			_, err = checks.Check(t.Context(), t.TempDir(), Identity{}, selection.Links)
			must(t, err)
			if tc.reason == "" {
				if selection.Mode != SelectionSelected || selection.FallbackPicks != 0 || !slices.Equal(selection.Links, checks.Links[:1]) {
					t.Fatalf("threshold selection %+v", selection)
				}
			} else if selection.Mode != SelectionFull || selection.Reason != tc.reason || len(selection.Links) != 0 || selection.FallbackPicks != 0 {
				t.Fatalf("full selection %+v, want %s", selection, tc.reason)
			}
			if !slices.Equal(checks.Runs()[0], selection.Links) {
				t.Fatalf("runner %v, selection %+v", checks.Runs(), selection)
			}
			if (tc.name == "disabled" || tc.name == "large change") && len(p.Requests()) != 0 {
				t.Fatal("full run asked Jev")
			}
		})
	}
}

func TestSelectionDescriptionAndLegacyRecord(t *testing.T) {
	t.Parallel()
	var legacy api.CheckSelection
	must(t, json.Unmarshal([]byte(`{"mode":"selected","links":["dag+check://standalone"],"candidates":4,"listed":8,"judgment":"judgment_old"}`), &legacy))
	want := "Jev selected 1 of 4 check links for the changed files in judgment judgment_old"
	if DescribeSelection(legacy) != want || legacy.BreadthJudgment != "" || legacy.DepthJudgment != "" || legacy.Roots != nil || legacy.KeptRoots != nil || legacy.Levels != nil {
		t.Fatalf("legacy selection %+v: %s", legacy, DescribeSelection(legacy))
	}
	roots := 5
	legacy.Roots, legacy.KeptRoots = &roots, []string{"a", "b"}
	legacy.Levels = map[string]string{"a": "go-package", "b": "go-package"}
	if got := DescribeSelection(legacy); got != want+" within 2 of 5 top-level checks, by go-package" {
		t.Fatal(got)
	}
	legacy.FallbackPicks = 1
	if got := DescribeSelection(legacy); !strings.HasSuffix(got, "within 2 of 5 top-level checks, by go-package") || !strings.Contains(got, "highest-scored") {
		t.Fatal(got)
	}
}

func TestRefusalPreservesStageEvidence(t *testing.T) {
	t.Parallel()
	for _, useCloud := range []bool{false, true} {
		t.Run(fmt.Sprint(useCloud), func(t *testing.T) {
			t.Parallel()
			roots := 5
			selection := api.CheckSelection{Mode: SelectionSelected, Links: []string{"dag+check://chosen"}, Candidates: 3, Judgment: "rank", BreadthJudgment: "breadth", DepthJudgment: "depth", Roots: &roots, KeptRoots: []string{"a", "b"}, Levels: map[string]string{"a": "go-package"}}
			local := &scriptedChecks{results: []CheckResult{{ExitCode: 1, Output: "Error: no checks matched\n", Refusal: "no checks matched"}, {Output: "CHECKS  ✔ 1 passed\n"}}}
			var cloud ReviewChecks
			if useCloud {
				cloud = &scriptedChecks{}
			}
			_, full, _, err := RunSelectionWithFallback(t.Context(), local, cloud, time.Second, Remembered{}, t.TempDir(), Identity{}, selection)
			must(t, err)
			if full.Mode != SelectionFull || full.BreadthJudgment != "breadth" || full.DepthJudgment != "depth" || full.Judgment != "rank" || full.Roots == nil || *full.Roots != 5 || !slices.Equal(full.KeptRoots, selection.KeptRoots) || full.Levels["a"] != "go-package" {
				t.Fatalf("refused selection lost stage evidence: %+v", full)
			}
			if got := DescribeSelection(full); !strings.Contains(got, "; ranking considered links within 2 of 5 top-level checks, by go-package") {
				t.Fatal(got)
			}
		})
	}
}
