package checks

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

const ReportLimit = 16 * 1024

var reportColor = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
var reportHeading = regexp.MustCompile(`^(?:CHECKS|== CHECKS ==)(?:[ \t]|$)`)
var reportCheck = regexp.MustCompile(`^([ \t]*)([✘✔⊘◌])[ \t]+(dag(?:\+check)?://\S+)(.*)$`)
var reportError = regexp.MustCompile(`(?m)^Error: (.+)$`)

// reportNothingSelected matches Dagger's refusal of a run whose filters leave no
// check: "no checks matched pattern" when it names addresses, and "no checks
// selected" when it does not.
var reportNothingSelected = regexp.MustCompile(`^no checks (?:matched|selected)`)

// reportNotRefused matches what a run shows when checks had started or its
// engine session was lost, so a missing report says nothing about its links.
var reportNotRefused = regexp.MustCompile(`checks: [0-9]+/[0-9]+ done|http://dagger/|engine closed session`)

// errorLimit bounds the Dagger error a reason or selection detail repeats.
const errorLimit = 1024

type checkFailure struct {
	link       string
	heading    string
	start, end int
	lines      []string
	// passedLater records that a later report in the output shows the check
	// passing.
	passedLater bool
}

// parseCheckReport reads check results only inside Dagger's CHECKS sections.
// Output with several reports yields every report's heading, one per line.
// Check links can exceed 64 KiB; splitting on newlines has no scanner limit.
func parseCheckReport(output string) (heading string, failures []checkFailure) {
	heading, failures, _ = parseCheckReportResults(output)
	return heading, failures
}

