package service

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

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

// attemptOperation reconciles one pending operation as the reconciliation
// controller does and returns what its effect returned: an effect that
// fails records a retry and no result, so the operation stays pending.
func attemptOperation(t *testing.T, s *Service, repository *trace.Repository, stream config.WorkstreamID, op coreadapter.Operation, r coreadapter.Reconciler) (coreadapter.OperationResult, error) {
	t.Helper()
	ctx := context.Background()
	ops, err := repository.Operations(stream)
	must(t, err)
	i := slices.IndexFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.ID == op.ID })
	if i < 0 {
		t.Fatalf("operation %s is not recorded", op.ID)
	}
	var result coreadapter.OperationResult
	var applied error
	now := s.now()
	must(t, repository.WithOperation(ctx, stream, ops[i].EventID, foremanActor, func() time.Time { return now }, func(a *trace.OperationAttempt, _ trace.OperationRecord) error {
		seen, err := r.Inspect(ctx, op)
		if err != nil {
			return err
		}
		observe := a.Action("observe", now)
		observe.Observation = &seen
		if err := a.Record(ctx, observe); err != nil {
			return err
		}
		if seen.State == coreadapter.EffectCompleted {
			result = *seen.Result
		} else {
			if err := a.Record(ctx, a.Action("effect", now)); err != nil {
				return err
			}
			if result, applied = r.Apply(ctx, op); applied != nil {
				retry := a.Action("retry", now)
				retry.Failure, retry.RetryAt = applied.Error(), now.Add(time.Nanosecond)
				return a.Record(ctx, retry)
			}
		}
		done := a.Action("result", now)
		done.Result = &result
		return a.Record(ctx, done)
	}))
	return result, applied
}

// awaitResolution attempts the drift operation and fails unless it waits
// for its conflict resolution.
func awaitResolution(t *testing.T, s *Service, d drifter, stream config.WorkstreamID, op coreadapter.Operation, want string) {
	t.Helper()
	if result, err := attemptOperation(t, s, d.repository, stream, op, d); !errors.Is(err, errDriftAwaits) || !strings.Contains(err.Error(), want) {
		t.Fatalf("the drift rebase did not await %q: %+v %v", want, result, err)
	}
}

// completeDriftTurn runs the oldest unfinished turn of the workstream's
// drift agent as a finished turn that ends with outcome, and returns it.
func completeDriftTurn(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, agent string, outcome *coreadapter.Outcome) trace.QueuedTurn {
	t.Helper()
	ctx := context.Background()
	th, err := repository.Thread(stream, agent)
	must(t, err)
	i := slices.IndexFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.CompletedAt.IsZero() })
	if i < 0 {
		t.Fatalf("thread %s has no turn to run", agent)
	}
	token := "done-" + th.Turns[i].Request.TurnID
	directory := filepath.Join(f.s.cfg.Root.String(), "threads", string(f.project), string(stream), agent, token)
	q, err := repository.ClaimTurn(ctx, stream, agent, token, directory, f.clock.Now())
	must(t, err)
	completeClaimedTurn(t, f, repository, stream, agent, q, outcome)
	return q
}

// completeClaimedTurn ends the claimed turn q of the workstream's agent as a
// finished turn with outcome.
func completeClaimedTurn(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, agent string, q trace.QueuedTurn, outcome *coreadapter.Outcome) {
	t.Helper()
	ctx := context.Background()
	h := q.Request.Header
	h.Schema, h.ID, h.At = "osmia.trace.turn-response", trace.EventID(q.Request.ID, "response"), f.clock.Now()
	response := trace.TurnResponse{Header: h, AgentID: agent, ThreadID: agent, TurnID: q.Request.TurnID, RequestID: q.Request.ID, RequestRevision: q.Request.Revision,
		Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: q.Request.Profile.Backend, ID: q.Claim.Token}, SessionDirectory: q.Claim.SessionDirectory, StartedAt: q.Claim.At, Outcome: outcome}}
	must(t, repository.CaptureTurn(ctx, q.Claim.Token, response))
	must(t, repository.CompleteTurn(ctx, stream, agent, q.Request.TurnID, q.Claim.Token, f.clock.Now()))
}

// resolvedDone is the outcome of a drift mason turn whose done the service
// accepted.
func resolvedDone(outcome string) *coreadapter.Outcome {
	data, _ := json.Marshal(MasonReport{Outcome: outcome})
	return &coreadapter.Outcome{Status: masonDone, Report: string(data)}
}

