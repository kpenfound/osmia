package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// chiefMoveTool returns the move_workstream tool of a claimed chief-of-staff
// turn of the workstream. The turn answers the owner when owner is set.
func chiefMoveTool(t *testing.T, f *shedFixture, repo *trace.Repository, stream config.WorkstreamID, turn string, owner bool) coreadapter.Tool {
	t.Helper()
	ctx := context.Background()
	_, err := repo.EnsureChiefOfStaff(ctx, stream, demoStart, serviceActor)
	must(t, err)
	actor, cause := serviceActor, "fixture"
	if owner {
		actor, cause = ownerActor, "owner-message"
	}
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: f.project, Workstream: stream, At: demoStart, Actor: actor, Cause: cause},
		AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: turn, Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "chief", Prompt: "The final review failed."})
	must(t, err)
	_, err = repo.ClaimTurn(ctx, stream, trace.ChiefOfStaff, "token-"+turn, filepath.Join(t.TempDir(), turn), demoStart)
	must(t, err)
	controls := &runtimeControls{}
	controls.service.Store(f.s)
	return controls.moveWorkstream(repo, coreadapter.Scope{Project: string(f.project), Workstream: string(stream), Role: trace.ChiefOfStaff, Thread: trace.ChiefOfStaff, Turn: turn})
}

func callMove(t *testing.T, tool coreadapter.Tool, input string) string {
	t.Helper()
	out, err := tool.Handle(context.Background(), json.RawMessage(input))
	must(t, err)
	return string(out)
}

// failFinalReview runs final review k as a reader turn that records no
// report, and returns the failed report.
func failFinalReview(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, a *finalReviewer, k int, op coreadapter.Operation) FinalReport {
	t.Helper()
	f.finalTurn(k, 1, func(context.Context, *mcp.ClientSession) error { return nil })
	result := settleOperation(t, f.s, repository, stream, op, a)
	report, _, err := latestFinalReport(repository, stream)
	must(t, err)
	if result.Outcome != "failed" || report.Outcome != finalFailed || report.Review != k {
		t.Fatalf("final review %d: %+v %+v", k, result, report)
	}
	return report
}

// A final review that failed on a branch that has not changed is not asked
// for again on its own. The owner's move to assembled asks for a fresh one,
// whose reader receives the move's note; its report authorises delivery.
func TestMoveToAssembledRerunsAFailedFinalReview(t *testing.T) {
	t.Parallel()
	f, stream, repository, a := newFinalFixture(t, "move-rerun")
	f.s.setSole(&activeProject{repository: repository})
	ctx := context.Background()
	_, op := assembleBoth(t, f, repository, a, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	failFinalReview(t, f, repository, stream, a, 1, op)
	must(t, a.Pass(ctx))
	must(t, a.Pass(ctx))
	if ops := finalOperations(t, repository, stream); len(ops) != 1 {
		t.Fatalf("a failed review on an unchanged branch was asked for again: %+v", ops)
	}
	if _, reason, err := a.finalGate(ctx, stream); err != nil || !strings.HasPrefix(reason, "final review 1 failed") {
		t.Fatalf("the gate after the failure: %q %v", reason, err)
	}

	note := "The reader stopped without a report; read the branch again."
	out, api := f.s.ownerMoveWorkstream(ctx, string(stream), WorkstreamMoveRequest{To: AssembledState, Note: note})
	if api != nil {
		t.Fatal(api.Message)
	}
	if out.State != AssembledState || out.Move == nil || out.Move.Move != 1 || out.Move.From != AssembledState || out.Move.By != "owner" || !strings.Contains(out.Detail, note) {
		t.Fatalf("the move %+v", out)
	}
	if body := noticeOf(t, repository, stream, workstreamMoveDocument(1)+"-recorded"); !strings.Contains(body, "the owner asked for a fresh final review of the assembled branch: "+note) {
		t.Fatalf("the chief of staff heard %q", body)
	}
	must(t, a.Pass(ctx))
	ops := finalOperations(t, repository, stream)
	if len(ops) != 2 {
		t.Fatalf("the move asked for no fresh review: %+v", ops)
	}
	in, err := decodeFinalReview(ops[1].Operation)
	must(t, err)
	if in.Review != 2 || in.Move != 1 {
		t.Fatalf("the fresh review's input %+v", in)
	}
	f.finalTurn(2, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Ready", "findings": []any{}})
		return err
	})
	if result := settleOperation(t, f.s, repository, stream, ops[1].Operation, a); result.Outcome != "succeeded" {
		t.Fatalf("the fresh review %+v", result)
	}
	reader, err := repository.Thread(stream, committeeAgent(1))
	must(t, err)
	if prompt := reader.Turns[len(reader.Turns)-1].Request.Prompt; !strings.Contains(prompt, "The owner asked for this review with this note: "+note) {
		t.Fatalf("the reader was not told of the move:\n%s", prompt)
	}
	if report, reason, err := a.finalGate(ctx, stream); err != nil || reason != "" || report.Move != 1 {
		t.Fatalf("the gate after the fresh review: %+v %q %v", report, reason, err)
	}
	must(t, a.Pass(ctx))
	if ops := finalOperations(t, repository, stream); len(ops) != 2 {
		t.Fatalf("a current review was asked for again: %+v", ops)
	}
}

