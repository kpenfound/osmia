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

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

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
)

var (
	rejectedResolution = service.UnitVerdict{Decision: "material_findings", Summary: fixturekit.ReviewSummary,
		Findings: []api.ReviewFinding{{Severity: "major", Evidence: "The resolution drops @upstream.", Action: "Keep @upstream beside @feature."}}}
)

func TestDriftConflictIsResolvedByAMasonAndMovesTheBranchOnApproval(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := journey.NewFinalFixtureWith(t, "drift-resolve", "")
	d := journey.Drifter{Foreman: &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}}
	ctx := context.Background()
	before, upstream, op := journey.ConflictedDrift(t, f, d, stream)
	sealed := intakefixture.StreamDocuments(t, repository, stream, seal.DocumentID)

	journey.AwaitResolution(t, f.S, d, stream, op, "its mason's resolution of CODEOWNERS")
	if tip := journey.FeatureTip(t, f, stream); tip != before {
		t.Fatalf("a conflicted drift rebase moved the feature branch to %s", tip)
	}
	if after := intakefixture.StreamDocuments(t, repository, stream, seal.DocumentID); !reflect.DeepEqual(after, sealed) {
		t.Fatalf("a conflicted drift rebase changed the seal: %+v", after)
	}
	if ops := journey.DriftOperations(t, repository, stream); len(ops) != 1 || ops[0].Result != nil {
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
	if records := journey.DriftRecords(t, repository, stream); !reflect.DeepEqual(records, []lifecycle.DriftRebase{opened, stopped}) {
		t.Fatalf("drift/rebase.json %+v", records)
	}
	w := journey.ResolutionWorkspace(t, f, stream)
	if w.Branch != lifecycle.DriftBranch(stream, 1) {
		t.Fatalf("the resolution workspace is on %s", w.Branch)
	}
	data, err := os.ReadFile(filepath.Join(w.Path, "CODEOWNERS"))
	fixturekit.Must(t, err)
	if !strings.Contains(string(data), "<<<<<<< ") || !strings.Contains(string(data), "@upstream") || !strings.Contains(string(data), "@feature") {
		t.Fatalf("the resolution workspace's CODEOWNERS holds %q", data)
	}
	if ids := intakefixture.TurnIDs(t, repository, stream, intake.DriftMasonAgent); !slices.Equal(ids, []string{lifecycle.DriftResolveTurnID(1, 1)}) {
		t.Fatalf("drift mason turns %v", ids)
	}
	th, err := repository.Thread(stream, intake.DriftMasonAgent)
	fixturekit.Must(t, err)
	if th.Identity.Role != intake.MasonRole {
		t.Fatalf("the drift mason's role is %s", th.Identity.Role)
	}
	for _, want := range []string{"- CODEOWNERS\n", "conflict markers", "## Sealed spec: spec.md", "## end of spec.md", before} {
		if !strings.Contains(th.Turns[0].Request.Prompt, want) {
			t.Fatalf("the resolve turn lacks %q:\n%s", want, th.Turns[0].Request.Prompt)
		}
	}

	journey.AwaitResolution(t, f.S, d, stream, op, "its mason's resolution of CODEOWNERS")
	if ids := intakefixture.TurnIDs(t, repository, stream, intake.DriftMasonAgent); len(ids) != 1 {
		t.Fatalf("drift mason turns %v", ids)
	}

	fixturekit.Must(t, os.WriteFile(filepath.Join(w.Path, "CODEOWNERS"), []byte("/internal/ @upstream @feature\n"), 0600))
	journey.CompleteDriftTurn(t, f, repository, stream, intake.DriftMasonAgent, journey.ResolvedDone("Kept both owners"))
	journey.AwaitResolution(t, f.S, d, stream, op, "review 1 of candidate")
	records := journey.DriftRecords(t, repository, stream)
	candidate := records[len(records)-1].Candidate
	resolved := stopped
	resolved.Outcome, resolved.Candidate, resolved.Review = intake.DriftResolved, candidate, 1
	if !reflect.DeepEqual(records[len(records)-1], resolved) || journey.ParentOf(t, f, candidate) != upstream {
		t.Fatalf("the resolved record %+v, want %+v on %s", records[len(records)-1], resolved, upstream)
	}
	if tip := journey.FeatureTip(t, f, stream); tip != before {
		t.Fatalf("the feature branch moved to %s before review", tip)
	}
	review, err := repository.Thread(stream, intake.DriftReviewerAgent)
	fixturekit.Must(t, err)
	if review.Identity.Role != intake.ReviewerRole || len(review.Turns) != 1 || review.Turns[0].Request.TurnID != lifecycle.DriftReviewTurnID(1, 1) {
		t.Fatalf("the drift reviewer's thread %+v", review)
	}
	for _, want := range []string{candidate, "- CODEOWNERS", `Change "resolution"`, "## Sealed spec: spec.md"} {
		if !strings.Contains(review.Turns[0].Request.Prompt, want) {
			t.Fatalf("the review turn lacks %q:\n%s", want, review.Turns[0].Request.Prompt)
		}
	}
	if resolution := journey.DriftResolutionDiff(t, f, repository, stream); !strings.Contains(resolution, "+/internal/ @upstream @feature") || strings.Contains(review.Turns[0].Request.Prompt, "+/internal/ @upstream @feature") {
		t.Fatalf("the reviewer reads the resolution %q through its prompt or not at all", resolution)
	}

	journey.CompleteDriftTurn(t, f, repository, stream, intake.DriftReviewerAgent, journey.DriftVerdict(t, journey.ApprovedResolution))
	result, err := journey.AttemptOperation(t, f.S, repository, stream, op, d)
	if err != nil || result.Outcome != "succeeded" {
		t.Fatalf("the approved drift rebase %+v %v", result, err)
	}
	if tip := journey.FeatureTip(t, f, stream); tip != candidate || journey.FileAt(t, f, tip, "CODEOWNERS") != "/internal/ @upstream @feature\n" {
		t.Fatalf("the feature branch is at %s, not the approved candidate %s", tip, candidate)
	}
	after := intakefixture.Seals(t, repository, stream)
	if len(after) != 2 || after[1].Base.Commit != upstream {
		t.Fatalf("seals after the approved drift rebase %+v", after)
	}
	records = journey.DriftRecords(t, repository, stream)
	replayed := resolved
	replayed.Outcome, replayed.Commit, replayed.Verdict = lifecycle.DriftReplayed, candidate, &journey.ApprovedResolution
	rebased := replayed
	rebased.Outcome, rebased.Seal, rebased.SealRevision = lifecycle.DriftRebased, 1, 2
	if !reflect.DeepEqual(records[len(records)-2:], []lifecycle.DriftRebase{replayed, rebased}) {
		t.Fatalf("drift/rebase.json ends %+v", records[len(records)-2:])
	}
	if done := intakefixture.TransitionByID(t, repository, stream, "drift-1-rebased"); done.From != "conflicted-1" || done.To != "rebased-1" {
		t.Fatalf("the outcome %+v", done)
	}
	if _, found, err := intake.Workspaces(f.S.Cfg, intake.DriftsDirectory, config.WorkspacesGit).Workspace(ctx, string(stream)); err != nil || found {
		t.Fatalf("the resolution workspace outlived the drift rebase: %t %v", found, err)
	}
	if seen, err := d.Inspect(ctx, op); err != nil || seen.State != coreadapter.EffectCompleted {
		t.Fatalf("inspection after the drift rebase %+v %v", seen, err)
	}
}

