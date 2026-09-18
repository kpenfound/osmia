package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/core/vcs"
)

func git(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_AUTHOR_NAME=Owner", "GIT_AUTHOR_EMAIL=owner@example.invalid", "GIT_COMMITTER_NAME=Owner", "GIT_COMMITTER_EMAIL=owner@example.invalid"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// fixture is a clone of a fork with a local bare upstream as a second remote,
// and a scratch clone of upstream to move it with.
type fixture struct {
	clone, upstream, scratch string
	provider                 *Git
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	home, err := os.MkdirTemp("", "ws-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	f := fixture{clone: filepath.Join(home, "clone"), upstream: filepath.Join(home, "remotes", "acme", "widgets.git"), scratch: filepath.Join(home, "scratch")}
	if err := os.MkdirAll(filepath.Join(home, "remotes", "acme"), 0700); err != nil {
		t.Fatal(err)
	}
	git(t, "init", "--quiet", "-b", "main", f.scratch)
	if err := os.WriteFile(filepath.Join(f.scratch, "README"), []byte("widgets\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, "-C", f.scratch, "add", "README")
	git(t, "-C", f.scratch, "commit", "--quiet", "-m", "base")
	git(t, "clone", "--quiet", "--bare", f.scratch, f.upstream)
	git(t, "clone", "--quiet", f.upstream, filepath.Join(home, "remotes", "owner", "widgets.git"))
	git(t, "clone", "--quiet", "--origin", "fork", filepath.Join(home, "remotes", "owner", "widgets.git"), f.clone)
	git(t, "-C", f.clone, "remote", "add", "upstream", f.upstream)
	git(t, "-C", f.scratch, "remote", "add", "origin", f.upstream)
	f.provider = &Git{Clone: f.clone, Directory: filepath.Join(home, "branches")}
	return f
}

// advance commits on upstream main and returns the new commit.
func (f fixture) advance(t *testing.T, name string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.scratch, name), []byte(name+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, "-C", f.scratch, "add", name)
	git(t, "-C", f.scratch, "commit", "--quiet", "-m", name)
	git(t, "-C", f.scratch, "push", "--quiet", "origin", "main")
	return git(t, "-C", f.scratch, "rev-parse", "HEAD")
}

func TestRemoteIsTheOneWhoseURLNamesTheRepository(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	remote, err := f.provider.Remote(ctx, "acme/widgets")
	if err != nil || remote != "upstream" {
		t.Fatalf("remote %q, %v", remote, err)
	}
	remote, err = f.provider.Remote(ctx, "owner/widgets")
	if err != nil || remote != "fork" {
		t.Fatalf("remote %q, %v", remote, err)
	}
	if _, err := f.provider.Remote(ctx, "acme/gadgets"); err == nil || !strings.Contains(err.Error(), "no remote whose URL names acme/gadgets") {
		t.Fatalf("a repository no remote names: %v", err)
	}
	for url, want := range map[string]bool{
		"https://github.com/acme/widgets.git": true,
		"https://github.com/Acme/Widgets":     true,
		"git@github.com:acme/widgets.git":     true,
		"ssh://git@github.com/acme/widgets":   true,
		"/srv/git/acme/widgets.git/":          true,
		"https://github.com/acme/widgets-old": false,
		"https://github.com/acme/gadgets.git": false,
		"https://github.com/me/acme/widgets":  true,
		"https://github.com/acmewidgets":      false,
	} {
		if got := names(url, "acme/widgets"); got != want {
			t.Errorf("names(%q) = %v, want %v", url, got, want)
		}
	}
}

func TestFetchReturnsTheUpstreamCommit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	base := git(t, "-C", f.scratch, "rev-parse", "HEAD")
	commit, err := f.provider.Fetch(ctx, "upstream", "main")
	if err != nil || commit != base {
		t.Fatalf("fetched %q, %v; want %s", commit, err, base)
	}
	moved := f.advance(t, "next")
	commit, err = f.provider.Fetch(ctx, "upstream", "main")
	if err != nil || commit != moved {
		t.Fatalf("fetched %q, %v; want %s", commit, err, moved)
	}
	if got := git(t, "-C", f.clone, "rev-parse", "refs/remotes/upstream/main"); got != moved {
		t.Fatalf("the remote-tracking ref is %s, want %s", got, moved)
	}
	if _, err := f.provider.Fetch(ctx, "upstream", "release"); err == nil {
		t.Fatal("fetching a branch upstream does not have")
	}
	var failure *Error
	if _, err := f.provider.Fetch(ctx, "nowhere", "main"); !errors.As(err, &failure) || failure.Args[0] != "fetch" || failure.Stderr == "" {
		t.Fatalf("fetching from a remote the clone does not have: %v", err)
	}
}

func TestAcquireCreatesTheBranchAndTheWorktreeOnce(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	base := git(t, "-C", f.scratch, "rev-parse", "HEAD")
	if _, found, err := f.provider.Branch(ctx, "osmia/w1"); err != nil || found {
		t.Fatalf("branch before: %v %v", found, err)
	}
	if _, found, err := f.provider.Workspace(ctx, "w1"); err != nil || found {
		t.Fatalf("workspace before: %v %v", found, err)
	}
	ws, err := f.provider.Acquire(ctx, vcs.Request{Name: "w1", Ref: base, Branch: "osmia/w1"})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(f.provider.Directory, "w1")
	if ws.Directory() != want || ws.VCS() == nil || ws.VCS().Mounts[0] != filepath.Join(f.clone, ".git") {
		t.Fatalf("workspace %+v", ws)
	}
	if data, err := os.ReadFile(filepath.Join(want, "README")); err != nil || string(data) != "widgets\n" {
		t.Fatalf("the worktree's files: %q %v", data, err)
	}
	tip, found, err := f.provider.Branch(ctx, "osmia/w1")
	if err != nil || !found || tip != base {
		t.Fatalf("branch after: %q %v %v", tip, found, err)
	}
	if got := git(t, "-C", want, "rev-parse", "--abbrev-ref", "HEAD"); got != "osmia/w1" {
		t.Fatalf("the worktree is on %s", got)
	}
	// The same request again creates nothing and returns what is there.
	again, err := f.provider.Acquire(ctx, vcs.Request{Name: "w1", Ref: "0000000000000000000000000000000000000000", Branch: "osmia/w1"})
	if err != nil || again.Directory() != want {
		t.Fatalf("acquiring again: %+v %v", again, err)
	}
	if out := git(t, "-C", f.clone, "worktree", "list", "--porcelain"); strings.Count(out, "worktree ") != 2 {
		t.Fatalf("worktrees:\n%s", out)
	}
	// A worktree on another branch is not the one asked for.
	if _, err := f.provider.Acquire(ctx, vcs.Request{Name: "w1", Ref: base, Branch: "osmia/w2"}); err == nil || !strings.Contains(err.Error(), `is on branch "osmia/w1", not "osmia/w2"`) {
		t.Fatalf("a worktree on another branch: %v", err)
	}
	for _, req := range []vcs.Request{{Ref: base, Branch: "osmia/w3"}, {Name: "w3", Ref: base}} {
		if _, err := f.provider.Acquire(ctx, req); err == nil || !strings.Contains(err.Error(), "needs a name and a branch") {
			t.Fatalf("acquiring %+v: %v", req, err)
		}
	}
	if _, err := f.provider.Acquire(ctx, vcs.Request{Name: "w3", Branch: "osmia/w3"}); err == nil || !strings.Contains(err.Error(), "names no ref to create it from") {
		t.Fatalf("a new branch with no ref: %v", err)
	}
}

// Recovery after an interruption: a branch without its worktree gets the
// worktree on that branch, a worktree whose directory is gone is forgotten
// and made again, and a directory in the way that is no worktree is reported,
// not removed.
func TestAcquireRecoversWhatAnInterruptionLeft(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	base := git(t, "-C", f.scratch, "rev-parse", "HEAD")
	git(t, "-C", f.clone, "branch", "osmia/w1", base)
	ws, err := f.provider.Acquire(ctx, vcs.Request{Name: "w1", Ref: "", Branch: "osmia/w1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := git(t, "-C", ws.Directory(), "rev-parse", "--abbrev-ref", "HEAD"); got != "osmia/w1" {
		t.Fatalf("the worktree is on %s", got)
	}
	// The owner's own worktree whose directory is gone is theirs to prune.
	mine := filepath.Join(filepath.Dir(f.clone), "mine")
	git(t, "-C", f.clone, "worktree", "add", "--quiet", "-b", "mine", mine, base)
	if err := os.RemoveAll(mine); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(ws.Directory()); err != nil {
		t.Fatal(err)
	}
	if _, found, err := f.provider.Workspace(ctx, "w1"); err != nil || found {
		t.Fatalf("a worktree whose directory is gone: %v %v", found, err)
	}
	ws, err = f.provider.Acquire(ctx, vcs.Request{Name: "w1", Ref: base, Branch: "osmia/w1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws.Directory(), "README")); err != nil {
		t.Fatal(err)
	}
	if out := git(t, "-C", f.clone, "worktree", "list", "--porcelain"); !strings.Contains(out, "worktree "+mine+"\n") || !strings.Contains(out, "prunable") {
		t.Fatalf("the owner's worktree was forgotten:\n%s", out)
	}
	if err := f.provider.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if out := git(t, "-C", f.clone, "worktree", "list", "--porcelain"); !strings.Contains(out, "worktree "+mine+"\n") {
		t.Fatalf("Prune forgot the owner's worktree:\n%s", out)
	}
	// Prune forgets the provider's own.
	if err := os.RemoveAll(ws.Directory()); err != nil {
		t.Fatal(err)
	}
	if err := f.provider.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if out := git(t, "-C", f.clone, "worktree", "list", "--porcelain"); strings.Contains(out, "osmia/w1") || !strings.Contains(out, "worktree "+mine+"\n") {
		t.Fatalf("after Prune:\n%s", out)
	}
	stray := filepath.Join(f.provider.Directory, "w2")
	if err := os.MkdirAll(stray, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stray, "keep"), []byte("mine\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.provider.Acquire(ctx, vcs.Request{Name: "w2", Ref: base, Branch: "osmia/w2"}); err == nil || !strings.Contains(err.Error(), stray+" exists and is no workspace of the clone") {
		t.Fatalf("a directory in the way: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stray, "keep")); err != nil {
		t.Fatalf("the directory in the way was touched: %v", err)
	}
}

func TestAncestorAndRelease(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	base := git(t, "-C", f.scratch, "rev-parse", "HEAD")
	moved := f.advance(t, "next")
	if _, err := f.provider.Fetch(ctx, "upstream", "main"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		commit, tip string
		want        bool
	}{{base, moved, true}, {moved, base, false}, {base, base, true}} {
		if got, err := f.provider.Ancestor(ctx, tc.commit, tc.tip); err != nil || got != tc.want {
			t.Fatalf("ancestor(%s, %s) = %v, %v; want %v", tc.commit, tc.tip, got, err, tc.want)
		}
	}
	if _, err := f.provider.Ancestor(ctx, "0000000000000000000000000000000000000000", base); err == nil {
		t.Fatal("an unknown commit is an error, not an answer")
	}
	ws, err := f.provider.Acquire(ctx, vcs.Request{Name: "w1", Ref: base, Branch: "osmia/w1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Directory(), "dirty"), []byte("x\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.provider.Release(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ws.Directory()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the released worktree: %v", err)
	}
	if _, found, err := f.provider.Branch(ctx, "osmia/w1"); err != nil || !found {
		t.Fatalf("the branch after release: %v %v", found, err)
	}
	if err := f.provider.Release(ctx, nil); err == nil {
		t.Fatal("releasing nothing")
	}
}

// SSH runs in batch mode for a fetch unless the owner has an SSH command of
// their own, in the environment or in their Git configuration.
func TestFetchPutsSSHInBatchModeUnlessTheOwnerConfiguredIt(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, name := range []string{"GIT_SSH_COMMAND", "GIT_SSH"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	env, err := f.provider.sshEnvironment(ctx)
	if err != nil || !slices.Equal(env, []string{"GIT_SSH_COMMAND=ssh -o BatchMode=yes"}) {
		t.Fatalf("with nothing configured: %q %v", env, err)
	}
	if _, err := f.provider.Fetch(ctx, "upstream", "main"); err != nil {
		t.Fatalf("a local fetch in batch mode: %v", err)
	}
	git(t, "-C", f.clone, "config", "core.sshCommand", "ssh -i mine")
	if env, err := f.provider.sshEnvironment(ctx); err != nil || env != nil {
		t.Fatalf("with core.sshCommand: %q %v", env, err)
	}
	git(t, "-C", f.clone, "config", "--unset", "core.sshCommand")
	for _, name := range []string{"GIT_SSH_COMMAND", "GIT_SSH"} {
		t.Setenv(name, "ssh -i mine")
		if env, err := f.provider.sshEnvironment(ctx); err != nil || env != nil {
			t.Fatalf("with %s: %q %v", name, env, err)
		}
		os.Unsetenv(name)
	}
}
