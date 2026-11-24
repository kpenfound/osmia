package lifecycle_test

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

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func chiefMoveTool(t *testing.T, f *shedFixture, repo *trace.Repository, stream config.WorkstreamID, turn string, owner bool) coreadapter.Tool {
	t.Helper()
	actor, cause := intake.ServiceActor, "fixture"
	if owner {
		actor, cause = intake.OwnerActor, "owner-message"
	}
	scope := intakefixture.ClaimChiefTurn(t, repo, f.project, stream, func() time.Time { return fixturekit.DemoStart }, intakefixture.ChiefTurnInput{
		Turn: turn, Prompt: "The final review failed.", Actor: actor, Cause: cause, Token: "token-" + turn, Directory: turn,
	})
	controls := &runtimeControls{}
	controls.service.Store(f.s)
	return controls.moveWorkstream(repo, scope)
}
func callMove(t *testing.T, tool coreadapter.Tool, input string) string {
	t.Helper()
	out, err := tool.Handle(context.Background(), json.RawMessage(input))
	intakefixture.Must(t, err)
	return string(out)
}

func failFinalReview(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, a *finalReviewer, k int, op coreadapter.Operation) api.FinalReport {
	t.Helper()
	f.finalTurn(k, 1, func(context.Context, *mcp.ClientSession) error { return nil })
	result := settleOperation(t, f.s, repository, stream, op, a)
	report, _, err := delivery.LatestFinalReport(repository, stream)
	intakefixture.Must(t, err)
	if result.Outcome != "failed" || report.Outcome != delivery.FinalFailed || report.Review != k {
		t.Fatalf("final review %d: %+v %+v", k, result, report)
	}
	return report
}

func TestMoveToAssembledRerunsAFailedFinalReview(t *testing.T) {
	t.Parallel()
	f, stream, repository, a := newFinalFixture(t, "move-rerun")
	hostFinalReviews(t, f, stream)
	f.s.setSole(&activeProject{repository: repository})
	ctx := context.Background()
	_, op := assembleBoth(t, f, repository, a, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	failFinalReview(t, f, repository, stream, a, 1, op)
	intakefixture.Must(t, a.Pass(ctx))
	intakefixture.Must(t, a.Pass(ctx))
	if ops := finalOperations(t, repository, stream); len(ops) != 1 {
		t.Fatalf("a failed review on an unchanged branch was asked for again: %+v", ops)
	}
	if _, reason, err := a.finalGate(ctx, stream); err != nil || !strings.HasPrefix(reason, "final review 1 failed") {
		t.Fatalf("the gate after the failure: %q %v", reason, err)
	}

	note := "The reader stopped without a report; read the branch again."
	out, apiFault := f.s.ownerMoveWorkstream(ctx, string(stream), api.WorkstreamMoveRequest{To: intake.AssembledState, Note: note})
	if apiFault != nil {
		t.Fatal(apiFault.Message)
	}
	if out.State != intake.AssembledState || out.Move == nil || out.Move.Move != 1 || out.Move.From != intake.AssembledState || out.Move.By != "owner" || !strings.Contains(out.Detail, note) {
		t.Fatalf("the move %+v", out)
	}
	if body := intakefixture.NoticeOf(t, repository, stream, lifecycle.WorkstreamMoveDocument(1)+"-recorded"); !strings.Contains(body, "the owner asked for a fresh final review of the assembled branch: "+note) {
		t.Fatalf("the chief of staff heard %q", body)
	}
	intakefixture.Must(t, a.Pass(ctx))
	ops := finalOperations(t, repository, stream)
	if len(ops) != 2 {
		t.Fatalf("the move asked for no fresh review: %+v", ops)
	}
	in, err := delivery.DecodeFinalReview(ops[1].Operation)
	intakefixture.Must(t, err)
	if in.Review != 2 || in.Move != 1 {
		t.Fatalf("the fresh review's input %+v", in)
	}
	f.finalTurn(2, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := intakefixture.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Ready", "findings": []any{}})
		return err
	})
	if result := settleOperation(t, f.s, repository, stream, ops[1].Operation, a); result.Outcome != "succeeded" {
		t.Fatalf("the fresh review %+v", result)
	}
	reader, err := repository.Thread(stream, intake.CommitteeAgent(1))
	intakefixture.Must(t, err)
	if prompt := reader.Turns[len(reader.Turns)-1].Request.Prompt; !strings.Contains(prompt, "The owner asked for this review with this note: "+note) {
		t.Fatalf("the reader was not told of the move:\n%s", prompt)
	}
	if report, reason, err := a.finalGate(ctx, stream); err != nil || reason != "" || report.Move != 1 {
		t.Fatalf("the gate after the fresh review: %+v %q %v", report, reason, err)
	}
	intakefixture.Must(t, a.Pass(ctx))
	if ops := finalOperations(t, repository, stream); len(ops) != 2 {
		t.Fatalf("a current review was asked for again: %+v", ops)
	}
}

