package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/service"
)

// Only a delivered or abandoned workstream is archived. An archived one is
// marked in its own status and named on one line of the list, and nothing of
// it is deleted; unarchive returns it to the list.
func TestArchiveCommands(t *testing.T) {
	root, project, _ := handInFixture(t)
	id := handInStdin(t, root, project, "# Design\n")
	kept := handInStdin(t, root, project, "# Other\n")
	handed := filepath.Join(root, "projects", string(project), "workstreams", id, "handed", "stdin")

	code, out, diag := invoke(t, root, "archive", id)
	if code != 5 || out != "" || diag != "conflict: workstream "+id+" is handed; only a delivered or abandoned workstream can be archived, so abandon it first\n" {
		t.Fatalf("archived work in progress: %d %q %q", code, out, diag)
	}

	successful(t, root, "abandon", id, "Superseded.")
	if text := successful(t, root, "archive", id); text != "Workstream "+id+" archived; osmia unarchive "+id+" returns it to the list\n" {
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
	if data, err := os.ReadFile(handed); err != nil || string(data) != "# Design\n" {
		t.Fatalf("handed copy %q %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "runtime.json")); err != nil || !strings.Contains(string(data), `"workstream": "`+id+`"`) {
		t.Fatalf("runtime.json %s %v", data, err)
	}

	var result service.ArchiveResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "unarchive", id, "--json")), &result))
	if string(result.Workstream) != id || result.Project != project || result.Archived {
		t.Fatalf("json %+v", result)
	}
	if list := successful(t, root, "status"); strings.Contains(list, "Archived:") || !strings.Contains(list, "  "+id+" state=abandoned") {
		t.Fatalf("status list after unarchiving:\n%s", list)
	}
	if text := successful(t, root, "unarchive", id); text != "Workstream "+id+" is in the list of work\n" {
		t.Fatalf("unarchive output %q", text)
	}

	for _, args := range [][]string{{"archive"}, {"archive", "w_x"}, {"archive", id, "extra"}, {"unarchive"}, {"unarchive", "w_x"}} {
		if code, _, _ := invoke(t, root, args...); code != 2 {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}
