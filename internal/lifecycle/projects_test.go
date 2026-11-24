package lifecycle_test

import (
	context "context"
	json "encoding/json"
	errors "errors"
	fmt "fmt"
	os "os"
	filepath "path/filepath"
	reflect "reflect"
	strings "strings"
	testing "testing"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/api"
	bundle "github.com/kpenfound/osmia/internal/bundle"
	osmiacharter "github.com/kpenfound/osmia/internal/charter"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	kb "github.com/kpenfound/osmia/internal/kb"
	projectstore "github.com/kpenfound/osmia/internal/projectstore"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func projectDirectories(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "projects"))
	if os.IsNotExist(err) {
		return nil
	}
	fixturekit.Must(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func activeProjects(t *testing.T, root string) []string {
	t.Helper()
	resolved, err := config.ResolveRoot(root, "")
	fixturekit.Must(t, err)
	ids, err := projectstore.Open(resolved).Projects()
	fixturekit.Must(t, err)
	out := []string{}
	for _, id := range ids {
		out = append(out, string(id))
	}
	return out
}

func loadRegistered(t *testing.T, options config.Options) *config.Config {
	t.Helper()
	top, err := config.LoadTopLevel(options)
	fixturekit.Must(t, err)
	cfg, err := projectstore.Open(top.Root).Apply(top, options.Home)
	fixturekit.Must(t, err)
	return cfg
}

func storedSettings(t *testing.T, root string, id config.ProjectID) string {
	t.Helper()
	resolved, err := config.ResolveRoot(root, "")
	fixturekit.Must(t, err)
	revision, err := projectstore.Open(resolved).Current(id)
	fixturekit.Must(t, err)
	return string(revision.Data)
}

func TestZeroProjectStart(t *testing.T) {
	t.Parallel()
	opts, _ := journey.ProjectFixture(t)
	s, c := journey.StartT(t, opts)
	ctx := context.Background()
	cfg, err := c.Configuration(ctx)
	fixturekit.Must(t, err)
	if cfg.Project != nil || cfg.Effective.Project.ID != "" || len(cfg.Effective.ActiveProjects) != 0 || !intakefixture.HasDiagnostic(cfg.Diagnostics, api.NoProject) || intakefixture.HasDiagnostic(cfg.Diagnostics, api.RestartRequired) {
		t.Fatalf("%+v", cfg)
	}
	rt, err := c.Runtime(ctx)
	fixturekit.Must(t, err)
	if !intakefixture.HasDiagnostic(rt.Diagnostics, api.NoProject) || len(rt.Effective.Profiles) != 6 || rt.Projects == nil || len(rt.Projects) != 0 {
		t.Fatalf("%+v", rt)
	}
	if s.Sole() != nil {
		t.Fatal("idle service opened a trace")
	}
	_, err = c.RemoveProject(ctx, fixturekit.Project)
	fixturekit.AssertCode(t, err, api.NoProject)
	fixturekit.AssertCode(t, c.Do(ctx, "PUT", api.Prefix+"/runtime/priority", api.PriorityRequest{Project: fixturekit.Project, Workstreams: []config.WorkstreamID{}}, nil), api.Validation)
	fixturekit.AssertCode(t, c.Do(ctx, "PUT", api.Prefix+"/runtime/pause", api.PauseRequest{Target: runtime.Target{Scope: "project", Project: fixturekit.Project}, Mode: "soft", Source: "owner"}, nil), api.Validation)
	intakefixture.Mutation(t, c, "PUT", "pause", api.PauseRequest{Target: runtime.Target{Scope: "factory"}, Mode: "soft", Source: "owner"})
	intakefixture.Mutation(t, c, "PUT", "profile", api.ProfileRequest{Role: "mason", Profile: "other"})
	fixturekit.Must(t, s.Close())
	_, c = journey.StartT(t, opts)
	after, err := c.Runtime(ctx)
	fixturekit.Must(t, err)
	if len(after.Effective.Pauses) != 1 || after.Effective.Profiles["mason"] != "other" {
		t.Fatalf("%+v", after)
	}
}

type addedProject struct {
	s        *journey.Service
	c        *journey.Client
	opts     journey.Options
	clone    string
	root     string
	traceDir string
	id       config.ProjectID
	added    api.ProjectResponse
	bound    *[]config.ProjectID

	cloneBefore map[string]string
}

func addProjectToFreshRoot(t *testing.T) *addedProject {
	t.Helper()
	opts, clone := journey.ProjectFixture(t)
	bound := &[]config.ProjectID{}
	opts.Threads = func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
		*bound = append(*bound, r.Project())
		return &fixturekit.CompletedRunner{}, nil
	}
	s, c := journey.StartT(t, opts)
	fixturekit.Must(t, os.MkdirAll(filepath.Join(clone, "internal", "trace"), 0700))
	fixturekit.Must(t, os.WriteFile(filepath.Join(clone, "CODEOWNERS"), []byte("/internal/ @core\n"), 0600))
	fixturekit.Must(t, os.WriteFile(filepath.Join(clone, "internal", "trace", "git.go"), []byte("package trace\n"), 0600))
	fixturekit.DemoGit(t, filepath.Dir(clone), "-C", clone, "add", "internal", "CODEOWNERS")
	cloneBefore := intakefixture.Snapshot(t, clone)
	added, err := c.AddProject(context.Background(), fixturekit.Request(clone))
	fixturekit.Must(t, err)
	root := opts.Config.Root
	return &addedProject{s: s, c: c, opts: opts, clone: clone, root: root, traceDir: filepath.Join(root, "projects", string(added.Project.ID)), id: added.Project.ID, added: added, bound: bound, cloneBefore: cloneBefore}
}

