package lifecycle_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

func attemptOperation(t *testing.T, s *Service, repository *trace.Repository, stream config.WorkstreamID, op coreadapter.Operation, r coreadapter.Reconciler) (coreadapter.OperationResult, error) {
	t.Helper()
	return intakefixture.AttemptOperation(t, s.now(), service.TestForemanActor, repository, stream, op, r)
}

func awaitResolution(t *testing.T, s *Service, d drifter, stream config.WorkstreamID, op coreadapter.Operation, want string) {
	t.Helper()
	if result, err := attemptOperation(t, s, d.repository, stream, op, d); !errors.Is(err, lifecycle.ErrDriftAwaits) || !strings.Contains(err.Error(), want) {
		t.Fatalf("the drift rebase did not await %q: %+v %v", want, result, err)
	}
}

func completeDriftTurn(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, agent string, outcome *coreadapter.Outcome) trace.QueuedTurn {
	t.Helper()
	return intakefixture.CompleteDriftTurn(t, f.sharedTurns(), repository, stream, agent, outcome)
}

func completeClaimedTurn(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, agent string, q trace.QueuedTurn, outcome *coreadapter.Outcome) {
	t.Helper()
	f.sharedTurns().CompleteClaimedTurn(t, repository, stream, agent, q, outcome)
}

func resolvedDone(outcome string) *coreadapter.Outcome {
	data, _ := json.Marshal(service.MasonReport{Outcome: outcome})
	return &coreadapter.Outcome{Status: execution.MasonDone, Report: string(data)}
}

func driftVerdict(t *testing.T, verdict service.UnitVerdict) *coreadapter.Outcome {
	t.Helper()
	return intakefixture.VerdictOutcome(t, verdict, execution.VerdictOutcome)
}

var (
	approvedResolution = service.UnitVerdict{Decision: "satisfactory", Summary: reviewSummary}
	rejectedResolution = service.UnitVerdict{Decision: "material_findings", Summary: reviewSummary,
		Findings: []api.ReviewFinding{{Severity: "major", Evidence: "The resolution drops @upstream.", Action: "Keep @upstream beside @feature."}}}
)

func resolutionWorkspace(t *testing.T, f *shedFixture, stream config.WorkstreamID) workspace.Worktree {
	t.Helper()
	return intakefixture.Workspace(t, intake.Workspaces(f.s.cfg, service.TestDriftsDirectory, backendOf(t, f, stream)), stream)
}

func featureTip(t *testing.T, f *shedFixture, stream config.WorkstreamID) string {
	t.Helper()
	tip, _, err := intake.Workspaces(f.s.cfg, intake.BranchesDirectory, backendOf(t, f, stream)).Branch(context.Background(), intake.FeatureBranch(stream))
	intakefixture.Must(t, err)
	return tip
}

func conflictedDrift(t *testing.T, f *shedFixture, d drifter, stream config.WorkstreamID) (string, string, coreadapter.Operation) {
	t.Helper()
	before := moveFeature(t, f, stream, map[string]string{"CODEOWNERS": "/internal/ @feature\n"})
	upstream := advanceUpstream(t, f, map[string]string{"CODEOWNERS": "/internal/ @upstream\n"})
	return before, upstream, requestDrift(t, d, stream)
}