// driftVerdict is the outcome of a drift reviewer turn that recorded
// verdict.
func driftVerdict(t *testing.T, verdict UnitVerdict) *coreadapter.Outcome {
	t.Helper()
	data, err := json.Marshal(verdict)
	must(t, err)
	return &coreadapter.Outcome{Status: verdictOutcome, Report: string(data)}
}

var (
	approvedResolution = UnitVerdict{Decision: "satisfactory", Evidence: []ReviewEvidence{{Criterion: "spec#1", Evidence: "Both owners stand"}}}
	rejectedResolution = UnitVerdict{Decision: "material_findings", Evidence: []ReviewEvidence{{Criterion: "spec#1", Evidence: "The upstream owner is gone"}},
		Findings: []ReviewFinding{{Criterion: "spec#1", Severity: "major", Evidence: "The resolution drops @upstream.", Action: "Keep @upstream beside @feature."}}}
)

// turnIDs returns the turn IDs of the workstream's agent thread, or none
// when it has no thread.
func turnIDs(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, agent string) []string {
	t.Helper()
	th, err := repository.Thread(stream, agent)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	must(t, err)
	var ids []string
	for _, q := range th.Turns {
		ids = append(ids, q.Request.TurnID)
	}
	return ids
}

// resolutionWorkspace returns the workstream's drift resolution workspace.
func resolutionWorkspace(t *testing.T, f *shedFixture, stream config.WorkstreamID) workspace.Worktree {
	t.Helper()
	w, found, err := workspaces(f.s.cfg, driftsDirectory, backendOf(t, f, stream)).Workspace(context.Background(), string(stream))
	must(t, err)
	if !found {
		t.Fatal("the workstream has no drift resolution workspace")
	}
	return w
}

// featureTip returns the tip of the workstream's feature branch.
func featureTip(t *testing.T, f *shedFixture, stream config.WorkstreamID) string {
	t.Helper()
	tip, _, err := workspaces(f.s.cfg, branchesDirectory, backendOf(t, f, stream)).Branch(context.Background(), featureBranch(stream))
	must(t, err)
	return tip
}

// conflictedDrift moves the feature branch and upstream to conflicting
// CODEOWNERS files and asks for a drift rebase. It returns the branch's tip,
// the upstream commit and the operation.
func conflictedDrift(t *testing.T, f *shedFixture, d drifter, stream config.WorkstreamID) (string, string, coreadapter.Operation) {
	t.Helper()
	before := moveFeature(t, f, stream, map[string]string{"CODEOWNERS": "/internal/ @feature\n"})
	upstream := advanceUpstream(t, f, map[string]string{"CODEOWNERS": "/internal/ @upstream\n"})
	return before, upstream, requestDrift(t, d, stream)
}

