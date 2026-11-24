package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/vcs"
)

func hangingJJ(t *testing.T) (jj, ctl string) {
	t.Helper()
	real, err := exec.LookPath("jj")
	if err != nil {
		t.Fatal(err)
	}
	ctl = t.TempDir()
	jj = filepath.Join(t.TempDir(), "jj")
	script := fmt.Sprintf(`#!/bin/sh
for a in "$@"; do
  if [ -e '%[1]s'/hang-"$a" ]; then
    echo $$ > '%[1]s'/pid-"$a"
    while [ ! -e '%[1]s'/release-"$a" ]; do sleep 0.05; done
    exit 1
  fi
done
exec '%[2]s' "$@"
`, ctl, real)
	if err := os.WriteFile(jj, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return jj, ctl
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
}

func pollPID(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid, err == nil
}

func pollUntil(ok func() bool) bool {
	for end := time.Now().Add(20 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return true
		}
	}
	return false
}

func waitPID(t *testing.T, path string) int {
	t.Helper()
	var pid int
	if !pollUntil(func() bool { var ok bool; pid, ok = pollPID(path); return ok }) {
		t.Fatalf("no process reported itself at %s", path)
	}
	return pid
}

func cancelOncePIDReported(path string, cancel context.CancelFunc) {
	go func() {
		pollUntil(func() bool { _, ok := pollPID(path); return ok })
		cancel()
	}()
}

func processGone(pid int) bool {
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	if !pollUntil(ok) {
		t.Fatalf("timed out waiting until %s", what)
	}
}

