package trace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace/legacytrace"
)

// legacyFixture is a project and its workstream as Git traces, with what
// each holds.
type legacyFixture struct {
	root    config.Root
	project config.Project
	// projectDir and streamDir are the project's and the workstream's trace
	// directories.
	projectDir, streamDir string
	// records holds each trace's records grouped by record path, in order,
	// and workflow the workstream's workflow.
	records  map[string][]string
	workflow workflowLog
	kb       map[string]string
}

// v2Workflow encodes log as a workflow.json of the current version.
func v2Workflow(t *testing.T, log workflowLog) []byte {
	t.Helper()
	data, err := json.MarshalIndent(workflowFile{Schema: "osmia.workflow", Version: workflowVersion, Transactions: log.Transactions, Deliveries: log.Deliveries, Operations: log.Operations, Threads: storeThreads(log.Threads)}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

// exportLegacy returns the files a Git trace holds for the trace r holds:
// each record log, each stored file, each document's latest revision, and
// the workstream's workflow encoded by workflow.
func exportLegacy(t *testing.T, r *Repository, workflow func(*testing.T, workflowLog) []byte) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	if r.stream == "" {
		files["documents.jsonl"] = nil
	} else {
		for name, data := range legacytrace.Workstream(string(r.project), string(r.stream), at) {
			if strings.HasSuffix(name, ".jsonl") {
				files[name] = data
			}
		}
	}
	for _, name := range storedPaths(t, r) {
		var data []byte
		for _, body := range storedBodies(t, r, name) {
			data = append(append(data, body...), '\n')
		}
		files[name] = data
	}
	rows, err := r.db.Query("SELECT path, content FROM files")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		var content []byte
		if err := rows.Scan(&name, &content); err != nil {
			t.Fatal(err)
		}
		files[name] = content
	}
	rows.Close()
	docs, err := Read[Document](r, r.stream)
	if err != nil {
		t.Fatal(err)
	}
	latest := map[string]Document{}
	for _, d := range docs {
		latest[scopePath(d.Header)+d.Path] = d
	}
	for name, d := range latest {
		if d.Workstream == "" && prose(d.Path) && d.Content == "" {
			continue
		}
		files[name] = []byte(d.Content)
	}
	if r.stream != "" {
		r.mu.Lock()
		log, _, err := r.loadWorkflow(r.stream)
		r.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		files["workstreams/"+string(r.stream)+"/workflow.json"] = workflow(t, log)
	}
	return files
}

// storedPaths lists the record paths of r's trace.
func storedPaths(t *testing.T, r *Repository) []string {
	t.Helper()
	rows, err := r.db.Query("SELECT DISTINCT path FROM records ORDER BY path")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, name)
	}
	return paths
}

// recordsByPath returns every record body of r's trace by record path.
func recordsByPath(t *testing.T, r *Repository) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, name := range storedPaths(t, r) {
		out[name] = storedBodies(t, r, name)
	}
	return out
}

