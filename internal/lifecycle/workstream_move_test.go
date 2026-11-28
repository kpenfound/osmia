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

	"github.com/kpenfound/osmia/internal/servicetest/journey"

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

func managerMoveTool(t *testing.T, f *journey.ShedFixture, repo *trace.Repository, stream config.WorkstreamID, turn string, owner bool) coreadapter.Tool {
	t.Helper()
	actor, cause := intake.ServiceActor, "fixture"
	if owner {
		actor, cause = intake.OwnerActor, "owner-message"
	}
	scope := intakefixture.ClaimManagerTurn(t, repo, f.Project, stream, func() time.Time { return fixturekit.DemoStart }, intakefixture.ManagerTurnInput{
		Turn: turn, Prompt: "The final review failed.", Actor: actor, Cause: cause, Token: "token-" + turn, Directory: turn,
	})
	controls := &journey.RuntimeControls{}
	controls.Service.Store(f.S)
	return controls.Lifecycle().MoveWorkstream(repo, scope)
}
func failFinalReview(t *testing.T, f *journey.ShedFixture, repository *trace.Repository, stream config.WorkstreamID, a *journey.FinalReviewer, k int, op coreadapter.Operation) api.FinalReport {
	t.Helper()
	f.FinalTurn(k, 1, func(context.Context, *mcp.ClientSession) error { return nil })
	result := journey.SettleOperation(t, f.S, repository, stream, op, a)
	report, _, err := delivery.LatestFinalReport(repository, stream)
	fixturekit.Must(t, err)
	if result.Outcome != "failed" || report.Outcome != delivery.FinalFailed || report.Review != k {
		t.Fatalf("final review %d: %+v %+v", k, result, report)
	}
	return report
}

func TestMoveToAssembledRerunsAFailedFinalReview(t *testing.T) {
	t.Parallel()
	f, stream, repository, a := journey.NewFinalFixtureWith(t, "move-rerun", "")
	journey.HostFinalReviews(t, f, stream)
	f.S.SetSole(&journey.ActiveProject{Repository: repository})
	ctx := context.Background()
	_, op := journey.AssembleBoth(t, f, repository, a, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	failFinalReview(t, f, repository, stream, a, 1, op)
	fixturekit.Must(t, a.Pass(ctx))
	fixturekit.Must(t, a.Pass(ctx))
	if ops := journey.FinalOperations(t, repository, stream); len(ops) != 1 {
		t.Fatalf("a failed review on an unchanged branch was asked for again: %+v", ops)
	}
	if _, reason, err := a.FinalGate(ctx, stream); err != nil || !strings.HasPrefix(reason, "final review 1 failed") {
		t.Fatalf("the gate after the failure: %q %v", reason, err)
	}

	note := "The reader stopped without a report; read the branch again."
	out, apiFault := f.S.Lifecycle().OwnerMoveWorkstream(ctx, string(stream), api.WorkstreamMoveRequest{To: intake.AssembledState, Note: note})
	if apiFault != nil {
		t.Fatal(apiFault.Message)
	}
	if out.State != intake.AssembledState || out.Move == nil || out.Move.Move != 1 || out.Move.From != intake.AssembledState || out.Move.By != "owner" || !strings.Contains(out.Detail, note) {
		t.Fatalf("the move %+v", out)
	}
	if body := intakefixture.NoticeOf(t, repository, stream, lifecycle.WorkstreamMoveDocument(1)+"-recorded"); !strings.Contains(body, "the owner asked for a fresh final review of the assembled branch: "+note) {
		t.Fatalf("the chief of staff heard %q", body)
	}
	fixturekit.Must(t, a.Pass(ctx))
	ops := journey.FinalOperations(t, repository, stream)
	if len(ops) != 2 {
		t.Fatalf("the move asked for no fresh review: %+v", ops)
	}
	in, err := delivery.DecodeFinalReview(ops[1].Operation)
	fixturekit.Must(t, err)
	if in.Review != 2 || in.Move != 1 {
		t.Fatalf("the fresh review's input %+v", in)
	}
	f.FinalTurn(2, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Ready", "findings": []any{}})
		return err
	})
	if result := journey.SettleOperation(t, f.S, repository, stream, ops[1].Operation, a); result.Outcome != "succeeded" {
		t.Fatalf("the fresh review %+v", result)
	}
	reader, err := repository.Thread(stream, intake.CommitteeAgent(1))
	fixturekit.Must(t, err)
	if prompt := reader.Turns[len(reader.Turns)-1].Request.Prompt; !strings.Contains(prompt, "The owner asked for this review with this note: "+note) {
		t.Fatalf("the reader was not told of the move:\n%s", prompt)
	}
	if report, reason, err := a.FinalGate(ctx, stream); err != nil || reason != "" || report.Move != 1 {
		t.Fatalf("the gate after the fresh review: %+v %q %v", report, reason, err)
	}
	fixturekit.Must(t, a.Pass(ctx))
	if ops := journey.FinalOperations(t, repository, stream); len(ops) != 2 {
		t.Fatalf("a current review was asked for again: %+v", ops)
	}
}

