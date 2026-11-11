package checks

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

func TestCheckReportClassifiesLifecycleFailureAndKeepsDiagnostics(t *testing.T) {
	t.Parallel()
	for _, styling := range []string{"plain", "ansi", "spacing"} {
		t.Run(styling, func(t *testing.T) {
			raw := fixturekit.LifecycleFailureOutput()
			switch styling {
			case "ansi":
				raw = strings.ReplaceAll(raw, "CHECKS", "\x1b[1mCHECKS\x1b[0m")
				raw = strings.ReplaceAll(raw, "✘", "\x1b[31m✘\x1b[0m")
			case "spacing":
				raw = strings.ReplaceAll(raw, "CHECKS  ✘", "CHECKS\t ✘")
				raw = strings.ReplaceAll(raw, "✘ dag://", "✘  dag://")
			}
			status, failed, err := ClassifyChecks(CheckResult{ExitCode: 1, Output: raw}, nil)
			if err != nil || status != ChecksFailed || !slices.Equal(failed, []string{"dag://?check=test&go-package=internal/lifecycle"}) {
				t.Fatalf("classification: %s, %v, %v", status, failed, err)
			}
			report, truncated := SummarizeCheckOutput(raw, 1)
			if truncated || !strings.Contains(report, "lifecycle_test.go:42: expected pending change to apply") ||
				!strings.Contains(report, "TestChangePassAppliesPendingChangeAfterArchivedCrossRepoLeader FAIL") ||
				strings.Contains(report, "progress ") || strings.Contains(report, "passing output") || strings.Contains(report, "\x1b") {
				t.Fatalf("diagnostics (truncated=%t): %s", truncated, report)
			}
		})
	}
}

func TestClassifyChecksRequiresFailedCheckForNonzeroExit(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]string{
		"engine":         "Error: engine unavailable\n",
		"cli":            "Error: unknown flag: --unrecognised\n",
		"pending":        "CHECKS\n◌ dag://?check=test pending\n",
		"summary only":   "CHECKS  ✘ 1 failed\n",
		"shutdown":       fixturekit.PassedReport + "Error: session shutdown: connection reset\n",
		"outside report": "! exit code 1\n✘ dag://not-a-report 1s ERROR\n",
	} {
		t.Run(name, func(t *testing.T) {
			for _, exit := range []int{-1, 1, 3, 137} {
				status, failed, err := ClassifyChecks(CheckResult{ExitCode: exit, Output: raw}, nil)
				if status != ChecksIncomplete || len(failed) != 0 || err == nil || !strings.Contains(err.Error(), "without reporting a failed check") {
					t.Fatalf("exit %d: %s, %v, %v", exit, status, failed, err)
				}
			}
		})
	}
	for _, raw := range []string{"", fixturekit.PassedReport, fixturekit.LifecycleFailureOutput()} {
		status, failed, err := ClassifyChecks(CheckResult{Output: raw}, nil)
		if status != ChecksPassed || len(failed) != 0 || err != nil {
			t.Fatalf("exit 0: %s, %v, %v", status, failed, err)
		}
	}
}

func TestFailedChecksReadEachChecksLatestResult(t *testing.T) {
	t.Parallel()
	const (
		failedThenPassed = "CHECKS  ✘ 1 failed\n✘ dag://a 1s ERROR\n    first\nCHECKS  ✔ 1 passed\n✔ dag://a 1s OK\n"
		failedAgain      = "CHECKS  ✘ 1 failed\n✘ dag://a 1s ERROR\n    first\nCHECKS  ✘ 1 failed\n✘ dag://a 1s ERROR\n    second\n"
		passedThenFailed = failedThenPassed + "CHECKS  ✘ 1 failed\n✘ dag://a 1s ERROR\n    third\n"
	)
	if got := FailedChecks(failedThenPassed); got != nil {
		t.Fatalf("failed then passed: %q", got)
	}
	if got := FailedChecks(failedAgain); !slices.Equal(got, []string{"dag://a"}) {
		t.Fatalf("failed again: %q", got)
	}
	if got := FailedChecks(passedThenFailed); !slices.Equal(got, []string{"dag://a"}) {
		t.Fatalf("passed then failed: %q", got)
	}
	summary, _ := SummarizeCheckOutput(failedAgain, 1)
	if strings.Contains(summary, "first") || !strings.Contains(summary, "second") || strings.Contains(summary, "passed on rerun") {
		t.Fatalf("a repeated failure keeps its latest diagnostics: %q", summary)
	}
	summary, _ = SummarizeCheckOutput(failedThenPassed, 0)
	if !strings.Contains(summary, "first") || !strings.Contains(summary, "✘ dag://a 1s ERROR [passed on rerun]") {
		t.Fatalf("a flake keeps its evidence, marked: %q", summary)
	}
	if got := FailedChecks("CHECKS  ✘ 1 failed\n✘ dag://a 1s ERROR\n              ✔ dag://a 1s OK\n"); !slices.Equal(got, []string{"dag://a"}) {
		t.Fatalf("a passing line in a failure's output is not a result: %q", got)
	}
}

