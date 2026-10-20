package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/vcs"
)

func TestGitConcurrentWorktreeUpkeep(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, name := range []string{"a", "b"} {
		go func() {
			<-start
			// Separate providers of the same clone share the lock.
			g := &Git{Clone: f.clone, Directory: f.provider.Directory}
			for n := range 10 {
				w, err := g.Acquire(ctx, vcs.Request{Name: name, Branch: name, Ref: "main"})
				if err != nil {
					results <- err
					return
				}
				if n%2 == 0 {
					err = os.RemoveAll(w.Directory())
					if err == nil {
						if n%4 == 0 {
							_, found, lookupErr := g.Workspace(ctx, name)
							err = lookupErr
							if found && err == nil {
								err = errors.New("missing worktree remained registered")
							}
						} else {
							err = g.Prune(ctx)
						}
					}
				} else {
					err = g.Release(ctx, w)
				}
				if err != nil {
					results <- err
					return
				}
			}
			results <- nil
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	entries, err := f.provider.list(ctx)
	if err != nil || len(entries) != 1 || entries[0].path != f.clone {
		t.Fatalf("worktree list after concurrent upkeep: %+v: %v", entries, err)
	}
}

func TestGitConcurrentReplaysUseOperationScratchWorktrees(t *testing.T) {
	// A wrapper gates core's first command after the scratch worktree is added,
	// proving both scratch directories coexist while their replays are active.
	f := newFixture(t)
	base := git(t, "-C", f.clone, "rev-parse", "HEAD")
	onto := f.advance(t, "upstream.txt")
	if _, err := f.provider.Fetch(context.Background(), "upstream", "main"); err != nil {
		t.Fatal(err)
	}
	w, err := f.provider.Acquire(context.Background(), vcs.Request{Name: "unit", Branch: "unit", Ref: base})
	if err != nil {
		t.Fatal(err)
	}
	writeFiles(t, w.Directory(), map[string]string{"unit.txt": "unit\n"})
	head, err := f.provider.Snapshot(context.Background(), w.(Worktree), base)
	if err != nil {
		t.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	gate := t.TempDir()
	wrapper := filepath.Join(t.TempDir(), "git")
	script := `#!/bin/sh
scratch="$2"
case "$PWD" in *".replay-"*) scratch="$PWD";; esac
case "$scratch" in
 *".replay-"*)
  touch "` + gate + `/$(basename "$scratch")"
  while [ ! -f "` + gate + `/release" ]; do sleep 0.01; done;;
esac
exec "` + realGit + `" "$@"
`
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(wrapper)+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	type result struct {
		commit    string
		conflicts []string
		err       error
	}
	results := make(chan result, 2)
	for _, op := range []string{"stream-a/replay-1", "stream-b/replay-1"} {
		go func() {
			c, paths, err := f.provider.ReplayFrom(WithOperation(ctx, op), base, head, onto, time.Now())
			results <- result{c, paths, err}
		}()
	}
	t.Cleanup(func() { _ = os.WriteFile(filepath.Join(gate, "release"), nil, 0600) })
	deadline := time.Now().Add(10 * time.Second)
	both := false
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir(gate)
		if len(entries) == 2 {
			both = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !both {
		cancel()
		t.Fatal("both operation scratch worktrees did not coexist")
	}
	if err := os.WriteFile(filepath.Join(gate, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	var commits []string
	for range 2 {
		r := <-results
		if r.err != nil || len(r.conflicts) != 0 {
			t.Fatalf("replay %+v", r)
		}
		commits = append(commits, r.commit)
	}
	if commits[0] != commits[1] || commits[0] == head {
		t.Fatalf("replayed commits %v", commits)
	}
	for _, op := range []string{"stream-a/replay-1", "stream-b/replay-1"} {
		if _, err := os.Stat(filepath.Join(gate, ReplayName(op))); err != nil {
			t.Fatalf("scratch not used for %s: %v", op, err)
		}
		if _, err := os.Stat(f.provider.path(ReplayName(op))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("scratch not removed: %v", err)
		}
	}
}

func TestFetchRetriesARefLockedByAnotherFetch(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.provider.Fetch(ctx, "upstream", "main"); err != nil {
		t.Fatal(err)
	}
	want := f.advance(t, "next.txt")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	lock := filepath.Join(f.clone, ".git", "refs", "remotes", "upstream", "main.lock")
	script := `#!/bin/sh
case " $* " in
 *" fetch "*)
  echo fetch >> "` + dir + `/attempts"
  if [ ! -f "` + dir + `/collided" ]; then
   touch "` + dir + `/collided" "` + lock + `"
   "` + realGit + `" "$@"
   status=$?
   rm "` + lock + `"
   exit "$status"
  fi;;
esac
exec "` + realGit + `" "$@"
`
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	got, err := f.provider.Fetch(ctx, "upstream", "main")
	if err != nil || got != want {
		t.Fatalf("fetch got %s, want %s: %v", got, want, err)
	}
	if attempts := readFile(t, filepath.Join(dir, "attempts")); attempts != "fetch\nfetch\n" {
		t.Fatalf("fetch attempts %q", attempts)
	}
}

func TestJujutsuLocalStepOwnsAndSettlesCheckpoint(t *testing.T) {
	t.Parallel()
	f, j := newJujutsuFixture(t)
	ctx := context.Background()
	base := git(t, "-C", f.clone, "rev-parse", "HEAD")
	w := acquireJJ(t, j, vcs.Request{Name: "a", Branch: "a", Ref: base})
	if err := j.Checkpoint(ctx, "outside", time.Now()); err == nil {
		t.Fatal("checkpoint without lock accepted")
	}
	var escaped context.Context
	err := LocalStep(ctx, "operation-a", time.Now(), []Provider{j}, func(local context.Context) error {
		escaped = local
		if !holdsClone(local, cloneKey(j.Clone)) {
			return errors.New("local step lacks clone ownership")
		}
		if _, err := os.Stat(j.checkpointPath("operation-a")); err != nil {
			return err
		}
		if _, err := j.Fetch(local, "upstream", "main"); err == nil {
			return errors.New("fetch accepted under clone lock")
		}
		if err := j.Push(local, "upstream", base, "a", ""); err == nil {
			return errors.New("push accepted under clone lock")
		}
		writeFiles(t, w.Path, map[string]string{"local.txt": "local\n"})
		_, err := j.Snapshot(local, w, base)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if holdsClone(escaped, cloneKey(j.Clone)) {
		t.Fatal("escaped context retained ownership")
	}
	if err := j.Checkpoint(escaped, "escaped", time.Now()); err == nil {
		t.Fatal("expired ownership accepted")
	}
	if err := func() error {
		local, unlock := lockClone(escaped, j.Clone)
		defer unlock()
		if err := j.Checkpoint(local, "operation-a", time.Now()); err == nil {
			return errors.New("expired checkpoint scope accepted after reacquiring lock")
		}
		return nil
	}(); err != nil {
		t.Fatal(err)
	}
	if ops, err := j.Recover(ctx); err != nil || len(ops) != 0 {
		t.Fatalf("outstanding checkpoints %v: %v", ops, err)
	}
	if _, err := j.Fetch(ctx, "upstream", "main"); err != nil {
		t.Fatal(err)
	}
}

func TestJujutsuCancelledLocalStepRecoversBeforeAnotherStep(t *testing.T) {
	t.Parallel()
	f, j := newJujutsuFixture(t)
	base := git(t, "-C", f.clone, "rev-parse", "HEAD")
	a := acquireJJ(t, j, vcs.Request{Name: "a", Branch: "a", Ref: base})
	b := acquireJJ(t, j, vcs.Request{Name: "b", Branch: "b", Ref: base})
	ctx, cancel := context.WithCancel(context.Background())
	entered, proceed := make(chan struct{}), make(chan struct{})
	aResult := make(chan error, 1)
	go func() {
		aResult <- LocalStep(ctx, "a", time.Now(), []Provider{j}, func(local context.Context) error {
			if _, err := j.Record(local, a); err != nil {
				return err
			}
			close(entered)
			<-proceed
			cancel()
			return ctx.Err()
		})
	}()
	<-entered
	bResult := make(chan error, 1)
	go func() {
		bResult <- LocalStep(context.Background(), "b", time.Now(), []Provider{j}, func(local context.Context) error {
			if holdsClone(ctx, cloneKey(j.Clone)) {
				return errors.New("wrong context owns lock")
			}
			_, err := j.Record(local, b)
			return err
		})
	}()
	close(proceed)
	if err := <-aResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("A: %v", err)
	}
	if err := <-bResult; err != nil {
		t.Fatalf("B: %v", err)
	}
	if ops, err := j.Recover(context.Background()); err != nil || len(ops) != 0 {
		t.Fatalf("outstanding %v: %v", ops, err)
	}
}

// TestJujutsuStepCrashHelper is a child process killed before LocalStep's
// defers, exercising the same on-disk checkpoint startup Recover consumes.
func TestJujutsuStepCrashHelper(t *testing.T) {
	clone := os.Getenv("OSMIA_STEP_CRASH_CLONE")
	if clone == "" {
		return
	}
	j := &Jujutsu{Clone: clone, Directory: os.Getenv("OSMIA_STEP_CRASH_DIRECTORY")}
	base := os.Getenv("OSMIA_STEP_CRASH_BASE")
	err := LocalStep(context.Background(), "a/second", time.Now(), []Provider{j}, func(ctx context.Context) error {
		// Creation changes repository workspace metadata. A crash inside the
		// step must restore it; a crash after settlement must inspect and reuse it.
		if _, err := j.Acquire(ctx, vcs.Request{Name: "a-step", Branch: "a-step", Ref: base}); err != nil {
			return err
		}
		if os.Getenv("OSMIA_STEP_CRASH_LOCATION") == "inside" {
			os.Exit(73)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	os.Exit(73)
}

func TestJujutsuRecoveryPreservesAnotherOperationsSettledReplay(t *testing.T) {
	t.Parallel()
	for _, location := range []string{"inside", "between"} {
		t.Run(location, func(t *testing.T) {
			f, j := newJujutsuFixture(t)
			ctx := context.Background()
			base := git(t, "-C", f.clone, "rev-parse", "HEAD")
			onto := f.advance(t, "upstream.txt")
			if _, err := j.Fetch(ctx, "upstream", "main"); err != nil {
				t.Fatal(err)
			}
			_ = acquireJJ(t, j, vcs.Request{Name: "a", Branch: "a", Ref: base})
			b := acquireJJ(t, j, vcs.Request{Name: "b", Branch: "b", Ref: base})
			writeFiles(t, b.Path, map[string]string{"b.txt": "B's output\n"})
			if _, err := j.Snapshot(ctx, b, base); err != nil {
				t.Fatal(err)
			}
			var replayed string
			runs := 0
			record := filepath.Join(t.TempDir(), "b-result")
			applyB := func() error {
				if _, err := os.Stat(record); err == nil {
					return nil
				}
				return LocalStep(ctx, "b/replay", time.Now(), []Provider{j}, func(local context.Context) error {
					runs++
					var paths []string
					var err error
					replayed, paths, err = j.ReplayIn(local, b, onto, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
					if err != nil || len(paths) > 0 {
						return fmt.Errorf("B replay: %v: %w", paths, err)
					}
					return writeDurably(record, []byte(replayed))
				})
			}
			if err := applyB(); err != nil {
				t.Fatal(err)
			}
			before, err := readMetadata(b.Path)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestJujutsuStepCrashHelper$")
			cmd.Env = append(os.Environ(), "OSMIA_STEP_CRASH_CLONE="+j.Clone, "OSMIA_STEP_CRASH_DIRECTORY="+j.Directory, "OSMIA_STEP_CRASH_BASE="+base, "OSMIA_STEP_CRASH_LOCATION="+location)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 73 {
				t.Fatalf("crash: %v\n%s", err, out)
			}
			restarted := &Jujutsu{Clone: j.Clone, Directory: j.Directory}
			ops, err := restarted.Recover(ctx)
			want := []string(nil)
			if location == "inside" {
				want = []string{"a/second"}
			}
			if err != nil || !slices.Equal(ops, want) {
				t.Fatalf("recovered %v want %v: %v", ops, want, err)
			}
			after, err := readMetadata(b.Path)
			if err != nil || !reflect.DeepEqual(after, before) {
				t.Fatalf("B metadata %+v -> %+v: %v", before, after, err)
			}
			if tip, _, err := restarted.Branch(ctx, "b"); err != nil || tip != replayed {
				t.Fatalf("B branch %s want %s: %v", tip, replayed, err)
			}
			if got := readFile(t, filepath.Join(b.Path, "b.txt")); got != "B's output\n" {
				t.Fatalf("B files: %q", got)
			}
			if got := readFile(t, record); got != replayed {
				t.Fatalf("B result: %s", got)
			}
			if err := applyB(); err != nil || runs != 1 {
				t.Fatalf("B replayed %d times: %v", runs, err)
			}
			// A's settled creation is recognised even when it has not written
			// its result. An interrupted creation is retried from its checkpoint.
			_, exists, err := restarted.Workspace(ctx, "a-step")
			if err != nil || exists != (location == "between") {
				t.Fatalf("A workspace after recovery: %t: %v", exists, err)
			}
			creates := 0
			applyA := func() error {
				if _, found, err := restarted.Workspace(ctx, "a-step"); err != nil || found {
					return err
				}
				return LocalStep(ctx, "a/second", time.Now(), []Provider{restarted}, func(local context.Context) error {
					creates++
					_, err := restarted.Acquire(local, vcs.Request{Name: "a-step", Branch: "a-step", Ref: base})
					return err
				})
			}
			if err := applyA(); err != nil {
				t.Fatal(err)
			}
			if err := applyA(); err != nil {
				t.Fatal(err)
			}
			wantCreates := 0
			if location == "inside" {
				wantCreates = 1
			}
			if creates != wantCreates {
				t.Fatalf("A creation retried %d times, want %d", creates, wantCreates)
			}

		})
	}
}

// Retain explicit coverage of the collision classifier's non-retry cases.
func TestFetchCollisionClassification(t *testing.T) {
	for _, text := range []string{"authentication failed", "cannot lock ref: permission denied", "connection refused"} {
		if fetchRefCollision(errors.New(text)) {
			t.Fatal(text)
		}
	}
}

func TestGitOtherRefsAndFetchDoNotWaitForWorktreeLock(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	// A separate operation may fetch or work on another ref while a worktree
	// mutation owns the clone lock. Its context carries no borrowed ownership.
	_, unlock := lockClone(context.Background(), f.clone)
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		if _, _, err := f.provider.Branch(ctx, "main"); err != nil {
			done <- err
			return
		}
		_, err := f.provider.Fetch(ctx, "upstream", "main")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("unrelated Git command waited for clone lock")
	}
}
