package errorreport

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/redact"
)

// Filing holds every field an issue or a comment may show, and nothing else.
// Free text in it must come from a Scrubber.
type Filing struct {
	Report Report `json:"report"`
	// Repository is the target repository as owner/name, used for the link
	// to the previous issue.
	Repository string `json:"repository"`
	// Version and Commit are the Osmia build identity.
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
	// FirstSeen, LastSeen and Count describe the occurrences the issue
	// covers.
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Count     int       `json:"count"`
	// SkippedCaps counts events not reported because a cap was spent, and
	// SkippedDropped counts events lost from a full queue.
	SkippedCaps    int `json:"skipped_caps,omitempty"`
	SkippedDropped int `json:"skipped_dropped,omitempty"`
	// Previous is the number, in Repository, of the closed issue a new issue
	// replaces; zero when there is none.
	Previous int `json:"previous,omitempty"`
	// Day is the UTC day of a comment, as 2006-01-02.
	Day string `json:"day,omitempty"`
}

// Rendered is the text of an issue.
type Rendered struct {
	Title string
	Body  string
}

const (
	markerName = "osmia-error-report"
	dayLayout  = "2006-01-02"
)

var (
	fingerprintRe = regexp.MustCompile(`^[0-9a-f]{16}$`)
	repositoryRe  = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)
	buildRe       = regexp.MustCompile(`^[A-Za-z0-9._+-]{0,64}$`)
)

// IssueMarker is the line a rendered issue carries so the reporter can later
// recognise its own write. It holds the fingerprint only, so it does not
// depend on labels or any other setting.
func IssueMarker(fingerprint string) string {
	return fmt.Sprintf("<!-- %s:issue fingerprint=%s -->", markerName, fingerprint)
}

// CommentMarker is the line a rendered comment carries: the fingerprint and
// the UTC day, as 2006-01-02.
func CommentMarker(fingerprint, day string) string {
	return fmt.Sprintf("<!-- %s:comment fingerprint=%s day=%s -->", markerName, fingerprint, day)
}

// MatchesIssue reports whether body is an issue the reporter rendered for
// fingerprint. A comment for the same fingerprint does not match.
func MatchesIssue(body, fingerprint string) bool {
	return fingerprintRe.MatchString(fingerprint) && hasLine(body, IssueMarker(fingerprint))
}

// MatchesComment reports whether body is a comment the reporter rendered for
// fingerprint on day. An issue does not match.
func MatchesComment(body, fingerprint, day string) bool {
	return fingerprintRe.MatchString(fingerprint) && hasLine(body, CommentMarker(fingerprint, day))
}

func hasLine(body, line string) bool {
	for _, l := range strings.Split(body, "\n") {
		if strings.TrimSpace(l) == line {
			return true
		}
	}
	return false
}

// RenderIssue renders the title and body of a new issue. It fails closed: a
// field that is not safe to publish, or a panic, returns an error and no text.
func RenderIssue(f Filing) (r Rendered, err error) {
	defer recoverRender(&err)
	if err := f.validate(false); err != nil {
		return Rendered{}, err
	}
	var b strings.Builder
	b.WriteString(IssueMarker(f.Report.Fingerprint) + "\n")
	b.WriteString("## Osmia error report\n\n")
	b.WriteString(f.fields())
	b.WriteString("\n### Message\n\n")
	b.WriteString(fenced(f.Report.Message))
	b.WriteString("\nFiled by the Osmia error reporter. Hand this issue in to have it fixed.\n")
	r = Rendered{Title: f.title(), Body: b.String()}
	if err := checkRendered(r.Title, r.Body); err != nil {
		return Rendered{}, err
	}
	return r, nil
}

// RenderComment renders the body of a daily "still occurring" comment.
func RenderComment(f Filing) (body string, err error) {
	defer recoverRender(&err)
	if err := f.validate(true); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(CommentMarker(f.Report.Fingerprint, f.Day) + "\n")
	fmt.Fprintf(&b, "**Still occurring** on %s\n\n", f.Day)
	fmt.Fprintf(&b, "- **Occurrences:** %d\n", f.Count)
	fmt.Fprintf(&b, "- **Last seen:** %s\n", stamp(f.LastSeen))
	b.WriteString(f.skipped())
	fmt.Fprintf(&b, "- **Fingerprint:** `%s`\n", f.Report.Fingerprint)
	body = b.String()
	if err := checkRendered(body); err != nil {
		return "", err
	}
	return body, nil
}

