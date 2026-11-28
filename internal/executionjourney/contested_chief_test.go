package executionjourney

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestChiefOfStaffResolvesAContestOnTheOwnersBehalf(t *testing.T) {
	t.Parallel()
	c := journey.NewContestFixture(t, "chief-resolves")
	contest := c.Contest(t)
	if raised, _ := c.Raised(t, contest); raised {
		t.Fatal("a contest the chief of staff has not seen was raised to the owner")
	}
	if out := c.Resolve(t, `{"unit":"resume","decision":"revise","note":"Rebuild it."}`); !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "use review") {
		t.Fatalf("a ruling the contest does not take was recorded: %s", out)
	}
	if out := c.Resolve(t, `{"unit":"resume","decision":"review","note":"Cite each criterion by its plan ID.","owner_decided":true}`); !strings.Contains(out, "only for a ruling the owner gave") {
		t.Fatalf("an event turn recorded an owner ruling: %s", out)
	}
	note := "Record a verdict citing spec#1 by its plan ID."
	out := c.Resolve(t, `{"unit":"resume","decision":"review","note":"`+note+`"}`)
	if !strings.Contains(out, `"recorded":true`) || !strings.Contains(out, `"by":"chief of staff"`) {
		t.Fatalf("resolve_contested: %s", out)
	}
	state := intakefixture.UnitWorkflow(t, c.Repo, c.Stream, "resume")
	transitions := intakefixture.WorkTransitions(t, c.Repo, c.Stream)
	last := transitions[len(transitions)-1]
	if state.Value != intake.UnitReviewing || last.Actor != intake.ChiefActor || !strings.Contains(last.Reason, "chief of staff ruled review") {
		t.Fatalf("resolved unit %s by %+v", state.Value, last)
	}
	if out := c.Resolve(t, `{"unit":"resume","decision":"review","note":"Again."}`); !strings.Contains(out, "is not contested") {
		t.Fatalf("a resolved unit took another ruling: %s", out)
	}
	fixturekit.Must(t, c.R.One(context.Background(), c.Stream, "resume", state, false))
	th, err := c.Repo.Thread(c.Stream, intake.ReviewerAgent("resume"))
	fixturekit.Must(t, err)
	if prompt := th.Turns[len(th.Turns)-1].Request.Prompt; th.Turns[len(th.Turns)-1].Request.TurnID != execution.ReviewTurnID("resume", state.Version) || !strings.Contains(prompt, note) || !strings.Contains(prompt, "chief of staff ruled review") {
		t.Fatalf("the resumed review lacks the ruling:\n%s", prompt)
	}
	actions, err := service.ChiefActions(c.Repo, c.Stream)
	fixturekit.Must(t, err)
	if len(actions) != 1 || actions[0].Kind != "action" || actions[0].Turn != "events_1" || actions[0].Text != "Resolved contested unit resume: its reviewer reviews the candidate again. "+note {
		t.Fatalf("conversation actions %+v", actions)
	}
}

func TestChiefOfStaffRaisesWhatItCannotResolve(t *testing.T) {
	t.Parallel()
	c := journey.NewContestFixture(t, "chief-raises")
	for i := range 3 {
		c.Contest(t)
		if out := c.Resolve(t, `{"unit":"resume","decision":"review","note":"Record a verdict."}`); !strings.Contains(out, `"recorded":true`) {
			t.Fatalf("ruling %d: %s", i+1, out)
		}
	}
	contest := c.Contest(t)
	if raised, _ := c.Raised(t, contest); raised {
		t.Fatal("engineering retries alone raised a contest to the owner")
	}

	escalation := "The reviewer ends every turn without a verdict; I recommend checking its profile."
	if out := c.Resolve(t, `{"unit":"resume","decision":"escalate","note":"`+escalation+`"}`); !strings.Contains(out, `"recorded":true`) {
		t.Fatalf("escalate: %s", out)
	}
	if raised, note := c.Raised(t, contest); !raised || note != escalation {
		t.Fatalf("escalated contest raised %t with %q", raised, note)
	}
	if out := c.Resolve(t, `{"unit":"resume","decision":"escalate","note":"Twice."}`); !strings.Contains(out, "already decided") {
		t.Fatalf("a second decision on one contest was recorded: %s", out)
	}
	if _, apiErr := recordContestedRuling(c.F.S, context.Background(), c.Repo, c.Stream, "resume", api.ContestedRulingRequest{Decision: "review", Note: "Try the other profile."}, intake.OwnerActor); apiErr != nil {
		t.Fatal(apiErr.Message)
	}
	if left, err := execution.ManagerRulingsLeft(c.Repo, c.Stream, "resume"); err != nil || left != execution.ManagerContestLimit {
		t.Fatalf("rulings left after the owner's ruling: %d %v", left, err)
	}
	actions, err := service.ChiefActions(c.Repo, c.Stream)
	fixturekit.Must(t, err)
	if len(actions) != 4 || actions[3].Text != "Raised contested unit resume to you: "+escalation {
		t.Fatalf("conversation actions %+v", actions)
	}
}

