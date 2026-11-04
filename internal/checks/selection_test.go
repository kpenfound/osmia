package checks

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
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

	t.Run("a refused selection runs every check and says why", func(t *testing.T) {
		t.Parallel()
		checks := &scriptedChecks{results: []CheckResult{refused, {ExitCode: 3, Output: "CHECKS  ✘ 1 failed\n✘ dag://failed 1s ERROR\n    wrong\n", TraceURL: "https://dagger.cloud/full"}}}
		result, ran, err := RunSelection(context.Background(), checks, t.TempDir(), Identity{}, selected)
		if err != nil {
			t.Fatal(err)
		}
		if len(checks.ran) != 2 || !slices.Equal(checks.ran[0], selected.Links) || checks.ran[1] != nil {
			t.Fatalf("ran %q", checks.ran)
		}
		if ran.Mode != SelectionFull || ran.Reason != SelectionRefused || ran.Detail != "no checks matched" || len(ran.Links) != 0 || ran.Judgment != "judgment_1" || ran.Candidates != 3 || ran.Listed != &listed {
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
