package lifecycle_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent/procs"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func newCleanupFixture(t *testing.T, backend string) (*shedFixture, config.WorkstreamID, *trace.Repository) {
	t.Helper()
	ctx := context.Background()
	f, _ := newParallelMasonFixtureOn(t, backend, 2, 3, disjointPlan)
	base := strings.TrimSpace(fixturekit.DemoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	stream, repository := seedBuild(t, f, "cleanup-"+backend, disjointPlan, backend, base)
	m := newMasonController(f.s, repository)
	for _, unit := range []string{"upload", "audit"} {
		b, found, err := m.read(stream)
		fixturekit.Must(t, err)
		if !found {
			t.Fatal("the workstream is not building")
		}
		if started, _, err := m.start(ctx, b, unit); err != nil || !started {
			t.Fatalf("%s did not start: %v", unit, err)
		}
		w, _, err := newUnitWorkspaces(f.s.cfg, repository).open(ctx, stream, unit)
		fixturekit.Must(t, err)
		fixturekit.Must(t, os.WriteFile(filepath.Join(w.Path, unit+".txt"), []byte(unit+"\n"), 0600))
	}
	return f, stream, repository
}
func newCleanup(f *shedFixture, repository *trace.Repository) *workspaceCleanup {
	return &workspaceCleanup{cfg: f.s.cfg, repository: repository, now: f.s.now}
}

func writeExport(t *testing.T, f *shedFixture, stream config.WorkstreamID, thread, turn string) string {
	t.Helper()
	dir := filepath.Join(f.s.cfg.Root.String(), lifecycle.ReviewInputsDirectory, string(f.project), string(stream), thread, turn)
	fixturekit.Must(t, os.MkdirAll(dir, 0700))
	fixturekit.Must(t, os.WriteFile(filepath.Join(dir, "candidate.txt"), []byte("candidate\n"), 0600))
	return dir
}

func assertThere(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("%s is gone: %v", path, err)
	}
}

func assertUnitWorkspace(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, unit string, want bool) {
	t.Helper()
	_, _, found, err := newUnitWorkspaces(f.s.cfg, repository).find(context.Background(), stream, unit)
	fixturekit.Must(t, err)
	dir := filepath.Join(f.s.cfg.Root.String(), intake.UnitsDirectory, string(f.project), string(stream), unit)
	if want {
		if !found {
			t.Fatalf("unit %s has no workspace", unit)
		}
		assertThere(t, dir)
		return
	}
	if found {
		t.Fatalf("unit %s still has a workspace", unit)
	}
	fixturekit.AssertGone(t, dir)
}

func branchFile(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, branch, name string) string {
	t.Helper()
	ctx := context.Background()
	g, err := newUnitWorkspaces(f.s.cfg, repository).Of(stream)
	fixturekit.Must(t, err)
	tip, exists, err := g.Branch(ctx, branch)
	fixturekit.Must(t, err)
	if !exists {
		t.Fatalf("branch %s is gone", branch)
	}
	dir := t.TempDir()
	fixturekit.Must(t, g.Export(ctx, tip, dir))
	data, err := os.ReadFile(filepath.Join(dir, name))
	fixturekit.Must(t, err)
	return string(data)
}
func recordUnitMerged(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string) {
	t.Helper()
	state, err := repository.Workflow(stream, trace.UnitSubject(unit))
	fixturekit.Must(t, err)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: trace.UnitSubject(unit) + "-" + intake.UnitMerged, Revision: 1, Project: repository.Project(), Workstream: stream, Unit: unit, At: time.Now().UTC(), Actor: execution.ForemanActor, Cause: "landing-" + unit}
	_, err = repository.Transact(context.Background(), trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: h, Subject: trace.UnitSubject(unit), From: state.Value, To: intake.UnitMerged, Reason: fmt.Sprintf("unit %s landed", unit)}})
	fixturekit.Must(t, err)
}