// The chief of staff moves its workstream to assembled or building on its own
// judgment. A move to building holds the workstream there until a revision
// is filed or it is moved to assembled. Every other target is the owner's,
// which the chief records only in a turn answering the owner, and after three
// final reviews with blocking findings reopening is the owner's too.
func TestChiefOfStaffMovesAWorkstreamAndRelaysOwnerGates(t *testing.T) {
	t.Parallel()
	f, stream, repository, a := newFinalFixture(t, "chief-move")
	f.s.setSole(&activeProject{repository: repository})
	ctx := context.Background()
	_, op := assembleBoth(t, f, repository, a, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	failFinalReview(t, f, repository, stream, a, 1, op)
	tool := chiefMoveTool(t, f, repository, stream, "events_1", false)

	for _, to := range []string{SketchedState, InShedState, RatifiedState, DeliveredState, AbandonedState} {
		if out := callMove(t, tool, fmt.Sprintf(`{"to":%q,"note":"Do it."}`, to)); !strings.Contains(out, "is the owner's decision") {
			t.Fatalf("the chief of staff moved the workstream to %s on its own: %s", to, out)
		}
	}
	if out := callMove(t, tool, `{"to":"abandoned","note":"Stop.","owner_decided":true}`); !strings.Contains(out, "only for a move the owner asked for") {
		t.Fatalf("an event turn recorded an owner move: %s", out)
	}
	if out := callMove(t, tool, `{"to":"building","note":"Reopen it so a revision can repair the gap."}`); !strings.Contains(out, `"recorded":true`) || !strings.Contains(out, `"state":"building"`) {
		t.Fatalf("move_workstream to building: %s", out)
	}
	must(t, a.Pass(ctx))
	must(t, a.Pass(ctx))
	if feature, err := repository.Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != BuildingState {
		t.Fatalf("the reopened workstream reassembled before any revision: %+v %v", feature, err)
	}
	if out := callMove(t, tool, `{"to":"building","note":"Again."}`); !strings.Contains(out, "already building") {
		t.Fatalf("a second move to building: %s", out)
	}
	if out := callMove(t, tool, `{"to":"assembled","note":"Nothing needs repair after all; review it again."}`); !strings.Contains(out, `"recorded":true`) {
		t.Fatalf("move_workstream to assembled: %s", out)
	}
	must(t, a.Pass(ctx))
	ops := finalOperations(t, repository, stream)
	if len(ops) != 2 {
		t.Fatalf("final review operations %+v", ops)
	}
	if in, err := decodeFinalReview(ops[1].Operation); err != nil || in.Move != 2 {
		t.Fatalf("the review asked for after the moves %+v %v", in, err)
	}
	actions, err := chiefActions(repository, stream)
	must(t, err)
	var texts []string
	for _, action := range actions {
		texts = append(texts, action.Text)
	}
	if !slices.Contains(texts, "Moved the workstream from assembled to building. Reopen it so a revision can repair the gap.") ||
		!slices.Contains(texts, "Moved the workstream from building to assembled. Nothing needs repair after all; review it again.") {
		t.Fatalf("conversation actions %q", texts)
	}

	report, _, err := latestFinalReport(repository, stream)
	must(t, err)
	for k := 2; k <= 4; k++ {
		report.Review, report.Outcome, report.Failure = k, finalReviewed, ""
		report.Findings = []FinalFinding{{Severity: FindingBlocking, Finding: "The feature does not resume."}}
		content, err := json.Marshal(report)
		must(t, err)
		revision, err := nextRevision(repository, stream, finalReportDocument)
		must(t, err)
		must(t, repository.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: finalReportDocument, Revision: revision, Project: repository.Project(), Workstream: stream, At: time.Now().UTC(), Actor: finalReviewActor, Cause: "final-review"}, Path: finalReportPath, Content: string(content)}}))
	}
	if out := callMove(t, tool, `{"to":"building","note":"Reopen it."}`); !strings.Contains(out, "reopening the workstream for further revisions is the owner's decision") {
		t.Fatalf("the chief of staff reopened after three blocking reviews: %s", out)
	}
	if out := callMove(t, tool, `{"to":"assembled","note":"Review it again."}`); !strings.Contains(out, `"recorded":true`) {
		t.Fatalf("a fresh review after three blocking reviews: %s", out)
	}
}

