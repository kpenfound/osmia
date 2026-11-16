package cli

import (
	"strings"
	"testing"
)

func TestChecksCommandAnswersOnlyAnOpenQuestion(t *testing.T) {
	root, project, _ := handInFixture(t)
	id := handInStdin(t, root, project, "# Design\n")
	code, out, diag := invoke(t, root, "checks", id, "continue")
	if code != 1 || out != "" || !strings.Contains(diag, "no question about the check results of workstream "+id+" waits for you") {
		t.Fatalf("no question open: %d %q %q", code, out, diag)
	}
	for _, args := range [][]string{{"checks"}, {"checks", id}, {"checks", id, "approve"}, {"checks", id, "continue", "extra"}} {
		if code, _, _ := invoke(t, root, args...); code != 2 {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}