func TestManagerMovesAWorkstreamAndRelaysOwnerGates(t *testing.T) {
	t.Parallel()
	f, stream, repository, a := journey.NewFinalFixtureWith(t, "chief-move", "")
	journey.HostFinalReviews(t, f, stream)
	f.S.SetSole(&journey.ActiveProject{Repository: repository})
	ctx := context.Background()
	_, op := journey.AssembleBoth(t, f, repository, a, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	failFinalReview(t, f, repository, stream, a, 1, op)
	tool := managerMoveTool(t, f, repository, stream, "events_1", false)

	for _, to := range []string{intake.SketchedState, intake.InShedState, intake.RatifiedState, intake.AbandonedState} {
		out := intakefixture.Handle(t, tool, fmt.Sprintf(`{"to":%q,"note":"Do it."}`, to))
		if !strings.Contains(out, `"recorded":false`) || strings.Contains(out, "owner_decided") || strings.Contains(out, "present it to the owner") {
			t.Fatalf("the chief of staff moved the workstream to %s on its own: %s", to, out)
		}
		if (to == intake.RatifiedState || to == intake.AbandonedState) && !strings.Contains(out, "pending as an owner decision") {
			t.Fatalf("the proposal to %s does not point to its card: %s", to, out)
		}
	}
	if out := intakefixture.Handle(t, tool, `{"to":"abandoned","note":"Stop.","owner_decided":true}`); !strings.Contains(out, "only for a move the owner asked for") {
		t.Fatalf("an event turn recorded an owner move: %s", out)
	}
	if out := intakefixture.Handle(t, tool, `{"to":"building","note":"Reopen it so a revision can repair the gap."}`); !strings.Contains(out, `"recorded":true`) || !strings.Contains(out, `"state":"building"`) {
		t.Fatalf("move_workstream to building: %s", out)
	}
	fixturekit.Must(t, a.Pass(ctx))
	fixturekit.Must(t, a.Pass(ctx))
	if feature, err := repository.Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != intake.BuildingState {
		t.Fatalf("the reopened workstream reassembled before any revision: %+v %v", feature, err)
	}
	if out := intakefixture.Handle(t, tool, `{"to":"building","note":"Again."}`); !strings.Contains(out, "already building") {
		t.Fatalf("a second move to building: %s", out)
	}
	if out := intakefixture.Handle(t, tool, `{"to":"assembled","note":"Nothing needs repair after all; review it again."}`); !strings.Contains(out, `"recorded":true`) {
		t.Fatalf("move_workstream to assembled: %s", out)
	}
	fixturekit.Must(t, a.Pass(ctx))
	ops := journey.FinalOperations(t, repository, stream)
	if len(ops) != 2 {
		t.Fatalf("final review operations %+v", ops)
	}
	if in, err := delivery.DecodeFinalReview(ops[1].Operation); err != nil || in.Move != 2 {
		t.Fatalf("the review asked for after the moves %+v %v", in, err)
	}
	actions, err := service.ManagerActions(repository, stream)
	fixturekit.Must(t, err)
	var texts []string
	for _, action := range actions {
		texts = append(texts, action.Text)
	}
	if !slices.Contains(texts, "Moved the workstream from assembled to building. Reopen it so a revision can repair the gap.") ||
		!slices.Contains(texts, "Moved the workstream from building to assembled. Nothing needs repair after all; review it again.") {
		t.Fatalf("conversation actions %q", texts)
	}

	report, _, err := delivery.LatestFinalReport(repository, stream)
	fixturekit.Must(t, err)
	for k := 2; k <= 4; k++ {
		report.Review, report.Outcome, report.Failure = k, delivery.FinalReviewed, ""
		report.Findings = []api.FinalFinding{{Severity: delivery.FindingBlocking, Finding: "The feature does not resume."}}
		content, err := json.Marshal(report)
		fixturekit.Must(t, err)
		revision, err := intake.NextRevision(repository, stream, delivery.FinalReportDocument)
		fixturekit.Must(t, err)
		fixturekit.Must(t, repository.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: delivery.FinalReportDocument, Revision: revision, Project: repository.Project(), Workstream: stream, At: time.Now().UTC(), Actor: delivery.FinalReviewActor, Cause: "final-review"}, Path: delivery.FinalReportPath, Content: string(content)}}))
	}
	if out := intakefixture.Handle(t, tool, `{"to":"assembled","note":"Review it again."}`); !strings.Contains(out, `"recorded":true`) {
		t.Fatalf("a fresh review after three blocking reviews: %s", out)
	}
}