// A drift rebase whose replay conflicts keeps the feature branch and the
// seal where they are and holds its operation open while the conflict is
// resolved: the replay stops in a resolution workspace of its own with the
// conflict markers, and the drift mason gets one explicit turn that names
// the conflicted path and carries the sealed spec. Once the mason is done,
// the service completes the replay and a reviewer reads the resolved
// candidate against the sealed spec. Approval moves the feature branch to
// the candidate and the seal's base to upstream together, and removes the
// resolution workspace.
func TestDriftConflictIsResolvedByAMasonAndMovesTheBranchOnApproval(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := newFinalFixture(t, "drift-resolve")
	d := drifter{&foreman{masons: newMasonController(f.s, repository)}}
	ctx := context.Background()
	before, upstream, op := conflictedDrift(t, f, d, stream)
	sealed := streamDocuments(t, repository, stream, seal.DocumentID)

	awaitResolution(t, f.s, d, stream, op, "its mason's resolution of CODEOWNERS")
	if tip := featureTip(t, f, stream); tip != before {
		t.Fatalf("a conflicted drift rebase moved the feature branch to %s", tip)
	}
	if after := streamDocuments(t, repository, stream, seal.DocumentID); !reflect.DeepEqual(after, sealed) {
		t.Fatalf("a conflicted drift rebase changed the seal: %+v", after)
	}
	if ops := driftOperations(t, repository, stream); len(ops) != 1 || ops[0].Result != nil {
		t.Fatalf("the conflicted drift operation %+v", ops)
	}
	if conflicted := transitionByID(t, repository, stream, "drift-1-conflicted"); conflicted.To != "conflicted-1" || !strings.Contains(conflicted.Reason, "CODEOWNERS conflicted") {
		t.Fatalf("the conflict %+v", conflicted)
	}
	base := DriftRebase{Drift: 1, Operation: op.ID, Branch: featureBranch(stream), Upstream: seal.Base{Remote: "upstream", Branch: "main", Commit: upstream}, From: seals(t, repository, stream)[0].Base.Commit, Before: before, Conflicts: []string{"CODEOWNERS"}}
	stopped := base
	stopped.Outcome, stopped.Round, stopped.Stop = driftConflicted, 1, before
	opened := base
	opened.Outcome = driftConflicted
	if records := driftRecords(t, repository, stream); !reflect.DeepEqual(records, []DriftRebase{opened, stopped}) {
		t.Fatalf("drift/rebase.json %+v", records)
	}
	w := resolutionWorkspace(t, f, stream)
	if w.Branch != driftBranch(stream, 1) {
		t.Fatalf("the resolution workspace is on %s", w.Branch)
	}
	data, err := os.ReadFile(filepath.Join(w.Path, "CODEOWNERS"))
	must(t, err)
	if !strings.Contains(string(data), "<<<<<<< ") || !strings.Contains(string(data), "@upstream") || !strings.Contains(string(data), "@feature") {
		t.Fatalf("the resolution workspace's CODEOWNERS holds %q", data)
	}
	if ids := turnIDs(t, repository, stream, driftMasonAgent); !slices.Equal(ids, []string{driftResolveTurnID(1, 1)}) {
		t.Fatalf("drift mason turns %v", ids)
	}
	th, err := repository.Thread(stream, driftMasonAgent)
	must(t, err)
	if th.Identity.Role != masonRole {
		t.Fatalf("the drift mason's role is %s", th.Identity.Role)
	}
	for _, want := range []string{"- CODEOWNERS\n", "conflict markers", "## Sealed spec: spec.md", "## end of spec.md", before} {
		if !strings.Contains(th.Turns[0].Request.Prompt, want) {
			t.Fatalf("the resolve turn lacks %q:\n%s", want, th.Turns[0].Request.Prompt)
		}
	}

	// Another attempt before the mason is done queues nothing more.
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution of CODEOWNERS")
	if ids := turnIDs(t, repository, stream, driftMasonAgent); len(ids) != 1 {
		t.Fatalf("drift mason turns %v", ids)
	}

	must(t, os.WriteFile(filepath.Join(w.Path, "CODEOWNERS"), []byte("/internal/ @upstream @feature\n"), 0600))
	completeDriftTurn(t, f, repository, stream, driftMasonAgent, resolvedDone("Kept both owners"))
	awaitResolution(t, f.s, d, stream, op, "review 1 of candidate")
	records := driftRecords(t, repository, stream)
	candidate := records[len(records)-1].Candidate
	resolved := stopped
	resolved.Outcome, resolved.Candidate, resolved.Review = driftResolved, candidate, 1
	if !reflect.DeepEqual(records[len(records)-1], resolved) || parentOf(t, f, candidate) != upstream {
		t.Fatalf("the resolved record %+v, want %+v on %s", records[len(records)-1], resolved, upstream)
	}
	if tip := featureTip(t, f, stream); tip != before {
		t.Fatalf("the feature branch moved to %s before review", tip)
	}
	review, err := repository.Thread(stream, driftReviewerAgent)
	must(t, err)
	if review.Identity.Role != reviewerRole || len(review.Turns) != 1 || review.Turns[0].Request.TurnID != driftReviewTurnID(1, 1) {
		t.Fatalf("the drift reviewer's thread %+v", review)
	}
	for _, want := range []string{candidate, "+/internal/ @upstream @feature", "- CODEOWNERS", "## Sealed spec: spec.md"} {
		if !strings.Contains(review.Turns[0].Request.Prompt, want) {
			t.Fatalf("the review turn lacks %q:\n%s", want, review.Turns[0].Request.Prompt)
		}
	}

	completeDriftTurn(t, f, repository, stream, driftReviewerAgent, driftVerdict(t, approvedResolution))
	result, err := attemptOperation(t, f.s, repository, stream, op, d)
	if err != nil || result.Outcome != "succeeded" {
		t.Fatalf("the approved drift rebase %+v %v", result, err)
	}
	if tip := featureTip(t, f, stream); tip != candidate || fileAt(t, f, tip, "CODEOWNERS") != "/internal/ @upstream @feature\n" {
		t.Fatalf("the feature branch is at %s, not the approved candidate %s", tip, candidate)
	}
	after := seals(t, repository, stream)
	if len(after) != 2 || after[1].Base.Commit != upstream {
		t.Fatalf("seals after the approved drift rebase %+v", after)
	}
	records = driftRecords(t, repository, stream)
	replayed := resolved
	replayed.Outcome, replayed.Commit, replayed.Verdict = driftReplayed, candidate, &approvedResolution
	rebased := replayed
	rebased.Outcome, rebased.Seal, rebased.SealRevision = driftRebased, 1, 2
	if !reflect.DeepEqual(records[len(records)-2:], []DriftRebase{replayed, rebased}) {
		t.Fatalf("drift/rebase.json ends %+v", records[len(records)-2:])
	}
	if done := transitionByID(t, repository, stream, "drift-1-rebased"); done.From != "conflicted-1" || done.To != "rebased-1" {
		t.Fatalf("the outcome %+v", done)
	}
	if _, found, err := workspaces(f.s.cfg, driftsDirectory, config.WorkspacesGit).Workspace(ctx, string(stream)); err != nil || found {
		t.Fatalf("the resolution workspace outlived the drift rebase: %t %v", found, err)
	}
	if seen, err := d.Inspect(ctx, op); err != nil || seen.State != coreadapter.EffectCompleted {
		t.Fatalf("inspection after the drift rebase %+v %v", seen, err)
	}
}

