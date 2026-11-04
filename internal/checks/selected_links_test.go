package checks

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/servicetest/daggerfixture"
)

func TestDaggerFixtureProcess(t *testing.T) { daggerfixture.Process() }

// serial: installs an executable fake Dagger on PATH.
func TestSelectedLinksReachDaggerExactly(t *testing.T) {
	const a = daggerfixture.PackageA
	const b = "dag+check://?check=test&go-module=.&go-package=b"
	for name, links := range map[string][]string{
		"packages": {a, b}, "tests": {a + "&go-test=T1", a + "&go-test=T2"},
		"mixed": {a, daggerfixture.TestB}, "correlated": {a + "&go-test=T1", b + "&go-test=T2"},
		"without filters": {a, daggerfixture.Version}, "all": nil,
		"one": {daggerfixture.TestB},
	} {
		t.Run(name, func(t *testing.T) {
			cli := daggerfixture.Install(t, daggerfixture.Config{})
			result, err := (DaggerChecks{}).Check(t.Context(), t.TempDir(), Identity{}, links)
			must(t, err)
			if status, failed, why := ClassifyChecks(result, nil); status != ChecksPassed || len(failed) > 0 || why != nil {
				t.Fatalf("%s %q %v: %s", status, failed, why, result.Output)
			}
			cli.AssertRun(t, links)
			if !slices.Equal(result.Command, Command(links)) {
				t.Fatalf("actual %q, planned %q", result.Command, Command(links))
			}
		})
	}
}

// serial: tests the refusal contract independently of the runner's grouping.
func TestDaggerDoubleRefusesDifferentFiltersAndTypes(t *testing.T) {
	cli := daggerfixture.Install(t, daggerfixture.Config{})
	for _, links := range [][]string{
		{daggerfixture.PackageA, daggerfixture.TestB},
		{daggerfixture.PackageA, "dag+check://?check=test&go-module=.&go-package=b"},
		{"dag+check://release/version", "dag+lint://release/version"},
	} {
		cmd := exec.Command("dagger", checkArgs(links)...)
		output, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(output), daggerfixture.Refusal) || strings.Contains(string(output), "CHECKS") {
			t.Fatalf("old shape %q: %v %s", links, err, output)
		}
	}
	if events := cli.Events(t); len(events) != 3 {
		t.Fatalf("events %+v", events)
	}
}

// serial: uses the real selection execution and classification boundaries.
func TestSelectedRunStopsWithOneTruthfulResult(t *testing.T) {
	links := []string{daggerfixture.PackageA, daggerfixture.TestB, daggerfixture.Version}
	for name, c := range map[string]struct {
		cfg         daggerfixture.Config
		status      string
		exit, parts int
	}{
		"passed":    {daggerfixture.Config{}, ChecksPassed, 0, 3},
		"failed":    {daggerfixture.Config{Fail: daggerfixture.TestB}, ChecksFailed, 3, 3}, // includes the existing one-time rerun
		"no report": {daggerfixture.Config{ExitOn: "go-package=b"}, ChecksIncomplete, 7, 2},
	} {
		t.Run(name, func(t *testing.T) {
			cli := daggerfixture.Install(t, c.cfg)
			selected := api.CheckSelection{Mode: SelectionSelected, Links: links, Judgment: "unchanged"}
			result, ran, err := RunSelection(t.Context(), DaggerChecks{}, t.TempDir(), Identity{}, selected)
			must(t, err)
			if !reflect.DeepEqual(ran, selected) {
				t.Fatalf("selection changed: %+v", ran)
			}
			status, failed, reason := ClassifyChecks(result, nil)
			if status != c.status || result.ExitCode != c.exit {
				t.Fatalf("%s %d %v:\n%s", status, result.ExitCode, reason, result.Output)
			}
			var starts []daggerfixture.Event
			for _, e := range cli.Events(t) {
				if e.Kind == "start" {
					starts = append(starts, e)
				}
			}
			if len(starts) != c.parts {
				t.Fatalf("starts %+v", starts)
			}
			if c.status != ChecksPassed && strings.Contains(strings.Join(result.Command, " "), daggerfixture.Version) {
				t.Fatalf("record includes unstarted command: %q", result.Command)
			}
			if c.status == ChecksFailed && !slices.Equal(failed, []string{daggerfixture.TestB}) {
				t.Fatalf("failed %q", failed)
			}
			if c.status == ChecksIncomplete && result.TraceURL != "" {
				t.Fatalf("trace belongs to earlier passing part: %s", result.TraceURL)
			}
			summary, _ := SummarizeCheckOutput(result.Output, result.ExitCode)
			if c.status == ChecksPassed && strings.Count(summary, "CHECKS") != 3 {
				t.Fatalf("summary %q", summary)
			}
			if c.status == ChecksFailed && (!strings.Contains(summary, "wrong answer") || result.TraceURL != "https://dagger.cloud/fixture/traces/1") {
				t.Fatalf("summary %s trace %s", summary, result.TraceURL)
			}
		})
	}
}