func TestCleanupRemovesTheWorkspaceOfAMergedUnit(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{config.WorkspacesGit, config.WorkspacesJujutsu} {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			f, stream, repository := newCleanupFixture(t, backend)
			defer repository.Close()
			ctx := context.Background()
			for _, unit := range []string{"upload", "audit"} {
				completeMasonTurn(t, f, repository, stream, unit, "built "+unit)
			}
			merged := writeExport(t, f, stream, intake.ReviewerAgent("upload"), intake.ReviewerAgent("upload")+"-review-1")
			inFlight := writeExport(t, f, stream, intake.ReviewerAgent("audit"), intake.ReviewerAgent("audit")+"-review-1")
			recordUnitMerged(t, repository, stream, "upload")

			c := newCleanup(f, repository)
			fixturekit.Must(t, c.Pass(ctx))
			assertUnitWorkspace(t, f, repository, stream, "upload", false)
			fixturekit.AssertGone(t, merged)
			assertUnitWorkspace(t, f, repository, stream, "audit", true)
			assertThere(t, inFlight)
			assertThere(t, filepath.Join(f.s.cfg.Root.String(), intake.BranchesDirectory, string(f.project), string(stream)))
			g, err := newUnitWorkspaces(f.s.cfg, repository).Of(stream)
			fixturekit.Must(t, err)
			if _, exists, err := g.Branch(ctx, execution.UnitBranch(stream, "upload")); err != nil || !exists {
				t.Fatalf("branch of the merged unit is gone: %v", err)
			}
			if len(c.failures) != 0 {
				t.Fatalf("cleanup failed: %v", c.failures)
			}
			fixturekit.Must(t, c.Pass(ctx))
			assertUnitWorkspace(t, f, repository, stream, "audit", true)
		})
	}
}

func TestCleanupKeepsTheWorkOfAnAbandonedWorkstream(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{config.WorkspacesGit, config.WorkspacesJujutsu} {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			f, stream, repository := newCleanupFixture(t, backend)
			defer repository.Close()
			ctx := context.Background()
			completeMasonTurn(t, f, repository, stream, "upload", "built upload")
			done := writeExport(t, f, stream, intake.ReviewerAgent("upload"), intake.ReviewerAgent("upload")+"-review-1")
			export := writeExport(t, f, stream, intake.ReviewerAgent("audit"), intake.ReviewerAgent("audit")+"-review-1")
			h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: intake.AbandonTransition, Revision: 1, Project: repository.Project(), Workstream: stream, At: f.s.now(), Actor: intake.OwnerActor, Cause: intake.AbandonTransition}
			_, err := repository.SetFeatureStateUnless(ctx, h, intake.AbandonedState, "superseded", intake.AbandonedState, intake.DeliveredState)
			fixturekit.Must(t, err)
			feature := filepath.Join(f.s.cfg.Root.String(), intake.BranchesDirectory, string(f.project), string(stream))

			c := newCleanup(f, repository)
			fixturekit.Must(t, c.Pass(ctx))
			assertUnitWorkspace(t, f, repository, stream, "upload", false)
			fixturekit.AssertGone(t, done)
			if got := branchFile(t, f, repository, stream, execution.UnitBranch(stream, "upload"), "upload.txt"); got != "upload\n" {
				t.Fatalf("upload's branch holds %q", got)
			}
			assertUnitWorkspace(t, f, repository, stream, "audit", true)
			assertThere(t, feature)
			assertThere(t, export)

			_, err = repository.CancelTurns(ctx, stream, f.s.now(), intake.AbandonActor, intake.CancelReason)
			fixturekit.Must(t, err)
			fixturekit.Must(t, c.Pass(ctx))
			if len(c.failures) != 0 {
				t.Fatalf("cleanup failed: %v", c.failures)
			}
			assertUnitWorkspace(t, f, repository, stream, "audit", false)
			if got := branchFile(t, f, repository, stream, execution.UnitBranch(stream, "audit"), "audit.txt"); got != "audit\n" {
				t.Fatalf("audit's branch holds %q", got)
			}
			fixturekit.AssertGone(t, feature)
			fixturekit.AssertGone(t, filepath.Join(f.s.cfg.Root.String(), lifecycle.ReviewInputsDirectory, string(f.project), string(stream)))
			fixturekit.AssertGone(t, filepath.Join(f.s.cfg.Root.String(), intake.UnitsDirectory, string(f.project), string(stream)))
			g, err := intake.FeatureWorkspaces(f.s.cfg, repository).Of(stream)
			fixturekit.Must(t, err)
			if _, exists, err := g.Branch(ctx, intake.FeatureBranch(stream)); err != nil || !exists {
				t.Fatalf("the feature branch is gone: %v", err)
			}
			fixturekit.Must(t, c.Pass(ctx))
		})
	}
}

