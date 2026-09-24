package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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

func TestChangedPathsUsesRecordedCommits(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	base := git(t, "-C", f.clone, "rev-parse", "HEAD")
	first := f.advance(t, "first.txt")
	second := f.advance(t, "second.txt")
	if _, err := f.provider.Fetch(ctx, "upstream", "main"); err != nil {
		t.Fatal(err)
	}
	paths, err := f.provider.ChangedPaths(ctx, base, first)
	if err != nil || !slices.Equal(paths, []string{"first.txt"}) {
		t.Fatalf("paths %v: %v; later commit %s", paths, err, second)
	}
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

func TestPushMovesTheForkBranchOnlyFromTheExpectedCommit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	base := git(t, "-C", f.clone, "rev-parse", "HEAD")
	first := f.advance(t, "first.txt")
	second := f.advance(t, "second.txt")
	if _, err := f.provider.Fetch(ctx, "upstream", "main"); err != nil {
		t.Fatal(err)
	}
	if commit, exists, err := f.provider.RemoteBranch(ctx, "fork", "osmia/w"); err != nil || exists || commit != "" {
		t.Fatalf("an absent branch reads %q %v %v", commit, exists, err)
	}
	if err := f.provider.Push(ctx, "fork", first, "osmia/w", base); err == nil {
		t.Fatal("pushed over an absent branch expected at a commit")
	}
	if err := f.provider.Push(ctx, "fork", first, "osmia/w", ""); err != nil {
		t.Fatal(err)
	}
	if commit, exists, err := f.provider.RemoteBranch(ctx, "fork", "osmia/w"); err != nil || !exists || commit != first {
		t.Fatalf("the pushed branch reads %q %v %v, want %s", commit, exists, err, first)
	}
	if err := f.provider.Push(ctx, "fork", second, "osmia/w", ""); err == nil {
		t.Fatal("pushed over an existing branch expected absent")
	}
	if err := f.provider.Push(ctx, "fork", base, "osmia/w", second); err == nil {
		t.Fatal("pushed over a branch expected at another commit")
	}
	if commit, _, err := f.provider.RemoteBranch(ctx, "fork", "osmia/w"); err != nil || commit != first {
		t.Fatalf("a refused push moved the branch to %q: %v", commit, err)
	}
	if err := f.provider.Push(ctx, "fork", base, "osmia/w", first); err != nil {
		t.Fatalf("a push from the expected commit that rewinds the branch: %v", err)
	}
	if commit, _, err := f.provider.RemoteBranch(ctx, "fork", "osmia/w"); err != nil || commit != base {
		t.Fatalf("the branch is at %q, want %s: %v", commit, base, err)
	}
	if _, _, err := f.provider.RemoteBranch(ctx, "nowhere", "osmia/w"); err == nil {
		t.Fatal("asked a remote the clone does not have")
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

// A snapshot commits the worktree's whole tree on top of its commit: changes,
// deletions and untracked files, not what the repository ignores nor what the
// clone's own excludes file names. The worktree's branch moves to it, and a
// worktree with nothing new is its own snapshot.
func TestSnapshotCommitsExactlyTheWorktreeOnTopOfItsBase(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	base := git(t, "-C", f.scratch, "rev-parse", "HEAD")
	git(t, "-C", f.clone, "branch", "osmia/w1", base)
	acquired, err := f.provider.Acquire(ctx, vcs.Request{Name: "w1/u1", Ref: "osmia/w1", Branch: "osmia-unit/w1/u1"})
	if err != nil {
		t.Fatal(err)
	}
	ws := acquired.(Worktree)
	excludes := filepath.Join(t.TempDir(), "excludes")
	put := func(name, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(ws.Path, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws.Path, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(excludes, []byte("mine\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, "-C", f.clone, "config", "core.excludesFile", excludes)
	put("README", "widgets, changed\n")
	put("src/added.go", "package src\n")
	put(".gitignore", "build/\n")
	put("build/out", "ignored\n")
	put("mine", "the owner's excludes do not apply\n")
	changes := func(from, to string) string {
		t.Helper()
		return git(t, "-C", f.clone, "diff-tree", "-r", "--name-status", "--no-renames", from, to)
	}
	first, err := f.provider.Snapshot(ctx, ws, "osmia/w1")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := changes(base, first), "A\t.gitignore\nM\tREADME\nA\tmine\nA\tsrc/added.go"; got != want {
		t.Fatalf("the snapshot's changes:\n%s\nwant\n%s", got, want)
	}
	if got := git(t, "-C", f.clone, "log", "-1", "--format=%P %an <%ae> %cn <%ce>", first); got != base+" Osmia <osmia@localhost> Osmia <osmia@localhost>" {
		t.Fatalf("the snapshot's parent and identity: %s", got)
	}
	if descends, err := f.provider.Ancestor(ctx, "osmia/w1", first); err != nil || !descends {
		t.Fatalf("the snapshot descends from the feature branch: %v %v", descends, err)
	}
	if tip, _, err := f.provider.Branch(ctx, "osmia-unit/w1/u1"); err != nil || tip != first {
		t.Fatalf("the unit branch is at %s, %v; want %s", tip, err, first)
	}
	if status := git(t, "-C", ws.Path, "status", "--porcelain"); status != "" {
		t.Fatalf("the worktree after its snapshot:\n%s", status)
	}
	if data, err := os.ReadFile(filepath.Join(ws.Path, "build/out")); err != nil || string(data) != "ignored\n" {
		t.Fatalf("the ignored file after the snapshot: %q %v", data, err)
	}
	again, err := f.provider.Snapshot(ctx, ws, "osmia/w1")
	if err != nil || again != first {
		t.Fatalf("a snapshot of an unchanged worktree: %s %v; want %s", again, err, first)
	}
	if err := os.Remove(filepath.Join(ws.Path, "src/added.go")); err != nil {
		t.Fatal(err)
	}
	second, err := f.provider.Snapshot(ctx, ws, base)
	if err != nil {
		t.Fatal(err)
	}
	if got := changes(first, second); got != "D\tsrc/added.go" {
		t.Fatalf("the second snapshot's changes:\n%s", got)
	}
	if parent := git(t, "-C", f.clone, "rev-parse", second+"^"); parent != first {
		t.Fatalf("the second snapshot's parent is %s, want %s", parent, first)
	}
}

// A worktree that does not descend from the base it is snapshotted against
// is refused before anything is committed.
func TestSnapshotRefusesAWorktreeThatDoesNotDescendFromItsBase(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	base := git(t, "-C", f.scratch, "rev-parse", "HEAD")
	moved := f.advance(t, "next")
	if _, err := f.provider.Fetch(ctx, "upstream", "main"); err != nil {
		t.Fatal(err)
	}
	acquired, err := f.provider.Acquire(ctx, vcs.Request{Name: "w1/u1", Ref: base, Branch: "osmia-unit/w1/u1"})
	if err != nil {
		t.Fatal(err)
	}
	ws := acquired.(Worktree)
	if err := os.WriteFile(filepath.Join(ws.Path, "added"), []byte("x\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.provider.Snapshot(ctx, ws, moved); err == nil || !strings.Contains(err.Error(), "at "+base+", which does not descend from "+moved) {
		t.Fatalf("a snapshot against a base the worktree does not descend from: %v", err)
	}
	if tip, _, err := f.provider.Branch(ctx, "osmia-unit/w1/u1"); err != nil || tip != base {
		t.Fatalf("the branch after a refused snapshot is at %s, %v", tip, err)
	}
	if status := git(t, "-C", ws.Path, "status", "--porcelain"); status != "?? added" {
		t.Fatalf("the worktree after a refused snapshot:\n%s", status)
	}
	if _, err := f.provider.Snapshot(ctx, Worktree{}, base); err == nil || !strings.Contains(err.Error(), "no workspace to snapshot") {
		t.Fatalf("a snapshot of no workspace: %v", err)
	}
}

// A squash of a candidate that took two snapshots is one commit of its tree
// on the base, made the same way twice. Advance moves the feature worktree's
// branch, index and files to it once, and refuses a worktree at any other
// commit.
func TestSquashAndAdvanceLandOneCommit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	base := git(t, "-C", f.scratch, "rev-parse", "HEAD")
	git(t, "-C", f.clone, "branch", "osmia/w1", base)
	feature, err := f.provider.Acquire(ctx, vcs.Request{Name: "feature-w1", Branch: "osmia/w1"})
	if err != nil {
		t.Fatal(err)
	}
	acquired, err := f.provider.Acquire(ctx, vcs.Request{Name: "w1/u1", Ref: "osmia/w1", Branch: "osmia-unit/w1/u1"})
	if err != nil {
		t.Fatal(err)
	}
	unit := acquired.(Worktree)
	for _, name := range []string{"first.go", "second.go"} {
		if err := os.WriteFile(filepath.Join(unit.Path, name), []byte(name+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := f.provider.Snapshot(ctx, unit, base); err != nil {
			t.Fatal(err)
		}
	}
	candidate := git(t, "-C", f.clone, "rev-parse", "osmia-unit/w1/u1")
	at := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	message := "Uploads resume\n\nOsmia-Operation: op-1"
	commit, err := f.provider.Squash(ctx, base, candidate, message, at)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.provider.Squash(ctx, base, candidate, message, at)
	if err != nil || again != commit {
		t.Fatalf("a second squash made %s, %v; want %s", again, err, commit)
	}
	got, err := f.provider.Commit(ctx, commit)
	if err != nil {
		t.Fatal(err)
	}
	if want := git(t, "-C", f.clone, "rev-parse", candidate+"^{tree}"); got.Tree != want || !slices.Equal(got.Parents, []string{base}) || got.Message != message {
		t.Fatalf("the squash %+v, want tree %s on %s with %q", got, want, base, message)
	}
	if who := git(t, "-C", f.clone, "log", "-1", "--format=%an <%ae> %cn <%ce> %at", commit); who != "Osmia <osmia@localhost> Osmia <osmia@localhost> "+fmt.Sprint(at.Unix()) {
		t.Fatalf("the squash's identity and date: %s", who)
	}
	if tip, _, err := f.provider.Branch(ctx, "osmia/w1"); err != nil || tip != base {
		t.Fatalf("a squash moved the feature branch to %s, %v", tip, err)
	}
	w := feature.(Worktree)
	if err := f.provider.Advance(ctx, w, candidate, commit); err == nil || !strings.Contains(err.Error(), "is at "+base+", not "+candidate) {
		t.Fatalf("an advance from another commit: %v", err)
	}
	for range 2 {
		if err := f.provider.Advance(ctx, w, base, commit); err != nil {
			t.Fatal(err)
		}
	}
	if tip, _, err := f.provider.Branch(ctx, "osmia/w1"); err != nil || tip != commit {
		t.Fatalf("the feature branch is at %s, %v; want %s", tip, err, commit)
	}
	if count := git(t, "-C", f.clone, "rev-list", "--count", base+"..osmia/w1"); count != "1" {
		t.Fatalf("%s commits landed", count)
	}
	if status := git(t, "-C", w.Path, "status", "--porcelain"); status != "" {
		t.Fatalf("the feature worktree after the advance:\n%s", status)
	}
	if data, err := os.ReadFile(filepath.Join(w.Path, "second.go")); err != nil || string(data) != "second.go\n" {
		t.Fatalf("the feature worktree's files: %q %v", data, err)
	}
	if _, err := f.provider.Squash(ctx, candidate, base, message, at); err == nil || !strings.Contains(err.Error(), "does not descend from") {
		t.Fatalf("a squash of a candidate not on its base: %v", err)
	}
}

// rebaseFixture is a feature branch osmia/w1 on base with a worktree, and a
// unit worktree created from it; each is a Worktree of the fixture's provider.
func rebaseFixture(t *testing.T, f fixture) (base string, feature, unit Worktree) {
	t.Helper()
	ctx := context.Background()
	base = git(t, "-C", f.scratch, "rev-parse", "HEAD")
	git(t, "-C", f.clone, "branch", "osmia/w1", base)
	acquired, err := f.provider.Acquire(ctx, vcs.Request{Name: "feature-w1", Branch: "osmia/w1"})
	if err != nil {
		t.Fatal(err)
	}
	feature = acquired.(Worktree)
	acquired, err = f.provider.Acquire(ctx, vcs.Request{Name: "w1/u1", Ref: "osmia/w1", Branch: "osmia-unit/w1/u1"})
	if err != nil {
		t.Fatal(err)
	}
	return base, feature, acquired.(Worktree)
}

// commitFiles writes files into the worktree and snapshots it on base.
func (f fixture) commitFiles(t *testing.T, w Worktree, base string, files map[string]string) string {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(w.Path, name)
		if content == "" {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	commit, err := f.provider.Snapshot(context.Background(), w, base)
	if err != nil {
		t.Fatal(err)
	}
	return commit
}

// A rebase merges the change a unit's snapshot holds since its base onto the
// new feature branch tip as one commit on that tip, made the same way every
// time, and moves nothing; Move then puts the unit's branch, index and files
// on it, and completes a move interrupted after the branch moved.
func TestRebaseMergesTheUnitsChangeOntoTheNewTip(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	base, feature, unit := rebaseFixture(t, f)
	landed := f.commitFiles(t, feature, base, map[string]string{"landed.go": "landed\n"})
	snapshot := f.commitFiles(t, unit, base, map[string]string{"unit.go": "unit\n"})
	if err := os.WriteFile(filepath.Join(unit.Path, "build.log"), []byte("ignored\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.clone, ".git", "info", "exclude"), []byte("build.log\n"), 0600); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	message := "Rebase unit u1\n\nOsmia-Operation: op-1"
	commit, conflicts, err := f.provider.Rebase(ctx, landed, snapshot, message, at)
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("rebase %s %v: %v", commit, conflicts, err)
	}
	again, _, err := f.provider.Rebase(ctx, landed, snapshot, message, at)
	if err != nil || again != commit {
		t.Fatalf("a second rebase made %s, %v; want %s", again, err, commit)
	}
	got, err := f.provider.Commit(ctx, commit)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Parents, []string{landed}) || got.Message != message {
		t.Fatalf("the rebased commit %+v", got)
	}
	if files := git(t, "-C", f.clone, "ls-tree", "-r", "--name-only", commit); files != "README\nlanded.go\nunit.go" {
		t.Fatalf("the rebased tree holds %q", files)
	}
	if who := git(t, "-C", f.clone, "log", "-1", "--format=%an <%ae> %at", commit); who != "Osmia <osmia@localhost> "+fmt.Sprint(at.Unix()) {
		t.Fatalf("the rebase's identity and date: %s", who)
	}
	if tip, _, err := f.provider.Branch(ctx, "osmia-unit/w1/u1"); err != nil || tip != snapshot {
		t.Fatalf("a rebase moved the unit branch to %s, %v", tip, err)
	}

	if err := f.provider.Move(ctx, unit, landed, commit); err == nil || !strings.Contains(err.Error(), "is at "+snapshot+", not "+landed) {
		t.Fatalf("a move from another commit: %v", err)
	}
	if err := f.provider.Move(ctx, unit, snapshot, commit); err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		if head := git(t, "-C", unit.Path, "rev-parse", "HEAD"); head != commit {
			t.Fatalf("the unit worktree is at %s, not %s", head, commit)
		}
		if tip, _, err := f.provider.Branch(ctx, "osmia-unit/w1/u1"); err != nil || tip != commit {
			t.Fatalf("the unit branch is at %s, %v", tip, err)
		}
		if status := git(t, "-C", unit.Path, "status", "--porcelain"); status != "" {
			t.Fatalf("the unit worktree after the move:\n%s", status)
		}
		for name, want := range map[string]string{"landed.go": "landed\n", "unit.go": "unit\n", "build.log": "ignored\n"} {
			if data, err := os.ReadFile(filepath.Join(unit.Path, name)); err != nil || string(data) != want {
				t.Fatalf("%s holds %q, %v", name, data, err)
			}
		}
	}
	check()
	if err := f.provider.Move(ctx, unit, snapshot, commit); err != nil {
		t.Fatal(err)
	}
	check()
	// Interrupted after the branch moved and before the files did.
	git(t, "-C", unit.Path, "reset", "--hard", "--quiet", snapshot)
	git(t, "-C", unit.Path, "update-ref", "HEAD", commit)
	if err := f.provider.Move(ctx, unit, snapshot, commit); err != nil {
		t.Fatal(err)
	}
	check()
}

// A rebase whose sides both changed a file, or one changed what the other
// deleted, still makes its commit, with the conflicted paths: a changed file
// carries conflict markers, and a deleted one is kept as the side that
// changed it. Markers finds the conflicted files still carrying markers.
func TestRebaseReportsConflictsAndMarkersFindsThem(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	base, feature, unit := rebaseFixture(t, f)
	base = f.commitFiles(t, feature, base, map[string]string{"gone.txt": "one\n"})
	git(t, "-C", unit.Path, "reset", "--hard", "--quiet", base)
	landed := f.commitFiles(t, feature, base, map[string]string{"README": "feature\n", "gone.txt": ""})
	snapshot := f.commitFiles(t, unit, base, map[string]string{"README": "unit\n", "gone.txt": "two\n", "clean.go": "clean\n"})
	commit, conflicts, err := f.provider.Rebase(ctx, landed, snapshot, "Rebase unit u1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(conflicts, []string{"README", "gone.txt"}) {
		t.Fatalf("conflicts %v", conflicts)
	}
	if parents := git(t, "-C", f.clone, "log", "-1", "--format=%P", commit); parents != landed {
		t.Fatalf("the conflicted commit's parents %s", parents)
	}
	readme := git(t, "-C", f.clone, "show", commit+":README")
	if !strings.HasPrefix(readme, "<<<<<<< "+landed+"\nfeature\n=======\nunit\n>>>>>>> "+snapshot) {
		t.Fatalf("the conflicted README:\n%s", readme)
	}
	if kept := git(t, "-C", f.clone, "show", commit+":gone.txt"); kept != "two" {
		t.Fatalf("the changed file the feature deleted holds %q", kept)
	}
	marked, err := f.provider.Markers(ctx, commit, []string{"README", "gone.txt", "missing.txt"})
	if err != nil || !slices.Equal(marked, []string{"README"}) {
		t.Fatalf("markers %v: %v", marked, err)
	}
	if err := f.provider.Move(ctx, unit, snapshot, commit); err != nil {
		t.Fatal(err)
	}
	resolved := f.commitFiles(t, unit, landed, map[string]string{"README": "feature and unit\n<<<<<<<< not a marker\n"})
	if marked, err := f.provider.Markers(ctx, resolved, conflicts); err != nil || len(marked) != 0 {
		t.Fatalf("markers after resolving %v: %v", marked, err)
	}
}

// replayLeftovers fails the test when Replay left its temporary worktree.
func (f fixture) replayLeftovers(t *testing.T) {
	t.Helper()
	if list := git(t, "-C", f.clone, "worktree", "list", "--porcelain"); strings.Contains(list, replayName) {
		t.Fatalf("the replay worktree is still listed:\n%s", list)
	}
	if _, err := os.Lstat(filepath.Join(f.provider.Directory, replayName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the replay worktree's directory: %v", err)
	}
}

// A replay applies each commit of the branch onto the new upstream commit in
// order, keeping each message and author, even where a later commit changes
// what an earlier one added. It is made the same way every time, moves no
// branch and leaves no worktree behind; a head already on upstream is
// returned as it is.
func TestReplayAppliesEachCommitOntoUpstream(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	base, feature, _ := rebaseFixture(t, f)
	first := f.commitFiles(t, feature, base, map[string]string{"a.go": "a\n"})
	second := f.commitFiles(t, feature, first, map[string]string{"a.go": "a2\n", "b.go": "b\n"})
	upstream := f.advance(t, "upstream.txt")
	if _, err := f.provider.Fetch(ctx, "upstream", "main"); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	commit, conflicts, err := f.provider.Replay(ctx, second, upstream, at)
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("replay %s %v: %v", commit, conflicts, err)
	}
	f.replayLeftovers(t)
	again, _, err := f.provider.Replay(ctx, second, upstream, at)
	if err != nil || again != commit {
		t.Fatalf("a second replay made %s, %v; want %s", again, err, commit)
	}
	if replayed := git(t, "-C", f.clone, "rev-list", "--reverse", upstream+".."+commit); len(strings.Fields(replayed)) != 2 {
		t.Fatalf("the replay holds %q on upstream", replayed)
	}
	if parent := git(t, "-C", f.clone, "rev-parse", commit+"~2"); parent != upstream {
		t.Fatalf("the replay starts from %s, not upstream %s", parent, upstream)
	}
	for i, original := range []string{second, first} {
		replayed := fmt.Sprintf("%s~%d", commit, i)
		for _, format := range []string{"%B", "%an <%ae> %at"} {
			if got, want := git(t, "-C", f.clone, "log", "-1", "--format="+format, replayed), git(t, "-C", f.clone, "log", "-1", "--format="+format, original); got != want {
				t.Fatalf("replayed %s %s is %q, original %q", replayed, format, got, want)
			}
		}
		if committed := git(t, "-C", f.clone, "log", "-1", "--format=%cn <%ce> %ct", replayed); committed != "Osmia <osmia@localhost> "+fmt.Sprint(at.Unix()) {
			t.Fatalf("replayed %s committed by %s", replayed, committed)
		}
	}
	if a := git(t, "-C", f.clone, "show", commit+":a.go"); a != "a2" {
		t.Fatalf("a.go holds %q", a)
	}
	if files := git(t, "-C", f.clone, "ls-tree", "-r", "--name-only", commit); files != "README\na.go\nb.go\nupstream.txt" {
		t.Fatalf("the replayed tree holds %q", files)
	}
	if tip, _, err := f.provider.Branch(ctx, "osmia/w1"); err != nil || tip != second {
		t.Fatalf("a replay moved the feature branch to %s, %v", tip, err)
	}
	if same, conflicts, err := f.provider.Replay(ctx, commit, upstream, at.Add(time.Hour)); err != nil || same != commit || len(conflicts) != 0 {
		t.Fatalf("a head on upstream replayed to %s %v: %v", same, conflicts, err)
	}
}

// A replay whose commit conflicts with upstream returns the conflicted paths
// and no commit, moves nothing and leaves no worktree or replay state behind,
// so the next replay starts clean.
func TestReplayReportsConflictsAndLeavesNothing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	base, feature, _ := rebaseFixture(t, f)
	head := f.commitFiles(t, feature, base, map[string]string{"README": "feature\n", "clean.go": "clean\n"})
	if err := os.WriteFile(filepath.Join(f.scratch, "README"), []byte("upstream\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, "-C", f.scratch, "commit", "--quiet", "-am", "upstream README")
	git(t, "-C", f.scratch, "push", "--quiet", "origin", "main")
	upstream, err := f.provider.Fetch(ctx, "upstream", "main")
	if err != nil {
		t.Fatal(err)
	}
	commit, conflicts, err := f.provider.Replay(ctx, head, upstream, time.Now())
	if err != nil || commit != "" || !slices.Equal(conflicts, []string{"README"}) {
		t.Fatalf("replay %q %v: %v", commit, conflicts, err)
	}
	f.replayLeftovers(t)
	if tip, _, err := f.provider.Branch(ctx, "osmia/w1"); err != nil || tip != head {
		t.Fatalf("a conflicted replay moved the feature branch to %s, %v", tip, err)
	}
	if _, conflicts, err := f.provider.Replay(ctx, head, upstream, time.Now()); err != nil || !slices.Equal(conflicts, []string{"README"}) {
		t.Fatalf("a second replay %v: %v", conflicts, err)
	}
}

// Export writes a commit's tracked regular files, with their executable
// bits, and leaves out symbolic links and the repository's metadata.
func TestExportWritesTheCommitsTrackedFiles(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	base, feature, _ := rebaseFixture(t, f)
	if err := os.MkdirAll(filepath.Join(feature.Path, "cmd"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(feature.Path, "cmd", "run.sh"), []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("README", filepath.Join(feature.Path, "link")); err != nil {
		t.Fatal(err)
	}
	commit := f.commitFiles(t, feature, base, map[string]string{"main.go": "package main\n"})
	if err := os.WriteFile(filepath.Join(feature.Path, "main.go"), []byte("uncommitted\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "branch")
	if err := f.provider.Export(ctx, commit, dir); err != nil {
		t.Fatal(err)
	}
	var files []string
	if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			rel, _ := filepath.Rel(dir, path)
			files = append(files, filepath.ToSlash(rel))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(files, []string{"README", "cmd/run.sh", "main.go"}) {
		t.Fatalf("exported %v", files)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "main.go")); err != nil || string(data) != "package main\n" {
		t.Fatalf("main.go holds %q, %v", data, err)
	}
	if info, err := os.Stat(filepath.Join(dir, "cmd", "run.sh")); err != nil || info.Mode().Perm()&0100 == 0 {
		t.Fatalf("run.sh lost its executable bit: %v %v", info, err)
	}
}

// A replay in a worktree stops at each commit that conflicts with the
// conflict markers in the worktree, reports the stop and its paths, and goes
// on from the worktree's resolved files, stop after stop, to a branch whose
// commits keep their messages and authors on upstream. The feature branch
// the worktree was made from does not move.
func TestReplayInStopsAtEachConflictAndContinuesFromTheResolvedFiles(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	base, feature, _ := rebaseFixture(t, f)
	first := f.commitFiles(t, feature, base, map[string]string{"README": "feature\n", "clean.go": "clean\n"})
	second := f.commitFiles(t, feature, first, map[string]string{"NOTES": "feature notes\n"})
	if err := os.WriteFile(filepath.Join(f.scratch, "README"), []byte("upstream\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.scratch, "NOTES"), []byte("upstream notes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, "-C", f.scratch, "add", "README", "NOTES")
	git(t, "-C", f.scratch, "commit", "--quiet", "-m", "upstream README and NOTES")
	git(t, "-C", f.scratch, "push", "--quiet", "origin", "main")
	upstream, err := f.provider.Fetch(ctx, "upstream", "main")
	if err != nil {
		t.Fatal(err)
	}
	acquired, err := f.provider.Acquire(ctx, vcs.Request{Name: "drift/w1", Ref: second, Branch: "osmia-drift/w1/1"})
	if err != nil {
		t.Fatal(err)
	}
	w := acquired.(Worktree)
	if _, _, replaying, err := f.provider.Replaying(ctx, w); err != nil || replaying {
		t.Fatalf("a fresh worktree is replaying: %t %v", replaying, err)
	}
	at := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	commit, conflicts, err := f.provider.ReplayIn(ctx, w, upstream, at)
	if err != nil || commit != "" || !slices.Equal(conflicts, []string{"README"}) {
		t.Fatalf("replay %q %v: %v", commit, conflicts, err)
	}
	stop, unmerged, replaying, err := f.provider.Replaying(ctx, w)
	if err != nil || !replaying || stop != first || !slices.Equal(unmerged, []string{"README"}) {
		t.Fatalf("the first stop %s %v %t: %v", stop, unmerged, replaying, err)
	}
	if stopped, found, err := f.provider.Workspace(ctx, "drift/w1"); err != nil || !found || stopped.Branch != "osmia-drift/w1/1" {
		t.Fatalf("the stopped worktree %+v %t: %v", stopped, found, err)
	}
	if again, err := f.provider.Acquire(ctx, vcs.Request{Name: "drift/w1", Ref: second, Branch: "osmia-drift/w1/1"}); err != nil || again.Directory() != w.Path {
		t.Fatalf("the stopped worktree acquired again %+v: %v", again, err)
	}
	if marked, err := f.provider.MarkedFiles(w, []string{"README", "clean.go", "missing"}); err != nil || !slices.Equal(marked, []string{"README"}) {
		t.Fatalf("marked files %v: %v", marked, err)
	}
	if err := os.WriteFile(filepath.Join(w.Path, "README"), []byte("upstream and feature\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if marked, err := f.provider.MarkedFiles(w, []string{"README"}); err != nil || len(marked) != 0 {
		t.Fatalf("a resolved file is marked %v: %v", marked, err)
	}
	commit, conflicts, err = f.provider.ContinueReplay(ctx, w, at)
	if err != nil || commit != "" || !slices.Equal(conflicts, []string{"NOTES"}) {
		t.Fatalf("continue %q %v: %v", commit, conflicts, err)
	}
	if stop, _, _, err := f.provider.Replaying(ctx, w); err != nil || stop != second {
		t.Fatalf("the second stop %s, want %s: %v", stop, second, err)
	}
	if err := os.WriteFile(filepath.Join(w.Path, "NOTES"), []byte("upstream and feature notes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	commit, conflicts, err = f.provider.ContinueReplay(ctx, w, at)
	if err != nil || commit == "" || len(conflicts) != 0 {
		t.Fatalf("the last continue %q %v: %v", commit, conflicts, err)
	}
	if _, _, replaying, err := f.provider.Replaying(ctx, w); err != nil || replaying {
		t.Fatalf("a finished replay is replaying: %t %v", replaying, err)
	}
	if tip, _, err := f.provider.Branch(ctx, "osmia-drift/w1/1"); err != nil || tip != commit {
		t.Fatalf("the worktree's branch is at %s, not %s: %v", tip, commit, err)
	}
	if parent := git(t, "-C", f.clone, "rev-parse", commit+"~2"); parent != upstream {
		t.Fatalf("the replay starts from %s, not upstream %s", parent, upstream)
	}
	for i, original := range []string{second, first} {
		replayed := fmt.Sprintf("%s~%d", commit, i)
		if got, want := git(t, "-C", f.clone, "log", "-1", "--format=%B%an", replayed), git(t, "-C", f.clone, "log", "-1", "--format=%B%an", original); got != want {
			t.Fatalf("replayed %s is %q, original %q", replayed, got, want)
		}
	}
	for path, want := range map[string]string{"README": "upstream and feature", "NOTES": "upstream and feature notes", "clean.go": "clean"} {
		if got := git(t, "-C", f.clone, "show", commit+":"+path); got != want {
			t.Fatalf("%s holds %q, want %q", path, got, want)
		}
	}
	if tip, _, err := f.provider.Branch(ctx, "osmia/w1"); err != nil || tip != second {
		t.Fatalf("the replay moved the feature branch to %s, %v", tip, err)
	}
}
