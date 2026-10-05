package service

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

	"github.com/kpenfound/busybees/core/agent/procs"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// newCleanupFixture builds disjointPlan on backend with the service stopped
// and starts upload and audit: each unit's workspace holds a file of its own
// that no commit holds, and its mason's first turn is queued.
func newCleanupFixture(t *testing.T, backend string) (*shedFixture, config.WorkstreamID, *trace.Repository) {
	t.Helper()
	ctx := context.Background()
	f, _ := newParallelMasonFixtureOn(t, backend, 2, 3, disjointPlan)
	base := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	stream, repository := seedBuild(t, f, "cleanup-"+backend, disjointPlan, backend, base)
	m := newMasonController(f.s, repository)
	for _, unit := range []string{"upload", "audit"} {
		b, found, err := m.read(stream)
		must(t, err)
		if !found {
			t.Fatal("the workstream is not building")
		}
		if started, _, err := m.start(ctx, b, unit); err != nil || !started {
			t.Fatalf("%s did not start: %v", unit, err)
		}
		w, _, err := newUnitWorkspaces(f.s.cfg, repository).open(ctx, stream, unit)
		must(t, err)
		must(t, os.WriteFile(filepath.Join(w.Path, unit+".txt"), []byte(unit+"\n"), 0600))
	}
	return f, stream, repository
}

func newCleanup(f *shedFixture, repository *trace.Repository) *workspaceCleanup {
	return &workspaceCleanup{cfg: f.s.cfg, repository: repository, now: f.s.now}
}

// writeExport leaves an export of the reviewer thread's turn, as a unit
// review does.
func writeExport(t *testing.T, f *shedFixture, stream config.WorkstreamID, thread, turn string) string {
	t.Helper()
	dir := filepath.Join(f.s.cfg.Root.String(), reviewInputsDirectory, string(f.project), string(stream), thread, turn)
	must(t, os.MkdirAll(dir, 0700))
	must(t, os.WriteFile(filepath.Join(dir, "candidate.txt"), []byte("candidate\n"), 0600))
	return dir
}

func assertGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s is still there: %v", path, err)
	}
}

func assertThere(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("%s is gone: %v", path, err)
	}
}

// assertUnitWorkspace reports whether the unit has a workspace, and fails
// the test when that is not want.
func assertUnitWorkspace(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, unit string, want bool) {
	t.Helper()
	_, _, found, err := newUnitWorkspaces(f.s.cfg, repository).find(context.Background(), stream, unit)
	must(t, err)
	dir := filepath.Join(f.s.cfg.Root.String(), unitsDirectory, string(f.project), string(stream), unit)
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
	assertGone(t, dir)
}

// branchFile returns the file at name in the tip of the branch.
func branchFile(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, branch, name string) string {
	t.Helper()
	ctx := context.Background()
	g, err := newUnitWorkspaces(f.s.cfg, repository).of(stream)
	must(t, err)
	tip, exists, err := g.Branch(ctx, branch)
	must(t, err)
	if !exists {
		t.Fatalf("branch %s is gone", branch)
	}
	dir := t.TempDir()
	must(t, g.Export(ctx, tip, dir))
	data, err := os.ReadFile(filepath.Join(dir, name))
	must(t, err)
	return string(data)
}

func recordUnitMerged(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string) {
	t.Helper()
	state, err := repository.Workflow(stream, trace.UnitSubject(unit))
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: trace.UnitSubject(unit) + "-" + UnitMerged, Revision: 1, Project: repository.Project(), Workstream: stream, Unit: unit, At: time.Now().UTC(), Actor: foremanActor, Cause: "landing-" + unit}
	_, err = repository.Transact(context.Background(), trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: h, Subject: trace.UnitSubject(unit), From: state.Value, To: UnitMerged, Reason: fmt.Sprintf("unit %s landed", unit)}})
	must(t, err)
}

// A merged unit's workspace and its reviewer's export are removed, and its
// branch stays. A unit in flight keeps its workspace, and so does the
// feature branch of a workstream still building.
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
			merged := writeExport(t, f, stream, reviewerAgent("upload"), reviewerAgent("upload")+"-review-1")
			inFlight := writeExport(t, f, stream, reviewerAgent("audit"), reviewerAgent("audit")+"-review-1")
			recordUnitMerged(t, repository, stream, "upload")

			c := newCleanup(f, repository)
			must(t, c.Pass(ctx))
			assertUnitWorkspace(t, f, repository, stream, "upload", false)
			assertGone(t, merged)
			assertUnitWorkspace(t, f, repository, stream, "audit", true)
			assertThere(t, inFlight)
			assertThere(t, filepath.Join(f.s.cfg.Root.String(), branchesDirectory, string(f.project), string(stream)))
			g, err := newUnitWorkspaces(f.s.cfg, repository).of(stream)
			must(t, err)
			if _, exists, err := g.Branch(ctx, unitBranch(stream, "upload")); err != nil || !exists {
				t.Fatalf("branch of the merged unit is gone: %v", err)
			}
			if len(c.failures) != 0 {
				t.Fatalf("cleanup failed: %v", c.failures)
			}
			must(t, c.Pass(ctx))
			assertUnitWorkspace(t, f, repository, stream, "audit", true)
		})
	}
}

