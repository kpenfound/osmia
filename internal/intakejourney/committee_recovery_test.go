package intakejourney

import (
	context "context"
	json "encoding/json"
	errors "errors"
	fmt "fmt"
	os "os"
	filepath "path/filepath"
	slices "slices"
	strings "strings"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	questions "github.com/kpenfound/osmia/internal/questions"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	shed "github.com/kpenfound/osmia/internal/shed"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

var committeeProfile = coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}

func completeClaimed(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, agent string, q trace.QueuedTurn, at time.Time) {
	t.Helper()
	ctx := context.Background()
	h := q.Request.Header
	h.Schema, h.ID, h.At = "osmia.trace.turn-response", trace.EventID(q.Request.ID, "response"), at
	response := trace.TurnResponse{Header: h, AgentID: agent, ThreadID: q.Request.ThreadID, TurnID: q.Request.TurnID, RequestID: q.Request.ID, RequestRevision: q.Request.Revision,
		Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: q.Request.Profile.Backend, ID: q.Claim.Token}, SessionDirectory: q.Claim.SessionDirectory, StartedAt: q.Claim.At}}
	fixturekit.Must(t, repo.CaptureTurn(ctx, q.Claim.Token, response))
	fixturekit.Must(t, repo.CompleteTurn(ctx, stream, agent, q.Request.TurnID, q.Claim.Token, at))
}

func plantEndedPlanRoundAnswer(t *testing.T, f *journey.ShedFixture, stream config.WorkstreamID, member string) string {
	t.Helper()
	ctx := context.Background()
	f.Stop(t)
	repo, err := trace.Open(f.S.Current().Root, f.S.Current().Project)
	fixturekit.Must(t, err)
	at := f.Clock.Now()
	th, err := repo.Thread(stream, member)
	if errors.Is(err, os.ErrNotExist) {
		fixturekit.Must(t, repo.CreateThread(ctx, trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, ID: member, Revision: 1, Project: f.Project, Workstream: stream, At: at, Actor: intake.ShedActor, Cause: "committee-thread"}, Role: intake.CommitteeRole, ThreadID: intake.CommitteeThread(1)}))
		th, err = repo.Thread(stream, member)
	}
	fixturekit.Must(t, err)
	asking := intake.RoundTurnID(1, member, 7)
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + asking, Revision: 1, Project: f.Project, Workstream: stream, At: at, Actor: intake.ShedActor, Cause: "fixture", Depth: 1},
		AgentID: member, ThreadID: th.Identity.ThreadID, TurnID: asking, Profile: committeeProfile, SystemPrompt: "committee", Prompt: "Debate round 1."})
	fixturekit.Must(t, err)
	claimed, err := repo.ClaimTurn(ctx, stream, member, "token-"+asking, filepath.Join(t.TempDir(), asking), at)
	fixturekit.Must(t, err)
	if claimed.Request.TurnID != asking {
		t.Fatalf("claimed %s, want %s", claimed.Request.TurnID, asking)
	}
	question, err := repo.Ask(ctx, member, coreadapter.Scope{Project: string(f.Project), Workstream: string(stream), Role: intake.CommitteeRole, Thread: th.Identity.ThreadID, Turn: asking}, "Should the evaluator skill load on the climb steps?", at)
	fixturekit.Must(t, err)
	completeClaimed(t, repo, stream, member, claimed, at)

	_, err = repo.EnsureManager(ctx, stream, at, intake.ServiceActor)
	fixturekit.Must(t, err)
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_events_answer", Revision: 1, Project: f.Project, Workstream: stream, At: at, Actor: intake.ServiceActor, Cause: "fixture"},
		AgentID: trace.Manager, ThreadID: trace.Manager, TurnID: "events_answer", Profile: committeeProfile, SystemPrompt: "manager", Prompt: "Answer question " + question.ID})
	fixturekit.Must(t, err)
	manager, err := repo.ClaimTurn(ctx, stream, trace.Manager, "token-events-answer", filepath.Join(t.TempDir(), "manager"), at)
	fixturekit.Must(t, err)
	_, err = repo.AnswerQuestion(ctx, trace.Manager, coreadapter.Scope{Project: string(f.Project), Workstream: string(stream), Role: trace.Manager, Thread: trace.Manager, Turn: manager.Request.TurnID}, question.ID, "Load it on the evaluator steps.", []string{"spec#1"}, at)
	fixturekit.Must(t, err)
	completeClaimed(t, repo, stream, trace.Manager, manager, at)
	asked, err := repo.Questions(stream)
	fixturekit.Must(t, err)
	i := slices.IndexFunc(asked, func(q trace.QuestionState) bool { return q.Asked.ID == question.ID })
	if i < 0 {
		t.Fatalf("question %s is not recorded", question.ID)
	}
	delivered, err := questions.Deliver(ctx, repo, asked[i], func(string, string) (coreadapter.Profile, error) { return committeeProfile, nil }, at)
	fixturekit.Must(t, err)
	if !delivered {
		t.Fatal("the answer was not queued on the member's thread")
	}
	fixturekit.Must(t, repo.Close())
	f.Start(t)
	return questions.TurnID(question.ID)
}

