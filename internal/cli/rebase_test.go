package cli

import (
	"strings"
	"testing"
)

func TestRebaseCommandRefusesAWorkstreamThatIsNotBuilding(t *testing.T) {
	root, project, _ := handInFixture(t)
	id := handInStdin(t, root, project, "# Design\n")
	code, out, diag := invoke(t, root, "rebase", id)
	if code != 5 || out != "" || !strings.HasPrefix(diag, "conflict: workstream "+id+" takes no drift rebase: the workstream is ") {
		t.Fatalf("a handed workstream: %d %q %q", code, out, diag)
	}
	for _, args := range [][]string{{"rebase"}, {"rebase", "w_x"}, {"rebase", id, "extra"}} {
		if code, _, _ := invoke(t, root, args...); code != 2 {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}
