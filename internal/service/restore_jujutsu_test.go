package service

import (
	"bytes"
	"context"
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
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

func stopAt(t *testing.T, s *Service, step string) context.Context {
	t.Helper()
	return intakefixture.StopAt(t, step, func(h func(string) error) { s.setBoundary(h) })
}

func jujutsuRepository(f *shedFixture, directory string) string {
	return intakefixture.JujutsuRepository(f.s.cfg.Root, f.s.cfg.Project.ID, directory)
}

func checkpoints(t *testing.T, f *shedFixture, directory string) []string {
	t.Helper()
	return intakefixture.Checkpoints(t, jujutsuRepository(f, directory))
}

func operationLog(t *testing.T, f *shedFixture, directory string) []string {
	t.Helper()
	return intakefixture.OperationLog(t, jujutsuRepository(f, directory), filepath.Dir(f.clone))
}

func interrupted(t *testing.T, f *shedFixture, directory string) (string, bool) {
	t.Helper()
	return intakefixture.Interrupted(t, directory, checkpoints(t, f, directory), operationLog(t, f, directory))
}

func restoredTo(t *testing.T, f *shedFixture, directory, entry string, changed bool) {
	t.Helper()
	intakefixture.RestoredTo(t, directory, entry, changed, operationLog(t, f, directory), checkpoints(t, f, directory))
}

func onCommit(t *testing.T, g workspace.Provider, w workspace.Worktree, commit string) {
	t.Helper()
	intakefixture.OnCommit(t, g, w, commit)
}

func TestJujutsuDriftRebaseCutShortIsRestoredAndRebasesOnce(t *testing.T) {
	intakefixture.RequireJJ(t)
	t.Parallel()
	ctx := context.Background()
	f := newDebateFixtureWith(t, 1, 1, "", func(opts *Options) { onWorkspaces(t, *opts, config.WorkspacesJujutsu) })
	base := f.upstream(t)
	stream, repository := seedBuild(t, f, "restored-drift", fixturekit.ValidPlan, config.WorkspacesJujutsu, base)
	d := drifter{&foreman{masons: newMasonController(f.s, repository)}}
	moveFeature(t, f, stream, map[string]string{"internal/trace/resume.go": "package trace\n"})
	g := intakefixture.ProviderOf(t, featureWorkspaces(f.s.cfg, repository), stream)
	for k, step := range []string{"drift-replayed", "drift-moved"} {
		k++
		before := featureTip(t, f, stream)
		upstream := advanceUpstream(t, f, map[string]string{"UPSTREAM-" + step + ".md": "upstream\n"})
		op := requestDrift(t, d, stream)
		requested, err := d.driver().RequestedAt(stream, op.ID)
		fixturekit.Must(t, err)
		if _, err := d.Apply(stopAt(t, f.s, step), op); err == nil || !strings.Contains(err.Error(), "the service stopped") {
			t.Fatalf("the drift rebase was not cut short at %s: %v", step, err)
		}
		entry, changed := interrupted(t, f, branchesDirectory)
		fixturekit.Must(t, f.s.recoverWorkspaces(ctx, f.s.cfg))
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
		fixturekit.Must(t, err)
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
		if all := intakefixture.Seals(t, repository, stream); len(all) != k+1 || all[k].Base.Commit != upstream {
			t.Fatalf("seals after drift rebase %d: %+v", k, all)
		}
	}
}

func TestJujutsuDriftResolutionCutShortIsRestoredAndResolvesOnce(t *testing.T) {
	intakefixture.RequireJJ(t)
	t.Parallel()
	ctx := context.Background()
	f := newDebateFixtureWith(t, 1, 1, "", func(opts *Options) { onWorkspaces(t, *opts, config.WorkspacesJujutsu) })
	base := f.upstream(t)
	stream, repository := seedBuild(t, f, "restored-resolution", fixturekit.ValidPlan, config.WorkspacesJujutsu, base)
	d := drifter{&foreman{masons: newMasonController(f.s, repository)}}
	before, upstream, op := conflictedDrift(t, f, d, stream)
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution of CODEOWNERS")
	resolved := "/internal/ @upstream @feature\n"
	fixturekit.Must(t, os.WriteFile(filepath.Join(resolutionWorkspace(t, f, stream).Path, "CODEOWNERS"), []byte(resolved), 0600))
	completeDriftTurn(t, f, repository, stream, driftMasonAgent, resolvedDone("Kept both owners"))

	cutShort := func(step string) (feature, resolution bool) {
		t.Helper()
		if _, err := d.Apply(stopAt(t, f.s, step), op); err == nil || !strings.Contains(err.Error(), "the service stopped") {
			t.Fatalf("the drift rebase was not cut short at %s: %v", step, err)
		}
		features, feature := interrupted(t, f, branchesDirectory)
		resolutions, resolution := interrupted(t, f, driftsDirectory)
		fixturekit.Must(t, f.s.recoverWorkspaces(ctx, f.s.cfg))
		restoredTo(t, f, branchesDirectory, features, feature)
		restoredTo(t, f, driftsDirectory, resolutions, resolution)
		return feature, resolution
	}

	cutShort("drift-staging")
	awaitResolution(t, f.s, d, stream, op, "review 1 of candidate")
	var candidates []string
	for _, r := range driftRecords(t, repository, stream) {
		if r.Outcome == intake.DriftResolved {
			candidates = append(candidates, r.Candidate)
		}
	}
	if len(candidates) != 1 {
		t.Fatalf("the resolution was staged into candidates %v", candidates)
	}
	candidate := candidates[0]
	g := intakefixture.ProviderOf(t, driftWorkspaces(f.s.cfg, repository), stream)
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
	features := intakefixture.ProviderOf(t, featureWorkspaces(f.s.cfg, repository), stream)
	w, found, err := features.Workspace(ctx, string(stream))
	fixturekit.Must(t, err)
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
	fixturekit.Must(t, err)
	if n := len(slices.DeleteFunc(transitions, func(tr trace.Transition) bool { return tr.ID != "drift-1-rebased" })); n != 1 {
		t.Fatalf("the drift rebase recorded its outcome %d times", n)
	}
	if all := intakefixture.Seals(t, repository, stream); len(all) != 2 || all[1].Base.Commit != upstream {
		t.Fatalf("seals %+v", all)
	}
}

// A's cancelled or settled-but-unrecorded local step cannot leave a checkpoint
// that a restart would use to undo B's completed landing on the same clone.
func TestJujutsuLocalStepInterleavingKeepsCompletedLanding(t *testing.T) {
	intakefixture.RequireJJ(t)
	t.Parallel()
	for _, cancelled := range []bool{true, false} {
		t.Run(fmt.Sprintf("cancelled-%t", cancelled), func(t *testing.T) {
			ctx := context.Background()
			f, a, repository := newApprovedFixtureOn(t, "interleaved-a", config.WorkspacesJujutsu)
			base := strings.TrimSpace(fixturekit.DemoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
			b := recordBuild(t, f, repository, "interleaved-b", independentPlan, config.WorkspacesJujutsu, base)
			seedReview(t, f, repository, b, "resume", resumeReport)
			approveDirectly(t, f.s, repository, b, "resume", "spec#1")
			lands := &foreman{masons: newMasonController(f.s, repository)}
			for _, stream := range []config.WorkstreamID{a, b} {
				review, result := approvedReview(t, repository, stream, "resume")
				fixturekit.Must(t, lands.request(ctx, stream, "resume", review, result))
			}
			aOp := landOperations(t, repository, a)[0].Operation
			bOp := landOperations(t, repository, b)[0].Operation
			stopped, cancel := context.WithCancel(ctx)
			defer cancel()
			f.s.setBoundary(func(step string) error {
				if step == "land-advanced" {
					if cancelled {
						cancel()
					}
					return errors.New("interrupted local step")
				}
				return nil
			})
			_, err := lands.Apply(stopped, aOp)
			if err == nil || !strings.Contains(err.Error(), "interrupted local step") {
				t.Fatalf("A: %v", err)
			}
			if left := checkpoints(t, f, branchesDirectory); len(left) != 0 {
				t.Fatalf("A unlocked with checkpoints %v", left)
			}
			f.s.setBoundary(nil)
			bResult := settleOperation(t, f.s, repository, b, bOp, lands)
			if bResult.Outcome != "succeeded" {
				t.Fatalf("B: %+v", bResult)
			}
			bCommit := featureTip(t, f, b)
			bProvider := intakefixture.ProviderOf(t, featureWorkspaces(f.s.cfg, repository), b)
			bWorkspace, found, err := bProvider.Workspace(ctx, string(b))
			fixturekit.Must(t, err)
			if !found {
				t.Fatal("B has no workspace")
			}
			metadataPath := filepath.Join(bWorkspace.Path, ".jj", "osmia.json")
			before, err := os.ReadFile(metadataPath)
			fixturekit.Must(t, err)
			fixturekit.Must(t, f.s.recoverWorkspaces(ctx, f.s.cfg))
			observed, err := lands.Inspect(ctx, bOp)
			fixturekit.Must(t, err)
			if observed.State != coreadapter.EffectCompleted || observed.Result == nil || !reflect.DeepEqual(*observed.Result, bResult) {
				t.Fatalf("B inspect: %+v", observed)
			}
			commits := 0
			f.s.setBoundary(func(step string) error {
				if step == "land-committing" {
					commits++
				}
				return nil
			})
			t.Cleanup(func() { f.s.setBoundary(nil) })
			if result := settleOperation(t, f.s, repository, a, aOp, lands); result.Outcome != "succeeded" {
				t.Fatalf("A retry: %+v", result)
			}
			if commits != 0 {
				t.Fatalf("A repeated its settled local effect %d times", commits)
			}
			after, err := os.ReadFile(metadataPath)
			fixturekit.Must(t, err)
			if !bytes.Equal(before, after) || featureTip(t, f, b) != bCommit {
				t.Fatal("B's metadata or branch changed during A recovery")
			}
			onCommit(t, bProvider, bWorkspace, bCommit)
			if docs := intakefixture.StreamDocuments(t, repository, b, landingDocument("resume")); len(docs) != 1 {
				t.Fatalf("B landing recorded %d times", len(docs))
			}
		})
	}
}

func TestJujutsuCapturedFilesSurviveAnotherLocalStepRecovery(t *testing.T) {
	intakefixture.RequireJJ(t)
	t.Parallel()
	ctx := context.Background()
	f, stream, repository := newApprovedFixtureOn(t, "captured-files", config.WorkspacesJujutsu)
	units := newUnitWorkspaces(f.s.cfg, repository)
	w, _, found, err := units.find(ctx, stream, "resume")
	fixturekit.Must(t, err)
	if !found {
		t.Fatal("no unit workspace")
	}
	g := intakefixture.ProviderOf(t, units.streamWorkspaces, stream)
	view := t.TempDir()
	fixturekit.Must(t, mirror(w.Path, view))
	fixturekit.Must(t, os.WriteFile(filepath.Join(view, "CAPTURED.md"), []byte("completed turn output\n"), 0600))
	fixturekit.Must(t, captureFiles(ctx, g, view, w.Path))
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
	fixturekit.Must(t, f.s.recoverWorkspaces(ctx, f.s.cfg))
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
	intakefixture.RequireJJ(t)
	t.Parallel()
	for _, directory := range []string{unitsDirectory, driftsDirectory} {
		t.Run(directory, func(t *testing.T) {
			ctx := context.Background()
			f, stream, repository := newApprovedFixtureOn(t, "copy-recovery-"+directory, config.WorkspacesJujutsu)
			providers := newUnitWorkspaces(f.s.cfg, repository).streamWorkspaces
			agent := masonAgent("resume")
			name := execution.UnitName(stream, "resume")
			if directory == driftsDirectory {
				providers = driftWorkspaces(f.s.cfg, repository)
				agent, name = driftMasonAgent, string(stream)
			}
			g := intakefixture.ProviderOf(t, providers, stream)
			base := featureTip(t, f, stream)
			if directory == driftsDirectory {
				_, err := g.Acquire(ctx, vcs.Request{Name: name, Branch: lifecycle.DriftBranch(stream, 1), Ref: base})
				fixturekit.Must(t, err)
			}
			w, found, err := g.Workspace(ctx, name)
			fixturekit.Must(t, err)
			if !found {
				t.Fatal("copy target is missing")
			}
			m := newMasonController(f.s, repository)
			fixturekit.Must(t, (drifter{&foreman{masons: m}}).ensureThread(ctx, stream, agent, masonRole))
			th, err := repository.Thread(stream, masonAgent("resume"))
			fixturekit.Must(t, err)
			req := th.Turns[0].Request
			req.ID, req.TurnID = "request-copy-recovery", "copy-recovery"
			req.AgentID, req.ThreadID, req.At = agent, agent, f.s.now()
			_, err = repository.EnqueueTurn(ctx, req)
			fixturekit.Must(t, err)
			root := execution.TurnViews(f.s.cfg, repository.Project(), stream, agent, req.TurnID)
			fixturekit.Must(t, os.MkdirAll(root, 0700))
			paths, err := viewPaths(w.Path)
			fixturekit.Must(t, err)
			view, err := (isolation.Views{Directory: root}).Create(ctx, coreadapter.Workspace{Directory: w.Path, Access: coreadapter.ReadWrite}, paths)
			fixturekit.Must(t, err)
			defer view.Release(ctx)
			fixturekit.Must(t, os.WriteFile(filepath.Join(root, "ready"), []byte(filepath.Base(view.Workspace().Directory)), 0600))
			fixturekit.Must(t, os.WriteFile(filepath.Join(view.Workspace().Directory, "CAPTURED.md"), []byte("surviving turn output\n"), 0600))
			_, err = repository.ClaimTurn(ctx, stream, agent, "copying", filepath.Join(f.s.cfg.Root.String(), "threads", string(stream), agent, req.TurnID), f.s.now())
			fixturekit.Must(t, err)
			jj, err := exec.LookPath("jj")
			fixturekit.Must(t, err)
			wrapper := filepath.Join(t.TempDir(), "jj")
			fixturekit.Must(t, os.WriteFile(wrapper, []byte("#!/bin/sh\ncase \" $* \" in\n *' log --no-graph --revisions @ '*) kill -KILL \"$PPID\"; exit 0;;\nesac\nexec \""+jj+"\" \"$@\"\n"), 0700))
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
			fixturekit.Must(t, repository.Close())
			repository, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
			fixturekit.Must(t, err)
			defer repository.Close()
			m.repository = repository
			fixturekit.Must(t, f.s.recoverWorkspaces(ctx, f.s.cfg))
			if _, err := os.Stat(filepath.Join(w.Path, "CAPTURED.md")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("interrupted copy was not restored: %v", err)
			}
			for range 2 {
				fixturekit.Must(t, m.driver().RecoverView(ctx, stream, agent, func() (string, error) { return w.Path, nil }))
			}
			data, err := os.ReadFile(filepath.Join(w.Path, "CAPTURED.md"))
			fixturekit.Must(t, err)
			if string(data) != "surviving turn output\n" {
				t.Fatalf("recovered copy: %q", data)
			}
			th, err = repository.Thread(stream, agent)
			fixturekit.Must(t, err)
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
