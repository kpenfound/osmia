package executionjourney

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

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
