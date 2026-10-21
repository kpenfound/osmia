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
var reportCheck = regexp.MustCompile(`^([ \t]*)([✘✔⊘◌]) (dag(?:\+check)?://\S+)(.*)$`)

type checkFailure struct {
	link       string
	heading    string
	start, end int
	lines      []string
}

// parseCheckReport reads check results only inside Dagger's CHECKS section.
// Check links can exceed 64 KiB; splitting on newlines has no scanner limit.
func parseCheckReport(output string) (heading string, failures []checkFailure) {
	lines := strings.Split(reportColor.ReplaceAllString(output, ""), "\n")
	active := -1
	inChecks := false
	checkIndent := 0
	for i, line := range lines {
		if strings.HasPrefix(line, "== CHECKS ==") {
			heading, inChecks, active = line, true, -1
			checkIndent = 0
			continue
		}
		if !inChecks {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if strings.HasPrefix(strings.TrimSpace(line), "== CHECKS ==") && indent <= checkIndent+2 {
			checkIndent = indent
			active = -1
			continue
		}
		// The rerun section can have a bare heading or a report heading.
		if strings.HasPrefix(line, "== ") || line == "RUN LOCALLY" || line == "RE-RUN IN CI" {
			inChecks, active = false, -1
			continue
		}
		if m := reportCheck.FindStringSubmatch(line); m != nil && len(m[1]) <= checkIndent {
			checkIndent = len(m[1])
			active = -1
			if m[2] == "✘" {
				failures = append(failures, checkFailure{link: m[3], heading: "✘ " + CheckLinkLabel(m[3]) + m[4], start: i + 1, end: i + 1})
				active = len(failures) - 1
			}
			continue
		}
		if active >= 0 {
			failures[active].lines = append(failures[active].lines, line)
			failures[active].end = i + 1
		}
	}
	return heading, failures
}

// failedChecks retains full check links independently of the prompt budget.
func FailedChecks(output string) []string {
	_, failures := parseCheckReport(output)
	var links []string
	for _, failure := range failures {
		if !slices.Contains(links, failure.link) {
			links = append(links, failure.link)
		}
	}
	return links
}

// SummarizeCheckOutput omits passing check blocks and rerun commands. Each
// failed check gets an equal diagnostic budget, so a noisy first failure
// cannot consume the entire report. Unrecognized output keeps both ends.
func SummarizeCheckOutput(output string, exit int) (string, bool) {
	heading, failures := parseCheckReport(output)
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
		block := fmt.Sprintf("%s [output lines %d-%d]\n%s\n", failure.heading, failure.start, failure.end, strings.TrimSpace(strings.Join(failure.lines, "\n")))
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

// ClassifyChecks classifies a finished dagger check from its result and the
// error it ran with: passed on exit 0, failed with the links Dagger's report
// shows failed, and otherwise incomplete with the reason.
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
		return ChecksIncomplete, failed, fmt.Errorf("dagger check exited %d without reporting a failed check", result.ExitCode)
	}
}
