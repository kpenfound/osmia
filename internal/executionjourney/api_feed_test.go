package executionjourney

import (
	context "context"
	fmt "fmt"
	filepath "path/filepath"
	reflect "reflect"
	strings "strings"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	checkrun "github.com/kpenfound/osmia/internal/checks"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func TestFeedOrdersEverythingThatHappened(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, cfg := journey.ConversationFixture(t, "fd-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	defer repo.Close()
	at := func(minutes int) time.Time { return fixturekit.DemoStart.Add(time.Duration(minutes) * time.Minute) }
	header := func(schema, id string, minutes int) trace.Header {
		return trace.Header{Schema: schema, Version: trace.Version, ID: id, Revision: 1, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: at(minutes), Actor: intake.ServiceActor, Cause: "test"}
	}
	transition := func(subject, unit, to string, minutes int) {
		t.Helper()
		state, err := repo.Workflow(fixturekit.Stream, subject)
		fixturekit.Must(t, err)
		h := header("osmia.trace.transition", subject+"-"+to, minutes)
		h.Unit = unit
		_, err = repo.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: subject, From: state.Value, To: to, Reason: "because " + to}})
		fixturekit.Must(t, err)
	}
	finish := func(agent string, q trace.QueuedTurn, result coreadapter.SessionResult, minutes int) {
		t.Helper()
		h := q.Request.Header
		h.Schema, h.ID, h.At = "osmia.trace.turn-response", trace.EventID(q.Request.ID, "response"), at(minutes)
		result.Session = coreadapter.BackendSession{Backend: q.Request.Profile.Backend, ID: "session-" + agent}
		result.StartedAt, result.SessionDirectory = q.Claim.At, q.Claim.SessionDirectory
		fixturekit.Must(t, repo.CaptureTurn(ctx, q.Claim.Token, trace.TurnResponse{Header: h, AgentID: agent, ThreadID: agent, TurnID: q.Request.TurnID, RequestID: q.Request.ID, RequestRevision: q.Request.Revision, Result: result}))
		fixturekit.Must(t, repo.CompleteTurn(ctx, fixturekit.Stream, agent, q.Request.TurnID, q.Claim.Token, at(minutes)))
	}

	transition(trace.FeatureSubject, "", intake.BuildingState, 1)
	transition(trace.UnitSubject("upload"), "upload", intake.UnitReady, 2)
	transition(trace.QuestionSubject("q1"), "", "asked", 3)

	_, err = repo.EnsureManager(ctx, fixturekit.Stream, at(0), intake.ServiceActor)
	fixturekit.Must(t, err)
	managerProfile := coreadapter.Profile{Name: "chief", Backend: "fake", Model: "test"}
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: header("osmia.trace.turn-request", "request_events", 4), AgentID: trace.Manager, ThreadID: trace.Manager, TurnID: "events", Profile: managerProfile, Prompt: "Events"})
	fixturekit.Must(t, err)
	manager, err := repo.ClaimTurn(ctx, fixturekit.Stream, trace.Manager, "token_events", filepath.Join(t.TempDir(), "events"), at(5))
	fixturekit.Must(t, err)
	scope := coreadapter.Scope{Project: string(fixturekit.Project), Workstream: string(fixturekit.Stream), Thread: trace.Manager, Turn: "events", Role: trace.Manager}
	content := trace.StatusContent{Goal: "Ship uploads.", Attention: "Rule on the API.", Note: "Upload is ready.", Agents: []string{"A mason builds upload."}}
	_, err = repo.SetStatus(ctx, trace.Manager, scope, content, at(6), func(trace.StatusContent, []string, []trace.OwnerGate) error { return nil })
	fixturekit.Must(t, err)
	finish(trace.Manager, manager, coreadapter.SessionResult{FinalResponse: "Status written."}, 7)

	fixturekit.QueueRoleTurn(t, repo, fixturekit.Stream, "agent_mason_upload", intake.MasonRole, "upload", at(8))
	mason, err := repo.ClaimTurn(ctx, fixturekit.Stream, "agent_mason_upload", "token_upload", filepath.Join(t.TempDir(), "upload"), at(9))
	fixturekit.Must(t, err)
	transition(trace.UnitSubject("upload"), "upload", intake.UnitImplementing, 10)
	fixturekit.QueueRoleTurn(t, repo, fixturekit.Stream, "agent_mason_audit", intake.MasonRole, "audit", at(11))
	finish("agent_mason_upload", mason, coreadapter.SessionResult{FinalResponse: "Long answer", Outcome: &coreadapter.Outcome{Status: execution.MasonDone, Report: "Built upload."}}, 12)

	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_owner", Revision: 1, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: at(13), Actor: intake.OwnerActor, Cause: "owner-message"},
		AgentID: trace.Manager, ThreadID: trace.Manager, TurnID: "message_owner", Profile: managerProfile, Prompt: "How is it going?"})
	fixturekit.Must(t, err)

	transition(execution.ChecksSubject("upload"), "upload", "requested-1", 14)
	selection := api.CheckSelection{Mode: checkrun.SelectionSelected, Links: []string{"dag://upload"}, Candidates: 113, Judgment: "judgment_feed"}
	fixturekit.FeedDocument(t, repo, fixturekit.Stream, execution.CheckSelectionDocument("upload", 1), execution.CheckSelectionPath("upload", 1), 1, at(15), execution.UnitCheckSelection{Unit: "upload", Run: 1, Selection: selection, StartedAt: at(15)})
	fixturekit.FeedDocument(t, repo, fixturekit.Stream, execution.CheckDocumentID("upload", 1), "units/upload/checks-1.json", 1, at(16), execution.UnitCheckRun{Unit: "upload", Run: 1, Status: checkrun.ChecksPassed, Selection: &selection, StartedAt: at(15), FinishedAt: at(16)})
	transition(delivery.FinalReviewSubject, "", "requested-1", 17)
	fixturekit.FeedDocument(t, repo, fixturekit.Stream, delivery.FinalChecksDocument(1), delivery.FinalChecksPath(1), 1, at(19), delivery.FinalCheckRun{Review: 1, Status: checkrun.ChecksPassed, StartedAt: at(18), FinishedAt: at(19)})
	fixturekit.QueueRoleTurn(t, repo, fixturekit.Stream, "agent_final_reader", intake.CommitteeRole, "", at(19))
	reader, err := repo.ClaimTurn(ctx, fixturekit.Stream, "agent_final_reader", "token_reader", filepath.Join(t.TempDir(), "reader"), at(20))
	fixturekit.Must(t, err)
	finish("agent_final_reader", reader, coreadapter.SessionResult{FinalResponse: "Reviewed the assembled branch."}, 21)
	fixturekit.FeedDocument(t, repo, fixturekit.Stream, delivery.FinalReportDocument, delivery.FinalReportPath, 1, at(22), api.FinalReport{Review: 1, Outcome: delivery.FinalReviewed, Reader: "agent_final_reader", Turn: "agent_final_reader-turn"})
	transition(delivery.FinalReviewSubject, "", "reviewed-1", 22)

	got, err := service.FeedEntries(repo, repo, fixturekit.Stream)
	fixturekit.Must(t, err)
	managerEnded, masonEnded, unitFinished, finalFinished, readerEnded := at(7), at(12), at(16), at(19), at(21)
	want := []api.FeedEntry{
		{Kind: "transition", At: at(1), Transition: &api.FeedTransition{Subject: trace.FeatureSubject, From: "", To: intake.BuildingState, Actor: "service:osmia", Reason: "because " + intake.BuildingState}},
		{Kind: "transition", At: at(2), Transition: &api.FeedTransition{Subject: trace.UnitSubject("upload"), Unit: "upload", From: "", To: intake.UnitReady, Actor: "service:osmia", Reason: "because " + intake.UnitReady}},
		{Kind: "transition", At: at(3), Transition: &api.FeedTransition{Subject: trace.QuestionSubject("q1"), From: "", To: "asked", Actor: "service:osmia", Reason: "because asked"}},
		{Kind: "session", At: at(5), Session: &api.FeedSession{Agent: trace.ManagerDisplay, Role: trace.ManagerDisplay, Turn: "events", Profile: "chief", State: "done", StartedAt: at(5), EndedAt: &managerEnded, Summary: "Status written."}},
		{Kind: "status", At: at(6), Status: &api.StatusView{Goal: "Ship uploads.", Attention: "Rule on the API.", Note: "Upload is ready.", Agents: []string{"A mason builds upload."}, Revision: 1, UpdatedAt: at(6)}},
		{Kind: "session", At: at(9), Session: &api.FeedSession{Agent: "agent_mason_upload", Role: intake.MasonRole, Unit: "upload", Turn: "agent_mason_upload-turn", Profile: "default", State: "done", StartedAt: at(9), EndedAt: &masonEnded, Summary: "Built upload."}},
		{Kind: "transition", At: at(10), Transition: &api.FeedTransition{Subject: trace.UnitSubject("upload"), Unit: "upload", From: intake.UnitReady, To: intake.UnitImplementing, Actor: "service:osmia", Reason: "because " + intake.UnitImplementing}},
		{Kind: "message", At: at(13), Turn: "message_owner", Text: "How is it going?", State: api.TurnQueued, QueuedReason: "starting"},

		{Kind: "transition", At: at(14), Transition: &api.FeedTransition{Subject: execution.ChecksSubject("upload"), Unit: "upload", From: "", To: "requested-1", Actor: "service:osmia", Reason: "because requested-1"}},
		{Kind: "unit_checks", At: at(15), UnitChecks: &api.FeedUnitChecks{Unit: "upload", Run: 1, State: checkrun.ChecksPassed, Selection: &api.FeedCheckSelection{Mode: checkrun.SelectionSelected, Selected: 1, Judged: 113, Description: "Jev selected 1 of 113 check links for the changed files in judgment judgment_feed"}, StartedAt: at(15), FinishedAt: &unitFinished}},
		{Kind: "transition", At: at(17), Transition: &api.FeedTransition{Subject: delivery.FinalReviewSubject, From: "", To: "requested-1", Actor: "service:osmia", Reason: "because requested-1"}},
		{Kind: "final_checks", At: at(19), FinalChecks: &api.FeedFinalChecks{Review: 1, State: checkrun.ChecksPassed, FinishedAt: &finalFinished}},
		{Kind: "session", At: at(20), Session: &api.FeedSession{Agent: "agent_final_reader", Role: intake.CommitteeRole, Turn: "agent_final_reader-turn", Profile: "default", State: "done", StartedAt: at(20), EndedAt: &readerEnded, Summary: "Reviewed the assembled branch."}},
		{Kind: "transition", At: at(22), Transition: &api.FeedTransition{Subject: delivery.FinalReviewSubject, From: "requested-1", To: "reviewed-1", Actor: "service:osmia", Reason: "because reviewed-1"}},
		{Kind: "final_review", At: at(22), FinalReview: &api.FeedFinalReview{Review: 1, State: delivery.FinalReviewed, Reader: "agent_final_reader", Turn: "agent_final_reader-turn"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("feed:\n%s\nwant:\n%s", describeFeed(got), describeFeed(want))
	}
}

func describeFeed(entries []api.FeedEntry) string {
	var out strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&out, "%s %s %s %s %s", e.At.Format(time.TimeOnly), e.Kind, e.Turn, e.State, e.Text)
		switch {
		case e.Session != nil:
			fmt.Fprintf(&out, " %+v", *e.Session)
			if e.Session.EndedAt != nil {
				fmt.Fprintf(&out, " ended %s", e.Session.EndedAt.Format(time.TimeOnly))
			}
		case e.Status != nil:
			fmt.Fprintf(&out, " %+v", *e.Status)
		case e.Transition != nil:
			fmt.Fprintf(&out, " %+v", *e.Transition)
		case e.UnitChecks != nil:
			fmt.Fprintf(&out, " %+v", *e.UnitChecks)
		case e.FinalChecks != nil:
			fmt.Fprintf(&out, " %+v", *e.FinalChecks)
		case e.FinalReview != nil:
			fmt.Fprintf(&out, " %+v", *e.FinalReview)
		}
		out.WriteString("\n")
	}
	return out.String()
}