func TestManagerRelaysTheOwnersAbandonment(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := journey.NewFinalFixtureWith(t, "chief-relay", "")
	f.S.SetSole(&journey.ActiveProject{Repository: repository})
	tool := managerMoveTool(t, f, repository, stream, "message_1", true)
	if out := intakefixture.Handle(t, tool, `{"to":"abandoned","note":"Stop this one.","owner_decided":true}`); !strings.Contains(out, `"recorded":true`) || !strings.Contains(out, `"state":"abandoned"`) {
		t.Fatalf("the relayed abandonment: %s", out)
	}
	abandon := intakefixture.TransitionByID(t, repository, stream, intake.AbandonTransition)
	if abandon.Actor != intake.OwnerActor || abandon.Reason != "Stop this one." {
		t.Fatalf("the abandonment %+v", abandon)
	}
}

func TestReopenedDeliveredPullRequestIsRepairedOnTheSamePullRequest(t *testing.T) {
	t.Parallel()
	p := journey.NewPublicationFixture(t, "squash")
	ctx := context.Background()
	p.Approve(t, nil)
	if result := journey.SettleOperation(t, p.S, p.Repository, p.Stream, p.Request(t), p.Publisher()); result.Outcome != "succeeded" || p.Feature(t) != intake.DeliveredState {
		t.Fatalf("the first publication %+v", result)
	}
	p.Pulls.SyncHead = true
	before, _ := p.ForkBranch(t)

	out, apiFault := p.S.Lifecycle().OwnerMoveWorkstream(ctx, string(p.Stream), api.WorkstreamMoveRequest{To: intake.BuildingState, Note: "Upstream broke resume; repair it on this pull request."})
	if apiFault != nil {
		t.Fatal(apiFault.Message)
	}
	if out.State != intake.BuildingState || !strings.Contains(out.Detail, "pull request #100 stays open") {
		t.Fatalf("the reopen %+v", out)
	}
	reviewer := &journey.FinalReviewer{S: p.S, Repository: p.Repository}
	fixturekit.Must(t, reviewer.Pass(ctx))
	if p.Feature(t) != intake.BuildingState {
		t.Fatalf("the reopened workstream is %s", p.Feature(t))
	}
	if _, apiFault := p.S.Lifecycle().OwnerMoveWorkstream(ctx, string(p.Stream), api.WorkstreamMoveRequest{To: intake.DeliveredState, Note: "Nothing to repair."}); apiFault != nil {
		t.Fatalf("an unchanged reopened branch did not return to delivered: %s", apiFault.Message)
	}
	if p.Feature(t) != intake.DeliveredState {
		t.Fatalf("the workstream is %s", p.Feature(t))
	}

	if _, apiFault := p.S.Lifecycle().OwnerMoveWorkstream(ctx, string(p.Stream), api.WorkstreamMoveRequest{To: intake.BuildingState, Note: "Repair it after all."}); apiFault != nil {
		t.Fatal(apiFault.Message)
	}
	journey.MoveFeature(t, p.ShedFixture, p.Stream, map[string]string{"resume.go": "package demo\n// Resume after the upstream change.\n"})
	if _, apiFault := p.S.Lifecycle().OwnerMoveWorkstream(ctx, string(p.Stream), api.WorkstreamMoveRequest{To: intake.DeliveredState, Note: "Done."}); apiFault == nil || apiFault.Code != api.Conflict || !strings.Contains(apiFault.Message, "since pull request #100 was published") {
		t.Fatalf("a changed branch returned to delivered without review: %v", apiFault)
	}
	if _, apiFault := p.S.Lifecycle().OwnerMoveWorkstream(ctx, string(p.Stream), api.WorkstreamMoveRequest{To: intake.AssembledState, Note: "The repair landed; review the branch."}); apiFault != nil {
		t.Fatal(apiFault.Message)
	}
	fixturekit.Must(t, reviewer.Pass(ctx))
	op := fixturekit.ActionOperation(t, p.Repository, p.Stream, delivery.FinalReviewAction)
	in, err := delivery.DecodeFinalReview(op)
	fixturekit.Must(t, err)
	if in.Maintenance || in.Move != 3 {
		t.Fatalf("the review of the repaired branch %+v", in)
	}
	p.FinalTurn(in.Review, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Repaired", "findings": []any{}})
		return err
	})
	if result := journey.SettleOperation(t, p.S, p.Repository, p.Stream, op, reviewer); result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	pushed, _ := p.ForkBranch(t)
	if pushed == before || p.Pulls.Creates != 1 || p.Pulls.Prs[0].HeadCommit != pushed {
		t.Fatalf("the repaired branch was not pushed to the same pull request: tip %s, was %s, creates %d, pull request %+v", pushed, before, p.Pulls.Creates, p.Pulls.Prs[0])
	}
	description := "Resumable uploads\n\nRepaired after upstream moved."
	p.Approve(t, &description)
	if result := journey.SettleOperation(t, p.S, p.Repository, p.Stream, p.Request(t), p.Publisher()); result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	if p.Pulls.Creates != 1 || p.Pulls.Updates == 0 || p.Pulls.Prs[0].Body != description || p.Feature(t) != intake.DeliveredState {
		t.Fatalf("the repaired delivery: creates %d, updates %d, body %q, state %s", p.Pulls.Creates, p.Pulls.Updates, p.Pulls.Prs[0].Body, p.Feature(t))
	}
}