func TestDriftConflictIsResolvedByAMasonAndMovesTheBranchOnApproval(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := newFinalFixture(t, "drift-resolve")
	d := drifter{&foreman{masons: newMasonController(f.s, repository)}}
	ctx := context.Background()
	before, upstream, op := conflictedDrift(t, f, d, stream)
	sealed := intakefixture.StreamDocuments(t, repository, stream, seal.DocumentID)

	awaitResolution(t, f.s, d, stream, op, "its mason's resolution of CODEOWNERS")
	if tip := featureTip(t, f, stream); tip != before {
		t.Fatalf("a conflicted drift rebase moved the feature branch to %s", tip)
	}
	if after := intakefixture.StreamDocuments(t, repository, stream, seal.DocumentID); !reflect.DeepEqual(after, sealed) {
		t.Fatalf("a conflicted drift rebase changed the seal: %+v", after)
	}
	if ops := driftOperations(t, repository, stream); len(ops) != 1 || ops[0].Result != nil {
		t.Fatalf("the conflicted drift operation %+v", ops)
	}
	if conflicted := intakefixture.TransitionByID(t, repository, stream, "drift-1-conflicted"); conflicted.To != "conflicted-1" || !strings.Contains(conflicted.Reason, "CODEOWNERS conflicted") {
		t.Fatalf("the conflict %+v", conflicted)
	}
	base := lifecycle.DriftRebase{Drift: 1, Operation: op.ID, Branch: intake.FeatureBranch(stream), Upstream: seal.Base{Remote: "upstream", Branch: "main", Commit: upstream}, From: intakefixture.Seals(t, repository, stream)[0].Base.Commit, Before: before, Conflicts: []string{"CODEOWNERS"}}
	stopped := base
	stopped.Outcome, stopped.Round, stopped.Stop = lifecycle.DriftConflicted, 1, before
	opened := base
	opened.Outcome = lifecycle.DriftConflicted
	if records := driftRecords(t, repository, stream); !reflect.DeepEqual(records, []lifecycle.DriftRebase{opened, stopped}) {
		t.Fatalf("drift/rebase.json %+v", records)
	}
	w := resolutionWorkspace(t, f, stream)
	if w.Branch != lifecycle.DriftBranch(stream, 1) {
		t.Fatalf("the resolution workspace is on %s", w.Branch)
	}
	data, err := os.ReadFile(filepath.Join(w.Path, "CODEOWNERS"))
	intakefixture.Must(t, err)
	if !strings.Contains(string(data), "<<<<<<< ") || !strings.Contains(string(data), "@upstream") || !strings.Contains(string(data), "@feature") {
		t.Fatalf("the resolution workspace's CODEOWNERS holds %q", data)
	}
	if ids := intakefixture.TurnIDs(t, repository, stream, service.TestDriftMasonAgent); !slices.Equal(ids, []string{lifecycle.DriftResolveTurnID(1, 1)}) {
		t.Fatalf("drift mason turns %v", ids)
	}
	th, err := repository.Thread(stream, service.TestDriftMasonAgent)
	intakefixture.Must(t, err)
	if th.Identity.Role != intakefixture.MasonRole {
		t.Fatalf("the drift mason's role is %s", th.Identity.Role)
	}
	for _, want := range []string{"- CODEOWNERS\n", "conflict markers", "## Sealed spec: spec.md", "## end of spec.md", before} {
		if !strings.Contains(th.Turns[0].Request.Prompt, want) {
			t.Fatalf("the resolve turn lacks %q:\n%s", want, th.Turns[0].Request.Prompt)
		}
	}

	awaitResolution(t, f.s, d, stream, op, "its mason's resolution of CODEOWNERS")
	if ids := intakefixture.TurnIDs(t, repository, stream, service.TestDriftMasonAgent); len(ids) != 1 {
		t.Fatalf("drift mason turns %v", ids)
	}

	intakefixture.Must(t, os.WriteFile(filepath.Join(w.Path, "CODEOWNERS"), []byte("/internal/ @upstream @feature\n"), 0600))
	completeDriftTurn(t, f, repository, stream, service.TestDriftMasonAgent, resolvedDone("Kept both owners"))
	awaitResolution(t, f.s, d, stream, op, "review 1 of candidate")
	records := driftRecords(t, repository, stream)
	candidate := records[len(records)-1].Candidate
	resolved := stopped
	resolved.Outcome, resolved.Candidate, resolved.Review = intake.DriftResolved, candidate, 1
	if !reflect.DeepEqual(records[len(records)-1], resolved) || parentOf(t, f, candidate) != upstream {
		t.Fatalf("the resolved record %+v, want %+v on %s", records[len(records)-1], resolved, upstream)
	}
	if tip := featureTip(t, f, stream); tip != before {
		t.Fatalf("the feature branch moved to %s before review", tip)
	}
	review, err := repository.Thread(stream, service.TestDriftReviewerAgent)
	intakefixture.Must(t, err)
	if review.Identity.Role != intakefixture.ReviewerRole || len(review.Turns) != 1 || review.Turns[0].Request.TurnID != lifecycle.DriftReviewTurnID(1, 1) {
		t.Fatalf("the drift reviewer's thread %+v", review)
	}
	for _, want := range []string{candidate, "- CODEOWNERS", `Change "resolution"`, "## Sealed spec: spec.md"} {
		if !strings.Contains(review.Turns[0].Request.Prompt, want) {
			t.Fatalf("the review turn lacks %q:\n%s", want, review.Turns[0].Request.Prompt)
		}
	}
	if resolution := driftResolutionDiff(t, f, repository, stream); !strings.Contains(resolution, "+/internal/ @upstream @feature") || strings.Contains(review.Turns[0].Request.Prompt, "+/internal/ @upstream @feature") {
		t.Fatalf("the reviewer reads the resolution %q through its prompt or not at all", resolution)
	}

	completeDriftTurn(t, f, repository, stream, service.TestDriftReviewerAgent, driftVerdict(t, approvedResolution))
	result, err := attemptOperation(t, f.s, repository, stream, op, d)
	if err != nil || result.Outcome != "succeeded" {
		t.Fatalf("the approved drift rebase %+v %v", result, err)
	}
	if tip := featureTip(t, f, stream); tip != candidate || fileAt(t, f, tip, "CODEOWNERS") != "/internal/ @upstream @feature\n" {
		t.Fatalf("the feature branch is at %s, not the approved candidate %s", tip, candidate)
	}
	after := intakefixture.Seals(t, repository, stream)
	if len(after) != 2 || after[1].Base.Commit != upstream {
		t.Fatalf("seals after the approved drift rebase %+v", after)
	}
	records = driftRecords(t, repository, stream)
	replayed := resolved
	replayed.Outcome, replayed.Commit, replayed.Verdict = lifecycle.DriftReplayed, candidate, &approvedResolution
	rebased := replayed
	rebased.Outcome, rebased.Seal, rebased.SealRevision = lifecycle.DriftRebased, 1, 2
	if !reflect.DeepEqual(records[len(records)-2:], []lifecycle.DriftRebase{replayed, rebased}) {
		t.Fatalf("drift/rebase.json ends %+v", records[len(records)-2:])
	}
	if done := intakefixture.TransitionByID(t, repository, stream, "drift-1-rebased"); done.From != "conflicted-1" || done.To != "rebased-1" {
		t.Fatalf("the outcome %+v", done)
	}
	if _, found, err := intake.Workspaces(f.s.cfg, service.TestDriftsDirectory, config.WorkspacesGit).Workspace(ctx, string(stream)); err != nil || found {
		t.Fatalf("the resolution workspace outlived the drift rebase: %t %v", found, err)
	}
	if seen, err := d.Inspect(ctx, op); err != nil || seen.State != coreadapter.EffectCompleted {
		t.Fatalf("inspection after the drift rebase %+v %v", seen, err)
	}
}

