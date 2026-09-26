package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

// stopAt returns a context that the service's boundary cancels at the named
// step, the first time it is reached, as the service stopping there does.
func stopAt(t *testing.T, s *Service, step string) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := false
	s.boundary = func(name string) error {
		if name == step && !stopped {
			stopped = true
			cancel()
			return errors.New("the service stopped")
		}
		return nil
	}
	t.Cleanup(func() {
		cancel()
		s.boundary = nil
	})
	return ctx
}

// jujutsuRepository returns the Jujutsu repository of the fixture project's
// workspaces under directory.
func jujutsuRepository(f *shedFixture, directory string) string {
	return filepath.Join(f.s.cfg.Root.String(), directory, string(f.s.cfg.Project.ID), ".jujutsu")
}

// checkpointEntry returns the operation-log entry of the one checkpoint an
// interrupted attempt left in the workspaces under directory.
func checkpointEntry(t *testing.T, f *shedFixture, directory string) string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(jujutsuRepository(f, directory), ".jj", "osmia-checkpoints", "*.json"))
	must(t, err)
	if len(paths) != 1 {
		t.Fatalf("the %s workspaces hold checkpoints %v", directory, paths)
	}
	data, err := os.ReadFile(paths[0])
	must(t, err)
	var c struct {
		Entry string `json:"entry"`
	}
	must(t, json.Unmarshal(data, &c))
	if c.Entry == "" {
		t.Fatalf("the checkpoint records no entry: %s", data)
	}
	return c.Entry
}

