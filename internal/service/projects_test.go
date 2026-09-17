package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/bundle"
	osmiacharter "github.com/kpenfound/osmia/internal/charter"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

const commentedConfig = `# Owner's notes stay at the top.
version = 1
active_projects = [] # none yet

[profiles.default] # keep this profile
agent = "claude"
model = "test"
[profiles.other]
agent = "codex"
model = "other"
`

// projectFixture writes a root without a project and a local Git clone.
func projectFixture(t *testing.T) (Options, string) {
	t.Helper()
	home, err := os.MkdirTemp("", "op-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	resolved, err := config.ResolveRoot(filepath.Join(home, "root"), "")
	must(t, err)
	root := resolved.String()
	must(t, os.MkdirAll(root, 0700))
	must(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(commentedConfig), 0600))
	clone := filepath.Join(home, "clone")
	must(t, os.Mkdir(clone, 0700))
	demoGit(t, home, "init", "--quiet", clone)
	return Options{Config: config.Options{Root: root}, Build: Identity{"test", "abc"}, ShutdownTimeout: 100 * time.Millisecond}, clone
}
func request(clone string) ProjectAddRequest {
	return ProjectAddRequest{Name: "dagger", Upstream: "dagger/dagger", Fork: "owner/dagger", Clone: clone}
}
func projectDirectories(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "projects"))
	if os.IsNotExist(err) {
		return nil
	}
	must(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
func hasDiagnostic(ds []Diagnostic, code Code) bool {
	for _, d := range ds {
		if d.Code == code {
			return true
		}
	}
	return false
}
func activeProjects(t *testing.T, root string) []string {
	t.Helper()
	cfg, err := config.Load(config.Options{Root: root})
	must(t, err)
	return cfg.ActiveProjects
}

func TestZeroProjectStart(t *testing.T) {
	t.Parallel()
	opts, _ := projectFixture(t)
	s, c := start(t, opts)
	ctx := context.Background()
	cfg, err := c.Configuration(ctx)
	must(t, err)
	if cfg.Project != nil || cfg.Effective.Project.ID != "" || len(cfg.Effective.ActiveProjects) != 0 || !hasDiagnostic(cfg.Diagnostics, NoProject) || hasDiagnostic(cfg.Diagnostics, RestartRequired) {
		t.Fatalf("%+v", cfg)
	}
	rt, err := c.Runtime(ctx)
	must(t, err)
	if !hasDiagnostic(rt.Diagnostics, NoProject) || len(rt.Effective.Profiles) != 7 || rt.Projects == nil || len(rt.Projects) != 0 {
		t.Fatalf("%+v", rt)
	}
	if s.active != nil {
		t.Fatal("idle service opened a trace")
	}
	_, err = c.RemoveProject(ctx, project)
	assertCode(t, err, NoProject)
	assertCode(t, c.Do(ctx, "PUT", Prefix+"/runtime/priority", PriorityRequest{Project: project, Workstreams: []config.WorkstreamID{}}, nil), Validation)
	assertCode(t, c.Do(ctx, "PUT", Prefix+"/runtime/pause", PauseRequest{Target: runtime.Target{Scope: "project", Project: project}, Mode: "soft", Source: "operator"}, nil), Validation)
	mutation(t, c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "factory"}, Mode: "soft", Source: "operator"})
	mutation(t, c, "PUT", "profile", ProfileRequest{"mason", "other"})
	must(t, s.Close())
	_, c = start(t, opts)
	after, err := c.Runtime(ctx)
	must(t, err)
	if len(after.Effective.Pauses) != 1 || after.Effective.Profiles["mason"] != "other" {
		t.Fatalf("%+v", after)
	}
}

