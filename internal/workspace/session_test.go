package workspace

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/vcs"
)

// newSessionWorktree gives a fixture clone a worktree on a session-style
// branch at the fixture's base commit, as a session's own open would.
func newSessionWorktree(t *testing.T, f fixture) Worktree {
	t.Helper()
	base := git(t, "-C", f.clone, "rev-parse", "HEAD")
	acquired, err := f.provider.Acquire(context.Background(), vcs.Request{Name: "session", Ref: base, Branch: "osmia/session/w1"})
	if err != nil {
		t.Fatal(err)
	}
	return acquired.(Worktree)
}

func TestWorktreeDiffReadsUncommittedChangesIncludingUntracked(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	w := newSessionWorktree(t, f)
	if diff, err := f.provider.WorktreeDiff(ctx, w); err != nil || diff != "" {
		t.Fatalf("a clean worktree's diff: %q, %v", diff, err)
	}
	if err := os.WriteFile(filepath.Join(w.Path, "README"), []byte("widgets, changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Path, "new.txt"), []byte("brand new\n"), 0600); err != nil {
		t.Fatal(err)
	}
	diff, err := f.provider.WorktreeDiff(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "README") || !strings.Contains(diff, "widgets, changed") || !strings.Contains(diff, "new.txt") || !strings.Contains(diff, "brand new") {
		t.Fatalf("worktree diff missing changes:\n%s", diff)
	}
	// Reading the diff leaves the worktree's own index untouched: it is
	// still reported dirty by an ordinary status, and committing afterward
	// still sees the same changes.
	status := git(t, "-C", w.Path, "status", "--porcelain")
	if status == "" {
		t.Fatal("WorktreeDiff staged the worktree's real index")
	}
}

func TestCommitChangesCommitsEveryChangeAndReportsNoChanges(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	w := newSessionWorktree(t, f)
	head := git(t, "-C", w.Path, "rev-parse", "HEAD")
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if commit, changed, err := f.provider.CommitChanges(ctx, w, "no changes yet", at); err != nil || changed || commit != head {
		t.Fatalf("an unchanged worktree: %s %v %v, want %s unchanged", commit, changed, err, head)
	}
	if err := os.WriteFile(filepath.Join(w.Path, "README"), []byte("widgets, changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Path, "new.txt"), []byte("brand new\n"), 0600); err != nil {
		t.Fatal(err)
	}
	commit, changed, err := f.provider.CommitChanges(ctx, w, "the owner's message", at)
	if err != nil || !changed {
		t.Fatalf("commit: %s %v %v", commit, changed, err)
	}
	if status := git(t, "-C", w.Path, "status", "--porcelain"); status != "" {
		t.Fatalf("the worktree after its commit:\n%s", status)
	}
	if got := git(t, "-C", w.Path, "rev-parse", "HEAD"); got != commit {
		t.Fatalf("HEAD is %s, want %s", got, commit)
	}
	if got := git(t, "-C", w.Path, "log", "-1", "--format=%P %an <%ae> %cn <%ce> %s", commit); got != head+" Osmia <osmia@localhost> Osmia <osmia@localhost> the owner's message" {
		t.Fatalf("commit parent, identity and message: %s", got)
	}
	changed2 := git(t, "-C", w.Path, "diff-tree", "-r", "--name-status", "--no-renames", head, commit)
	if changed2 != "M\tREADME\nA\tnew.txt" {
		t.Fatalf("commit's changes:\n%s", changed2)
	}
	// Committing again with nothing new to add reports the same commit
	// unchanged.
	again, changed, err := f.provider.CommitChanges(ctx, w, "retried message", at)
	if err != nil || changed || again != commit {
		t.Fatalf("a repeated commit of an unchanged worktree: %s %v %v, want %s unchanged", again, changed, err, commit)
	}
}

func TestLogReadsCommitsAfterBaseOrNoneWhenEqual(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	w := newSessionWorktree(t, f)
	base := git(t, "-C", w.Path, "rev-parse", "HEAD")
	if messages, err := f.provider.Log(ctx, base, base); err != nil || len(messages) != 0 {
		t.Fatalf("log of an unchanged branch: %+v, %v", messages, err)
	}
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.WriteFile(filepath.Join(w.Path, "one.txt"), []byte("one\n"), 0600); err != nil {
		t.Fatal(err)
	}
	first, _, err := f.provider.CommitChanges(ctx, w, "first commit", at)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Path, "two.txt"), []byte("two\n"), 0600); err != nil {
		t.Fatal(err)
	}
	second, _, err := f.provider.CommitChanges(ctx, w, "second commit", at)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := f.provider.Log(ctx, base, second)
	if err != nil {
		t.Fatal(err)
	}
	want := []DeliveryMessage{{Commit: first, Message: "first commit"}, {Commit: second, Message: "second commit"}}
	if !slices.Equal(messages, want) {
		t.Fatalf("log: %+v, want %+v", messages, want)
	}
}
