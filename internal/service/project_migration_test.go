package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/doctor"
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/trace"
)

const unlistedProject config.ProjectID = "p_00000000000000000000000000000003"

// rootFiles reads the top-level file and every project file under the root,
// with the migration's journal and store where they exist, so a test can show
// a run changed none of them.
func rootFiles(t *testing.T, root config.Root) map[string]string {
	t.Helper()
	out := map[string]string{}
	paths, err := filepath.Glob(filepath.Join(root.String(), "projects", "*", "config.toml"))
	must(t, err)
	for _, name := range []string{"config.toml", "project-splits.json", "project-store/registry.json"} {
		paths = append(paths, filepath.Join(root.String(), name))
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if info, statErr := os.Stat(path); statErr == nil && info.IsDir() {
			out[path] = "<directory>"
			continue
		}
		must(t, err)
		out[path] = string(data)
	}
	return out
}

func sameFiles(t *testing.T, before, after map[string]string) {
	t.Helper()
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("files changed:\nbefore %q\nafter  %q", before, after)
	}
}

// stepLog records the migration steps a service run reaches.
type stepLog struct {
	mu    sync.Mutex
	names []string
}

func (l *stepLog) record(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.names = append(l.names, name)
}

func (l *stepLog) steps() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.names)
}

func (l *stepLog) index(name string) int { return slices.Index(l.steps(), name) }

// migrationSteps returns the names of the project migration's own steps among
// steps: the split's and the import's.
func migrationSteps(steps []string) []string {
	var out []string
	for _, s := range steps {
		if strings.HasPrefix(s, "split-") || strings.HasPrefix(s, "import-") || strings.HasPrefix(s, "settings-imported:") || strings.HasPrefix(s, "file-retired:") || strings.HasPrefix(s, "revision-") || s == "registry-staged" {
			out = append(out, s)
		}
	}
	return out
}

func (l *stepLog) boundary(failAt string, failure error) func(context.Context, string) error {
	return func(_ context.Context, at string) error {
		l.record(at)
		if at == failAt {
			return failure
		}
		return nil
	}
}

// doctorOptions are doctor's options for root with every host boundary faked.
func doctorOptions(root config.Root, step func(string) error) doctor.Options {
	return doctor.Options{
		Root:        root,
		LookPath:    func(name string) (string, error) { return "/bin/" + name, nil },
		Command:     func(_ context.Context, path string, _ ...string) (string, error) { return filepath.Base(path) + " 1.0.0\n", nil },
		CheckJJ:     func(context.Context) (string, error) { return "0.45.1", nil },
		TotalMemory: func() (uint64, error) { return 16 << 30, nil },
		Getenv:      func(string) string { return "" },
		Step:        step,
	}
}

func doctorCheck(t *testing.T, checks []doctor.Check, name string) doctor.Check {
	t.Helper()
	for _, c := range checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check %q in %+v", name, checks)
	return doctor.Check{}
}

// importedStore asserts that the import is complete and holds exactly the
// projects of ids, in order, each with one revision and one retired mark, and
// returns the store.
func importedStore(t *testing.T, f *twoProjectFixture, ids ...config.ProjectID) *projectstore.Store {
	t.Helper()
	store := projectstore.Open(f.cfg.Root)
	if done, err := store.Imported(); err != nil || !done {
		t.Fatalf("import complete: %t, %v", done, err)
	}
	got, err := store.Projects()
	must(t, err)
	if !slices.Equal(got, ids) {
		t.Fatalf("registry %v, want %v", got, ids)
	}
	top, err := config.LoadTopLevel(f.opts.Config)
	must(t, err)
	for _, id := range ids {
		revisions, err := store.Revisions(id)
		must(t, err)
		if len(revisions) != 1 {
			t.Fatalf("project %s has %d revisions", id, len(revisions))
		}
		path, err := f.cfg.Root.ProjectConfig(id)
		must(t, err)
		data, err := os.ReadFile(path)
		must(t, err)
		if strings.Count(string(data), projectstore.RetiredMark) != 1 || !strings.HasPrefix(string(data), projectstore.RetiredMark) {
			t.Fatalf("project %s file is not retired exactly once: %q", id, data)
		}
		retired, err := top.DecodeProject(id, f.opts.Config.Home, data)
		must(t, err)
		stored, _, err := store.Load(top, f.opts.Config.Home, id)
		must(t, err)
		if !reflect.DeepEqual(retired.Project, stored.Project) {
			t.Fatalf("project %s: the retired file decodes to %+v, the store holds %+v", id, retired.Project, stored.Project)
		}
		if strings.Contains(string(revisions[0].Data), "group_id") {
			t.Fatalf("project %s: the imported revision still names a group: %q", id, revisions[0].Data)
		}
	}
	return store
}

