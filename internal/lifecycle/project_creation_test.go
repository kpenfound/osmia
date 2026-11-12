package lifecycle_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	wire "github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

const creationCharter = "# Charter\n\n## Rules\n\n1. Keep the repository small.\n"

// creationFixture is a service whose [projects] table is set, a fake GitHub
// whose created repositories are local bare repositories, and the directories
// they live in. The tests set GITHUB_TOKEN, so none of them runs in parallel.
type creationFixture struct {
	opts     Options
	root     string
	dir      string
	cloneDir string
	github   *servicetest.FakeGitHub
	remote   string
	readme   string
}

// newCreationFixture seeds the remote on defaultBranch, which is also the
// default branch the fake GitHub gives a created repository.
func newCreationFixture(t *testing.T, defaultBranch string, projects string) *creationFixture {
	t.Helper()
	t.Setenv("GITHUB_TOKEN", "fixture-token")
	opts, _ := projectFixture(t)
	dir, err := os.MkdirTemp("", "os-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := &creationFixture{opts: opts, root: opts.Config.Root, dir: dir, cloneDir: filepath.Join(dir, "clones")}
	f.github = servicetest.NewFakeGitHub(t, "Owner")
	f.github.DefaultBranch = defaultBranch
	f.remote, f.readme = servicetest.SeedRemote(t, dir, "newrepo", defaultBranch)
	f.github.Remote = func(string) string { return f.remote }
	f.opts.Repositories = f.github.Client()
	if projects == "" {
		projects = fmt.Sprintf("[projects]\nrepo_owner = \"owner\"\nclone_dir = %q\n", f.cloneDir)
	}
	path := filepath.Join(f.root, "config.toml")
	text, err := os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(path, append(text, []byte("\n"+projects)...), 0600))
	return f
}

func (f *creationFixture) request() wire.ProjectCreateRequest {
	return wire.ProjectCreateRequest{Name: "New Project", Repo: "newrepo", Description: "A new project", Charter: creationCharter}
}

func (f *creationFixture) clone() string { return filepath.Join(f.cloneDir, "newrepo") }

func (f *creationFixture) git(t *testing.T, args ...string) string {
	t.Helper()
	return strings.TrimSpace(demoGit(t, f.dir, args...))
}

func (f *creationFixture) journal() string { return filepath.Join(f.root, "project-add.json") }

// reserve asks the service for the creation identity and sets it on req.
func (f *creationFixture) reserve(t *testing.T, c *Client) wire.ProjectCreateRequest {
	t.Helper()
	id, err := c.ReserveProjectCreation(context.Background())
	must(t, err)
	if err := config.CheckProjectIDs(id); err != nil {
		t.Fatal(err)
	}
	req := f.request()
	req.ID = id
	return req
}