func checkpointFiles(t *testing.T, j *Jujutsu) map[string]string {
	t.Helper()
	files := map[string]string{}
	entries, err := os.ReadDir(j.checkpoints())
	if errors.Is(err, os.ErrNotExist) {
		return files
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(j.checkpoints(), entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files[entry.Name()] = string(data)
	}
	return files
}

func setDuration(t *testing.T, p *time.Duration, v time.Duration) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

func TestLocalStepGivesUpOnAContendedCloneLock(t *testing.T) {
	f, j := newJujutsuFixture(t)
	base := git(t, "-C", f.clone, "rev-parse", "HEAD")
	w := acquireJJ(t, j, vcs.Request{Name: "a", Branch: "a", Ref: base})
	if err := crashCheckpoint(context.Background(), j, "interrupted", time.Now()); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, w.Path, map[string]string{"work.txt": "unsaved\n"})
	checkpointsBefore := checkpointFiles(t, j)
	if len(checkpointsBefore) != 1 {
		t.Fatalf("checkpoints before: %v", checkpointsBefore)
	}

	_, unlock, err := lockClone(context.Background(), f.clone)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	ran := false
	start := time.Now()
	err = LocalStep(ctx, "waiting", time.Now(), []Provider{j}, func(context.Context) error {
		ran = true
		return nil
	})
	if !IsContention(err) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a contended step returned %v", err)
	}
	var contended *ContentionError
	if !errors.As(err, &contended) || contended.Clone != cloneKey(f.clone) || contended.Waited < 100*time.Millisecond {
		t.Fatalf("contention error %+v does not name the clone and the wait", contended)
	}
	if !strings.Contains(err.Error(), cloneKey(f.clone)) || time.Since(start) > 10*time.Second {
		t.Fatalf("contention error %q after %s", err, time.Since(start))
	}
	if ran {
		t.Fatal("the step ran without the clone")
	}
	unlock()
	if got := checkpointFiles(t, j); fmt.Sprint(got) != fmt.Sprint(checkpointsBefore) {
		t.Fatalf("checkpoints changed from %v to %v", checkpointsBefore, got)
	}
	if got := readFile(t, filepath.Join(w.Path, "work.txt")); got != "unsaved\n" {
		t.Fatalf("the workspace's files changed: %q", got)
	}
	if q := j.Quiescence(); q.Running != 0 {
		t.Fatalf("contention left %+v", q)
	}

	err = LocalStep(context.Background(), "after", time.Now(), []Provider{j}, func(context.Context) error {
		if got := checkpointFiles(t, j); len(got) != 1 || fmt.Sprint(got) == fmt.Sprint(checkpointsBefore) {
			return fmt.Errorf("checkpoints at the start of the step: %v", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestGitWorktreeMutationGivesUpOnAContendedCloneLock(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, unlock, err := lockClone(context.Background(), f.clone)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	base := git(t, "-C", f.clone, "rev-parse", "HEAD")
	if _, err := f.provider.Acquire(ctx, vcs.Request{Name: "a", Branch: "a", Ref: base}); !IsContention(err) {
		t.Fatalf("Acquire returned %v", err)
	}
	if err := f.provider.Prune(ctx); !IsContention(err) {
		t.Fatalf("Prune returned %v", err)
	}
	if _, found, err := f.provider.Workspace(context.Background(), "a"); err != nil || found {
		t.Fatalf("the contended Acquire left a workspace: %t %v", found, err)
	}
}

func TestLocalStepKillsAHangingPrimaryCommand(t *testing.T) {
	f, j := newJujutsuFixture(t)
	var ctl string
	j.JJ, ctl = hangingJJ(t)
	base := git(t, "-C", f.clone, "rev-parse", "HEAD")
	acquireJJ(t, j, vcs.Request{Name: "a", Branch: "a", Ref: base})
	touch(t, filepath.Join(ctl, "hang-hang-primary"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelOncePIDReported(filepath.Join(ctl, "pid-hang-primary"), cancel)
	err := LocalStep(ctx, "hanging", time.Now(), []Provider{j}, func(local context.Context) error {
		if _, err := j.Acquire(local, vcs.Request{Name: "made", Branch: "made", Ref: base}); err != nil {
			return err
		}
		_, err := j.run(local, "", nil, "hang-primary")
		return err
	})
	if !errors.Is(err, context.Canceled) || IsRecoveryFailure(err) {
		t.Fatalf("the step returned %v", err)
	}
	if pid := waitPID(t, filepath.Join(ctl, "pid-hang-primary")); !processGone(pid) {
		t.Fatalf("process %d is still running after the step returned", pid)
	}
	if q := j.Quiescence(); !q.Quiesced() {
		t.Fatalf("clone after the step: %+v", q)
	}
	if got := checkpointFiles(t, j); len(got) != 0 {
		t.Fatalf("checkpoints left: %v", got)
	}
	if _, found, err := j.Workspace(context.Background(), "made"); err != nil || found {
		t.Fatalf("the checkout was not restored: %t %v", found, err)
	}
}

func TestLocalStepReturnsFromAHangingRecoverAndRefusesDirtyReuse(t *testing.T) {
	f, j := newJujutsuFixture(t)
	var ctl string
	j.JJ, ctl = hangingJJ(t)
	productionCleanup := cleanupDeadline
	setDuration(t, &cleanupDeadline, 400*time.Millisecond)
	setDuration(t, &killWait, 5*time.Second)
	base := git(t, "-C", f.clone, "rev-parse", "HEAD")
	acquireJJ(t, j, vcs.Request{Name: "a", Branch: "a", Ref: base})
	touch(t, filepath.Join(ctl, "hang-restore"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	err := LocalStep(ctx, "interrupted", time.Now(), []Provider{j}, func(local context.Context) error {
		if _, err := j.Acquire(local, vcs.Request{Name: "made", Branch: "made", Ref: base}); err != nil {
			return err
		}
		cancel()
		return local.Err()
	})
	if time.Since(start) > 30*time.Second {
		t.Fatalf("the step took %s", time.Since(start))
	}
	var recovery *RecoveryError
	if !IsRecoveryFailure(err) || !errors.As(err, &recovery) || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, context.Canceled) {
		t.Fatalf("the step returned %v", err)
	}
	if !strings.Contains(err.Error(), "recover clone") || !strings.Contains(err.Error(), cloneKey(f.clone)) {
		t.Fatalf("the error does not name the recovery failure: %v", err)
	}
	if pid := waitPID(t, filepath.Join(ctl, "pid-restore")); !processGone(pid) {
		t.Fatalf("restore process %d is still running", pid)
	}
	if q := j.Quiescence(); q.Running != 0 || !q.NeedsRecovery {
		t.Fatalf("clone after the failed restoration: %+v", q)
	}
	kept := checkpointFiles(t, j)
	if len(kept) != 1 {
		t.Fatalf("checkpoints kept: %v", kept)
	}

	ran := false
	err = LocalStep(context.Background(), "next", time.Now(), []Provider{j}, func(context.Context) error {
		ran = true
		return nil
	})
	if !IsRecoveryFailure(err) || ran {
		t.Fatalf("the next step ran=%t returned %v", ran, err)
	}
	if got := checkpointFiles(t, j); fmt.Sprint(got) != fmt.Sprint(kept) {
		t.Fatalf("checkpoints changed from %v to %v", kept, got)
	}
	if q := j.Quiescence(); !q.NeedsRecovery {
		t.Fatalf("the refusal cleared the mark: %+v", q)
	}

	if err := os.Remove(filepath.Join(ctl, "hang-restore")); err != nil {
		t.Fatal(err)
	}
	setDuration(t, &cleanupDeadline, productionCleanup)
	err = LocalStep(context.Background(), "after", time.Now(), []Provider{j}, func(local context.Context) error {
		if _, found, err := j.Workspace(local, "made"); err != nil || found {
			return fmt.Errorf("the step began on a dirty checkout: %t %v", found, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if q := j.Quiescence(); !q.Quiesced() {
		t.Fatalf("clone after the recovery: %+v", q)
	}
}

func TestLocalStepSettleFailureMarksTheClone(t *testing.T) {
	t.Parallel()
	f, j := newJujutsuFixture(t)
	acquireJJ(t, j, vcs.Request{Name: "a", Branch: "a", Ref: git(t, "-C", f.clone, "rev-parse", "HEAD")})
	err := LocalStep(context.Background(), "settling", time.Now(), []Provider{j}, func(context.Context) error {
		path := j.checkpointPath("settling")
		if err := os.Remove(path); err != nil {
			return err
		}
		if err := os.Mkdir(path, 0700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(path, "held"), nil, 0600)
	})
	if !IsRecoveryFailure(err) {
		t.Fatalf("the step returned %v", err)
	}
	if q := j.Quiescence(); !q.NeedsRecovery {
		t.Fatalf("clone after the failed settlement: %+v", q)
	}
	if _, err := os.Stat(filepath.Join(j.checkpointPath("settling"), "held")); err != nil {
		t.Fatal(err)
	}
}

func TestLocalStepKeepsTheCloneLockedWhileAKilledProcessSurvives(t *testing.T) {
	f, j := newJujutsuFixture(t)
	var ctl string
	j.JJ, ctl = hangingJJ(t)
	setDuration(t, &killWait, 300*time.Millisecond)
	oldTerminate := terminate
	terminate = func(*exec.Cmd) error { return nil }
	t.Cleanup(func() { terminate = oldTerminate })
	base := git(t, "-C", f.clone, "rev-parse", "HEAD")
	acquireJJ(t, j, vcs.Request{Name: "a", Branch: "a", Ref: base})
	touch(t, filepath.Join(ctl, "hang-hang-primary"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelOncePIDReported(filepath.Join(ctl, "pid-hang-primary"), cancel)
	err := LocalStep(ctx, "surviving", time.Now(), []Provider{j}, func(local context.Context) error {
		if _, err := j.Acquire(local, vcs.Request{Name: "made", Branch: "made", Ref: base}); err != nil {
			return err
		}
		_, err := j.run(local, "", nil, "hang-primary")
		return err
	})
	pid := waitPID(t, filepath.Join(ctl, "pid-hang-primary"))
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	var running *ProcessError
	if !errors.Is(err, ErrProcessRunning) || !errors.As(err, &running) || running.PID != pid || !IsRecoveryFailure(err) {
		t.Fatalf("the step returned %v", err)
	}
	if processGone(pid) {
		t.Fatal("the process exited")
	}
	if q := j.Quiescence(); q.Running != 1 || !q.NeedsRecovery || q.Quiesced() {
		t.Fatalf("clone with a surviving process: %+v", q)
	}
	if q := CloneQuiescence(f.clone); q.Running != 1 || !q.NeedsRecovery {
		t.Fatalf("clone state: %+v", q)
	}
	if got := checkpointFiles(t, j); len(got) != 1 {
		t.Fatalf("checkpoints with a surviving process: %v", got)
	}

	short, stop := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer stop()
	ran := false
	err = LocalStep(short, "later", time.Now(), []Provider{j}, func(context.Context) error {
		ran = true
		return nil
	})
	if !IsContention(err) || ran {
		t.Fatalf("a later step ran=%t returned %v", ran, err)
	}

	touch(t, filepath.Join(ctl, "release-hang-primary"))
	eventually(t, "the process has exited and freed the clone", func() bool { return CloneQuiescence(f.clone).Running == 0 })
	if q := j.Quiescence(); q.Running != 0 || !q.NeedsRecovery {
		t.Fatalf("clone after the exit: %+v", q)
	}
	err = LocalStep(context.Background(), "after", time.Now(), []Provider{j}, func(local context.Context) error {
		if _, found, err := j.Workspace(local, "made"); err != nil || found {
			return fmt.Errorf("the step began on a dirty checkout: %t %v", found, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if q := j.Quiescence(); !q.Quiesced() {
		t.Fatalf("clone after the recovery: %+v", q)
	}
}

func TestQuiescedGivesUpOnAContendedClone(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	_, jujutsu := newJujutsuFixture(t)
	for name, provider := range map[string]Provider{"git": fixture.provider, "jujutsu": jujutsu} {
		t.Run(name, func(t *testing.T) {
			var clone string
			switch p := provider.(type) {
			case *Git:
				clone = p.Clone
			case *Jujutsu:
				clone = p.Clone
			}
			_, unlock, err := lockClone(context.Background(), clone)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			ran := false
			err = Quiesced(ctx, []Provider{provider}, func(context.Context) error { ran = true; return nil })
			if !IsContention(err) || ran {
				t.Fatalf("a held clone: ran=%t, %v", ran, err)
			}
			unlock()
			if err := Quiesced(context.Background(), []Provider{provider}, func(context.Context) error { ran = true; return nil }); err != nil || !ran {
				t.Fatalf("a free clone: ran=%t, %v", ran, err)
			}
		})
	}
}

func TestQuiescedRecoversBeforeItReadsAndIsRefusedWhenRecoveryFails(t *testing.T) {
	f, j := newJujutsuFixture(t)
	var ctl string
	j.JJ, ctl = hangingJJ(t)
	productionCleanup := cleanupDeadline
	setDuration(t, &cleanupDeadline, 400*time.Millisecond)
	setDuration(t, &killWait, 5*time.Second)
	base := git(t, "-C", f.clone, "rev-parse", "HEAD")
	acquireJJ(t, j, vcs.Request{Name: "a", Branch: "a", Ref: base})
	touch(t, filepath.Join(ctl, "hang-restore"))
	cut, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := LocalStep(cut, "interrupted", time.Now(), []Provider{j}, func(local context.Context) error {
		if _, err := j.Acquire(local, vcs.Request{Name: "made", Branch: "made", Ref: base}); err != nil {
			return err
		}
		cancel()
		return local.Err()
	})
	if !IsRecoveryFailure(err) || len(checkpointFiles(t, j)) != 1 {
		t.Fatalf("the interrupted step returned %v, checkpoints %v", err, checkpointFiles(t, j))
	}

	ran := false
	err = Quiesced(context.Background(), []Provider{j}, func(context.Context) error { ran = true; return nil })
	if !IsRecoveryFailure(err) || ran {
		t.Fatalf("a clone that cannot recover: ran=%t, %v", ran, err)
	}
	if q := j.Quiescence(); q.Running != 0 || !q.NeedsRecovery || len(checkpointFiles(t, j)) != 1 {
		t.Fatalf("clone after the failed recovery: %+v, checkpoints %v", q, checkpointFiles(t, j))
	}

	if err := os.Remove(filepath.Join(ctl, "hang-restore")); err != nil {
		t.Fatal(err)
	}
	setDuration(t, &cleanupDeadline, productionCleanup)
	err = Quiesced(context.Background(), []Provider{j}, func(context.Context) error {
		ran = true
		if got := checkpointFiles(t, j); len(got) != 0 {
			return fmt.Errorf("the read began before recovery: %v", got)
		}
		return nil
	})
	if err != nil || !ran {
		t.Fatalf("a clone that can recover: ran=%t, %v", ran, err)
	}
	if q := j.Quiescence(); !q.Quiesced() {
		t.Fatalf("clone after the recovery: %+v", q)
	}
}