func TestChiefOfStaffMovesAWorkstreamAndRelaysOwnerGates(t *testing.T) {
	t.Parallel()
	f, stream, repository, a := newFinalFixture(t, "chief-move")
	hostFinalReviews(t, f, stream)
	f.s.setSole(&activeProject{repository: repository})
	ctx := context.Background()
	_, op := assembleBoth(t, f, repository, a, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	failFinalReview(t, f, repository, stream, a, 1, op)
	tool := chiefMoveTool(t, f, repository, stream, "events_1", false)

	for _, to := range []string{intake.SketchedState, intake.InShedState, intake.RatifiedState, intake.AbandonedState} {
		out := callMove(t, tool, fmt.Sprintf(`{"to":%q,"note":"Do it."}`, to))
		if !strings.Contains(out, `"recorded":false`) || strings.Contains(out, "owner_decided") || strings.Contains(out, "present it to the owner") {
			t.Fatalf("the chief of staff moved the workstream to %s on its own: %s", to, out)
		}
		if (to == intake.RatifiedState || to == intake.AbandonedState) && !strings.Contains(out, "pending as an owner decision") {
			t.Fatalf("the proposal to %s does not point to its card: %s", to, out)
		}
	}
	if out := callMove(t, tool, `{"to":"abandoned","note":"Stop.","owner_decided":true}`); !strings.Contains(out, "only for a move the owner asked for") {
		t.Fatalf("an event turn recorded an owner move: %s", out)
	}
	if out := callMove(t, tool, `{"to":"building","note":"Reopen it so a revision can repair the gap."}`); !strings.Contains(out, `"recorded":true`) || !strings.Contains(out, `"state":"building"`) {
		t.Fatalf("move_workstream to building: %s", out)
	}
	intakefixture.Must(t, a.Pass(ctx))
	intakefixture.Must(t, a.Pass(ctx))
	if feature, err := repository.Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != intakefixture.BuildingState {
		t.Fatalf("the reopened workstream reassembled before any revision: %+v %v", feature, err)
	}
	if out := callMove(t, tool, `{"to":"building","note":"Again."}`); !strings.Contains(out, "already building") {
		t.Fatalf("a second move to building: %s", out)
	}
	if out := callMove(t, tool, `{"to":"assembled","note":"Nothing needs repair after all; review it again."}`); !strings.Contains(out, `"recorded":true`) {
		t.Fatalf("move_workstream to assembled: %s", out)
	}
	intakefixture.Must(t, a.Pass(ctx))
	ops := finalOperations(t, repository, stream)
	if len(ops) != 2 {
		t.Fatalf("final review operations %+v", ops)
	}
	if in, err := delivery.DecodeFinalReview(ops[1].Operation); err != nil || in.Move != 2 {
		t.Fatalf("the review asked for after the moves %+v %v", in, err)
	}
	actions, err := service.TestChiefActions(repository, stream)
	intakefixture.Must(t, err)
	var texts []string
	for _, action := range actions {
		texts = append(texts, action.Text)
	}
	if !slices.Contains(texts, "Moved the workstream from assembled to building. Reopen it so a revision can repair the gap.") ||
		!slices.Contains(texts, "Moved the workstream from building to assembled. Nothing needs repair after all; review it again.") {
		t.Fatalf("conversation actions %q", texts)
	}

	report, _, err := delivery.LatestFinalReport(repository, stream)
	intakefixture.Must(t, err)
	for k := 2; k <= 4; k++ {
		report.Review, report.Outcome, report.Failure = k, delivery.FinalReviewed, ""
		report.Findings = []api.FinalFinding{{Severity: delivery.FindingBlocking, Finding: "The feature does not resume."}}
		content, err := json.Marshal(report)
		intakefixture.Must(t, err)
		revision, err := intake.NextRevision(repository, stream, delivery.FinalReportDocument)
		intakefixture.Must(t, err)
		intakefixture.Must(t, repository.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: delivery.FinalReportDocument, Revision: revision, Project: repository.Project(), Workstream: stream, At: time.Now().UTC(), Actor: delivery.FinalReviewActor, Cause: "final-review"}, Path: delivery.FinalReportPath, Content: string(content)}}))
	}
	if out := callMove(t, tool, `{"to":"assembled","note":"Review it again."}`); !strings.Contains(out, `"recorded":true`) {
		t.Fatalf("a fresh review after three blocking reviews: %s", out)
	}
}

