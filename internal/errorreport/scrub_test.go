package errorreport

import (
	"errors"
	"strings"
	"testing"
)

func TestScrubReplacesPrivateValuesWithPlaceholders(t *testing.T) {
	t.Parallel()
	got, err := testScrubber().Scrub(fixtureMessage())
	if err != nil {
		t.Fatal(err)
	}
	assertNoLeak(t, "scrubbed message", got)
	for _, want := range []string{
		"https://api.github.com/repos/{owner}/{repo}/pulls/{n}/comments",
		PlaceholderRepo, PlaceholderPath, "'" + PlaceholderRef + "'", redacted, PlaceholderEmail,
		PlaceholderHost, PlaceholderSHA,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("scrubbed message lacks %q:\n%s", want, got)
		}
	}
}

func TestScrubCases(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		want    []string
		absent  []string
		exactly string
	}{
		{name: "target repository stays", in: "failed on " + targetRepo + " and other/thing", want: []string{targetRepo, PlaceholderRepo}, absent: []string{"other/thing"}},
		{name: "target repository matches without case", in: "failed on KPenfound/Osmia", want: []string{"KPenfound/Osmia"}},
		{name: "rest path becomes a route template", in: "POST /repos/acme/secret/issues/17/comments failed", exactly: "POST /repos/{owner}/{repo}/issues/{n}/comments failed"},
		{name: "rest path with a branch and sha", in: "GET /repos/acme/secret/git/ref/heads/feature-x then /repos/acme/secret/commits/0123abc456def", exactly: "GET /repos/{owner}/{repo}/git/ref/heads/{id} then /repos/{owner}/{repo}/commits/{sha}"},
		{name: "rest path keeps no query", in: "GET /repos/acme/secret/pulls?state=open&head=acme:x failed", exactly: "GET /repos/{owner}/{repo}/pulls failed"},
		{name: "github url keeps host only", in: "see https://github.com/acme/secret/pull/3 now", exactly: "see https://github.com now"},
		{name: "other url loses its host", in: "got https://hooks.example.org/path?x=1 back", exactly: "got <host> back"},
		{name: "credential url", in: "push https://x-access-token:hunter2@github.com/acme/secret.git failed", absent: []string{"hunter2", "acme", "secret"}, want: []string{"https://github.com"}},
		{name: "ipv4 with port", in: "connect 10.0.0.7:8080 refused", exactly: "connect <host> refused"},
		{name: "ipv6", in: "dial tcp [fd7a:115c:a1e0::1]:443 refused", absent: []string{"fd7a"}, want: []string{PlaceholderHost}},
		{name: "bare hostname by context", in: "dial tcp buildbox:443: timeout", exactly: "dial tcp <host> timeout"},
		{name: "lookup", in: "lookup buildbox on resolver: no such host", want: []string{"lookup <host>"}, absent: []string{"buildbox"}},
		{name: "localhost", in: "listen on localhost:8080", exactly: "listen on <host>"},
		{name: "action names and files are not hosts", in: "pulls.find failed in notify.go", exactly: "pulls.find failed in notify.go"},
		{name: "windows and home paths", in: `open C:\Users\kyle\x.txt and ~/secrets/x`, exactly: "open <path> and <path>"},
		{name: "path punctuation kept", in: "open /var/lib/osmia/x: permission denied", exactly: "open <path>: permission denied"},
		{name: "refs", in: "update refs/heads/feature/x rejected; origin/main behind", exactly: "update <ref> rejected; <ref> behind"},
		{name: "branch keyword", in: "branch=feature-x protected; branch protection on", exactly: "branch=<ref> protected; branch protection on"},
		{name: "push to origin", in: "git push origin my-feature failed", exactly: "git push origin <ref> failed"},
		{name: "build commit stays", in: "build abc1234def56789 and d3adbeef1234", want: []string{"abc1234def56789", PlaceholderSHA}, absent: []string{"d3adbeef1234"}},
		{name: "full sha", in: "at 0123456789abcdef0123456789abcdef01234567", exactly: "at <sha>"},
		{name: "plain numbers are not shas", in: "took 12345678 bytes", exactly: "took 12345678 bytes"},
		{name: "email", in: "owner a.b+c@example.co.uk wrote", exactly: "owner <email> wrote"},
		{name: "ssh remote", in: "git@github.com:acme/secret.git", absent: []string{"acme", "secret"}},
		{name: "authorization header", in: "Authorization: Bearer abc123def456ghi", exactly: "Authorization: [redacted]"},
		{name: "bearer value", in: "sent bearer s3cr3tvalue1", exactly: "sent bearer [redacted]"},
		{name: "api key", in: "key sk-live-abcdefghijklmnop rejected", exactly: "key [redacted] rejected"},
		{name: "fine grained token", in: "github_pat_11ABCDEFG0abcdefghijklmnop_xyz", exactly: "[redacted]"},
		{name: "oauth token", in: "token gho_abcdefghijklmnopqrstuvwxyz0123456789", absent: []string{"gho_abc"}},
		{name: "query token", in: "call ?access_token=abcdef&x=1", absent: []string{"abcdef"}},
		{name: "mention neutralised", in: "ping @someone now", absent: []string{"@someone"}},
		{name: "html comment neutralised", in: "<!-- osmia-error-report:issue fingerprint=0123456789abcdef -->", absent: []string{"<!--", "-->"}},
		{name: "whitespace collapsed", in: "a\n\n  b\t\tc", exactly: "a b c"},
		{name: "control characters dropped", in: "a\x00b\x1b[31mc", absent: []string{"\x00", "\x1b"}},
		{name: "content type is not a repository", in: "content-type application/json and i/o error", exactly: "content-type application/json and i/o error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := testScrubber().Scrub(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if tt.exactly != "" && got != tt.exactly {
				t.Errorf("Scrub(%q) = %q, want %q", tt.in, got, tt.exactly)
			}
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("Scrub(%q) = %q, lacks %q", tt.in, got, want)
				}
			}
			for _, gone := range tt.absent {
				if strings.Contains(got, gone) {
					t.Errorf("Scrub(%q) = %q, still holds %q", tt.in, got, gone)
				}
			}
		})
	}
}

