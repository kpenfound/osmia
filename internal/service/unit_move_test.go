package service

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// newCheckingFixture creates a building workstream whose unit resume is
// checking its recorded candidate, with no check run yet.
func newCheckingFixture(t *testing.T, key string) (*shedFixture, config.WorkstreamID, *trace.Repository) {
	t.Helper()
	f, _ := newMasonFixture(t, 1, independentPlan)
	base := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	stream, repo := seedBuild(t, f, key, independentPlan, config.WorkspacesGit, base)
	seedChecking(t, f, repo, stream, "resume", resumeReport)
	f.s.setSole(&activeProject{repository: repo})
	return f, stream, repo
}

func workflowOf(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, unit string) trace.WorkflowState {
	t.Helper()
	state, err := repo.Workflow(stream, trace.UnitSubject(unit))
	must(t, err)
	return state
}

func lastTurn(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, agent string) trace.QueuedTurn {
	t.Helper()
	th, err := repo.Thread(stream, agent)
	must(t, err)
	return th.Turns[len(th.Turns)-1]
}

// The owner sends a unit held in checking back to implementing. Its mason
// gets a turn with the owner's note and a fresh clean-turn allowance, and
// the done it reported for the old candidate does not move the unit on.
func TestOwnerMovesACheckingUnitBackToImplementing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repo := newCheckingFixture(t, "move-checking")
	reported := lastTurn(t, repo, stream, masonAgent("resume"))
	note := "Split the shadow writer out before the checks run again."
	out, api := f.s.ownerMove(ctx, string(stream), "resume", UnitMoveRequest{To: UnitImplementing, Note: note})
	if api != nil {
		t.Fatal(api.Message)
	}
	if out.Move.From != UnitChecking || out.Move.To != UnitImplementing || out.Move.By != "owner" || out.Move.ResetTurn != reported.Sequence {
		t.Fatalf("move %+v", out.Move)
	}
	state := workflowOf(t, repo, stream, "resume")
	transitions, err := trace.Read[trace.Transition](repo, stream)
	must(t, err)
	moved := transitions[len(transitions)-1]
	if state.Value != UnitImplementing || !isMove(moved, "resume") || moved.Actor != ownerActor || !strings.Contains(moved.Reason, note) {
		t.Fatalf("unit %s after %+v", state.Value, moved)
	}
	if !strings.Contains(noticeOf(t, repo, stream, moved.ID), "owner moved unit resume from checking to implementing") {
		t.Fatal("the move did not tell the chief of staff")
	}

	m := newMasonController(f.s, repo)
	must(t, m.Pass(ctx))
	turn := lastTurn(t, repo, stream, masonAgent("resume"))
	if turn.Sequence <= reported.Sequence || !strings.Contains(turn.Request.Prompt, note) || !strings.Contains(turn.Request.Prompt, "moved this unit from checking back to implementing") || turn.Request.Actor != ownerActor {
		t.Fatalf("the mason's next turn %s: %s", turn.Request.TurnID, turn.Request.Prompt)
	}
	if state := workflowOf(t, repo, stream, "resume"); state.Value != UnitImplementing {
		t.Fatalf("the queued revision left the unit %s", state.Value)
	}
	must(t, m.Pass(ctx))
	if again := lastTurn(t, repo, stream, masonAgent("resume")); again.Request.TurnID != turn.Request.TurnID {
		t.Fatalf("a second pass queued %s", again.Request.TurnID)
	}
	ruling, found, err := latestMasonRuling(repo, stream, "resume")
	must(t, err)
	if !found || ruling.ResetTurn != reported.Sequence {
		t.Fatalf("clean-turn reset %+v", ruling)
	}

	completeMasonTurn(t, f, repo, stream, "resume", resumeReport)
	must(t, m.Pass(ctx))
	if state := workflowOf(t, repo, stream, "resume"); state.Value != UnitChecking {
		t.Fatalf("the revised unit is %s, not checking", state.Value)
	}
}

// A unit moved into checking runs its checks again, even on a candidate
// whose checks already passed.
func TestMoveIntoCheckingRunsTheChecksAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repo := newReviewFixture(t, "move-recheck")
	f.s.setSole(&activeProject{repository: repo})
	if _, api := f.s.ownerMove(ctx, string(stream), "resume", UnitMoveRequest{To: UnitChecking, Note: "The engine was flaky; run them again."}); api != nil {
		t.Fatal(api.Message)
	}
	if state := workflowOf(t, repo, stream, "resume"); state.Value != UnitChecking {
		t.Fatalf("unit is %s", state.Value)
	}
	runChecks(t, f.s, repo, stream)
	runs, err := checkRuns(repo, stream)
	must(t, err)
	if len(runs) != 2 || runs[1].Run != 2 || runs[1].Status != ChecksPassed {
		t.Fatalf("runs %+v", runs)
	}
	if state := workflowOf(t, repo, stream, "resume"); state.Value != UnitReviewing {
		t.Fatalf("unit is %s after its checks ran again", state.Value)
	}
}