func TestChiefViewShowsWhyAUnitIsStuck(t *testing.T) {
	t.Parallel()
	c := journey.NewContestFixture(t, "chief-view")
	contest := c.Contest(t)
	th, err := c.Repo.Thread(c.Stream, intake.ReviewerAgent("resume"))
	fixturekit.Must(t, err)
	turn := th.Turns[len(th.Turns)-1]
	call := trace.ToolCall{Scope: coreadapter.Scope{Workstream: string(c.Stream), Unit: "resume", Thread: intake.ReviewerAgent("resume"), Turn: turn.Request.TurnID, Role: intake.ReviewerRole}, Name: execution.VerdictTool, Effect: coreadapter.ToolMemory, State: "completed",
		Output: &trace.ToolPayload{Content: `{"recorded":false,"reason":"unit resume does not address criterion \"spec#1 / plan resume\""}`}}
	data, err := json.Marshal(call)
	fixturekit.Must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "tool-refused-1", Revision: 1, Project: c.Repo.Project(), Workstream: c.Stream, Unit: "resume", At: time.Now(), Actor: trace.Actor{Kind: "agent", ID: intake.ReviewerAgent("resume")}, Cause: turn.Request.ID}
	fixturekit.Must(t, c.Repo.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: "tools/tool-refused-1.json", Content: string(data)}}))

	dir := t.TempDir()
	paths, err := service.StageChiefDocuments(c.Repo, c.Stream, dir)
	fixturekit.Must(t, err)
	raw, err := os.ReadFile(filepath.Join(dir, "units", "resume", "activity.json"))
	fixturekit.Must(t, err)
	var activity api.UnitActivity
	fixturekit.Must(t, json.Unmarshal(raw, &activity))
	if !strings.Contains(strings.Join(paths, ","), "units") || activity.State != intake.UnitContested || activity.Contest == nil || activity.Contest.ID != contest.ID || strings.Join(activity.Contest.Rulings, ",") != "review" || activity.Contest.ChiefRulingsLeft != execution.ManagerContestLimit {
		t.Fatalf("activity %s", raw)
	}
	var reviewed *api.ActivityTurn
	for i := range activity.Turns {
		if activity.Turns[i].Turn == turn.Request.TurnID {
			reviewed = &activity.Turns[i]
		}
	}
	if reviewed == nil || reviewed.Status != "idle" || len(reviewed.Refused) != 1 || reviewed.Refused[0].Tool != execution.VerdictTool || !strings.Contains(reviewed.Refused[0].Reason, "spec#1 / plan resume") {
		t.Fatalf("the reviewer's turn in the activity: %+v", reviewed)
	}
	if last := activity.Transitions[len(activity.Transitions)-1]; last.To != intake.UnitContested || last.Reason != contest.Reason {
		t.Fatalf("latest transition %+v", last)
	}
	if _, err := os.Stat(filepath.Join(dir, "tools")); !os.IsNotExist(err) {
		t.Fatalf("the view holds the raw tool records: %v", err)
	}
}

func TestConversationPlacesActionsBetweenTurns(t *testing.T) {
	t.Parallel()
	at := func(minute int) time.Time { return fixturekit.DemoStart.Add(time.Duration(minute) * time.Minute) }
	entries := []api.ConversationEntry{
		{Turn: "m1", Kind: "message", At: at(0)}, {Turn: "m2", Kind: "message", At: at(1)},
		{Turn: "m1", Kind: "response", At: at(2)}, {Turn: "m2", Kind: "response", At: at(5)},
	}
	actions := []api.ConversationEntry{{Turn: "e2", Kind: "action", At: at(6)}, {Turn: "e1", Kind: "action", At: at(3)}}
	var got []string
	for _, e := range service.WithActions(entries, actions) {
		got = append(got, e.Turn+":"+e.Kind)
	}
	if want := "m1:message,m2:message,m1:response,e1:action,m2:response,e2:action"; strings.Join(got, ",") != want {
		t.Fatalf("conversation %v, want %s", got, want)
	}
}