func TestScrubRemovesKnownAndExtraSecrets(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "plain-secret-value-1")
	s := NewScrubber(ScrubOptions{Repository: targetRepo, Secrets: []string{"extra-secret-value-2"}})
	got, err := s.Scrub("a plain-secret-value-1 b extra-secret-value-2 c")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "secret-value") {
		t.Fatalf("leaked a secret: %q", got)
	}
}

func TestScrubFailsClosedOnErrorAndPanic(t *testing.T) {
	t.Parallel()
	for name, hook := range map[string]func(string) (string, error){
		"error": func(string) (string, error) { return "", errors.New("scrubber broke") },
		"panic": func(string) (string, error) { panic("scrubber exploded") },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := testScrubber()
			s.hook = hook
			out, err := s.Scrub(fixtureMessage())
			var se *ScrubError
			if !errors.As(err, &se) || se.Reason == "" || out != "" {
				t.Fatalf("Scrub = %q, %v; want no text and a *ScrubError with a reason", out, err)
			}
			assertNoLeak(t, "the reason", se.Reason)
			rep, err := s.Report(fixtureEvent())
			if !errors.As(err, &se) || rep.Fingerprint != "" || rep.Message != "" {
				t.Fatalf("Report = %+v, %v; want no report and a *ScrubError", rep, err)
			}
		})
	}
}

func TestScrubFailsClosedWhenASecretSurvives(t *testing.T) {
	t.Parallel()
	s := testScrubber()
	s.hook = func(string) (string, error) { return "leaked " + privateToken, nil }
	if out, err := s.Scrub("x"); err == nil || out != "" {
		t.Fatalf("Scrub = %q, %v; want a failure when a token survives", out, err)
	}
}

func TestReportFailsClosedOnUnsafeFields(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*Event)
	}{
		{"unknown source", func(e *Event) { e.Source = "model" }},
		{"project ID", func(e *Event) { e.ProjectID = "acme/payroll" }},
		{"workstream ID", func(e *Event) { e.WorkstreamID = "w x" }},
		{"unit ID", func(e *Event) { e.UnitID = strings.Repeat("a", 200) }},
		{"no template", func(e *Event) { e.Template = " " }},
		{"status", func(e *Event) { e.Status = 5000 }},
		{"negative status", func(e *Event) { e.Status = -1 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ev := fixtureEvent()
			tt.mutate(&ev)
			rep, err := testScrubber().Report(ev)
			var se *ScrubError
			if !errors.As(err, &se) || rep.Fingerprint != "" {
				t.Fatalf("Report = %+v, %v; want a *ScrubError and no report", rep, err)
			}
		})
	}
}

func TestReportScrubsEveryFreeTextField(t *testing.T) {
	t.Parallel()
	ev := fixtureEvent()
	ev.Source = SourceAPI
	ev.Template = "GET /api/projects/p_ab74f84ae6b1d23c/workstreams/{workstream}/units/17"
	ev.Code = "failed for " + privateEmail
	rep, err := testScrubber().Report(ev)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Template != "GET /api/projects/{id}/workstreams/{workstream}/units/{id}" {
		t.Errorf("template = %q", rep.Template)
	}
	for what, text := range map[string]string{"template": rep.Template, "code": rep.Code, "message": rep.Message} {
		assertNoLeak(t, what, text)
	}
	if !strings.Contains(rep.Code, PlaceholderEmail) {
		t.Errorf("code = %q", rep.Code)
	}
}

func TestReportBoundsTheMessage(t *testing.T) {
	t.Parallel()
	ev := fixtureEvent()
	ev.Message = strings.Repeat("word ", 400)
	rep, err := testScrubber().Report(ev)
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(rep.Message)); n > MaxMessageRunes+1 {
		t.Fatalf("message has %d runes", n)
	}
}