func TestRejectedDriftResolutionReturnsToTheMasonAndKeepsTheBranch(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := newFinalFixture(t, "drift-reject")
	d := drifter{&foreman{masons: newMasonController(f.s, repository)}}
	before, upstream, op := conflictedDrift(t, f, d, stream)
	sealed := intakefixture.StreamDocuments(t, repository, stream, seal.DocumentID)
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution")
	w := resolutionWorkspace(t, f, stream)

	done := completeDriftTurn(t, f, repository, stream, service.TestDriftMasonAgent, resolvedDone("Resolved"))
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution")
	remind := fmt.Sprintf("%s-markers-%d", service.TestDriftMasonAgent, done.Sequence)
	if ids := intakefixture.TurnIDs(t, repository, stream, service.TestDriftMasonAgent); !slices.Equal(ids, []string{lifecycle.DriftResolveTurnID(1, 1), remind}) {
		t.Fatalf("drift mason turns after a done with markers %v", ids)
	}
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution")
	if ids := intakefixture.TurnIDs(t, repository, stream, service.TestDriftMasonAgent); len(ids) != 2 {
		t.Fatalf("the reminder was queued again: %v", ids)
	}

	intakefixture.Must(t, os.WriteFile(filepath.Join(w.Path, "CODEOWNERS"), []byte("/internal/ @feature\n"), 0600))
	completeDriftTurn(t, f, repository, stream, service.TestDriftMasonAgent, resolvedDone("Kept the feature owner"))
	awaitResolution(t, f.s, d, stream, op, "review 1")
	first := driftRecords(t, repository, stream)
	candidate := first[len(first)-1].Candidate
	completeDriftTurn(t, f, repository, stream, service.TestDriftReviewerAgent, driftVerdict(t, rejectedResolution))
	awaitResolution(t, f.s, d, stream, op, "its mason's answer to review 1")
	records := driftRecords(t, repository, stream)
	rejected := records[len(records)-1]
	if rejected.Outcome != intake.DriftRejected || rejected.Review != 1 || rejected.Candidate != candidate || !reflect.DeepEqual(rejected.Verdict, &rejectedResolution) {
		t.Fatalf("the rejected record %+v", rejected)
	}
	if tip := featureTip(t, f, stream); tip != before {
		t.Fatalf("a rejected resolution moved the feature branch to %s", tip)
	}
	if after := intakefixture.StreamDocuments(t, repository, stream, seal.DocumentID); !reflect.DeepEqual(after, sealed) {
		t.Fatalf("a rejected resolution changed the seal: %+v", after)
	}
	th, err := repository.Thread(stream, service.TestDriftMasonAgent)
	intakefixture.Must(t, err)
	fix := th.Turns[len(th.Turns)-1]
	if fix.Request.TurnID != lifecycle.DriftFixTurnID(1, 1) || !strings.Contains(fix.Request.Prompt, "Keep @upstream beside @feature.") || !strings.Contains(fix.Request.Prompt, "## Sealed spec: spec.md") {
		t.Fatalf("the mason's turn after the rejection %s:\n%s", fix.Request.TurnID, fix.Request.Prompt)
	}

	intakefixture.Must(t, os.WriteFile(filepath.Join(w.Path, "CODEOWNERS"), []byte("/internal/ @upstream @feature\n"), 0600))
	completeDriftTurn(t, f, repository, stream, service.TestDriftMasonAgent, resolvedDone("Kept both owners"))
	awaitResolution(t, f.s, d, stream, op, "review 2")
	records = driftRecords(t, repository, stream)
	second := records[len(records)-1]
	if second.Outcome != intake.DriftResolved || second.Review != 2 || second.Candidate == candidate || second.Verdict != nil || fileAt(t, f, second.Candidate, "CODEOWNERS") != "/internal/ @upstream @feature\n" {
		t.Fatalf("the second candidate %+v", second)
	}
	if ids := intakefixture.TurnIDs(t, repository, stream, service.TestDriftReviewerAgent); !slices.Equal(ids, []string{lifecycle.DriftReviewTurnID(1, 1), lifecycle.DriftReviewTurnID(1, 2)}) {
		t.Fatalf("drift reviewer turns %v", ids)
	}
	if tip := featureTip(t, f, stream); tip != before {
		t.Fatalf("the feature branch moved to %s before the second review", tip)
	}

	completeDriftTurn(t, f, repository, stream, service.TestDriftReviewerAgent, driftVerdict(t, approvedResolution))
	if result, err := attemptOperation(t, f.s, repository, stream, op, d); err != nil || result.Outcome != "succeeded" {
		t.Fatalf("the approved drift rebase %+v %v", result, err)
	}
	if tip := featureTip(t, f, stream); tip != second.Candidate {
		t.Fatalf("the feature branch is at %s, not the approved candidate %s", tip, second.Candidate)
	}
	if after := intakefixture.Seals(t, repository, stream); len(after) != 2 || after[1].Base.Commit != upstream {
		t.Fatalf("seals after approval %+v", after)
	}
}