func TestCleanupRemovesTheTurnDirectoriesOfAFinishedWorkstream(t *testing.T) {
	t.Parallel()
	f, stream, repository := newCleanupFixture(t, config.WorkspacesGit)
	defer repository.Close()
	ctx := context.Background()
	for _, unit := range []string{"upload", "audit"} {
		completeMasonTurn(t, f, repository, stream, unit, "built "+unit)
	}
	root, project := f.s.cfg.Root.String(), string(f.project)
	var turns []string
	for _, rel := range []string{
		filepath.Join("threads", project, string(stream), intake.MasonAgent("upload"), "turn-1", "transcript.jsonl"),
		filepath.Join("architect", project, string(stream), "draft-1", "session", "transcript.jsonl"),
		filepath.Join("shed", project, string(stream), "round-1-member-1", "session", "prompt.md"),
		filepath.Join("final", project, string(stream), "final-1", "session", "report.json"),
		filepath.Join("chief_of_staff", project, string(stream), "status.json"),
		filepath.Join("workspaces", project, string(stream), "notes.md"),
		filepath.Join("views", project, string(stream), "mason", "turn-1", "output.txt"),
		filepath.Join("checks", project, string(stream), "final-123", "output.txt"),
	} {
		path := filepath.Join(root, rel)
		fixturekit.Must(t, os.MkdirAll(filepath.Dir(path), 0700))
		fixturekit.Must(t, os.WriteFile(path, []byte("recorded\n"), 0600))
		turns = append(turns, filepath.Join(root, strings.Join(strings.Split(rel, string(filepath.Separator))[:3], string(filepath.Separator))))
	}

	c := newCleanup(f, repository)
	fixturekit.Must(t, c.Pass(ctx))
	for _, dir := range turns {
		assertThere(t, dir)
	}

	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: intake.AbandonTransition, Revision: 1, Project: repository.Project(), Workstream: stream, At: f.s.now(), Actor: intake.OwnerActor, Cause: intake.AbandonTransition}
	_, err := repository.SetFeatureStateUnless(ctx, h, intake.AbandonedState, "superseded", intake.AbandonedState, intake.DeliveredState)
	fixturekit.Must(t, err)
	_, err = repository.EnsureChiefOfStaff(ctx, stream, f.s.now(), intake.ServiceActor)
	fixturekit.Must(t, err)
	request := trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_owner-message", Revision: 1, Project: repository.Project(), Workstream: stream, At: f.s.now(), Actor: intake.OwnerActor, Cause: "fixture"}
	_, err = repository.EnqueueTurn(ctx, trace.TurnRequest{Header: request, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "owner-message",
		Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "You are the chief of staff.", Prompt: "Anything left?"})
	fixturekit.Must(t, err)
	fixturekit.Must(t, c.Pass(ctx))
	assertUnitWorkspace(t, f, repository, stream, "upload", false)
	fixturekit.AssertGone(t, filepath.Join(root, intake.BranchesDirectory, project, string(stream)))
	for _, dir := range turns {
		assertThere(t, dir)
	}

	_, err = repository.CancelTurns(ctx, stream, f.s.now(), intake.AbandonActor, intake.CancelReason)
	fixturekit.Must(t, err)
	fixturekit.Must(t, c.Pass(ctx))
	if len(c.failures) != 0 {
		t.Fatalf("cleanup failed: %v", c.failures)
	}
	for _, dir := range turns {
		fixturekit.AssertGone(t, dir)
	}
	if th, err := repository.Thread(stream, intake.MasonAgent("upload")); err != nil || len(th.Turns) == 0 {
		t.Fatalf("the trace lost the mason's turns: %+v %v", th, err)
	}
}

