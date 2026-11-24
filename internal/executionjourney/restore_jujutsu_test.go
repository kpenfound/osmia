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

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestJujutsuLandingCutShortIsRestoredAndLandsOnce(t *testing.T) {
	intakefixture.RequireJJ(t)
	t.Parallel()
	for _, step := range []string{"land-committed", "land-advanced", "land-recorded"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f, stream, repository := journey.NewApprovedFixtureOn(t, "restored-"+strings.TrimPrefix(step, "land-"), config.WorkspacesJujutsu)
			lands := &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}
			fixturekit.Must(t, lands.Pass(ctx))
			ops := journey.LandOperations(t, repository, stream)
			if len(ops) != 1 {
				t.Fatalf("landing operations %+v", ops)
			}
			op := ops[0].Operation
			in, err := execution.DecodeLand(op)
			fixturekit.Must(t, err)
			review, result := journey.ApprovedReview(t, repository, stream, in.Unit)
			message, err := lands.Driver().Message(stream, in, review, result, op.ID)
			fixturekit.Must(t, err)
			requested, err := lands.Driver().RequestedAt(stream, op.ID)
			fixturekit.Must(t, err)
			g := intakefixture.ProviderOf(t, intake.FeatureWorkspaces(f.S.Cfg, repository), stream)
			want, err := g.Squash(ctx, in.Base, in.Candidate, message, requested)
			fixturekit.Must(t, err)

			if _, err := lands.Apply(f.S.StopAt(t, step), op); err == nil || !strings.Contains(err.Error(), "the service stopped") {
				t.Fatalf("the landing was not cut short at %s: %v", step, err)
			}
			entry, changed := f.Jujutsu().Interrupted(t, intake.BranchesDirectory)
			if step == "land-advanced" {
				fixturekit.Must(t, repository.Close())
				f.Start(t)
				f.AwaitMerged(t, stream, in.Unit)
				f.Stop(t)
				if repository, err = trace.Open(f.S.Cfg.Root, f.S.Cfg.Project); err != nil {
					t.Fatal(err)
				}
			} else {
				fixturekit.Must(t, journey.InspectForTest(f.S).RecoverWorkspaces(ctx, f.S.Cfg))
				journey.SettleOperation(t, f.S, repository, stream, op, lands)
			}
			defer repository.Close()
			f.Jujutsu().RestoredTo(t, intake.BranchesDirectory, entry, changed)

			if tip := journey.FeatureTip(t, f, stream); tip != want {
				t.Fatalf("the feature branch is at %s, not the uninterrupted landing's %s", tip, want)
			}
			if commits := landedCommits(f, t, stream, in.Base); !slices.Equal(commits, []string{want}) {
				t.Fatalf("the feature branch gained %v", commits)
			}
			var merged, landed []string
			for _, tr := range intakefixture.AllTransitions(t, f.S.Cfg.Root, f.Project, stream) {
				if tr.Subject == trace.UnitSubject(in.Unit) && tr.To == intake.UnitMerged {
					merged = append(merged, tr.ID)
				}
				if tr.Subject == execution.LandingSubject(in.Unit) && strings.HasPrefix(tr.To, "landed-") {
					landed = append(landed, tr.ID)
				}
			}
			if len(merged) != 1 || len(landed) != 1 {
				t.Fatalf("merged %v, landed %v", merged, landed)
			}
			var landing api.UnitLanding
			fixturekit.Must(t, json.Unmarshal([]byte(intakefixture.StreamDocuments(t, repository, stream, execution.LandingDocument(in.Unit))[0].Content), &landing))
			if landing.Commit != want || strings.TrimSpace(landing.Message) != strings.TrimSpace(message) {
				t.Fatalf("landing.json records %s:\n%s", landing.Commit, landing.Message)
			}
			w, found, err := g.Workspace(ctx, string(stream))
			fixturekit.Must(t, err)
			if !found {
				t.Fatal("the feature branch has no workspace")
			}
			intakefixture.OnCommit(t, g, w, want)
		})
	}
}