func TestProjectAddRegistersTheProjectInTheRegistryAndStore(t *testing.T) {
	t.Parallel()
	p := addProjectToFreshRoot(t)
	if err := config.CheckProjectIDs(p.id); err != nil {
		t.Fatal(err)
	}
	if p.added.Project.Trace != p.traceDir || p.added.NextStep != "Write the charter with: osmia edit charter "+string(p.id) || p.added.Project.Name != "dagger" || p.added.Project.BaseBranch != "main" {
		t.Fatalf("%+v", p.added)
	}
	text, err := os.ReadFile(filepath.Join(p.root, "config.toml"))
	fixturekit.Must(t, err)
	if string(text) != fixturekit.CommentedConfig {
		t.Fatalf("registration wrote the top-level file:\n%s", text)
	}
	if got := activeProjects(t, p.root); !reflect.DeepEqual(got, []string{string(p.id)}) {
		t.Fatalf("registry %v", got)
	}
	if _, err := os.Lstat(filepath.Join(p.traceDir, "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("registration wrote a project file: %v", err)
	}
	projectText := storedSettings(t, p.root, p.id)
	if string(projectText) != "version = 1\nname = \"dagger\"\nupstream = \"dagger/dagger\"\nfork = \"owner/dagger\"\nclone = "+fmt.Sprintf("%q", p.added.Project.Clone)+"\nbase_branch = \"main\"\nlanding = \"commit-per-unit\"\ndelivery = \"pull-request\"\n" {
		t.Fatalf("project configuration:\n%s", projectText)
	}
	if _, err := os.Lstat(filepath.Join(p.root, "project-add.json")); !os.IsNotExist(err) {
		t.Fatal("journal retained")
	}
}

func TestProjectAddSeedsTheCharterAndEntitiesWithoutTouchingTheClone(t *testing.T) {
	t.Parallel()
	p := addProjectToFreshRoot(t)
	s, id := p.s, p.id
	charter, err := fixturekit.ProjectDocument(t, s.Cfg.Root, id, "charter.md")
	fixturekit.Must(t, err)
	if string(charter) != trace.CharterTemplate || osmiacharter.Guidance(trace.CharterTemplate) != "" {
		t.Fatalf("charter: %q", charter)
	}
	if !reflect.DeepEqual(p.cloneBefore, intakefixture.Snapshot(t, p.clone)) {
		t.Fatal("clone changed")
	}
	entities, err := fixturekit.ProjectDocument(t, s.Cfg.Root, id, trace.EntitiesPath)
	fixturekit.Must(t, err)
	seeded, err := kb.Parse(entities)
	fixturekit.Must(t, err)
	if e, ok := seeded.Lookup("internal.trace"); !ok || !reflect.DeepEqual(e.Owners, []string{"@core"}) || !reflect.DeepEqual(e.PartOf, []string{"internal"}) {
		t.Fatalf("seeded entities:\n%s", entities)
	}
	documents := journey.DocumentRevisions(t, s, trace.EntitiesDocument)
	if len(documents) != 1 || documents[0].Revision != 1 || documents[0].Content != string(entities) {
		t.Fatalf("documents: %+v", documents)
	}
}

func TestProjectAddActivatesTheProjectWithoutRestart(t *testing.T) {
	t.Parallel()
	p := addProjectToFreshRoot(t)
	cfg, err := p.c.Configuration(context.Background())
	fixturekit.Must(t, err)
	if cfg.Project == nil || cfg.Project.ID != p.id || cfg.Effective.Project.ID != p.id || len(cfg.Diagnostics) != 0 {
		t.Fatalf("%+v", cfg)
	}
	if len(*p.bound) != 1 || (*p.bound)[0] != p.id || p.s.Sole() == nil || p.s.Sole().Repository.Project() != p.id {
		t.Fatalf("activation: bound=%v active=%v", *p.bound, p.s.Sole())
	}
	if streams, err := p.s.Sole().Repository.Workstreams(); err != nil || len(streams) != 0 {
		t.Fatalf("workstreams after registration: %v %v", streams, err)
	}
}

func TestProjectAddedAtRuntimeAcceptsPausesAndPriorities(t *testing.T) {
	t.Parallel()
	p := addProjectToFreshRoot(t)
	intakefixture.Mutation(t, p.c, "PUT", "pause", api.PauseRequest{Target: runtime.Target{Scope: "project", Project: p.id}, Mode: "soft", Source: "owner"})
	intakefixture.Mutation(t, p.c, "PUT", "priority", api.PriorityRequest{Project: p.id, Workstreams: []config.WorkstreamID{}})
	rt, err := p.c.Runtime(context.Background())
	fixturekit.Must(t, err)
	if len(rt.Effective.Pauses) != 1 || len(rt.Effective.Priorities) != 1 || len(rt.Diagnostics) != 0 || !reflect.DeepEqual(rt.Projects, []api.ProjectRuntime{{Project: p.id, ContextMode: bundle.ModeFile}}) {
		t.Fatalf("%+v", rt)
	}
}

func TestProjectAddIsIdempotentAndRefusesADifferentProjectWhileActive(t *testing.T) {
	t.Parallel()
	p := addProjectToFreshRoot(t)
	ctx := context.Background()
	again, err := p.c.AddProject(ctx, fixturekit.Request(p.clone))
	fixturekit.Must(t, err)
	if again.Project.ID != p.id {
		t.Fatal(again)
	}
	other := fixturekit.Request(p.clone)
	other.Upstream = "other/repo"
	_, err = p.c.AddProject(ctx, other)
	fixturekit.AssertCode(t, err, api.ProjectActive)
	if !strings.Contains(err.Error(), string(p.id)) {
		t.Fatal(err)
	}
	if names := projectDirectories(t, p.root); len(names) != 1 {
		t.Fatal(names)
	}
}

func TestProjectRemoveRefusesAnIdThatIsNotTheActiveProject(t *testing.T) {
	t.Parallel()
	p := addProjectToFreshRoot(t)
	_, err := p.c.RemoveProject(context.Background(), fixturekit.Project)
	fixturekit.AssertCode(t, err, api.Validation)
	if !strings.Contains(err.Error(), string(p.id)) || !strings.Contains(err.Error(), string(fixturekit.Project)) {
		t.Fatal(err)
	}
}

func TestProjectRemoveRetainsTheTraceAndTheCloneAndClearsRuntimeState(t *testing.T) {
	t.Parallel()
	p := addProjectToFreshRoot(t)
	ctx := context.Background()
	intakefixture.Mutation(t, p.c, "PUT", "pause", api.PauseRequest{Target: runtime.Target{Scope: "project", Project: p.id}, Mode: "soft", Source: "owner"})
	intakefixture.Mutation(t, p.c, "PUT", "priority", api.PriorityRequest{Project: p.id, Workstreams: []config.WorkstreamID{}})
	traceBefore := traceState(t, p.traceDir)
	removed, err := p.c.RemoveProject(ctx, p.id)
	fixturekit.Must(t, err)
	if removed.Project.ID != p.id || removed.Project.Trace != p.traceDir || !strings.Contains(removed.NextStep, p.traceDir) {
		t.Fatalf("%+v", removed)
	}
	text, err := os.ReadFile(filepath.Join(p.root, "config.toml"))
	fixturekit.Must(t, err)
	if string(text) != fixturekit.CommentedConfig {
		t.Fatalf("configuration text after remove:\n%s", text)
	}
	if traceBefore != traceState(t, p.traceDir) || !reflect.DeepEqual(p.cloneBefore, intakefixture.Snapshot(t, p.clone)) {
		t.Fatal("remove changed the trace or the clone")
	}
	cfg, err := p.c.Configuration(ctx)
	fixturekit.Must(t, err)
	if cfg.Project != nil || cfg.Effective.HasProject() || !intakefixture.HasDiagnostic(cfg.Diagnostics, api.NoProject) || intakefixture.HasDiagnostic(cfg.Diagnostics, api.RestartRequired) || p.s.Sole() != nil {
		t.Fatalf("%+v", cfg)
	}
	rt, err := p.c.Runtime(ctx)
	fixturekit.Must(t, err)
	if len(rt.Effective.Pauses) != 0 || len(rt.Effective.Priorities) != 0 || len(rt.Diagnostics) != 3 || len(rt.Projects) != 0 {
		t.Fatalf("%+v", rt)
	}
}

func TestProjectAddedAgainAfterRemoveGetsANewIdentityAndSurvivesARestart(t *testing.T) {
	t.Parallel()
	p := addProjectToFreshRoot(t)
	ctx := context.Background()
	traceBefore := traceState(t, p.traceDir)
	_, err := p.c.RemoveProject(ctx, p.id)
	fixturekit.Must(t, err)
	resolved, err := config.ResolveRoot(p.root, "")
	fixturekit.Must(t, err)
	repository, err := trace.Open(resolved, config.Project{ID: p.id, Clone: p.clone})
	fixturekit.Must(t, err)
	fixturekit.Must(t, repository.Close())
	readded, err := p.c.AddProject(ctx, fixturekit.Request(p.clone))
	fixturekit.Must(t, err)
	if readded.Project.ID == p.id || readded.Project.Trace == p.traceDir {
		t.Fatalf("identity reused: %+v", readded)
	}
	if traceBefore != traceState(t, p.traceDir) {
		t.Fatal("archived trace changed")
	}
	if names := projectDirectories(t, p.root); len(names) != 2 || len(*p.bound) != 2 || (*p.bound)[1] != readded.Project.ID {
		t.Fatalf("directories=%v bound=%v", names, *p.bound)
	}
	if got := activeProjects(t, p.root); !reflect.DeepEqual(got, []string{string(readded.Project.ID)}) {
		t.Fatal(got)
	}
	fixturekit.Must(t, p.s.Close())
	s, c := journey.StartT(t, p.opts)
	cfg, err := c.Configuration(ctx)
	fixturekit.Must(t, err)
	if cfg.Project == nil || cfg.Project.ID != readded.Project.ID || s.Sole() == nil {
		t.Fatalf("%+v", cfg)
	}
}

func TestProjectAddValidation(t *testing.T) {
	t.Parallel()
	opts, clone := journey.ProjectFixture(t)
	_, c := journey.StartT(t, opts)
	ctx := context.Background()
	root := opts.Config.Root
	home := filepath.Dir(root)
	plain := filepath.Join(home, "plain")
	fixturekit.Must(t, os.Mkdir(plain, 0700))
	inside := filepath.Join(root, "inside")
	fixturekit.Must(t, os.Mkdir(inside, 0700))
	fixturekit.Must(t, os.Mkdir(filepath.Join(inside, ".git"), 0700))
	fixturekit.Must(t, os.Mkdir(filepath.Join(home, ".git"), 0700))
	before := intakefixture.Snapshot(t, clone)
	cases := []struct {
		name   string
		change func(*api.ProjectAddRequest)
		want   string
	}{
		{"name", func(r *api.ProjectAddRequest) { r.Name = " " }, "name is required"},
		{"upstream url", func(r *api.ProjectAddRequest) { r.Upstream = "https://github.com/dagger/dagger" }, "upstream must be owner/repository"},
		{"fork suffix", func(r *api.ProjectAddRequest) { r.Fork = "owner/dagger.git" }, "fork must be owner/repository"},
		{"branch", func(r *api.ProjectAddRequest) { r.BaseBranch = "bad..name" }, "base_branch"},
		{"relative clone", func(r *api.ProjectAddRequest) { r.Clone = "clone" }, "absolute path"},
		{"missing clone", func(r *api.ProjectAddRequest) { r.Clone = filepath.Join(home, "missing") }, "does not exist"},
		{"not a repository", func(r *api.ProjectAddRequest) { r.Clone = plain }, "not a Git repository"},
		{"clone in root", func(r *api.ProjectAddRequest) { r.Clone = inside }, "non-nested"},
		{"root in clone", func(r *api.ProjectAddRequest) { r.Clone = home }, "non-nested"},
	}
	seen := map[string]bool{}
	for _, tc := range cases {
		req := fixturekit.Request(clone)
		tc.change(&req)
		_, err := c.AddProject(ctx, req)
		fixturekit.AssertCode(t, err, api.Validation)
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: %v", tc.name, err)
		}
		seen[err.Error()] = true
	}
	if len(seen) != len(cases)-1 {
		t.Fatalf("messages are not distinct: %v", seen)
	}
	if !reflect.DeepEqual(before, intakefixture.Snapshot(t, clone)) || projectDirectories(t, root) != nil {
		t.Fatal("rejected registration wrote files")
	}
	if _, err := os.Lstat(filepath.Join(root, "project-add.json")); !os.IsNotExist(err) {
		t.Fatal("rejected registration journaled")
	}
	cfg, err := c.Configuration(ctx)
	fixturekit.Must(t, err)
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
				opts, clone := journey.ProjectFixture(t)
				root := opts.Config.Root
				s, c := journey.StartT(t, opts)
				ctx := context.Background()
				crash := errors.New("crash")
				s.Boundary = func(name string) error {
					if name != step {
						return nil
					}
					if step == "project-config-written" {
						for _, name := range projectDirectories(t, root) {
							fixturekit.TraceExec(t, filepath.Join(root, "projects", name), "CREATE TABLE files (path TEXT PRIMARY KEY, content BLOB NOT NULL)")
						}
					}
					return crash
				}
				_, err := c.AddProject(ctx, fixturekit.Request(clone))
				fixturekit.AssertCode(t, err, api.Internal)
				if !strings.Contains(err.Error(), "incomplete") {
					t.Fatal(err)
				}
				names := projectDirectories(t, root)
				if len(names) > 1 {
					t.Fatal(names)
				}
				var journal service.PendingProject
				data, jerr := os.ReadFile(filepath.Join(root, "project-add.json"))
				if step == "journal-removed" {
					if !os.IsNotExist(jerr) || len(names) != 1 {
						t.Fatalf("journal after final step: %v %v", jerr, names)
					}
					journal.Project.ID = config.ProjectID(names[0])
				} else {
					fixturekit.Must(t, jerr)
					fixturekit.Must(t, json.Unmarshal(data, &journal))
				}
				id := journal.Project.ID
				if len(names) == 1 && names[0] != string(id) {
					t.Fatalf("directory %v does not match journal %s", names, id)
				}
				s.Boundary = nil
				if mode == "restart" {
					fixturekit.Must(t, s.Close())
					s, c = journey.StartT(t, opts)
				} else {
					result, err := c.AddProject(ctx, fixturekit.Request(clone))
					fixturekit.Must(t, err)
					if result.Project.ID != id {
						t.Fatalf("retry created %s instead of finishing %s", result.Project.ID, id)
					}
				}
				cfg, err := c.Configuration(ctx)
				fixturekit.Must(t, err)
				if cfg.Project == nil || cfg.Project.ID != id || len(cfg.Diagnostics) != 0 || s.Sole() == nil || s.Sole().Repository.Project() != id {
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
				charter, err := fixturekit.ProjectDocument(t, s.Cfg.Root, id, "charter.md")
				fixturekit.Must(t, err)
				if string(charter) != trace.CharterTemplate {
					t.Fatalf("charter: %q", charter)
				}
				if n := fixturekit.TraceCommits(t, filepath.Join(root, "projects", string(id))); n != 1 {
					t.Fatalf("project trace history: %d commits", n)
				}
				if streams, err := s.Sole().Repository.Workstreams(); err != nil || len(streams) != 0 {
					t.Fatalf("workstreams after recovery: %v %v", streams, err)
				}
				documents := journey.DocumentRevisions(t, s, trace.EntitiesDocument)
				if len(documents) != 1 || documents[0].Revision != 1 {
					t.Fatalf("entity map revisions: %+v", documents)
				}
				fixturekit.Must(t, s.Close())
				s, c = journey.StartT(t, opts)
				cfg, err = c.Configuration(ctx)
				fixturekit.Must(t, err)
				if cfg.Project == nil || cfg.Project.ID != id || len(cfg.Diagnostics) != 0 || s.Sole() == nil {
					t.Fatalf("after restart: %+v", cfg)
				}
			})
		}
	}
}