// operationLog returns the descriptions of the operations of the Jujutsu
// repository of the workspaces under directory, newest first, each after
// its ID.
func operationLog(t *testing.T, f *shedFixture, directory string) []string {
	t.Helper()
	cmd := exec.Command("jj", "--no-pager", "--color=never", "--repository", jujutsuRepository(f, directory), "--ignore-working-copy", "operation", "log", "--no-graph", "--template", `id ++ " " ++ description ++ "\n"`)
	cmd.Env = []string{"JJ_CONFIG=" + os.DevNull, "PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Dir(f.clone)}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("jj operation log: %v\n%s", err, out)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

// interrupted returns the entry of the one checkpoint an interrupted attempt
// left in the workspaces under directory, and whether the attempt recorded
// an operation after it.
func interrupted(t *testing.T, f *shedFixture, directory string) (string, bool) {
	t.Helper()
	entry := checkpointEntry(t, f, directory)
	return entry, !strings.HasPrefix(operationLog(t, f, directory)[0], entry+" ")
}

// restoredTo fails the test unless the workspaces under directory hold no
// checkpoint any more and, when the interrupted attempt recorded an
// operation after the checkpoint's entry, were restored to it.
func restoredTo(t *testing.T, f *shedFixture, directory, entry string, changed bool) {
	t.Helper()
	log := operationLog(t, f, directory)
	restored := slices.ContainsFunc(log, func(line string) bool { return strings.HasSuffix(line, " restore to operation "+entry) })
	if changed != restored {
		t.Fatalf("the %s workspaces were restored to %s: %t, want %t:\n%s", directory, entry, restored, changed, strings.Join(log, "\n"))
	}
	if paths, err := filepath.Glob(filepath.Join(jujutsuRepository(f, directory), ".jj", "osmia-checkpoints", "*.json")); err != nil || len(paths) != 0 {
		t.Fatalf("checkpoints left after the restore: %v %v", paths, err)
	}
}

// onCommit fails the test unless the workspace sits on commit, holding its
// files and nothing of its own.
func onCommit(t *testing.T, g workspace.Provider, w workspace.Worktree, commit string) {
	t.Helper()
	ctx := context.Background()
	current, err := g.Record(ctx, w)
	must(t, err)
	c, err := g.Commit(ctx, current)
	must(t, err)
	if !slices.Equal(c.Parents, []string{commit}) {
		t.Fatalf("workspace %s is on %v, not %s", w.Path, c.Parents, commit)
	}
	if changed, err := g.Changed(ctx, w, commit); err != nil || len(changed) != 0 {
		t.Fatalf("workspace %s differs from %s in %v: %v", w.Path, commit, changed, err)
	}
}

// A landing on Jujutsu workspaces cut short after its squash, after the
// feature branch moved or after its record leaves the checkpoint taken
// before the attempt, and the restarted service restores the feature branch
// workspaces to it before the landing is retried. The retry ends where an
// uninterrupted landing does: the feature branch at the commit that landing
// makes, the unit merged and landed once, and the feature workspace on the
// commit with its files.
func TestJujutsuLandingCutShortIsRestoredAndLandsOnce(t *testing.T) {
	requireJJ(t)
	t.Parallel()
	for _, step := range []string{"land-committed", "land-advanced", "land-recorded"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f, stream, repository := newApprovedFixtureOn(t, "restored-"+strings.TrimPrefix(step, "land-"), config.WorkspacesJujutsu)
			lands := &foreman{masons: newMasonController(f.s, repository)}
			must(t, lands.Pass(ctx))
			ops := landOperations(t, repository, stream)
			if len(ops) != 1 {
				t.Fatalf("landing operations %+v", ops)
			}
			op := ops[0].Operation
			in, err := decodeLand(op)
			must(t, err)
			review, result := approvedReview(t, repository, stream, in.Unit)
			message, err := lands.message(stream, in, review, result, op.ID)
			must(t, err)
			requested, err := lands.requestedAt(stream, op.ID)
			must(t, err)
			g := providerOf(t, featureWorkspaces(f.s.cfg, repository), stream)
			want, err := g.Squash(ctx, in.Base, in.Candidate, message, requested)
			must(t, err)

			if _, err := lands.Apply(stopAt(t, f.s, step), op); err == nil || !strings.Contains(err.Error(), "the service stopped") {
				t.Fatalf("the landing was not cut short at %s: %v", step, err)
			}
			entry, changed := interrupted(t, f, branchesDirectory)
			if changed != (step != "land-committed") {
				t.Fatalf("the landing cut short at %s changed the operation log: %t", step, changed)
			}
			if step == "land-advanced" {
				must(t, repository.Close())
				f.start(t)
				f.awaitMerged(t, stream, in.Unit)
				f.stop(t)
				if repository, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project); err != nil {
					t.Fatal(err)
				}
			} else {
				must(t, f.s.recoverWorkspaces(ctx, f.s.cfg))
				settleOperation(t, f.s, repository, stream, op, lands)
			}
			defer repository.Close()
			restoredTo(t, f, branchesDirectory, entry, changed)

			if tip := featureTip(t, f, stream); tip != want {
				t.Fatalf("the feature branch is at %s, not the uninterrupted landing's %s", tip, want)
			}
			if commits := f.landedCommits(t, stream, in.Base); !slices.Equal(commits, []string{want}) {
				t.Fatalf("the feature branch gained %v", commits)
			}
			var merged, landed []string
			for _, tr := range allTransitions(t, f.trace, stream) {
				if tr.Subject == trace.UnitSubject(in.Unit) && tr.To == UnitMerged {
					merged = append(merged, tr.ID)
				}
				if tr.Subject == landingSubject(in.Unit) && strings.HasPrefix(tr.To, "landed-") {
					landed = append(landed, tr.ID)
				}
			}
			if len(merged) != 1 || len(landed) != 1 {
				t.Fatalf("merged %v, landed %v", merged, landed)
			}
			var landing UnitLanding
			must(t, json.Unmarshal([]byte(streamDocuments(t, repository, stream, landingDocument(in.Unit))[0].Content), &landing))
			if landing.Commit != want || strings.TrimSpace(landing.Message) != strings.TrimSpace(message) {
				t.Fatalf("landing.json records %s:\n%s", landing.Commit, landing.Message)
			}
			w, found, err := g.Workspace(ctx, string(stream))
			must(t, err)
			if !found {
				t.Fatal("the feature branch has no workspace")
			}
			onCommit(t, g, w, want)
		})
	}
}

