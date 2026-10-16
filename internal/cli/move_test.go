package cli

import (
	"strings"
	"testing"
)

// A unit move and a workstream move reach the service and report why it
// refuses one: a workstream moves its units only while building or
// assembled, moves to assembled only once it is built, and never moves once
// abandoned.
func TestMoveCommand(t *testing.T) {
	root, project, _ := handInFixture(t)
	id := handInStdin(t, root, project, "# Design\n")

	code, out, diag := invoke(t, root, "move", id, "resume", "implementing", "Send it back.")
	if code != 5 || out != "" || !strings.HasPrefix(diag, "conflict: the workstream is ") || !strings.HasSuffix(diag, "; units move while it is building or assembled\n") {
		t.Fatalf("move before building: %d %q %q", code, out, diag)
	}
	code, out, diag = invoke(t, root, "move", id, "assembled", "Review it again.")
	if code != 5 || out != "" || !strings.HasPrefix(diag, "conflict: the workstream is ") || !strings.HasSuffix(diag, "; it moves to assembled once it is building, assembled or delivered with an open pull request\n") {
		t.Fatalf("workstream move before building: %d %q %q", code, out, diag)
	}
	if code, _, _ := invoke(t, root, "move", id, "implementing"); code != 2 {
		t.Fatalf("move with two arguments exited %d", code)
	}
	successful(t, root, "abandon", id, "Superseded.")
	code, out, diag = invoke(t, root, "move", id, "building", "Reopen it.")
	if code != 5 || out != "" || diag != "conflict: an abandoned workstream does not move\n" {
		t.Fatalf("workstream move after abandoning: %d %q %q", code, out, diag)
	}
	code, out, diag = invoke(t, root, "move", id, "resume", "implementing", "Send it back.")
	if code != 5 || out != "" || diag != "conflict: an abandoned workstream's units do not move\n" {
		t.Fatalf("move after abandoning: %d %q %q", code, out, diag)
	}
}