func charterCopies(t *testing.T, f *twoProjectFixture, id config.ProjectID, source trace.Document) int {
	t.Helper()
	r, err := trace.Open(f.cfg.Root, f.cfg.For(id).Project)
	must(t, err)
	defer r.Close()
	docs, err := trace.Read[trace.Document](r, "")
	must(t, err)
	cause := fmt.Sprintf("split-project:%s:charter#%d", project, source.Revision)
	n := 0
	for _, d := range docs {
		if d.Cause == cause {
			n++
		}
	}
	return n
}

// A legacy root imports its list and every listed file at startup. The same
// projects then run with the same effective settings, loaded from the store,
// and the file of an unlisted project, grouped or not and valid or not, is
// never read or changed.
func TestStartupImportsListedProjectsAndLeavesUnlistedGroupedFilesAlone(t *testing.T) {
	t.Parallel()
	f, before, source := splitFixture(t)
	f.opts.Threads = nil
	unlisted := filepath.Join(f.cfg.Root.String(), "projects", string(unlistedProject), "config.toml")
	must(t, os.MkdirAll(filepath.Dir(unlisted), 0700))
	must(t, os.WriteFile(unlisted, []byte("# unlisted\ngroup_id = \""+string(project)+"\"\nupstream = \"https://not-a-repository\"\n"), 0600))
	legacy, err := config.Load(f.opts.Config)
	must(t, err)
	unlistedBefore := rootFiles(t, f.cfg.Root)[unlisted]

	s, err := Start(context.Background(), f.opts)
	must(t, err)
	if !reflect.DeepEqual(s.current().Projects, legacy.Projects) {
		t.Fatalf("effective projects changed:\nlegacy %+v\nnow    %+v", legacy.Projects, s.current().Projects)
	}
	if got := s.current().ActiveProjects; !slices.Equal(got, []string{string(project), string(otherProject)}) {
		t.Fatalf("active projects %v", got)
	}
	must(t, s.Close())

	importedStore(t, f, project, otherProject)
	if got := rootFiles(t, f.cfg.Root)[unlisted]; got != unlistedBefore {
		t.Fatalf("unlisted project file changed: %q", got)
	}
	entries, err := os.ReadDir(filepath.Dir(unlisted))
	must(t, err)
	if len(entries) != 1 {
		t.Fatalf("unlisted project directory gained entries: %v", entries)
	}
	assertSplit(t, f, before, source)
	if n := charterCopies(t, f, otherProject, source); n != 1 {
		t.Fatalf("charter copied %d times", n)
	}
}

// A listed file that does not load stops the start and doctor alike, naming
// the file and the field. The grouped files, the top-level file and the root
// are as they were: no project split began and nothing is marked imported.
func TestInvalidListedFileStopsStartAndDoctorWithNothingChanged(t *testing.T) {
	t.Parallel()
	f, before, _ := splitFixture(t)
	f.opts.Threads = nil
	brokenPath, err := f.cfg.Root.ProjectConfig(otherProject)
	must(t, err)
	must(t, os.WriteFile(brokenPath, []byte(before[otherProject]+"\nlanding = \"sideways\"\n"), 0600))
	files := rootFiles(t, f.cfg.Root)

	var steps stepLog
	f.opts.boundary = steps.boundary("", nil)
	s, err := Start(context.Background(), f.opts)
	if s != nil {
		s.Close()
	}
	var field *config.FieldError
	if !errors.As(err, &field) || field.Path != brokenPath || field.Field != "landing" {
		t.Fatalf("start: %v", err)
	}
	if strings.Contains(err.Error(), "sideways") {
		t.Fatalf("the error quotes the file: %v", err)
	}
	if len(migrationSteps(steps.steps())) != 0 {
		t.Fatalf("migration steps ran: %v", steps.steps())
	}
	sameFiles(t, files, rootFiles(t, f.cfg.Root))

	checks := doctor.Run(context.Background(), doctorOptions(f.cfg.Root, nil))
	c := doctorCheck(t, checks, "project "+string(otherProject))
	if c.Status != doctor.Fail || !strings.Contains(c.Detail, brokenPath) || !strings.Contains(c.Detail, "landing") || !doctor.Failed(checks) {
		t.Fatalf("doctor: %+v", c)
	}
	sameFiles(t, files, rootFiles(t, f.cfg.Root))
	if _, err := os.Stat(filepath.Join(f.cfg.Root.String(), "project-store")); !os.IsNotExist(err) {
		t.Fatalf("the refused runs created a store: %v", err)
	}
}