func TestAnAnswerAfterItsPlanRoundEndedDoesNotHoldTheCommitteeMember(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream := journey.BuiltForAmendment(t, 2)
	defer f.Stop(t)
	member := intake.CommitteeAgent(1)
	round := fmt.Sprintf("amend-1-round-2-%s-1", member)
	f.Script(round, nil, nil)
	f.Script("amend-1-round-2-reply-1", nil, nil)
	journey.PresentAmendment(t, f, stream, intake.MasonRole, intakefixture.AmendedSpec, fixturekit.ValidPlan, intakefixture.AmendmentObjection(shed.Fit))
	answer := plantEndedPlanRoundAnswer(t, f, stream, member)

	_, err := f.C.DecideAmendment(ctx, stream, "1", api.AmendmentDecisionRequest{Decision: intake.AmendmentRound, Note: "Debate the fit once more.", Packet: 1})
	fixturekit.Must(t, err)
	f.AwaitAmendment(t, stream, intake.AmendmentPresented)
	if runs := f.Runs(); !slices.Contains(runs, round) || slices.Contains(runs, answer) {
		t.Fatalf("turns %v", runs)
	}
	th, err := f.Repository().Thread(stream, member)
	fixturekit.Must(t, err)
	i := slices.IndexFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == answer })
	if i < 0 {
		t.Fatalf("the member's thread lost the answer turn: %+v", th.Turns)
	}
	stale := th.Turns[i]
	if stale.CompletedAt.IsZero() || stale.Response == nil || !stale.Response.Result.Cancelled || !strings.Contains(stale.Response.Failure, "the plan debate ended") || !strings.Contains(stale.Response.Failure, intake.RoundTurnID(1, member, 7)) {
		t.Fatalf("the stale answer turn %+v", stale)
	}
}

func askingAmendmentRound(t *testing.T) (*journey.ShedFixture, config.WorkstreamID, string) {
	t.Helper()
	f, stream := journey.BuiltForAmendment(t, 2)
	t.Cleanup(func() { f.Stop(t) })
	member := intake.CommitteeAgent(1)
	f.Script(fmt.Sprintf("amend-1-round-2-%s-1", member), nil, func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		_, err := fixturekit.CallTool(ctx, tools, questions.AskTool, map[string]any{"question": "Does the checkpoint survive a restart of the uploader?"})
		return err
	})
	requester := journey.PresentAmendment(t, f, stream, intake.MasonRole, intakefixture.AmendedSpec, fixturekit.ValidPlan, intakefixture.AmendmentObjection(shed.Fit))
	_, err := f.C.DecideAmendment(context.Background(), stream, "1", api.AmendmentDecisionRequest{Decision: intake.AmendmentRound, Packet: 1})
	fixturekit.Must(t, err)
	f.S.Until(t, "the committee member to ask in round 2", func() bool {
		asked, err := f.Repository().Questions(stream)
		return err == nil && slices.ContainsFunc(asked, func(q trace.QuestionState) bool { return q.Asked.AskedBy.ID == member })
	})
	return f, stream, requester
}