// assertNothingWritten checks that a refused creation left no journal, no
// clone directory (other than one that was there already) and no project, and
// that no create call reached GitHub.
func (f *creationFixture) assertNothingWritten(t *testing.T, existing ...string) {
	t.Helper()
	if _, err := os.Lstat(f.journal()); !os.IsNotExist(err) {
		t.Fatalf("a journal was written: %v", err)
	}
	if got := f.github.CreateCalls(); got != 0 {
		t.Fatalf("%d create calls reached GitHub", got)
	}
	entries, err := os.ReadDir(f.cloneDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != strings.Join(existing, ",") {
		t.Fatalf("clone directory holds %v, want %v", names, existing)
	}
	if got := projectDirectories(t, f.root); len(got) != 0 {
		t.Fatalf("projects written: %v", got)
	}
	if got := f.github.Writes(); len(got) != 0 {
		t.Fatalf("GitHub was written to: %+v", got)
	}
}

// assertCompleted checks the project created under id against every outcome
// creation promises: it is listed as active with the requested fields, its
// clone has main checked out at the README commit, main is the remote's and
// GitHub's default branch, its first charter revision is the supplied charter,
// and the journal is gone.
func (f *creationFixture) assertCompleted(t *testing.T, s *Service, c *Client, id config.ProjectID, req wire.ProjectCreateRequest) {
	t.Helper()
	cfg, err := config.Load(f.opts.Config)
	must(t, err)
	if !cfg.Active(id) {
		t.Fatalf("project %s is not active: %v", id, cfg.ActiveProjects)
	}
	p := cfg.For(id).Project
	if p.Name != req.Name || p.Upstream != "owner/"+req.Repo || p.Fork != "" || p.BaseBranch != "main" || p.Clone != filepath.Join(f.cloneDir, req.Repo) || p.Landing != "commit-per-unit" {
		t.Fatalf("registered project: %+v", p)
	}
	if got := f.git(t, "-C", p.Clone, "symbolic-ref", "--short", "HEAD"); got != "main" {
		t.Fatalf("clone has %q checked out", got)
	}
	if got := f.git(t, "-C", p.Clone, "rev-parse", "HEAD"); got != f.readme {
		t.Fatalf("clone is at %s, not the README commit %s", got, f.readme)
	}
	if got := f.git(t, "-C", p.Clone, "rev-parse", "--abbrev-ref", "main@{upstream}"); got != "origin/main" {
		t.Fatalf("main tracks %q", got)
	}
	if got := f.git(t, "-C", f.remote, "rev-parse", "refs/heads/main"); got != f.readme {
		t.Fatalf("remote main is %s, not the README commit %s", got, f.readme)
	}
	if repo, ok := f.github.Repo("owner/" + req.Repo); !ok || repo.DefaultBranch != "main" {
		t.Fatalf("GitHub repository: %+v", repo)
	}
	if _, err := os.Lstat(f.journal()); !os.IsNotExist(err) {
		t.Fatalf("journal retained: %v", err)
	}
	if s.sole() == nil || s.sole().repository.Project() != id {
		t.Fatal("the created project is not the running one")
	}
	revisions := intakefixture.DocumentRevisions(t, s.sole().repository, "charter")
	if len(revisions) != 1 || revisions[0].Revision != 1 || revisions[0].Content != req.Charter {
		t.Fatalf("charter revisions: %+v", revisions)
	}
	state, err := c.ProjectCreation(context.Background(), id)
	must(t, err)
	if state.State != wire.CreationComplete || state.Project == nil || state.Project.ID != id || state.Project.Name != req.Name {
		t.Fatalf("creation state: %+v", state)
	}
}

func (f *creationFixture) assertNoDelete(t *testing.T) {
	t.Helper()
	if f.github.Deleted() {
		t.Fatalf("a delete reached GitHub: %+v", f.github.Calls())
	}
}

func TestProjectCreateCreatesCloneAndRegisters(t *testing.T) {
	f := newCreationFixture(t, "main", "")
	s, c := start(t, f.opts)
	ctx := context.Background()
	req := f.reserve(t, c)
	state, err := c.ProjectCreation(ctx, req.ID)
	must(t, err)
	if state.State != wire.CreationNotStarted || state.ID != req.ID {
		t.Fatalf("before creation: %+v", state)
	}
	added, err := c.CreateProject(ctx, req)
	must(t, err)
	if added.Project.ID != req.ID || added.Project.Upstream != "owner/newrepo" || added.Project.Fork != "" || added.Project.Clone != f.clone() || added.Project.BaseBranch != "main" {
		t.Fatalf("%+v", added)
	}
	f.assertCompleted(t, s, c, req.ID, req)
	// The repository is private, initialised with a README and carries the
	// description; the account's default-branch preference is left alone.
	repo, _ := f.github.Repo("owner/newrepo")
	if !repo.Private || !repo.AutoInit || repo.Description != "A new project" {
		t.Fatalf("created repository: %+v", repo)
	}
	if writes := f.github.Writes(); len(writes) != 1 || writes[0].Path != "/user/repos" {
		t.Fatalf("GitHub writes: %+v", writes)
	}
	f.assertNoDelete(t)
	// Repeating the request returns the project without another create.
	again, err := c.CreateProject(ctx, req)
	must(t, err)
	if again.Project.ID != req.ID || f.github.CreateCalls() != 1 {
		t.Fatalf("repeat: %+v, %d create calls", again, f.github.CreateCalls())
	}
}

func TestProjectCreateIsPublicOnlyWhenRequested(t *testing.T) {
	for _, public := range []bool{false, true} {
		t.Run(fmt.Sprint("public=", public), func(t *testing.T) {
			f := newCreationFixture(t, "main", "")
			_, c := start(t, f.opts)
			req := f.reserve(t, c)
			if public {
				req.Visibility = wire.VisibilityPublic
			}
			_, err := c.CreateProject(context.Background(), req)
			must(t, err)
			repo, _ := f.github.Repo("owner/newrepo")
			if repo.Private == public || !repo.AutoInit || repo.Description != "A new project" {
				t.Fatalf("created repository: %+v", repo)
			}
			var create map[string]any
			for _, call := range f.github.Calls() {
				if call.Path == "/user/repos" {
					create = call.Body
				}
			}
			if create["private"] != !public || create["auto_init"] != true || create["name"] != "newrepo" || create["description"] != "A new project" {
				t.Fatalf("create request: %v", create)
			}
		})
	}
}

func TestProjectCreateMovesTheREADMECommitToMain(t *testing.T) {
	f := newCreationFixture(t, "trunk", "")
	s, c := start(t, f.opts)
	req := f.reserve(t, c)
	_, err := c.CreateProject(context.Background(), req)
	must(t, err)
	f.assertCompleted(t, s, c, req.ID, req)
	if got := f.git(t, "-C", f.remote, "rev-parse", "refs/heads/trunk"); got != f.readme {
		t.Fatalf("the original branch moved to %s", got)
	}
	// The only writes are the create and the repository's own default branch;
	// nothing touches the account's preference.
	writes := f.github.Writes()
	if len(writes) != 2 || writes[0].Path != "/user/repos" || writes[1].Method != "PATCH" || writes[1].Path != "/repos/owner/newrepo" || len(writes[1].Body) != 1 || writes[1].Body["default_branch"] != "main" {
		t.Fatalf("GitHub writes: %+v", writes)
	}
	f.assertNoDelete(t)
}

func TestProjectCreateRefusesBeforeWritingAnything(t *testing.T) {
	cases := []struct {
		name     string
		projects string
		prepare  func(t *testing.T, f *creationFixture)
		code     wire.Code
		message  string
		existing []string
	}{
		{name: "no projects table", projects: "# none\n", code: Validation, message: "projects.repo_owner"},
		{name: "no clone directory", projects: "[projects]\nrepo_owner = \"owner\"\n", code: Validation, message: "projects.clone_dir"},
		{name: "no repo owner", projects: "[projects]\nclone_dir = \"/tmp/clones\"\n", code: Validation, message: "projects.repo_owner"},
		{name: "no token", prepare: func(t *testing.T, f *creationFixture) { t.Setenv("GITHUB_TOKEN", "") }, code: Validation, message: "GITHUB_TOKEN is not set"},
		{name: "another account", prepare: func(t *testing.T, f *creationFixture) { f.github.Login = "someone-else" }, code: Validation, message: "someone-else"},
		{name: "taken name", prepare: func(t *testing.T, f *creationFixture) { f.github.AddRepo("owner/newrepo", "elsewhere", "main") }, code: Validation, message: "already exists on GitHub"},
		{name: "existing clone path", prepare: func(t *testing.T, f *creationFixture) { must(t, os.MkdirAll(f.clone(), 0700)) }, code: Validation, message: "already exists", existing: []string{"newrepo"}},
		{name: "bad repo name", prepare: func(t *testing.T, f *creationFixture) {}, code: Validation, message: "repo is not a valid GitHub repository name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCreationFixture(t, "main", tc.projects)
			if tc.prepare != nil {
				tc.prepare(t, f)
			}
			_, c := start(t, f.opts)
			req := f.request()
			if tc.name == "bad repo name" {
				req.Repo = "not a name"
			}
			_, err := c.CreateProject(context.Background(), req)
			assertCode(t, err, tc.code)
			if !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("message %q lacks %q", err, tc.message)
			}
			f.assertNothingWritten(t, tc.existing...)
		})
	}
}