// A unit moved into reviewing gets a fresh review turn that carries the
// note, and a move that re-enters reviewing restarts the review.
func TestMoveIntoReviewingRestartsTheReviewWithTheNote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repo := newReviewFixture(t, "move-review")
	f.s.setSole(&activeProject{repository: repo})
	r := &reviewers{masons: newMasonController(f.s, repo)}
	must(t, r.one(ctx, stream, "resume", workflowOf(t, repo, stream, "resume"), false))
	first := lastTurn(t, repo, stream, reviewerAgent("resume"))
	note := "Judge only the acceptance; the footprint question is settled."
	if _, api := f.s.ownerMove(ctx, string(stream), "resume", UnitMoveRequest{To: UnitReviewing, Note: note}); api != nil {
		t.Fatal(api.Message)
	}
	state := workflowOf(t, repo, stream, "resume")
	must(t, r.one(ctx, stream, "resume", state, false))
	turn := lastTurn(t, repo, stream, reviewerAgent("resume"))
	if turn.Request.TurnID == first.Request.TurnID || turn.Request.TurnID != reviewTurnID("resume", state.Version) || !strings.Contains(turn.Request.Prompt, note) {
		t.Fatalf("review turn %s after the move: %s", turn.Request.TurnID, turn.Request.Prompt)
	}
}

// A unit moved to approved records an approval of its candidate that the
// foreman accepts for landing.
func TestMoveToApprovedRecordsAnApprovalTheForemanLands(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repo := newCheckingFixture(t, "move-approve")
	note := "The only failing check is the upstream flake; the change holds."
	out, api := f.s.ownerMove(ctx, string(stream), "resume", UnitMoveRequest{To: UnitApproved, Note: note})
	if api != nil {
		t.Fatal(api.Message)
	}
	if state := workflowOf(t, repo, stream, "resume"); state.Value != UnitApproved {
		t.Fatalf("unit is %s", state.Value)
	}
	review, result := approvedReview(t, repo, stream, "resume")
	if result.Verdict.Decision != "satisfactory" || !strings.Contains(result.Verdict.Summary, note) || result.Identity.Candidate.Revision != out.Move.Candidate || review.Actor != ownerActor {
		t.Fatalf("approval %+v", result)
	}
	lands := &foreman{masons: newMasonController(f.s, repo)}
	if reason := landingReason(t, lands, repo, stream, "resume"); reason != "" {
		t.Fatalf("the foreman refuses the approval: %s", reason)
	}
}

// A move needs a started unit that has not merged, a target state, a note,
// and no landing or rebase of the unit in flight.
func TestMoveRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repo := newCheckingFixture(t, "move-refusals")
	for _, c := range []struct {
		unit string
		req  UnitMoveRequest
		want string
	}{
		{"resume", UnitMoveRequest{To: UnitMerged, Note: "Land it."}, "a move requires a note and a state"},
		{"resume", UnitMoveRequest{To: UnitReviewing}, "a move requires a note and a state"},
		{"dedupe", UnitMoveRequest{To: UnitImplementing, Note: "Start."}, "only a started unit that has not merged moves"},
		{"missing", UnitMoveRequest{To: UnitImplementing, Note: "Start."}, "has no unit missing"},
	} {
		if _, api := f.s.ownerMove(ctx, string(stream), c.unit, c.req); api == nil || !strings.Contains(api.Message, c.want) {
			t.Fatalf("move %s %+v: %v", c.unit, c.req, api)
		}
	}
	if _, api := f.s.ownerMove(ctx, string(stream), "resume", UnitMoveRequest{To: UnitApproved, Note: "Holds."}); api != nil {
		t.Fatal(api.Message)
	}
	if _, api := f.s.ownerMove(ctx, string(stream), "resume", UnitMoveRequest{To: UnitApproved, Note: "Again."}); api == nil || !strings.Contains(api.Message, "already approved") {
		t.Fatalf("a second approval: %v", api)
	}
	lands := &foreman{masons: newMasonController(f.s, repo)}
	must(t, lands.Pass(ctx))
	if _, api := f.s.ownerMove(ctx, string(stream), "resume", UnitMoveRequest{To: UnitImplementing, Note: "Wait."}); api == nil || !strings.Contains(api.Message, "the foreman is landing unit resume") {
		t.Fatalf("a move during the landing: %v", api)
	}
}