func TestChiefOfStaffRelaysTheOwnersAbandonment(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := newFinalFixture(t, "chief-relay")
	f.s.setSole(&activeProject{repository: repository})
	tool := chiefMoveTool(t, f, repository, stream, "message_1", true)
	if out := callMove(t, tool, `{"to":"abandoned","note":"Stop this one.","owner_decided":true}`); !strings.Contains(out, `"recorded":true`) || !strings.Contains(out, `"state":"abandoned"`) {
		t.Fatalf("the relayed abandonment: %s", out)
	}
	abandon := intakefixture.TransitionByID(t, repository, stream, intake.AbandonTransition)
	if abandon.Actor != intake.OwnerActor || abandon.Reason != "Stop this one." {
		t.Fatalf("the abandonment %+v", abandon)
	}
}

func TestReopenedDeliveredPullRequestIsRepairedOnTheSamePullRequest(t *testing.T) {
	t.Parallel()
	p := newPublicationFixture(t, "squash")
	ctx := context.Background()
	p.approve(t, nil)
	if result := settleOperation(t, p.s, p.repository, p.stream, p.request(t), p.publisher()); result.Outcome != "succeeded" || p.feature(t) != intake.DeliveredState {
		t.Fatalf("the first publication %+v", result)
	}
	p.pulls.SyncHead = true
	before, _ := p.forkBranch(t)

	out, apiFault := p.s.ownerMoveWorkstream(ctx, string(p.stream), api.WorkstreamMoveRequest{To: intakefixture.BuildingState, Note: "Upstream broke resume; repair it on this pull request."})
	if apiFault != nil {
		t.Fatal(apiFault.Message)
	}
	if out.State != intakefixture.BuildingState || !strings.Contains(out.Detail, "pull request #100 stays open") {
		t.Fatalf("the reopen %+v", out)
	}
	reviewer := &finalReviewer{s: p.s, repository: p.repository}
	intakefixture.Must(t, reviewer.Pass(ctx))
	if p.feature(t) != intakefixture.BuildingState {
		t.Fatalf("the reopened workstream is %s", p.feature(t))
	}
	if _, apiFault := p.s.ownerMoveWorkstream(ctx, string(p.stream), api.WorkstreamMoveRequest{To: intake.DeliveredState, Note: "Nothing to repair."}); apiFault != nil {
		t.Fatalf("an unchanged reopened branch did not return to delivered: %s", apiFault.Message)
	}
	if p.feature(t) != intake.DeliveredState {
		t.Fatalf("the workstream is %s", p.feature(t))
	}

	if _, apiFault := p.s.ownerMoveWorkstream(ctx, string(p.stream), api.WorkstreamMoveRequest{To: intakefixture.BuildingState, Note: "Repair it after all."}); apiFault != nil {
		t.Fatal(apiFault.Message)
	}
	moveFeature(t, p.shedFixture, p.stream, map[string]string{"resume.go": "package demo\n// Resume after the upstream change.\n"})
	if _, apiFault := p.s.ownerMoveWorkstream(ctx, string(p.stream), api.WorkstreamMoveRequest{To: intake.DeliveredState, Note: "Done."}); apiFault == nil || apiFault.Code != api.Conflict || !strings.Contains(apiFault.Message, "since pull request #100 was published") {
		t.Fatalf("a changed branch returned to delivered without review: %v", apiFault)
	}
	if _, apiFault := p.s.ownerMoveWorkstream(ctx, string(p.stream), api.WorkstreamMoveRequest{To: intake.AssembledState, Note: "The repair landed; review the branch."}); apiFault != nil {
		t.Fatal(apiFault.Message)
	}
	intakefixture.Must(t, reviewer.Pass(ctx))
	op := fixturekit.ActionOperation(t, p.repository, p.stream, delivery.FinalReviewAction)
	in, err := delivery.DecodeFinalReview(op)
	intakefixture.Must(t, err)
	if in.Maintenance || in.Move != 3 {
		t.Fatalf("the review of the repaired branch %+v", in)
	}
	p.finalTurn(in.Review, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := intakefixture.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Repaired", "findings": []any{}})
		return err
	})
	if result := settleOperation(t, p.s, p.repository, p.stream, op, reviewer); result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	pushed, _ := p.forkBranch(t)
	if pushed == before || p.pulls.Creates != 1 || p.pulls.Prs[0].HeadCommit != pushed {
		t.Fatalf("the repaired branch was not pushed to the same pull request: tip %s, was %s, creates %d, pull request %+v", pushed, before, p.pulls.Creates, p.pulls.Prs[0])
	}
	description := "Resumable uploads\n\nRepaired after upstream moved."
	p.approve(t, &description)
	if result := settleOperation(t, p.s, p.repository, p.stream, p.request(t), p.publisher()); result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	if p.pulls.Creates != 1 || p.pulls.Updates == 0 || p.pulls.Prs[0].Body != description || p.feature(t) != intake.DeliveredState {
		t.Fatalf("the repaired delivery: creates %d, updates %d, body %q, state %s", p.pulls.Creates, p.pulls.Updates, p.pulls.Prs[0].Body, p.feature(t))
	}
}

