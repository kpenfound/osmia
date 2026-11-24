package executionjourney

import (
	"context"
	"encoding/json"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func newCheckingFixture(t *testing.T, key string) (*shedFixture, config.WorkstreamID, *trace.Repository) {
	t.Helper()
	f, _ := newMasonFixture(t, 1, independentPlan)
	base := strings.TrimSpace(fixturekit.DemoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	stream, repo := seedBuild(t, f, key, independentPlan, config.WorkspacesGit, base)
	seedChecking(t, f, repo, stream, "resume", resumeReport)
	f.s.setSole(&activeProject{repository: repo})
	return f, stream, repo
}

func workflowOf(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, unit string) trace.WorkflowState {
	t.Helper()
	state, err := repo.Workflow(stream, trace.UnitSubject(unit))
	fixturekit.Must(t, err)
	return state
}

func lastTurn(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, agent string) trace.QueuedTurn {
	t.Helper()
	th, err := repo.Thread(stream, agent)
	fixturekit.Must(t, err)
	return th.Turns[len(th.Turns)-1]
}

func TestOwnerMovesACheckingUnitBackToImplementing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repo := newCheckingFixture(t, "move-checking")
	reported := lastTurn(t, repo, stream, intake.MasonAgent("resume"))
	note := "Split the shadow writer out before the checks run again."
	out, apiErr := f.s.ownerMove(ctx, string(stream), "resume", api.UnitMoveRequest{To: intake.UnitImplementing, Note: note})
	if apiErr != nil {
		t.Fatal(apiErr.Message)
	}
	if out.Move.From != execution.UnitChecking || out.Move.To != intake.UnitImplementing || out.Move.By != "owner" || out.Move.ResetTurn != reported.Sequence {
		t.Fatalf("move %+v", out.Move)
	}
	state := workflowOf(t, repo, stream, "resume")
	transitions := workTransitions(t, repo, stream)
	moved := transitions[len(transitions)-1]
	if state.Value != intake.UnitImplementing || !execution.IsMove(moved, "resume") || moved.Actor != intake.OwnerActor || !strings.Contains(moved.Reason, note) {
		t.Fatalf("unit %s after %+v", state.Value, moved)
	}
	if !strings.Contains(intakefixture.NoticeOf(t, repo, stream, moved.ID), "owner moved unit resume from checking to implementing") {
		t.Fatal("the move did not tell the chief of staff")
	}

	m := newMasonController(f.s, repo)
	fixturekit.Must(t, m.Pass(ctx))
	turn := lastTurn(t, repo, stream, intake.MasonAgent("resume"))
	if turn.Sequence <= reported.Sequence || !strings.Contains(turn.Request.Prompt, note) || !strings.Contains(turn.Request.Prompt, "moved this unit from checking back to implementing") || turn.Request.Actor != intake.OwnerActor {
		t.Fatalf("the mason's next turn %s: %s", turn.Request.TurnID, turn.Request.Prompt)
	}
	if state := workflowOf(t, repo, stream, "resume"); state.Value != intake.UnitImplementing {
		t.Fatalf("the queued revision left the unit %s", state.Value)
	}
	fixturekit.Must(t, m.Pass(ctx))
	if again := lastTurn(t, repo, stream, intake.MasonAgent("resume")); again.Request.TurnID != turn.Request.TurnID {
		t.Fatalf("a second pass queued %s", again.Request.TurnID)
	}
	ruling, found, err := execution.LatestMasonRuling(repo, stream, "resume")
	fixturekit.Must(t, err)
	if !found || ruling.ResetTurn != reported.Sequence {
		t.Fatalf("clean-turn reset %+v", ruling)
	}

	completeMasonTurn(t, f, repo, stream, "resume", resumeReport)
	fixturekit.Must(t, m.Pass(ctx))
	if state := workflowOf(t, repo, stream, "resume"); state.Value != execution.UnitChecking {
		t.Fatalf("the revised unit is %s, not checking", state.Value)
	}
}

func TestMoveIntoCheckingRunsTheChecksAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repo := newReviewFixture(t, "move-recheck")
	f.s.setSole(&activeProject{repository: repo})
	if _, apiErr := f.s.ownerMove(ctx, string(stream), "resume", api.UnitMoveRequest{To: execution.UnitChecking, Note: "The engine was flaky; run them again."}); apiErr != nil {
		t.Fatal(apiErr.Message)
	}
	if state := workflowOf(t, repo, stream, "resume"); state.Value != execution.UnitChecking {
		t.Fatalf("unit is %s", state.Value)
	}
	runChecks(t, f.s, repo, stream)
	runs, err := execution.CheckRuns(repo, stream)
	fixturekit.Must(t, err)
	if len(runs) != 2 || runs[1].Run != 2 || runs[1].Status != checks.ChecksPassed {
		t.Fatalf("runs %+v", runs)
	}
	if state := workflowOf(t, repo, stream, "resume"); state.Value != intake.UnitReviewing {
		t.Fatalf("unit is %s after its checks ran again", state.Value)
	}
}