func TestProjectCreateStopsWhenGitHubCreatesAnotherRepository(t *testing.T) {
	f := newCreationFixture(t, "main", "")
	f.github.OnCreate = func(g *servicetest.FakeGitHub, body map[string]any) (bool, int, any) {
		return true, 201, map[string]any{"id": 4242, "full_name": "owner/other", "ssh_url": f.remote, "default_branch": "main"}
	}
	s, c := start(t, f.opts)
	req := f.reserve(t, c)
	_, err := c.CreateProject(context.Background(), req)
	assertCode(t, err, Conflict)
	if !strings.Contains(err.Error(), "owner/other") || !strings.Contains(err.Error(), "ownership") {
		t.Fatal(err)
	}
	state, lookup := c.ProjectCreation(context.Background(), req.ID)
	must(t, lookup)
	if state.State != wire.CreationPending || state.Stuck == nil || state.Stuck.Kind != wire.StuckMismatch {
		t.Fatalf("%+v", state)
	}
	var journal struct {
		Creation struct {
			RepositoryID int64  `json:"repository_id"`
			FullName     string `json:"full_name"`
		}
	}
	data, err := os.ReadFile(f.journal())
	must(t, err)
	must(t, json.Unmarshal(data, &journal))
	if journal.Creation.RepositoryID != 4242 || journal.Creation.FullName != "owner/other" {
		t.Fatalf("journal: %s", data)
	}
	if _, err := os.Lstat(f.cloneDir); !os.IsNotExist(err) {
		t.Fatalf("something was cloned: %v", err)
	}
	if s.sole() != nil || len(activeProjects(t, f.root)) != 0 {
		t.Fatal("a project was registered")
	}
	// A retry never creates a second repository.
	_, err = c.CreateProject(context.Background(), wire.ProjectCreateRequest{ID: req.ID})
	assertCode(t, err, Conflict)
	if f.github.CreateCalls() != 1 {
		t.Fatalf("%d create calls", f.github.CreateCalls())
	}
	f.assertNoDelete(t)
}