// A resolution the mason reports done with markers left gets one reminder
// that names the marked file. A review with material findings returns the
// resolution to the mason with the findings, the feature branch and the seal
// stay, and the mason's answer, snapshotted by the service, is the next
// candidate a reviewer reads. Only its approval moves the branch.
func TestRejectedDriftResolutionReturnsToTheMasonAndKeepsTheBranch(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := newFinalFixture(t, "drift-reject")
	d := drifter{&foreman{masons: newMasonController(f.s, repository)}}
	before, upstream, op := conflictedDrift(t, f, d, stream)
	sealed := streamDocuments(t, repository, stream, seal.DocumentID)
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution")
	w := resolutionWorkspace(t, f, stream)

	done := completeDriftTurn(t, f, repository, stream, driftMasonAgent, resolvedDone("Resolved"))
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution")
	remind := fmt.Sprintf("%s-markers-%d", driftMasonAgent, done.Sequence)
	if ids := turnIDs(t, repository, stream, driftMasonAgent); !slices.Equal(ids, []string{driftResolveTurnID(1, 1), remind}) {
		t.Fatalf("drift mason turns after a done with markers %v", ids)
	}
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution")
	if ids := turnIDs(t, repository, stream, driftMasonAgent); len(ids) != 2 {
		t.Fatalf("the reminder was queued again: %v", ids)
	}

	must(t, os.WriteFile(filepath.Join(w.Path, "CODEOWNERS"), []byte("/internal/ @feature\n"), 0600))
	completeDriftTurn(t, f, repository, stream, driftMasonAgent, resolvedDone("Kept the feature owner"))
	awaitResolution(t, f.s, d, stream, op, "review 1")
	first := driftRecords(t, repository, stream)
	candidate := first[len(first)-1].Candidate
	completeDriftTurn(t, f, repository, stream, driftReviewerAgent, driftVerdict(t, rejectedResolution))
	awaitResolution(t, f.s, d, stream, op, "its mason's answer to review 1")
	records := driftRecords(t, repository, stream)
	rejected := records[len(records)-1]
	if rejected.Outcome != driftRejected || rejected.Review != 1 || rejected.Candidate != candidate || !reflect.DeepEqual(rejected.Verdict, &rejectedResolution) {
		t.Fatalf("the rejected record %+v", rejected)
	}
	if tip := featureTip(t, f, stream); tip != before {
		t.Fatalf("a rejected resolution moved the feature branch to %s", tip)
	}
	if after := streamDocuments(t, repository, stream, seal.DocumentID); !reflect.DeepEqual(after, sealed) {
		t.Fatalf("a rejected resolution changed the seal: %+v", after)
	}
	th, err := repository.Thread(stream, driftMasonAgent)
	must(t, err)
	fix := th.Turns[len(th.Turns)-1]
	if fix.Request.TurnID != driftFixTurnID(1, 1) || !strings.Contains(fix.Request.Prompt, "Keep @upstream beside @feature.") || !strings.Contains(fix.Request.Prompt, "## Sealed spec: spec.md") {
		t.Fatalf("the mason's turn after the rejection %s:\n%s", fix.Request.TurnID, fix.Request.Prompt)
	}

	must(t, os.WriteFile(filepath.Join(w.Path, "CODEOWNERS"), []byte("/internal/ @upstream @feature\n"), 0600))
	completeDriftTurn(t, f, repository, stream, driftMasonAgent, resolvedDone("Kept both owners"))
	awaitResolution(t, f.s, d, stream, op, "review 2")
	records = driftRecords(t, repository, stream)
	second := records[len(records)-1]
	if second.Outcome != driftResolved || second.Review != 2 || second.Candidate == candidate || second.Verdict != nil || fileAt(t, f, second.Candidate, "CODEOWNERS") != "/internal/ @upstream @feature\n" {
		t.Fatalf("the second candidate %+v", second)
	}
	if ids := turnIDs(t, repository, stream, driftReviewerAgent); !slices.Equal(ids, []string{driftReviewTurnID(1, 1), driftReviewTurnID(1, 2)}) {
		t.Fatalf("drift reviewer turns %v", ids)
	}
	if tip := featureTip(t, f, stream); tip != before {
		t.Fatalf("the feature branch moved to %s before the second review", tip)
	}

	completeDriftTurn(t, f, repository, stream, driftReviewerAgent, driftVerdict(t, approvedResolution))
	if result, err := attemptOperation(t, f.s, repository, stream, op, d); err != nil || result.Outcome != "succeeded" {
		t.Fatalf("the approved drift rebase %+v %v", result, err)
	}
	if tip := featureTip(t, f, stream); tip != second.Candidate {
		t.Fatalf("the feature branch is at %s, not the approved candidate %s", tip, second.Candidate)
	}
	if after := seals(t, repository, stream); len(after) != 2 || after[1].Base.Commit != upstream {
		t.Fatalf("seals after approval %+v", after)
	}
}