func TestMoveIntoReviewingRestartsTheReviewWithTheNote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repo := newReviewFixture(t, "move-review")
	f.s.setSole(&activeProject{repository: repo})
	r := &reviewers{masons: newMasonController(f.s, repo)}
	fixturekit.Must(t, r.one(ctx, stream, "resume", workflowOf(t, repo, stream, "resume"), false))
	first := lastTurn(t, repo, stream, intake.ReviewerAgent("resume"))
	note := "Judge only the acceptance; the footprint question is settled."
	if _, apiErr := f.s.ownerMove(ctx, string(stream), "resume", api.UnitMoveRequest{To: intake.UnitReviewing, Note: note}); apiErr != nil {
		t.Fatal(apiErr.Message)
	}
	state := workflowOf(t, repo, stream, "resume")
	fixturekit.Must(t, r.one(ctx, stream, "resume", state, false))
	turn := lastTurn(t, repo, stream, intake.ReviewerAgent("resume"))
	if turn.Request.TurnID == first.Request.TurnID || turn.Request.TurnID != execution.ReviewTurnID("resume", state.Version) || !strings.Contains(turn.Request.Prompt, note) {
		t.Fatalf("review turn %s after the move: %s", turn.Request.TurnID, turn.Request.Prompt)
	}
}

func TestMoveToApprovedRecordsAnApprovalTheForemanLands(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repo := newCheckingFixture(t, "move-approve")
	note := "The only failing check is the upstream flake; the change holds."
	out, apiErr := f.s.ownerMove(ctx, string(stream), "resume", api.UnitMoveRequest{To: intake.UnitApproved, Note: note})
	if apiErr != nil {
		t.Fatal(apiErr.Message)
	}
	if state := workflowOf(t, repo, stream, "resume"); state.Value != intake.UnitApproved {
		t.Fatalf("unit is %s", state.Value)
	}
	review, result := approvedReview(t, repo, stream, "resume")
	if result.Verdict.Decision != "satisfactory" || !strings.Contains(result.Verdict.Summary, note) || result.Identity.Candidate.Revision != out.Move.Candidate || review.Actor != intake.OwnerActor {
		t.Fatalf("approval %+v", result)
	}
	lands := &foreman{masons: newMasonController(f.s, repo)}
	if reason := landingReason(t, lands, repo, stream, "resume"); reason != "" {
		t.Fatalf("the foreman refuses the approval: %s", reason)
	}
}

func TestMoveRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repo := newCheckingFixture(t, "move-refusals")
	for _, c := range []struct {
		unit string
		req  api.UnitMoveRequest
		want string
	}{
		{"resume", api.UnitMoveRequest{To: intake.UnitMerged, Note: "Land it."}, "a move requires a note and a state"},
		{"resume", api.UnitMoveRequest{To: intake.UnitReviewing}, "a move requires a note and a state"},
		{"dedupe", api.UnitMoveRequest{To: intake.UnitImplementing, Note: "Start."}, "only a started unit that has not merged moves"},
		{"missing", api.UnitMoveRequest{To: intake.UnitImplementing, Note: "Start."}, "has no unit missing"},
	} {
		if _, apiErr := f.s.ownerMove(ctx, string(stream), c.unit, c.req); apiErr == nil || !strings.Contains(apiErr.Message, c.want) {
			t.Fatalf("move %s %+v: %v", c.unit, c.req, apiErr)
		}
	}
	if _, apiErr := f.s.ownerMove(ctx, string(stream), "resume", api.UnitMoveRequest{To: intake.UnitApproved, Note: "Holds."}); apiErr != nil {
		t.Fatal(apiErr.Message)
	}
	if _, apiErr := f.s.ownerMove(ctx, string(stream), "resume", api.UnitMoveRequest{To: intake.UnitApproved, Note: "Again."}); apiErr == nil || !strings.Contains(apiErr.Message, "already approved") {
		t.Fatalf("a second approval: %v", apiErr)
	}
	lands := &foreman{masons: newMasonController(f.s, repo)}
	fixturekit.Must(t, lands.Pass(ctx))
	if _, apiErr := f.s.ownerMove(ctx, string(stream), "resume", api.UnitMoveRequest{To: intake.UnitImplementing, Note: "Wait."}); apiErr == nil || !strings.Contains(apiErr.Message, "the foreman is landing unit resume") {
		t.Fatalf("a move during the landing: %v", apiErr)
	}
}

type moveFixture struct {
	*contestFixture
	move coreadapter.Tool
}

func newMoveFixture(t *testing.T, key string) *moveFixture {
	t.Helper()
	c := newContestFixture(t, key)
	controls := &runtimeControls{}
	controls.service.Store(c.f.s)
	scope := coreadapter.Scope{Project: string(c.f.project), Workstream: string(c.stream), Role: trace.ChiefOfStaff, Thread: trace.ChiefOfStaff, Turn: "events_1"}
	return &moveFixture{contestFixture: c, move: controls.moveUnit(c.repo, scope)}
}

func (m *moveFixture) moveUnit(t *testing.T, input string) string {
	t.Helper()
	return intakefixture.Handle(t, m.move, input)
}