// creationSteps are the interruption points of a creation, each reached after
// the step's effect: the journal, the repository's identity, the clone, the
// main branch and the registration steps.
var creationSteps = []struct{ seam, reached string }{
	{"journal-written", wire.CreationPlanned},
	{"repository-created", wire.CreationRepository},
	{"repository-cloned", wire.CreationCloned},
	{"main-branch-ready", wire.CreationMain},
	{"project-config-written", wire.CreationMain},
	{"trace-created", wire.CreationMain},
	{"active-project-listed", wire.CreationMain},
	{"journal-removed", wire.CreationComplete},
}

func TestProjectCreateRecoversAtEachStep(t *testing.T) {
	for _, step := range creationSteps {
		for _, mode := range []string{"restart", "retry"} {
			t.Run(step.seam+"/"+mode, func(t *testing.T) {
				f := newCreationFixture(t, "trunk", "")
				s, c := start(t, f.opts)
				ctx := context.Background()
				req := f.reserve(t, c)
				crash := errors.New("crash")
				s.boundary = func(name string) error {
					if name == step.seam {
						return crash
					}
					return nil
				}
				_, err := c.CreateProject(ctx, req)
				assertCode(t, err, Internal)
				if !strings.Contains(err.Error(), "incomplete") || !strings.Contains(err.Error(), string(req.ID)) {
					t.Fatal(err)
				}
				state, lookup := c.ProjectCreation(ctx, req.ID)
				must(t, lookup)
				if step.reached == wire.CreationComplete {
					if state.State != wire.CreationComplete || state.Project == nil || state.Project.ID != req.ID {
						t.Fatalf("after the journal was removed: %+v", state)
					}
				} else if state.State != wire.CreationPending || state.Step != step.reached || state.Stuck != nil || state.Repository != "owner/newrepo" {
					t.Fatalf("pending state: %+v", state)
				}
				s.boundary = nil
				if mode == "restart" {
					must(t, s.Close())
					s, c = start(t, f.opts)
					cfg, err := c.Configuration(ctx)
					must(t, err)
					if cfg.Project == nil || cfg.Project.ID != req.ID || len(cfg.Diagnostics) != 0 {
						t.Fatalf("not completed after restart: %+v", cfg)
					}
				} else {
					added, err := c.CreateProject(ctx, req)
					must(t, err)
					if added.Project.ID != req.ID {
						t.Fatalf("retry created %s instead of finishing %s", added.Project.ID, req.ID)
					}
				}
				f.assertCompleted(t, s, c, req.ID, req)
				if got := f.github.CreateCalls(); got != 1 {
					t.Fatalf("%d create calls", got)
				}
				f.assertNoDelete(t)
				if names := projectDirectories(t, f.root); len(names) != 1 || names[0] != string(req.ID) {
					t.Fatalf("traces: %v", names)
				}
			})
		}
	}
}