func TestProjectAddActivatesAndRemoveRetains(t *testing.T) {
	t.Parallel()
	opts, clone := projectFixture(t)
	var bound []config.ProjectID
	opts.Threads = func(r *trace.Repository) (coreadapter.Reconciler, error) {
		bound = append(bound, r.Project())
		return &completedRunner{}, nil
	}
	s, c := start(t, opts)
	ctx := context.Background()
	root := opts.Config.Root
	must(t, os.MkdirAll(filepath.Join(clone, "internal", "trace"), 0700))
	must(t, os.WriteFile(filepath.Join(clone, "CODEOWNERS"), []byte("/internal/ @core\n"), 0600))
	must(t, os.WriteFile(filepath.Join(clone, "internal", "trace", "git.go"), []byte("package trace\n"), 0600))
	demoGit(t, filepath.Dir(clone), "-C", clone, "add", "internal", "CODEOWNERS")
	cloneBefore := snapshot(t, clone)
	added, err := c.AddProject(ctx, request(clone))
	must(t, err)
	id := added.Project.ID
	if err := config.CheckProjectIDs(id); err != nil {
		t.Fatal(err)
	}
	traceDir := filepath.Join(root, "projects", string(id))
	if added.Project.Trace != traceDir || added.Project.Charter != filepath.Join(traceDir, "charter.md") || !strings.Contains(added.NextStep, added.Project.Charter) || added.Project.Name != "dagger" || added.Project.BaseBranch != "main" {
		t.Fatalf("%+v", added)
	}
	text, err := os.ReadFile(filepath.Join(root, "config.toml"))
	must(t, err)
	want := strings.Replace(commentedConfig, "active_projects = []", `active_projects = ["`+string(id)+`"]`, 1)
	if string(text) != want {
		t.Fatalf("configuration text changed beyond the list:\n%s", text)
	}
	projectText, err := os.ReadFile(filepath.Join(traceDir, "config.toml"))
	must(t, err)
	if string(projectText) != "version = 1\nname = \"dagger\"\nupstream = \"dagger/dagger\"\nfork = \"owner/dagger\"\nclone = "+fmt.Sprintf("%q", added.Project.Clone)+"\nbase_branch = \"main\"\nlanding = \"commit-per-unit\"\n" {
		t.Fatalf("project configuration:\n%s", projectText)
	}
	charter, err := os.ReadFile(added.Project.Charter)
	must(t, err)
	if string(charter) != trace.CharterTemplate || !osmiacharter.Parse(trace.CharterTemplate).Empty() {
		t.Fatalf("charter: %q", charter)
	}
	if !reflect.DeepEqual(cloneBefore, snapshot(t, clone)) {
		t.Fatal("clone changed")
	}
	// The seeded entity map is the first revision of kb/entities.json.
	entities, err := os.ReadFile(filepath.Join(traceDir, trace.EntitiesPath))
	must(t, err)
	seeded, err := kb.Parse(entities)
	must(t, err)
	if e, ok := seeded.Lookup("internal.trace"); !ok || !reflect.DeepEqual(e.Owners, []string{"@core"}) || !reflect.DeepEqual(e.PartOf, []string{"internal"}) {
		t.Fatalf("seeded entities:\n%s", entities)
	}
	documents := documentRevisions(t, s, trace.EntitiesDocument)
	if len(documents) != 1 || documents[0].Revision != 1 || documents[0].Content != string(entities) {
		t.Fatalf("documents: %+v", documents)
	}
	if _, err := os.Lstat(filepath.Join(root, "project-add.json")); !os.IsNotExist(err) {
		t.Fatal("journal retained")
	}
	cfg, err := c.Configuration(ctx)
	must(t, err)
	if cfg.Project == nil || cfg.Project.ID != id || cfg.Effective.Project.ID != id || len(cfg.Diagnostics) != 0 {
		t.Fatalf("%+v", cfg)
	}
	if len(bound) != 1 || bound[0] != id || s.active == nil || s.active.repository.Project() != id {
		t.Fatalf("activation: bound=%v active=%v", bound, s.active)
	}
	// Registration starts the librarian's extraction; without a runner it
	// fails, is reported, and leaves the project usable.
	if x := awaitExtraction(t, c); x.Extraction != 1 || x.State != "failed" || !strings.Contains(x.Reason, "no agent runner") {
		t.Fatalf("extraction without a runner: %+v", x)
	}
	// The runtime store resolves against the new project without a restart.
	mutation(t, c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "project", Project: id}, Mode: "soft", Source: "operator"})
	mutation(t, c, "PUT", "priority", PriorityRequest{Project: id, Workstreams: []config.WorkstreamID{}})
	rt, err := c.Runtime(ctx)
	must(t, err)
	if len(rt.Effective.Pauses) != 1 || len(rt.Effective.Priorities) != 1 || len(rt.Diagnostics) != 0 || !reflect.DeepEqual(rt.Projects, []ProjectRuntime{{id, bundle.ModeFile}}) {
		t.Fatalf("%+v", rt)
	}
	// Repeating the same registration returns the project; another is refused.
	again, err := c.AddProject(ctx, request(clone))
	must(t, err)
	if again.Project.ID != id {
		t.Fatal(again)
	}
	other := request(clone)
	other.Upstream = "other/repo"
	_, err = c.AddProject(ctx, other)
	assertCode(t, err, ProjectActive)
	if !strings.Contains(err.Error(), string(id)) {
		t.Fatal(err)
	}
	if names := projectDirectories(t, root); len(names) != 1 {
		t.Fatal(names)
	}
	_, err = c.RemoveProject(ctx, project)
	assertCode(t, err, Validation)
	if !strings.Contains(err.Error(), string(id)) || !strings.Contains(err.Error(), string(project)) {
		t.Fatal(err)
	}
	traceBefore := snapshot(t, traceDir)
	removed, err := c.RemoveProject(ctx, id)
	must(t, err)
	if removed.Project.ID != id || removed.Project.Trace != traceDir || !strings.Contains(removed.NextStep, traceDir) {
		t.Fatalf("%+v", removed)
	}
	text, err = os.ReadFile(filepath.Join(root, "config.toml"))
	must(t, err)
	if string(text) != commentedConfig {
		t.Fatalf("configuration text after remove:\n%s", text)
	}
	if !reflect.DeepEqual(traceBefore, snapshot(t, traceDir)) || !reflect.DeepEqual(cloneBefore, snapshot(t, clone)) {
		t.Fatal("remove changed the trace or the clone")
	}
	cfg, err = c.Configuration(ctx)
	must(t, err)
	if cfg.Project != nil || cfg.Effective.HasProject() || !hasDiagnostic(cfg.Diagnostics, NoProject) || hasDiagnostic(cfg.Diagnostics, RestartRequired) || s.active != nil {
		t.Fatalf("%+v", cfg)
	}
	rt, err = c.Runtime(ctx)
	must(t, err)
	if len(rt.Effective.Pauses) != 0 || len(rt.Effective.Priorities) != 0 || len(rt.Diagnostics) != 3 || len(rt.Projects) != 0 {
		t.Fatalf("%+v", rt)
	}
	// The released trace can be opened by another owner.
	resolved, err := config.ResolveRoot(root, "")
	must(t, err)
	repository, err := trace.Open(resolved, config.Project{ID: id, Clone: clone})
	must(t, err)
	must(t, repository.Close())
	// Adding the same upstream again is a fresh start under a new identity.
	readded, err := c.AddProject(ctx, request(clone))
	must(t, err)
	if readded.Project.ID == id || readded.Project.Trace == traceDir {
		t.Fatalf("identity reused: %+v", readded)
	}
	if !reflect.DeepEqual(traceBefore, snapshot(t, traceDir)) {
		t.Fatal("archived trace changed")
	}
	if names := projectDirectories(t, root); len(names) != 2 || len(bound) != 2 || bound[1] != readded.Project.ID {
		t.Fatalf("directories=%v bound=%v", names, bound)
	}
	if got := activeProjects(t, root); !reflect.DeepEqual(got, []string{string(readded.Project.ID)}) {
		t.Fatal(got)
	}
	must(t, s.Close())
	s, c = start(t, opts)
	cfg, err = c.Configuration(ctx)
	must(t, err)
	if cfg.Project == nil || cfg.Project.ID != readded.Project.ID || s.active == nil {
		t.Fatalf("%+v", cfg)
	}
}

