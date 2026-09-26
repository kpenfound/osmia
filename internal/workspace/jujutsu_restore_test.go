package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/vcs"
)

// visibleHeads returns the commits the repository shows as heads.
func visibleHeads(t *testing.T, j *Jujutsu) []string {
	t.Helper()
	out, err := j.run(context.Background(), j.repository(), nil, "--ignore-working-copy", "log", "--no-graph", "--revisions", "visible_heads()", "--template", `commit_id ++ "\n"`)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(out)
}

// readFile returns the file's content, or "" when it is missing.
func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Recover restores the repository to the checkpoint an interrupted attempt
// left: the commits and workspaces the attempt made are gone, the branches
// the attempt moved in the clone stay, and each workspace whose branch moved
// is on it again, with the files it held before the attempt, none lost. The
// checkpoint is dropped, so a second Recover restores nothing.
func TestJujutsuRecoverRestoresTheCheckpointAnInterruptedAttemptLeft(t *testing.T) {
	t.Parallel()
	f, j := newJujutsuFixture(t)
	ctx := context.Background()
	at := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	base := git(t, "-C", f.scratch, "rev-parse", "HEAD")
	git(t, "-C", f.clone, "branch", "osmia/w1", base)
	feature := acquireJJ(t, j, vcs.Request{Name: "w1", Branch: "osmia/w1"})
	unit := acquireJJ(t, j, vcs.Request{Name: "w1/u1", Ref: "osmia/w1", Branch: "osmia-unit/w1/u1"})
	idle := acquireJJ(t, j, vcs.Request{Name: "w1/u2", Ref: "osmia/w1", Branch: "osmia-unit/w1/u2"})
	writeFiles(t, unit.Path, map[string]string{"unit.go": "unit\n", "README": "widgets by the unit\n"})
	writeFiles(t, idle.Path, map[string]string{"idle.go": "captured, never snapshotted\n"})
	before := visibleHeads(t, j)
	if err := j.Checkpoint(ctx, "operation-1", at); err != nil {
		t.Fatal(err)
	}

	// The interrupted attempt: the unit's snapshot moves its branch, a
	// conflicting rebase leaves a commit of Jujutsu's own, a landing moves
	// the feature branch and its workspace, and a workspace is made.
	snapshot, err := j.Snapshot(ctx, unit, base)
	if err != nil {
		t.Fatal(err)
	}
	landed := snapshotJJ(t, j, feature, base, map[string]string{"README": "widgets by the feature\n"})
	stray, conflicts, err := j.Rebase(ctx, landed, snapshot, "Rebase unit u1", at)
	if err != nil || !slices.Equal(conflicts, []string{"README"}) {
		t.Fatalf("the conflicting rebase %s %v: %v", stray, conflicts, err)
	}
	made := acquireJJ(t, j, vcs.Request{Name: "w1/u3", Ref: "osmia/w1", Branch: "osmia-unit/w1/u3"})
	if !slices.Contains(visibleHeads(t, j), stray) {
		t.Fatalf("the conflicting rebase's commit %s is no head", stray)
	}

	operations, err := j.Recover(ctx)
	if err != nil || !slices.Equal(operations, []string{"operation-1"}) {
		t.Fatalf("recovered %v: %v", operations, err)
	}
	if heads := visibleHeads(t, j); slices.Contains(heads, stray) {
		t.Fatalf("the interrupted attempt's commit %s is still a head: %v (before %v)", stray, heads, before)
	}
	for branch, want := range map[string]string{"osmia/w1": landed, "osmia-unit/w1/u1": snapshot, "osmia-unit/w1/u2": base} {
		if head, _, err := j.Branch(ctx, branch); err != nil || head != want {
			t.Fatalf("branch %s is at %s, %v; want %s", branch, head, err, want)
		}
	}
	if again, err := j.Snapshot(ctx, unit, base); err != nil || again != snapshot {
		t.Fatalf("the unit's snapshot after the restore is %s, %v; want %s", again, err, snapshot)
	}
	if got := readFile(t, filepath.Join(unit.Path, "unit.go")); got != "unit\n" {
		t.Fatalf("the unit's unit.go after the restore: %q", got)
	}
	current, err := j.describe(ctx, feature.Path, true, "@")
	if err != nil || !slices.Equal(current.parents, []string{landed}) || !current.empty {
		t.Fatalf("the feature workspace is on %v (empty %t), not %s: %v", current.parents, current.empty, landed, err)
	}
	if got := readFile(t, filepath.Join(feature.Path, "README")); got != "widgets by the feature\n" {
		t.Fatalf("the feature workspace's README after the restore: %q", got)
	}
	if got := readFile(t, filepath.Join(idle.Path, "idle.go")); got != "captured, never snapshotted\n" {
		t.Fatalf("the idle workspace's idle.go after the restore: %q", got)
	}
	if _, found, err := j.Workspace(ctx, "w1/u3"); err != nil || found {
		t.Fatalf("the workspace the attempt made is still one: %t %v", found, err)
	}
	if again := acquireJJ(t, j, vcs.Request{Name: "w1/u3", Ref: "osmia/w1", Branch: "osmia-unit/w1/u3"}); again.Path != made.Path {
		t.Fatalf("the workspace is made again at %s, not %s", again.Path, made.Path)
	}
	if operations, err := j.Recover(ctx); err != nil || len(operations) != 0 {
		t.Fatalf("a second recovery restored %v: %v", operations, err)
	}
}