// A restart while a conflict is resolved resumes the recorded workspace and
// turns: the replay is not started again, no turn is queued twice, a mason
// turn the stop interrupted has its view copied back and one continuation,
// and a stop between the service's own steps goes on from where it was.
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
	f.s.boundary = nil
	restart := func() {
		t.Helper()
		must(t, repository.Close())
		var err error
		repository, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
		must(t, err)
		d = drifter{&foreman{masons: newMasonController(f.s, repository)}}
	}
	restart()
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution")
	w := resolutionWorkspace(t, f, stream)
	marked, err := os.ReadFile(filepath.Join(w.Path, "CODEOWNERS"))
	must(t, err)
	records := len(driftRecords(t, repository, stream))

	restart()
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution")
	if ids := turnIDs(t, repository, stream, driftMasonAgent); !slices.Equal(ids, []string{driftResolveTurnID(1, 1)}) {
		t.Fatalf("drift mason turns after a restart %v", ids)
	}
	if n := len(driftRecords(t, repository, stream)); n != records {
		t.Fatalf("a restart recorded %d drift records, not %d", n, records)
	}
	if again, err := os.ReadFile(filepath.Join(w.Path, "CODEOWNERS")); err != nil || string(again) != string(marked) {
		t.Fatalf("a restart replayed the resolution workspace again: %q %v", again, err)
	}

	// The mason's turn runs on a preserved view when the service stops.
	turn := driftResolveTurnID(1, 1)
	paths, err := viewPaths(w.Path)
	must(t, err)
	viewRoot := filepath.Join(f.s.cfg.Root.String(), "views", string(f.project), string(stream), driftMasonAgent, turn)
	must(t, os.MkdirAll(viewRoot, 0700))
	view, err := (isolation.Views{Directory: viewRoot}).Create(ctx, coreadapter.Workspace{Directory: w.Path, Access: coreadapter.ReadWrite}, paths)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(viewRoot, "ready"), []byte(filepath.Base(view.Workspace().Directory)), 0600))
	must(t, os.WriteFile(filepath.Join(view.Workspace().Directory, "CODEOWNERS"), []byte("/internal/ @upstream @feature\n"), 0600))
	_, err = repository.ClaimTurn(ctx, stream, driftMasonAgent, "crashed", filepath.Join(f.s.cfg.Root.String(), "threads", string(f.project), string(stream), driftMasonAgent, turn), f.clock.Now())
	must(t, err)
	restart()
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution")
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution")
	recover := fmt.Sprintf("%s-recover-1", driftMasonAgent)
	if ids := turnIDs(t, repository, stream, driftMasonAgent); !slices.Equal(ids, []string{turn, recover}) {
		t.Fatalf("drift mason turns after an interrupted turn %v", ids)
	}
	if data, err := os.ReadFile(filepath.Join(w.Path, "CODEOWNERS")); err != nil || string(data) != "/internal/ @upstream @feature\n" {
		t.Fatalf("the interrupted turn's view was not copied back: %q %v", data, err)
	}

	// The service stops after staging the resolution and after the approval.
	completeDriftTurn(t, f, repository, stream, driftMasonAgent, resolvedDone("Kept both owners"))
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
		f.s.boundary = nil
		restart()
	}
	awaitResolution(t, f.s, d, stream, op, "review 1")
	completeDriftTurn(t, f, repository, stream, driftReviewerAgent, driftVerdict(t, approvedResolution))
	f.s.boundary = func(name string) error {
		if name == "drift-approved" {
			return errors.New("the service stopped")
		}
		return nil
	}
	if _, err := attemptOperation(t, f.s, repository, stream, op, d); err == nil || !strings.Contains(err.Error(), "the service stopped") {
		t.Fatalf("the drift rebase did not stop after its approval: %v", err)
	}
	f.s.boundary = nil
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
	if ids := turnIDs(t, repository, stream, driftReviewerAgent); len(ids) != 1 {
		t.Fatalf("drift reviewer turns %v", ids)
	}
	if _, found, err := workspaces(f.s.cfg, driftsDirectory, config.WorkspacesGit).Workspace(ctx, string(stream)); err != nil || found {
		t.Fatalf("the resolution workspace outlived the drift rebase: %t %v", found, err)
	}
	var outcomes []string
	for _, r := range records2 {
		outcomes = append(outcomes, r.Outcome)
	}
	if want := []string{driftConflicted, driftConflicted, driftResolved, driftReplayed, driftRebased}; !slices.Equal(outcomes, want) {
		t.Fatalf("drift records %q, want %q", outcomes, want)
	}
	repository.Close()
}