func TestTheOwnerWithdrawsAnAmendmentWhoseDebateCannotFinish(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, requester := askingAmendmentRound(t)
	state, err := f.Repository().Workflow(stream, intake.AmendmentSubject("1"))
	fixturekit.Must(t, err)
	if state.Value != "debating" {
		t.Fatalf("amendment 1 is %s while its round waits", state.Value)
	}
	if _, err := f.C.DecideAmendment(ctx, stream, "1", api.AmendmentDecisionRequest{Decision: intake.AmendmentReject, Packet: 2}); !intakefixture.Failed(err, api.Conflict) || !strings.Contains(err.Error(), "decision withdraw") {
		t.Fatalf("rejecting an amendment in debate: %v", err)
	}
	out, err := f.C.DecideAmendment(ctx, stream, "1", api.AmendmentDecisionRequest{Decision: intake.AmendmentWithdraw, Note: "The round waits on a question nobody needs answered."})
	fixturekit.Must(t, err)
	if out.State != intake.AmendmentRejected || out.Decision == nil || out.Decision.Decision != intake.AmendmentWithdraw {
		t.Fatalf("withdrawal %+v", out)
	}
	withdrawn := f.Transition(t, stream, intake.AmendmentDecided("1", 2))
	if withdrawn.From != "debating" || withdrawn.To != intake.AmendmentRejected || withdrawn.Actor != intake.OwnerActor || !strings.Contains(withdrawn.Reason, "the owner withdrew amendment 1 while it was debating") {
		t.Fatalf("withdrawal transition %+v", withdrawn)
	}
	f.AwaitAmendment(t, stream, intake.AmendmentRuled)
	turns := f.Ruling(t, stream, requester)
	if len(turns) != 1 || !strings.Contains(turns[0].Request.Prompt, "Decision: withdraw") || !strings.Contains(turns[0].Request.Prompt, "| The round waits on a question nobody needs answered.") {
		t.Fatalf("ruling %+v", turns)
	}
	f.Resumed(t, stream, intake.UnitImplementing)
	again, err := f.C.DecideAmendment(ctx, stream, "1", api.AmendmentDecisionRequest{Decision: intake.AmendmentWithdraw})
	fixturekit.Must(t, err)
	if again.State != intake.AmendmentRuled || !strings.Contains(again.Detail, "already withdrawn") {
		t.Fatalf("second withdrawal %+v", again)
	}
	if got := f.Revisions(t, stream, intake.AmendmentDecisionID("1")); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("decision revisions %v", got)
	}
}

func TestTheManagerWithdrawsAnAmendmentWhoseDebateCannotFinish(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, _ := askingAmendmentRound(t)
	f.Stop(t)
	repo, err := trace.Open(f.S.Current().Root, f.S.Current().Project)
	fixturekit.Must(t, err)
	defer func() { _ = repo.Close() }()
	f.S.SetSole(&journey.ActiveProject{Repository: repo})
	_, err = repo.EnsureManager(ctx, stream, fixturekit.DemoStart, intake.ServiceActor)
	fixturekit.Must(t, err)
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_events_withdraw", Revision: 1, Project: f.Project, Workstream: stream, At: fixturekit.DemoStart, Actor: intake.ServiceActor, Cause: "fixture"},
		AgentID: trace.Manager, ThreadID: trace.Manager, TurnID: "events_withdraw", Profile: committeeProfile, SystemPrompt: "manager", Prompt: "Amendment 1 is stalled in debate."})
	fixturekit.Must(t, err)
	claimed, err := repo.ClaimTurn(ctx, stream, trace.Manager, "token-withdraw", filepath.Join(t.TempDir(), "withdraw"), fixturekit.DemoStart)
	fixturekit.Must(t, err)
	controls := &journey.RuntimeControls{}
	controls.Service.Store(f.S)
	scope := coreadapter.Scope{Project: string(f.Project), Workstream: string(stream), Role: trace.Manager, Thread: trace.Manager, Turn: claimed.Request.TurnID}
	tool := controls.DecideAmendment(repo, scope)
	if out, err := tool.Handle(ctx, json.RawMessage(`{"amendment":"1","decision":"withdraw"}`)); err != nil || !strings.Contains(string(out), `"recorded":false`) || !strings.Contains(string(out), "needs a note") {
		t.Fatalf("withdrawal without a reason: %s %v", out, err)
	}
	out, err := tool.Handle(ctx, json.RawMessage(`{"amendment":"1","decision":"withdraw","note":"Its debate round waits on a question that blocks the final review."}`))
	fixturekit.Must(t, err)
	var result struct {
		Recorded bool   `json:"recorded"`
		State    string `json:"state"`
		Detail   string `json:"detail"`
	}
	fixturekit.Must(t, json.Unmarshal(out, &result))
	if !result.Recorded || result.State != intake.AmendmentRejected || !strings.Contains(result.Detail, "the chief of staff withdrew amendment 1") {
		t.Fatalf("chief withdrawal: %s", out)
	}
	docs, err := trace.Read[trace.Document](repo, stream)
	fixturekit.Must(t, err)
	i := slices.IndexFunc(docs, func(d trace.Document) bool { return d.ID == intake.AmendmentDecisionID("1") && d.Revision == 2 })
	if i < 0 || docs[i].Actor.ID != trace.Manager || !strings.Contains(docs[i].Content, `"decision": "withdraw"`) {
		t.Fatalf("decision record %+v", docs)
	}
}
