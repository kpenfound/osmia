package cli

import (
	"strings"
	"testing"
)

func TestMergedCommandRefusesAWorkstreamThatIsNotAssembled(t *testing.T) {
	root, project, _ := handInFixture(t)
	id := handInStdin(t, root, project, "# Design\n")
	code, out, diag := invoke(t, root, "merged", id)
	if code != 5 || out != "" || !strings.HasPrefix(diag, "conflict: workstream "+id+" is ") || !strings.HasSuffix(diag, "; only an assembled workstream is recorded merged\n") {
		t.Fatalf("a handed workstream: %d %q %q", code, out, diag)
	}
	for _, args := range [][]string{{"merged"}, {"merged", "w_x"}, {"merged", id, "extra"}} {
		if code, _, _ := invoke(t, root, args...); code != 2 {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}