// Abandoning a workstream removes its workspaces once their turns are done:
// each unit's files are committed to its branch first, and the feature
// branch and the unit branches stay. A unit with a turn unfinished keeps its
// workspace and its reviewer's export, and the workstream its feature branch
// workspace.
func TestCleanupKeepsTheWorkOfAnAbandonedWorkstream(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{config.WorkspacesGit, config.WorkspacesJujutsu} {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			f, stream, repository := newCleanupFixture(t, backend)
			defer repository.Close()
			ctx := context.Background()
			completeMasonTurn(t, f, repository, stream, "upload", "built upload")
			done := writeExport(t, f, stream, reviewerAgent("upload"), reviewerAgent("upload")+"-review-1")
			export := writeExport(t, f, stream, reviewerAgent("audit"), reviewerAgent("audit")+"-review-1")
			h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: abandonTransition, Revision: 1, Project: repository.Project(), Workstream: stream, At: f.s.now(), Actor: ownerActor, Cause: abandonTransition}
			_, err := repository.SetFeatureStateUnless(ctx, h, AbandonedState, "superseded", AbandonedState, DeliveredState)
			must(t, err)
			feature := filepath.Join(f.s.cfg.Root.String(), branchesDirectory, string(f.project), string(stream))

			c := newCleanup(f, repository)
			must(t, c.Pass(ctx))
			assertUnitWorkspace(t, f, repository, stream, "upload", false)
			assertGone(t, done)
			if got := branchFile(t, f, repository, stream, unitBranch(stream, "upload"), "upload.txt"); got != "upload\n" {
				t.Fatalf("upload's branch holds %q", got)
			}
			assertUnitWorkspace(t, f, repository, stream, "audit", true)
			assertThere(t, feature)
			assertThere(t, export)

			_, err = repository.CancelTurns(ctx, stream, f.s.now(), abandonActor, cancelReason)
			must(t, err)
			must(t, c.Pass(ctx))
			if len(c.failures) != 0 {
				t.Fatalf("cleanup failed: %v", c.failures)
			}
			assertUnitWorkspace(t, f, repository, stream, "audit", false)
			if got := branchFile(t, f, repository, stream, unitBranch(stream, "audit"), "audit.txt"); got != "audit\n" {
				t.Fatalf("audit's branch holds %q", got)
			}
			assertGone(t, feature)
			assertGone(t, filepath.Join(f.s.cfg.Root.String(), reviewInputsDirectory, string(f.project), string(stream)))
			assertGone(t, filepath.Join(f.s.cfg.Root.String(), unitsDirectory, string(f.project), string(stream)))
			g, err := featureWorkspaces(f.s.cfg, repository).of(stream)
			must(t, err)
			if _, exists, err := g.Branch(ctx, featureBranch(stream)); err != nil || !exists {
				t.Fatalf("the feature branch is gone: %v", err)
			}
			must(t, c.Pass(ctx))
		})
	}
}

