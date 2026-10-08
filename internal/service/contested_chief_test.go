package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// contestFixture is a unit under review whose reviewer can be made to end a
// turn without a verdict, and a claimed chief-of-staff turn of its
// workstream.
type contestFixture struct {
	f      *shedFixture
	stream config.WorkstreamID
	repo   *trace.Repository
	r      *reviewers
	tool   coreadapter.Tool
}

func newContestFixture(t *testing.T, key string) *contestFixture {
	t.Helper()
	ctx := context.Background()
	f, stream, repo := newReviewFixture(t, key)
	f.s.setSole(&activeProject{repository: repo})
	_, err := repo.EnsureChiefOfStaff(ctx, stream, demoStart, serviceActor)
	must(t, err)
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_events_1", Revision: 1, Project: f.project, Workstream: stream, At: demoStart, Actor: serviceActor, Cause: "fixture"},
		AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "events_1", Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "chief", Prompt: "Unit resume is contested."})
	must(t, err)
	_, err = repo.ClaimTurn(ctx, stream, trace.ChiefOfStaff, "token-events", filepath.Join(t.TempDir(), "events"), demoStart)
	must(t, err)
	controls := &runtimeControls{}
	controls.service.Store(f.s)
	scope := coreadapter.Scope{Project: string(f.project), Workstream: string(stream), Role: trace.ChiefOfStaff, Thread: trace.ChiefOfStaff, Turn: "events_1"}
	return &contestFixture{f: f, stream: stream, repo: repo, r: &reviewers{masons: newMasonController(f.s, repo)}, tool: controls.resolveContested(repo, scope)}
}

func (c *contestFixture) state(t *testing.T) trace.WorkflowState {
	t.Helper()
	state, err := c.repo.Workflow(c.stream, trace.UnitSubject("resume"))
	must(t, err)
	return state
}

// contest queues a review turn, ends it without a verdict and returns the
// contest the next pass records.
func (c *contestFixture) contest(t *testing.T) trace.Transition {
	t.Helper()
	ctx := context.Background()
	must(t, c.r.one(ctx, c.stream, "resume", c.state(t), false))
	captureTurn(t, c.f, c.repo, c.stream, reviewerAgent("resume"), nil)
	must(t, c.r.one(ctx, c.stream, "resume", c.state(t), false))
	if state := c.state(t); state.Value != UnitContested {
		t.Fatalf("a review turn without a verdict left the unit %s", state.Value)
	}
	contest, contested, err := unitContest(c.repo, c.stream, "resume")
	must(t, err)
	if !contested || !strings.Contains(contest.Reason, "ended without recording a verdict") {
		t.Fatalf("contest %+v", contest)
	}
	return contest
}

func (c *contestFixture) resolve(t *testing.T, input string) string {
	t.Helper()
	out, err := c.tool.Handle(context.Background(), json.RawMessage(input))
	must(t, err)
	return string(out)
}

func (c *contestFixture) raised(t *testing.T, contest trace.Transition) (bool, string) {
	t.Helper()
	raised, note, err := contestRaised(c.repo, c.stream, "resume", contest)
	must(t, err)
	return raised, note
}