// In a turn answering the owner, the chief of staff records the owner's move
// to an owner gate as the owner's own action.
func TestChiefOfStaffRelaysTheOwnersAbandonment(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := newFinalFixture(t, "chief-relay")
	f.s.setSole(&activeProject{repository: repository})
	tool := chiefMoveTool(t, f, repository, stream, "message_1", true)
	if out := callMove(t, tool, `{"to":"abandoned","note":"Stop this one.","owner_decided":true}`); !strings.Contains(out, `"recorded":true`) || !strings.Contains(out, `"state":"abandoned"`) {
		t.Fatalf("the relayed abandonment: %s", out)
	}
	abandon := transitionByID(t, repository, stream, abandonTransition)
	if abandon.Actor != ownerActor || abandon.Reason != "Stop this one." {
		t.Fatalf("the abandonment %+v", abandon)
	}
}

// A delivered workstream whose pull request is open reopens on its existing
// branch. The repaired branch gets a fresh final review and the owner's
// delivery approval, and publication updates the same pull request.
func TestReopenedDeliveredPullRequestIsRepairedOnTheSamePullRequest(t *testing.T) {
	t.Parallel()
	p := newPublicationFixture(t, "squash")
	ctx := context.Background()
	p.approve(t, nil)
	if result := settleOperation(t, p.s, p.repository, p.stream, p.request(t), p.publisher()); result.Outcome != "succeeded" || p.feature(t) != DeliveredState {
		t.Fatalf("the first publication %+v", result)
	}
	p.pulls.SyncHead = true
	before, _ := p.forkBranch(t)

	out, api := p.s.ownerMoveWorkstream(ctx, string(p.stream), WorkstreamMoveRequest{To: BuildingState, Note: "Upstream broke resume; repair it on this pull request."})
	if api != nil {
		t.Fatal(api.Message)
	}
	if out.State != BuildingState || !strings.Contains(out.Detail, "pull request #100 stays open") {
		t.Fatalf("the reopen %+v", out)
	}
	reviewer := &finalReviewer{s: p.s, repository: p.repository}
	must(t, reviewer.Pass(ctx))
	if p.feature(t) != BuildingState {
		t.Fatalf("the reopened workstream is %s", p.feature(t))
	}
	if _, api := p.s.ownerMoveWorkstream(ctx, string(p.stream), WorkstreamMoveRequest{To: DeliveredState, Note: "Nothing to repair."}); api != nil {
		t.Fatalf("an unchanged reopened branch did not return to delivered: %s", api.Message)
	}
	if p.feature(t) != DeliveredState {
		t.Fatalf("the workstream is %s", p.feature(t))
	}

	if _, api := p.s.ownerMoveWorkstream(ctx, string(p.stream), WorkstreamMoveRequest{To: BuildingState, Note: "Repair it after all."}); api != nil {
		t.Fatal(api.Message)
	}
	moveFeature(t, p.shedFixture, p.stream, map[string]string{"resume.go": "package demo\n// Resume after the upstream change.\n"})
	if _, api := p.s.ownerMoveWorkstream(ctx, string(p.stream), WorkstreamMoveRequest{To: DeliveredState, Note: "Done."}); api == nil || api.Code != Conflict || !strings.Contains(api.Message, "since pull request #100 was published") {
		t.Fatalf("a changed branch returned to delivered without review: %v", api)
	}
	if _, api := p.s.ownerMoveWorkstream(ctx, string(p.stream), WorkstreamMoveRequest{To: AssembledState, Note: "The repair landed; review the branch."}); api != nil {
		t.Fatal(api.Message)
	}
	must(t, reviewer.Pass(ctx))
	op := actionOperation(t, p.repository, p.stream, FinalReviewAction)
	in, err := decodeFinalReview(op)
	must(t, err)
	if in.Maintenance || in.Move != 3 {
		t.Fatalf("the review of the repaired branch %+v", in)
	}
	p.finalTurn(in.Review, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Repaired", "findings": []any{}})
		return err
	})
	if result := settleOperation(t, p.s, p.repository, p.stream, op, reviewer); result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	if tip, _ := p.forkBranch(t); tip != before {
		t.Fatal("the pull request changed before the owner's approval")
	}
	description := "Resumable uploads\n\nRepaired after upstream moved."
	p.approve(t, &description)
	if result := settleOperation(t, p.s, p.repository, p.stream, p.request(t), p.publisher()); result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	tip, _ := p.forkBranch(t)
	if tip == before || p.pulls.Creates != 1 || p.pulls.Updates == 0 || p.pulls.Prs[0].Body != description || p.feature(t) != DeliveredState {
		t.Fatalf("the repaired delivery: tip %s, creates %d, updates %d, body %q, state %s", tip, p.pulls.Creates, p.pulls.Updates, p.pulls.Prs[0].Body, p.feature(t))
	}
}