// A replay the attempt went on with goes back to where the checkpoint found
// it, holding the resolution the workspace held then, and going on with it
// again makes the same branch the interrupted attempt moved to.
func TestJujutsuRecoverTakesAReplayBackToItsCheckpoint(t *testing.T) {
	t.Parallel()
	f, j := newJujutsuFixture(t)
	ctx := context.Background()
	at := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	base := git(t, "-C", f.scratch, "rev-parse", "HEAD")
	git(t, "-C", f.clone, "branch", "osmia/w1", base)
	feature := acquireJJ(t, j, vcs.Request{Name: "w1", Branch: "osmia/w1"})
	first := snapshotJJ(t, j, feature, base, map[string]string{"README": "feature\n"})
	second := snapshotJJ(t, j, feature, first, map[string]string{"NOTES": "notes\n"})
	writeFiles(t, f.scratch, map[string]string{"README": "upstream\n"})
	git(t, "-C", f.scratch, "commit", "--quiet", "-am", "upstream README")
	git(t, "-C", f.scratch, "push", "--quiet", "origin", "main")
	upstream, err := j.Fetch(ctx, "upstream", "main")
	if err != nil {
		t.Fatal(err)
	}
	git(t, "-C", f.clone, "branch", "osmia-drift/w1/1", second)
	resolution := acquireJJ(t, j, vcs.Request{Name: "drift", Branch: "osmia-drift/w1/1"})
	if _, conflicts, err := j.ReplayIn(ctx, resolution, upstream, at); err != nil || !slices.Equal(conflicts, []string{"README"}) {
		t.Fatalf("the replay stopped with %v: %v", conflicts, err)
	}
	writeFiles(t, resolution.Path, map[string]string{"README": "upstream and feature\n"})
	if err := j.Checkpoint(ctx, "operation-2", at); err != nil {
		t.Fatal(err)
	}
	tip, conflicts, err := j.ContinueReplay(ctx, resolution, at)
	if err != nil || len(conflicts) != 0 || tip == "" {
		t.Fatalf("the replay went on to %s with %v: %v", tip, conflicts, err)
	}
	if _, _, replaying, err := j.Replaying(ctx, resolution); err != nil || replaying {
		t.Fatalf("the finished replay is in progress (%t): %v", replaying, err)
	}

	if operations, err := j.Recover(ctx); err != nil || !slices.Equal(operations, []string{"operation-2"}) {
		t.Fatalf("recovered %v: %v", operations, err)
	}
	if stop, unmerged, replaying, err := j.Replaying(ctx, resolution); err != nil || !replaying || stop != "" || len(unmerged) != 0 {
		t.Fatalf("the restored replay stops at %s with %v (%t): %v", stop, unmerged, replaying, err)
	}
	if got := readFile(t, filepath.Join(resolution.Path, "README")); got != "upstream and feature\n" {
		t.Fatalf("the restored resolution's README: %q", got)
	}
	again, conflicts, err := j.ContinueReplay(ctx, resolution, at)
	if err != nil || len(conflicts) != 0 || again != tip {
		t.Fatalf("the replay went on again to %s with %v: %v; want %s", again, conflicts, err, tip)
	}
	if head, _, err := j.Branch(ctx, "osmia-drift/w1/1"); err != nil || head != tip {
		t.Fatalf("the resolution branch is at %s, %v; want %s", head, err, tip)
	}
}

// A settled checkpoint restores nothing, and a repository not made yet
// records none.
func TestJujutsuSettledCheckpointRestoresNothing(t *testing.T) {
	t.Parallel()
	f, j := newJujutsuFixture(t)
	ctx := context.Background()
	at := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	if err := j.Checkpoint(ctx, "operation-0", at); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(j.repository()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a checkpoint made the repository: %v", err)
	}
	base := git(t, "-C", f.scratch, "rev-parse", "HEAD")
	git(t, "-C", f.clone, "branch", "osmia/w1", base)
	feature := acquireJJ(t, j, vcs.Request{Name: "w1", Branch: "osmia/w1"})
	if err := j.Checkpoint(ctx, "operation-1", at); err != nil {
		t.Fatal(err)
	}
	landed := snapshotJJ(t, j, feature, base, map[string]string{"landed.go": "landed\n"})
	if err := j.Settle(ctx, "operation-1"); err != nil {
		t.Fatal(err)
	}
	if err := j.Settle(ctx, "operation-1"); err != nil {
		t.Fatalf("settling again: %v", err)
	}
	if operations, err := j.Recover(ctx); err != nil || len(operations) != 0 {
		t.Fatalf("recovered %v: %v", operations, err)
	}
	current, err := j.describe(ctx, feature.Path, true, "@")
	if err != nil || !slices.Equal(current.parents, []string{landed}) || readFile(t, filepath.Join(feature.Path, "landed.go")) != "landed\n" {
		t.Fatalf("the feature workspace is on %v: %v", current.parents, err)
	}
}

// Git keeps no state of its own to restore: its checkpoints record nothing.
func TestGitCheckpointsRecordNothing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	if err := f.provider.Checkpoint(ctx, "operation-1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if operations, err := f.provider.Recover(ctx); err != nil || len(operations) != 0 {
		t.Fatalf("recovered %v: %v", operations, err)
	}
	if err := f.provider.Settle(ctx, "operation-1"); err != nil {
		t.Fatal(err)
	}
}