func TestInterruptedAddWithDifferentRequestIsRefused(t *testing.T) {
	t.Parallel()
	opts, clone := journey.ProjectFixture(t)
	s, c := journey.StartT(t, opts)
	ctx := context.Background()
	s.Boundary = func(name string) error {
		if name == "trace-created" {
			return errors.New("crash")
		}
		return nil
	}
	_, err := c.AddProject(ctx, fixturekit.Request(clone))
	fixturekit.AssertCode(t, err, api.Internal)
	s.Boundary = nil
	other := fixturekit.Request(clone)
	other.Name = "renamed"
	_, refused := c.AddProject(ctx, other)
	fixturekit.AssertCode(t, refused, api.ProjectActive)
	cfg, err := c.Configuration(ctx)
	fixturekit.Must(t, err)
	if cfg.Project == nil || !strings.Contains(refused.Error(), string(cfg.Project.ID)) || cfg.Project.Name != "dagger" {
		t.Fatalf("%v %+v", refused, cfg)
	}
	if names := projectDirectories(t, opts.Config.Root); len(names) != 1 {
		t.Fatal(names)
	}
}

func TestStartupRecoveryFailureIsDiagnosed(t *testing.T) {
	t.Parallel()
	opts, clone := journey.ProjectFixture(t)
	root := opts.Config.Root
	s, c := journey.StartT(t, opts)
	ctx := context.Background()
	s.Boundary = func(name string) error {
		if name == "journal-written" {
			return errors.New("crash")
		}
		return nil
	}
	_, err := c.AddProject(ctx, fixturekit.Request(clone))
	fixturekit.AssertCode(t, err, api.Internal)
	fixturekit.Must(t, s.Close())
	data, err := os.ReadFile(filepath.Join(root, "project-add.json"))
	fixturekit.Must(t, err)
	var journal service.PendingProject
	fixturekit.Must(t, json.Unmarshal(data, &journal))
	fixturekit.Must(t, os.WriteFile(filepath.Join(root, "projects"), []byte("blocked"), 0600))
	s, c = journey.StartT(t, opts)
	cfg, err := c.Configuration(ctx)
	fixturekit.Must(t, err)
	if cfg.Project != nil || !intakefixture.HasDiagnostic(cfg.Diagnostics, api.NoProject) || !intakefixture.HasDiagnostic(cfg.Diagnostics, api.Internal) {
		t.Fatalf("%+v", cfg)
	}
	_, err = c.AddProject(ctx, fixturekit.Request(clone))
	fixturekit.AssertCode(t, err, api.Internal)
	if !strings.Contains(err.Error(), string(journal.Project.ID)) {
		t.Fatal(err)
	}
	fixturekit.Must(t, os.Remove(filepath.Join(root, "projects")))
	result, err := c.AddProject(ctx, fixturekit.Request(clone))
	fixturekit.Must(t, err)
	if result.Project.ID != journal.Project.ID || s.Sole() == nil {
		t.Fatalf("%+v", result)
	}
	cfg, err = c.Configuration(ctx)
	fixturekit.Must(t, err)
	if len(cfg.Diagnostics) != 0 {
		t.Fatalf("%+v", cfg)
	}
}