func TestUnitReviewKeepsOneExportPerThread(t *testing.T) {
	t.Parallel()
	f, stream, repo := newReviewFixture(t, "exports")
	ctx := context.Background()
	r := &reviewers{masons: newMasonController(f.s, repo)}
	fixturekit.Must(t, r.Pass(ctx))
	th, err := repo.Thread(stream, intake.ReviewerAgent("resume"))
	fixturekit.Must(t, err)
	turn := th.Turns[0].Request.TurnID
	earlier := writeExport(t, f, stream, th.Identity.ThreadID, turn+"-earlier")
	other := writeExport(t, f, stream, intake.ReviewerAgent("dedupe"), intake.ReviewerAgent("dedupe")+"-review-1")
	scope := coreadapter.Scope{Project: string(repo.Project()), Workstream: string(stream), Unit: "resume", Thread: th.Identity.ThreadID, Turn: turn, Role: intake.ReviewerRole}

	selection, err := execution.UnitReviewerSelection(ctx, f.s.about(repo), repo, scope, coreadapter.ExecutionSettings{})
	fixturekit.Must(t, err)
	fixturekit.AssertGone(t, earlier)
	assertThere(t, other)
	assertThere(t, filepath.Join(selection.Workspace.SourceDirectory, fixturekit.MasonWrote))
}

func TestReapSandboxesRemovesWhatInterruptedSessionsLeft(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := config.ResolveRoot(dir, dir)
	fixturekit.Must(t, err)
	cfg := &config.Config{Root: root, Project: config.Project{ID: "p_0123456789abcdef0123456789abcdef"}}
	project := string(cfg.Project.ID)
	record := func(name string, parts ...string) string {
		session := filepath.Join(append([]string{dir}, parts...)...)
		fixturekit.Must(t, os.MkdirAll(session, 0700))
		fixturekit.Must(t, procs.WriteSandboxName(session, name))
		return session
	}
	mason := record("agent-mason-1", "threads", project, "w_1", "mason-upload", "turn-1")
	architect := record("agent-architect-1", "architect", project, "w_1", "draft-1", "session")
	stuck := record("agent-stuck-1", "final", project, "w_1", "final-1", "session")
	other := record("agent-other-1", "threads", "p_fedcba9876543210fedcba9876543210", "w_1", "mason-upload", "turn-1")
	sbxDir := t.TempDir()
	sbxBin := filepath.Join(sbxDir, "sbx")
	removedLog := filepath.Join(sbxDir, "removed.txt")
	script := "#!/bin/sh\n" +
		`if [ "$1" = rm ] && [ "$3" = agent-stuck-1 ]; then echo "sbx is unavailable" >&2; exit 1; fi` + "\n" +
		`echo "$3" >> "` + removedLog + `"` + "\n"
	fixturekit.Must(t, os.WriteFile(sbxBin, []byte(script), 0700))
	service.TestReapSandboxes(context.Background(), cfg, sbxBin)
	removedData, err := os.ReadFile(removedLog)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	removed := strings.Fields(string(removedData))
	slices.Sort(removed)
	if want := []string{"agent-architect-1", "agent-mason-1"}; !slices.Equal(removed, want) {
		t.Fatalf("removed %v, want %v", removed, want)
	}
	for _, session := range []string{mason, architect} {
		if name := procs.SandboxName(session); name != "" {
			t.Fatalf("%s still records sandbox %s", session, name)
		}
	}
	if procs.SandboxName(stuck) != "agent-stuck-1" || procs.SandboxName(other) != "agent-other-1" {
		t.Fatal("a record whose sandbox was not removed is gone")
	}
}