func TestProjectAddValidation(t *testing.T) {
	t.Parallel()
	opts, clone := projectFixture(t)
	_, c := start(t, opts)
	ctx := context.Background()
	root := opts.Config.Root
	home := filepath.Dir(root)
	plain := filepath.Join(home, "plain")
	must(t, os.Mkdir(plain, 0700))
	inside := filepath.Join(root, "inside")
	must(t, os.Mkdir(inside, 0700))
	must(t, os.Mkdir(filepath.Join(inside, ".git"), 0700))
	must(t, os.Mkdir(filepath.Join(home, ".git"), 0700))
	before := snapshot(t, clone)
	cases := []struct {
		name   string
		change func(*ProjectAddRequest)
		want   string
	}{
		{"name", func(r *ProjectAddRequest) { r.Name = " " }, "name is required"},
		{"upstream url", func(r *ProjectAddRequest) { r.Upstream = "https://github.com/dagger/dagger" }, "upstream must be owner/repository"},
		{"fork suffix", func(r *ProjectAddRequest) { r.Fork = "owner/dagger.git" }, "fork must be owner/repository"},
		{"same fork", func(r *ProjectAddRequest) { r.Fork = "DAGGER/dagger" }, "fork must differ"},
		{"branch", func(r *ProjectAddRequest) { r.BaseBranch = "bad..name" }, "base_branch"},
		{"relative clone", func(r *ProjectAddRequest) { r.Clone = "clone" }, "absolute path"},
		{"missing clone", func(r *ProjectAddRequest) { r.Clone = filepath.Join(home, "missing") }, "does not exist"},
		{"not a repository", func(r *ProjectAddRequest) { r.Clone = plain }, "not a Git repository"},
		{"clone in root", func(r *ProjectAddRequest) { r.Clone = inside }, "non-nested"},
		{"root in clone", func(r *ProjectAddRequest) { r.Clone = home }, "non-nested"},
	}
	seen := map[string]bool{}
	for _, tc := range cases {
		req := request(clone)
		tc.change(&req)
		_, err := c.AddProject(ctx, req)
		assertCode(t, err, Validation)
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: %v", tc.name, err)
		}
		seen[err.Error()] = true
	}
	if len(seen) != len(cases)-1 {
		t.Fatalf("messages are not distinct: %v", seen)
	}
	if !reflect.DeepEqual(before, snapshot(t, clone)) || projectDirectories(t, root) != nil {
		t.Fatal("rejected registration wrote files")
	}
	if _, err := os.Lstat(filepath.Join(root, "project-add.json")); !os.IsNotExist(err) {
		t.Fatal("rejected registration journaled")
	}
	cfg, err := c.Configuration(ctx)
	must(t, err)
	if cfg.Project != nil {
		t.Fatal("rejected registration activated")
	}
}

