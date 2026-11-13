package isolation

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/agentcore/agent"
	a "github.com/kpenfound/osmia/internal/coreadapter"
)

var daggerSettings = a.ExecutionSettings{Mode: agent.SandboxSbx, Dagger: &agent.Dagger{Engine: "tcp://127.0.0.1:1234", Version: "v0.20.5"}}

// sbxEndpoint is the host loopback address a sandbox reaches the service's MCP
// server at.
const sbxEndpoint = "http://127.0.0.1:8080/mcp"

// gitIn runs a read-only git command in dir with no host configuration.
func gitIn(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// A view for a turn whose execution settings carry Dagger has a Git repository
// at its root, and the repository is empty whatever the host's Git templates,
// configuration and environment say.
// serial: sets Git environment variables via t.Setenv
func TestDaggerViewsHoldAnEmptyRepository(t *testing.T) {
	source, views, host := t.TempDir(), Views{Directory: t.TempDir()}, t.TempDir()
	put(t, source, "src/main.go", "original")
	put(t, source, ".git/config", "[remote \"origin\"]\n\turl = https://example.invalid/clone.git\n")
	// A hostile host: a template with a hook, a configured hooks path and
	// template directory, and a Git directory in the environment.
	put(t, host, "template/hooks/pre-commit", "#!/bin/sh\necho hook\n")
	put(t, host, "template/info/exclude", "excluded\n")
	put(t, host, "gitconfig", "[init]\n\ttemplateDir = "+filepath.Join(host, "template")+"\n[core]\n\thooksPath = "+filepath.Join(host, "template", "hooks")+"\n[remote \"host\"]\n\turl = https://example.invalid/host.git\n")
	elsewhere := filepath.Join(host, "elsewhere")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(host, "gitconfig"))
	t.Setenv("GIT_TEMPLATE_DIR", filepath.Join(host, "template"))
	t.Setenv("GIT_DIR", elsewhere)
	t.Setenv("GIT_WORK_TREE", host)
	t.Setenv("HOME", host)
	t.Setenv("XDG_CONFIG_HOME", host)
	v, err := views.CreateFor(context.Background(), a.Workspace{Directory: source, Access: a.ReadWrite}, []string{"src"}, daggerSettings)
	if err != nil {
		t.Fatal(err)
	}
	dir := v.Workspace().Directory
	t.Cleanup(func() { v.Release(context.Background()) })
	if data, err := os.ReadFile(filepath.Join(dir, "src/main.go")); err != nil || string(data) != "original" {
		t.Fatalf("the view's files: %q %v", data, err)
	}
	if _, err := os.Stat(elsewhere); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the repository went to the inherited GIT_DIR: %v", err)
	}
	if info, err := os.Lstat(filepath.Join(dir, ".git")); err != nil || !info.IsDir() {
		t.Fatalf("the view's .git: %v %v", info, err)
	}
	if top, err := gitIn(t, dir, "rev-parse", "--show-toplevel"); err != nil || top != dir {
		t.Fatalf("the view's Git root is %q: %v", top, err)
	}
	for _, args := range [][]string{{"rev-parse", "--verify", "--quiet", "HEAD"}, {"log", "--oneline"}} {
		if out, err := gitIn(t, dir, args...); err == nil {
			t.Fatalf("git %v succeeded in a repository with no commits: %s", args, out)
		}
	}
	for _, args := range [][]string{{"remote"}, {"ls-files"}, {"for-each-ref"}, {"config", "--local", "--get-regexp", "^(remote|branch)\\."}, {"stash", "list"}} {
		if out, _ := gitIn(t, dir, args...); out != "" {
			t.Fatalf("git %v in the view's repository: %q", args, out)
		}
	}
	var files []string
	err = filepath.WalkDir(filepath.Join(dir, ".git"), func(name string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			rel, _ := filepath.Rel(filepath.Join(dir, ".git"), name)
			files = append(files, rel)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if name != "HEAD" && name != "config" {
			t.Fatalf("the view's repository holds %s, want HEAD and config alone: %v", name, files)
		}
	}
	for _, name := range []string{"hooks", "info", "description"} {
		if _, err := os.Lstat(filepath.Join(dir, ".git", name)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf(".git/%s from a template: %v", name, err)
		}
	}
	config, err := os.ReadFile(filepath.Join(dir, ".git", "config"))
	if err != nil || strings.Contains(string(config), "example.invalid") {
		t.Fatalf("the view's repository configuration: %q %v", config, err)
	}
}

func TestViewsWithoutDaggerHoldNoRepository(t *testing.T) {
	source, views := t.TempDir(), Views{Directory: t.TempDir()}
	put(t, source, "src/main.go", "original")
	put(t, source, ".git/HEAD", "ref: refs/heads/main\n")
	for name, settings := range map[string]a.ExecutionSettings{
		"none":     {},
		"sbx":      {Mode: agent.SandboxSbx},
		"claude":   {Mode: agent.SandboxClaude},
		"template": {Mode: agent.SandboxSbx, Image: "example/template:1"},
	} {
		t.Run(name, func(t *testing.T) {
			v, err := views.CreateFor(context.Background(), a.Workspace{Directory: source, Access: a.ReadWrite}, []string{"src"}, settings)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { v.Release(context.Background()) })
			if _, err := os.Lstat(filepath.Join(v.Workspace().Directory, ".git")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the view holds .git: %v", err)
			}
		})
	}
}