func TestRestartDuringDriftResolutionResumesWithoutDuplicates(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := newFinalFixture(t, "drift-restart")
	d := drifter{&foreman{masons: newMasonController(f.s, repository)}}
	ctx := context.Background()
	before, _, op := conflictedDrift(t, f, d, stream)
	stops := 0
	f.s.boundary = func(name string) error {
		if name == "drift-resolving" {
			stops++
			if stops == 1 {
				return errors.New("the service stopped")
			}
		}
		return nil
	}
	if _, err := attemptOperation(t, f.s, repository, stream, op, d); err == nil || !strings.Contains(err.Error(), "the service stopped") {
		t.Fatalf("the drift rebase stopped before its resolution replay: %v", err)
	}
	f.s.clearBoundary()
	restart := func() {
		t.Helper()
		intakefixture.Must(t, repository.Close())
		var err error
		repository, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
		intakefixture.Must(t, err)
		d = drifter{&foreman{masons: newMasonController(f.s, repository)}}
	}
	restart()
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution")
	w := resolutionWorkspace(t, f, stream)
	marked, err := os.ReadFile(filepath.Join(w.Path, "CODEOWNERS"))
	intakefixture.Must(t, err)
	records := len(driftRecords(t, repository, stream))

	restart()
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution")
	if ids := intakefixture.TurnIDs(t, repository, stream, service.TestDriftMasonAgent); !slices.Equal(ids, []string{lifecycle.DriftResolveTurnID(1, 1)}) {
		t.Fatalf("drift mason turns after a restart %v", ids)
	}
	if n := len(driftRecords(t, repository, stream)); n != records {
		t.Fatalf("a restart recorded %d drift records, not %d", n, records)
	}
	if again, err := os.ReadFile(filepath.Join(w.Path, "CODEOWNERS")); err != nil || string(again) != string(marked) {
		t.Fatalf("a restart replayed the resolution workspace again: %q %v", again, err)
	}

	turn := lifecycle.DriftResolveTurnID(1, 1)
	paths, err := execution.ViewPaths(w.Path)
	intakefixture.Must(t, err)
	viewRoot := filepath.Join(f.s.cfg.Root.String(), "views", string(f.project), string(stream), service.TestDriftMasonAgent, turn)
	intakefixture.Must(t, os.MkdirAll(viewRoot, 0700))
	view, err := (isolation.Views{Directory: viewRoot}).Create(ctx, coreadapter.Workspace{Directory: w.Path, Access: coreadapter.ReadWrite}, paths)
	intakefixture.Must(t, err)
	intakefixture.Must(t, os.WriteFile(filepath.Join(viewRoot, "ready"), []byte(filepath.Base(view.Workspace().Directory)), 0600))
	intakefixture.Must(t, os.WriteFile(filepath.Join(view.Workspace().Directory, "CODEOWNERS"), []byte("/internal/ @upstream @feature\n"), 0600))
	_, err = repository.ClaimTurn(ctx, stream, service.TestDriftMasonAgent, "crashed", filepath.Join(f.s.cfg.Root.String(), "threads", string(f.project), string(stream), service.TestDriftMasonAgent, turn), f.clock.Now())
	intakefixture.Must(t, err)
	restart()
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution")
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution")
	recover := fmt.Sprintf("%s-recover-1", service.TestDriftMasonAgent)
	if ids := intakefixture.TurnIDs(t, repository, stream, service.TestDriftMasonAgent); !slices.Equal(ids, []string{turn, recover}) {
		t.Fatalf("drift mason turns after an interrupted turn %v", ids)
	}
	if data, err := os.ReadFile(filepath.Join(w.Path, "CODEOWNERS")); err != nil || string(data) != "/internal/ @upstream @feature\n" {
		t.Fatalf("the interrupted turn's view was not copied back: %q %v", data, err)
	}

	completeDriftTurn(t, f, repository, stream, service.TestDriftMasonAgent, resolvedDone("Kept both owners"))
	for _, point := range []string{"drift-staging"} {
		f.s.boundary = func(name string) error {
			if name == point {
				return errors.New("the service stopped")
			}
			return nil
		}
		if _, err := attemptOperation(t, f.s, repository, stream, op, d); err == nil || !strings.Contains(err.Error(), "the service stopped") {
			t.Fatalf("the drift rebase did not stop at %s: %v", point, err)
		}
		f.s.clearBoundary()
		restart()
	}
	awaitResolution(t, f.s, d, stream, op, "review 1")
	completeDriftTurn(t, f, repository, stream, service.TestDriftReviewerAgent, driftVerdict(t, approvedResolution))
	f.s.boundary = func(name string) error {
		if name == "drift-approved" {
			return errors.New("the service stopped")
		}
		return nil
	}
	if _, err := attemptOperation(t, f.s, repository, stream, op, d); err == nil || !strings.Contains(err.Error(), "the service stopped") {
		t.Fatalf("the drift rebase did not stop after its approval: %v", err)
	}
	f.s.clearBoundary()
	if tip := featureTip(t, f, stream); tip != before {
		t.Fatalf("the feature branch moved to %s before the approval was applied", tip)
	}
	restart()
	if result, err := attemptOperation(t, f.s, repository, stream, op, d); err != nil || result.Outcome != "succeeded" {
		t.Fatalf("the approved drift rebase after a restart %+v %v", result, err)
	}
	records2 := driftRecords(t, repository, stream)
	candidate := records2[len(records2)-1].Commit
	if tip := featureTip(t, f, stream); tip != candidate || fileAt(t, f, tip, "CODEOWNERS") != "/internal/ @upstream @feature\n" {
		t.Fatalf("the feature branch is at %s, not %s", tip, candidate)
	}
	if ids := intakefixture.TurnIDs(t, repository, stream, service.TestDriftReviewerAgent); len(ids) != 1 {
		t.Fatalf("drift reviewer turns %v", ids)
	}
	if _, found, err := intake.Workspaces(f.s.cfg, service.TestDriftsDirectory, config.WorkspacesGit).Workspace(ctx, string(stream)); err != nil || found {
		t.Fatalf("the resolution workspace outlived the drift rebase: %t %v", found, err)
	}
	var outcomes []string
	for _, r := range records2 {
		outcomes = append(outcomes, r.Outcome)
	}
	if want := []string{lifecycle.DriftConflicted, lifecycle.DriftConflicted, intake.DriftResolved, lifecycle.DriftReplayed, lifecycle.DriftRebased}; !slices.Equal(outcomes, want) {
		t.Fatalf("drift records %q, want %q", outcomes, want)
	}
	repository.Close()
}