func TestDescribeFlakyNamesTheChecks(t *testing.T) {
	t.Parallel()
	if got := DescribeFlaky(nil); got != "" {
		t.Fatalf("no flakes: %q", got)
	}
	if got := DescribeFlaky([]string{"dag://a", "dag://b"}); !strings.Contains(got, "dag://a, dag://b") || !strings.Contains(got, "rerun") {
		t.Fatalf("flakes: %q", got)
	}
}

func TestCheckReportReadsDecoratedHeading(t *testing.T) {
	t.Parallel()
	raw := "== CHECKS ==  ✘ 1 failed\n✘ dag://check 1s ERROR\n    assertion failed\n== RUN LOCALLY ==\nrerun\n"
	if got := FailedChecks(raw); !slices.Equal(got, []string{"dag://check"}) {
		t.Fatalf("failures: %v", got)
	}
	report, _ := SummarizeCheckOutput(raw, 1)
	if !strings.Contains(report, "assertion failed") || strings.Contains(report, "rerun") {
		t.Fatalf("diagnostics: %s", report)
	}
}

func TestCheckReportManyFailuresRetainsDiagnosticsAndFullClassification(t *testing.T) {
	t.Parallel()
	var raw strings.Builder
	raw.WriteString("CHECKS  ✘ 1000 failed\n")
	for i := range 1000 {
		fmt.Fprintf(&raw, "✘ dag://check-%d 1s ERROR\n    assertion %d failed\n", i, i)
	}
	report, truncated := SummarizeCheckOutput(raw.String(), 1)
	if !truncated || len(report) > ReportLimit || !strings.Contains(report, "Showing 32 of 1000") || !strings.Contains(report, "assertion 0 failed") || !strings.Contains(report, "assertion 31 failed") || len(FailedChecks(raw.String())) != 1000 {
		t.Fatalf("failed checks lost: %s", report)
	}
}

func TestCheckReportKeepsFailuresBeforeLargePassingAndRerunOutput(t *testing.T) {
	t.Parallel()
	link := "dag://?check=go/packages/tests/test&go-package=internal/service" + strings.Repeat("&go-test=TestPassing", 5000)
	raw := "[dagger] loading\n" + failedReport(link) +
		"              ✔ dag://quoted-in-a-test 1s OK\n              keep this diagnostic too\n" +
		"✔ dag://passing 1s OK\n" + strings.Repeat("    successful output\n", 5000) +
		"RUN LOCALLY\n  dagger check \"" + link + "\"\n"
	status, failed, err := ClassifyChecks(CheckResult{ExitCode: 1, Output: raw}, nil)
	if err != nil || status != ChecksFailed || !slices.Equal(failed, []string{link}) {
		t.Fatalf("classification: %s, %d failures, %v", status, len(failed), err)
	}
	report, truncated := SummarizeCheckOutput(raw, 1)
	if truncated || len(report) > ReportLimit || !strings.Contains(report, "built_test.go:9: wrong chunk") || !strings.Contains(report, "keep this diagnostic too") || !strings.Contains(report, "[output lines 5-") {
		t.Fatalf("diagnostics (truncated=%t): %s", truncated, report)
	}
	for _, unwanted := range []string{"[dagger] loading", "successful output", "RUN LOCALLY", "✔ dag://passing"} {
		if strings.Contains(report, unwanted) {
			t.Fatalf("report includes %q", unwanted)
		}
	}
}