// moveFixture is a reviewing unit and the move_unit and resolve_contested
// tools of a claimed chief-of-staff turn of its workstream.
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
	out, err := m.move.Handle(context.Background(), json.RawMessage(input))
	must(t, err)
	return string(out)
}

// Engineering recovery remains internal; explicit escalation and owner
// rulings retain their own attribution.
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
	if left, err := chiefRulingsLeft(m.repo, m.stream, "resume"); err != nil || left != -1 {
		t.Fatalf("rulings left after two moves: %d %v", left, err)
	}
	if out := m.moveUnit(t, `{"unit":"resume","to":"implementing","note":"Revise it."}`); !strings.Contains(out, `"recorded":true`) {
		t.Fatalf("a move past the limit was recorded: %s", out)
	}
	hold := "Checks pass but every review finds the same gap; I recommend amending the plan."
	if out := m.moveUnit(t, `{"unit":"resume","to":"contested","note":"`+hold+`"}`); !strings.Contains(out, `"recorded":true`) {
		t.Fatalf("move to contested: %s", out)
	}
	contest, contested, err := unitContest(m.repo, m.stream, "resume")
	must(t, err)
	if raised, note := m.raised(t, contest); !contested || !raised || note != hold {
		t.Fatalf("held unit raised %t with %q", raised, note)
	}
	if out := m.resolve(t, `{"unit":"resume","decision":"review","note":"Review it."}`); !strings.Contains(out, "moved to contested for the owner") {
		t.Fatalf("the chief of staff ruled on the owner's contest: %s", out)
	}
	if out := m.moveUnit(t, `{"unit":"resume","to":"reviewing","note":"Review it."}`); !strings.Contains(out, "the owner moves it") {
		t.Fatalf("the chief of staff moved the owner's contest: %s", out)
	}
	entry, ok, err := m.f.s.contestedEntry(m.repo, m.stream, "resume")
	must(t, err)
	if !ok || entry.Recommendation != hold || strings.Join(entry.Options, ",") != "review,revise" {
		t.Fatalf("inbox entry %+v", entry)
	}
	out, api := m.f.s.recordContestedRuling(ctx, m.repo, m.stream, "resume", ContestedRulingRequest{Decision: "revise", Note: "Close the gap the reviews keep finding."}, ownerActor)
	if api != nil {
		t.Fatal(api.Message)
	}
	if state := m.state(t); state.Value != UnitImplementing || out.Ruling.Contest != contest.ID {
		t.Fatalf("ruled unit is %s with %+v", state.Value, out.Ruling)
	}
	if left, err := chiefRulingsLeft(m.repo, m.stream, "resume"); err != nil || left != chiefContestLimit {
		t.Fatalf("rulings left after the owner's ruling: %d %v", left, err)
	}
	actions, err := chiefActions(m.repo, m.stream)
	must(t, err)
	if len(actions) != 4 || actions[0].Text != "Moved unit resume from reviewing to checking. Run the checks again." || actions[3].Text != "Held unit resume for you, from implementing: "+hold || actions[3].Turn != "events_1" {
		t.Fatalf("conversation actions %+v", actions)
	}
}

// In a turn answering the owner, the chief of staff records the move the
// owner asked for as the owner's, past its own limit.
func TestChiefOfStaffRecordsTheOwnersMove(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repo := newCheckingFixture(t, "chief-owner-move")
	_, err := repo.EnsureChiefOfStaff(ctx, stream, demoStart, serviceActor)
	must(t, err)
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_message_1", Revision: 1, Project: f.project, Workstream: stream, At: demoStart, Actor: ownerActor, Cause: "owner-message"},
		AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "message_1", Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "chief", Prompt: "Send resume back to the mason."})
	must(t, err)
	_, err = repo.ClaimTurn(ctx, stream, trace.ChiefOfStaff, "token-message", filepath.Join(t.TempDir(), "message"), demoStart)
	must(t, err)
	controls := &runtimeControls{}
	controls.service.Store(f.s)
	tool := controls.moveUnit(repo, coreadapter.Scope{Project: string(f.project), Workstream: string(stream), Role: trace.ChiefOfStaff, Thread: trace.ChiefOfStaff, Turn: "message_1"})
	out, err := tool.Handle(ctx, json.RawMessage(`{"unit":"resume","to":"implementing","note":"Send it back to the mason.","owner_decided":true}`))
	must(t, err)
	if !strings.Contains(string(out), `"by":"owner"`) {
		t.Fatalf("move_unit: %s", out)
	}
	transitions, err := trace.Read[trace.Transition](repo, stream)
	must(t, err)
	if moved := transitions[len(transitions)-1]; moved.Actor != ownerActor || moved.To != UnitImplementing {
		t.Fatalf("move %+v", moved)
	}
}