func TestWorkstreamMoveRefusals(t *testing.T) {
	t.Parallel()
	p := newUnpushedPublicationFixture(t, "commit-per-unit", "")
	ctx := context.Background()
	move := func(to, note string) *api.APIError {
		_, apiFault := p.s.ownerMoveWorkstream(ctx, string(p.stream), api.WorkstreamMoveRequest{To: to, Note: note})
		return apiFault
	}
	if apiFault := move("merged", "x"); apiFault == nil || apiFault.Code != api.Validation {
		t.Fatalf("an unknown state: %v", apiFault)
	}
	if apiFault := move(intakefixture.BuildingState, " "); apiFault == nil || apiFault.Code != api.Validation {
		t.Fatalf("an empty note: %v", apiFault)
	}
	if apiFault := move(intake.DeliveredState, "Deliver it."); apiFault == nil || apiFault.Code != api.Conflict || !strings.Contains(apiFault.Message, "delivered through the owner's delivery approval") {
		t.Fatalf("an assembled workstream moved to delivered: %v", apiFault)
	}
	p.publishDraft(t)
	p.approve(t, nil)
	publish := p.request(t)
	if apiFault := move(intakefixture.BuildingState, "Reopen it."); apiFault == nil || apiFault.Code != api.Conflict || !strings.Contains(apiFault.Message, "publication is in flight") {
		t.Fatalf("a move during publication: %v", apiFault)
	}
	settleOperation(t, p.s, p.repository, p.stream, publish, p.publisher())
	if p.feature(t) != intake.DeliveredState {
		t.Fatalf("the workstream is %s", p.feature(t))
	}
	p.pulls.Prs[0].State = "closed"
	fm := cadenceForeman(p.shedFixture, p.repository, "1m")
	if _, apiFault := p.s.rebaseWorkstream(ctx, string(p.stream)); apiFault != nil {
		t.Fatal(apiFault.Message)
	}
	passDrifts(t, fm)
	settleOperation(t, p.s, p.repository, p.stream, driftOperations(t, p.repository, p.stream)[0].Operation, drifter{fm})
	if apiFault := move(intakefixture.BuildingState, "Reopen it."); apiFault == nil || apiFault.Code != api.Conflict || !strings.Contains(apiFault.Message, "is merged or closed") {
		t.Fatalf("a closed pull request reopened: %v", apiFault)
	}
	if apiFault := move(intake.AssembledState, "Review it."); apiFault == nil || apiFault.Code != api.Conflict || !strings.Contains(apiFault.Message, "is merged or closed") {
		t.Fatalf("a closed pull request returned to final review: %v", apiFault)
	}
}

