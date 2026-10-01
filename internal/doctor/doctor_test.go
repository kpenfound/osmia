package doctor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

const projectID = "p_0123456789abcdef0123456789abcdef"

const topLevel = `version = 1
active_projects = [%q]

[profiles.default]
agent = "claude"
model = "opus"
`

// host is a fake toolchain: every executable is on PATH and answers, except
// those named missing.
type host struct{ missing map[string]bool }

func (h host) lookPath(name string) (string, error) {
	if h.missing[name] {
		return "", exec.ErrNotFound
	}
	return "/bin/" + name, nil
}

func (host) command(_ context.Context, path string, _ ...string) (string, error) {
	return filepath.Base(path) + " 1.0.0\n", nil
}

// github is a fake GitHub API that accepts the token "good" and knows the
// repositories listed.
func github(t *testing.T, repositories ...string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/user" {
			fmt.Fprint(w, `{"login":"owner"}`)
			return
		}
		for _, repo := range repositories {
			if r.URL.Path == "/repos/"+repo {
				fmt.Fprintf(w, `{"full_name":%q}`, repo)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// fixture is a root with one active project whose clone has a local
// upstream remote, and the options that check it against a fake host.
func fixture(t *testing.T) (Options, string) {
	t.Helper()
	base := t.TempDir()
	upstream := filepath.Join(base, "remotes", "acme", "widgets")
	must(t, os.MkdirAll(upstream, 0o700))
	git(t, upstream, "init", "--quiet", "--bare", "--initial-branch=main")
	clone := filepath.Join(base, "clone")
	must(t, os.MkdirAll(clone, 0o700))
	git(t, clone, "init", "--quiet", "--initial-branch=main")
	git(t, clone, "commit", "--quiet", "--allow-empty", "-m", "first")
	git(t, clone, "remote", "add", "origin", upstream)
	git(t, clone, "push", "--quiet", "origin", "main")
	dir := filepath.Join(base, "r")
	must(t, os.MkdirAll(filepath.Join(dir, "projects", projectID), 0o700))
	must(t, os.WriteFile(filepath.Join(dir, "config.toml"), fmt.Appendf(nil, topLevel, projectID), 0o600))
	project := fmt.Sprintf("version = 1\nname = \"widgets\"\nupstream = \"acme/widgets\"\nclone = %q\n", clone)
	must(t, os.WriteFile(filepath.Join(dir, "projects", projectID, "config.toml"), []byte(project), 0o600))
	root, err := config.ResolveRoot(dir, "")
	must(t, err)
	h := host{}
	return Options{
		Root: root, LookPath: h.lookPath, Command: h.command,
		CheckJJ:   func(context.Context) (string, error) { return "0.45.1", nil },
		Token:     "good",
		GitHubAPI: github(t, "acme/widgets"),
	}, dir
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func find(t *testing.T, checks []Check, name string) Check {
	t.Helper()
	for _, c := range checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check %q in %+v", name, checks)
	return Check{}
}

func absent(t *testing.T, checks []Check, name string) {
	t.Helper()
	for _, c := range checks {
		if c.Name == name {
			t.Fatalf("unexpected check %+v", c)
		}
	}
}

func expect(t *testing.T, c Check, status Status, detail string) {
	t.Helper()
	if c.Status != status || !strings.Contains(c.Detail, detail) {
		t.Fatalf("%s: got %s %q, want %s containing %q", c.Name, c.Status, c.Detail, status, detail)
	}
	if status != Pass && c.Remediation == "" {
		t.Fatalf("%s: %s without a remediation", c.Name, status)
	}
}

func TestHealthyRoot(t *testing.T) {
	o, dir := fixture(t)
	root, err := config.ResolveRoot(dir, "")
	must(t, err)
	r, err := trace.Create(context.Background(), root, config.Project{ID: projectID, Name: "widgets", Clone: filepath.Join(filepath.Dir(dir), "clone")}, time.Unix(0, 0).UTC(), trace.Actor{Kind: "owner", ID: "local"})
	must(t, err)
	must(t, r.Close())
	checks := Run(context.Background(), o)
	for _, c := range checks {
		if c.Status != Pass {
			t.Errorf("%+v", c)
		}
	}
	expect(t, find(t, checks, "widgets upstream"), Pass, `remote "origin" answers, branch main exists`)
	expect(t, find(t, checks, "widgets trace"), Pass, "0 workstreams")
	expect(t, find(t, checks, "runtime.json"), Pass, "")
	expect(t, find(t, checks, "claude"), Pass, "claude 1.0.0")
	expect(t, find(t, checks, "GITHUB_TOKEN"), Pass, "owner")
	absent(t, checks, "docker")
	if Failed(checks) {
		t.Fatal("a healthy root failed")
	}
	// Doctor releases the ownership lock it took.
	lock, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_RDWR, 0)
	must(t, err)
	defer lock.Close()
	must(t, syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
}

func TestMissingRootChecksToolchainOnly(t *testing.T) {
	o, _ := fixture(t)
	root, err := config.ResolveRoot(filepath.Join(t.TempDir(), "absent"), "")
	must(t, err)
	o.Root = root
	checks := Run(context.Background(), o)
	expect(t, find(t, checks, "root directory"), Pass, "does not exist yet")
	find(t, checks, "git")
	absent(t, checks, "config.toml")
	absent(t, checks, "ownership lock")
	if _, err := os.Stat(root.String()); !os.IsNotExist(err) {
		t.Fatalf("doctor created the root: %v", err)
	}
}

func TestConfigurationErrorsNameTheField(t *testing.T) {
	o, dir := fixture(t)
	must(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte("version = 1\nsecret_value = \"hunter2\"\n"), 0o600))
	checks := Run(context.Background(), o)
	c := find(t, checks, "config.toml")
	expect(t, c, Fail, "secret_value")
	if strings.Contains(c.Detail, "hunter2") {
		t.Fatalf("detail quotes the file: %q", c.Detail)
	}
	absent(t, checks, "project "+projectID)
	if !Failed(checks) {
		t.Fatal("a broken configuration passed")
	}

	must(t, os.Remove(filepath.Join(dir, "config.toml")))
	expect(t, find(t, Run(context.Background(), o), "config.toml"), Fail, "cannot be read")
}

func TestProjectConfigurationError(t *testing.T) {
	o, dir := fixture(t)
	must(t, os.WriteFile(filepath.Join(dir, "projects", projectID, "config.toml"), []byte("version = 1\nname = \"widgets\"\nupstream = \"https://github.com/acme/widgets\"\nclone = \"/x\"\n"), 0o600))
	checks := Run(context.Background(), o)
	expect(t, find(t, checks, "project "+projectID), Fail, "upstream")
	absent(t, checks, "widgets clone")
}

func TestToolchainFollowsConfiguration(t *testing.T) {
	o, dir := fixture(t)
	cfg := "workspaces = \"jujutsu\"\n" + fmt.Sprintf(topLevel, projectID) + `[profiles.backup]
agent = "codex"
model = "gpt"
[profiles.main]
agent = "claude"
model = "opus"
fallback = "backup"
[roles.mason]
profile = "main"
sandbox = "container"
image = "alpine"
[roles.reviewer]
sandbox = "sbx"
`
	must(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0o600))
	h := host{missing: map[string]bool{"codex": true, "dagger": true}}
	o.LookPath = h.lookPath
	o.CheckJJ = func(context.Context) (string, error) { return "", errors.New("jj is missing") }
	checks := Run(context.Background(), o)
	expect(t, find(t, checks, "codex"), Fail, "not on PATH")
	expect(t, find(t, checks, "claude"), Pass, "")
	expect(t, find(t, checks, "docker"), Pass, "")
	expect(t, find(t, checks, "sbx"), Pass, "")
	expect(t, find(t, checks, "dagger"), Warn, "not on PATH")
	expect(t, find(t, checks, "jj"), Fail, "jj is missing")

	must(t, os.WriteFile(filepath.Join(dir, "config.toml"), fmt.Appendf(nil, topLevel, projectID), 0o600))
	checks = Run(context.Background(), o)
	expect(t, find(t, checks, "jj"), Warn, "Git worktrees")
	absent(t, checks, "codex")
}

func TestRunningServiceLeavesStateToIt(t *testing.T) {
	o, dir := fixture(t)
	lock, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_CREATE|os.O_RDWR, 0o600)
	must(t, err)
	defer lock.Close()
	must(t, syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
	l, err := net.Listen("unix", filepath.Join(dir, "osmia.sock"))
	must(t, err)
	defer l.Close()
	checks := Run(context.Background(), o)
	expect(t, find(t, checks, "ownership lock"), Warn, "a service owns this root")
	expect(t, find(t, checks, "socket"), Pass, "the running service listens")
	absent(t, checks, "widgets trace")
	absent(t, checks, "runtime.json")
	if Failed(checks) {
		t.Fatalf("a running service is a failure: %+v", checks)
	}
}

func TestSocket(t *testing.T) {
	o, dir := fixture(t)
	socket := filepath.Join(dir, "osmia.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	must(t, err)
	expect(t, find(t, Run(context.Background(), o), "socket"), Fail, "does not own the root")
	l.SetUnlinkOnClose(false)
	l.Close()
	expect(t, find(t, Run(context.Background(), o), "socket"), Pass, "stale")
	must(t, os.Remove(socket))
	expect(t, find(t, Run(context.Background(), o), "socket"), Pass, "free")
}

func TestServiceLog(t *testing.T) {
	o, dir := fixture(t)
	must(t, os.WriteFile(filepath.Join(dir, "service.log"), nil, 0o644))
	must(t, os.Chmod(filepath.Join(dir, "service.log"), 0o644))
	expect(t, find(t, Run(context.Background(), o), "service log"), Warn, "not a private regular file")
}

func TestProjectRemotes(t *testing.T) {
	o, dir := fixture(t)
	clone := filepath.Join(filepath.Dir(dir), "clone")
	git(t, clone, "remote", "remove", "origin")
	checks := Run(context.Background(), o)
	expect(t, find(t, checks, "widgets upstream"), Fail, "no remote whose URL names acme/widgets")

	git(t, clone, "remote", "add", "origin", filepath.Join(filepath.Dir(dir), "remotes", "acme", "widgets"))
	project := fmt.Sprintf("version = 1\nname = \"widgets\"\nupstream = \"acme/widgets\"\nfork = \"me/widgets\"\nbase_branch = \"trunk\"\nclone = %q\n", clone)
	must(t, os.WriteFile(filepath.Join(dir, "projects", projectID, "config.toml"), []byte(project), 0o600))
	checks = Run(context.Background(), o)
	expect(t, find(t, checks, "widgets upstream"), Fail, "no branch trunk")
	expect(t, find(t, checks, "widgets fork"), Fail, "no remote whose URL names me/widgets")
	expect(t, find(t, checks, "widgets me/widgets"), Fail, "HTTP 404")

	must(t, os.RemoveAll(clone))
	expect(t, find(t, Run(context.Background(), o), "widgets clone"), Fail, "not a directory")
}

func TestGitHubToken(t *testing.T) {
	o, _ := fixture(t)
	o.Token = ""
	checks := Run(context.Background(), o)
	expect(t, find(t, checks, "GITHUB_TOKEN"), Warn, "not set")
	absent(t, checks, "widgets acme/widgets")
	o.Token = "bad"
	expect(t, find(t, Run(context.Background(), o), "GITHUB_TOKEN"), Fail, "HTTP 401")
}

func TestUnopenableState(t *testing.T) {
	o, dir := fixture(t)
	must(t, os.WriteFile(filepath.Join(dir, "projects", projectID, "project.json"), []byte("{}"), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "runtime.json"), []byte("not json"), 0o600))
	checks := Run(context.Background(), o)
	expect(t, find(t, checks, "widgets trace"), Fail, "")
	expect(t, find(t, checks, "runtime.json"), Fail, "runtime.json")

	must(t, os.Remove(filepath.Join(dir, "projects", projectID, "project.json")))
	expect(t, find(t, Run(context.Background(), o), "widgets trace"), Warn, "no trace")
}
