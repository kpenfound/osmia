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

// requireJJ skips a test that needs jj where jj is not installed, and fails it
// where OSMIA_REQUIRE_JJ says jj must be there, as in the dagger test
// containers.
func requireJJ(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("jj"); err != nil {
		if os.Getenv("OSMIA_REQUIRE_JJ") != "" {
			t.Fatalf("jj is required but not on PATH: %v", err)
		}
		t.Skip("jj is not installed; the Jujutsu provider's tests need it")
	}
}

// newJujutsuFixture is the fixture's clone with a Jujutsu provider whose
// directory stands where the Osmia root keeps it, beside the clone.
func newJujutsuFixture(t *testing.T) (fixture, *Jujutsu) {
	t.Helper()
	requireJJ(t)
	f := newFixture(t)
	return f, &Jujutsu{Clone: f.clone, Directory: filepath.Join(filepath.Dir(f.clone), "osmia", "branches", "p1")}
}

func acquireJJ(t *testing.T, j *Jujutsu, req vcs.Request) Worktree {
	t.Helper()
	acquired, err := j.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return acquired.(Worktree)
}

// writeFiles writes files into dir, removing those whose content is empty.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if content == "" {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// snapshotJJ writes files into the workspace and snapshots it on base.
func snapshotJJ(t *testing.T, j *Jujutsu, w Worktree, base string, files map[string]string) string {
	t.Helper()
	writeFiles(t, w.Path, files)
	commit, err := j.Snapshot(context.Background(), w, base)
	if err != nil {
		t.Fatal(err)
	}
	return commit
}

// plainCommit reports whether Git reads commit as a commit Git itself would
// write: a tree, parents, an author and a committer, and the message.
func plainCommit(t *testing.T, clone, commit string) {
	t.Helper()
	header, _, _ := strings.Cut(git(t, "-C", clone, "cat-file", "commit", commit), "\n\n")
	for _, line := range strings.Split(header, "\n") {
		if key, _, _ := strings.Cut(line, " "); !slices.Contains([]string{"tree", "parent", "author", "committer"}, key) {
			t.Fatalf("commit %s carries a %q header:\n%s", commit, key, header)
		}
	}
}

// The fake executables are all written before any runs, and the test is not
// parallel, so no process forked meanwhile holds one open for writing while
// it is executed.
func TestCheckJJReportsAMissingOrTooOldJJWithTheVersionFound(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	scripts := map[string]string{"broken": "exit 1", "old": "echo 'jj 0.44.2-0123abcd'", "other": "echo 'hg 6.8'"}
	supported := map[string]string{"jj 0.45.0": "0.45.0", "jj 0.45.1-7c41cdeb16b6b321c64e789a966b6adf723816a5": "0.45.1-7c41cdeb16b6b321c64e789a966b6adf723816a5", "jj 0.46.0": "0.46.0", "jj 1.0.0": "1.0.0"}
	for output := range supported {
		scripts[output] = "echo '" + output + "'"
	}
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := CheckJJ(ctx, filepath.Join(dir, "absent")); !errors.Is(err, ErrJJMissing) {
		t.Fatalf("an absent jj: %v", err)
	}
	if _, err := CheckJJ(ctx, filepath.Join(dir, "broken")); !errors.Is(err, ErrJJMissing) {
		t.Fatalf("a jj that does not run: %v", err)
	}
	version, err := CheckJJ(ctx, filepath.Join(dir, "old"))
	if !errors.Is(err, ErrJJTooOld) || errors.Is(err, ErrJJMissing) || version != "0.44.2-0123abcd" || !strings.Contains(err.Error(), "found jj 0.44.2-0123abcd, Osmia needs "+MinimumJJ+" or later") {
		t.Fatalf("an old jj: %q %v", version, err)
	}
	for output, want := range supported {
		if version, err := CheckJJ(ctx, filepath.Join(dir, output)); err != nil || version != want {
			t.Fatalf("%q: %q %v", output, version, err)
		}
	}
	if _, err := CheckJJ(ctx, filepath.Join(dir, "other")); err == nil || errors.Is(err, ErrJJMissing) || errors.Is(err, ErrJJTooOld) || !strings.Contains(err.Error(), `reports version "hg 6.8"`) {
		t.Fatalf("a binary that is no jj: %v", err)
	}
	if _, err := exec.LookPath("jj"); err == nil {
		if version, err := (&Jujutsu{}).Check(ctx); err != nil {
			t.Fatalf("the installed jj %s: %v", version, err)
		}
	}
}