func TestProjectCreateStopsWhenARepositoryAppearsBeforeTheCreate(t *testing.T) {
	f := newCreationFixture(t, "main", "")
	s, c := start(t, f.opts)
	ctx := context.Background()
	req := f.reserve(t, c)
	s.boundary = func(name string) error {
		if name == "journal-written" {
			return errors.New("crash")
		}
		return nil
	}
	_, err := c.CreateProject(ctx, req)
	assertCode(t, err, Internal)
	s.boundary = nil
	unrelated, _ := servicetest.SeedRemote(t, f.dir, "unrelated", "main")
	f.github.AddRepo("owner/newrepo", unrelated, "main")
	must(t, s.Close())
	s, c = start(t, f.opts)
	f.assertOwnershipStuck(t, s, c, req)
	// The stopped creation is reported with the command that finishes it.
	cfg, err := c.Configuration(ctx)
	must(t, err)
	var message string
	for _, d := range cfg.Diagnostics {
		if d.Field == "projects" {
			message = d.Message
		}
	}
	if !strings.Contains(message, "project creation is incomplete") || !strings.Contains(message, "osmia project create --id "+string(req.ID)) || !strings.Contains(message, "ownership could not be established") {
		t.Fatalf("diagnostics: %+v", cfg.Diagnostics)
	}
}

// assertOwnershipStuck checks that the creation under req stopped as stuck
// because the repository's ownership could not be established, and that
// nothing of the unrelated repository was cloned or registered.
func (f *creationFixture) assertOwnershipStuck(t *testing.T, s *Service, c *Client, req wire.ProjectCreateRequest) {
	t.Helper()
	ctx := context.Background()
	state, err := c.ProjectCreation(ctx, req.ID)
	must(t, err)
	if state.State != wire.CreationPending || state.Stuck == nil || state.Stuck.Kind != wire.StuckOwnership || !strings.Contains(state.Stuck.Reason, "ownership could not be established") {
		t.Fatalf("%+v", state)
	}
	if _, err := os.Lstat(f.clone()); !os.IsNotExist(err) {
		t.Fatalf("the unrelated repository was cloned: %v", err)
	}
	if s.sole() != nil || len(activeProjects(t, f.root)) != 0 {
		t.Fatal("the unrelated repository was registered")
	}
	_, err = c.CreateProject(ctx, wire.ProjectCreateRequest{ID: req.ID})
	assertCode(t, err, Conflict)
	if !strings.Contains(err.Error(), "ownership could not be established") {
		t.Fatal(err)
	}
	f.assertNoDelete(t)
}