func TestProjectAddRecoversAtEachStep(t *testing.T) {
	t.Parallel()
	steps := []string{"journal-written", "project-config-written", "trace-created", "active-project-listed", "journal-removed"}
	for _, step := range steps {
		for _, mode := range []string{"restart", "retry"} {
			t.Run(step+"/"+mode, func(t *testing.T) {
				opts, clone := projectFixture(t)
				root := opts.Config.Root
				s, c := start(t, opts)
				ctx := context.Background()
				crash := errors.New("crash")
				s.boundary = func(name string) error {
					if name != step {
						return nil
					}
					if step == "project-config-written" {
						// Trace initialization interrupted before its first commit.
						for _, name := range projectDirectories(t, root) {
							must(t, os.MkdirAll(filepath.Join(root, "projects", name, ".git", "objects"), 0700))
							must(t, os.WriteFile(filepath.Join(root, "projects", name, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0600))
						}
					}
					return crash
				}
				_, err := c.AddProject(ctx, request(clone))
				assertCode(t, err, Internal)
				if !strings.Contains(err.Error(), "incomplete") {
					t.Fatal(err)
				}
				names := projectDirectories(t, root)
				if len(names) > 1 {
					t.Fatal(names)
				}
				var journal pendingProject
				data, jerr := os.ReadFile(filepath.Join(root, "project-add.json"))
				if step == "journal-removed" {
					if !os.IsNotExist(jerr) || len(names) != 1 {
						t.Fatalf("journal after final step: %v %v", jerr, names)
					}
					journal.Project.ID = config.ProjectID(names[0])
				} else {
					must(t, jerr)
					must(t, json.Unmarshal(data, &journal))
				}
				id := journal.Project.ID
				if len(names) == 1 && names[0] != string(id) {
					t.Fatalf("directory %v does not match journal %s", names, id)
				}
				s.boundary = nil
				if mode == "restart" {
					must(t, s.Close())
					s, c = start(t, opts)
				} else {
					result, err := c.AddProject(ctx, request(clone))
					must(t, err)
					if result.Project.ID != id {
						t.Fatalf("retry created %s instead of finishing %s", result.Project.ID, id)
					}
				}
				cfg, err := c.Configuration(ctx)
				must(t, err)
				if cfg.Project == nil || cfg.Project.ID != id || len(cfg.Diagnostics) != 0 || s.active == nil || s.active.repository.Project() != id {
					t.Fatalf("not completed: %+v", cfg)
				}
				if names := projectDirectories(t, root); !reflect.DeepEqual(names, []string{string(id)}) {
					t.Fatalf("traces: %v", names)
				}
				if got := activeProjects(t, root); !reflect.DeepEqual(got, []string{string(id)}) {
					t.Fatal(got)
				}
				if _, err := os.Lstat(filepath.Join(root, "project-add.json")); !os.IsNotExist(err) {
					t.Fatal("journal retained")
				}
				charter, err := os.ReadFile(cfg.Project.Charter)
				must(t, err)
				if string(charter) != trace.CharterTemplate {
					t.Fatalf("charter: %q", charter)
				}
				// Recovery repeats no commit: the creation, the librarian's
				// workstream, its chief-of-staff thread, the librarian thread,
				// the extraction request and its five reconciliation actions
				// (claim, observe, effect, result and acknowledgement) are each
				// recorded once.
				if x := awaitExtraction(t, c); x.State != "failed" {
					t.Fatalf("extraction: %+v", x)
				}
				if out := demoGit(t, filepath.Dir(root), "--git-dir="+filepath.Join(root, "projects", string(id), ".git"), "rev-list", "--count", "HEAD"); strings.TrimSpace(out) != "10" {
					t.Fatalf("trace history: %s", out)
				}
				// The seed is committed with the trace, so recovery never leaves a
				// trace without its first entity map revision.
				documents := documentRevisions(t, s, trace.EntitiesDocument)
				if len(documents) != 1 || documents[0].Revision != 1 {
					t.Fatalf("entity map revisions: %+v", documents)
				}
				must(t, s.Close())
				s, c = start(t, opts)
				cfg, err = c.Configuration(ctx)
				must(t, err)
				if cfg.Project == nil || cfg.Project.ID != id || len(cfg.Diagnostics) != 0 || s.active == nil {
					t.Fatalf("after restart: %+v", cfg)
				}
			})
		}
	}
}

func TestInterruptedAddWithDifferentRequestIsRefused(t *testing.T) {
	t.Parallel()
	opts, clone := projectFixture(t)
	s, c := start(t, opts)
	ctx := context.Background()
	s.boundary = func(name string) error {
		if name == "trace-created" {
			return errors.New("crash")
		}
		return nil
	}
	_, err := c.AddProject(ctx, request(clone))
	assertCode(t, err, Internal)
	s.boundary = nil
	other := request(clone)
	other.Name = "renamed"
	_, refused := c.AddProject(ctx, other)
	assertCode(t, refused, ProjectActive)
	cfg, err := c.Configuration(ctx)
	must(t, err)
	if cfg.Project == nil || !strings.Contains(refused.Error(), string(cfg.Project.ID)) || cfg.Project.Name != "dagger" {
		t.Fatalf("%v %+v", refused, cfg)
	}
	if names := projectDirectories(t, opts.Config.Root); len(names) != 1 {
		t.Fatal(names)
	}
}

func TestStartupRecoveryFailureIsDiagnosed(t *testing.T) {
	t.Parallel()
	opts, clone := projectFixture(t)
	root := opts.Config.Root
	s, c := start(t, opts)
	ctx := context.Background()
	s.boundary = func(name string) error {
		if name == "journal-written" {
			return errors.New("crash")
		}
		return nil
	}
	_, err := c.AddProject(ctx, request(clone))
	assertCode(t, err, Internal)
	must(t, s.Close())
	data, err := os.ReadFile(filepath.Join(root, "project-add.json"))
	must(t, err)
	var journal pendingProject
	must(t, json.Unmarshal(data, &journal))
	// A file where the projects directory belongs blocks every step.
	must(t, os.WriteFile(filepath.Join(root, "projects"), []byte("blocked"), 0600))
	s, c = start(t, opts)
	cfg, err := c.Configuration(ctx)
	must(t, err)
	if cfg.Project != nil || !hasDiagnostic(cfg.Diagnostics, NoProject) || !hasDiagnostic(cfg.Diagnostics, Internal) {
		t.Fatalf("%+v", cfg)
	}
	_, err = c.AddProject(ctx, request(clone))
	assertCode(t, err, Internal)
	if !strings.Contains(err.Error(), string(journal.Project.ID)) {
		t.Fatal(err)
	}
	must(t, os.Remove(filepath.Join(root, "projects")))
	result, err := c.AddProject(ctx, request(clone))
	must(t, err)
	if result.Project.ID != journal.Project.ID || s.active == nil {
		t.Fatalf("%+v", result)
	}
	cfg, err = c.Configuration(ctx)
	must(t, err)
	if len(cfg.Diagnostics) != 0 {
		t.Fatalf("%+v", cfg)
	}
}

func TestJournalForAnotherProjectIsRefused(t *testing.T) {
	t.Parallel()
	opts, clone := projectFixture(t)
	root := opts.Config.Root
	s, c := start(t, opts)
	ctx := context.Background()
	added, err := c.AddProject(ctx, request(clone))
	must(t, err)
	active := added.Project.ID
	// A journal left by another registration names a project that is not active.
	stale := config.Project{ID: "p_ffffffffffffffffffffffffffffffff", Version: 1, Name: "dagger", Upstream: "dagger/dagger", Fork: "owner/dagger", Clone: added.Project.Clone, BaseBranch: "main", Landing: "commit-per-unit"}
	data, err := json.Marshal(pendingProject{Version: 1, Project: stale})
	must(t, err)
	must(t, os.WriteFile(filepath.Join(root, "project-add.json"), data, 0600))
	before, err := os.ReadFile(filepath.Join(root, "config.toml"))
	must(t, err)
	_, err = c.AddProject(ctx, request(clone))
	assertCode(t, err, Internal)
	if !strings.Contains(err.Error(), string(stale.ID)) || !strings.Contains(err.Error(), string(active)) {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(root, "config.toml"))
	must(t, err)
	if string(after) != string(before) || !reflect.DeepEqual(activeProjects(t, root), []string{string(active)}) {
		t.Fatalf("configuration changed:\n%s", after)
	}
	if names := projectDirectories(t, root); !reflect.DeepEqual(names, []string{string(active)}) {
		t.Fatal(names)
	}
	// Startup reports the mismatch and keeps the active project running.
	must(t, s.Close())
	s, c = start(t, opts)
	cfg, err := c.Configuration(ctx)
	must(t, err)
	if cfg.Project == nil || cfg.Project.ID != active || !hasDiagnostic(cfg.Diagnostics, Internal) || s.active == nil {
		t.Fatalf("%+v", cfg)
	}
	if !reflect.DeepEqual(activeProjects(t, root), []string{string(active)}) {
		t.Fatal("startup listed the journaled project")
	}
	// Removing the active project lets the journaled registration finish.
	_, err = c.RemoveProject(ctx, active)
	must(t, err)
	finished, err := c.AddProject(ctx, request(clone))
	must(t, err)
	if finished.Project.ID != stale.ID || !reflect.DeepEqual(activeProjects(t, root), []string{string(stale.ID)}) {
		t.Fatalf("%+v", finished)
	}
}

func TestStartupRecoveryReloadFailureIsDiagnosed(t *testing.T) {
	t.Parallel()
	opts, clone := projectFixture(t)
	root := opts.Config.Root
	s, c := start(t, opts)
	ctx := context.Background()
	s.boundary = func(name string) error {
		if name == "trace-created" {
			return errors.New("crash")
		}
		return nil
	}
	_, err := c.AddProject(ctx, request(clone))
	assertCode(t, err, Internal)
	must(t, s.Close())
	// The registration finishes, but the project no longer loads: its clone is a file.
	must(t, os.RemoveAll(clone))
	must(t, os.WriteFile(clone, []byte("not a directory"), 0600))
	s, c = start(t, opts)
	cfg, err := c.Configuration(ctx)
	must(t, err)
	if cfg.Project != nil || !hasDiagnostic(cfg.Diagnostics, NoProject) || !hasDiagnostic(cfg.Diagnostics, Internal) || s.active != nil {
		t.Fatalf("%+v", cfg)
	}
	if names := projectDirectories(t, root); len(names) != 1 {
		t.Fatal(names)
	}
	if _, err := config.Load(config.Options{Root: root}); err == nil {
		t.Fatal("project configuration loaded with a file as clone")
	}
	if _, err := c.Health(ctx); err != nil {
		t.Fatal(err)
	}
}
