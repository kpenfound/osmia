package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Only a delivered or abandoned workstream is archived. An archived one is
// marked in its own status and named on one line of the list. Archive is permanent.
func TestArchiveCommands(t *testing.T) {
	root, project, _ := handInFixture(t)
	id := handInStdin(t, root, project, "# Design\n")
	kept := handInStdin(t, root, project, "# Other\n")

	code, out, diag := invoke(t, root, "archive", id)
	if code != 5 || out != "" || diag != "conflict: workstream "+id+" is handed; only a delivered or abandoned workstream can be archived, so abandon it first\n" {
		t.Fatalf("archived work in progress: %d %q %q", code, out, diag)
	}

	successful(t, root, "abandon", id, "Superseded.")
	if text := successful(t, root, "archive", id); text != "Workstream "+id+" permanently archived; its title remains and its trace will be deleted\n" {
		t.Fatalf("archive output %q", text)
	}
	successful(t, root, "archive", id)
	if status := successful(t, root, "status", id); !strings.Contains(status, "state=abandoned") || !strings.Contains(status, "archived=true") {
		t.Fatalf("status of the archived workstream:\n%s", status)
	}
	list := successful(t, root, "status")
	if strings.Contains(list, "  "+id+" state=") || !strings.Contains(list, "  Archived: "+id+"\n") || !strings.Contains(list, "  "+kept+" state=") {
		t.Fatalf("status list:\n%s", list)
	}
	if data, err := os.ReadFile(filepath.Join(root, "runtime.json")); err != nil || !strings.Contains(string(data), `"workstream": "`+id+`"`) {
		t.Fatalf("runtime.json %s %v", data, err)
	}

	if code, _, diag := invoke(t, root, "unarchive", id); code != 5 || !strings.Contains(diag, "archive is permanent") {
		t.Fatalf("unarchive: %d %s", code, diag)
	}

	for _, args := range [][]string{{"archive"}, {"archive", "w_x"}, {"archive", id, "extra"}, {"unarchive"}, {"unarchive", "w_x"}} {
		if code, _, _ := invoke(t, root, args...); code != 2 {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}
