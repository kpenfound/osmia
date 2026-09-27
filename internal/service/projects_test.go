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
workspaces = "git" # the tests run on Git worktrees whether or not jj is installed

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
	if s.sole() != nil {
		t.Fatal("idle service opened a trace")
	}
	_, err = c.RemoveProject(ctx, project)
	assertCode(t, err, NoProject)
	assertCode(t, c.Do(ctx, "PUT", Prefix+"/runtime/priority", PriorityRequest{Project: project, Workstreams: []config.WorkstreamID{}}, nil), Validation)
	assertCode(t, c.Do(ctx, "PUT", Prefix+"/runtime/pause", PauseRequest{Target: runtime.Target{Scope: "project", Project: project}, Mode: "soft", Source: "owner"}, nil), Validation)
	mutation(t, c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "factory"}, Mode: "soft", Source: "owner"})
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
	opts.Threads = func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
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
	if len(bound) != 1 || bound[0] != id || s.sole() == nil || s.sole().repository.Project() != id {
		t.Fatalf("activation: bound=%v active=%v", bound, s.sole())
	}
	// Registration starts the librarian's extraction; without a runner it
	// fails, is reported, and leaves the project usable.
	if x := awaitExtraction(t, c); x.Extraction != 1 || x.State != "failed" || !strings.Contains(x.Reason, "no agent runner") {
		t.Fatalf("extraction without a runner: %+v", x)
	}
	// The result and acknowledgement commits land after the terminal state,
	// so wait for both before snapshotting the trace below.
	awaitExtractionAcknowledged(t, s)
	// The runtime store resolves against the new project without a restart.
	mutation(t, c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "project", Project: id}, Mode: "soft", Source: "owner"})
	mutation(t, c, "PUT", "priority", PriorityRequest{Project: id, Workstreams: []config.WorkstreamID{}})
	rt, err := c.Runtime(ctx)
	must(t, err)
	if len(rt.Effective.Pauses) != 1 || len(rt.Effective.Priorities) != 1 || len(rt.Diagnostics) != 0 || !reflect.DeepEqual(rt.Projects, []ProjectRuntime{{id, bundle.ModeFile}}) {
		t.Fatalf("%+v", rt)
	}
	// Repeating the same registration returns the project.
	again, err := c.AddProject(ctx, request(clone))
	must(t, err)
	if again.Project.ID != id {
		t.Fatal(again)
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
	if removed.Project.ID != id || removed.Project.Trace != traceDir || !strings.Contains(removed.NextStep, traceDir) || !strings.Contains(removed.NextStep, "drains") || len(removed.Unfinished) != 0 {
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
	if cfg.Project != nil || cfg.Effective.HasProject() || !hasDiagnostic(cfg.Diagnostics, NoProject) || hasDiagnostic(cfg.Diagnostics, RestartRequired) || s.sole() != nil {
		t.Fatalf("%+v", cfg)
	}
	rt, err = c.Runtime(ctx)
	must(t, err)
	if len(rt.Effective.Pauses) != 0 || len(rt.Effective.Priorities) != 0 || len(rt.Diagnostics) != 3 || len(rt.Projects) != 0 {
		t.Fatalf("%+v", rt)
	}
	awaitDrained(t, s)
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
	if cfg.Project == nil || cfg.Project.ID != readded.Project.ID || s.sole() == nil {
		t.Fatalf("%+v", cfg)
	}
}

// A project added while another runs starts beside it: both are listed and
// active, both traces run their loops, and each registration repeated returns
// its own project.
func TestProjectAddStartsASecondProjectBesideTheFirst(t *testing.T) {
	t.Parallel()
	opts, clone := projectFixture(t)
	s, c := start(t, opts)
	ctx := context.Background()
	first, err := c.AddProject(ctx, request(clone))
	must(t, err)
	second := request(clone)
	second.Name, second.Upstream = "other", "other/repo"
	added, err := c.AddProject(ctx, second)
	must(t, err)
	ids := []config.ProjectID{first.Project.ID, added.Project.ID}
	if ids[0] == ids[1] || added.Project.Name != "other" || !strings.Contains(added.NextStep, added.Project.Charter) {
		t.Fatalf("second project: %+v", added)
	}
	if got := activeProjects(t, opts.Config.Root); !reflect.DeepEqual(got, []string{string(ids[0]), string(ids[1])}) {
		t.Fatalf("active projects %v", got)
	}
	cfg, err := c.Configuration(ctx)
	must(t, err)
	if cfg.Project != nil || len(cfg.Projects) != 2 || cfg.Projects[0].ID != ids[0] || cfg.Projects[1].ID != ids[1] {
		t.Fatalf("configuration: %+v", cfg)
	}
	var running []config.ProjectID
	for _, r := range s.traces() {
		running = append(running, r.Project())
	}
	if !reflect.DeepEqual(running, ids) {
		t.Fatalf("running %v", running)
	}
	// Each project's loop runs its own first extraction.
	for _, id := range ids {
		soon(t, "the extraction of project "+string(id), func() bool {
			x, err := s.projectExtraction(id)
			return err == nil && x != nil && x.State == "failed"
		})
	}
	for i, req := range []ProjectAddRequest{request(clone), second} {
		again, err := c.AddProject(ctx, req)
		must(t, err)
		if again.Project.ID != ids[i] {
			t.Fatalf("repeated registration %d returned %s", i, again.Project.ID)
		}
	}
	if names := projectDirectories(t, opts.Config.Root); len(names) != 2 {
		t.Fatal(names)
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
				if cfg.Project == nil || cfg.Project.ID != id || len(cfg.Diagnostics) != 0 || s.sole() == nil || s.sole().repository.Project() != id {
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
				awaitExtractionAcknowledged(t, s)
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
				if cfg.Project == nil || cfg.Project.ID != id || len(cfg.Diagnostics) != 0 || s.sole() == nil {
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
	assertCode(t, refused, Conflict)
	cfg, err := c.Configuration(ctx)
	must(t, err)
	if cfg.Project == nil || !strings.Contains(refused.Error(), "an interrupted registration was finished instead: project "+string(cfg.Project.ID)+" is now active") || cfg.Project.Name != "dagger" {
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
	if result.Project.ID != journal.Project.ID || s.sole() == nil {
		t.Fatalf("%+v", result)
	}
	cfg, err = c.Configuration(ctx)
	must(t, err)
	if len(cfg.Diagnostics) != 0 {
		t.Fatalf("%+v", cfg)
	}
}

// An interrupted registration finishes beside a project that is already
// active, whether a restart or a repeated add finishes it, and both projects
// then run.
func TestInterruptedAddFinishesBesideAnActiveProject(t *testing.T) {
	t.Parallel()
	for _, step := range []string{"journal-written", "trace-created", "active-project-listed"} {
		for _, mode := range []string{"restart", "retry"} {
			t.Run(step+"/"+mode, func(t *testing.T) {
				opts, clone := projectFixture(t)
				root := opts.Config.Root
				s, c := start(t, opts)
				ctx := context.Background()
				first, err := c.AddProject(ctx, request(clone))
				must(t, err)
				second := request(clone)
				second.Name, second.Upstream = "other", "other/repo"
				s.boundary = func(name string) error {
					if name == step {
						return errors.New("crash")
					}
					return nil
				}
				_, err = c.AddProject(ctx, second)
				assertCode(t, err, Internal)
				s.boundary = nil
				data, err := os.ReadFile(filepath.Join(root, "project-add.json"))
				must(t, err)
				var journal pendingProject
				must(t, json.Unmarshal(data, &journal))
				id := journal.Project.ID
				if mode == "restart" {
					must(t, s.Close())
					s, c = start(t, opts)
				} else {
					finished, err := c.AddProject(ctx, second)
					must(t, err)
					if finished.Project.ID != id {
						t.Fatalf("retry registered %s instead of finishing %s", finished.Project.ID, id)
					}
				}
				want := []string{string(first.Project.ID), string(id)}
				if got := activeProjects(t, root); !reflect.DeepEqual(got, want) {
					t.Fatalf("active projects %v, want %v", got, want)
				}
				if _, err := os.Lstat(filepath.Join(root, "project-add.json")); !os.IsNotExist(err) {
					t.Fatal("journal retained")
				}
				cfg, err := c.Configuration(ctx)
				must(t, err)
				if len(cfg.Projects) != 2 || cfg.Projects[0].ID != first.Project.ID || cfg.Projects[1].ID != id || cfg.Projects[1].Name != "other" || len(cfg.Diagnostics) != 0 {
					t.Fatalf("configuration: %+v", cfg)
				}
				var running []string
				for _, p := range s.traces() {
					running = append(running, string(p.Project()))
				}
				if !reflect.DeepEqual(running, want) {
					t.Fatalf("running projects %v, want %v", running, want)
				}
				// The first registration repeated still returns its project.
				again, err := c.AddProject(ctx, request(clone))
				must(t, err)
				if again.Project.ID != first.Project.ID {
					t.Fatalf("repeated registration: %+v", again)
				}
			})
		}
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
	if cfg.Project != nil || !hasDiagnostic(cfg.Diagnostics, NoProject) || !hasDiagnostic(cfg.Diagnostics, Internal) || s.sole() != nil {
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