func failedDriftTurn(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, agent string) {
	t.Helper()
	ctx := context.Background()
	th, err := repository.Thread(stream, agent)
	intakefixture.Must(t, err)
	i := slices.IndexFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.CompletedAt.IsZero() })
	if i < 0 {
		t.Fatalf("thread %s has no turn to run", agent)
	}
	token := "failed-" + th.Turns[i].Request.TurnID
	q, err := repository.ClaimTurn(ctx, stream, agent, token, filepath.Join(t.TempDir(), token), f.clock.Now())
	intakefixture.Must(t, err)
	h := q.Request.Header
	h.Schema, h.ID, h.At = "osmia.trace.turn-response", trace.EventID(q.Request.ID, "response"), f.clock.Now()
	response := trace.TurnResponse{Header: h, AgentID: agent, ThreadID: agent, TurnID: q.Request.TurnID, RequestID: q.Request.ID, RequestRevision: q.Request.Revision, Failure: "the backend crashed",
		Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: q.Request.Profile.Backend, ID: token}, SessionDirectory: q.Claim.SessionDirectory, StartedAt: q.Claim.At, IsError: true}}
	intakefixture.Must(t, repository.CaptureTurn(ctx, token, response))
	intakefixture.Must(t, repository.CompleteTurn(ctx, stream, agent, q.Request.TurnID, token, f.clock.Now()))
}