func TestChiefOfStaffRecoversUnitsWithoutAnOwnerRetryGate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := newMoveFixture(t, "chief-moves")
	if out := m.moveUnit(t, `{"unit":"resume","to":"checking","note":"Run the checks again.","owner_decided":true}`); !strings.Contains(out, "only for a move the owner asked for") {
		t.Fatalf("an event turn recorded an owner move: %s", out)
	}
	if out := m.moveUnit(t, `{"unit":"resume","to":"checking","note":"Run the checks again."}`); !strings.Contains(out, `"recorded":true`) || !strings.Contains(out, `"by":"chief of staff"`) {
		t.Fatalf("move_unit: %s", out)
	}
	if out := m.moveUnit(t, `{"unit":"resume","to":"reviewing","note":"Review it again."}`); !strings.Contains(out, `"recorded":true`) {
		t.Fatalf("move_unit: %s", out)
	}
	if left, err := execution.ChiefRulingsLeft(m.repo, m.stream, "resume"); err != nil || left != -1 {
		t.Fatalf("rulings left after two moves: %d %v", left, err)
	}
	if out := m.moveUnit(t, `{"unit":"resume","to":"implementing","note":"Revise it."}`); !strings.Contains(out, `"recorded":true`) {
		t.Fatalf("a move past the limit was recorded: %s", out)
	}
	hold := "Checks pass but every review finds the same gap; I recommend amending the plan."
	if out := m.moveUnit(t, `{"unit":"resume","to":"contested","note":"`+hold+`"}`); !strings.Contains(out, `"recorded":true`) {
		t.Fatalf("move to contested: %s", out)
	}
	contest, contested, err := execution.UnitContest(m.repo, m.stream, "resume")
	fixturekit.Must(t, err)
	if raised, note := m.raised(t, contest); !contested || !raised || note != hold {
		t.Fatalf("held unit raised %t with %q", raised, note)
	}
	if out := m.resolve(t, `{"unit":"resume","decision":"review","note":"Review it."}`); !strings.Contains(out, "moved to contested for the owner") {
		t.Fatalf("the chief of staff ruled on the owner's contest: %s", out)
	}
	entry, ok, err := m.f.s.contestedEntry(m.repo, m.stream, "resume")
	fixturekit.Must(t, err)
	if !ok || entry.Recommendation != hold || strings.Join(entry.Options, ",") != "review,revise" {
		t.Fatalf("inbox entry %+v", entry)
	}

	if entry.Question != "Unit resume is contested: the chief of staff moved it here from implementing for you to rule on." {
		t.Fatalf("inbox question %q", entry.Question)
	}
	out, apiErr := m.f.s.recordContestedRuling(ctx, m.repo, m.stream, "resume", api.ContestedRulingRequest{Decision: "revise", Note: "Close the gap the reviews keep finding."}, intake.OwnerActor)
	if apiErr != nil {
		t.Fatal(apiErr.Message)
	}
	if state := m.state(t); state.Value != intake.UnitImplementing || out.Ruling.Contest != contest.ID {
		t.Fatalf("ruled unit is %s with %+v", state.Value, out.Ruling)
	}
	if left, err := execution.ChiefRulingsLeft(m.repo, m.stream, "resume"); err != nil || left != execution.ChiefContestLimit {
		t.Fatalf("rulings left after the owner's ruling: %d %v", left, err)
	}
	actions, err := service.ChiefActions(m.repo, m.stream)
	fixturekit.Must(t, err)
	if len(actions) != 4 || actions[0].Text != "Moved unit resume from reviewing to checking. Run the checks again." || actions[3].Text != "Held unit resume for you, from implementing: "+hold || actions[3].Turn != "events_1" {
		t.Fatalf("conversation actions %+v", actions)
	}
}

func TestChiefOfStaffRecordsTheOwnersMove(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repo := newCheckingFixture(t, "chief-owner-move")
	_, err := repo.EnsureChiefOfStaff(ctx, stream, fixturekit.DemoStart, intake.ServiceActor)
	fixturekit.Must(t, err)
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_message_1", Revision: 1, Project: f.project, Workstream: stream, At: fixturekit.DemoStart, Actor: intake.OwnerActor, Cause: "owner-message"},
		AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "message_1", Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "chief", Prompt: "Send resume back to the mason."})
	fixturekit.Must(t, err)
	_, err = repo.ClaimTurn(ctx, stream, trace.ChiefOfStaff, "token-message", filepath.Join(t.TempDir(), "message"), fixturekit.DemoStart)
	fixturekit.Must(t, err)
	controls := &runtimeControls{}
	controls.service.Store(f.s)
	tool := controls.moveUnit(repo, coreadapter.Scope{Project: string(f.project), Workstream: string(stream), Role: trace.ChiefOfStaff, Thread: trace.ChiefOfStaff, Turn: "message_1"})
	out, err := tool.Handle(ctx, json.RawMessage(`{"unit":"resume","to":"implementing","note":"Send it back to the mason.","owner_decided":true}`))
	fixturekit.Must(t, err)
	if !strings.Contains(string(out), `"by":"owner"`) {
		t.Fatalf("move_unit: %s", out)
	}
	transitions := workTransitions(t, repo, stream)
	if moved := transitions[len(transitions)-1]; moved.Actor != intake.OwnerActor || moved.To != intake.UnitImplementing {
		t.Fatalf("move %+v", moved)
	}
}