// A workstream moves only where its state allows: not while its publication
// is in flight, and never reopening a pull request that is merged or closed.
func TestWorkstreamMoveRefusals(t *testing.T) {
	t.Parallel()
	p := newPublicationFixture(t, "commit-per-unit")
	ctx := context.Background()
	move := func(to, note string) *APIError {
		_, api := p.s.ownerMoveWorkstream(ctx, string(p.stream), WorkstreamMoveRequest{To: to, Note: note})
		return api
	}
	if api := move("merged", "x"); api == nil || api.Code != Validation {
		t.Fatalf("an unknown state: %v", api)
	}
	if api := move(BuildingState, " "); api == nil || api.Code != Validation {
		t.Fatalf("an empty note: %v", api)
	}
	if api := move(DeliveredState, "Deliver it."); api == nil || api.Code != Conflict || !strings.Contains(api.Message, "delivered through the owner's delivery approval") {
		t.Fatalf("an assembled workstream moved to delivered: %v", api)
	}
	p.approve(t, nil)
	publish := p.request(t)
	if api := move(BuildingState, "Reopen it."); api == nil || api.Code != Conflict || !strings.Contains(api.Message, "publication is in flight") {
		t.Fatalf("a move during publication: %v", api)
	}
	settleOperation(t, p.s, p.repository, p.stream, publish, p.publisher())
	if p.feature(t) != DeliveredState {
		t.Fatalf("the workstream is %s", p.feature(t))
	}
	p.pulls.Prs[0].State = "closed"
	fm := cadenceForeman(p.shedFixture, p.repository, "1m")
	if _, api := p.s.rebaseWorkstream(ctx, string(p.stream)); api != nil {
		t.Fatal(api.Message)
	}
	passDrifts(t, fm)
	settleOperation(t, p.s, p.repository, p.stream, driftOperations(t, p.repository, p.stream)[0].Operation, drifter{fm})
	if api := move(BuildingState, "Reopen it."); api == nil || api.Code != Conflict || !strings.Contains(api.Message, "is merged or closed") {
		t.Fatalf("a closed pull request reopened: %v", api)
	}
	if api := move(AssembledState, "Review it."); api == nil || api.Code != Conflict || !strings.Contains(api.Message, "is merged or closed") {
		t.Fatalf("a closed pull request returned to final review: %v", api)
	}
}

// failedDriftTurn ends the oldest unfinished turn of the workstream's drift
// agent as a failed turn.
func failedDriftTurn(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, agent string) {
	t.Helper()
	ctx := context.Background()
	th, err := repository.Thread(stream, agent)
	must(t, err)
	i := slices.IndexFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.CompletedAt.IsZero() })
	if i < 0 {
		t.Fatalf("thread %s has no turn to run", agent)
	}
	token := "failed-" + th.Turns[i].Request.TurnID
	q, err := repository.ClaimTurn(ctx, stream, agent, token, filepath.Join(t.TempDir(), token), f.clock.Now())
	must(t, err)
	h := q.Request.Header
	h.Schema, h.ID, h.At = "osmia.trace.turn-response", trace.EventID(q.Request.ID, "response"), f.clock.Now()
	response := trace.TurnResponse{Header: h, AgentID: agent, ThreadID: agent, TurnID: q.Request.TurnID, RequestID: q.Request.ID, RequestRevision: q.Request.Revision, Failure: "the backend crashed",
		Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: q.Request.Profile.Backend, ID: token}, SessionDirectory: q.Claim.SessionDirectory, StartedAt: q.Claim.At, IsError: true}}
	must(t, repository.CaptureTurn(ctx, token, response))
	must(t, repository.CompleteTurn(ctx, stream, agent, q.Request.TurnID, token, f.clock.Now()))
}