// Once a workstream is abandoned and its turns are done, the directories its
// turns ran with go: their session directories, transcripts included, and
// the staged views. They stay while the workstream builds, and while any turn
// of it is unfinished, the chief of staff's included. The trace stays.
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
		filepath.Join("threads", project, string(stream), masonAgent("upload"), "turn-1", "transcript.jsonl"),
		filepath.Join("architect", project, string(stream), "draft-1", "session", "transcript.jsonl"),
		filepath.Join("shed", project, string(stream), "round-1-member-1", "session", "prompt.md"),
		filepath.Join("final", project, string(stream), "final-1", "session", "report.json"),
		filepath.Join("chief_of_staff", project, string(stream), "status.json"),
		filepath.Join("workspaces", project, string(stream), "notes.md"),
	} {
		path := filepath.Join(root, rel)
		must(t, os.MkdirAll(filepath.Dir(path), 0700))
		must(t, os.WriteFile(path, []byte("recorded\n"), 0600))
		turns = append(turns, filepath.Join(root, strings.Join(strings.Split(rel, string(filepath.Separator))[:3], string(filepath.Separator))))
	}

	c := newCleanup(f, repository)
	must(t, c.Pass(ctx))
	for _, dir := range turns {
		assertThere(t, dir)
	}

	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: abandonTransition, Revision: 1, Project: repository.Project(), Workstream: stream, At: f.s.now(), Actor: ownerActor, Cause: abandonTransition}
	_, err := repository.SetFeatureStateUnless(ctx, h, AbandonedState, "superseded", AbandonedState, DeliveredState)
	must(t, err)
	_, err = repository.EnsureChiefOfStaff(ctx, stream, f.s.now(), serviceActor)
	must(t, err)
	request := trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_owner-message", Revision: 1, Project: repository.Project(), Workstream: stream, At: f.s.now(), Actor: ownerActor, Cause: "fixture"}
	_, err = repository.EnqueueTurn(ctx, trace.TurnRequest{Header: request, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "owner-message",
		Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "You are the chief of staff.", Prompt: "Anything left?"})
	must(t, err)
	must(t, c.Pass(ctx))
	assertUnitWorkspace(t, f, repository, stream, "upload", false)
	assertGone(t, filepath.Join(root, branchesDirectory, project, string(stream)))
	for _, dir := range turns {
		assertThere(t, dir)
	}

	_, err = repository.CancelTurns(ctx, stream, f.s.now(), abandonActor, cancelReason)
	must(t, err)
	must(t, c.Pass(ctx))
	if len(c.failures) != 0 {
		t.Fatalf("cleanup failed: %v", c.failures)
	}
	for _, dir := range turns {
		assertGone(t, dir)
	}
	if th, err := repository.Thread(stream, masonAgent("upload")); err != nil || len(th.Turns) == 0 {
		t.Fatalf("the trace lost the mason's turns: %+v %v", th, err)
	}
}

// A unit review's export replaces the exports of its thread's earlier turns
// and leaves other threads' alone.
func TestUnitReviewKeepsOneExportPerThread(t *testing.T) {
	t.Parallel()
	f, stream, repo := newReviewFixture(t, "exports")
	ctx := context.Background()
	r := &reviewers{masons: newMasonController(f.s, repo)}
	must(t, r.Pass(ctx))
	th, err := repo.Thread(stream, reviewerAgent("resume"))
	must(t, err)
	turn := th.Turns[0].Request.TurnID
	earlier := writeExport(t, f, stream, th.Identity.ThreadID, turn+"-earlier")
	other := writeExport(t, f, stream, reviewerAgent("dedupe"), reviewerAgent("dedupe")+"-review-1")
	scope := coreadapter.Scope{Project: string(repo.Project()), Workstream: string(stream), Unit: "resume", Thread: th.Identity.ThreadID, Turn: turn, Role: reviewerRole}

	selection, err := unitReviewerSelection(ctx, f.s.about(repo), repo, scope, coreadapter.ExecutionSettings{})
	must(t, err)
	assertGone(t, earlier)
	assertThere(t, other)
	assertThere(t, filepath.Join(selection.Workspace.SourceDirectory, masonWrote))
}

// The sandboxes interrupted sessions of the project recorded are removed
// with their records. A sandbox that cannot be removed keeps its record, and
// another project's records are left alone.
func TestReapSandboxesRemovesWhatInterruptedSessionsLeft(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := config.ResolveRoot(dir, dir)
	must(t, err)
	cfg := &config.Config{Root: root, Project: config.Project{ID: "p_0123456789abcdef0123456789abcdef"}}
	project := string(cfg.Project.ID)
	record := func(name string, parts ...string) string {
		session := filepath.Join(append([]string{dir}, parts...)...)
		must(t, os.MkdirAll(session, 0700))
		must(t, procs.WriteSandboxName(session, name))
		return session
	}
	mason := record("agent-mason-1", "threads", project, "w_1", "mason-upload", "turn-1")
	architect := record("agent-architect-1", "architect", project, "w_1", "draft-1", "session")
	librarian := record("agent-librarian-1", "librarian", project, "extract-1", "session")
	stuck := record("agent-stuck-1", "final", project, "w_1", "final-1", "session")
	other := record("agent-other-1", "threads", "p_fedcba9876543210fedcba9876543210", "w_1", "mason-upload", "turn-1")
	var removed []string
	reapSandboxes(context.Background(), cfg, func(_ context.Context, name string) error {
		if name == "agent-stuck-1" {
			return errors.New("sbx is unavailable")
		}
		removed = append(removed, name)
		return nil
	})
	slices.Sort(removed)
	if want := []string{"agent-architect-1", "agent-librarian-1", "agent-mason-1"}; !slices.Equal(removed, want) {
		t.Fatalf("removed %v, want %v", removed, want)
	}
	for _, session := range []string{mason, architect, librarian} {
		if name := procs.SandboxName(session); name != "" {
			t.Fatalf("%s still records sandbox %s", session, name)
		}
	}
	if procs.SandboxName(stuck) != "agent-stuck-1" || procs.SandboxName(other) != "agent-other-1" {
		t.Fatal("a record whose sandbox was not removed is gone")
	}
}