func TestDriftResolutionStuckTurnsHoldTheDriftRebase(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, want string
		stick      func(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, d drifter, op coreadapter.Operation)
	}{
		{name: "mason", want: "its mason's turn " + lifecycle.DriftResolveTurnID(1, 1) + " failed: the backend crashed",
			stick: func(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, _ drifter, _ coreadapter.Operation) {
				failedDriftTurn(t, f, repository, stream, service.TestDriftMasonAgent)
			}},
		{name: "reviewer", want: "its reviewer's turn " + lifecycle.DriftReviewTurnID(1, 1) + " ended without recording a verdict",
			stick: func(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, d drifter, op coreadapter.Operation) {
				w := resolutionWorkspace(t, f, stream)
				intakefixture.Must(t, os.WriteFile(filepath.Join(w.Path, "CODEOWNERS"), []byte("/internal/ @upstream @feature\n"), 0600))
				completeDriftTurn(t, f, repository, stream, service.TestDriftMasonAgent, resolvedDone("Resolved"))
				awaitResolution(t, f.s, d, stream, op, "review 1")
				completeDriftTurn(t, f, repository, stream, service.TestDriftReviewerAgent, nil)
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
			if state, err := repository.Workflow(stream, lifecycle.DriftSubject); err != nil || state.Value != lifecycle.DriftHeld+"-1" {
				t.Fatalf("the drift subject is %+v: %v", state, err)
			}
			if tip := featureTip(t, f, stream); tip != before {
				t.Fatalf("a held drift rebase moved the feature branch to %s", tip)
			}
			if k, apiFault := f.s.handBackDrift(context.Background(), repository, stream, "Keep both owners.", intakefixture.ChiefActor, "", f.s.now()); apiFault != nil || k != 2 {
				t.Fatalf("the handback of the stuck drift rebase %d %v", k, apiFault)
			}
		})
	}
}

func TestReviewersTreatRebaseChangesAsInScope(t *testing.T) {
	t.Parallel()
	if prompt := delivery.FinalSystemPrompt(config.Project{Name: "dagger", Upstream: "dagger/dagger"}); !strings.Contains(prompt, "changes a rebase or a drift resolution needed so that both upstream's behaviour and the feature's stand are part of the feature branch") ||
		!strings.Contains(prompt, "upstream's own changes are not the feature's") {
		t.Fatalf("the final system prompt:\n%s", prompt)
	}
	previous := api.FinalReport{Review: 1, Commit: "abc", Outcome: delivery.FinalReviewed}
	report := api.FinalReport{Review: 2, Commit: "def", Upstream: &seal.Base{Remote: "upstream", Branch: "main", Commit: "abc"}}
	if prompt := delivery.FinalPrompt(report, delivery.FinalCheckRun{}, nil, nil, previous); !strings.Contains(prompt, "that is upstream's change, not the feature's") {
		t.Fatalf("the final prompt after a previous review:\n%s", prompt)
	}

	f, stream, repository := newReviewFixture(t, "rebase-scope")
	ctx := context.Background()
	if conflicts, err := lifecycle.UnitRebaseConflicts(repository, stream, "resume", "tip"); err != nil || conflicts != nil {
		t.Fatalf("conflicts without a rebase %v %v", conflicts, err)
	}
	content, err := json.Marshal(api.UnitRebase{Unit: "resume", Rebase: 1, Onto: "tip", Conflicts: []string{"CODEOWNERS"}})
	intakefixture.Must(t, err)
	intakefixture.Must(t, repository.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: execution.RebaseDocument("resume"), Revision: 1, Project: f.project, Workstream: stream, Unit: "resume", At: fixturekit.DemoStart, Actor: service.TestForemanActor, Cause: "rebase"}, Path: "units/resume/rebase.json", Content: string(content)}}))
	if conflicts, err := lifecycle.UnitRebaseConflicts(repository, stream, "resume", "tip"); err != nil || !slices.Equal(conflicts, []string{"CODEOWNERS"}) {
		t.Fatalf("conflicts of the rebase onto the candidate's base %v %v", conflicts, err)
	}
	if conflicts, err := lifecycle.UnitRebaseConflicts(repository, stream, "resume", "other"); err != nil || conflicts != nil {
		t.Fatalf("conflicts of a rebase onto another base %v %v", conflicts, err)
	}
}