func TestJournalForAnotherProjectIsRefused(t *testing.T) {
	t.Parallel()
	opts, clone := journey.ProjectFixture(t)
	root := opts.Config.Root
	s, c := journey.StartT(t, opts)
	ctx := context.Background()
	added, err := c.AddProject(ctx, fixturekit.Request(clone))
	fixturekit.Must(t, err)
	active := added.Project.ID
	stale := config.Project{ID: "p_ffffffffffffffffffffffffffffffff", Version: 1, Name: "dagger", Upstream: "dagger/dagger", Fork: "owner/dagger", Clone: added.Project.Clone, BaseBranch: "main", Landing: "commit-per-unit"}
	data, err := json.Marshal(service.PendingProject{Version: 1, Project: stale})
	fixturekit.Must(t, err)
	fixturekit.Must(t, os.WriteFile(filepath.Join(root, "project-add.json"), data, 0600))
	before, err := os.ReadFile(filepath.Join(root, "config.toml"))
	fixturekit.Must(t, err)
	_, err = c.AddProject(ctx, fixturekit.Request(clone))
	fixturekit.AssertCode(t, err, api.Internal)
	if !strings.Contains(err.Error(), string(stale.ID)) || !strings.Contains(err.Error(), string(active)) {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(root, "config.toml"))
	fixturekit.Must(t, err)
	if string(after) != string(before) || !reflect.DeepEqual(activeProjects(t, root), []string{string(active)}) {
		t.Fatalf("configuration changed:\n%s", after)
	}
	if names := projectDirectories(t, root); !reflect.DeepEqual(names, []string{string(active)}) {
		t.Fatal(names)
	}
	fixturekit.Must(t, s.Close())
	s, c = journey.StartT(t, opts)
	cfg, err := c.Configuration(ctx)
	fixturekit.Must(t, err)
	if cfg.Project == nil || cfg.Project.ID != active || !intakefixture.HasDiagnostic(cfg.Diagnostics, api.Internal) || s.Sole() == nil {
		t.Fatalf("%+v", cfg)
	}
	if !reflect.DeepEqual(activeProjects(t, root), []string{string(active)}) {
		t.Fatal("startup listed the journaled project")
	}
	_, err = c.RemoveProject(ctx, active)
	fixturekit.Must(t, err)
	finished, err := c.AddProject(ctx, fixturekit.Request(clone))
	fixturekit.Must(t, err)
	if finished.Project.ID != stale.ID || !reflect.DeepEqual(activeProjects(t, root), []string{string(stale.ID)}) {
		t.Fatalf("%+v", finished)
	}
}