func TestCheckReportSharesSpaceBetweenFailures(t *testing.T) {
	t.Parallel()
	raw := "CHECKS  ✘ 2 failed\n✘ dag://first 1s ERROR\n    first assertion\n" +
		strings.Repeat("é", 50000) + "\n    first stack tail\n✘ dag://second 1s ERROR\n    second assertion\n"
	report, truncated := SummarizeCheckOutput(raw, 1)
	if !truncated || len(report) > ReportLimit || !utf8.ValidString(report) {
		t.Fatalf("report: truncated=%t bytes=%d valid=%t", truncated, len(report), utf8.ValidString(report))
	}
	for _, want := range []string{"first assertion", "first stack tail", "second assertion", "omitted"} {
		if !strings.Contains(report, want) {
			t.Fatalf("missing %q", want)
		}
	}
	evidence := OutputEvidence(report, "units/u/checks-1-output.txt", 1, truncated)
	if !strings.Contains(evidence, "factory_context") || !strings.Contains(evidence, "excerpt is truncated") {
		t.Fatalf("missing retrieval instructions: %s", evidence)
	}
}

func TestCheckReportReadsNestedFailuresAndColors(t *testing.T) {
	t.Parallel()
	raw := "CHECKS  ✘ 1 failed\n\x1b[31m✘ dag://parent 1s ERROR\x1b[0m\n  CHECKS\n  ✘ dag+check://child 1s ERROR\n    compiler: missing symbol\n  ✔ dag://sibling 1s OK\n    passing detail\n✔ dag://other 1s OK\nRUN LOCALLY\nrerun\n"
	if got := FailedChecks(raw); !slices.Equal(got, []string{"dag://parent", "dag+check://child"}) {
		t.Fatalf("failures: %v", got)
	}
	report, truncated := SummarizeCheckOutput(raw, 1)
	if truncated || !strings.Contains(report, "compiler: missing symbol") || strings.Contains(report, "passing detail") || strings.Contains(report, "rerun") || strings.Contains(report, "\x1b") {
		t.Fatalf("report: %s", report)
	}
}

func TestCheckReportReadsChecksListedInsideAGroup(t *testing.T) {
	t.Parallel()
	raw := "CHECKS  ✘ 1 failed  ✔ 1 passed\n" +
		"✘ go/tests/test 0.2s ERROR\n" +
		"  CHECKS  ✘ 1 failed  ✔ 1 passed\n" +
		"    ✘ dag://?check=test&go-test=TestBroken 0.1s ERROR\n" +
		"      ! wrong answer\n" +
		"    ✔ dag://?check=test&go-test=TestOne 0.1s OK\n" +
		"✔ dag://?check=other 0.0s OK\n" +
		"RUN LOCALLY\n  dagger check x\n"
	if got := FailedChecks(raw); !slices.Equal(got, []string{"dag://?check=test&go-test=TestBroken"}) {
		t.Fatalf("failures: %v", got)
	}
	if got := PassedChecks(raw); !slices.Equal(got, []string{"dag://?check=test&go-test=TestOne", "dag://?check=other"}) {
		t.Fatalf("passed: %v", got)
	}
	status, failed, err := ClassifyChecks(CheckResult{ExitCode: 1, Output: raw}, nil)
	if status != ChecksFailed || err != nil || !slices.Equal(failed, []string{"dag://?check=test&go-test=TestBroken"}) {
		t.Fatalf("classified %s %v: %v", status, failed, err)
	}
}

func TestNestedGroupFailureIsClassifiedFailed(t *testing.T) {
	t.Parallel()
	raw := "CHECKS  ✘ 1 failed\n" +
		"✘ go/modules/packages/tests/test 0.2s ERROR\n" +
		"  CHECKS  ✘ 1 failed  ✔ 1 passed\n" +
		"    ✘ dag://?check=test&go-module=.&go-package=broken&go-test=TestBroken 0.1s ERROR\n" +
		"      ! Go tests in broken: TestBroken failed\n" +
		"        ✔ dag://?check=quoted 0.0s OK\n" +
		"    ✔ dag://?check=test&go-module=.&go-package=a&go-test=TestOne 0.1s OK\n"
	link := "dag://?check=test&go-module=.&go-package=broken&go-test=TestBroken"
	if got := FailedChecks(raw); !slices.Equal(got, []string{link}) {
		t.Fatalf("failures: %v", got)
	}
	if got := PassedChecks(raw); slices.Contains(got, "dag://?check=quoted") {
		t.Fatalf("quoted output read as a result: %v", got)
	}
	status, failed, err := ClassifyChecks(CheckResult{ExitCode: 1, Output: raw}, nil)
	if status != ChecksFailed || err != nil || !slices.Equal(failed, []string{link}) {
		t.Fatalf("classified %s %v: %v", status, failed, err)
	}
}

