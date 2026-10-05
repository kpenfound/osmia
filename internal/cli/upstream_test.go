package cli

import "testing"

func TestUpstreamCommandRefusesAWorkstreamThatIsNotParked(t *testing.T) {
	root, project, _ := handInFixture(t)
	id := handInStdin(t, root, project, "# Design\n")
	base := handInStdin(t, root, project, "# Base\n")
	code, out, diag := invoke(t, root, "upstream", id, base)
	if code != 5 || out != "" || diag != "conflict: workstream "+id+" is not parked on its base\n" {
		t.Fatalf("an unparked workstream: %d %q %q", code, out, diag)
	}
	for _, args := range [][]string{{"upstream", id}, {"upstream", "w_x", base}, {"upstream", id, "w_x"}, {"upstream", id, base, "extra"}} {
		if code, _, _ := invoke(t, root, args...); code != 2 {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}