// newLegacyFixture records a project and a workstream with documents, a
// thread and a transaction, then replaces both traces with Git traces of the
// same content, the workstream's workflow encoded by workflow.
func newLegacyFixture(t *testing.T, workflow func(*testing.T, workflowLog) []byte) *legacyFixture {
	t.Helper()
	ctx := context.Background()
	r, root, p := create(t)
	if err := r.RecordDocuments(ctx, []Document{projectDocument("subsystem-trace", "kb/trace.md", "# trace\n", 1), projectDocument("subsystem-gone", "kb/gone.md", "# gone\n", 1)}); err != nil {
		t.Fatal(err)
	}
	if err := r.RecordDocuments(ctx, []Document{projectDocument("subsystem-gone", "kb/gone.md", "", 2)}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.reserveInboxNumber(ctx); err != nil {
		t.Fatal(err)
	}
	notes := change{message: "Record notes"}
	notes.setFile("notes/mason.md", []byte("remember\n"))
	r.mu.Lock()
	err := r.publish(ctx, notes)
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := r.RecordDocuments(ctx, []Document{streamDocument("spec", "spec.md", "# Spec\n", 1), streamDocument("plan", "plan.json", "{}\n", 1)}); err != nil {
		t.Fatal(err)
	}
	transact(t, r, transaction("initial", 0, "", "in-shed", 2))
	if err := r.CreateThread(ctx, threadAgent()); err != nil {
		t.Fatal(err)
	}
	enqueue(t, r, "one")
	q := claimTurn(t, r, "token")
	if err := r.CaptureTurn(ctx, "token", threadResponse(q)); err != nil {
		t.Fatal(err)
	}
	enqueue(t, r, "two")
	s := mustScoped(t, r, streamID)
	f := &legacyFixture{root: root, project: p, records: map[string][]string{}}
	f.projectDir, f.streamDir = r.directory, s.directory
	for name, bodies := range recordsByPath(t, r) {
		f.records[name] = bodies
	}
	for name, bodies := range recordsByPath(t, s) {
		f.records[name] = bodies
	}
	s.mu.Lock()
	f.workflow, _, err = s.loadWorkflow(streamID)
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	f.kb = kbFiles(t, r)
	projectFiles, streamFiles := exportLegacy(t, r, workflow), exportLegacy(t, s, workflow)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	for dir, files := range map[string]map[string][]byte{f.projectDir: projectFiles, f.streamDir: streamFiles} {
		for _, name := range storeFiles {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				t.Fatal(err)
			}
		}
		if err := legacytrace.Build(dir, files); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.projectDir, "config.toml"), []byte("name = \"widgets\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return f
}

// migrationLog collects Migrate's log lines.
type migrationLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *migrationLog) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *migrationLog) has(part string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.ContainsFunc(l.lines, func(line string) bool { return strings.Contains(line, part) })
}

func (f *legacyFixture) migrate(t *testing.T, check func(map[string]string) error) (*migrationLog, error) {
	t.Helper()
	log := &migrationLog{}
	return log, Migrate(context.Background(), f.root, f.project.ID, check, log.logf)
}