func recoverRender(err *error) {
	if r := recover(); r != nil {
		*err = &ScrubError{Reason: fmt.Sprintf("rendering the report panicked: %v", r)}
	}
}

func checkRendered(texts ...string) error {
	for _, text := range texts {
		if reason := leakReason(text, redact.KnownSecrets()...); reason != "" {
			return &ScrubError{Reason: "rendered text is not safe to publish: " + reason}
		}
	}
	return nil
}

func (f Filing) validate(comment bool) error {
	bad := func(what string) error { return &ScrubError{Reason: "cannot render the report: " + what} }
	switch {
	case !f.Report.Source.Valid():
		return bad("unknown source")
	case !fingerprintRe.MatchString(f.Report.Fingerprint):
		return bad("malformed fingerprint")
	case !buildRe.MatchString(f.Version) || !buildRe.MatchString(f.Commit):
		return bad("malformed build identity")
	case f.Report.Template == "":
		return bad("no template")
	}
	for _, id := range []string{f.Report.ProjectID, f.Report.WorkstreamID, f.Report.UnitID} {
		if id != "" && !idRe.MatchString(id) {
			return bad("malformed ID")
		}
	}
	if comment || f.Day != "" {
		if _, err := time.Parse(dayLayout, f.Day); err != nil {
			return bad("malformed day")
		}
	}
	if f.Previous > 0 && !repositoryRe.MatchString(f.Repository) {
		return bad("malformed repository for the previous issue link")
	}
	return nil
}

func (f Filing) title() string {
	head := string(f.Report.Source) + " " + inline(f.Report.Template)
	switch {
	case f.Report.Status != 0:
		head += fmt.Sprintf(" %d", f.Report.Status)
	case f.Report.Code != "":
		head += " " + inline(f.Report.Code)
	}
	title := "Osmia error: " + head
	if f.Report.Message != "" {
		title += ": " + truncate(oneLine(f.Report.Message), 80)
	}
	return truncate(oneLine(title), 200)
}

func (f Filing) fields() string {
	var b strings.Builder
	line := func(label, value string) {
		if value != "" {
			fmt.Fprintf(&b, "- **%s:** %s\n", label, value)
		}
	}
	code := func(v string) string {
		if v == "" {
			return ""
		}
		return "`" + inline(v) + "`"
	}
	line("Source", code(string(f.Report.Source)))
	line("Action or endpoint", code(f.Report.Template))
	if f.Report.Status != 0 {
		line("Status", fmt.Sprintf("`%d`", f.Report.Status))
	}
	line("Error code", code(f.Report.Code))
	line("Osmia version", code(f.Version))
	line("Osmia commit", code(f.Commit))
	line("First seen", stamp(f.FirstSeen))
	line("Last seen", stamp(f.LastSeen))
	line("Occurrences", fmt.Sprint(f.Count))
	b.WriteString(f.skipped())
	line("Fingerprint", code(f.Report.Fingerprint))
	line("Project", code(f.Report.ProjectID))
	line("Workstream", code(f.Report.WorkstreamID))
	line("Unit", code(f.Report.UnitID))
	if f.Previous > 0 {
		line("Previous issue (closed)", fmt.Sprintf("https://github.com/%s/issues/%d", f.Repository, f.Previous))
	}
	return b.String()
}

func (f Filing) skipped() string {
	var b strings.Builder
	if f.SkippedCaps > 0 {
		fmt.Fprintf(&b, "- **Skipped over a cap:** %d\n", f.SkippedCaps)
	}
	if f.SkippedDropped > 0 {
		fmt.Fprintf(&b, "- **Skipped from a full queue:** %d\n", f.SkippedDropped)
	}
	return b.String()
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func inline(s string) string {
	return strings.NewReplacer("`", "'", "\n", " ", "\r", " ", "|", "/").Replace(s)
}

func fenced(s string) string {
	longest, run := 0, 0
	for _, c := range s {
		if c == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	return fence + "text\n" + s + "\n" + fence + "\n"
}
