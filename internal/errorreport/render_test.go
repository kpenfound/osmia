package errorreport

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

func fixtureFiling(t *testing.T) Filing {
	t.Helper()
	rep, err := testScrubber().Report(fixtureEvent())
	if err != nil {
		t.Fatal(err)
	}
	return Filing{
		Report:         rep,
		Repository:     targetRepo,
		Version:        "v1.2.3",
		Commit:         "abc1234",
		FirstSeen:      time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC),
		LastSeen:       time.Date(2026, 11, 20, 11, 30, 0, 0, time.UTC),
		Count:          7,
		SkippedCaps:    2,
		SkippedDropped: 3,
		Previous:       41,
		Day:            "2026-11-20",
	}
}

func TestRenderedTextHoldsNoPrivateValues(t *testing.T) {
	t.Parallel()
	f := fixtureFiling(t)
	issue, err := RenderIssue(f)
	if err != nil {
		t.Fatal(err)
	}
	comment, err := RenderComment(f)
	if err != nil {
		t.Fatal(err)
	}
	assertNoLeak(t, "title", issue.Title)
	assertNoLeak(t, "body", issue.Body)
	assertNoLeak(t, "comment", comment)
	for _, want := range []string{
		"https://api.github.com/repos/{owner}/{repo}/pulls/{n}/comments",
		PlaceholderRepo, PlaceholderPath, PlaceholderRef, PlaceholderEmail, PlaceholderHost, PlaceholderSHA,
	} {
		if !strings.Contains(issue.Body, want) {
			t.Errorf("body lacks %q:\n%s", want, issue.Body)
		}
	}
	if strings.ContainsAny(issue.Title, "\n\r") || !strings.HasPrefix(issue.Title, "Osmia error: github pulls.find 502") {
		t.Errorf("title = %q", issue.Title)
	}
}

var fieldLine = regexp.MustCompile(`^- \*\*([^:]+):\*\*`)

func TestRenderedFieldsAreTheAllowlist(t *testing.T) {
	t.Parallel()
	allowed := map[string]bool{
		"Source": true, "Action or endpoint": true, "Status": true, "Error code": true,
		"Osmia version": true, "Osmia commit": true, "First seen": true, "Last seen": true,
		"Occurrences": true, "Skipped over a cap": true, "Skipped from a full queue": true,
		"Fingerprint": true, "Project": true, "Workstream": true, "Unit": true,
		"Previous issue (closed)": true,
	}
	f := fixtureFiling(t)
	issue, err := RenderIssue(f)
	if err != nil {
		t.Fatal(err)
	}
	comment, err := RenderComment(f)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, text := range []string{issue.Body, comment} {
		for _, line := range strings.Split(text, "\n") {
			if m := fieldLine.FindStringSubmatch(line); m != nil {
				if !allowed[m[1]] {
					t.Errorf("field %q is not on the allowlist", m[1])
				}
				seen[m[1]] = true
			}
		}
	}
	for name := range allowed {
		if !seen[name] {
			t.Errorf("field %q was not rendered", name)
		}
	}
	for what, text := range map[string]string{"title": issue.Title, "body": issue.Body, "comment": comment} {
		for _, extra := range []string{fixturePrompt, fixtureCheck, fixtureIssueTitle} {
			if strings.Contains(text, extra) {
				t.Errorf("%s leaks the free-form attribute %q", what, extra)
			}
		}
	}
	for _, want := range []string{
		"https://github.com/" + targetRepo + "/issues/41", "`p_ab74f84ae6b1d23cdeed8d6806da664f`",
		"`w_eac398a71ab8c125588dd29e5e0c7473`", "`error-report-model`", "2026-11-20T10:00:00Z",
		"**Occurrences:** 7", "**Skipped over a cap:** 2", "**Skipped from a full queue:** 3",
	} {
		if !strings.Contains(issue.Body, want) {
			t.Errorf("body lacks %q", want)
		}
	}
	for _, want := range []string{"**Occurrences:** 7", "2026-11-20T11:30:00Z", "`" + f.Report.Fingerprint + "`", "2026-11-20"} {
		if !strings.Contains(comment, want) {
			t.Errorf("comment lacks %q:\n%s", want, comment)
		}
	}
}