func TestRejectedDriftResolutionReturnsToTheMasonAndKeepsTheBranch(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := journey.NewFinalFixtureWith(t, "drift-reject", "")
	d := journey.Drifter{Foreman: &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}}
	before, upstream, op := journey.ConflictedDrift(t, f, d, stream)
	sealed := intakefixture.StreamDocuments(t, repository, stream, seal.DocumentID)
	journey.AwaitResolution(t, f.S, d, stream, op, "its mason's resolution")
	w := journey.ResolutionWorkspace(t, f, stream)

	done := journey.CompleteDriftTurn(t, f, repository, stream, intake.DriftMasonAgent, journey.ResolvedDone("Resolved"))
	journey.AwaitResolution(t, f.S, d, stream, op, "its mason's resolution")
	remind := fmt.Sprintf("%s-markers-%d", intake.DriftMasonAgent, done.Sequence)
	if ids := intakefixture.TurnIDs(t, repository, stream, intake.DriftMasonAgent); !slices.Equal(ids, []string{lifecycle.DriftResolveTurnID(1, 1), remind}) {
		t.Fatalf("drift mason turns after a done with markers %v", ids)
	}
	journey.AwaitResolution(t, f.S, d, stream, op, "its mason's resolution")
	if ids := intakefixture.TurnIDs(t, repository, stream, intake.DriftMasonAgent); len(ids) != 2 {
		t.Fatalf("the reminder was queued again: %v", ids)
	}

	fixturekit.Must(t, os.WriteFile(filepath.Join(w.Path, "CODEOWNERS"), []byte("/internal/ @feature\n"), 0600))
	journey.CompleteDriftTurn(t, f, repository, stream, intake.DriftMasonAgent, journey.ResolvedDone("Kept the feature owner"))
	journey.AwaitResolution(t, f.S, d, stream, op, "review 1")
	first := journey.DriftRecords(t, repository, stream)
	candidate := first[len(first)-1].Candidate
	journey.CompleteDriftTurn(t, f, repository, stream, intake.DriftReviewerAgent, journey.DriftVerdict(t, rejectedResolution))
	journey.AwaitResolution(t, f.S, d, stream, op, "its mason's answer to review 1")
	records := journey.DriftRecords(t, repository, stream)
	rejected := records[len(records)-1]
	if rejected.Outcome != intake.DriftRejected || rejected.Review != 1 || rejected.Candidate != candidate || !reflect.DeepEqual(rejected.Verdict, &rejectedResolution) {
		t.Fatalf("the rejected record %+v", rejected)
	}
	if tip := journey.FeatureTip(t, f, stream); tip != before {
		t.Fatalf("a rejected resolution moved the feature branch to %s", tip)
	}
	if after := intakefixture.StreamDocuments(t, repository, stream, seal.DocumentID); !reflect.DeepEqual(after, sealed) {
		t.Fatalf("a rejected resolution changed the seal: %+v", after)
	}
	th, err := repository.Thread(stream, intake.DriftMasonAgent)
	fixturekit.Must(t, err)
	fix := th.Turns[len(th.Turns)-1]
	if fix.Request.TurnID != lifecycle.DriftFixTurnID(1, 1) || !strings.Contains(fix.Request.Prompt, "Keep @upstream beside @feature.") || !strings.Contains(fix.Request.Prompt, "## Sealed spec: spec.md") {
		t.Fatalf("the mason's turn after the rejection %s:\n%s", fix.Request.TurnID, fix.Request.Prompt)
	}

	fixturekit.Must(t, os.WriteFile(filepath.Join(w.Path, "CODEOWNERS"), []byte("/internal/ @upstream @feature\n"), 0600))
	journey.CompleteDriftTurn(t, f, repository, stream, intake.DriftMasonAgent, journey.ResolvedDone("Kept both owners"))
	journey.AwaitResolution(t, f.S, d, stream, op, "review 2")
	records = journey.DriftRecords(t, repository, stream)
	second := records[len(records)-1]
	if second.Outcome != intake.DriftResolved || second.Review != 2 || second.Candidate == candidate || second.Verdict != nil || journey.FileAt(t, f, second.Candidate, "CODEOWNERS") != "/internal/ @upstream @feature\n" {
		t.Fatalf("the second candidate %+v", second)
	}
	if ids := intakefixture.TurnIDs(t, repository, stream, intake.DriftReviewerAgent); !slices.Equal(ids, []string{lifecycle.DriftReviewTurnID(1, 1), lifecycle.DriftReviewTurnID(1, 2)}) {
		t.Fatalf("drift reviewer turns %v", ids)
	}
	if tip := journey.FeatureTip(t, f, stream); tip != before {
		t.Fatalf("the feature branch moved to %s before the second review", tip)
	}

	journey.CompleteDriftTurn(t, f, repository, stream, intake.DriftReviewerAgent, journey.DriftVerdict(t, journey.ApprovedResolution))
	if result, err := journey.AttemptOperation(t, f.S, repository, stream, op, d); err != nil || result.Outcome != "succeeded" {
		t.Fatalf("the approved drift rebase %+v %v", result, err)
	}
	if tip := journey.FeatureTip(t, f, stream); tip != second.Candidate {
		t.Fatalf("the feature branch is at %s, not the approved candidate %s", tip, second.Candidate)
	}
	if after := intakefixture.Seals(t, repository, stream); len(after) != 2 || after[1].Base.Commit != upstream {
		t.Fatalf("seals after approval %+v", after)
	}
}