func TestProjectCreateStopsOnANameConflict(t *testing.T) {
	f := newCreationFixture(t, "main", "")
	unrelated, _ := servicetest.SeedRemote(t, f.dir, "unrelated", "main")
	f.github.OnCreate = func(g *servicetest.FakeGitHub, body map[string]any) (bool, int, any) {
		g.AddRepo("owner/newrepo", unrelated, "main")
		return true, 422, servicetest.GitHubNameTaken
	}
	s, c := start(t, f.opts)
	req := f.reserve(t, c)
	_, err := c.CreateProject(context.Background(), req)
	assertCode(t, err, Conflict)
	f.assertOwnershipStuck(t, s, c, req)
	if f.github.CreateCalls() != 1 {
		t.Fatalf("%d create calls", f.github.CreateCalls())
	}
	// A restart does not resend the create.
	must(t, s.Close())
	s, c = start(t, f.opts)
	f.assertOwnershipStuck(t, s, c, req)
	if f.github.CreateCalls() != 1 {
		t.Fatalf("%d create calls after the restart", f.github.CreateCalls())
	}
}

func TestProjectCreateStopsWhenAnUnknownOutcomeLeavesARepository(t *testing.T) {
	f := newCreationFixture(t, "main", "")
	unrelated, _ := servicetest.SeedRemote(t, f.dir, "unrelated", "main")
	f.github.OnCreate = func(g *servicetest.FakeGitHub, body map[string]any) (bool, int, any) {
		g.AddRepo("owner/newrepo", unrelated, "main")
		return true, 502, map[string]string{"message": "Bad Gateway"}
	}
	s, c := start(t, f.opts)
	req := f.reserve(t, c)
	_, err := c.CreateProject(context.Background(), req)
	assertCode(t, err, Internal)
	if !strings.Contains(err.Error(), "outcome") {
		t.Fatal(err)
	}
	must(t, s.Close())
	s, c = start(t, f.opts)
	f.assertOwnershipStuck(t, s, c, req)
	if f.github.CreateCalls() != 1 {
		t.Fatalf("%d create calls", f.github.CreateCalls())
	}
}

func TestProjectCreateMayResendAnUnknownOutcomeWhenNothingExists(t *testing.T) {
	f := newCreationFixture(t, "main", "")
	first := true
	f.github.OnCreate = func(g *servicetest.FakeGitHub, body map[string]any) (bool, int, any) {
		if first {
			first = false
			return true, 502, map[string]string{"message": "Bad Gateway"}
		}
		return false, 0, nil
	}
	s, c := start(t, f.opts)
	req := f.reserve(t, c)
	_, err := c.CreateProject(context.Background(), req)
	assertCode(t, err, Internal)
	if _, err := os.Lstat(f.journal()); err != nil {
		t.Fatal(err)
	}
	_, err = c.CreateProject(context.Background(), req)
	must(t, err)
	f.assertCompleted(t, s, c, req.ID, req)
	if f.github.CreateCalls() != 2 {
		t.Fatalf("%d create calls", f.github.CreateCalls())
	}
	f.assertNoDelete(t)
}

func TestProjectCreateResumesAfterTheOwnerResolvesAConflict(t *testing.T) {
	f := newCreationFixture(t, "main", "")
	unrelated, _ := servicetest.SeedRemote(t, f.dir, "unrelated", "main")
	conflict := true
	f.github.OnCreate = func(g *servicetest.FakeGitHub, body map[string]any) (bool, int, any) {
		if conflict {
			g.AddRepo("owner/newrepo", unrelated, "main")
			return true, 422, servicetest.GitHubNameTaken
		}
		return false, 0, nil
	}
	s, c := start(t, f.opts)
	req := f.reserve(t, c)
	_, err := c.CreateProject(context.Background(), req)
	assertCode(t, err, Conflict)
	// The owner renames the other repository on GitHub and retries.
	conflict = false
	f.github.Forget("owner/newrepo")
	_, err = c.CreateProject(context.Background(), wire.ProjectCreateRequest{ID: req.ID})
	must(t, err)
	f.assertCompleted(t, s, c, req.ID, req)
}