func TestWorkstreamMoveRefusals(t *testing.T) {
	t.Parallel()
	p := journey.NewUnpushedPublicationFixture(t, "commit-per-unit", "")
	ctx := context.Background()
	move := func(to, note string) *api.APIError {
		_, apiFault := p.S.Lifecycle().OwnerMoveWorkstream(ctx, string(p.Stream), api.WorkstreamMoveRequest{To: to, Note: note})
		return apiFault
	}
	if apiFault := move("merged", "x"); apiFault == nil || apiFault.Code != api.Validation {
		t.Fatalf("an unknown state: %v", apiFault)
	}
	if apiFault := move(intake.BuildingState, " "); apiFault == nil || apiFault.Code != api.Validation {
		t.Fatalf("an empty note: %v", apiFault)
	}
	if apiFault := move(intake.DeliveredState, "Deliver it."); apiFault == nil || apiFault.Code != api.Conflict || !strings.Contains(apiFault.Message, "delivered through the owner's delivery approval") {
		t.Fatalf("an assembled workstream moved to delivered: %v", apiFault)
	}
	p.PublishDraft(t)
	p.Approve(t, nil)
	publish := p.Request(t)
	if apiFault := move(intake.BuildingState, "Reopen it."); apiFault == nil || apiFault.Code != api.Conflict || !strings.Contains(apiFault.Message, "publication is in flight") {
		t.Fatalf("a move during publication: %v", apiFault)
	}
	journey.SettleOperation(t, p.S, p.Repository, p.Stream, publish, p.Publisher())
	if p.Feature(t) != intake.DeliveredState {
		t.Fatalf("the workstream is %s", p.Feature(t))
	}
	p.Pulls.Prs[0].State = "closed"
	fm := cadenceForeman(p.ShedFixture, p.Repository, "1m")
	if _, apiFault := p.S.Lifecycle().RebaseWorkstream(ctx, string(p.Stream)); apiFault != nil {
		t.Fatal(apiFault.Message)
	}
	passDrifts(t, fm)
	journey.SettleOperation(t, p.S, p.Repository, p.Stream, journey.DriftOperations(t, p.Repository, p.Stream)[0].Operation, journey.Drifter{Foreman: fm})
	if apiFault := move(intake.BuildingState, "Reopen it."); apiFault == nil || apiFault.Code != api.Conflict || !strings.Contains(apiFault.Message, "is merged or closed") {
		t.Fatalf("a closed pull request reopened: %v", apiFault)
	}
	if apiFault := move(intake.AssembledState, "Review it."); apiFault == nil || apiFault.Code != api.Conflict || !strings.Contains(apiFault.Message, "is merged or closed") {
		t.Fatalf("a closed pull request returned to final review: %v", apiFault)
	}
}