// While a feature branch conflict of a drift rebase is resolved, the drift
// operation holds the project's lander: no landing is asked for, however
// many passes run, and no other drift rebase either.
func TestLandingWaitsWhileADriftConflictIsOpen(t *testing.T) {
	t.Parallel()
	f, stream, repository := newApprovedFixture(t, "drift-holds-landing")
	defer func() { repository.Close() }()
	lands := &foreman{masons: newMasonController(f.s, repository)}
	d := drifter{lands}
	ctx := context.Background()
	must(t, lands.Pass(ctx))
	landings := landOperations(t, repository, stream)
	if len(landings) != 1 {
		t.Fatalf("landing operations %+v", landings)
	}
	first, err := decodeLand(landings[0].Operation)
	must(t, err)
	settleOperation(t, f.s, repository, stream, landings[0].Operation, lands)
	other, criterion := "dedupe", "spec#2"
	if first.Unit == "dedupe" {
		other, criterion = "resume", "spec#1"
	}
	must(t, lands.Pass(ctx))
	rebases := rebaseOperations(t, repository, stream, other)
	if len(rebases) != 1 {
		t.Fatalf("%s's rebases %+v", other, rebases)
	}
	settleOperation(t, f.s, repository, stream, rebases[0].Operation, rebaser{lands})
	approveDirectly(t, f.s, repository, stream, other, criterion)

	advanceUpstream(t, f, map[string]string{masonWrote: "package trace\n\n// upstream\n"})
	op := requestDrift(t, d, stream)
	attemptOperation(t, f.s, repository, stream, op, d)
	for range 3 {
		must(t, lands.Pass(ctx))
	}
	if ops := landOperations(t, repository, stream); len(ops) != 1 {
		t.Fatalf("a landing was asked for while a drift conflict is open: %+v", ops)
	}
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution of "+masonWrote)
	if state, err := repository.Workflow(stream, trace.UnitSubject(other)); err != nil || state.Value != UnitApproved {
		t.Fatalf("unit %s is %+v %v", other, state, err)
	}
	if requested, err := d.requestDrifts(ctx, testDrift); err != nil || len(requested) != 0 {
		t.Fatalf("drift rebases asked for while a drift conflict is open: %v %v", requested, err)
	}
}

