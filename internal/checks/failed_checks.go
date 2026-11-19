package checks

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/kpenfound/osmia/internal/api"
)

// TraceCommand is the command that reads the full trace of a check: the
// `dagger trace` id its commit status description names, verbatim. It is empty
// for an id that is not a plain trace id, so no other text becomes a command.
func TraceCommand(traceID string) string {
	if !traceIDPattern.MatchString(traceID) {
		return ""
	}
	return "dagger trace " + traceID
}

var traceIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// secretQuery matches the names of query parameters that carry a credential.
var secretQuery = regexp.MustCompile(`(?i)token|secret|password|passwd|credential|auth|signature|sig|key|session`)

// SafeLink returns link when it can be given to an agent as it stands: an
// http or https URL with no userinfo, fragment or credential query parameter.
// Anything else yields "".
func SafeLink(link string) string {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return ""
	}
	u.Fragment = ""
	query := u.Query()
	cleaned := false
	for name := range query {
		if secretQuery.MatchString(name) {
			query.Del(name)
			cleaned = true
		}
	}
	if cleaned {
		u.RawQuery = query.Encode()
	}
	return u.String()
}

var linkInText = regexp.MustCompile(`(?i)https?://[^\s<>"'`+"`"+`)\]]+`)

// SafeText returns text with every http or https link in it as SafeLink gives
// it: a link that carries a credential keeps no part of it, and a link that
// cannot be given as it stands is replaced by "[link withheld]".
func SafeText(text string) string {
	return linkInText.ReplaceAllStringFunc(text, func(link string) string {
		trimmed := strings.TrimRight(link, ".,;:!?")
		tail := link[len(trimmed):]
		if safe := SafeLink(trimmed); safe != "" {
			return safe + tail
		}
		return "[link withheld]" + tail
	})
}

// CloudFailures lists the failed checks of the result as the records that keep
// them: name, trace id, the failing status and its link.
func CloudFailures(result CloudResult) []api.CheckFailure {
	var out []api.CheckFailure
	for _, f := range result.Failed {
		out = append(out, api.CheckFailure{Name: f.Name, TraceID: f.TraceID, StatusID: f.Status.ID, TargetURL: f.Status.TargetURL})
	}
	return out
}

// DescribeFailedCheck says what one failed check offers to read: its
// `dagger trace` command, or that no trace id was posted for it, with its
// status link when it has one.
func DescribeFailedCheck(f api.CheckFailure) string {
	if command := TraceCommand(f.TraceID); command != "" {
		return fmt.Sprintf("%s: `%s`", SafeText(f.Name), command)
	}
	out := SafeText(f.Name) + ": Dagger Cloud posted no trace id for this check"
	if link := SafeLink(f.TargetURL); link != "" {
		out += "; status: " + link
	}
	return out
}

// DescribeFailedChecks lists the failed checks of a failed run, one per line,
// each starting with "- ". A failed run that names none says that no failing
// check status was found, with the counts of its JSON and the run's status
// URL, and never lists nothing. It is empty for a run that did not fail.
// counts and statusURL may be absent.
func DescribeFailedChecks(failed []api.CheckFailure, counts *api.CheckCounts, statusURL string, runFailed bool) string {
	if len(failed) > 0 {
		lines := make([]string, 0, len(failed))
		for _, f := range failed {
			lines = append(lines, "- "+CheckExcerpt(DescribeFailedCheck(f), 2048))
		}
		return fmt.Sprintf("Failed checks (%d):\n%s", len(failed), strings.Join(lines, "\n"))
	}
	if !runFailed {
		return ""
	}
	out := "The run failed, but no failing check status was found, so no failed check can be named"
	if counts != nil {
		out += fmt.Sprintf(": the run's JSON counts %d failed of %d checks (%d passed, %d cancelled)", counts.Failed, counts.Total, counts.Passed, counts.Cancelled)
	}
	if link := SafeLink(statusURL); link != "" {
		out += "; run status: " + link
	}
	return out + "."
}