func TestJujutsuUnitRebaseCutShortIsRestoredAndRebasesOnce(t *testing.T) {
	intakefixture.RequireJJ(t)
	t.Parallel()
	for _, step := range []string{"rebase-snapshotted", "rebase-moved"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f, stream, repository, lands, landed, ops := newJujutsuLandedFixture(t, "restored-"+strings.TrimPrefix(step, "rebase-"))
			r := journey.Rebaser{Foreman: lands}
			if _, err := r.Apply(f.S.StopAt(t, step), ops["resume"]); err == nil || !strings.Contains(err.Error(), "the service stopped") {
				t.Fatalf("the rebase was not cut short at %s: %v", step, err)
			}
			entry, changed := f.Jujutsu().Interrupted(t, intake.UnitsDirectory)
			if !changed {
				t.Fatalf("the rebase cut short at %s changed nothing in the operation log", step)
			}
			fixturekit.Must(t, journey.InspectForTest(f.S).RecoverWorkspaces(ctx, f.S.Cfg))
			f.Jujutsu().RestoredTo(t, intake.UnitsDirectory, entry, changed)
			if result := journey.SettleOperation(t, f.S, repository, stream, ops["resume"], r); result.Outcome != "succeeded" {
				t.Fatalf("the retried rebase %+v", result)
			}

			g := intakefixture.ProviderOf(t, journey.NewUnitWorkspaces(f.S.Cfg, repository).StreamWorkspaces, stream)
			rebases := journey.UnitRebases(t, repository, stream, "resume")
			if len(rebases) != 1 {
				t.Fatalf("resume's rebases %+v", rebases)
			}
			rebase := rebases[0]
			if tip, _, err := g.Branch(ctx, execution.UnitBranch(stream, "resume")); err != nil || tip != rebase.Commit || journey.ParentOf(t, f, tip) != landed {
				t.Fatalf("resume's branch is at %s, recorded %s, on %s: %v", tip, rebase.Commit, landed, err)
			}
			if stored, err := g.StoredConflicts(ctx, rebase.Commit); err != nil || !slices.Equal(stored, jujutsuConflicted["resume"]) || !slices.Equal(rebase.Conflicts, stored) {
				t.Fatalf("the rebased commit stores %v, recorded %v: %v", stored, rebase.Conflicts, err)
			}
			change, err := execution.ReadUnitChange(repository, stream, "resume")
			fixturekit.Must(t, err)
			if got, err := g.ChangeOf(ctx, rebase.Commit); err != nil || change == "" || got != change {
				t.Fatalf("the rebased commit is change %q, %v; want %q", got, err, change)
			}
			if got := journey.FileAt(t, f, rebase.Snapshot, fixturekit.MasonWrote); got != jujutsuConflicts["resume"][fixturekit.MasonWrote] {
				t.Fatalf("the snapshot holds %q", got)
			}
			w, _, found, err := journey.NewUnitWorkspaces(f.S.Cfg, repository).Find(ctx, stream, "resume")
			fixturekit.Must(t, err)
			if !found {
				t.Fatal("resume has no workspace")
			}
			if marked, err := g.MarkedFiles(w, []string{fixturekit.MasonWrote}); err != nil || !slices.Equal(marked, []string{fixturekit.MasonWrote}) {
				t.Fatalf("resume's workspace marks %v: %v", marked, err)
			}
			var outcomes []string
			for _, tr := range intakefixture.AllTransitions(t, f.S.Cfg.Root, f.Project, stream) {
				if tr.Subject == execution.RebaseSubject("resume") && tr.From != "" && !strings.HasPrefix(tr.To, "requested-") {
					outcomes = append(outcomes, tr.To)
				}
			}
			if !slices.Equal(outcomes, []string{"conflicted-1"}) {
				t.Fatalf("resume's rebase outcomes %v", outcomes)
			}
			for _, unit := range []string{"upload", "audit"} {
				w, _, found, err := journey.NewUnitWorkspaces(f.S.Cfg, repository).Find(ctx, stream, unit)
				fixturekit.Must(t, err)
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
	intakefixture.RequireJJ(t)
	t.Parallel()
	ctx := context.Background()
	f, stream, repository := journey.NewApprovedFixtureOn(t, "failed-landing", config.WorkspacesJujutsu)
	lands := &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}
	fixturekit.Must(t, lands.Pass(ctx))
	ops := journey.LandOperations(t, repository, stream)
	if len(ops) != 1 {
		t.Fatalf("landing operations %+v", ops)
	}
	op := ops[0].Operation
	in, err := execution.DecodeLand(op)
	fixturekit.Must(t, err)
	failed := false
	f.S.Boundary = func(name string) error {
		if name == "land-advanced" && !failed {
			failed = true
			return errors.New("the landing failed")
		}
		return nil
	}
	t.Cleanup(func() { f.S.Boundary = nil })
	if _, err := lands.Apply(ctx, op); err == nil || !strings.Contains(err.Error(), "the landing failed") {
		t.Fatalf("the landing did not fail at land-advanced: %v", err)
	}
	if left := f.Jujutsu().Checkpoints(t, intake.BranchesDirectory); len(left) != 0 {
		t.Fatalf("a failed attempt left checkpoints %v", left)
	}
	landed := journey.FeatureTip(t, f, stream)
	log := f.Jujutsu().OperationLog(t, intake.BranchesDirectory)
	fixturekit.Must(t, journey.InspectForTest(f.S).RecoverWorkspaces(ctx, f.S.Cfg))
	if after := f.Jujutsu().OperationLog(t, intake.BranchesDirectory); !slices.Equal(after, log) {
		t.Fatalf("a start after a failed attempt changed the operation log:\n%s", strings.Join(after, "\n"))
	}
	if result := journey.SettleOperation(t, f.S, repository, stream, op, lands); result.Outcome != "succeeded" {
		t.Fatalf("the retried landing %+v", result)
	}
	if commits := landedCommits(f, t, stream, in.Base); !slices.Equal(commits, []string{landed}) {
		t.Fatalf("the feature branch gained %v, want the failed attempt's %s", commits, landed)
	}
	var merged []string
	for _, tr := range intakefixture.AllTransitions(t, f.S.Cfg.Root, f.Project, stream) {
		if tr.Subject == trace.UnitSubject(in.Unit) && tr.To == intake.UnitMerged {
			merged = append(merged, tr.ID)
		}
	}
	if len(merged) != 1 {
		t.Fatalf("merged %v", merged)
	}
}
