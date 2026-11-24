package cli

import (
	"encoding/json"
	"strings"
	"testing"

	wire "github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
)

type abandonRoot struct {
	root, id string
	project  config.ProjectID
}

func newAbandonRoot(t *testing.T) abandonRoot {
	t.Helper()
	root, project, _ := handInFixture(t)
	return abandonRoot{root: root, project: project, id: handInStdin(t, root, project, "# Design\n")}
}

func TestAbandonReportsTheReason(t *testing.T) {
	a := newAbandonRoot(t)
	text := successful(t, a.root, "abandon", a.id, "Superseded by the upload design.")
	if want := "Workstream " + a.id + " abandoned\nReason: Superseded by the upload design.\n"; text != want {
		t.Fatalf("output %q, want %q", text, want)
	}
}

func TestAbandonedWorkstreamShowsAsAbandonedInStatus(t *testing.T) {
	a := newAbandonRoot(t)
	successful(t, a.root, "abandon", a.id, "Superseded.")
	if status := successful(t, a.root, "status", a.id); !strings.Contains(status, "abandoned") {
		t.Fatalf("status:\n%s", status)
	}
}

func TestAbandonKeepsTheHandedCopy(t *testing.T) {
	a := newAbandonRoot(t)
	successful(t, a.root, "abandon", a.id, "Superseded.")
	db := workstreamDatabase(t, a.root, a.project, a.id)
	if handed := latestDocument(t, db, "workstreams/"+a.id+"/documents.jsonl", "handed/stdin"); handed.Content != "# Design\n" {
		t.Fatalf("handed copy %+v", handed)
	}
}

func TestAbandonRecordsTheTransition(t *testing.T) {
	a := newAbandonRoot(t)
	successful(t, a.root, "abandon", a.id, "Superseded.")
	if len(traceBodies(t, workstreamDatabase(t, a.root, a.project, a.id), "workstreams/"+a.id+"/events.jsonl")) == 0 {
		t.Fatal("the abandoned workstream records no transitions")
	}
}

func TestAbandonTwiceIsAConflict(t *testing.T) {
	a := newAbandonRoot(t)
	successful(t, a.root, "abandon", a.id, "Superseded.")
	code, out, diag := invoke(t, a.root, "abandon", a.id, "again")
	if code != 5 || out != "" || diag != "conflict: workstream "+a.id+" is abandoned and cannot be abandoned\n" {
		t.Fatalf("abandoned again: %d %q %q", code, out, diag)
	}
}

func TestAbandonRefusesAnEmptyReason(t *testing.T) {
	a := newAbandonRoot(t)
	code, out, diag := invoke(t, a.root, "abandon", a.id, "")
	if code != 4 || out != "" || diag != "validation: reason must not be empty\n" {
		t.Fatalf("empty reason: %d %q %q", code, out, diag)
	}
}

func TestAbandonReportsTheAbandonmentAsJSON(t *testing.T) {
	a := newAbandonRoot(t)
	var result wire.AbandonResponse
	must(t, json.Unmarshal([]byte(successful(t, a.root, "abandon", a.id, "Not needed.", "--json")), &result))
	if string(result.Workstream) != a.id || result.Project != a.project || result.State != "abandoned" || result.Reason != "Not needed." {
		t.Fatalf("json %+v", result)
	}
}

func TestAbandonRefusesInvalidArguments(t *testing.T) {
	a := newAbandonRoot(t)
	for _, args := range [][]string{{"abandon", a.id}, {"abandon", "w_x", "reason"}, {"abandon", a.id, "a", "b"}} {
		if code, _, _ := invoke(t, a.root, args...); code != 2 {
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
	var handed wire.HandInResponse
	must(t, json.Unmarshal([]byte(out), &handed))
	return string(handed.Workstream)
}
