package service

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCheckReportManyFailuresRetainsDiagnosticsAndFullClassification(t *testing.T) {
	t.Parallel()
	var raw strings.Builder
	raw.WriteString("== CHECKS ==  ✘ 1000 failed\n")
	for i := range 1000 {
		fmt.Fprintf(&raw, "✘ dag://check-%d 1s ERROR\n    assertion %d failed\n", i, i)
	}
	report, truncated := summarizeCheckOutput(raw.String(), 1)
	if !truncated || len(report) > checkReportLimit || !strings.Contains(report, "Showing 32 of 1000") || !strings.Contains(report, "assertion 0 failed") || !strings.Contains(report, "assertion 31 failed") || len(failedChecks(raw.String())) != 1000 {
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
	status, failed, err := classifyChecks(CheckResult{ExitCode: 1, Output: raw}, nil)
	if err != nil || status != ChecksFailed || !slices.Equal(failed, []string{link}) {
		t.Fatalf("classification: %s, %d failures, %v", status, len(failed), err)
	}
	report, truncated := summarizeCheckOutput(raw, 1)
	if truncated || len(report) > checkReportLimit || !strings.Contains(report, "built_test.go:9: wrong chunk") || !strings.Contains(report, "keep this diagnostic too") || !strings.Contains(report, "[output lines 5-") {
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
	raw := "== CHECKS ==  ✘ 2 failed\n✘ dag://first 1s ERROR\n    first assertion\n" +
		strings.Repeat("é", 50000) + "\n    first stack tail\n✘ dag://second 1s ERROR\n    second assertion\n"
	report, truncated := summarizeCheckOutput(raw, 1)
	if !truncated || len(report) > checkReportLimit || !utf8.ValidString(report) {
		t.Fatalf("report: truncated=%t bytes=%d valid=%t", truncated, len(report), utf8.ValidString(report))
	}
	for _, want := range []string{"first assertion", "first stack tail", "second assertion", "omitted"} {
		if !strings.Contains(report, want) {
			t.Fatalf("missing %q", want)
		}
	}
	evidence := outputEvidence(report, "units/u/checks-1-output.txt", 1, truncated)
	if !strings.Contains(evidence, "factory_context") || !strings.Contains(evidence, "excerpt is truncated") {
		t.Fatalf("missing retrieval instructions: %s", evidence)
	}
}

func TestCheckReportReadsNestedFailuresAndColors(t *testing.T) {
	t.Parallel()
	raw := "== CHECKS ==  ✘ 1 failed\n\x1b[31m✘ dag://parent 1s ERROR\x1b[0m\n  == CHECKS ==\n  ✘ dag+check://child 1s ERROR\n    compiler: missing symbol\n  ✔ dag://sibling 1s OK\n    passing detail\n✔ dag://other 1s OK\n== RUN LOCALLY ==\nrerun\n"
	if got := failedChecks(raw); !slices.Equal(got, []string{"dag://parent", "dag+check://child"}) {
		t.Fatalf("failures: %v", got)
	}
	report, truncated := summarizeCheckOutput(raw, 1)
	if truncated || !strings.Contains(report, "compiler: missing symbol") || strings.Contains(report, "passing detail") || strings.Contains(report, "rerun") || strings.Contains(report, "\x1b") {
		t.Fatalf("report: %s", report)
	}
}

func TestCheckReportKeepsUnrecognizedAndInfrastructureOutput(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"Error: engine unavailable\n",
		"compiler: undefined symbol\n" + strings.Repeat("verbose detail\n", 10000) + "exit code 1\n",
		"== CHECKS ==  ◌ 1 pending\nengine disconnected\n",
	} {
		report, truncated := summarizeCheckOutput(raw, 1)
		if len(report) > checkReportLimit || truncated != (len(raw) > checkReportLimit) || !strings.HasPrefix(report, strings.Split(raw, "\n")[0]) || !strings.HasSuffix(report, strings.Split(raw, "\n")[len(strings.Split(raw, "\n"))-2]+"\n") {
			t.Fatalf("fallback report: %s", report)
		}
		status, _, err := classifyChecks(CheckResult{ExitCode: 1, Output: raw}, nil)
		if status != ChecksIncomplete || err == nil {
			t.Fatalf("unreported failure classified as %s: %v", status, err)
		}
	}
}