// A contested unit goes to the chief of staff first. Its ruling resumes the
// unit with its note in the next review turn, shows in the conversation, and
// never reaches the owner's inbox.
func TestChiefOfStaffResolvesAContestOnTheOwnersBehalf(t *testing.T) {
	t.Parallel()
	c := newContestFixture(t, "chief-resolves")
	contest := c.contest(t)
	if raised, _ := c.raised(t, contest); raised {
		t.Fatal("a contest the chief of staff has not seen was raised to the owner")
	}
	if out := c.resolve(t, `{"unit":"resume","decision":"revise","note":"Rebuild it."}`); !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "use review") {
		t.Fatalf("a ruling the contest does not take was recorded: %s", out)
	}
	if out := c.resolve(t, `{"unit":"resume","decision":"review","note":"Cite each criterion by its plan ID.","owner_decided":true}`); !strings.Contains(out, "only for a ruling the owner gave") {
		t.Fatalf("an event turn recorded an owner ruling: %s", out)
	}
	note := "Record a verdict citing spec#1 by its plan ID."
	out := c.resolve(t, `{"unit":"resume","decision":"review","note":"`+note+`"}`)
	if !strings.Contains(out, `"recorded":true`) || !strings.Contains(out, `"by":"chief of staff"`) {
		t.Fatalf("resolve_contested: %s", out)
	}
	state := c.state(t)
	transitions, err := trace.Read[trace.Transition](c.repo, c.stream)
	must(t, err)
	last := transitions[len(transitions)-1]
	if state.Value != UnitReviewing || last.Actor != chiefActor || !strings.Contains(last.Reason, "chief of staff ruled review") {
		t.Fatalf("resolved unit %s by %+v", state.Value, last)
	}
	if out := c.resolve(t, `{"unit":"resume","decision":"review","note":"Again."}`); !strings.Contains(out, "is not contested") {
		t.Fatalf("a resolved unit took another ruling: %s", out)
	}
	must(t, c.r.one(context.Background(), c.stream, "resume", state, false))
	th, err := c.repo.Thread(c.stream, reviewerAgent("resume"))
	must(t, err)
	if prompt := th.Turns[len(th.Turns)-1].Request.Prompt; th.Turns[len(th.Turns)-1].Request.TurnID != reviewTurnID("resume", state.Version) || !strings.Contains(prompt, note) || !strings.Contains(prompt, "chief of staff ruled review") {
		t.Fatalf("the resumed review lacks the ruling:\n%s", prompt)
	}
	actions, err := chiefActions(c.repo, c.stream)
	must(t, err)
	if len(actions) != 1 || actions[0].Kind != "action" || actions[0].Turn != "events_1" || actions[0].Text != "Resolved contested unit resume: its reviewer reviews the candidate again. "+note {
		t.Fatalf("conversation actions %+v", actions)
	}
}

// Engineering retries remain internal until the chief explicitly escalates.
func TestChiefOfStaffRaisesWhatItCannotResolve(t *testing.T) {
	t.Parallel()
	c := newContestFixture(t, "chief-raises")
	for i := range 3 {
		c.contest(t)
		if out := c.resolve(t, `{"unit":"resume","decision":"review","note":"Record a verdict."}`); !strings.Contains(out, `"recorded":true`) {
			t.Fatalf("ruling %d: %s", i+1, out)
		}
	}
	contest := c.contest(t)
	if raised, _ := c.raised(t, contest); raised {
		t.Fatal("engineering retries alone raised a contest to the owner")
	}

	escalation := "The reviewer ends every turn without a verdict; I recommend checking its profile."
	if out := c.resolve(t, `{"unit":"resume","decision":"escalate","note":"`+escalation+`"}`); !strings.Contains(out, `"recorded":true`) {
		t.Fatalf("escalate: %s", out)
	}
	if raised, note := c.raised(t, contest); !raised || note != escalation {
		t.Fatalf("escalated contest raised %t with %q", raised, note)
	}
	if out := c.resolve(t, `{"unit":"resume","decision":"escalate","note":"Twice."}`); !strings.Contains(out, "already decided") {
		t.Fatalf("a second decision on one contest was recorded: %s", out)
	}
	if _, api := c.f.s.recordContestedRuling(context.Background(), c.repo, c.stream, "resume", ContestedRulingRequest{Decision: "review", Note: "Try the other profile."}, ownerActor); api != nil {
		t.Fatal(api.Message)
	}
	if left, err := chiefRulingsLeft(c.repo, c.stream, "resume"); err != nil || left != chiefContestLimit {
		t.Fatalf("rulings left after the owner's ruling: %d %v", left, err)
	}
	actions, err := chiefActions(c.repo, c.stream)
	must(t, err)
	if len(actions) != 4 || actions[3].Text != "Raised contested unit resume to you: "+escalation {
		t.Fatalf("conversation actions %+v", actions)
	}
}

