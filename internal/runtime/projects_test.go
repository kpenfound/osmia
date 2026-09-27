package runtime

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
)

const pidOther config.ProjectID = "p_fedcba9876543210fedcba9876543210"
const wOther config.WorkstreamID = "w_fedcba9876543210fedcba9876543210"

// twoProjects extends the fixture with a second active project that has one
// workstream of its own.
func twoProjects(t *testing.T) Inputs {
	t.Helper()
	in := fixture(t)
	root := in.Config.Root.String()
	must(t, os.MkdirAll(filepath.Join(root, "projects", string(pidOther)), 0700))
	must(t, os.WriteFile(filepath.Join(root, "projects", string(pidOther), "config.toml"), []byte(`version = 1
upstream = "upstream/other"
fork = "owner/other"
clone = "`+filepath.Join(root, "..", "other")+`"
`), 0600))
	top, err := os.ReadFile(filepath.Join(root, "config.toml"))
	must(t, err)
	must(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(strings.Replace(string(top), `active_projects = ["`+string(pid)+`"]`, `active_projects = ["`+string(pid)+`", "`+string(pidOther)+`"]`, 1)), 0600))
	c, err := config.Load(config.Options{Root: root})
	must(t, err)
	in.Config = c
	in.Workstreams[pidOther] = []config.WorkstreamID{wOther}
	return in
}

