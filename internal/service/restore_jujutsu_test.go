package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/vcs"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
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

// checkpoints returns the checkpoint files the workspaces under directory
// hold.
func checkpoints(t *testing.T, f *shedFixture, directory string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(jujutsuRepository(f, directory), ".jj", "osmia-checkpoints", "*.json"))
	must(t, err)
	return paths
}

// checkpointEntry returns the operation-log entry of the one checkpoint an
// interrupted attempt left in the workspaces under directory.
func checkpointEntry(t *testing.T, f *shedFixture, directory string) string {
	t.Helper()
	paths := checkpoints(t, f, directory)
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

// interrupted returns the entry a cancelled local step restored before
// releasing the clone lock. An actual process crash instead leaves a file
// for startup recovery, which this helper also accepts.
func interrupted(t *testing.T, f *shedFixture, directory string) (string, bool) {
	t.Helper()
	if len(checkpoints(t, f, directory)) > 0 {
		entry := checkpointEntry(t, f, directory)
		return entry, !strings.HasPrefix(operationLog(t, f, directory)[0], entry+" ")
	}
	for _, line := range operationLog(t, f, directory) {
		if _, entry, ok := strings.Cut(line, " restore to operation "); ok {
			return entry, true
		}
	}
	// Restoring the current entry records no new operation when the step
	// changed no jj state (for example, a Git-only squash or replay).
	log := operationLog(t, f, directory)
	if len(log) > 0 {
		entry, _, _ := strings.Cut(log[0], " ")
		return entry, false
	}
	t.Fatalf("the cancelled %s local step has no operation log", directory)
	return "", false
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
	if paths := checkpoints(t, f, directory); len(paths) != 0 {
		t.Fatalf("checkpoints left after the restore: %v", paths)
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
// feature branch moved or after its record restores the checkpoint before releasing the clone lock. The restarted
// service retries the landing from its recorded branch effect. The retry ends where an
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
			for _, tr := range allTransitions(t, f.s.cfg.Root, f.project, stream) {
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
			for _, tr := range allTransitions(t, f.s.cfg.Root, f.project, stream) {
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
	base := f.upstream(t)
	stream, repository := seedBuild(t, f, "restored-drift", validPlan, config.WorkspacesJujutsu, base)
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

// A landing attempt on Jujutsu workspaces that fails while the service keeps
// running settles its checkpoint: a start finds nothing to restore, and the
// retry goes on from what the attempt left, landing once.
func TestJujutsuLandingThatFailsWithoutAStopKeepsNoCheckpoint(t *testing.T) {
	requireJJ(t)
	t.Parallel()
	ctx := context.Background()
	f, stream, repository := newApprovedFixtureOn(t, "failed-landing", config.WorkspacesJujutsu)
	lands := &foreman{masons: newMasonController(f.s, repository)}
	must(t, lands.Pass(ctx))
	ops := landOperations(t, repository, stream)
	if len(ops) != 1 {
		t.Fatalf("landing operations %+v", ops)
	}
	op := ops[0].Operation
	in, err := decodeLand(op)
	must(t, err)
	failed := false
	f.s.boundary = func(name string) error {
		if name == "land-advanced" && !failed {
			failed = true
			return errors.New("the landing failed")
		}
		return nil
	}
	t.Cleanup(func() { f.s.boundary = nil })
	if _, err := lands.Apply(ctx, op); err == nil || !strings.Contains(err.Error(), "the landing failed") {
		t.Fatalf("the landing did not fail at land-advanced: %v", err)
	}
	if left := checkpoints(t, f, branchesDirectory); len(left) != 0 {
		t.Fatalf("a failed attempt left checkpoints %v", left)
	}
	landed := featureTip(t, f, stream)
	log := operationLog(t, f, branchesDirectory)
	must(t, f.s.recoverWorkspaces(ctx, f.s.cfg))
	if after := operationLog(t, f, branchesDirectory); !slices.Equal(after, log) {
		t.Fatalf("a start after a failed attempt changed the operation log:\n%s", strings.Join(after, "\n"))
	}
	if result := settleOperation(t, f.s, repository, stream, op, lands); result.Outcome != "succeeded" {
		t.Fatalf("the retried landing %+v", result)
	}
	if commits := f.landedCommits(t, stream, in.Base); !slices.Equal(commits, []string{landed}) {
		t.Fatalf("the feature branch gained %v, want the failed attempt's %s", commits, landed)
	}
	var merged []string
	for _, tr := range allTransitions(t, f.s.cfg.Root, f.project, stream) {
		if tr.Subject == trace.UnitSubject(in.Unit) && tr.To == UnitMerged {
			merged = append(merged, tr.ID)
		}
	}
	if len(merged) != 1 {
		t.Fatalf("merged %v", merged)
	}
}

// A conflicted drift rebase on Jujutsu workspaces cut short while its
// resolution is staged, and again after its approved resolution moved the
// feature branch, restores both the drift resolution and feature branch workspaces
// before releasing the clone lock. The
// resolution goes on from the mason's files to one candidate, reviewed once,
// and the approved drift rebase ends where an uninterrupted one does: the
// feature branch and its workspace at the approved candidate, the
// resolution workspace removed, and the rebase and the seal's move recorded
// once.
func TestJujutsuDriftResolutionCutShortIsRestoredAndResolvesOnce(t *testing.T) {
	requireJJ(t)
	t.Parallel()
	ctx := context.Background()
	f := newDebateFixtureWith(t, 1, 1, "", func(opts *Options) { onWorkspaces(t, *opts, config.WorkspacesJujutsu) })
	base := f.upstream(t)
	stream, repository := seedBuild(t, f, "restored-resolution", validPlan, config.WorkspacesJujutsu, base)
	d := drifter{&foreman{masons: newMasonController(f.s, repository)}}
	before, upstream, op := conflictedDrift(t, f, d, stream)
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution of CODEOWNERS")
	resolved := "/internal/ @upstream @feature\n"
	must(t, os.WriteFile(filepath.Join(resolutionWorkspace(t, f, stream).Path, "CODEOWNERS"), []byte(resolved), 0600))
	completeDriftTurn(t, f, repository, stream, driftMasonAgent, resolvedDone("Kept both owners"))

	// cutShort cuts the drift rebase's next attempt short at step and has a
	// start restore what it left, and returns whether the attempt changed
	// the resolution workspaces' Jujutsu state.
	cutShort := func(step string) (feature, resolution bool) {
		t.Helper()
		if _, err := d.Apply(stopAt(t, f.s, step), op); err == nil || !strings.Contains(err.Error(), "the service stopped") {
			t.Fatalf("the drift rebase was not cut short at %s: %v", step, err)
		}
		features, feature := interrupted(t, f, branchesDirectory)
		resolutions, resolution := interrupted(t, f, driftsDirectory)
		must(t, f.s.recoverWorkspaces(ctx, f.s.cfg))
		restoredTo(t, f, branchesDirectory, features, feature)
		restoredTo(t, f, driftsDirectory, resolutions, resolution)
		return feature, resolution
	}

	cutShort("drift-staging")
	awaitResolution(t, f.s, d, stream, op, "review 1 of candidate")
	var candidates []string
	for _, r := range driftRecords(t, repository, stream) {
		if r.Outcome == driftResolved {
			candidates = append(candidates, r.Candidate)
		}
	}
	if len(candidates) != 1 {
		t.Fatalf("the resolution was staged into candidates %v", candidates)
	}
	candidate := candidates[0]
	g := providerOf(t, driftWorkspaces(f.s.cfg, repository), stream)
	if stored, err := g.StoredConflicts(ctx, candidate); err != nil || len(stored) != 0 || parentOf(t, f, candidate) != upstream || fileAt(t, f, candidate, "CODEOWNERS") != resolved {
		t.Fatalf("the resolved candidate %s stores %v: %v", candidate, stored, err)
	}
	if ids := turnIDs(t, repository, stream, driftMasonAgent); !slices.Equal(ids, []string{driftResolveTurnID(1, 1)}) {
		t.Fatalf("drift mason turns %v", ids)
	}
	if ids := turnIDs(t, repository, stream, driftReviewerAgent); len(ids) != 1 {
		t.Fatalf("drift reviewer turns %v", ids)
	}
	if tip := featureTip(t, f, stream); tip != before {
		t.Fatalf("the feature branch moved to %s before review", tip)
	}

	completeDriftTurn(t, f, repository, stream, driftReviewerAgent, driftVerdict(t, approvedResolution))
	if feature, resolution := cutShort("drift-moved"); !feature || !resolution {
		t.Fatalf("the attempt cut short after the branch moved changed the feature workspaces: %t, the resolution workspaces: %t", feature, resolution)
	}
	if result := settleOperation(t, f.s, repository, stream, op, d); result.Outcome != "succeeded" {
		t.Fatalf("the retried drift rebase %+v", result)
	}
	if tip := featureTip(t, f, stream); tip != candidate {
		t.Fatalf("the feature branch is at %s, not the approved candidate %s", tip, candidate)
	}
	features := providerOf(t, featureWorkspaces(f.s.cfg, repository), stream)
	w, found, err := features.Workspace(ctx, string(stream))
	must(t, err)
	if !found {
		t.Fatal("the feature branch has no workspace")
	}
	onCommit(t, features, w, candidate)
	if _, found, err := g.Workspace(ctx, string(stream)); err != nil || found {
		t.Fatalf("the resolution workspace is still there: %t %v", found, err)
	}
	records := driftRecords(t, repository, stream)
	if last := records[len(records)-1]; last.Outcome != driftRebased || last.Commit != candidate {
		t.Fatalf("the drift rebase's last record %+v", last)
	}
	transitions, err := trace.Read[trace.Transition](repository, stream)
	must(t, err)
	if n := len(slices.DeleteFunc(transitions, func(tr trace.Transition) bool { return tr.ID != "drift-1-rebased" })); n != 1 {
		t.Fatalf("the drift rebase recorded its outcome %d times", n)
	}
	if all := seals(t, repository, stream); len(all) != 2 || all[1].Base.Commit != upstream {
		t.Fatalf("seals %+v", all)
	}
}

// A's cancelled or settled-but-unrecorded local step cannot leave a checkpoint
// that a restart would use to undo B's completed landing on the same clone.
func TestJujutsuLocalStepInterleavingKeepsCompletedLanding(t *testing.T) {
	requireJJ(t)
	t.Parallel()
	for _, cancelled := range []bool{true, false} {
		t.Run(fmt.Sprintf("cancelled-%t", cancelled), func(t *testing.T) {
			ctx := context.Background()
			f, a, repository := newApprovedFixtureOn(t, "interleaved-a", config.WorkspacesJujutsu)
			base := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
			b := recordBuild(t, f, repository, "interleaved-b", independentPlan, config.WorkspacesJujutsu, base)
			seedReview(t, f, repository, b, "resume", resumeReport)
			approveDirectly(t, f.s, repository, b, "resume", "spec#1")
			lands := &foreman{masons: newMasonController(f.s, repository)}
			for _, stream := range []config.WorkstreamID{a, b} {
				review, result := approvedReview(t, repository, stream, "resume")
				must(t, lands.request(ctx, stream, "resume", review, result))
			}
			aOp := landOperations(t, repository, a)[0].Operation
			bOp := landOperations(t, repository, b)[0].Operation
			stopped, cancel := context.WithCancel(ctx)
			defer cancel()
			f.s.boundary = func(step string) error {
				if step == "land-advanced" {
					if cancelled {
						cancel()
					}
					return errors.New("interrupted local step")
				}
				return nil
			}
			_, err := lands.Apply(stopped, aOp)
			if err == nil || !strings.Contains(err.Error(), "interrupted local step") {
				t.Fatalf("A: %v", err)
			}
			if left := checkpoints(t, f, branchesDirectory); len(left) != 0 {
				t.Fatalf("A unlocked with checkpoints %v", left)
			}
			f.s.boundary = nil
			bResult := settleOperation(t, f.s, repository, b, bOp, lands)
			if bResult.Outcome != "succeeded" {
				t.Fatalf("B: %+v", bResult)
			}
			bCommit := featureTip(t, f, b)
			bProvider := providerOf(t, featureWorkspaces(f.s.cfg, repository), b)
			bWorkspace, found, err := bProvider.Workspace(ctx, string(b))
			must(t, err)
			if !found {
				t.Fatal("B has no workspace")
			}
			metadataPath := filepath.Join(bWorkspace.Path, ".jj", "osmia.json")
			before, err := os.ReadFile(metadataPath)
			must(t, err)
			must(t, f.s.recoverWorkspaces(ctx, f.s.cfg))
			observed, err := lands.Inspect(ctx, bOp)
			must(t, err)
			if observed.State != coreadapter.EffectCompleted || observed.Result == nil || !reflect.DeepEqual(*observed.Result, bResult) {
				t.Fatalf("B inspect: %+v", observed)
			}
			// A resumes from the landed branch effect; neither landing commits again.
			commits := 0
			f.s.boundary = func(step string) error {
				if step == "land-committing" {
					commits++
				}
				return nil
			}
			t.Cleanup(func() { f.s.boundary = nil })
			if result := settleOperation(t, f.s, repository, a, aOp, lands); result.Outcome != "succeeded" {
				t.Fatalf("A retry: %+v", result)
			}
			if commits != 0 {
				t.Fatalf("A repeated its settled local effect %d times", commits)
			}
			after, err := os.ReadFile(metadataPath)
			must(t, err)
			if !bytes.Equal(before, after) || featureTip(t, f, b) != bCommit {
				t.Fatal("B's metadata or branch changed during A recovery")
			}
			onCommit(t, bProvider, bWorkspace, bCommit)
			if docs := streamDocuments(t, repository, b, landingDocument("resume")); len(docs) != 1 {
				t.Fatalf("B landing recorded %d times", len(docs))
			}
		})
	}
}

func TestJujutsuCapturedFilesSurviveAnotherLocalStepRecovery(t *testing.T) {
	requireJJ(t)
	t.Parallel()
	ctx := context.Background()
	f, stream, repository := newApprovedFixtureOn(t, "captured-files", config.WorkspacesJujutsu)
	units := newUnitWorkspaces(f.s.cfg, repository)
	w, _, found, err := units.find(ctx, stream, "resume")
	must(t, err)
	if !found {
		t.Fatal("no unit workspace")
	}
	g := providerOf(t, units.streamWorkspaces, stream)
	view := t.TempDir()
	must(t, mirror(w.Path, view))
	must(t, os.WriteFile(filepath.Join(view, "CAPTURED.md"), []byte("completed turn output\n"), 0600))
	must(t, captureFiles(ctx, g, view, w.Path))
	stopped, cancel := context.WithCancel(ctx)
	defer cancel()
	err = workspace.LocalStep(stopped, "another-step", f.s.now(), []workspace.Provider{g}, func(local context.Context) error {
		if _, err := g.Record(local, w); err != nil {
			return err
		}
		cancel()
		return stopped.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("local step: %v", err)
	}
	must(t, f.s.recoverWorkspaces(ctx, f.s.cfg))
	if data, err := os.ReadFile(filepath.Join(w.Path, "CAPTURED.md")); err != nil || string(data) != "completed turn output\n" {
		t.Fatalf("captured files after recovery: %q: %v", data, err)
	}
}

// The child is killed by its jj wrapper after mirroring the files but before
// snapshotting them. No deferred settlement or view cleanup can run.
func TestJujutsuCopyCrashHelper(t *testing.T) {
	clone := os.Getenv("OSMIA_COPY_CRASH_CLONE")
	if clone == "" {
		return
	}
	g := &workspace.Jujutsu{Clone: clone, Directory: os.Getenv("OSMIA_COPY_CRASH_DIRECTORY"), JJ: os.Getenv("OSMIA_COPY_CRASH_JJ")}
	if err := captureFiles(context.Background(), g, os.Getenv("OSMIA_COPY_CRASH_SOURCE"), os.Getenv("OSMIA_COPY_CRASH_TARGET")); err != nil {
		t.Fatal(err)
	}
	t.Fatal("copy returned without crashing")
}

func TestJujutsuInterruptedCopyRecoversFromTurnView(t *testing.T) {
	requireJJ(t)
	t.Parallel()
	for _, directory := range []string{unitsDirectory, driftsDirectory} {
		t.Run(directory, func(t *testing.T) {
			ctx := context.Background()
			f, stream, repository := newApprovedFixtureOn(t, "copy-recovery-"+directory, config.WorkspacesJujutsu)
			providers := newUnitWorkspaces(f.s.cfg, repository).streamWorkspaces
			agent := masonAgent("resume")
			name := unitName(stream, "resume")
			if directory == driftsDirectory {
				providers = driftWorkspaces(f.s.cfg, repository)
				agent, name = driftMasonAgent, string(stream)
			}
			g := providerOf(t, providers, stream)
			base := featureTip(t, f, stream)
			if directory == driftsDirectory {
				_, err := g.Acquire(ctx, vcs.Request{Name: name, Branch: driftBranch(stream, 1), Ref: base})
				must(t, err)
			}
			w, found, err := g.Workspace(ctx, name)
			must(t, err)
			if !found {
				t.Fatal("copy target is missing")
			}
			m := newMasonController(f.s, repository)
			must(t, (drifter{&foreman{masons: m}}).ensureThread(ctx, stream, agent, masonRole))
			th, err := repository.Thread(stream, masonAgent("resume"))
			must(t, err)
			req := th.Turns[0].Request
			req.ID, req.TurnID = "request-copy-recovery", "copy-recovery"
			req.AgentID, req.ThreadID, req.At = agent, agent, f.s.now()
			_, err = repository.EnqueueTurn(ctx, req)
			must(t, err)
			root := turnViews(f.s.cfg, repository.Project(), stream, agent, req.TurnID)
			must(t, os.MkdirAll(root, 0700))
			paths, err := viewPaths(w.Path)
			must(t, err)
			view, err := (isolation.Views{Directory: root}).Create(ctx, coreadapter.Workspace{Directory: w.Path, Access: coreadapter.ReadWrite}, paths)
			must(t, err)
			defer view.Release(ctx)
			must(t, os.WriteFile(filepath.Join(root, "ready"), []byte(filepath.Base(view.Workspace().Directory)), 0600))
			must(t, os.WriteFile(filepath.Join(view.Workspace().Directory, "CAPTURED.md"), []byte("surviving turn output\n"), 0600))
			_, err = repository.ClaimTurn(ctx, stream, agent, "copying", filepath.Join(f.s.cfg.Root.String(), "threads", string(stream), agent, req.TurnID), f.s.now())
			must(t, err)
			jj, err := exec.LookPath("jj")
			must(t, err)
			wrapper := filepath.Join(t.TempDir(), "jj")
			must(t, os.WriteFile(wrapper, []byte("#!/bin/sh\ncase \" $* \" in\n *' log --no-graph --revisions @ '*) kill -KILL \"$PPID\"; exit 0;;\nesac\nexec \""+jj+"\" \"$@\"\n"), 0700))
			childCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			child := exec.CommandContext(childCtx, os.Args[0], "-test.run=^TestJujutsuCopyCrashHelper$")
			child.Env = append(os.Environ(), "OSMIA_COPY_CRASH_CLONE="+f.clone, "OSMIA_COPY_CRASH_DIRECTORY="+g.(*workspace.Jujutsu).Directory, "OSMIA_COPY_CRASH_JJ="+wrapper,
				"OSMIA_COPY_CRASH_SOURCE="+view.Workspace().Directory, "OSMIA_COPY_CRASH_TARGET="+w.Path)
			out, err := child.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != -1 || childCtx.Err() != nil {
				t.Fatalf("copy crash: %v\n%s", err, out)
			}
			if left := checkpoints(t, f, directory); len(left) != 1 {
				t.Fatalf("crash left checkpoints %v", left)
			}
			must(t, repository.Close())
			repository, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
			must(t, err)
			defer repository.Close()
			m.repository = repository
			must(t, f.s.recoverWorkspaces(ctx, f.s.cfg))
			if _, err := os.Stat(filepath.Join(w.Path, "CAPTURED.md")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("interrupted copy was not restored: %v", err)
			}
			for range 2 {
				must(t, m.recoverView(ctx, stream, agent, func() (string, error) { return w.Path, nil }))
			}
			data, err := os.ReadFile(filepath.Join(w.Path, "CAPTURED.md"))
			must(t, err)
			if string(data) != "surviving turn output\n" {
				t.Fatalf("recovered copy: %q", data)
			}
			th, err = repository.Thread(stream, agent)
			must(t, err)
			last := th.Turns[len(th.Turns)-1]
			if th.Active != "" || last.Request.TurnID != req.TurnID || last.Status() != "interrupted" {
				t.Fatalf("copy recovery did not settle its original claim: %+v", th)
			}
			if left := checkpoints(t, f, directory); len(left) != 0 {
				t.Fatalf("recovery unlocked with checkpoints %v", left)
			}
		})
	}
}