func TestRenderOmitsEmptyOptionalFields(t *testing.T) {
	t.Parallel()
	f := fixtureFiling(t)
	f.Previous, f.SkippedCaps, f.SkippedDropped = 0, 0, 0
	f.Report.Status, f.Report.Code, f.Report.ProjectID, f.Report.WorkstreamID, f.Report.UnitID = 0, "", "", "", ""
	f.Commit = ""
	issue, err := RenderIssue(f)
	if err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"Previous issue", "Skipped", "Status", "Error code", "Project", "Workstream", "Unit", "Osmia commit"} {
		if absent := "**" + label; strings.Contains(issue.Body, absent) {
			t.Errorf("body holds %q:\n%s", absent, issue.Body)
		}
	}
}

func TestMarkersAreRecognisedByFingerprintAndDay(t *testing.T) {
	t.Parallel()
	f := fixtureFiling(t)
	issue, err := RenderIssue(f)
	if err != nil {
		t.Fatal(err)
	}
	comment, err := RenderComment(f)
	if err != nil {
		t.Fatal(err)
	}
	fp := f.Report.Fingerprint
	other := "0123456789abcdef"
	if !MatchesIssue(issue.Body, fp) || MatchesIssue(issue.Body, other) {
		t.Error("issue matcher must match its fingerprint and no other")
	}
	if !MatchesComment(comment, fp, "2026-11-20") || MatchesComment(comment, fp, "2026-11-21") || MatchesComment(comment, other, "2026-11-20") {
		t.Error("comment matcher must match its fingerprint and day and no other")
	}
	if MatchesComment(issue.Body, fp, "2026-11-20") || MatchesIssue(comment, fp) {
		t.Error("an issue is not a comment and a comment is not an issue")
	}
	if MatchesIssue("", fp) || MatchesIssue(issue.Body, "") || MatchesIssue(issue.Body, "not-a-fingerprint") {
		t.Error("malformed input must not match")
	}
	if strings.Count(IssueMarker(fp), fp) != 1 || strings.Contains(IssueMarker(fp), "label") {
		t.Errorf("issue marker = %q", IssueMarker(fp))
	}
}

func TestMessageCannotForgeAMarker(t *testing.T) {
	t.Parallel()
	forged := "x\n" + IssueMarker("0123456789abcdef") + "\n" + CommentMarker("0123456789abcdef", "2026-11-20")
	ev := fixtureEvent()
	ev.Message = forged
	rep, err := testScrubber().Report(ev)
	if err != nil {
		t.Fatal(err)
	}
	f := fixtureFiling(t)
	f.Report = rep
	issue, err := RenderIssue(f)
	if err != nil {
		t.Fatal(err)
	}
	if MatchesIssue(issue.Body, "0123456789abcdef") {
		t.Fatalf("a message forged a marker:\n%s", issue.Body)
	}
	if !MatchesIssue(issue.Body, rep.Fingerprint) {
		t.Fatal("the real marker is missing")
	}
}

func TestMessageWithBackticksStaysInItsFence(t *testing.T) {
	t.Parallel()
	f := fixtureFiling(t)
	f.Report.Message = "a ```` b ``` c"
	issue, err := RenderIssue(f)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(issue.Body, "`````text\na ```` b ``` c\n`````\n") {
		t.Fatalf("body = %s", issue.Body)
	}
}

func TestRenderFailsClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(*Filing)
		comment bool
	}{
		{"unknown source", func(f *Filing) { f.Report.Source = "x" }, true},
		{"fingerprint", func(f *Filing) { f.Report.Fingerprint = "../../x" }, true},
		{"version", func(f *Filing) { f.Version = "v1 <script>" }, true},
		{"id", func(f *Filing) { f.Report.UnitID = "a b" }, true},
		{"day", func(f *Filing) { f.Day = "today" }, true},
		{"repository link", func(f *Filing) { f.Repository = "not a repo" }, true},
		{"token in the message", func(f *Filing) { f.Report.Message = "leaked " + privateToken }, false},
		{"token in the template", func(f *Filing) { f.Report.Template = "sk-live-abcdefghijklmnop" }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := fixtureFiling(t)
			tt.mutate(&f)
			if r, err := RenderIssue(f); err == nil || r.Body != "" || r.Title != "" {
				t.Errorf("RenderIssue = %+v, %v; want a failure", r, err)
			}
			if c, err := RenderComment(f); tt.comment && (err == nil || c != "") {
				t.Errorf("RenderComment = %q, %v; want a failure", c, err)
			}
		})
	}
}
