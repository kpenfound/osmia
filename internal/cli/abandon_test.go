package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/service"
)

func TestAbandonCommand(t *testing.T) {
	root, project, _ := handInFixture(t)
	id := handInStdin(t, root, project, "# Design\n")
	handed := filepath.Join(root, "projects", string(project), "workstreams", id, "handed", "stdin")

	text := successful(t, root, "abandon", id, "Superseded by the upload design.")
	if want := "Workstream " + id + " abandoned\nReason: Superseded by the upload design.\n"; text != want {
		t.Fatalf("output %q, want %q", text, want)
	}
	if status := successful(t, root, "status", id); !strings.Contains(status, "abandoned") {
		t.Fatalf("status:\n%s", status)
	}
	if data, err := os.ReadFile(handed); err != nil || string(data) != "# Design\n" {
		t.Fatalf("handed copy %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(root, "projects", string(project), "workstreams", id, "events.jsonl")); err != nil {
		t.Fatal(err)
	}

	code, out, diag := invoke(t, root, "abandon", id, "again")
	if code != 5 || out != "" || diag != "conflict: workstream "+id+" is abandoned and cannot be abandoned\n" {
		t.Fatalf("abandoned again: %d %q %q", code, out, diag)
	}
	code, out, diag = invoke(t, root, "abandon", id, "")
	if code != 4 || out != "" || diag != "validation: reason must not be empty\n" {
		t.Fatalf("empty reason: %d %q %q", code, out, diag)
	}

	other := handInStdin(t, root, project, "other")
	var result service.AbandonResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "abandon", other, "Not needed.", "--json")), &result))
	if string(result.Workstream) != other || result.Project != project || result.State != "abandoned" || result.Reason != "Not needed." {
		t.Fatalf("json %+v", result)
	}

	for _, args := range [][]string{{"abandon", id}, {"abandon", "w_x", "reason"}, {"abandon", id, "a", "b"}} {
		if code, _, _ := invoke(t, root, args...); code != 2 {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}

func handInStdin(t *testing.T, root string, project config.ProjectID, text string) string {
	t.Helper()
	code, out, diag := invokeInput(t, root, text, "handin", string(project), "-", "--json")
	if code != 0 || diag != "" {
		t.Fatalf("handin: %d %s %s", code, out, diag)
	}
	var handed service.HandInResponse
	must(t, json.Unmarshal([]byte(out), &handed))
	return string(handed.Workstream)
}