// open opens the migrated project.
func (f *legacyFixture) open(t *testing.T) *Repository {
	t.Helper()
	r, err := Open(f.root, f.project)
	if err != nil {
		if r != nil {
			r.Close()
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// check verifies the migrated traces hold what the Git traces held and
// nothing else is left in their directories.
func (f *legacyFixture) check(t *testing.T, r *Repository) {
	t.Helper()
	s := mustScoped(t, r, streamID)
	got := recordsByPath(t, r)
	for name, bodies := range recordsByPath(t, s) {
		got[name] = bodies
	}
	if !reflect.DeepEqual(got, f.records) {
		t.Fatalf("migrated records differ:\n%v\n%v", got, f.records)
	}
	log, _, err := func() (workflowLog, *workflowView, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.loadWorkflow(streamID)
	}()
	if err != nil || !equalJSON(comparable(log), comparable(f.workflow)) {
		t.Fatalf("migrated workflow differs: %v\n%+v\n%+v", err, log, f.workflow)
	}
	if got := kbFiles(t, r); !reflect.DeepEqual(got, f.kb) {
		t.Fatalf("migrated knowledge base: %v, want %v", got, f.kb)
	}
	if notes := fileOf(t, r, "notes/mason.md"); notes != "remember\n" {
		t.Fatalf("notes: %q", notes)
	}
	for dir, want := range map[string][]string{f.projectDir: {"config.toml", lockFile, databaseFile}, f.streamDir: {lockFile, databaseFile}} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range entries {
			if e.Name() != databaseFile+"-wal" && e.Name() != databaseFile+"-shm" {
				names = append(names, e.Name())
			}
		}
		if !reflect.DeepEqual(names, want) {
			t.Fatalf("%s holds %v, want %v", dir, names, want)
		}
	}
}

func TestMigrateMovesGitTracesToDatabases(t *testing.T) {
	for name, workflow := range map[string]func(*testing.T, workflowLog) []byte{"current": v2Workflow, "embedded-turns": embeddedWorkflow} {
		t.Run(name, func(t *testing.T) {
			f := newLegacyFixture(t, workflow)
			log, err := f.migrate(t, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, step := range []string{"starting migration", "importing HEAD", "verifying the import", "switching to the trace database", "removing the Git trace files", "migration complete"} {
				if !log.has("project " + string(f.project.ID) + ": " + step) {
					t.Fatalf("no project log of %q: %v", step, log.lines)
				}
				if !log.has("workstream " + string(streamID) + ": " + step) {
					t.Fatalf("no workstream log of %q: %v", step, log.lines)
				}
			}
			if log.has("dropping") {
				t.Fatalf("a committed trace dropped files: %v", log.lines)
			}
			r := f.open(t)
			f.check(t, r)
			mason := mustThread(t, r)
			if len(mason.Turns) != 2 || mason.Turns[0].Response == nil || mason.Turns[0].Response.Result.FinalResponse != "Captured final" {
				t.Fatalf("migrated thread: %+v", mason)
			}
			r.Close()
			// A second call finds nothing left to do.
			again, err := f.migrate(t, nil)
			if err != nil || again.has("starting migration") {
				t.Fatalf("repeated migration: %v %v", err, again.lines)
			}
		})
	}
}

func TestMigrateFinishesThePublicationJournal(t *testing.T) {
	t.Run("published", func(t *testing.T) {
		f := newLegacyFixture(t, v2Workflow)
		head, err := legacytrace.Head(f.streamDir)
		if err != nil {
			t.Fatal(err)
		}
		spec := "workstreams/" + string(streamID) + "/spec.md"
		// The publication's ref moved, but its spec.md was not rewritten.
		if err := legacytrace.Write(f.streamDir, map[string][]byte{spec: []byte("# Stale\n")}); err != nil {
			t.Fatal(err)
		}
		if err := legacytrace.Publication(f.streamDir, strings.Repeat("0", 40), head, spec); err != nil {
			t.Fatal(err)
		}
		if _, err := f.migrate(t, nil); err != nil {
			t.Fatal(err)
		}
		r := f.open(t)
		f.check(t, r)
		if got := fileOf(t, mustScoped(t, r, streamID), spec); got != "# Spec\n" {
			t.Fatalf("spec after the finished publication: %q", got)
		}
	})
	t.Run("unpublished", func(t *testing.T) {
		f := newLegacyFixture(t, v2Workflow)
		head, err := legacytrace.Head(f.streamDir)
		if err != nil {
			t.Fatal(err)
		}
		if err := legacytrace.Publication(f.streamDir, head, strings.Repeat("1", 40), "workstreams/"+string(streamID)+"/workflow.json"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.migrate(t, nil); err != nil {
			t.Fatal(err)
		}
		f.check(t, f.open(t))
	})
}

func TestMigrateDropsUncommittedBytes(t *testing.T) {
	f := newLegacyFixture(t, v2Workflow)
	if err := legacytrace.Write(f.projectDir, map[string][]byte{"kb/stray.md": []byte("# stray\n"), "notes/mason.md": []byte("unsaved\n")}); err != nil {
		t.Fatal(err)
	}
	log, err := f.migrate(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"dropping kb/stray.md", "dropping notes/mason.md"} {
		if !log.has(name) {
			t.Fatalf("no log of %q: %v", name, log.lines)
		}
	}
	for _, line := range log.lines {
		if strings.Contains(line, "unsaved") || strings.Contains(line, "# stray") {
			t.Fatalf("the log carries content: %q", line)
		}
	}
	f.check(t, f.open(t))
}

func TestMigrateRecordsOwnerHandEdits(t *testing.T) {
	f := newLegacyFixture(t, v2Workflow)
	spec := "workstreams/" + string(streamID) + "/spec.md"
	if err := legacytrace.Write(f.projectDir, map[string][]byte{"charter.md": []byte("# Charter\n\n1. Test everything.\n")}); err != nil {
		t.Fatal(err)
	}
	if err := legacytrace.Write(f.streamDir, map[string][]byte{spec: []byte("# Spec INVALID\n")}); err != nil {
		t.Fatal(err)
	}
	var checked map[string]string
	check := func(content map[string]string) error {
		checked = content
		if strings.Contains(content["spec"], "INVALID") {
			return errors.New("the spec cites no criterion")
		}
		return nil
	}
	log, err := f.migrate(t, check)
	if !errors.Is(err, ErrMigrationIncomplete) || !strings.Contains(err.Error(), spec) || !strings.Contains(err.Error(), "the spec cites no criterion") {
		t.Fatalf("an invalid draft: %v", err)
	}
	if checked["plan"] != "{}\n" {
		t.Fatalf("check saw %v", checked)
	}
	if !log.has("recording the owner's edit of charter.md") {
		t.Fatalf("no charter edit log: %v", log.lines)
	}
	if switched, err := HasTraceDatabase(f.streamDir); err != nil || switched {
		t.Fatalf("a refused draft switched: %v %v", switched, err)
	}
	if _, err := os.Lstat(filepath.Join(f.streamDir, ".git")); err != nil {
		t.Fatalf("a refused draft lost its Git trace: %v", err)
	}
	if err := legacytrace.Write(f.streamDir, map[string][]byte{spec: []byte("# Spec 2\n")}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.migrate(t, check); err != nil {
		t.Fatal(err)
	}
	r := f.open(t)
	charter, err := r.Charter(context.Background())
	if err != nil || charter.Revision != 2 || charter.Content != "# Charter\n\n1. Test everything.\n" || charter.Actor != (Actor{Kind: "owner", ID: "local"}) || charter.Cause != "owner-edit" {
		t.Fatalf("charter: %+v %v", charter, err)
	}
	docs, err := Read[Document](r, streamID)
	if err != nil {
		t.Fatal(err)
	}
	var specs []Document
	for _, d := range docs {
		if d.ID == "spec" {
			specs = append(specs, d)
		}
	}
	if len(specs) != 2 || specs[1].Content != "# Spec 2\n" || specs[1].Actor != (Actor{Kind: "owner", ID: "local"}) || specs[1].Cause != "owner-edit" {
		t.Fatalf("spec revisions: %+v", specs)
	}
}

func TestMigrateRefusesAnUnknownHeadPath(t *testing.T) {
	f := newLegacyFixture(t, v2Workflow)
	if _, err := legacytrace.Commit(f.streamDir, map[string][]byte{"workstreams/" + string(streamID) + "/stray.txt": []byte("?")}, "Stray"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.migrate(t, nil); !errors.Is(err, ErrMigrationIncomplete) || !strings.Contains(err.Error(), "stray.txt") {
		t.Fatalf("an unknown path: %v", err)
	}
	if switched, _ := HasTraceDatabase(f.streamDir); switched {
		t.Fatal("an unknown path switched")
	}
	if _, err := os.Lstat(filepath.Join(f.streamDir, databaseFile+importSuffix)); err == nil {
		t.Fatal("an aborted import left its database")
	}
}

func TestMigrateResumesAfterACrashAtEveryStep(t *testing.T) {
	steps := []string{"migration-journal-finished", "migration-imported", "migration-verified", "migration-switched", "migration-cleaned"}
	for _, step := range steps {
		for _, target := range []string{"project", "workstream"} {
			t.Run(step+"/"+target, func(t *testing.T) {
				f := newLegacyFixture(t, v2Workflow)
				crash := errors.New("crash")
				hit := false
				migrationInterrupt = func(got string) error {
					if !hit && strings.HasPrefix(got, step) && (target == "workstream") == strings.Contains(got, "workstream ") {
						hit = true
						return crash
					}
					return nil
				}
				defer func() { migrationInterrupt = nil }()
				if _, err := f.migrate(t, nil); !errors.Is(err, crash) || !hit {
					t.Fatalf("crash not reached: %v", err)
				}
				migrationInterrupt = nil
				if _, err := f.migrate(t, nil); err != nil {
					t.Fatal(err)
				}
				f.check(t, f.open(t))
			})
		}
	}
}

func TestMigrateRefusesTheSharedLayout(t *testing.T) {
	for name, plant := range map[string]func(dir string) error{
		"workstreams": func(dir string) error { return os.MkdirAll(filepath.Join(dir, "workstreams", string(streamID)), 0700) },
		"reclaim":     func(dir string) error { return os.WriteFile(filepath.Join(dir, legacyReclaimFile), []byte("{}"), 0600) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newLegacyFixture(t, v2Workflow)
			if err := plant(f.projectDir); err != nil {
				t.Fatal(err)
			}
			if _, err := f.migrate(t, nil); !errors.Is(err, ErrMigrationIncomplete) || !strings.Contains(err.Error(), "no longer supported") || !strings.Contains(err.Error(), f.projectDir) {
				t.Fatalf("the shared layout: %v", err)
			}
		})
	}
}

func TestMigrateLeavesDeletingWorkstreamsAlone(t *testing.T) {
	f := newLegacyFixture(t, v2Workflow)
	deleting, err := f.root.WorkstreamDeleting(f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(deleting, "w_00000000000000000000000000000009")
	if err := legacytrace.Build(gone, legacytrace.Workstream(string(f.project.ID), "w_00000000000000000000000000000009", at)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.migrate(t, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(gone, ".git")); err != nil {
		t.Fatalf("a deleting workstream was migrated: %v", err)
	}
	if switched, _ := HasTraceDatabase(gone); switched {
		t.Fatal("a deleting workstream has a database")
	}
	f.check(t, f.open(t))
}

func TestMigrateWorkgroupsMovesTheWorkgroupAndItsHost(t *testing.T) {
	ctx := context.Background()
	root, _ := fixture(t)
	id, err := config.NewWorkgroupID()
	if err != nil {
		t.Fatal(err)
	}
	host, err := config.NewWorkstreamID()
	if err != nil {
		t.Fatal(err)
	}
	r, err := createWorkgroupTraceDirectory(ctx, root, id, at, owner, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.CreateWorkstreamOn(ctx, host, "", at, owner); err != nil {
		r.Close()
		t.Fatal(err)
	}
	s, err := r.scoped(host)
	if err != nil {
		r.Close()
		t.Fatal(err)
	}
	want := map[string][]string{}
	for _, repo := range []*Repository{r, s} {
		for name, bodies := range recordsByPath(t, repo) {
			want[name] = bodies
		}
	}
	hostFiles := exportLegacy(t, s, v2Workflow)
	groupFiles := exportLegacy(t, r, v2Workflow)
	groupDir, hostDir := r.directory, s.directory
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	for dir, files := range map[string]map[string][]byte{groupDir: groupFiles, hostDir: hostFiles} {
		for _, name := range storeFiles {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				t.Fatal(err)
			}
		}
		if err := legacytrace.Build(dir, files); err != nil {
			t.Fatal(err)
		}
	}
	log := &migrationLog{}
	if err := MigrateWorkgroups(ctx, root, log.logf); err != nil {
		t.Fatal(err)
	}
	reopened, err := openSharedWorkgroupTrace(root, id, nil)
	if err != nil {
		if reopened != nil {
			reopened.Close()
		}
		t.Fatal(err)
	}
	defer reopened.Close()
	hostRepo, err := reopened.scoped(host)
	if err != nil {
		t.Fatal(err)
	}
	got := recordsByPath(t, reopened)
	for name, bodies := range recordsByPath(t, hostRepo) {
		got[name] = bodies
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("migrated workgroup records differ:\n%v\n%v", got, want)
	}
	if _, err := hostRepo.ChiefOfStaffThread(host); err != nil {
		t.Fatalf("host chief thread: %v", err)
	}
	for _, dir := range []string{groupDir, hostDir} {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s keeps its Git trace: %v", dir, err)
		}
	}
}