func TestStartupRecoveryReloadFailureIsDiagnosed(t *testing.T) {
	t.Parallel()
	opts, clone := journey.ProjectFixture(t)
	root := opts.Config.Root
	s, c := journey.StartT(t, opts)
	ctx := context.Background()
	s.Boundary = func(name string) error {
		if name == "trace-created" {
			return errors.New("crash")
		}
		return nil
	}
	_, err := c.AddProject(ctx, fixturekit.Request(clone))
	fixturekit.AssertCode(t, err, api.Internal)
	fixturekit.Must(t, s.Close())
	fixturekit.Must(t, os.RemoveAll(clone))
	fixturekit.Must(t, os.WriteFile(clone, []byte("not a directory"), 0600))
	s, c = journey.StartT(t, opts)
	cfg, err := c.Configuration(ctx)
	fixturekit.Must(t, err)
	if cfg.Project != nil || !intakefixture.HasDiagnostic(cfg.Diagnostics, api.NoProject) || !intakefixture.HasDiagnostic(cfg.Diagnostics, api.Internal) || s.Sole() != nil {
		t.Fatalf("%+v", cfg)
	}
	if names := projectDirectories(t, root); len(names) != 1 {
		t.Fatal(names)
	}
	top, err := config.LoadTopLevel(config.Options{Root: root})
	fixturekit.Must(t, err)
	if _, err := projectstore.Open(top.Root).Apply(top, ""); err == nil {
		t.Fatal("stored settings loaded with a file as clone")
	}
	if _, err := c.Health(ctx); err != nil {
		t.Fatal(err)
	}
}

func traceState(t *testing.T, directory string) string {
	t.Helper()
	return fmt.Sprintf("%d commits", fixturekit.TraceCommits(t, directory))
}
