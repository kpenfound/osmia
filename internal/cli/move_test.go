package cli

import (
	"strings"
	"testing"
)

// A move reaches the service and reports why it refuses one: a workstream
// moves its units only while building or assembled, and never once
// abandoned.
func TestMoveCommand(t *testing.T) {
	root, project, _ := handInFixture(t)
	id := handInStdin(t, root, project, "# Design\n")

	code, out, diag := invoke(t, root, "move", id, "resume", "implementing", "Send it back.")
	if code != 5 || out != "" || !strings.HasPrefix(diag, "conflict: the workstream is ") || !strings.HasSuffix(diag, "; units move while it is building or assembled\n") {
		t.Fatalf("move before building: %d %q %q", code, out, diag)
	}
	if code, _, _ := invoke(t, root, "move", id, "resume", "implementing"); code != 2 {
		t.Fatalf("move without a note exited %d", code)
	}
	successful(t, root, "abandon", id, "Superseded.")
	code, out, diag = invoke(t, root, "move", id, "resume", "implementing", "Send it back.")
	if code != 5 || out != "" || diag != "conflict: an abandoned workstream's units do not move\n" {
		t.Fatalf("move after abandoning: %d %q %q", code, out, diag)
	}
}