// A drift rebase whose mason's turn fails, or whose reviewer's turn ends
// without a verdict, is held for a handback instead of waiting forever.
func TestDriftResolutionStuckTurnsHoldTheDriftRebase(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, want string
		stick      func(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, d drifter, op coreadapter.Operation)
	}{
		{name: "mason", want: "its mason's turn " + driftResolveTurnID(1, 1) + " failed: the backend crashed",
			stick: func(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, _ drifter, _ coreadapter.Operation) {
				failedDriftTurn(t, f, repository, stream, driftMasonAgent)
			}},
		{name: "reviewer", want: "its reviewer's turn " + driftReviewTurnID(1, 1) + " ended without recording a verdict",
			stick: func(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, d drifter, op coreadapter.Operation) {
				w := resolutionWorkspace(t, f, stream)
				must(t, os.WriteFile(filepath.Join(w.Path, "CODEOWNERS"), []byte("/internal/ @upstream @feature\n"), 0600))
				completeDriftTurn(t, f, repository, stream, driftMasonAgent, resolvedDone("Resolved"))
				awaitResolution(t, f.s, d, stream, op, "review 1")
				completeDriftTurn(t, f, repository, stream, driftReviewerAgent, nil)
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, stream, repository, _ := newFinalFixture(t, "drift-stuck-"+tc.name)
			fm := cadenceForeman(f, repository, "1h")
			d := drifter{fm}
			before, _, op := conflictedDrift(t, f, d, stream)
			awaitResolution(t, f.s, d, stream, op, "its mason's resolution")
			tc.stick(t, f, repository, stream, d, op)
			result, err := attemptOperation(t, f.s, repository, stream, op, d)
			if err != nil || result.Outcome != "succeeded" || !strings.Contains(result.Evidence, "drift rebase 1 is held for the owner: "+tc.want) {
				t.Fatalf("the stuck drift rebase %+v %v", result, err)
			}
			if state, err := repository.Workflow(stream, driftSubject); err != nil || state.Value != driftHeld+"-1" {
				t.Fatalf("the drift subject is %+v: %v", state, err)
			}
			if tip := featureTip(t, f, stream); tip != before {
				t.Fatalf("a held drift rebase moved the feature branch to %s", tip)
			}
			if k, api := f.s.handBackDrift(context.Background(), repository, stream, "Keep both owners.", chiefActor, "", f.s.now()); api != nil || k != 2 {
				t.Fatalf("the handback of the stuck drift rebase %d %v", k, api)
			}
		})
	}
}

// Reviewers treat the changes a rebase needs as in scope: the final reader
// is told so, and that upstream's changes since the last review are not the
// feature's, and a unit reviewer is told which paths its candidate's rebase
// resolved.
func TestReviewersTreatRebaseChangesAsInScope(t *testing.T) {
	t.Parallel()
	if prompt := finalSystemPrompt(config.Project{Name: "dagger", Upstream: "dagger/dagger"}); !strings.Contains(prompt, "changes a rebase or a drift resolution needed so that both upstream's behaviour and the feature's stand are part of the feature branch") ||
		!strings.Contains(prompt, "upstream's own changes are not the feature's") {
		t.Fatalf("the final system prompt:\n%s", prompt)
	}
	previous := FinalReport{Review: 1, Commit: "abc", Outcome: finalReviewed}
	report := FinalReport{Review: 2, Commit: "def", Upstream: &seal.Base{Remote: "upstream", Branch: "main", Commit: "abc"}}
	if prompt := finalPrompt(report, FinalCheckRun{}, nil, nil, previous); !strings.Contains(prompt, "that is upstream's change, not the feature's") {
		t.Fatalf("the final prompt after a previous review:\n%s", prompt)
	}

	f, stream, repository := newReviewFixture(t, "rebase-scope")
	ctx := context.Background()
	if conflicts, err := unitRebaseConflicts(repository, stream, "resume", "tip"); err != nil || conflicts != nil {
		t.Fatalf("conflicts without a rebase %v %v", conflicts, err)
	}
	content, err := json.Marshal(UnitRebase{Unit: "resume", Rebase: 1, Onto: "tip", Conflicts: []string{"CODEOWNERS"}})
	must(t, err)
	must(t, repository.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: rebaseDocument("resume"), Revision: 1, Project: f.project, Workstream: stream, Unit: "resume", At: demoStart, Actor: foremanActor, Cause: "rebase"}, Path: "units/resume/rebase.json", Content: string(content)}}))
	if conflicts, err := unitRebaseConflicts(repository, stream, "resume", "tip"); err != nil || !slices.Equal(conflicts, []string{"CODEOWNERS"}) {
		t.Fatalf("conflicts of the rebase onto the candidate's base %v %v", conflicts, err)
	}
	if conflicts, err := unitRebaseConflicts(repository, stream, "resume", "other"); err != nil || conflicts != nil {
		t.Fatalf("conflicts of a rebase onto another base %v %v", conflicts, err)
	}
}