func TestRestartDuringDriftResolutionResumesWithoutDuplicates(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := journey.NewFinalFixtureWith(t, "drift-restart", "")
	d := journey.Drifter{Foreman: &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}}
	ctx := context.Background()
	before, _, op := journey.ConflictedDrift(t, f, d, stream)
	stops := 0
	f.S.Boundary = func(name string) error {
		if name == "drift-resolving" {
			stops++
			if stops == 1 {
				return errors.New("the service stopped")
			}
		}
		return nil
	}
	if _, err := journey.AttemptOperation(t, f.S, repository, stream, op, d); err == nil || !strings.Contains(err.Error(), "the service stopped") {
		t.Fatalf("the drift rebase stopped before its resolution replay: %v", err)
	}
	f.S.ClearBoundary()
	restart := func() {
		t.Helper()
		fixturekit.Must(t, repository.Close())
		var err error
		repository, err = trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
		fixturekit.Must(t, err)
		d = journey.Drifter{Foreman: &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}}
	}
	restart()
	journey.AwaitResolution(t, f.S, d, stream, op, "its mason's resolution")
	w := journey.ResolutionWorkspace(t, f, stream)
	marked, err := os.ReadFile(filepath.Join(w.Path, "CODEOWNERS"))
	fixturekit.Must(t, err)
	records := len(journey.DriftRecords(t, repository, stream))

	restart()
	journey.AwaitResolution(t, f.S, d, stream, op, "its mason's resolution")
	if ids := intakefixture.TurnIDs(t, repository, stream, intake.DriftMasonAgent); !slices.Equal(ids, []string{lifecycle.DriftResolveTurnID(1, 1)}) {
		t.Fatalf("drift mason turns after a restart %v", ids)
	}
	if n := len(journey.DriftRecords(t, repository, stream)); n != records {
		t.Fatalf("a restart recorded %d drift records, not %d", n, records)
	}
	if again, err := os.ReadFile(filepath.Join(w.Path, "CODEOWNERS")); err != nil || string(again) != string(marked) {
		t.Fatalf("a restart replayed the resolution workspace again: %q %v", again, err)
	}

	turn := lifecycle.DriftResolveTurnID(1, 1)
	paths, err := execution.ViewPaths(w.Path)
	fixturekit.Must(t, err)
	viewRoot := filepath.Join(f.S.Cfg.Root.String(), "views", string(f.Project), string(stream), intake.DriftMasonAgent, turn)
	fixturekit.Must(t, os.MkdirAll(viewRoot, 0700))
	view, err := (isolation.Views{Directory: viewRoot}).Create(ctx, coreadapter.Workspace{Directory: w.Path, Access: coreadapter.ReadWrite}, paths)
	fixturekit.Must(t, err)
	fixturekit.Must(t, os.WriteFile(filepath.Join(viewRoot, "ready"), []byte(filepath.Base(view.Workspace().Directory)), 0600))
	fixturekit.Must(t, os.WriteFile(filepath.Join(view.Workspace().Directory, "CODEOWNERS"), []byte("/internal/ @upstream @feature\n"), 0600))
	_, err = repository.ClaimTurn(ctx, stream, intake.DriftMasonAgent, "crashed", filepath.Join(f.S.Cfg.Root.String(), "threads", string(f.Project), string(stream), intake.DriftMasonAgent, turn), f.Clock.Now())
	fixturekit.Must(t, err)
	restart()
	journey.AwaitResolution(t, f.S, d, stream, op, "its mason's resolution")
	journey.AwaitResolution(t, f.S, d, stream, op, "its mason's resolution")
	recover := fmt.Sprintf("%s-recover-1", intake.DriftMasonAgent)
	if ids := intakefixture.TurnIDs(t, repository, stream, intake.DriftMasonAgent); !slices.Equal(ids, []string{turn, recover}) {
		t.Fatalf("drift mason turns after an interrupted turn %v", ids)
	}
	if data, err := os.ReadFile(filepath.Join(w.Path, "CODEOWNERS")); err != nil || string(data) != "/internal/ @upstream @feature\n" {
		t.Fatalf("the interrupted turn's view was not copied back: %q %v", data, err)
	}

	journey.CompleteDriftTurn(t, f, repository, stream, intake.DriftMasonAgent, journey.ResolvedDone("Kept both owners"))
	for _, point := range []string{"drift-staging"} {
		f.S.Boundary = func(name string) error {
			if name == point {
				return errors.New("the service stopped")
			}
			return nil
		}
		if _, err := journey.AttemptOperation(t, f.S, repository, stream, op, d); err == nil || !strings.Contains(err.Error(), "the service stopped") {
			t.Fatalf("the drift rebase did not stop at %s: %v", point, err)
		}
		f.S.ClearBoundary()
		restart()
	}
	journey.AwaitResolution(t, f.S, d, stream, op, "review 1")
	journey.CompleteDriftTurn(t, f, repository, stream, intake.DriftReviewerAgent, journey.DriftVerdict(t, journey.ApprovedResolution))
	f.S.Boundary = func(name string) error {
		if name == "drift-approved" {
			return errors.New("the service stopped")
		}
		return nil
	}
	if _, err := journey.AttemptOperation(t, f.S, repository, stream, op, d); err == nil || !strings.Contains(err.Error(), "the service stopped") {
		t.Fatalf("the drift rebase did not stop after its approval: %v", err)
	}
	f.S.ClearBoundary()
	if tip := journey.FeatureTip(t, f, stream); tip != before {
		t.Fatalf("the feature branch moved to %s before the approval was applied", tip)
	}
	restart()
	if result, err := journey.AttemptOperation(t, f.S, repository, stream, op, d); err != nil || result.Outcome != "succeeded" {
		t.Fatalf("the approved drift rebase after a restart %+v %v", result, err)
	}
	records2 := journey.DriftRecords(t, repository, stream)
	candidate := records2[len(records2)-1].Commit
	if tip := journey.FeatureTip(t, f, stream); tip != candidate || journey.FileAt(t, f, tip, "CODEOWNERS") != "/internal/ @upstream @feature\n" {
		t.Fatalf("the feature branch is at %s, not %s", tip, candidate)
	}
	if ids := intakefixture.TurnIDs(t, repository, stream, intake.DriftReviewerAgent); len(ids) != 1 {
		t.Fatalf("drift reviewer turns %v", ids)
	}
	if _, found, err := intake.Workspaces(f.S.Cfg, intake.DriftsDirectory, config.WorkspacesGit).Workspace(ctx, string(stream)); err != nil || found {
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
	f, stream, repository, _ := journey.NewFinalFixtureWith(t, "drift-workspace", "")
	d := journey.Drifter{Foreman: &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}}
	ctx := context.Background()
	_, _, op := journey.ConflictedDrift(t, f, d, stream)
	journey.AwaitResolution(t, f.S, d, stream, op, "its mason's resolution")
	w := journey.ResolutionWorkspace(t, f, stream)
	drifts := lifecycle.Resolutions{StreamWorkspaces: lifecycle.DriftWorkspaces(f.S.Cfg, repository)}
	scope := coreadapter.Scope{Project: string(f.Project), Workstream: string(stream), Thread: intake.DriftMasonAgent, Turn: lifecycle.DriftResolveTurnID(1, 1), Role: intake.MasonRole}

	lease, err := service.ThreadWorkspaceAcquireForTest(ctx, f.S.Cfg, repository, coreadapter.WorkspaceRequest{Scope: scope, Access: coreadapter.ReadWrite})
	fixturekit.Must(t, err)
	if lease.Workspace.Directory != w.Path {
		t.Fatalf("the drift mason turn is lent %s, not %s", lease.Workspace.Directory, w.Path)
	}
	if _, err := drifts.Acquire(ctx, coreadapter.WorkspaceRequest{Scope: coreadapter.Scope{Project: scope.Project, Workstream: scope.Workstream, Thread: intake.MasonAgent("resume"), Unit: "resume", Role: intake.MasonRole}}); err == nil {
		t.Fatal("a unit's mason turn was lent the resolution workspace")
	}
	selection, err := drifts.Selection(ctx, scope, coreadapter.ExecutionSettings{})
	fixturekit.Must(t, err)
	if slices.Contains(selection.Paths, ".git") || !slices.Contains(selection.Paths, "CODEOWNERS") {
		t.Fatalf("the drift mason's view selects %v", selection.Paths)
	}
	if selection.Narrow != nil {
		t.Fatalf("the drift mason's selection narrows the role grant: %+v", selection.Narrow)
	}
	if !slices.Equal(service.TestMasonGrant.Tools, []string{"file_read", "file_write", questions.AskTool, questions.AmendTool, intake.FactoryContextTool, "record_discovery", execution.DoneTool}) || !service.TestMasonGrant.WriteFiles || !service.TestMasonGrant.Execute {
		t.Fatalf("masonGrant %+v", service.TestMasonGrant)
	}

	viewRoot := filepath.Join(t.TempDir(), "views")
	fixturekit.Must(t, os.MkdirAll(viewRoot, 0700))
	view, err := (isolation.Views{Directory: viewRoot}).Create(ctx, lease.Workspace, selection.Paths)
	fixturekit.Must(t, err)
	fixturekit.Must(t, os.WriteFile(filepath.Join(view.Workspace().Directory, "CODEOWNERS"), []byte("resolved\n"), 0600))
	fixturekit.Must(t, drifts.Capture(ctx, scope, view))
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
		fixturekit.Must(t, err)
		recorded := raw == `{"outcome":"Kept both owners"}`
		if strings.Contains(string(out), `"recorded":true`) != recorded {
			t.Fatalf("done %s returned %s", raw, out)
		}
	}
	if got := reports.AcceptedForTest(execution.TurnKey(scope)); got.Report.Outcome != "Kept both owners" || got.Card != nil {
		t.Fatalf("the accepted report %+v", got)
	}
}