func TestProjectCreateNeverReusesARepositoryWithAnotherIdentity(t *testing.T) {
	f := newCreationFixture(t, "main", "")
	s, c := start(t, f.opts)
	ctx := context.Background()
	req := f.reserve(t, c)
	s.boundary = func(name string) error {
		if name == "repository-created" {
			return errors.New("crash")
		}
		return nil
	}
	_, err := c.CreateProject(ctx, req)
	assertCode(t, err, Internal)
	s.boundary = nil
	// The repository this creation made is replaced by another of its name.
	f.github.Forget("owner/newrepo")
	unrelated, _ := servicetest.SeedRemote(t, f.dir, "unrelated", "main")
	f.github.AddRepo("owner/newrepo", unrelated, "main")
	_, err = c.CreateProject(ctx, req)
	assertCode(t, err, Conflict)
	f.assertOwnershipStuck(t, s, c, req)
	if f.github.CreateCalls() != 1 {
		t.Fatalf("%d create calls", f.github.CreateCalls())
	}
}

func TestProjectCreateCloneFailureIsRetriedOnTheSameRepository(t *testing.T) {
	f := newCreationFixture(t, "main", "")
	later := filepath.Join(f.dir, "later.git")
	f.github.Remote = func(string) string { return later }
	s, c := start(t, f.opts)
	ctx := context.Background()
	req := f.reserve(t, c)
	_, err := c.CreateProject(ctx, req)
	assertCode(t, err, Conflict)
	if !strings.Contains(err.Error(), "git credential") {
		t.Fatalf("the error does not name the host git credential: %v", err)
	}
	state, lookup := c.ProjectCreation(ctx, req.ID)
	must(t, lookup)
	if state.State != wire.CreationPending || state.Step != wire.CreationRepository || state.Stuck == nil || state.Stuck.Kind != wire.StuckClone || !strings.Contains(state.Stuck.Reason, "git credential") {
		t.Fatalf("%+v", state)
	}
	if entries, _ := os.ReadDir(f.cloneDir); len(entries) != 0 {
		t.Fatalf("a failed clone left %d entries", len(entries))
	}
	// The clone source is fixed: the repository is reachable now.
	remote, readme := servicetest.SeedRemote(t, f.dir, "later", "main")
	f.remote, f.readme = remote, readme
	_, err = c.CreateProject(ctx, req)
	must(t, err)
	f.assertCompleted(t, s, c, req.ID, req)
	if f.github.CreateCalls() != 1 {
		t.Fatalf("%d create calls", f.github.CreateCalls())
	}
}

func TestProjectCreateKeepsTheTokenOutOfGit(t *testing.T) {
	const sentinel = "ghp_sentinel_0123456789abcdef"
	f := newCreationFixture(t, "trunk", "")
	logs := filepath.Join(f.dir, "gitlog")
	must(t, os.MkdirAll(logs, 0700))
	bin := filepath.Join(f.dir, "bin")
	must(t, os.MkdirAll(bin, 0700))
	real, err := exec.LookPath("git")
	must(t, err)
	// Other git callers build their own environment, so the wrapper carries
	// the log directory and the real git itself.
	script := fmt.Sprintf("#!/bin/sh\n{ echo ARGS; for a in \"$@\"; do echo \"$a\"; done; echo ENV; env; } > %q/call-$$\nexec %q \"$@\"\n", logs, real)
	must(t, os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700))
	t.Setenv("OSMIA_CREATION_MARKER", "inherited")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GITHUB_TOKEN", sentinel)
	s, c := start(t, f.opts)
	req := f.reserve(t, c)
	_, err = c.CreateProject(context.Background(), req)
	must(t, err)
	f.assertCompleted(t, s, c, req.ID, req)
	entries, err := os.ReadDir(logs)
	must(t, err)
	var clones, pushes int
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(logs, e.Name()))
		must(t, err)
		text := string(data)
		if strings.Contains(text, sentinel) {
			t.Fatalf("the token reached a git invocation:\n%s", text)
		}
		args, env, _ := strings.Cut(text, "\nENV\n")
		if strings.HasPrefix(args, "ARGS\nclone\n") {
			clones++
			// The environment is inherited, minus the token.
			if !strings.Contains(env, "OSMIA_CREATION_MARKER=inherited\n") || strings.Contains(env, "GITHUB_TOKEN") {
				t.Fatalf("clone environment:\n%s", env)
			}
		}
		if strings.HasPrefix(args, "ARGS\npush\n") {
			pushes++
		}
	}
	if clones != 1 || pushes != 1 {
		t.Fatalf("captured %d clones and %d pushes among %d git invocations", clones, pushes, len(entries))
	}
	config, err := os.ReadFile(filepath.Join(f.clone(), ".git", "config"))
	must(t, err)
	if strings.Contains(string(config), sentinel) {
		t.Fatalf("the clone's git configuration holds the token:\n%s", config)
	}
}

