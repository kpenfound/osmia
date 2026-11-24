package errorreport

import (
	"strings"
	"testing"
	"time"
)

const (
	privateRepo       = "acme/payroll-secrets"
	privateRepoName   = "payroll-secrets"
	privatePath       = "/home/kyle/.local/share/osmia/projects/p_1/work/main.go"
	privateBranch     = "feature/payroll-q3"
	privateToken      = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	privateEmail      = "dev@acme-corp.example"
	privateHost       = "osmia-box.tail1a2b3.ts.net"
	targetRepo        = "kpenfound/osmia"
	buildCommit       = "abc1234def5678900000000000000000000000ff"
	fixturePrompt     = "PROMPT-SECRET-TEXT"
	fixtureCheck      = "CHECK-OUTPUT-SECRET"
	fixtureIssueTitle = "ISSUE-TITLE-SECRET"
)

var leaks = []string{
	privateRepo, privateRepoName, "/home/kyle", "main.go", privateBranch, "payroll-q3",
	privateToken, "abcdefghijklmnop", privateEmail, "acme-corp", "dev@", privateHost, "tail1a2b3",
	fixturePrompt, fixtureCheck, fixtureIssueTitle,
}

func fixtureMessage() string {
	return "GET https://api.github.com/repos/" + privateRepo + "/pulls/42/comments: 502 for " + privateRepo +
		"; opened " + privatePath + "; push to branch '" + privateBranch + "' failed with token " + privateToken +
		"; contact " + privateEmail + "; dial tcp " + privateHost + ":443: i/o timeout; " +
		"commit 0123456789abcdef0123456789abcdef01234567; see https://internal.example.com/x"
}

func fixtureEvent() Event {
	return Event{
		Source:       SourceGitHub,
		Template:     "pulls.find",
		Status:       502,
		Code:         "bad_gateway",
		Message:      fixtureMessage(),
		ProjectID:    "p_ab74f84ae6b1d23cdeed8d6806da664f",
		WorkstreamID: "w_eac398a71ab8c125588dd29e5e0c7473",
		UnitID:       "error-report-model",
		Time:         time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC),
		Attributes: map[string]string{
			"prompt": fixturePrompt, "check_output": fixtureCheck, "issue_title": fixtureIssueTitle,
		},
	}
}

func testScrubber() *Scrubber {
	return NewScrubber(ScrubOptions{Repository: targetRepo, BuildCommit: buildCommit})
}

func assertNoLeak(t *testing.T, what string, text string) {
	t.Helper()
	for _, leak := range leaks {
		if strings.Contains(text, leak) {
			t.Errorf("%s holds %q:\n%s", what, leak, text)
		}
	}
}