func failedDriftTurn(t *testing.T, f *journey.ShedFixture, repository *trace.Repository, stream config.WorkstreamID, agent string) {
	t.Helper()
	ctx := context.Background()
	th, err := repository.Thread(stream, agent)
	fixturekit.Must(t, err)
	i := slices.IndexFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.CompletedAt.IsZero() })
	if i < 0 {
		t.Fatalf("thread %s has no turn to run", agent)
	}
	token := "failed-" + th.Turns[i].Request.TurnID
	q, err := repository.ClaimTurn(ctx, stream, agent, token, filepath.Join(t.TempDir(), token), f.Clock.Now())
	fixturekit.Must(t, err)
	h := q.Request.Header
	h.Schema, h.ID, h.At = "osmia.trace.turn-response", trace.EventID(q.Request.ID, "response"), f.Clock.Now()
	response := trace.TurnResponse{Header: h, AgentID: agent, ThreadID: agent, TurnID: q.Request.TurnID, RequestID: q.Request.ID, RequestRevision: q.Request.Revision, Failure: "the backend crashed",
		Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: q.Request.Profile.Backend, ID: token}, SessionDirectory: q.Claim.SessionDirectory, StartedAt: q.Claim.At, IsError: true}}
	fixturekit.Must(t, repository.CaptureTurn(ctx, token, response))
	fixturekit.Must(t, repository.CompleteTurn(ctx, stream, agent, q.Request.TurnID, token, f.Clock.Now()))
}