func TestDriftResolutionIsHeldForTheOwnerAtMaxBounces(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := journey.NewFinalFixtureWith(t, "drift-held", "")
	f.S.Cfg.Shed.MaxBounces = 2
	fm := cadenceForeman(f, repository, "1h")
	d := journey.Drifter{Foreman: fm}
	ctx := context.Background()
	before, _, op := journey.ConflictedDrift(t, f, d, stream)
	sealed := intakefixture.StreamDocuments(t, repository, stream, seal.DocumentID)
	journey.AwaitResolution(t, f.S, d, stream, op, "its mason's resolution")
	w := journey.ResolutionWorkspace(t, f, stream)
	for review := 1; review <= 2; review++ {
		fixturekit.Must(t, os.WriteFile(filepath.Join(w.Path, "CODEOWNERS"), []byte(fmt.Sprintf("/internal/ @feature%d\n", review)), 0600))
		journey.CompleteDriftTurn(t, f, repository, stream, intake.DriftMasonAgent, journey.ResolvedDone("Resolved"))
		journey.AwaitResolution(t, f.S, d, stream, op, fmt.Sprintf("review %d", review))
		journey.CompleteDriftTurn(t, f, repository, stream, intake.DriftReviewerAgent, journey.DriftVerdict(t, rejectedResolution))
		if review == 1 {
			journey.AwaitResolution(t, f.S, d, stream, op, "its mason's answer to review 1")
		}
	}
	result, err := journey.AttemptOperation(t, f.S, repository, stream, op, d)
	if err != nil || result.Outcome != "succeeded" || !strings.Contains(result.Evidence, "drift rebase 1 is held for the owner: review sent its resolution of the conflicts in CODEOWNERS back 2 times, reaching shed.max_bounces") {
		t.Fatalf("the drift rebase at max_bounces %+v %v", result, err)
	}
	if ids := intakefixture.TurnIDs(t, repository, stream, intake.DriftMasonAgent); !slices.Equal(ids, []string{lifecycle.DriftResolveTurnID(1, 1), lifecycle.DriftFixTurnID(1, 1)}) {
		t.Fatalf("drift mason turns %v", ids)
	}
	if state, err := repository.Workflow(stream, lifecycle.DriftSubject); err != nil || state.Value != lifecycle.DriftHeld+"-1" {
		t.Fatalf("the drift subject is %+v: %v", state, err)
	}
	if tip := journey.FeatureTip(t, f, stream); tip != before {
		t.Fatalf("a held drift rebase moved the feature branch to %s", tip)
	}
	if after := intakefixture.StreamDocuments(t, repository, stream, seal.DocumentID); !reflect.DeepEqual(after, sealed) {
		t.Fatalf("a held drift rebase changed the seal: %+v", after)
	}
	if _, found, err := intake.Workspaces(f.S.Cfg, intake.DriftsDirectory, journey.BackendOf(t, f, stream)).Workspace(ctx, string(stream)); err != nil || found {
		t.Fatalf("the held drift rebase kept its resolution workspace: %v %v", found, err)
	}
	if again, err := d.Apply(ctx, op); err != nil || again.Outcome != result.Outcome || again.Evidence != result.Evidence {
		t.Fatalf("a retry of the held drift rebase %+v %v", again, err)
	}

	later := f.S.Now().Add(48 * time.Hour)
	if why, err := fm.DriftDriver().DriftDue(stream, later); err != nil || why != "" {
		t.Fatalf("a held workstream is due for a drift rebase: %q %v", why, err)
	}
	statuses, err := repository.Statuses()
	fixturekit.Must(t, err)
	i := slices.IndexFunc(statuses, func(w trace.WorkstreamStatus) bool { return w.Workstream == stream })
	decisions, err := journey.InspectForTest(f.S).OpenDecisions(ctx, repository, statuses[i])
	fixturekit.Must(t, err)
	j := slices.IndexFunc(decisions, func(e api.InboxEntry) bool { return e.Kind == api.InboxDrift })
	if j < 0 {
		t.Fatalf("the inbox does not list the held drift rebase: %+v", decisions)
	}
	if e := decisions[j]; e.Revision != 1 || e.Answer.Path != api.Prefix+"/rebase/"+string(stream) || len(e.Answer.Body) != 0 || !strings.Contains(e.Blocked, "osmia rebase "+string(stream)) || !strings.Contains(e.Question, "is held for the owner") {
		t.Fatalf("the held drift rebase entry %+v", e)
	}

	if k, err := fm.DriftDriver().AskDrift(ctx, stream, f.S.Now()); err != nil || k != 2 {
		t.Fatalf("the owner's request answers drift rebase %d: %v", k, err)
	}
	if why, err := fm.DriftDriver().DriftDue(stream, later); err != nil || !strings.HasPrefix(why, "the owner asked for a drift rebase") {
		t.Fatalf("the owner's request does not make the held workstream due: %q %v", why, err)
	}
	statuses, err = repository.Statuses()
	fixturekit.Must(t, err)
	decisions, err = journey.InspectForTest(f.S).OpenDecisions(ctx, repository, statuses[i])
	fixturekit.Must(t, err)
	if slices.ContainsFunc(decisions, func(e api.InboxEntry) bool { return e.Kind == api.InboxDrift }) {
		t.Fatalf("the inbox still lists the drift rebase the owner asked again for: %+v", decisions)
	}
}