func atTheFinalRoundLimit(t *testing.T, key string) (*shedFixture, *trace.Repository, config.WorkstreamID, coreadapter.Tool) {
	t.Helper()
	f, stream, repository, a := newFinalFixture(t, key)
	hostFinalReviews(t, f, stream)
	f.s.setSole(&activeProject{repository: repository})
	_, op := assembleBoth(t, f, repository, a, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	report := failFinalReview(t, f, repository, stream, a, 1, op)
	for k := 2; k <= 4; k++ {
		report.Review, report.Outcome, report.Failure = k, delivery.FinalReviewed, ""
		report.Findings = []api.FinalFinding{{Severity: delivery.FindingBlocking, Finding: "The feature does not resume."}}
		content, err := json.Marshal(report)
		intakefixture.Must(t, err)
		revision, err := intake.NextRevision(repository, stream, delivery.FinalReportDocument)
		intakefixture.Must(t, err)
		intakefixture.Must(t, repository.RecordDocuments(context.Background(), []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: delivery.FinalReportDocument, Revision: revision, Project: repository.Project(), Workstream: stream, At: time.Now().UTC(), Actor: delivery.FinalReviewActor, Cause: "final-review"}, Path: delivery.FinalReportPath, Content: string(content)}}))
	}
	return f, repository, stream, chiefMoveTool(t, f, repository, stream, "events_1", false)
}

func TestTheChiefReopensAWorkstreamPastTheFinalRoundLimit(t *testing.T) {
	t.Parallel()
	f, repository, stream, tool := atTheFinalRoundLimit(t, "limit-chief")
	jev := &jevfixture.Transitions{}
	routeJev(f.s, jev)
	out := callMove(t, tool, `{"to":"building","note":"Reopen it."}`)
	if !strings.Contains(out, `"recorded":true`) || !strings.Contains(out, `"state":"building"`) {
		t.Fatalf("the chief of staff did not reopen the workstream: %s", out)
	}
	moved := lastOn(t, repository, stream, trace.FeatureSubject)
	if moved.To != intakefixture.BuildingState || moved.DecidedBy != trace.DecidedByChief || moved.Departed() {
		t.Fatalf("the transition %+v", moved)
	}
	if n := len(jev.TransitionRequests()); n != 0 {
		t.Fatalf("Jev got %d transition requests", n)
	}
}

func TestARevisionCommissionedOnAnAssembledWorkstreamReopensIt(t *testing.T) {
	t.Parallel()
	f, stream, repository, a := newFinalFixture(t, "chief-revision")
	hostFinalReviews(t, f, stream)
	f.s.setSole(&activeProject{repository: repository})
	_, op := assembleBoth(t, f, repository, a, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	failFinalReview(t, f, repository, stream, a, 1, op)
	if feature, err := repository.Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != intake.AssembledState {
		t.Fatalf("the workstream %+v %v", feature, err)
	}
	scope := intakefixture.ClaimChiefTurn(t, repository, f.project, stream, func() time.Time { return fixturekit.DemoStart }, intakefixture.ChiefTurnInput{
		Turn: "events_revision", Prompt: "The final review found a gap.", Actor: intake.ServiceActor, Cause: "fixture", Token: "token-events_revision", Directory: "events_revision",
	})
	tools, err := questions.Tools(repository, trace.ChiefOfStaff, scope, f.s.now)
	intakefixture.Must(t, err)
	i := slices.IndexFunc(tools, func(t coreadapter.Tool) bool { return t.Name == questions.RouteAmendmentTool })
	if i < 0 {
		t.Fatal("the chief of staff has no route_amendment tool")
	}
	controls := &runtimeControls{}
	controls.service.Store(f.s)
	tool := controls.lifecycle().ReopenForRevision(repository, scope, tools[i])
	out := callMove(t, tool, `{"citations":["spec#1"],"change":"Add a unit that resumes interrupted runs.","reason":"The final review found that runs do not resume."}`)
	if !strings.Contains(out, `"recorded":true`) || !strings.Contains(out, `"reopened"`) || !strings.Contains(out, `"amendment"`) {
		t.Fatalf("the commissioned revision: %s", out)
	}
	moved := lastOn(t, repository, stream, trace.FeatureSubject)
	if moved.To != intakefixture.BuildingState || moved.DecidedBy != trace.DecidedByChief {
		t.Fatalf("the reopening %+v", moved)
	}
}