// The repository lives under the provider's directory on the clone's Git
// store: the clone's checkout gets no .jj, its configuration does not change,
// and the workspace's branch is a branch of the clone.
func TestJujutsuKeepsItsRepositoryOutsideTheClone(t *testing.T) {
	t.Parallel()
	f, j := newJujutsuFixture(t)
	ctx := context.Background()
	base := git(t, "-C", f.scratch, "rev-parse", "HEAD")
	configuration := git(t, "-C", f.clone, "config", "--list", "--local")
	if _, found, err := j.Workspace(ctx, "w1"); err != nil || found {
		t.Fatalf("a workspace before any: %v %v", found, err)
	}
	ws, err := j.Acquire(ctx, vcs.Request{Name: "w1", Ref: base, Branch: "osmia/w1"})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(j.Directory, "w1")
	if ws.Directory() != want || ws.VCS() == nil || !slices.Equal(ws.VCS().Mounts, []string{filepath.Join(j.Directory, ".jujutsu"), filepath.Join(f.clone, ".git")}) {
		t.Fatalf("workspace %+v, mounts %+v", ws, ws.VCS())
	}
	if data, err := os.ReadFile(filepath.Join(want, "README")); err != nil || string(data) != "widgets\n" {
		t.Fatalf("the workspace's files: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(j.Directory, ".jujutsu", ".jj", "repo")); err != nil {
		t.Fatalf("the repository: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.clone, ".jj")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the clone's checkout holds .jj: %v", err)
	}
	if got := git(t, "-C", f.clone, "config", "--list", "--local"); got != configuration {
		t.Fatalf("the clone's configuration changed:\n%s\nwas\n%s", got, configuration)
	}
	if status := git(t, "-C", f.clone, "status", "--porcelain"); status != "" {
		t.Fatalf("the clone's checkout:\n%s", status)
	}
	if out := git(t, "-C", f.clone, "worktree", "list", "--porcelain"); strings.Count(out, "worktree ") != 1 {
		t.Fatalf("worktrees of the clone:\n%s", out)
	}
	if tip := git(t, "-C", f.clone, "rev-parse", "refs/heads/osmia/w1"); tip != base {
		t.Fatalf("the clone's branch is at %s, want %s", tip, base)
	}
	entries, err := os.ReadDir(filepath.Join(j.Directory, ".jujutsu"))
	if err != nil || len(entries) != 1 || entries[0].Name() != ".jj" {
		t.Fatalf("the repository has a checkout of its own: %v %v", entries, err)
	}
}

func TestJujutsuAcquireCreatesTheBranchAndTheWorkspaceOnce(t *testing.T) {
	t.Parallel()
	f, j := newJujutsuFixture(t)
	ctx := context.Background()
	base := git(t, "-C", f.scratch, "rev-parse", "HEAD")
	w := acquireJJ(t, j, vcs.Request{Name: "w1/u1", Ref: base, Branch: "osmia-unit/w1/u1"})
	if w.Path != filepath.Join(j.Directory, "w1", "u1") || w.Branch != "osmia-unit/w1/u1" {
		t.Fatalf("workspace %+v", w)
	}
	if found, ok, err := j.Workspace(ctx, "w1/u1"); err != nil || !ok || found.Path != w.Path || found.Branch != w.Branch {
		t.Fatalf("looked up %+v %v %v", found, ok, err)
	}
	again, err := j.Acquire(ctx, vcs.Request{Name: "w1/u1", Ref: "0000000000000000000000000000000000000000", Branch: "osmia-unit/w1/u1"})
	if err != nil || again.Directory() != w.Path {
		t.Fatalf("acquiring again: %+v %v", again, err)
	}
	if names, err := j.registered(ctx); err != nil || !slices.Equal(names, []string{"w1/u1"}) {
		t.Fatalf("the repository's workspaces %v: %v", names, err)
	}
	if _, err := j.Acquire(ctx, vcs.Request{Name: "w1/u1", Ref: base, Branch: "osmia/w2"}); err == nil || !strings.Contains(err.Error(), `is on branch "osmia-unit/w1/u1", not "osmia/w2"`) {
		t.Fatalf("a workspace on another branch: %v", err)
	}
	for _, req := range []vcs.Request{{Ref: base, Branch: "osmia/w3"}, {Name: "w3", Ref: base}} {
		if _, err := j.Acquire(ctx, req); err == nil || !strings.Contains(err.Error(), "needs a name and a branch") {
			t.Fatalf("acquiring %+v: %v", req, err)
		}
	}
	if _, err := j.Acquire(ctx, vcs.Request{Name: "w3", Branch: "osmia/w3"}); err == nil || !strings.Contains(err.Error(), "names no ref to create it from") {
		t.Fatalf("a new branch with no ref: %v", err)
	}
	if _, err := j.Acquire(ctx, vcs.Request{Name: "w4", Ref: base, Branch: "osmia/bad..name"}); err == nil {
		t.Fatal("a branch Git refuses to name")
	}
	if _, found, err := j.Branch(ctx, "osmia/bad..name"); err != nil || found {
		t.Fatalf("the refused branch: %v %v", found, err)
	}
}

// Recovery after an interruption: an interrupted repository is made again, a
// branch without its workspace gets the workspace on that branch, a workspace
// whose directory is gone or whose making stopped before its branch was
// recorded is forgotten and made again, and a directory in the way that is no
// workspace is reported, not removed.
func TestJujutsuAcquireRecoversWhatAnInterruptionLeft(t *testing.T) {
	t.Parallel()
	f, j := newJujutsuFixture(t)
	ctx := context.Background()
	base := git(t, "-C", f.scratch, "rev-parse", "HEAD")
	writeFiles(t, filepath.Join(j.Directory, ".jujutsu.new"), map[string]string{"left": "by an interrupted initialisation\n"})
	git(t, "-C", f.clone, "branch", "osmia/w1", base)
	w := acquireJJ(t, j, vcs.Request{Name: "w1", Branch: "osmia/w1"})
	if _, err := os.Stat(filepath.Join(j.Directory, ".jujutsu.new")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the interrupted initialisation is left: %v", err)
	}
	if err := os.RemoveAll(w.Path); err != nil {
		t.Fatal(err)
	}
	if _, found, err := j.Workspace(ctx, "w1"); err != nil || found {
		t.Fatalf("a workspace whose directory is gone: %v %v", found, err)
	}
	if names, err := j.registered(ctx); err != nil || len(names) != 0 {
		t.Fatalf("the gone workspace is still there: %v %v", names, err)
	}
	w = acquireJJ(t, j, vcs.Request{Name: "w1", Ref: base, Branch: "osmia/w1"})
	if err := os.Remove(filepath.Join(w.Path, ".jj", metadataName)); err != nil {
		t.Fatal(err)
	}
	if _, found, err := j.Workspace(ctx, "w1"); err != nil || found {
		t.Fatalf("a workspace whose branch was never recorded: %v %v", found, err)
	}
	if _, err := os.Stat(w.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the unfinished workspace's directory: %v", err)
	}
	w = acquireJJ(t, j, vcs.Request{Name: "w1", Ref: base, Branch: "osmia/w1"})
	if _, err := os.Stat(filepath.Join(w.Path, "README")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(w.Path); err != nil {
		t.Fatal(err)
	}
	if err := j.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if names, err := j.registered(ctx); err != nil || len(names) != 0 {
		t.Fatalf("after Prune: %v %v", names, err)
	}
	stray := filepath.Join(j.Directory, "w2")
	writeFiles(t, stray, map[string]string{"keep": "mine\n"})
	if _, err := j.Acquire(ctx, vcs.Request{Name: "w2", Ref: base, Branch: "osmia/w2"}); err == nil || !strings.Contains(err.Error(), stray+" exists and is no workspace of the clone") {
		t.Fatalf("a directory in the way: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stray, "keep")); err != nil {
		t.Fatalf("the directory in the way was touched: %v", err)
	}
	if err := j.Release(ctx, vcs.Directory(stray)); err == nil || !strings.Contains(err.Error(), "is no workspace of the clone") {
		t.Fatalf("releasing a directory in the way: %v", err)
	}
	w = acquireJJ(t, j, vcs.Request{Name: "w3", Ref: base, Branch: "osmia/w3"})
	writeFiles(t, w.Path, map[string]string{"scratch": "dropped with the workspace\n"})
	if err := j.Release(ctx, w); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(w.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a released workspace's directory: %v", err)
	}
	if _, found, err := j.Workspace(ctx, "w3"); err != nil || found {
		t.Fatalf("a released workspace: %v %v", found, err)
	}
	if tip, found, err := j.Branch(ctx, "osmia/w3"); err != nil || !found || tip != base {
		t.Fatalf("a released workspace's branch: %s %v %v", tip, found, err)
	}
	// A workspace whose making stopped before the repository registered it
	// is made again.
	w = acquireJJ(t, j, vcs.Request{Name: "w4", Ref: base, Branch: "osmia/w4"})
	if _, err := j.run(ctx, j.repository(), nil, "--ignore-working-copy", "workspace", "forget", "w4"); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, w.Path, map[string]string{"half": "left by the interruption\n"})
	if _, found, err := j.Workspace(ctx, "w4"); err != nil || found {
		t.Fatalf("an unregistered workspace: %v %v", found, err)
	}
	w = acquireJJ(t, j, vcs.Request{Name: "w4", Ref: base, Branch: "osmia/w4"})
	if names, err := j.registered(ctx); err != nil || !slices.Contains(names, "w4") {
		t.Fatalf("the workspace made again is not registered: %v %v", names, err)
	}
	if _, err := os.Stat(filepath.Join(w.Path, "half")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("what the interruption left: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(w.Path, "README")); err != nil || string(data) != "widgets\n" {
		t.Fatalf("the workspace made again: %q %v", data, err)
	}
}

// A snapshot commits the workspace's whole tree on top of its branch's
// commit: changes, deletions and untracked files, not what the repository
// ignores nor what the clone's own excludes file names. The branch moves to
// it, Git reads it as a plain commit, and a workspace with nothing new is
// its own snapshot.
func TestJujutsuSnapshotCommitsExactlyTheWorkspaceOnTopOfItsBase(t *testing.T) {
	t.Parallel()
	f, j := newJujutsuFixture(t)
	ctx := context.Background()
	base := git(t, "-C", f.scratch, "rev-parse", "HEAD")
	git(t, "-C", f.clone, "branch", "osmia/w1", base)
	ws := acquireJJ(t, j, vcs.Request{Name: "w1/u1", Ref: "osmia/w1", Branch: "osmia-unit/w1/u1"})
	excludes := filepath.Join(t.TempDir(), "excludes")
	if err := os.WriteFile(excludes, []byte("mine\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, "-C", f.clone, "config", "core.excludesFile", excludes)
	writeFiles(t, ws.Path, map[string]string{
		"README":       "widgets, changed\n",
		"src/added.go": "package src\n",
		".gitignore":   "build/\n",
		"build/out":    "ignored\n",
		"mine":         "the owner's excludes do not apply\n",
	})
	changes := func(from, to string) string {
		t.Helper()
		return git(t, "-C", f.clone, "diff-tree", "-r", "--name-status", "--no-renames", from, to)
	}
	first, err := j.Snapshot(ctx, ws, "osmia/w1")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := changes(base, first), "A\t.gitignore\nM\tREADME\nA\tmine\nA\tsrc/added.go"; got != want {
		t.Fatalf("the snapshot's changes:\n%s\nwant\n%s", got, want)
	}
	if got := git(t, "-C", f.clone, "log", "-1", "--format=%P %an <%ae> %cn <%ce> %B", first); got != base+" Osmia <osmia@localhost> Osmia <osmia@localhost> Snapshot of osmia-unit/w1/u1" {
		t.Fatalf("the snapshot's parent, identity and message: %s", got)
	}
	plainCommit(t, f.clone, first)
	if tip, _, err := j.Branch(ctx, "osmia-unit/w1/u1"); err != nil || tip != first {
		t.Fatalf("the unit branch is at %s, %v; want %s", tip, err, first)
	}
	if data, err := os.ReadFile(filepath.Join(ws.Path, "build/out")); err != nil || string(data) != "ignored\n" {
		t.Fatalf("the ignored file after the snapshot: %q %v", data, err)
	}
	again, err := j.Snapshot(ctx, ws, "osmia/w1")
	if err != nil || again != first {
		t.Fatalf("a snapshot of an unchanged workspace: %s %v; want %s", again, err, first)
	}
	writeFiles(t, ws.Path, map[string]string{"src/added.go": ""})
	second, err := j.Snapshot(ctx, ws, base)
	if err != nil {
		t.Fatal(err)
	}
	if got := changes(first, second); got != "D\tsrc/added.go" {
		t.Fatalf("the second snapshot's changes:\n%s", got)
	}
	if parent := git(t, "-C", f.clone, "rev-parse", second+"^"); parent != first {
		t.Fatalf("the second snapshot's parent is %s, want %s", parent, first)
	}
	// A snapshot committed before an interruption kept the branch from
	// moving is completed with what the workspace gained since.
	writeFiles(t, ws.Path, map[string]string{"interrupted": "committed\n"})
	if _, err := j.run(ctx, ws.Path, nil, "commit", "--message", "Snapshot of osmia-unit/w1/u1"); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, ws.Path, map[string]string{"later": "written after the interruption\n"})
	third, err := j.Snapshot(ctx, ws, base)
	if err != nil {
		t.Fatal(err)
	}
	if got := changes(second, third); got != "A\tinterrupted\nA\tlater" {
		t.Fatalf("the completed snapshot's changes:\n%s", got)
	}
	if parent := git(t, "-C", f.clone, "rev-parse", third+"^"); parent != second {
		t.Fatalf("the completed snapshot's parent is %s, want %s", parent, second)
	}
	if tip, _, err := j.Branch(ctx, "osmia-unit/w1/u1"); err != nil || tip != third {
		t.Fatalf("the unit branch is at %s, %v; want %s", tip, err, third)
	}
	if again, err := j.Snapshot(ctx, ws, base); err != nil || again != third {
		t.Fatalf("a snapshot after the completed one: %s %v; want %s", again, err, third)
	}
	// A commit in the workspace that is no snapshot of its branch is not
	// taken for one.
	writeFiles(t, ws.Path, map[string]string{"other": "committed by someone else\n"})
	if _, err := j.run(ctx, ws.Path, nil, "commit", "--message", "Something else"); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Snapshot(ctx, ws, base); err == nil || !strings.Contains(err.Error(), "is not on its branch osmia-unit/w1/u1 at "+third) {
		t.Fatalf("a snapshot of a workspace off its branch: %v", err)
	}
	if tip, _, err := j.Branch(ctx, "osmia-unit/w1/u1"); err != nil || tip != third {
		t.Fatalf("a refused snapshot moved the unit branch to %s, %v", tip, err)
	}
}

// A workspace that does not descend from the base it is snapshotted against
// is refused before anything is committed.
func TestJujutsuSnapshotRefusesAWorkspaceThatDoesNotDescendFromItsBase(t *testing.T) {
	t.Parallel()
	f, j := newJujutsuFixture(t)
	ctx := context.Background()
	base := git(t, "-C", f.scratch, "rev-parse", "HEAD")
	moved := f.advance(t, "next")
	if _, err := j.Fetch(ctx, "upstream", "main"); err != nil {
		t.Fatal(err)
	}
	ws := acquireJJ(t, j, vcs.Request{Name: "w1/u1", Ref: base, Branch: "osmia-unit/w1/u1"})
	writeFiles(t, ws.Path, map[string]string{"added": "x\n"})
	if _, err := j.Snapshot(ctx, ws, moved); err == nil || !strings.Contains(err.Error(), "at "+base+", which does not descend from "+moved) {
		t.Fatalf("a snapshot against a base the workspace does not descend from: %v", err)
	}
	if tip, _, err := j.Branch(ctx, "osmia-unit/w1/u1"); err != nil || tip != base {
		t.Fatalf("the branch after a refused snapshot is at %s, %v", tip, err)
	}
	if data, err := os.ReadFile(filepath.Join(ws.Path, "added")); err != nil || string(data) != "x\n" {
		t.Fatalf("the workspace after a refused snapshot: %q %v", data, err)
	}
	if _, err := j.Snapshot(ctx, Worktree{}, base); err == nil || !strings.Contains(err.Error(), "no workspace to snapshot") {
		t.Fatalf("a snapshot of no workspace: %v", err)
	}
}

// A unit worked in a Jujutsu workspace lands as the very commit the same work
// in a Git worktree lands as, and the feature branch pushed to the fork is a
// plain Git branch whatever backend made it. Advance moves the feature
// workspace's branch and files to the landed commit once, and refuses a
// workspace at any other commit.
func TestJujutsuLandsAndPushesTheCommitGitWould(t *testing.T) {
	t.Parallel()
	f, j := newJujutsuFixture(t)
	ctx := context.Background()
	base := git(t, "-C", f.scratch, "rev-parse", "HEAD")
	git(t, "-C", f.clone, "branch", "osmia/w1", base)
	feature := acquireJJ(t, j, vcs.Request{Name: "w1", Branch: "osmia/w1"})
	unit := acquireJJ(t, j, vcs.Request{Name: "w1/u1", Ref: "osmia/w1", Branch: "osmia-unit/w1/u1"})
	acquired, err := f.provider.Acquire(ctx, vcs.Request{Name: "w1/u1", Ref: base, Branch: "osmia-git/w1/u1"})
	if err != nil {
		t.Fatal(err)
	}
	gitUnit := acquired.(Worktree)
	for _, name := range []string{"first.go", "second.go"} {
		snapshotJJ(t, j, unit, base, map[string]string{name: name + "\n"})
		f.commitFiles(t, gitUnit, base, map[string]string{name: name + "\n"})
	}
	candidate, _, _ := j.Branch(ctx, "osmia-unit/w1/u1")
	gitCandidate, _, _ := f.provider.Branch(ctx, "osmia-git/w1/u1")
	at := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	message := "Uploads resume\n\nOsmia-Operation: op-1"
	commit, err := j.Squash(ctx, base, candidate, message, at)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := j.Squash(ctx, base, candidate, message, at); err != nil || again != commit {
		t.Fatalf("a second squash made %s, %v; want %s", again, err, commit)
	}
	if byGit, err := f.provider.Squash(ctx, base, gitCandidate, message, at); err != nil || byGit != commit {
		t.Fatalf("the same unit landed from a Git worktree as %s, %v; from Jujutsu as %s", byGit, err, commit)
	}
	got, err := j.Commit(ctx, commit)
	if err != nil || !slices.Equal(got.Parents, []string{base}) || got.Message != message {
		t.Fatalf("the squash %+v: %v", got, err)
	}
	if err := j.Advance(ctx, feature, candidate, commit); err == nil || !strings.Contains(err.Error(), "is at "+base+", not "+candidate) {
		t.Fatalf("an advance from another commit: %v", err)
	}
	writeFiles(t, feature.Path, map[string]string{"pending": "mine\n"})
	for range 2 {
		if err := j.Advance(ctx, feature, base, commit); err != nil {
			t.Fatal(err)
		}
	}
	if tip, _, err := j.Branch(ctx, "osmia/w1"); err != nil || tip != commit {
		t.Fatalf("the feature branch is at %s, %v; want %s", tip, err, commit)
	}
	if data, err := os.ReadFile(filepath.Join(feature.Path, "second.go")); err != nil || string(data) != "second.go\n" {
		t.Fatalf("the feature workspace's files: %q %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(feature.Path, "pending")); err != nil || string(data) != "mine\n" {
		t.Fatalf("the feature workspace's own file after the advance: %q %v", data, err)
	}
	if current, err := j.describe(ctx, feature.Path, true, "@"); err != nil || !slices.Equal(current.parents, []string{commit}) || current.empty {
		t.Fatalf("the feature workspace's own change stands on %+v: %v", current, err)
	}
	if err := j.Push(ctx, "fork", commit, "osmia/w1", ""); err != nil {
		t.Fatal(err)
	}
	fork := filepath.Join(filepath.Dir(f.clone), "remotes", "owner", "widgets.git")
	if pushed := git(t, "-C", fork, "rev-parse", "refs/heads/osmia/w1"); pushed != commit {
		t.Fatalf("the fork's branch is at %s, want %s", pushed, commit)
	}
	if remote, found, err := j.RemoteBranch(ctx, "fork", "osmia/w1"); err != nil || !found || remote != commit {
		t.Fatalf("the fork's branch as the provider reads it: %s %v %v", remote, found, err)
	}
	if err := j.Push(ctx, "fork", base, "osmia/w1", base); err == nil {
		t.Fatal("a push from a commit the fork's branch is not at")
	}
	if log := git(t, "-C", fork, "log", "--format=%an <%ae>|%B", base+"..osmia/w1"); log != "Osmia <osmia@localhost>|"+message {
		t.Fatalf("the pushed branch's history:\n%s", log)
	}
}

// A rebase of a unit's snapshot onto a new feature branch tip makes the
// commit Git's does; Move puts the unit's branch and files on it, completes a
// move interrupted after the branch moved, and refuses a workspace at any
// other commit. A rebase that conflicts is Jujutsu's commit on the new tip
// holding the conflict stored, which a workspace on it materializes with
// markers: a snapshot that leaves them keeps the conflict, and one of the
// resolved files holds none.
func TestJujutsuRebaseAndMoveTheUnitsWorkspace(t *testing.T) {
	t.Parallel()
	f, j := newJujutsuFixture(t)
	ctx := context.Background()
	base := git(t, "-C", f.scratch, "rev-parse", "HEAD")
	git(t, "-C", f.clone, "branch", "osmia/w1", base)
	feature := acquireJJ(t, j, vcs.Request{Name: "w1", Branch: "osmia/w1"})
	unit := acquireJJ(t, j, vcs.Request{Name: "w1/u1", Ref: "osmia/w1", Branch: "osmia-unit/w1/u1"})
	snapshot := snapshotJJ(t, j, unit, base, map[string]string{"unit.go": "unit\n", "README": "widgets by the unit\n"})
	tip := snapshotJJ(t, j, feature, base, map[string]string{"landed.go": "landed\n"})
	at := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	commit, conflicts, err := j.Rebase(ctx, tip, snapshot, "Rebase unit u1", at)
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("rebase %s %v: %v", commit, conflicts, err)
	}
	if byGit, _, err := f.provider.Rebase(ctx, tip, snapshot, "Rebase unit u1", at); err != nil || byGit != commit {
		t.Fatalf("Git's rebase made %s, %v; want %s", byGit, err, commit)
	}
	writeFiles(t, unit.Path, map[string]string{"stray": "dropped by the move\n"})
	if err := j.Move(ctx, unit, tip, commit); err == nil || !strings.Contains(err.Error(), "is at "+snapshot+", not "+tip) {
		t.Fatalf("a move from another commit: %v", err)
	}
	if err := j.Move(ctx, unit, snapshot, commit); err != nil {
		t.Fatal(err)
	}
	if head, _, err := j.Branch(ctx, "osmia-unit/w1/u1"); err != nil || head != commit {
		t.Fatalf("the unit branch is at %s, %v; want %s", head, err, commit)
	}
	for path, want := range map[string]string{"unit.go": "unit\n", "landed.go": "landed\n", "README": "widgets by the unit\n"} {
		if data, err := os.ReadFile(filepath.Join(unit.Path, path)); err != nil || string(data) != want {
			t.Fatalf("%s after the move: %q %v", path, data, err)
		}
	}
	if _, err := os.Stat(filepath.Join(unit.Path, "stray")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a file of the workspace's own after the move: %v", err)
	}
	if again, err := j.Snapshot(ctx, unit, tip); err != nil || again != commit {
		t.Fatalf("a snapshot after the move: %s %v; want %s", again, err, commit)
	}
	// A move interrupted after the branch moved completes.
	conflicting := snapshotJJ(t, j, feature, tip, map[string]string{"README": "widgets by the feature\n"})
	conflicted, conflicts, err := j.Rebase(ctx, conflicting, commit, "Rebase unit u1 again", at)
	if err != nil || !slices.Equal(conflicts, []string{"README"}) {
		t.Fatalf("a conflicted rebase %s %v: %v", conflicted, conflicts, err)
	}
	if stored, err := j.StoredConflicts(ctx, conflicted); err != nil || !slices.Equal(stored, []string{"README"}) {
		t.Fatalf("the conflicted rebase stores %v: %v", stored, err)
	}
	if c, err := j.Commit(ctx, conflicted); err != nil || !slices.Equal(c.Parents, []string{conflicting}) || c.Message != "Rebase unit u1 again" {
		t.Fatalf("the conflicted rebase %+v: %v", c, err)
	}
	byGit, byGitConflicts, err := f.provider.Rebase(ctx, conflicting, commit, "Rebase unit u1 again", at)
	if err != nil || !slices.Equal(byGitConflicts, []string{"README"}) {
		t.Fatalf("Git's conflicted rebase %s %v: %v", byGit, byGitConflicts, err)
	}
	if stored, err := f.provider.StoredConflicts(ctx, byGit); err != nil || len(stored) != 0 {
		t.Fatalf("Git's conflicted rebase stores %v: %v", stored, err)
	}
	for _, clean := range []string{commit, byGit} {
		if stored, err := j.StoredConflicts(ctx, clean); err != nil || len(stored) != 0 {
			t.Fatalf("commit %s stores %v: %v", clean, stored, err)
		}
	}
	git(t, "-C", f.clone, "update-ref", "refs/heads/osmia-unit/w1/u1", conflicted, commit)
	if err := j.Move(ctx, unit, commit, conflicted); err != nil {
		t.Fatal(err)
	}
	if marked, err := j.MarkedFiles(unit, []string{"README", "unit.go", "missing"}); err != nil || !slices.Equal(marked, []string{"README"}) {
		t.Fatalf("marked files %v: %v", marked, err)
	}
	data, err := os.ReadFile(filepath.Join(unit.Path, "README"))
	if err != nil {
		t.Fatal(err)
	}
	var sides []string
	for _, line := range strings.Split(string(data), "\n") {
		for _, marker := range []string{"<<<<<<< ", "||||||| ", "=======", ">>>>>>> "} {
			if strings.HasPrefix(line, marker) {
				sides = append(sides, strings.TrimSpace(marker))
			}
		}
		if strings.HasPrefix(line, "widgets") {
			sides = append(sides, line)
		}
	}
	if want := []string{"<<<<<<<", "widgets by the feature", "|||||||", "widgets", "=======", "widgets by the unit", ">>>>>>>"}; !slices.Equal(sides, want) {
		t.Fatalf("the conflicted README reads %v, want %v:\n%s", sides, want, data)
	}
	kept := snapshotJJ(t, j, unit, conflicting, map[string]string{"unit.go": "unit, kept\n"})
	if stored, err := j.StoredConflicts(ctx, kept); err != nil || !slices.Equal(stored, []string{"README"}) {
		t.Fatalf("a snapshot with the markers left stores %v: %v", stored, err)
	}
	// A rebase of a commit that holds the conflict keeps it stored.
	moved := snapshotJJ(t, j, feature, conflicting, map[string]string{"landed2.go": "landed again\n"})
	again, conflicts, err := j.Rebase(ctx, moved, kept, "Rebase unit u1 onto the moved tip", at)
	if err != nil || !slices.Equal(conflicts, []string{"README"}) {
		t.Fatalf("a rebase of the conflicted snapshot %s %v: %v", again, conflicts, err)
	}
	if stored, err := j.StoredConflicts(ctx, again); err != nil || !slices.Equal(stored, []string{"README"}) {
		t.Fatalf("the rebased conflicted snapshot stores %v: %v", stored, err)
	}
	listed, err := j.run(ctx, j.repository(), nil, "--ignore-working-copy", "file", "list", "--revision", again, "--template", `path ++ "\n"`)
	if err != nil || !slices.Equal(strings.Fields(listed), []string{"README", "landed.go", "landed2.go", "unit.go"}) {
		t.Fatalf("the rebased conflicted snapshot holds %q: %v", listed, err)
	}
	resolved := snapshotJJ(t, j, unit, conflicting, map[string]string{"README": "widgets by the feature and the unit\n"})
	if stored, err := j.StoredConflicts(ctx, resolved); err != nil || len(stored) != 0 {
		t.Fatalf("the resolved snapshot stores %v: %v", stored, err)
	}
	if marked, err := j.Markers(ctx, resolved, []string{"README", "unit.go"}); err != nil || len(marked) != 0 {
		t.Fatalf("the resolved snapshot's markers %v: %v", marked, err)
	}
	if diff, err := j.Diff(ctx, conflicting, resolved); err != nil || strings.Contains(diff, "jjconflict") || !strings.Contains(diff, "+widgets by the feature and the unit") {
		t.Fatalf("the resolved candidate's diff, %v:\n%s", err, diff)
	}
}

// A replay in a workspace stops at each commit that conflicts with the
// conflict markers in the workspace, reports the stop and its paths as they
// were given, keeps a path whose markers are left conflicted, and goes on
// from the workspace's resolved files, stop after stop, to a branch whose
// commits keep their messages and authors on upstream and are committed by
// Osmia at the given time. A resolution that leaves a commit's change empty
// drops the commit. The feature branch the workspace was made from does not
// move.
func TestJujutsuReplayInStopsAtEachConflictAndContinuesFromTheResolvedFiles(t *testing.T) {
	t.Parallel()
	f, j := newJujutsuFixture(t)
	ctx := context.Background()
	base := git(t, "-C", f.scratch, "rev-parse", "HEAD")
	git(t, "-C", f.clone, "branch", "osmia/w1", base)
	feature := acquireJJ(t, j, vcs.Request{Name: "w1", Branch: "osmia/w1"})
	first := snapshotJJ(t, j, feature, base, map[string]string{"README": "feature\n", "clean.go": "clean\n"})
	second := snapshotJJ(t, j, feature, first, map[string]string{"NOTES": "feature notes\n"})
	third := snapshotJJ(t, j, feature, second, map[string]string{"LICENSE": "feature license\n"})
	writeFiles(t, f.scratch, map[string]string{"README": "upstream\n", "NOTES": "upstream notes\n", "LICENSE": "upstream license\n"})
	git(t, "-C", f.scratch, "add", "README", "NOTES", "LICENSE")
	git(t, "-C", f.scratch, "commit", "--quiet", "-m", "upstream README, NOTES and LICENSE")
	git(t, "-C", f.scratch, "push", "--quiet", "origin", "main")
	upstream, err := j.Fetch(ctx, "upstream", "main")
	if err != nil {
		t.Fatal(err)
	}
	w := acquireJJ(t, j, vcs.Request{Name: "drift/w1", Ref: third, Branch: "osmia-drift/w1/1"})
	if _, _, replaying, err := j.Replaying(ctx, w); err != nil || replaying {
		t.Fatalf("a fresh workspace is replaying: %t %v", replaying, err)
	}
	at := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	commit, conflicts, err := j.ReplayIn(ctx, w, upstream, at)
	if err != nil || commit != "" || !slices.Equal(conflicts, []string{"README"}) {
		t.Fatalf("replay %q %v: %v", commit, conflicts, err)
	}
	stop, unmerged, replaying, err := j.Replaying(ctx, w)
	if err != nil || !replaying || stop != first || !slices.Equal(unmerged, []string{"README"}) {
		t.Fatalf("the first stop %s %v %t: %v", stop, unmerged, replaying, err)
	}
	if _, _, err := j.ReplayIn(ctx, w, upstream, at); err == nil || !strings.Contains(err.Error(), "replaying already") {
		t.Fatalf("a replay while one is in progress: %v", err)
	}
	if stopped, found, err := j.Workspace(ctx, "drift/w1"); err != nil || !found || stopped.Branch != "osmia-drift/w1/1" {
		t.Fatalf("the stopped workspace %+v %t: %v", stopped, found, err)
	}
	if again, err := j.Acquire(ctx, vcs.Request{Name: "drift/w1", Ref: third, Branch: "osmia-drift/w1/1"}); err != nil || again.Directory() != w.Path {
		t.Fatalf("the stopped workspace acquired again %+v: %v", again, err)
	}
	if tip, _, err := j.Branch(ctx, "osmia-drift/w1/1"); err != nil || tip != third {
		t.Fatalf("a stopped replay moved its branch to %s, %v", tip, err)
	}
	if marked, err := j.MarkedFiles(w, []string{"README", "clean.go", "missing"}); err != nil || !slices.Equal(marked, []string{"README"}) {
		t.Fatalf("marked files %v: %v", marked, err)
	}
	if data, err := os.ReadFile(filepath.Join(w.Path, "README")); err != nil || !strings.Contains(string(data), "\n=======\nfeature\n>>>>>>> ") || strings.Contains(string(data), "%%%%%%%") {
		t.Fatalf("the conflicted file does not hold Git's markers: %q %v", data, err)
	}
	commit, conflicts, err = j.ContinueReplay(ctx, w, at)
	if err != nil || commit != "" || !slices.Equal(conflicts, []string{"README"}) {
		t.Fatalf("continuing with the markers left %q %v: %v", commit, conflicts, err)
	}
	writeFiles(t, w.Path, map[string]string{"README": "upstream and feature\n"})
	if stop, unmerged, _, err := j.Replaying(ctx, w); err != nil || stop != first || !slices.Equal(unmerged, []string{"README"}) {
		t.Fatalf("the stop with its files resolved %s %v: %v", stop, unmerged, err)
	}
	commit, conflicts, err = j.ContinueReplay(ctx, w, at)
	if err != nil || commit != "" || !slices.Equal(conflicts, []string{"NOTES"}) {
		t.Fatalf("continue %q %v: %v", commit, conflicts, err)
	}
	if stop, _, _, err := j.Replaying(ctx, w); err != nil || stop != second {
		t.Fatalf("the second stop %s, want %s: %v", stop, second, err)
	}
	writeFiles(t, w.Path, map[string]string{"NOTES": "upstream notes\n"})
	commit, conflicts, err = j.ContinueReplay(ctx, w, at)
	if err != nil || commit != "" || !slices.Equal(conflicts, []string{"LICENSE"}) {
		t.Fatalf("continue %q %v: %v", commit, conflicts, err)
	}
	if stop, _, _, err := j.Replaying(ctx, w); err != nil || stop != third {
		t.Fatalf("the third stop %s, want %s: %v", stop, third, err)
	}
	writeFiles(t, w.Path, map[string]string{"LICENSE": "upstream and feature license\n"})
	interrupted, err := readMetadata(w.Path)
	if err != nil {
		t.Fatal(err)
	}
	commit, conflicts, err = j.ContinueReplay(ctx, w, at)
	if err != nil || commit == "" || len(conflicts) != 0 {
		t.Fatalf("the last continue %q %v: %v", commit, conflicts, err)
	}
	// A replay interrupted after its branch moved, before it was recorded
	// as finished, finishes on the same commit.
	if err := writeMetadata(w.Path, interrupted); err != nil {
		t.Fatal(err)
	}
	if stop, unmerged, replaying, err := j.Replaying(ctx, w); err != nil || !replaying || stop != "" || len(unmerged) != 0 {
		t.Fatalf("the interrupted replay %s %v %t: %v", stop, unmerged, replaying, err)
	}
	if again, conflicts, err := j.ContinueReplay(ctx, w, at); err != nil || again != commit || len(conflicts) != 0 {
		t.Fatalf("finishing the interrupted replay %q %v: %v; want %s", again, conflicts, err, commit)
	}
	if _, _, replaying, err := j.Replaying(ctx, w); err != nil || replaying {
		t.Fatalf("a finished replay is replaying: %t %v", replaying, err)
	}
	if tip, _, err := j.Branch(ctx, "osmia-drift/w1/1"); err != nil || tip != commit {
		t.Fatalf("the workspace's branch is at %s, not %s: %v", tip, commit, err)
	}
	if parent := git(t, "-C", f.clone, "rev-parse", commit+"~2"); parent != upstream {
		t.Fatalf("the replay starts from %s, not upstream %s; the emptied commit was kept", parent, upstream)
	}
	for i, original := range []string{third, first} {
		replayed := fmt.Sprintf("%s~%d", commit, i)
		if got, want := git(t, "-C", f.clone, "log", "-1", "--format=%B%an <%ae> %at", replayed), git(t, "-C", f.clone, "log", "-1", "--format=%B%an <%ae> %at", original); got != want {
			t.Fatalf("replayed %s is %q, original %q", replayed, got, want)
		}
		if got := git(t, "-C", f.clone, "log", "-1", "--format=%cn <%ce> %ct", replayed); got != fmt.Sprintf("Osmia <osmia@localhost> %d", at.Unix()) {
			t.Fatalf("replayed %s was committed by %s", replayed, got)
		}
		plainCommit(t, f.clone, replayed)
	}
	for path, want := range map[string]string{"README": "upstream and feature", "NOTES": "upstream notes", "LICENSE": "upstream and feature license", "clean.go": "clean"} {
		if got := git(t, "-C", f.clone, "show", commit+":"+path); got != want {
			t.Fatalf("%s holds %q, want %q", path, got, want)
		}
	}
	if current, err := j.describe(ctx, w.Path, true, "@"); err != nil || !slices.Equal(current.parents, []string{commit}) || !current.empty {
		t.Fatalf("the workspace after the replay stands on %+v: %v", current, err)
	}
	if tip, _, err := j.Branch(ctx, "osmia/w1"); err != nil || tip != third {
		t.Fatalf("the replay moved the feature branch to %s, %v", tip, err)
	}
	if again, conflicts, err := j.ReplayIn(ctx, w, upstream, at); err != nil || again != commit || len(conflicts) != 0 {
		t.Fatalf("replaying a branch already on upstream %s %v: %v", again, conflicts, err)
	}
}

// A replay that meets no conflict ends in one call. A commit whose change
// upstream already holds is dropped, and one that changed nothing to begin
// with is kept. A workspace holding changes of its own is refused.
func TestJujutsuReplayInDropsWhatUpstreamHolds(t *testing.T) {
	t.Parallel()
	f, j := newJujutsuFixture(t)
	ctx := context.Background()
	base := git(t, "-C", f.scratch, "rev-parse", "HEAD")
	git(t, "-C", f.clone, "branch", "osmia/w1", base)
	feature := acquireJJ(t, j, vcs.Request{Name: "w1", Branch: "osmia/w1"})
	held := snapshotJJ(t, j, feature, base, map[string]string{"same.txt": "same.txt\n"})
	tree := git(t, "-C", f.clone, "rev-parse", held+"^{tree}")
	empty := git(t, "-C", f.clone, "commit-tree", "-p", held, "-m", "nothing changes", tree)
	git(t, "-C", f.clone, "update-ref", "refs/heads/osmia/w1", empty, held)
	if err := j.Move(ctx, feature, held, empty); err != nil {
		t.Fatal(err)
	}
	own := snapshotJJ(t, j, feature, base, map[string]string{"own.txt": "own\n"})
	upstream := f.advance(t, "same.txt")
	if fetched, err := j.Fetch(ctx, "upstream", "main"); err != nil || fetched != upstream {
		t.Fatalf("fetched %s, %v", fetched, err)
	}
	w := acquireJJ(t, j, vcs.Request{Name: "drift/w1", Ref: own, Branch: "osmia-drift/w1/1"})
	writeFiles(t, w.Path, map[string]string{"scratch": "the workspace's own\n"})
	at := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	if _, _, err := j.ReplayIn(ctx, w, upstream, at); err == nil || !strings.Contains(err.Error(), "holds changes its branch osmia-drift/w1/1 does not") {
		t.Fatalf("a replay of a workspace with changes of its own: %v", err)
	}
	writeFiles(t, w.Path, map[string]string{"scratch": ""})
	commit, conflicts, err := j.ReplayIn(ctx, w, upstream, at)
	if err != nil || commit == "" || len(conflicts) != 0 {
		t.Fatalf("replay %q %v: %v", commit, conflicts, err)
	}
	if subjects := git(t, "-C", f.clone, "log", "--format=%s", upstream+".."+commit); subjects != "Snapshot of osmia/w1\nnothing changes" {
		t.Fatalf("the replayed commits:\n%s", subjects)
	}
	if tip, _, err := j.Branch(ctx, "osmia-drift/w1/1"); err != nil || tip != commit {
		t.Fatalf("the workspace's branch is at %s, not %s: %v", tip, commit, err)
	}
	if _, _, replaying, err := j.Replaying(ctx, w); err != nil || replaying {
		t.Fatalf("a finished replay is replaying: %t %v", replaying, err)
	}
	if data, err := os.ReadFile(filepath.Join(w.Path, "own.txt")); err != nil || string(data) != "own\n" {
		t.Fatalf("the workspace's files after the replay: %q %v", data, err)
	}
}

// What the provider commits reads the same through its history operations as
// through Git's, and a replay of recorded commits makes the commit Git's
// makes, leaving nothing behind.
func TestJujutsuReadsItsCommitsAsGitDoes(t *testing.T) {
	t.Parallel()
	f, j := newJujutsuFixture(t)
	ctx := context.Background()
	base := git(t, "-C", f.scratch, "rev-parse", "HEAD")
	git(t, "-C", f.clone, "branch", "osmia/w1", base)
	feature := acquireJJ(t, j, vcs.Request{Name: "w1", Branch: "osmia/w1"})
	commit := snapshotJJ(t, j, feature, base, map[string]string{"src/a.go": "package src\n", "README": ""})
	if paths, err := j.ChangedPaths(ctx, base, commit); err != nil || !slices.Equal(paths, []string{"README", "src/a.go"}) {
		t.Fatalf("changed paths %v: %v", paths, err)
	}
	if diff, err := j.Diff(ctx, base, commit); err != nil || !strings.Contains(diff, "+package src") {
		t.Fatalf("diff %q: %v", diff, err)
	}
	if ancestor, err := j.Ancestor(ctx, base, commit); err != nil || !ancestor {
		t.Fatalf("ancestor %v: %v", ancestor, err)
	}
	if merged, err := j.MergeBase(ctx, base, commit); err != nil || merged != base {
		t.Fatalf("merge base %s: %v", merged, err)
	}
	dir := filepath.Join(t.TempDir(), "export")
	if err := j.Export(ctx, commit, dir); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "src", "a.go")); err != nil || string(data) != "package src\n" {
		t.Fatalf("exported %q: %v", data, err)
	}
	if remote, err := j.Remote(ctx, "acme/widgets"); err != nil || remote != "upstream" {
		t.Fatalf("remote %q: %v", remote, err)
	}
	upstream := f.advance(t, "next")
	if _, err := j.Fetch(ctx, "upstream", "main"); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	replayed, conflicts, err := j.Replay(ctx, commit, upstream, at)
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("replay %s %v: %v", replayed, conflicts, err)
	}
	if byGit, _, err := f.provider.Replay(ctx, commit, upstream, at); err != nil || byGit != replayed {
		t.Fatalf("Git's replay made %s, %v; want %s", byGit, err, replayed)
	}
	if _, err := os.Stat(filepath.Join(j.Directory, replayName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the replay left its worktree: %v", err)
	}
	if tip, _, err := j.Branch(ctx, "osmia/w1"); err != nil || tip != commit {
		t.Fatalf("the replay moved the feature branch to %s, %v", tip, err)
	}
}