// A split an earlier run journalled finishes before the import, and the
// import then records the files the split rewrote.
func TestJournalledSplitFinishesBeforeTheImport(t *testing.T) {
	t.Parallel()
	f, before, source := splitFixture(t)
	f.opts.Threads = nil
	stop := errors.New("power loss after the journal")
	var first stepLog
	f.opts.boundary = first.boundary("split-intent-written", stop)
	s, err := Start(context.Background(), f.opts)
	if !errors.Is(err, stop) {
		if s != nil {
			s.Close()
		}
		t.Fatalf("interrupt: %v", err)
	}
	journal := filepath.Join(f.cfg.Root.String(), "project-splits.json")
	if _, err := os.Stat(journal); err != nil {
		t.Fatalf("no journal left: %v", err)
	}
	if done, err := projectstore.Open(f.cfg.Root).Imported(); err != nil || done {
		t.Fatalf("imported before the split finished: %t %v", done, err)
	}

	var second stepLog
	f.opts.boundary = second.boundary("", nil)
	s, err = Start(context.Background(), f.opts)
	must(t, err)
	must(t, s.Close())
	finished, imported := second.index("split-intent-removed"), second.index("import-checked")
	if finished < 0 || imported < 0 || finished > imported {
		t.Fatalf("split finished at %d, import began at %d: %v", finished, imported, second.steps())
	}
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatalf("journal remains: %v", err)
	}
	importedStore(t, f, project, otherProject)
	assertSplit(t, f, before, source)
	if n := charterCopies(t, f, otherProject, source); n != 1 {
		t.Fatalf("charter copied %d times", n)
	}
}

// After the import a restart and doctor read no project file and look for no
// split: the retired files may be unreadable or invalid, the top-level list may
// say anything, and the same projects run with the same settings.
func TestCompletedImportIgnoresRetiredFilesAndTheLeftoverList(t *testing.T) {
	t.Parallel()
	f, _, _ := splitFixture(t)
	f.opts.Threads = nil
	var logs syncBuffer
	f.opts.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	s, err := Start(context.Background(), f.opts)
	must(t, err)
	projects := s.current().Projects
	must(t, s.Close())
	if !strings.Contains(logs.String(), "active_projects is ignored") {
		t.Fatalf("no warning that the list has no effect: %s", logs.String())
	}
	importedStore(t, f, project, otherProject)

	// The first project's file becomes unreadable, the second's invalid and
	// still grouped, which a split would act on, and the list names another
	// project.
	first, err := f.cfg.Root.ProjectConfig(project)
	must(t, err)
	second, err := f.cfg.Root.ProjectConfig(otherProject)
	must(t, err)
	must(t, os.Remove(first))
	must(t, os.Mkdir(first, 0700))
	must(t, os.WriteFile(second, []byte("group_id = \""+string(project)+"\"\nupstream = \"https://not-a-repository\"\n"), 0600))
	top := filepath.Join(f.cfg.Root.String(), "config.toml")
	text, err := os.ReadFile(top)
	must(t, err)
	edited := strings.Replace(string(text), fmt.Sprintf("active_projects = [%q, %q]", project, otherProject), fmt.Sprintf("active_projects = [%q]", unlistedProject), 1)
	if edited == string(text) {
		t.Fatal("the fixture's list was not found")
	}
	must(t, os.WriteFile(top, []byte(edited), 0600))
	files := rootFiles(t, f.cfg.Root)

	for range 2 {
		logs.Reset()
		var steps stepLog
		f.opts.boundary = steps.boundary("", nil)
		s, err := Start(context.Background(), f.opts)
		must(t, err)
		if !reflect.DeepEqual(s.current().Projects, projects) {
			t.Fatalf("projects changed after a restart:\nbefore %+v\nafter  %+v", projects, s.current().Projects)
		}
		must(t, s.Close())
		if got := migrationSteps(steps.steps()); len(got) != 0 {
			t.Fatalf("a completed import ran steps: %v", got)
		}
		if !strings.Contains(logs.String(), "active_projects is ignored") {
			t.Fatalf("no warning that the list has no effect: %s", logs.String())
		}
		sameFiles(t, files, rootFiles(t, f.cfg.Root))
	}

	var steps stepLog
	checks := doctor.Run(context.Background(), doctorOptions(f.cfg.Root, func(name string) error { steps.record(name); return nil }))
	for _, id := range []config.ProjectID{project, otherProject} {
		if c := doctorCheck(t, checks, "project "+string(id)); c.Status != doctor.Pass {
			t.Fatalf("doctor: %+v", c)
		}
	}
	if len(steps.steps()) != 0 {
		t.Fatalf("doctor ran migration steps: %v", steps.steps())
	}
	sameFiles(t, files, rootFiles(t, f.cfg.Root))
}