func TestDriftResolutionStuckTurnsHoldTheDriftRebase(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, want string
		stick      func(t *testing.T, f *journey.ShedFixture, repository *trace.Repository, stream config.WorkstreamID, d journey.Drifter, op coreadapter.Operation)
	}{
		{name: "mason", want: "its mason's turn " + lifecycle.DriftResolveTurnID(1, 1) + " failed: the backend crashed",
			stick: func(t *testing.T, f *journey.ShedFixture, repository *trace.Repository, stream config.WorkstreamID, _ journey.Drifter, _ coreadapter.Operation) {
				failedDriftTurn(t, f, repository, stream, intake.DriftMasonAgent)
			}},
		{name: "reviewer", want: "its reviewer's turn " + lifecycle.DriftReviewTurnID(1, 1) + " ended without recording a verdict",
			stick: func(t *testing.T, f *journey.ShedFixture, repository *trace.Repository, stream config.WorkstreamID, d journey.Drifter, op coreadapter.Operation) {
				w := journey.ResolutionWorkspace(t, f, stream)
				fixturekit.Must(t, os.WriteFile(filepath.Join(w.Path, "CODEOWNERS"), []byte("/internal/ @upstream @feature\n"), 0600))
				journey.CompleteDriftTurn(t, f, repository, stream, intake.DriftMasonAgent, journey.ResolvedDone("Resolved"))
				journey.AwaitResolution(t, f.S, d, stream, op, "review 1")
				journey.CompleteDriftTurn(t, f, repository, stream, intake.DriftReviewerAgent, nil)
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, stream, repository, _ := journey.NewFinalFixtureWith(t, "drift-stuck-"+tc.name, "")
			fm := cadenceForeman(f, repository, "1h")
			d := journey.Drifter{Foreman: fm}
			before, _, op := journey.ConflictedDrift(t, f, d, stream)
			journey.AwaitResolution(t, f.S, d, stream, op, "its mason's resolution")
			tc.stick(t, f, repository, stream, d, op)
			result, err := journey.AttemptOperation(t, f.S, repository, stream, op, d)
			if err != nil || result.Outcome != "succeeded" || !strings.Contains(result.Evidence, "drift rebase 1 is held for the owner: "+tc.want) {
				t.Fatalf("the stuck drift rebase %+v %v", result, err)
			}
			if state, err := repository.Workflow(stream, lifecycle.DriftSubject); err != nil || state.Value != lifecycle.DriftHeld+"-1" {
				t.Fatalf("the drift subject is %+v: %v", state, err)
			}
			if tip := journey.FeatureTip(t, f, stream); tip != before {
				t.Fatalf("a held drift rebase moved the feature branch to %s", tip)
			}
			if k, apiFault := f.S.Lifecycle().HandBackDrift(context.Background(), repository, stream, "Keep both owners.", intake.ManagerActor, "", f.S.Now()); apiFault != nil || k != 2 {
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

	f, stream, repository := journey.NewReviewFixture(t, "rebase-scope")
	ctx := context.Background()
	if conflicts, err := lifecycle.UnitRebaseConflicts(repository, stream, "resume", "tip"); err != nil || conflicts != nil {
		t.Fatalf("conflicts without a rebase %v %v", conflicts, err)
	}
	content, err := json.Marshal(api.UnitRebase{Unit: "resume", Rebase: 1, Onto: "tip", Conflicts: []string{"CODEOWNERS"}})
	fixturekit.Must(t, err)
	fixturekit.Must(t, repository.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: execution.RebaseDocument("resume"), Revision: 1, Project: f.Project, Workstream: stream, Unit: "resume", At: fixturekit.DemoStart, Actor: execution.ForemanActor, Cause: "rebase"}, Path: "units/resume/rebase.json", Content: string(content)}}))
	if conflicts, err := lifecycle.UnitRebaseConflicts(repository, stream, "resume", "tip"); err != nil || !slices.Equal(conflicts, []string{"CODEOWNERS"}) {
		t.Fatalf("conflicts of the rebase onto the candidate's base %v %v", conflicts, err)
	}
	if conflicts, err := lifecycle.UnitRebaseConflicts(repository, stream, "resume", "other"); err != nil || conflicts != nil {
		t.Fatalf("conflicts of a rebase onto another base %v %v", conflicts, err)
	}
}