// The repository stays out of reach of the file tools and goes with the view.
func TestDaggerViewRepositoryIsHiddenAndReleasedWithTheView(t *testing.T) {
	source := t.TempDir()
	put(t, source, "src/main.go", "original")
	v, err := (Views{Directory: t.TempDir()}).CreateFor(context.Background(), a.Workspace{Directory: source, Access: a.ReadWrite}, []string{"src"}, daggerSettings)
	if err != nil {
		t.Fatal(err)
	}
	dir := v.Workspace().Directory
	if _, err := os.Lstat(filepath.Join(dir, ".git", "HEAD")); err != nil {
		t.Fatalf("no repository: %v", err)
	}
	if listing, err := v.Read("."); err != nil || string(listing) != "src/" {
		t.Fatalf("the view root lists %q: %v", listing, err)
	}
	for _, path := range []string{".git", ".git/", ".git/HEAD", ".git/config", ".git/hooks/pre-commit", "src/../.git/HEAD"} {
		if _, err := v.Read(path); err == nil {
			t.Fatalf("read of %s accepted", path)
		}
		if err := v.Write(path, []byte("bad")); err == nil {
			t.Fatalf("write of %s accepted", path)
		}
	}
	if head, err := os.ReadFile(filepath.Join(dir, ".git", "HEAD")); err != nil || !strings.HasPrefix(string(head), "ref: refs/heads/") {
		t.Fatalf("the repository changed: %q %v", head, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, ".git", "hooks")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a refused write reached the repository: %v", err)
	}
	if err := v.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the view and its repository remain: %v", err)
	}
}

func TestViewRepositoryFailureReleasesTheView(t *testing.T) {
	source, views := t.TempDir(), Views{Directory: t.TempDir()}
	put(t, source, "src/main.go", "original")
	t.Setenv("PATH", t.TempDir())
	if _, err := views.CreateFor(context.Background(), a.Workspace{Directory: source, Access: a.ReadWrite}, []string{"src"}, daggerSettings); err == nil {
		t.Fatal("a view without a repository was returned for a Dagger turn")
	}
	if entries, err := os.ReadDir(views.Directory); err != nil || len(entries) != 0 {
		t.Fatalf("the failed view remains: %v %v", entries, err)
	}
}

// A Dagger turn's view holds its repository before the sandbox is prepared, the
// turn's isolation still denies version control, and the repository goes with
// the view.
func TestDaggerTurnRunsInAViewWithItsRepositoryAndNoVCS(t *testing.T) {
	r, p, h, engine, input := fixture(t, "mason", agent.SandboxSbx)
	h.url = sbxEndpoint
	inner := r.Select
	r.Select = func(ctx context.Context, scope a.Scope) (Selection, error) {
		selection, err := inner(ctx, scope)
		selection.Execution = daggerSettings
		return selection, err
	}
	var view string
	engine.PrepareFor = func(grants agent.Grants) error {
		view = grants.Mounts[0].Path
		if _, err := os.Lstat(filepath.Join(view, ".git", "HEAD")); err != nil {
			return errors.New("the view holds no repository when the sandbox is prepared: " + err.Error())
		}
		return nil
	}
	var captured string
	r.Capture = func(_ context.Context, _ a.Scope, v *FileView, _ a.SessionResult) error {
		if _, err := os.Lstat(filepath.Join(v.Workspace().Directory, ".git", "HEAD")); err != nil {
			return err
		}
		listing, err := v.Read(".")
		captured = string(listing)
		return err
	}
	if _, err := r.Run(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if len(engine.Requests) != 1 || view == "" {
		t.Fatalf("the turn did not run: %d requests", len(engine.Requests))
	}
	req := engine.Requests[0]
	if req.Grants.VCS || req.Grants.DaggerEngine != daggerSettings.Dagger.Engine || req.Profile.Dagger == nil {
		t.Fatalf("grants %+v profile Dagger %+v", req.Grants, req.Profile.Dagger)
	}
	for i, mount := range req.Grants.Mounts {
		if mount.EmptyRepository != (i == 0) {
			t.Fatalf("mount %d %+v: only the view is marked as an empty repository", i, mount)
		}
	}
	if captured != "src/" {
		t.Fatalf("the file tools list %q", captured)
	}
	if _, err := os.Lstat(view); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the view and its repository remain: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(p.directory, ".git")); string(data) != "gitdir: /service/vcs" {
		t.Fatal("the provider workspace changed")
	}
}

// A turn without Dagger runs in a view with no repository.
func TestTurnWithoutDaggerRunsInAViewWithNoRepository(t *testing.T) {
	r, _, h, engine, input := fixture(t, "mason", agent.SandboxSbx)
	h.url = sbxEndpoint
	engine.PrepareFor = func(grants agent.Grants) error {
		if _, err := os.Lstat(filepath.Join(grants.Mounts[0].Path, ".git")); !errors.Is(err, fs.ErrNotExist) {
			return errors.New("the view holds .git")
		}
		return nil
	}
	if _, err := r.Run(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if len(engine.Requests) != 1 {
		t.Fatal("the turn did not run")
	}
	for _, mount := range engine.Requests[0].Grants.Mounts {
		if mount.EmptyRepository {
			t.Fatalf("mount %+v is marked as an empty repository", mount)
		}
	}
}
