package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	wire "github.com/kpenfound/osmia/internal/api"
)

type archiveRoot struct {
	root, id, kept string
}

func startArchiveRoot(t *testing.T) archiveRoot {
	t.Helper()
	root, project, _ := handInFixture(t)
	return archiveRoot{root: root, id: handInStdin(t, root, project, "# Design\n"), kept: handInStdin(t, root, project, "# Other\n")}
}

func (a archiveRoot) archive(t *testing.T) {
	t.Helper()
	successful(t, a.root, "abandon", a.id, "Superseded.")
	successful(t, a.root, "archive", a.id)
}

func TestArchiveRefusesWorkInProgress(t *testing.T) {
	a := startArchiveRoot(t)
	code, out, diag := invoke(t, a.root, "archive", a.id)
	if code != 5 || out != "" || diag != "conflict: workstream "+a.id+" is handed; only a delivered or abandoned workstream can be archived, so abandon it first\n" {
		t.Fatalf("archived work in progress: %d %q %q", code, out, diag)
	}
}

func TestArchiveReportsTheArchivedWorkstream(t *testing.T) {
	a := startArchiveRoot(t)
	successful(t, a.root, "abandon", a.id, "Superseded.")
	if text := successful(t, a.root, "archive", a.id); text != "Workstream "+a.id+" permanently archived; its title remains and its trace will be deleted\n" {
		t.Fatalf("archive output %q", text)
	}
}

func TestArchiveTwiceSucceeds(t *testing.T) {
	a := startArchiveRoot(t)
	a.archive(t)
	successful(t, a.root, "archive", a.id)
}

func TestStatusOfAnArchivedWorkstreamSaysWhoArchivedIt(t *testing.T) {
	a := startArchiveRoot(t)
	a.archive(t)
	if status := successful(t, a.root, "status", a.id); !strings.Contains(status, "state=abandoned") || !strings.Contains(status, "archived=true archived_by=owner") {
		t.Fatalf("status of the archived workstream:\n%s", status)
	}
}

func TestStatusListOmitsAnArchivedWorkstream(t *testing.T) {
	a := startArchiveRoot(t)
	a.archive(t)
	list := successful(t, a.root, "status")
	if strings.Contains(list, a.id) || strings.Contains(list, "Archived:") || !strings.Contains(list, "  "+a.kept+" state=") {
		t.Fatalf("status list:\n%s", list)
	}
}

func TestStatusJSONOfAnArchivedWorkstreamCarriesTheArchivedFlag(t *testing.T) {
	a := startArchiveRoot(t)
	a.archive(t)
	var archived wire.WorkstreamStatus
	must(t, json.Unmarshal([]byte(successful(t, a.root, "status", a.id, "--json")), &archived))
	if string(archived.Workstream) != a.id || !archived.Archived {
		t.Fatalf("archived status --json: %+v", archived)
	}
}

func TestStatusOverviewJSONKeepsTheArchivedWorkstreamFlagged(t *testing.T) {
	a := startArchiveRoot(t)
	a.archive(t)
	var all struct {
		Status wire.StatusResponse `json:"status"`
	}
	must(t, json.Unmarshal([]byte(successful(t, a.root, "status", "--json")), &all))
	for _, st := range all.Status.Workstreams {
		if string(st.Workstream) == a.id {
			if !st.Archived {
				t.Fatalf("overview --json lost the archived flag: %+v", st)
			}
			return
		}
	}
	t.Fatalf("overview --json omits the archived workstream: %+v", all.Status)
}

func TestArchivedWorkstreamStaysInTheRuntimeRecord(t *testing.T) {
	a := startArchiveRoot(t)
	a.archive(t)
	if data, err := os.ReadFile(filepath.Join(a.root, "runtime.json")); err != nil || !strings.Contains(string(data), `"workstream": "`+a.id+`"`) {
		t.Fatalf("runtime.json %s %v", data, err)
	}
}

func TestUnarchiveIsRefusedBecauseArchiveIsPermanent(t *testing.T) {
	a := startArchiveRoot(t)
	a.archive(t)
	if code, _, diag := invoke(t, a.root, "unarchive", a.id); code != 5 || !strings.Contains(diag, "archive is permanent") {
		t.Fatalf("unarchive: %d %s", code, diag)
	}
}

func TestArchiveCommandsRefuseInvalidArguments(t *testing.T) {
	a := startArchiveRoot(t)
	for _, args := range [][]string{{"archive"}, {"archive", "w_x"}, {"archive", a.id, "extra"}, {"unarchive"}, {"unarchive", "w_x"}} {
		if code, _, _ := invoke(t, a.root, args...); code != 2 {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}

func TestStatusFactsNameWhoArchived(t *testing.T) {
	state := "delivered"
	for _, tc := range []struct{ by, want string }{
		{wire.ArchivedByManager, " archived=true archived_by=chief_of_staff"},
		{wire.ArchivedByOwner, " archived=true archived_by=owner"},
		{"", " archived=true"},
	} {
		got := facts(wire.WorkstreamStatus{State: &state, Archived: true, ArchivedBy: tc.by})
		if !strings.HasSuffix(got, tc.want) {
			t.Errorf("archived by %q: %q", tc.by, got)
		}
	}
	if got := facts(wire.WorkstreamStatus{State: &state}); strings.Contains(got, "archived") {
		t.Errorf("live workstream: %q", got)
	}
}