// A drift mason turn is lent its workstream's resolution workspace, sees all
// of it but its VCS metadata, holds file tools, amend and done alone, and has
// its view copied back. Its done takes an outcome.
func TestDriftMasonTurnWorksInTheResolutionWorkspace(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := newFinalFixture(t, "drift-workspace")
	d := drifter{&foreman{masons: newMasonController(f.s, repository)}}
	ctx := context.Background()
	_, _, op := conflictedDrift(t, f, d, stream)
	awaitResolution(t, f.s, d, stream, op, "its mason's resolution")
	w := resolutionWorkspace(t, f, stream)
	drifts := resolutions{driftWorkspaces(f.s.cfg, repository)}
	scope := coreadapter.Scope{Project: string(f.project), Workstream: string(stream), Thread: driftMasonAgent, Turn: driftResolveTurnID(1, 1), Role: masonRole}

	lease, err := threadWorkspaces{units: newUnitWorkspaces(f.s.cfg, repository), drifts: drifts}.Acquire(ctx, coreadapter.WorkspaceRequest{Scope: scope, Access: coreadapter.ReadWrite})
	must(t, err)
	if lease.Workspace.Directory != w.Path {
		t.Fatalf("the drift mason turn is lent %s, not %s", lease.Workspace.Directory, w.Path)
	}
	if _, err := drifts.Acquire(ctx, coreadapter.WorkspaceRequest{Scope: coreadapter.Scope{Project: scope.Project, Workstream: scope.Workstream, Thread: masonAgent("resume"), Unit: "resume", Role: masonRole}}); err == nil {
		t.Fatal("a unit's mason turn was lent the resolution workspace")
	}
	selection, err := drifts.selection(ctx, scope, coreadapter.ExecutionSettings{})
	must(t, err)
	if slices.Contains(selection.Paths, ".git") || !slices.Contains(selection.Paths, "CODEOWNERS") {
		t.Fatalf("the drift mason's view selects %v", selection.Paths)
	}
	if n := selection.Narrow; n == nil || !slices.Equal(n.Tools, []string{"file_read", "file_write", questions.AmendTool, doneTool}) || !n.WriteFiles || !n.Execute {
		t.Fatalf("the drift mason's capabilities %+v", n)
	}

	viewRoot := filepath.Join(t.TempDir(), "views")
	must(t, os.MkdirAll(viewRoot, 0700))
	view, err := (isolation.Views{Directory: viewRoot}).Create(ctx, lease.Workspace, selection.Paths)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(view.Workspace().Directory, "CODEOWNERS"), []byte("resolved\n"), 0600))
	must(t, drifts.capture(ctx, scope, view))
	if data, err := os.ReadFile(filepath.Join(w.Path, "CODEOWNERS")); err != nil || string(data) != "resolved\n" {
		t.Fatalf("the view was not copied back: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(w.Path, ".git")); err != nil {
		t.Fatalf("the capture lost the workspace's VCS metadata: %v", err)
	}

	reports := &masonReports{}
	tool := reports.driftTool(scope)
	for _, raw := range []string{`{"outcome":" "}`, `{"outcome":"Kept both owners"}`, `{"outcome":"again"}`} {
		out, err := tool.Handle(ctx, json.RawMessage(raw))
		must(t, err)
		recorded := raw == `{"outcome":"Kept both owners"}`
		if strings.Contains(string(out), `"recorded":true`) != recorded {
			t.Fatalf("done %s returned %s", raw, out)
		}
	}
	if got := reports.accepted[turnKey(scope)]; got.Report.Outcome != "Kept both owners" || got.Card != (coreadapter.Card{}) {
		t.Fatalf("the accepted report %+v", got)
	}
}