// A unit rebase on Jujutsu workspaces cut short after the snapshot or after
// the workspace moved is restored and retried, and ends where an
// uninterrupted rebase does: the unit branch on the landed tip carrying the
// unit's change, its conflicts stored and materialized in its workspace, the
// snapshot holding the mason's files, and the rebase recorded once. The
// other units' workspaces keep the files their masons left, which no
// snapshot had committed.
func TestJujutsuUnitRebaseCutShortIsRestoredAndRebasesOnce(t *testing.T) {
	requireJJ(t)
	t.Parallel()
	for _, step := range []string{"rebase-snapshotted", "rebase-moved"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f, stream, repository, lands, landed, ops := newJujutsuLandedFixture(t, "restored-"+strings.TrimPrefix(step, "rebase-"))
			r := rebaser{lands}
			if _, err := r.Apply(stopAt(t, f.s, step), ops["resume"]); err == nil || !strings.Contains(err.Error(), "the service stopped") {
				t.Fatalf("the rebase was not cut short at %s: %v", step, err)
			}
			entry, changed := interrupted(t, f, unitsDirectory)
			if !changed {
				t.Fatalf("the rebase cut short at %s changed nothing in the operation log", step)
			}
			must(t, f.s.recoverWorkspaces(ctx, f.s.cfg))
			restoredTo(t, f, unitsDirectory, entry, changed)
			if result := settleOperation(t, f.s, repository, stream, ops["resume"], r); result.Outcome != "succeeded" {
				t.Fatalf("the retried rebase %+v", result)
			}

			g := providerOf(t, newUnitWorkspaces(f.s.cfg, repository).streamWorkspaces, stream)
			rebases := unitRebases(t, repository, stream, "resume")
			if len(rebases) != 1 {
				t.Fatalf("resume's rebases %+v", rebases)
			}
			rebase := rebases[0]
			if tip, _, err := g.Branch(ctx, unitBranch(stream, "resume")); err != nil || tip != rebase.Commit || parentOf(t, f, tip) != landed {
				t.Fatalf("resume's branch is at %s, recorded %s, on %s: %v", tip, rebase.Commit, landed, err)
			}
			if stored, err := g.StoredConflicts(ctx, rebase.Commit); err != nil || !slices.Equal(stored, jujutsuConflicted["resume"]) || !slices.Equal(rebase.Conflicts, stored) {
				t.Fatalf("the rebased commit stores %v, recorded %v: %v", stored, rebase.Conflicts, err)
			}
			change, err := unitChange(repository, stream, "resume")
			must(t, err)
			if got, err := g.ChangeOf(ctx, rebase.Commit); err != nil || change == "" || got != change {
				t.Fatalf("the rebased commit is change %q, %v; want %q", got, err, change)
			}
			if got := fileAt(t, f, rebase.Snapshot, masonWrote); got != jujutsuConflicts["resume"][masonWrote] {
				t.Fatalf("the snapshot holds %q", got)
			}
			w, _, found, err := newUnitWorkspaces(f.s.cfg, repository).find(ctx, stream, "resume")
			must(t, err)
			if !found {
				t.Fatal("resume has no workspace")
			}
			if marked, err := g.MarkedFiles(w, []string{masonWrote}); err != nil || !slices.Equal(marked, []string{masonWrote}) {
				t.Fatalf("resume's workspace marks %v: %v", marked, err)
			}
			var outcomes []string
			for _, tr := range allTransitions(t, f.trace, stream) {
				if tr.Subject == rebaseSubject("resume") && tr.From != "" && !strings.HasPrefix(tr.To, "requested-") {
					outcomes = append(outcomes, tr.To)
				}
			}
			if !slices.Equal(outcomes, []string{"conflicted-1"}) {
				t.Fatalf("resume's rebase outcomes %v", outcomes)
			}
			for _, unit := range []string{"upload", "audit"} {
				w, _, found, err := newUnitWorkspaces(f.s.cfg, repository).find(ctx, stream, unit)
				must(t, err)
				if !found {
					t.Fatalf("unit %s has no workspace", unit)
				}
				for name, content := range jujutsuConflicts[unit] {
					data, err := os.ReadFile(filepath.Join(w.Path, name))
					if content == "" && !errors.Is(err, os.ErrNotExist) || content != "" && string(data) != content {
						t.Fatalf("unit %s's %s after the restore: %q %v", unit, name, data, err)
					}
				}
			}
		})
	}
}