func TestProjectAddReportsAPendingCreation(t *testing.T) {
	f := newCreationFixture(t, "main", "")
	s, c := start(t, f.opts)
	ctx := context.Background()
	req := f.reserve(t, c)
	s.boundary = func(name string) error {
		if name == "repository-created" {
			return errors.New("crash")
		}
		return nil
	}
	_, err := c.CreateProject(ctx, req)
	assertCode(t, err, Internal)
	s.boundary = nil
	before, err := os.ReadFile(f.journal())
	must(t, err)
	_, err = c.AddProject(ctx, request(f.remote))
	assertCode(t, err, Conflict)
	if !strings.Contains(err.Error(), string(req.ID)) || !strings.Contains(err.Error(), "creation") {
		t.Fatal(err)
	}
	// Neither another creation nor a registration finishes or discards it.
	other, err := c.ReserveProjectCreation(ctx)
	must(t, err)
	_, err = c.CreateProject(ctx, wire.ProjectCreateRequest{ID: other, Name: "Other", Repo: "other", Charter: creationCharter})
	assertCode(t, err, Conflict)
	after, err := os.ReadFile(f.journal())
	must(t, err)
	if string(before) != string(after) {
		t.Fatalf("the journal changed:\n%s\n%s", before, after)
	}
	if _, err := os.Lstat(f.clone()); !os.IsNotExist(err) {
		t.Fatalf("the clone was made: %v", err)
	}
	// A different plan under the same identity is refused.
	changed := req
	changed.Description = "Something else"
	_, err = c.CreateProject(ctx, changed)
	assertCode(t, err, Conflict)
	// The creation itself finishes.
	_, err = c.CreateProject(ctx, req)
	must(t, err)
	f.assertCompleted(t, s, c, req.ID, req)
}

func TestProjectCreationStates(t *testing.T) {
	f := newCreationFixture(t, "main", "")
	s, c := start(t, f.opts)
	ctx := context.Background()
	req := f.reserve(t, c)
	state, err := c.ProjectCreation(ctx, req.ID)
	must(t, err)
	if state.State != wire.CreationNotStarted {
		t.Fatalf("%+v", state)
	}
	_, err = c.ProjectCreation(ctx, "not-an-id")
	assertCode(t, err, Validation)
	s.boundary = func(name string) error {
		if name == "journal-removed" {
			return errors.New("crash")
		}
		return nil
	}
	_, err = c.CreateProject(ctx, req)
	assertCode(t, err, Internal)
	// The journal is gone, and the registered project still answers.
	if _, err := os.Lstat(f.journal()); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	state, err = c.ProjectCreation(ctx, req.ID)
	must(t, err)
	if state.State != wire.CreationComplete || state.Project == nil || state.Project.ID != req.ID || state.Project.Upstream != "owner/newrepo" {
		t.Fatalf("%+v", state)
	}
	other, err := c.ReserveProjectCreation(ctx)
	must(t, err)
	state, err = c.ProjectCreation(ctx, other)
	must(t, err)
	if state.State != wire.CreationNotStarted || other == req.ID {
		t.Fatalf("%+v", state)
	}
}
