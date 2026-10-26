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