// An interruption at any step of the migration, in the service or in doctor,
// finishes on the next start or doctor run: each project has one revision and
// one retired mark, the charter is copied once and the settings are the ones
// the files held.
func TestMigrationResumesAfterEveryDurableStep(t *testing.T) {
	var recorded stepLog
	{
		f, _, _ := splitFixture(t)
		f.opts.Threads = nil
		f.opts.boundary = recorded.boundary("", nil)
		s, err := Start(context.Background(), f.opts)
		must(t, err)
		must(t, s.Close())
	}
	steps := migrationSteps(recorded.steps())
	for _, want := range []string{"split-intent-written", "split-intent-removed", "import-checked", "settings-imported:" + string(project), "file-retired:" + string(otherProject), "registry-staged", "import-recorded"} {
		if !slices.Contains(steps, want) {
			t.Fatalf("step %s is not reached: %v", want, steps)
		}
	}
	for _, step := range steps {
		for _, interrupted := range []string{"start", "doctor"} {
			for _, resumed := range []string{"start", "doctor"} {
				t.Run(interrupted+"-"+step+"-then-"+resumed, func(t *testing.T) {
					t.Parallel()
					f, before, source := splitFixture(t)
					f.opts.Threads = nil
					legacy, err := config.Load(f.opts.Config)
					must(t, err)
					stop := errors.New("power loss")
					at := func(name string) error {
						if name == step {
							return stop
						}
						return nil
					}
					switch interrupted {
					case "start":
						f.opts.boundary = func(_ context.Context, name string) error { return at(name) }
						s, err := Start(context.Background(), f.opts)
						if !errors.Is(err, stop) {
							if s != nil {
								s.Close()
							}
							t.Fatalf("interrupt at %s: %v", step, err)
						}
					case "doctor":
						doctor.Run(context.Background(), doctorOptions(f.cfg.Root, at))
					}
					f.opts.boundary = nil
					if resumed == "doctor" {
						checks := doctor.Run(context.Background(), doctorOptions(f.cfg.Root, nil))
						for _, id := range []config.ProjectID{project, otherProject} {
							if c := doctorCheck(t, checks, "project "+string(id)); c.Status != doctor.Pass {
								t.Fatalf("doctor after the interruption: %+v", c)
							}
						}
					}
					for range 2 {
						s, err := Start(context.Background(), f.opts)
						must(t, err)
						if !reflect.DeepEqual(s.current().Projects, legacy.Projects) {
							t.Fatalf("settings changed:\nlegacy %+v\nnow    %+v", legacy.Projects, s.current().Projects)
						}
						must(t, s.Close())
					}
					importedStore(t, f, project, otherProject)
					assertSplit(t, f, before, source)
					if n := charterCopies(t, f, otherProject, source); n != 1 {
						t.Fatalf("charter copied %d times", n)
					}
					if _, err := os.Stat(filepath.Join(f.cfg.Root.String(), "project-splits.json")); !os.IsNotExist(err) {
						t.Fatalf("journal remains: %v", err)
					}
				})
			}
		}
	}
}

// The import writes only under the store and to the listed project files: the
// top-level file stays byte for byte as it was, and the run succeeds when it
// is read-only.
func TestMigrationNeverWritesTheTopLevelFile(t *testing.T) {
	t.Parallel()
	f, _, _ := splitFixture(t)
	f.opts.Threads = nil
	top := filepath.Join(f.cfg.Root.String(), "config.toml")
	before, err := os.ReadFile(top)
	must(t, err)
	must(t, os.Chmod(top, 0400))
	s, err := Start(context.Background(), f.opts)
	must(t, err)
	must(t, s.Close())
	after, err := os.ReadFile(top)
	must(t, err)
	if string(before) != string(after) {
		t.Fatal("startup changed the top-level file")
	}
	info, err := os.Stat(top)
	must(t, err)
	if info.Mode().Perm() != 0400 {
		t.Fatalf("mode %v", info.Mode())
	}
}