func TestDriftMasonTurnWorksInTheResolutionWorkspace(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := newFinalFixture(t, "drift-workspace")
	d := drifter{&foreman{masons: newMasonController(f.s, repository)}}
	ctx := context.Background()
	_, _, op := conflictedDrift(t, f, d, stream)
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution")
	w := resolutionWorkspace(t, f, stream)
	drifts := resolutions{lifecycle.DriftWorkspaces(f.s.cfg, repository)}
	scope := coreadapter.Scope{Project: string(f.project), Workstream: string(stream), Thread: service.TestDriftMasonAgent, Turn: lifecycle.DriftResolveTurnID(1, 1), Role: intakefixture.MasonRole}

	lease, err := threadWorkspaces{units: newUnitWorkspaces(f.s.cfg, repository), drifts: drifts}.Acquire(ctx, coreadapter.WorkspaceRequest{Scope: scope, Access: coreadapter.ReadWrite})
	intakefixture.Must(t, err)
	if lease.Workspace.Directory != w.Path {
		t.Fatalf("the drift mason turn is lent %s, not %s", lease.Workspace.Directory, w.Path)
	}
	if _, err := drifts.Acquire(ctx, coreadapter.WorkspaceRequest{Scope: coreadapter.Scope{Project: scope.Project, Workstream: scope.Workstream, Thread: intakefixture.MasonAgent("resume"), Unit: "resume", Role: intakefixture.MasonRole}}); err == nil {
		t.Fatal("a unit's mason turn was lent the resolution workspace")
	}
	selection, err := drifts.selection(ctx, scope, coreadapter.ExecutionSettings{})
	intakefixture.Must(t, err)
	if slices.Contains(selection.Paths, ".git") || !slices.Contains(selection.Paths, "CODEOWNERS") {
		t.Fatalf("the drift mason's view selects %v", selection.Paths)
	}
	if selection.Narrow != nil {
		t.Fatalf("the drift mason's selection narrows the role grant: %+v", selection.Narrow)
	}
	if !slices.Equal(service.TestMasonGrant.Tools, []string{"file_read", "file_write", questions.AskTool, questions.AmendTool, intake.FactoryContextTool, discoveryTool, execution.DoneTool}) || !service.TestMasonGrant.WriteFiles || !service.TestMasonGrant.Execute {
		t.Fatalf("masonGrant %+v", service.TestMasonGrant)
	}

	viewRoot := filepath.Join(t.TempDir(), "views")
	intakefixture.Must(t, os.MkdirAll(viewRoot, 0700))
	view, err := (isolation.Views{Directory: viewRoot}).Create(ctx, lease.Workspace, selection.Paths)
	intakefixture.Must(t, err)
	intakefixture.Must(t, os.WriteFile(filepath.Join(view.Workspace().Directory, "CODEOWNERS"), []byte("resolved\n"), 0600))
	intakefixture.Must(t, drifts.capture(ctx, scope, view))
	if data, err := os.ReadFile(filepath.Join(w.Path, "CODEOWNERS")); err != nil || string(data) != "resolved\n" {
		t.Fatalf("the view was not copied back: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(w.Path, ".git")); err != nil {
		t.Fatalf("the capture lost the workspace's VCS metadata: %v", err)
	}

	reports := &execution.MasonReports{}
	tool := reports.DriftTool(scope)
	for _, raw := range []string{`{"outcome":" "}`, `{"outcome":"Kept both owners"}`, `{"outcome":"again"}`} {
		out, err := tool.Handle(ctx, json.RawMessage(raw))
		intakefixture.Must(t, err)
		recorded := raw == `{"outcome":"Kept both owners"}`
		if strings.Contains(string(out), `"recorded":true`) != recorded {
			t.Fatalf("done %s returned %s", raw, out)
		}
	}
	if got := reports.AcceptedForTest(execution.TurnKey(scope)); got.Report.Outcome != "Kept both owners" || got.Card != nil {
		t.Fatalf("the accepted report %+v", got)
	}
}

func driftResolutionDiff(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID) string {
	t.Helper()
	return intakefixture.DriftResolutionDiff(t, func() (coreadapter.Tool, error) {
		return lifecycle.DriftDiffTool(context.Background(), f.s.cfg, repository, coreadapter.Scope{Workstream: string(stream), Thread: service.TestDriftReviewerAgent, Turn: lifecycle.DriftReviewTurnID(1, 1), Role: intakefixture.ReviewerRole})
	})
}

func TestDriftResolutionIsHeldForTheOwnerAtMaxBounces(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := newFinalFixture(t, "drift-held")
	f.s.cfg.Shed.MaxBounces = 2
	fm := cadenceForeman(f, repository, "1h")
	d := drifter{fm}
	ctx := context.Background()
	before, _, op := conflictedDrift(t, f, d, stream)
	sealed := intakefixture.StreamDocuments(t, repository, stream, seal.DocumentID)
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution")
	w := resolutionWorkspace(t, f, stream)
	for review := 1; review <= 2; review++ {
		intakefixture.Must(t, os.WriteFile(filepath.Join(w.Path, "CODEOWNERS"), []byte(fmt.Sprintf("/internal/ @feature%d\n", review)), 0600))
		completeDriftTurn(t, f, repository, stream, service.TestDriftMasonAgent, resolvedDone("Resolved"))
		awaitResolution(t, f.s, d, stream, op, fmt.Sprintf("review %d", review))
		completeDriftTurn(t, f, repository, stream, service.TestDriftReviewerAgent, driftVerdict(t, rejectedResolution))
		if review == 1 {
			awaitResolution(t, f.s, d, stream, op, "its mason's answer to review 1")
		}
	}
	result, err := attemptOperation(t, f.s, repository, stream, op, d)
	if err != nil || result.Outcome != "succeeded" || !strings.Contains(result.Evidence, "drift rebase 1 is held for the owner: review sent its resolution of the conflicts in CODEOWNERS back 2 times, reaching shed.max_bounces") {
		t.Fatalf("the drift rebase at max_bounces %+v %v", result, err)
	}
	if ids := intakefixture.TurnIDs(t, repository, stream, service.TestDriftMasonAgent); !slices.Equal(ids, []string{lifecycle.DriftResolveTurnID(1, 1), lifecycle.DriftFixTurnID(1, 1)}) {
		t.Fatalf("drift mason turns %v", ids)
	}
	if state, err := repository.Workflow(stream, lifecycle.DriftSubject); err != nil || state.Value != lifecycle.DriftHeld+"-1" {
		t.Fatalf("the drift subject is %+v: %v", state, err)
	}
	if tip := featureTip(t, f, stream); tip != before {
		t.Fatalf("a held drift rebase moved the feature branch to %s", tip)
	}
	if after := intakefixture.StreamDocuments(t, repository, stream, seal.DocumentID); !reflect.DeepEqual(after, sealed) {
		t.Fatalf("a held drift rebase changed the seal: %+v", after)
	}
	if _, found, err := intake.Workspaces(f.s.cfg, service.TestDriftsDirectory, backendOf(t, f, stream)).Workspace(ctx, string(stream)); err != nil || found {
		t.Fatalf("the held drift rebase kept its resolution workspace: %v %v", found, err)
	}
	if again, err := d.Apply(ctx, op); err != nil || again.Outcome != result.Outcome || again.Evidence != result.Evidence {
		t.Fatalf("a retry of the held drift rebase %+v %v", again, err)
	}

	later := f.s.now().Add(48 * time.Hour)
	if why, err := fm.driftDue(stream, later); err != nil || why != "" {
		t.Fatalf("a held workstream is due for a drift rebase: %q %v", why, err)
	}
	statuses, err := repository.Statuses()
	intakefixture.Must(t, err)
	i := slices.IndexFunc(statuses, func(w trace.WorkstreamStatus) bool { return w.Workstream == stream })
	decisions, err := f.s.openDecisions(ctx, repository, statuses[i])
	intakefixture.Must(t, err)
	j := slices.IndexFunc(decisions, func(e api.InboxEntry) bool { return e.Kind == api.InboxDrift })
	if j < 0 {
		t.Fatalf("the inbox does not list the held drift rebase: %+v", decisions)
	}
	if e := decisions[j]; e.Revision != 1 || e.Answer.Path != api.Prefix+"/rebase/"+string(stream) || len(e.Answer.Body) != 0 || !strings.Contains(e.Blocked, "osmia rebase "+string(stream)) || !strings.Contains(e.Question, "is held for the owner") {
		t.Fatalf("the held drift rebase entry %+v", e)
	}

	if k, err := fm.askDrift(ctx, stream, f.s.now()); err != nil || k != 2 {
		t.Fatalf("the owner's request answers drift rebase %d: %v", k, err)
	}
	if why, err := fm.driftDue(stream, later); err != nil || !strings.HasPrefix(why, "the owner asked for a drift rebase") {
		t.Fatalf("the owner's request does not make the held workstream due: %q %v", why, err)
	}
	statuses, err = repository.Statuses()
	intakefixture.Must(t, err)
	decisions, err = f.s.openDecisions(ctx, repository, statuses[i])
	intakefixture.Must(t, err)
	if slices.ContainsFunc(decisions, func(e api.InboxEntry) bool { return e.Kind == api.InboxDrift }) {
		t.Fatalf("the inbox still lists the drift rebase the owner asked again for: %+v", decisions)
	}
}

func TestDriftMarkerRemindersAreBoundedByCleanTurns(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := newFinalFixture(t, "drift-markers")
	f.s.cfg.Mason.MaxCleanTurns = 1
	d := drifter{&foreman{masons: newMasonController(f.s, repository)}}
	before, _, op := conflictedDrift(t, f, d, stream)
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution")
	done := completeDriftTurn(t, f, repository, stream, service.TestDriftMasonAgent, resolvedDone("Resolved"))
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution")
	remind := fmt.Sprintf("%s-markers-%d", service.TestDriftMasonAgent, done.Sequence)
	if ids := intakefixture.TurnIDs(t, repository, stream, service.TestDriftMasonAgent); !slices.Equal(ids, []string{lifecycle.DriftResolveTurnID(1, 1), remind}) {
		t.Fatalf("drift mason turns after a done with markers %v", ids)
	}
	completeDriftTurn(t, f, repository, stream, service.TestDriftMasonAgent, resolvedDone("Resolved again"))
	result, err := attemptOperation(t, f.s, repository, stream, op, d)
	if err != nil || result.Outcome != "succeeded" || !strings.Contains(result.Evidence, "drift rebase 1 is held for the owner: its mason left conflict markers in CODEOWNERS after 1 reminders, exhausting mason.max_clean_turns") {
		t.Fatalf("the drift rebase after a second done with markers %+v %v", result, err)
	}
	if ids := intakefixture.TurnIDs(t, repository, stream, service.TestDriftMasonAgent); len(ids) != 2 {
		t.Fatalf("the drift mason was reminded again: %v", ids)
	}
	if state, err := repository.Workflow(stream, lifecycle.DriftSubject); err != nil || state.Value != lifecycle.DriftHeld+"-1" {
		t.Fatalf("the drift subject is %+v: %v", state, err)
	}
	if tip := featureTip(t, f, stream); tip != before {
		t.Fatalf("a held drift rebase moved the feature branch to %s", tip)
	}
}