// parseCheckReportResults is parseCheckReport that also returns the links the
// report shows passing, in the order they first appear.
func parseCheckReportResults(output string) (heading string, failures []checkFailure, passed []string) {
	lines := strings.Split(reportColor.ReplaceAllString(output, ""), "\n")
	active := -1
	inChecks := false
	checkIndent, childIndent := 0, 0
	for i, line := range lines {
		if reportHeading.MatchString(line) {
			if heading != "" {
				heading += "\n"
			}
			heading, inChecks, active = heading+line, true, -1
			checkIndent, childIndent = 0, 0
			continue
		}
		if !inChecks {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if reportHeading.MatchString(strings.TrimSpace(line)) && indent <= checkIndent+2 {
			checkIndent = indent
			// Dagger lists the checks of a nested report one level inside its
			// heading; a top-level report lists them flush with it.
			childIndent = 0
			if indent > 0 {
				childIndent = indent + 2
			}
			active = -1
			continue
		}
		// The rerun section can have a bare heading or a report heading.
		if strings.HasPrefix(line, "== ") || line == "RUN LOCALLY" || line == "RE-RUN IN CI" {
			inChecks, active = false, -1
			continue
		}
		if m := reportCheck.FindStringSubmatch(line); m != nil && len(m[1]) <= max(checkIndent, childIndent) {
			checkIndent = len(m[1])
			active = -1
			switch m[2] {
			case "✘":
				failures = append(failures, checkFailure{link: m[3], heading: "✘ " + CheckLinkLabel(m[3]) + m[4], start: i + 1, end: i + 1})
				active = len(failures) - 1
			case "✔":
				if !slices.Contains(passed, m[3]) {
					passed = append(passed, m[3])
				}
				for f := range failures {
					if failures[f].link == m[3] {
						failures[f].passedLater = true
					}
				}
			}
			continue
		}
		if active >= 0 {
			failures[active].lines = append(failures[active].lines, line)
			failures[active].end = i + 1
		}
	}
	return heading, failures, passed
}

// latestFailures keeps one failure for each link: its most recent, in the order
// the links first failed.
func latestFailures(failures []checkFailure) []checkFailure {
	var latest []checkFailure
	for _, failure := range failures {
		if i := slices.IndexFunc(latest, func(f checkFailure) bool { return f.link == failure.link }); i >= 0 {
			latest[i] = failure
		} else {
			latest = append(latest, failure)
		}
	}
	return latest
}

// FailedChecks returns the links whose latest result in output is a failure,
// independently of the prompt budget. A check that failed and then passed in a
// later report is not among them.
func FailedChecks(output string) []string {
	_, failures := parseCheckReport(output)
	var links []string
	for _, failure := range latestFailures(failures) {
		if !failure.passedLater {
			links = append(links, failure.link)
		}
	}
	return links
}

// PassedChecks returns the links output reports as passed and not failed
// again, in the order they first appear. A run that did not finish can still
// report the checks that passed before it ended.
func PassedChecks(output string) []string {
	_, _, passed := parseCheckReportResults(output)
	failed := FailedChecks(output)
	return slices.DeleteFunc(passed, func(link string) bool { return slices.Contains(failed, link) })
}

// DescribeFlaky names the checks that failed and then passed on their rerun,
// for a record's reason or a prompt. It is empty when none did.
func DescribeFlaky(links []string) string {
	if len(links) == 0 {
		return ""
	}
	return "Flaky, failed and then passed on rerun: " + CheckExcerpt(strings.Join(links, ", "), 2048)
}

// SummarizeCheckOutput omits passing check blocks and rerun commands. Each
// failed check gets an equal diagnostic budget, so a noisy first failure
// cannot consume the entire report, and a check that failed more than once
// keeps only its latest diagnostics. A check that passed on its rerun keeps its
// failure's diagnostics, marked as such, because they are the evidence of the
// flake. Unrecognized output keeps both ends.
func SummarizeCheckOutput(output string, exit int) (string, bool) {
	heading, failures := parseCheckReport(output)
	failures = latestFailures(failures)
	if len(failures) == 0 {
		if exit == 0 && heading != "" {
			return CheckExcerpt(heading+"\n", ReportLimit), len(heading)+1 > ReportLimit
		}
		clean := reportColor.ReplaceAllString(output, "")
		return CheckExcerpt(clean, ReportLimit), len(clean) > ReportLimit
	}
	var out strings.Builder
	out.WriteString(CheckExcerpt(heading, 1024) + "\n")
	truncated := false
	if len(failures) > 32 {
		fmt.Fprintf(&out, "Showing 32 of %d failed check blocks; read the captured output for the rest.\n", len(failures))
		failures, truncated = failures[:32], true
	}
	budget := (ReportLimit - out.Len()) / len(failures)
	for _, failure := range failures {
		label := failure.heading
		if failure.passedLater {
			label += " [passed on rerun]"
		}
		block := fmt.Sprintf("%s [output lines %d-%d]\n%s\n", label, failure.start, failure.end, strings.TrimSpace(strings.Join(failure.lines, "\n")))
		out.WriteString(CheckExcerpt(block, budget))
		truncated = truncated || len(block) > budget
	}
	return out.String(), truncated
}

// CheckExcerpt preserves both ends and marks omitted bytes without splitting
// UTF-8 characters. Even a single long line cannot exhaust a report's budget.
func CheckExcerpt(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	const omitted = "\n[... omitted; read the captured output ...]\n"
	if limit < len(omitted) {
		return ""
	}
	available := limit - len(omitted)
	head, tail := available*2/3, len(text)-available/3
	for head > 0 && !utf8.RuneStart(text[head]) {
		head--
	}
	for tail < len(text) && !utf8.RuneStart(text[tail]) {
		tail++
	}
	return text[:head] + omitted + text[tail:]
}

func CheckOutputPath(recordPath string) string {
	return strings.TrimSuffix(recordPath, ".json") + "-output.txt"
}

func CheckLinkLabel(link string) string {
	if len(link) <= 512 {
		return link
	}
	end := 512
	for !utf8.RuneStart(link[end]) {
		end--
	}
	return link[:end] + "… [full link in check record]"
}

// OutputEvidence points readers to the complete captured output, including
// the diagnostics omitted from a bounded excerpt.
func OutputEvidence(output, path string, revision int, truncated bool) string {
	out := ""
	if path != "" {
		out = fmt.Sprintf("\nCaptured check output: %s (revision %d). Read it with factory_context using that revision, start/lines and offset/next_offset for large lines.", path, revision)
	}
	if truncated || len(output) > ReportLimit {
		out += "\nThe diagnostic excerpt is truncated."
		if path != "" {
			out += " Consult the captured output for omitted details."
		} else if truncated {
			out += " The stored check output is also incomplete."
		}
	}
	if output != "" {
		out += "\nCheck diagnostics:\n" + CheckExcerpt(output, ReportLimit)
	}
	return out
}

// daggerError returns the message of the last error Dagger printed, or empty
// when it printed none.
func daggerError(output string) string {
	matches := reportError.FindAllStringSubmatch(reportColor.ReplaceAllString(output, ""), -1)
	if len(matches) == 0 {
		return ""
	}
	return CheckExcerpt(matches[len(matches)-1][1], errorLimit)
}

// refusal returns Dagger's error when the output of one dagger check run shows
// it refused to run: it exited with an error, without a report, without
// starting a check and with its engine session intact.
func refusal(output string, exit int) string {
	if exit <= 0 {
		return ""
	}
	if heading, _ := parseCheckReport(output); heading != "" {
		return ""
	}
	if reportNotRefused.MatchString(reportColor.ReplaceAllString(output, "")) {
		return ""
	}
	return daggerError(output)
}

// ClassifyChecks classifies a finished dagger check from its result and the
// error it ran with: passed on exit 0, failed with the links Dagger's report
// shows failed, and otherwise incomplete with the reason, which repeats the
// last error Dagger printed.
func ClassifyChecks(result CheckResult, err error) (status string, failed []string, reason error) {
	failed = FailedChecks(result.Output)
	switch {
	case err != nil:
		return ChecksIncomplete, failed, err
	case result.ExitCode == 0:
		return ChecksPassed, nil, nil
	case len(failed) > 0:
		return ChecksFailed, failed, nil
	default:
		if message := daggerError(result.Output); message != "" {
			return ChecksIncomplete, failed, fmt.Errorf("dagger check exited %d without reporting a failed check: %s", result.ExitCode, message)
		}
		return ChecksIncomplete, failed, fmt.Errorf("dagger check exited %d without reporting a failed check", result.ExitCode)
	}
}