func atTheFinalRoundLimit(t *testing.T, key string) (*journey.ShedFixture, *trace.Repository, config.WorkstreamID, coreadapter.Tool) {
	t.Helper()
	f, stream, repository, a := journey.NewFinalFixtureWith(t, key, "")
	journey.HostFinalReviews(t, f, stream)
	f.S.SetSole(&journey.ActiveProject{Repository: repository})
	_, op := journey.AssembleBoth(t, f, repository, a, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	report := failFinalReview(t, f, repository, stream, a, 1, op)
	for k := 2; k <= 4; k++ {
		report.Review, report.Outcome, report.Failure = k, delivery.FinalReviewed, ""
		report.Findings = []api.FinalFinding{{Severity: delivery.FindingBlocking, Finding: "The feature does not resume."}}
		content, err := json.Marshal(report)
		fixturekit.Must(t, err)
		revision, err := intake.NextRevision(repository, stream, delivery.FinalReportDocument)
		fixturekit.Must(t, err)
		fixturekit.Must(t, repository.RecordDocuments(context.Background(), []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: delivery.FinalReportDocument, Revision: revision, Project: repository.Project(), Workstream: stream, At: time.Now().UTC(), Actor: delivery.FinalReviewActor, Cause: "final-review"}, Path: delivery.FinalReportPath, Content: string(content)}}))
	}
	return f, repository, stream, managerMoveTool(t, f, repository, stream, "events_1", false)
}

func TestTheManagerReopensAWorkstreamPastTheFinalRoundLimit(t *testing.T) {
	t.Parallel()
	f, repository, stream, tool := atTheFinalRoundLimit(t, "limit-chief")
	jev := &jevfixture.Transitions{}
	journey.InspectForTest(f.S).SetRouterJudge(jevfixture.TransitionJudge(f.S.Now, jev))
	out := intakefixture.Handle(t, tool, `{"to":"building","note":"Reopen it."}`)
	if !strings.Contains(out, `"recorded":true`) || !strings.Contains(out, `"state":"building"`) {
		t.Fatalf("the chief of staff did not reopen the workstream: %s", out)
	}
	moved := lastOn(t, repository, stream, trace.FeatureSubject)
	if moved.To != intake.BuildingState || moved.DecidedBy != trace.DecidedByManager || moved.Departed() {
		t.Fatalf("the transition %+v", moved)
	}
	if n := len(jev.TransitionRequests()); n != 0 {
		t.Fatalf("Jev got %d transition requests", n)
	}
}

func TestARevisionCommissionedOnAnAssembledWorkstreamReopensIt(t *testing.T) {
	t.Parallel()
	f, stream, repository, a := journey.NewFinalFixtureWith(t, "chief-revision", "")
	journey.HostFinalReviews(t, f, stream)
	f.S.SetSole(&journey.ActiveProject{Repository: repository})
	_, op := journey.AssembleBoth(t, f, repository, a, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	failFinalReview(t, f, repository, stream, a, 1, op)
	if feature, err := repository.Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != intake.AssembledState {
		t.Fatalf("the workstream %+v %v", feature, err)
	}
	scope := intakefixture.ClaimManagerTurn(t, repository, f.Project, stream, func() time.Time { return fixturekit.DemoStart }, intakefixture.ManagerTurnInput{
		Turn: "events_revision", Prompt: "The final review found a gap.", Actor: intake.ServiceActor, Cause: "fixture", Token: "token-events_revision", Directory: "events_revision",
	})
	tools, err := questions.Tools(repository, trace.Manager, scope, f.S.Now)
	fixturekit.Must(t, err)
	i := slices.IndexFunc(tools, func(t coreadapter.Tool) bool { return t.Name == questions.RouteAmendmentTool })
	if i < 0 {
		t.Fatal("the chief of staff has no route_amendment tool")
	}
	controls := &journey.RuntimeControls{}
	controls.Service.Store(f.S)
	tool := controls.Lifecycle().ReopenForRevision(repository, scope, tools[i])
	out := intakefixture.Handle(t, tool, `{"citations":["spec#1"],"change":"Add a unit that resumes interrupted runs.","reason":"The final review found that runs do not resume."}`)
	if !strings.Contains(out, `"recorded":true`) || !strings.Contains(out, `"reopened"`) || !strings.Contains(out, `"amendment"`) {
		t.Fatalf("the commissioned revision: %s", out)
	}
	moved := lastOn(t, repository, stream, trace.FeatureSubject)
	if moved.To != intake.BuildingState || moved.DecidedBy != trace.DecidedByManager {
		t.Fatalf("the reopening %+v", moved)
	}
}