// serial: each completed command spends less than the timeout, but their sum
// exceeds it; cancellation is also tested after a completed first part.
func TestSelectedRunSharesDeadlineAndCancellation(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline", true: "cancelled"}[cancelled], func(t *testing.T) {
			cli := daggerfixture.Install(t, daggerfixture.Config{Delay: time.Second})
			ctx, cancel := context.WithTimeout(t.Context(), 1900*time.Millisecond)
			defer cancel()
			if cancelled {
				go func() { time.Sleep(1700 * time.Millisecond); cancel() }()
			}
			result, err := (DaggerChecks{}).Check(ctx, t.TempDir(), Identity{}, []string{daggerfixture.PackageA, daggerfixture.TestB, daggerfixture.Version})
			want := error(context.DeadlineExceeded)
			if cancelled {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Fatalf("err %v", err)
			}
			if status, _, _ := ClassifyChecks(result, err); status != ChecksIncomplete {
				t.Fatalf("status %s", status)
			}
			if !strings.Contains(result.Output, "✔ 2 passed") || strings.Contains(strings.Join(result.Command, " "), daggerfixture.Version) {
				t.Fatalf("output %s commands %q", result.Output, result.Command)
			}
			if len(cli.Events(t)) != 3 {
				t.Fatalf("expected first start/end and interrupted second start: %+v", cli.Events(t))
			}
		})
	}
}

// serial: selected commands, reruns and fallback all use the real runner.
func TestSelectedRunResolvesFlakesBeforeLaterCommands(t *testing.T) {
	a1 := daggerfixture.PackageA + "&go-test=T1"
	b1 := daggerfixture.TestB
	links := []string{daggerfixture.PackageA, b1, daggerfixture.Version}
	cli := daggerfixture.Install(t, daggerfixture.Config{Attempts: []daggerfixture.Attempt{
		{Fail: a1}, {}, {}, // fail, rerun, finish a's cancelled T2
		{Fail: b1}, {}, {}, // fail, rerun, resume with nothing left
		{}, // later version check
	}})
	selected := api.CheckSelection{Mode: SelectionSelected, Links: links, FallbackPicks: 3, Judgment: "stable"}
	result, ran, err := RunSelection(t.Context(), DaggerChecks{}, t.TempDir(), Identity{}, selected)
	must(t, err)
	status, failed, reason := ClassifyChecks(result, nil)
	if status != ChecksPassed || result.ExitCode != 0 || len(failed) != 0 || reason != nil || !reflect.DeepEqual(ran, selected) || !slices.Equal(result.Flaky, []string{a1, b1}) {
		t.Fatalf("status %s failed %q flaky %q selection %+v: %v\n%s", status, failed, result.Flaky, ran, reason, result.Output)
	}
	cli.AssertRun(t, links)
	events := cli.Events(t)
	if len(events) != 14 || !slices.Equal(events[4].Links, []string{daggerfixture.PackageA + "&go-test=T2"}) || len(events[10].Links) != 0 || !slices.Equal(events[12].Links, []string{daggerfixture.Version}) {
		t.Fatalf("cancelled work or later commands not completed: %+v", events)
	}
	if strings.Count(result.Output, "$ dagger check") != 7 || !strings.Contains(result.Output, "--skip "+a1) || !strings.Contains(result.Output, "--skip "+b1) {
		t.Fatalf("missing command evidence: %s", result.Output)
	}
}