func TestReferencesToEveryActiveProject(t *testing.T) {
	in := twoProjects(t)
	s := open(t, in)
	must(t, s.SetPause(Pause{Target: Target{Scope: "project", Project: pidOther}, Mode: "soft", Source: PauseOwner, Reason: "test"}))
	must(t, s.SetPause(Pause{Target: Target{Scope: "workstream", Project: pid, Workstream: w1}, Mode: "soft", Source: PauseOwner, Reason: "test"}))
	must(t, s.SetPriority([]Ranked{{pid, w2}, {pidOther, wOther}, {pid, w1}}))
	// A workstream is known only in its own project.
	if err := s.SetPause(Pause{Target: Target{Scope: "workstream", Project: pid, Workstream: wOther}, Mode: "soft", Source: PauseOwner, Reason: "test"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("another project's workstream accepted: %v", err)
	}
	effective, ds := s.Effective()
	if len(ds) != 0 || len(effective.Pauses) != 2 || !slices.Equal(effective.Priority, []Ranked{{pid, w2}, {pidOther, wOther}, {pid, w1}}) {
		t.Fatalf("%+v %v", effective, ds)
	}
	// Once the second project is inactive its references are stale, and the
	// first project's still apply.
	one := in
	one.Config = in.Config.WithoutProjectID(pidOther)
	one.Workstreams = maps.Clone(in.Workstreams)
	delete(one.Workstreams, pidOther)
	must(t, s.Resolve(one))
	effective, ds = s.Effective()
	if len(ds) != 2 || len(effective.Pauses) != 1 || effective.Pauses[0].Target.Project != pid || !slices.Equal(effective.Priority, []Ranked{{pid, w2}, {pid, w1}}) {
		t.Fatalf("%+v %v", effective, ds)
	}
	for _, d := range ds {
		if !strings.Contains(d.Reason, "inactive project "+string(pidOther)) {
			t.Fatalf("diagnostic %+v", d)
		}
	}
	// Workstreams of a project that is not loaded are refused.
	if err := s.Resolve(Inputs{Config: one.Config, Workstreams: map[config.ProjectID][]config.WorkstreamID{pidOther: {wOther}}}); err == nil {
		t.Fatal("workstreams of an inactive project accepted")
	}
}

// A runtime file that holds each project's own order keeps loading: the
// orders take their places after the one order, place by place across the
// projects, and those of an inactive project are stale.
func TestProjectOrdersLoadIntoTheOneOrder(t *testing.T) {
	in := twoProjects(t)
	body := `{"version":1,"priority":[{"project":"` + string(pid) + `","workstream":"` + string(w1) + `"}],"priorities":[` +
		`{"project":"` + string(pid) + `","workstreams":["` + string(w1) + `","` + string(w2) + `"]},` +
		`{"project":"` + string(pidOther) + `","workstreams":["` + string(wOther) + `"]}]}`
	must(t, os.WriteFile(filepath.Join(in.Config.Root.String(), "runtime.json"), []byte(body), 0600))
	s := open(t, in)
	effective, ds := s.Effective()
	if want := []Ranked{{pid, w1}, {pid, w2}, {pidOther, wOther}}; len(ds) != 0 || !slices.Equal(effective.Priority, want) || effective.Priorities != nil {
		t.Fatalf("effective %+v %v, want %v", effective, ds, want)
	}
	one := in
	one.Config = in.Config.WithoutProjectID(pidOther)
	one.Workstreams = maps.Clone(in.Workstreams)
	delete(one.Workstreams, pidOther)
	must(t, s.Resolve(one))
	effective, ds = s.Effective()
	if !slices.Equal(effective.Priority, []Ranked{{pid, w1}, {pid, w2}}) || len(ds) != 1 || ds[0].Field != "priorities[1]" || ds[0].Reason != "inactive project "+string(pidOther) {
		t.Fatalf("with the second project inactive: %+v %v", effective.Priority, ds)
	}
	must(t, s.Resolve(in))

	// Without the one order, the first of each project's order goes first.
	body = `{"version":1,"priorities":[` +
		`{"project":"` + string(pid) + `","workstreams":["` + string(w2) + `","` + string(w1) + `"]},` +
		`{"project":"` + string(pidOther) + `","workstreams":["` + string(wOther) + `"]}]}`
	must(t, os.WriteFile(filepath.Join(in.Config.Root.String(), "runtime.json"), []byte(body), 0600))
	s = open(t, in)
	effective, _ = s.Effective()
	if want := []Ranked{{pid, w2}, {pidOther, wOther}, {pid, w1}}; !slices.Equal(effective.Priority, want) {
		t.Fatalf("effective %v, want %v", effective.Priority, want)
	}
	// Setting the order replaces the projects' own orders.
	must(t, s.SetPriority([]Ranked{{pidOther, wOther}}))
	stored, _ := s.Snapshot()
	if !slices.Equal(stored.Priority, []Ranked{{pidOther, wOther}}) || stored.Priorities != nil {
		t.Fatalf("stored %+v", stored)
	}
}

// A stale workstream of the one order is reported and left out of the
// effective order.
func TestStaleEntriesOfTheOneOrder(t *testing.T) {
	in := twoProjects(t)
	s := open(t, in)
	must(t, s.SetPriority([]Ranked{{pidOther, wOther}, {pid, w2}}))
	one := in
	one.Config = in.Config.WithoutProjectID(pidOther)
	one.Workstreams = maps.Clone(in.Workstreams)
	delete(one.Workstreams, pidOther)
	must(t, s.Resolve(one))
	effective, ds := s.Effective()
	if !slices.Equal(effective.Priority, []Ranked{{pid, w2}}) || len(ds) != 1 || ds[0].Field != "priority[0]" || ds[0].Reason != "inactive project "+string(pidOther) {
		t.Fatalf("effective %v %v", effective.Priority, ds)
	}
	stored, _ := s.Snapshot()
	if len(stored.Priority) != 2 {
		t.Fatalf("stale entry dropped: %v", stored.Priority)
	}
	one.Workstreams = map[config.ProjectID][]config.WorkstreamID{pid: {w1}}
	must(t, s.Resolve(one))
	if _, ds = s.Effective(); len(ds) != 2 || ds[1].Field != "priority[1]" || ds[1].Reason != "unknown workstream "+string(w2) {
		t.Fatalf("diagnostics %v", ds)
	}
}

// Prioritise puts one project's workstreams first and keeps the other
// projects' places; ClearPriority clears one project's places or all.
func TestPrioritiseAndClearOneProject(t *testing.T) {
	in := twoProjects(t)
	s := open(t, in)
	must(t, s.SetPriority([]Ranked{{pid, w1}, {pidOther, wOther}, {pid, w2}}))
	must(t, s.Prioritise(pid, []config.WorkstreamID{w2}))
	effective, _ := s.Effective()
	if want := []Ranked{{pid, w2}, {pidOther, wOther}}; !slices.Equal(effective.Priority, want) {
		t.Fatalf("after prioritising: %v, want %v", effective.Priority, want)
	}
	if err := s.Prioritise(pid, []config.WorkstreamID{wOther}); !errors.Is(err, ErrValidation) {
		t.Fatalf("another project's workstream prioritised: %v", err)
	}
	before, _ := s.Snapshot()
	must(t, s.ClearPriority(pidOther))
	if effective, _ = s.Effective(); !slices.Equal(effective.Priority, []Ranked{{pid, w2}}) {
		t.Fatalf("after clearing the other project: %v", effective.Priority)
	}
	must(t, s.RestorePriority(before))
	if effective, _ = s.Effective(); !slices.Equal(effective.Priority, before.Priority) {
		t.Fatalf("after restoring: %v", effective.Priority)
	}
	must(t, s.ClearPriority(""))
	if effective, _ = s.Effective(); len(effective.Priority) != 0 {
		t.Fatalf("after clearing: %v", effective.Priority)
	}
}