func TestDriftMarkerRemindersAreBoundedByCleanTurns(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := journey.NewFinalFixtureWith(t, "drift-markers", "")
	f.S.Cfg.Mason.MaxCleanTurns = 1
	d := journey.Drifter{Foreman: &journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}}
	before, _, op := journey.ConflictedDrift(t, f, d, stream)
	journey.AwaitResolution(t, f.S, d, stream, op, "its mason's resolution")
	done := journey.CompleteDriftTurn(t, f, repository, stream, intake.DriftMasonAgent, journey.ResolvedDone("Resolved"))
	journey.AwaitResolution(t, f.S, d, stream, op, "its mason's resolution")
	remind := fmt.Sprintf("%s-markers-%d", intake.DriftMasonAgent, done.Sequence)
	if ids := intakefixture.TurnIDs(t, repository, stream, intake.DriftMasonAgent); !slices.Equal(ids, []string{lifecycle.DriftResolveTurnID(1, 1), remind}) {
		t.Fatalf("drift mason turns after a done with markers %v", ids)
	}
	journey.CompleteDriftTurn(t, f, repository, stream, intake.DriftMasonAgent, journey.ResolvedDone("Resolved again"))
	result, err := journey.AttemptOperation(t, f.S, repository, stream, op, d)
	if err != nil || result.Outcome != "succeeded" || !strings.Contains(result.Evidence, "drift rebase 1 is held for the owner: its mason left conflict markers in CODEOWNERS after 1 reminders, exhausting mason.max_clean_turns") {
		t.Fatalf("the drift rebase after a second done with markers %+v %v", result, err)
	}
	if ids := intakefixture.TurnIDs(t, repository, stream, intake.DriftMasonAgent); len(ids) != 2 {
		t.Fatalf("the drift mason was reminded again: %v", ids)
	}
	if state, err := repository.Workflow(stream, lifecycle.DriftSubject); err != nil || state.Value != lifecycle.DriftHeld+"-1" {
		t.Fatalf("the drift subject is %+v: %v", state, err)
	}
	if tip := journey.FeatureTip(t, f, stream); tip != before {
		t.Fatalf("a held drift rebase moved the feature branch to %s", tip)
	}
}