func TestCheckReportKeepsUnrecognizedAndInfrastructureOutput(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"Error: engine unavailable\n",
		"compiler: undefined symbol\n" + strings.Repeat("verbose detail\n", 10000) + "exit code 1\n",
		"CHECKS  ◌ 1 pending\nengine disconnected\n",
	} {
		report, truncated := SummarizeCheckOutput(raw, 1)
		if len(report) > ReportLimit || truncated != (len(raw) > ReportLimit) || !strings.HasPrefix(report, strings.Split(raw, "\n")[0]) || !strings.HasSuffix(report, strings.Split(raw, "\n")[len(strings.Split(raw, "\n"))-2]+"\n") {
			t.Fatalf("fallback report: %s", report)
		}
		status, _, err := ClassifyChecks(CheckResult{ExitCode: 1, Output: raw}, nil)
		if status != ChecksIncomplete || err == nil {
			t.Fatalf("unreported failure classified as %s: %v", status, err)
		}
	}
}

func TestClassifyChecksRepeatsDaggersLastError(t *testing.T) {
	t.Parallel()
	_, _, reason := ClassifyChecks(CheckResult{ExitCode: 1, Output: "Error: first\n\x1b[31mError: second problem\x1b[0m\ncleanup failed\n"}, nil)
	if want := "dagger check exited 1 without reporting a failed check: second problem"; reason == nil || reason.Error() != want {
		t.Fatalf("reason %v, want %q", reason, want)
	}
	_, _, reason = ClassifyChecks(CheckResult{ExitCode: 1, Output: "Error: " + strings.Repeat("x", 4*errorLimit) + "\n"}, nil)
	if reason == nil || len(reason.Error()) > errorLimit+128 {
		t.Fatalf("reason is unbounded: %d bytes", len(reason.Error()))
	}
}

func TestRefusalRecognisesARunRefusedBeforeAnyCheckStarted(t *testing.T) {
	t.Parallel()
	const differentFilters = "addresses with different filters require a union of workspace views; use shared filter flags"
	for name, test := range map[string]struct {
		output string
		exit   int
		want   string
	}{
		"different filters":    {"TRACE  ✘ FAILED\n! " + differentFilters + "\n\n✔ connect 0.1s\n✘ prepare artifacts 0.3s ERROR\n! " + differentFilters + "\nError: " + differentFilters + "\n", 1, differentFilters},
		"no matching check":    {"== TRACE ==  ✘ FAILED\n! no checks matched pattern \"dag+check://x\"\n\nError: no checks matched pattern \"dag+check://x\"\n", 1, "no checks matched pattern \"dag+check://x\""},
		"after modules loaded": {"[dagger] 30.0s elapsed · 15 steps running\nError: no checks matched pattern \"dag+check://x\"\n", 1, "no checks matched pattern \"dag+check://x\""},
		"failed check":         {fixturekit.LifecycleFailureOutput() + "Error: exit code 1\n", 1, ""},
		"passed then lost":     {fixturekit.PassedReport + "Error: session shutdown: connection reset\n", 1, ""},
		"checks started":       {"[dagger] 7m0s elapsed · checks: 1/5 done · running: dag://?check=a (10s)\nError: something broke\n", 1, ""},
		"engine lost":          {"[dagger] 6m0s elapsed · 732 steps running\nError: Post \"http://dagger/query\": unexpected EOF\n", 1, ""},
		"session closed":       {"Error: engine closed session \"x\": it closed this client's connection\n", 1, ""},
		"no error":             {"killed\n", 137, ""},
		"did not exit":         {"Error: no checks matched\n", -1, ""},
		"passed":               {"Error: no checks matched\n", 0, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := refusal(test.output, test.exit); got != test.want {
				t.Fatalf("refusal %q, want %q", got, test.want)
			}
		})
	}
}