// A drift rebase on Jujutsu workspaces cut short after its replay is
// recorded or after the feature branch moved is restored and retried, and
// ends where an uninterrupted one does: the feature branch at the replay of
// its old tip onto upstream, the feature workspace on it with its files, and
// the rebase and the seal's move recorded once.
func TestJujutsuDriftRebaseCutShortIsRestoredAndRebasesOnce(t *testing.T) {
	requireJJ(t)
	t.Parallel()
	ctx := context.Background()
	f := newDebateFixtureWith(t, 1, 1, "", func(opts *Options) { onWorkspaces(t, *opts, config.WorkspacesJujutsu) })
	f.upstream(t)
	stream, _ := f.builtAs(t, "restored-drift")
	f.stop(t)
	repository, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	t.Cleanup(func() { repository.Close() })
	d := drifter{&foreman{masons: newMasonController(f.s, repository)}}
	moveFeature(t, f, stream, map[string]string{"internal/trace/resume.go": "package trace\n"})
	g := providerOf(t, featureWorkspaces(f.s.cfg, repository), stream)
	for k, step := range []string{"drift-replayed", "drift-moved"} {
		k++
		before := featureTip(t, f, stream)
		upstream := advanceUpstream(t, f, map[string]string{"UPSTREAM-" + step + ".md": "upstream\n"})
		op := requestDrift(t, d, stream)
		requested, err := d.requestedAt(stream, op.ID)
		must(t, err)
		if _, err := d.Apply(stopAt(t, f.s, step), op); err == nil || !strings.Contains(err.Error(), "the service stopped") {
			t.Fatalf("the drift rebase was not cut short at %s: %v", step, err)
		}
		entry, changed := interrupted(t, f, branchesDirectory)
		if changed != (step == "drift-moved") {
			t.Fatalf("the drift rebase cut short at %s changed the operation log: %t", step, changed)
		}
		must(t, f.s.recoverWorkspaces(ctx, f.s.cfg))
		restoredTo(t, f, branchesDirectory, entry, changed)
		if result := settleOperation(t, f.s, repository, stream, op, d); result.Outcome != "succeeded" {
			t.Fatalf("the retried drift rebase %+v", result)
		}

		want, conflicts, err := g.Replay(ctx, before, upstream, requested)
		if err != nil || len(conflicts) != 0 {
			t.Fatalf("the uninterrupted replay %s %v: %v", want, conflicts, err)
		}
		if tip := featureTip(t, f, stream); tip != want {
			t.Fatalf("drift rebase %d cut short at %s left the branch at %s, not %s", k, step, tip, want)
		}
		w, found, err := g.Workspace(ctx, string(stream))
		must(t, err)
		if !found {
			t.Fatal("the feature branch has no workspace")
		}
		onCommit(t, g, w, want)
		var outcomes []string
		for _, r := range driftRecords(t, repository, stream) {
			if r.Drift == k {
				outcomes = append(outcomes, r.Outcome)
			}
		}
		if !slices.Equal(outcomes, []string{driftReplayed, driftRebased}) {
			t.Fatalf("drift rebase %d recorded %q", k, outcomes)
		}
		if all := seals(t, repository, stream); len(all) != k+1 || all[k].Base.Commit != upstream {
			t.Fatalf("seals after drift rebase %d: %+v", k, all)
		}
	}
}