// The chief of staff's view shows a contested unit's contest, the rulings it
// takes and the tool calls the service refused its roles.
func TestChiefViewShowsWhyAUnitIsStuck(t *testing.T) {
	t.Parallel()
	c := newContestFixture(t, "chief-view")
	contest := c.contest(t)
	th, err := c.repo.Thread(c.stream, reviewerAgent("resume"))
	must(t, err)
	turn := th.Turns[len(th.Turns)-1]
	call := trace.ToolCall{Scope: coreadapter.Scope{Workstream: string(c.stream), Unit: "resume", Thread: reviewerAgent("resume"), Turn: turn.Request.TurnID, Role: reviewerRole}, Name: verdictTool, Effect: coreadapter.ToolMemory, State: "completed",
		Output: &trace.ToolPayload{Content: `{"recorded":false,"reason":"unit resume does not address criterion \"spec#1 / plan resume\""}`}}
	data, err := json.Marshal(call)
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "tool-refused-1", Revision: 1, Project: c.repo.Project(), Workstream: c.stream, Unit: "resume", At: time.Now(), Actor: trace.Actor{Kind: "agent", ID: reviewerAgent("resume")}, Cause: turn.Request.ID}
	must(t, c.repo.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: "tools/tool-refused-1.json", Content: string(data)}}))

	dir := t.TempDir()
	paths, err := stageChiefDocuments(c.repo, c.stream, dir)
	must(t, err)
	raw, err := os.ReadFile(filepath.Join(dir, "units", "resume", "activity.json"))
	must(t, err)
	var activity UnitActivity
	must(t, json.Unmarshal(raw, &activity))
	if !strings.Contains(strings.Join(paths, ","), "units") || activity.State != UnitContested || activity.Contest == nil || activity.Contest.ID != contest.ID || strings.Join(activity.Contest.Rulings, ",") != "review" || activity.Contest.ChiefRulingsLeft != chiefContestLimit {
		t.Fatalf("activity %s", raw)
	}
	var reviewed *ActivityTurn
	for i := range activity.Turns {
		if activity.Turns[i].Turn == turn.Request.TurnID {
			reviewed = &activity.Turns[i]
		}
	}
	if reviewed == nil || reviewed.Status != "idle" || len(reviewed.Refused) != 1 || reviewed.Refused[0].Tool != verdictTool || !strings.Contains(reviewed.Refused[0].Reason, "spec#1 / plan resume") {
		t.Fatalf("the reviewer's turn in the activity: %+v", reviewed)
	}
	if last := activity.Transitions[len(activity.Transitions)-1]; last.To != UnitContested || last.Reason != contest.Reason {
		t.Fatalf("latest transition %+v", last)
	}
	if _, err := os.Stat(filepath.Join(dir, "tools")); !os.IsNotExist(err) {
		t.Fatalf("the view holds the raw tool records: %v", err)
	}
}

func TestConversationPlacesActionsBetweenTurns(t *testing.T) {
	t.Parallel()
	at := func(minute int) time.Time { return demoStart.Add(time.Duration(minute) * time.Minute) }
	entries := []ConversationEntry{
		{Turn: "m1", Kind: "message", At: at(0)}, {Turn: "m2", Kind: "message", At: at(1)},
		{Turn: "m1", Kind: "response", At: at(2)}, {Turn: "m2", Kind: "response", At: at(5)},
	}
	actions := []ConversationEntry{{Turn: "e2", Kind: "action", At: at(6)}, {Turn: "e1", Kind: "action", At: at(3)}}
	var got []string
	for _, e := range withActions(entries, actions) {
		got = append(got, e.Turn+":"+e.Kind)
	}
	if want := "m1:message,m2:message,m1:response,e1:action,m2:response,e2:action"; strings.Join(got, ",") != want {
		t.Fatalf("conversation %v, want %s", got, want)
	}
}