// serial: a late refusal is judged on that command even after earlier reports.
func TestPicksLateRefusalRunsFullSelection(t *testing.T) {
	a1 := daggerfixture.PackageA + "&go-test=T1"
	selected := api.CheckSelection{Mode: SelectionSelected, Links: []string{daggerfixture.PackageA, daggerfixture.TestB}, FallbackPicks: 2, Candidates: 3, Judgment: "stable"}
	cli := daggerfixture.Install(t, daggerfixture.Config{Attempts: []daggerfixture.Attempt{
		{Fail: a1}, {}, {}, {Refusal: daggerfixture.Refusal}, {},
	}})
	result, ran, err := RunSelection(t.Context(), DaggerChecks{}, t.TempDir(), Identity{}, selected)
	must(t, err)
	if status, _, why := ClassifyChecks(result, nil); status != ChecksPassed || why != nil {
		t.Fatalf("%s %v: %s", status, why, result.Output)
	}
	if ran.Mode != SelectionFull || ran.Reason != SelectionRefused || ran.Detail != daggerfixture.Refusal || len(ran.Links) != 0 || ran.FallbackPicks != 0 || ran.Judgment != selected.Judgment || ran.Candidates != selected.Candidates {
		t.Fatalf("finished selection %+v", ran)
	}
	if !slices.Equal(result.Command, Command(nil)) || !slices.Equal(result.Flaky, []string{a1}) || selected.FallbackPicks != 2 || len(selected.Links) != 2 {
		t.Fatalf("command %q flaky %q start %+v", result.Command, result.Flaky, selected)
	}
	before, full, ok := strings.Cut(result.Output, "Dagger refused the selected links, so every check runs.")
	if !ok || !strings.Contains(before, "CHECKS") || !strings.Contains(before, daggerfixture.Refusal) || !strings.Contains(full, "$ "+strings.Join(Command(nil), " ")) || !strings.Contains(full, "✔ 5 passed") {
		t.Fatalf("combined output: %s", result.Output)
	}
	cli.AssertRun(t, nil)
	if events := cli.Events(t); len(events) != 10 || len(events[8].Links) != len(daggerfixture.Leaves) {
		t.Fatalf("commands %+v", events)
	}
}

// serial: a fallback is bounded by the same context, and cannot turn a
// refused full run or a lost session into a pass.
func TestSelectedFallbackKeepsIncompleteAndDeadline(t *testing.T) {
	for _, name := range []string{"full refused", "fallback refused", "fallback deadline", "late engine error"} {
		t.Run(name, func(t *testing.T) {
			selection := api.CheckSelection{Mode: SelectionSelected, Links: []string{daggerfixture.PackageA, daggerfixture.TestB}, FallbackPicks: 2}
			attempts := []daggerfixture.Attempt{{}, {Refusal: daggerfixture.Refusal}, {Refusal: daggerfixture.Refusal}}
			parts := 3
			ctx := t.Context()
			switch name {
			case "full refused":
				selection = FullSelection("disabled", "")
				attempts, parts = attempts[1:2], 1
			case "fallback deadline":
				attempts = []daggerfixture.Attempt{{Delay: time.Second}, {Refusal: daggerfixture.Refusal}, {Delay: time.Second}}
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 1900*time.Millisecond)
				defer cancel()
			case "late engine error":
				attempts, parts = []daggerfixture.Attempt{{}, {ExitCode: 7}}, 2
			}
			cli := daggerfixture.Install(t, daggerfixture.Config{Attempts: attempts})
			result, ran, err := RunSelection(ctx, DaggerChecks{}, t.TempDir(), Identity{}, selection)
			if status, _, _ := ClassifyChecks(result, err); status != ChecksIncomplete {
				t.Fatalf("status %s: %v\n%s", status, err, result.Output)
			}
			if name == "fallback deadline" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("deadline %v", err)
			}
			if name == "late engine error" && !reflect.DeepEqual(ran, selection) {
				t.Fatalf("engine error caused fallback: %+v", ran)
			}
			starts := 0
			for _, e := range cli.Events(t) {
				if e.Kind == "start" {
					starts++
				}
			}
			if starts != parts {
				t.Fatalf("started %d parts, want %d", starts, parts)
			}
		})
	}
}

// serial: a rerun without a report cannot clear the reported failure.
func TestSelectedFailedCheckStandsWhenRerunHasNoResult(t *testing.T) {
	for name, rerun := range map[string]daggerfixture.Attempt{
		"refused":     {Refusal: daggerfixture.Refusal},
		"engine lost": {ExitCode: 7},
		"deadline":    {Delay: 3 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			cli := daggerfixture.Install(t, daggerfixture.Config{Attempts: []daggerfixture.Attempt{{}, {Fail: daggerfixture.TestB}, rerun}})
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			selection := api.CheckSelection{Mode: SelectionSelected, Links: []string{daggerfixture.PackageA, daggerfixture.TestB, daggerfixture.Version}, FallbackPicks: 3}
			result, ran, err := RunSelection(ctx, DaggerChecks{}, t.TempDir(), Identity{}, selection)
			must(t, err)
			status, failed, _ := ClassifyChecks(result, err)
			if status != ChecksFailed || !slices.Equal(failed, []string{daggerfixture.TestB}) || !reflect.DeepEqual(ran, selection) || result.Refusal != "" || len(result.Flaky) != 0 {
				t.Fatalf("%s failed %q selection %+v result %+v", status, failed, ran, result)
			}
			starts := 0
			for _, e := range cli.Events(t) {
				if e.Kind == "start" {
					starts++
				}
			}
			if starts != 3 {
				t.Fatalf("started %d commands", starts)
			}
		})
	}
}
