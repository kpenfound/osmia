package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

// An archived workstream persists with the time it was first archived;
// archiving it again keeps that time, and clearing it returns it to the list
// of work. Only a workstream of an active project can be archived.
func TestArchivedWorkstreamsPersistAndClear(t *testing.T) {
	in := fixture(t)
	s := open(t, in)
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	must(t, s.SetArchived(Archive{Project: pid, Workstream: w2, ArchivedAt: at}))
	must(t, s.SetArchived(Archive{Project: pid, Workstream: w1, ArchivedAt: at.Add(time.Hour)}))
	must(t, s.SetArchived(Archive{Project: pid, Workstream: w2, ArchivedAt: at.Add(2 * time.Hour)}))
	want := []Archive{{Project: pid, Workstream: w1, ArchivedAt: at.Add(time.Hour)}, {Project: pid, Workstream: w2, ArchivedAt: at}}
	restarted := open(t, in)
	effective, ds := restarted.Effective()
	if len(ds) != 0 || !reflect.DeepEqual(effective.Archived, want) {
		t.Fatalf("archived after a restart %+v %v", effective.Archived, ds)
	}

	for _, a := range []Archive{
		{Project: pid, Workstream: "w_2123456789abcdef0123456789abcdef"},
		{Project: "p_2123456789abcdef0123456789abcdef", Workstream: w1},
		{Project: pid, Workstream: "w_x"},
	} {
		if err := restarted.SetArchived(a); !errors.Is(err, ErrValidation) {
			t.Fatalf("archived %+v: %v", a, err)
		}
	}

	must(t, restarted.ClearArchived(w1))
	must(t, restarted.ClearArchived(w1))
	if state, _ := restarted.Snapshot(); !reflect.DeepEqual(state.Archived, want[1:]) {
		t.Fatalf("archived after clearing %+v", state.Archived)
	}
	if err := restarted.ClearArchived("w_x"); !errors.Is(err, ErrValidation) {
		t.Fatalf("cleared an invalid workstream: %v", err)
	}
}

// A workstream that is no longer known stays recorded as archived, is
// diagnosed and left out of the state in force, and can still be cleared.
func TestStaleArchiveIsDiagnosedAndClearable(t *testing.T) {
	in := fixture(t)
	s := open(t, in)
	must(t, s.SetArchived(Archive{Project: pid, Workstream: w1}))
	changed, _ := copyInputs(in)
	changed.Workstreams = map[config.ProjectID][]config.WorkstreamID{pid: {w2}}
	must(t, s.Resolve(changed))
	effective, ds := s.Effective()
	if len(effective.Archived) != 0 || len(ds) != 1 || ds[0].Field != "archived[0]" {
		t.Fatalf("stale archive %+v %v", effective.Archived, ds)
	}
	if raw, _ := s.Snapshot(); len(raw.Archived) != 1 {
		t.Fatalf("stale archive dropped %+v", raw.Archived)
	}
	must(t, s.ClearArchived(w1))
	if raw, ds := s.Snapshot(); len(raw.Archived) != 0 || len(ds) != 0 {
		t.Fatalf("after clearing %+v %v", raw.Archived, ds)
	}
}

// A runtime file with a duplicate or untimed archive does not open.
func TestInvalidArchivesRefused(t *testing.T) {
	for _, body := range []string{
		`{"version":1,"archived":[{"project":"` + string(pid) + `","workstream":"` + string(w1) + `","archived_at":"2026-09-29T12:00:00Z"},{"project":"` + string(pid) + `","workstream":"` + string(w1) + `","archived_at":"2026-09-29T12:00:00Z"}]}`,
		`{"version":1,"archived":[{"project":"` + string(pid) + `","workstream":"` + string(w1) + `"}]}`,
	} {
		in := fixture(t)
		must(t, os.WriteFile(filepath.Join(in.Config.Root.String(), "runtime.json"), []byte(body), 0600))
		if s, _, err := Open(in); err == nil {
			s.Close()
			t.Fatalf("opened %s", body)
		}
	}
}
