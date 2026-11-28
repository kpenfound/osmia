package trace

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

func askerThreadOn(t *testing.T, r *Repository, stream config.WorkstreamID, agent, role, turn string) coreadapter.Scope {
	t.Helper()
	ctx := context.Background()
	h := Header{Schema: "osmia.trace.agent", Version: Version, ID: agent, Revision: 1, Project: projectID, Workstream: stream, At: at, Actor: Actor{Kind: "service", ID: "controller"}, Cause: "owner-message-1", Depth: 2}
	if err := r.CreateThread(ctx, Agent{Header: h, Role: role, ThreadID: agent + "_thread"}); err != nil {
		t.Fatal(err)
	}
	reqHeader := h
	reqHeader.Schema, reqHeader.ID, reqHeader.Unit = "osmia.trace.turn-request", "request_"+turn, "unit1"
	req := TurnRequest{Header: reqHeader, AgentID: agent, ThreadID: agent + "_thread", TurnID: turn,
		Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Build the unit"}
	if _, err := r.EnqueueTurn(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ClaimTurn(ctx, stream, agent, "token_"+turn, "/owned/"+turn, at); err != nil {
		t.Fatal(err)
	}
	return coreadapter.Scope{Project: string(projectID), Workstream: string(stream), Unit: "unit1", Thread: agent + "_thread", Turn: turn, Role: role}
}

func TestConsolidatedEscalationPausesOnlyItsMembersWhileAThirdContinues(t *testing.T) {
	r, _, _ := create(t)
	ctx := context.Background()
	if err := r.CreateWorkstream(ctx, secondStream, at, owner); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateWorkstream(ctx, thirdStream, at, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateWorkgroup(ctx, r, []config.WorkstreamID{streamID, secondStream, thirdStream}, at, owner, "test-workgroup"); err != nil {
		t.Fatal(err)
	}

	firstAsk := at
	secondAsk := at.Add(time.Second)
	thirdAsk := at.Add(2 * time.Second)
	askerA := askerThread(t, r, "agent_a", "mason", "build_a")
	if _, err := r.Ask(ctx, "agent_a", askerA, "Which store holds the upload index?", firstAsk); err != nil {
		t.Fatal(err)
	}
	askerB := askerThreadOn(t, r, secondStream, "agent_b", "mason", "build_b")
	if _, err := r.Ask(ctx, "agent_b", askerB, "Does the index rebuild on start?", secondAsk); err != nil {
		t.Fatal(err)
	}
	askerC := askerThreadOn(t, r, thirdStream, "agent_c", "reviewer", "review_c")
	if _, err := r.Ask(ctx, "agent_c", askerC, "Is the log format fixed?", thirdAsk); err != nil {
		t.Fatal(err)
	}

	manager := managerThread(t, r, "chief_turn")
	managerAt := at.Add(time.Minute)

	batch, err := r.EscalateQuestions(ctx, "chief", manager, EscalationRequest{
		Questions: []string{"1", string(secondStream) + ":1"}, Rephrasing: "Where does the upload index live, and does it rebuild on start?",
		Section: "spec#3", Conflict: "The spec says the index is derived, but doesn't say whether a restart may recompute it.",
		Blocked: "Both workstreams' upload units.", Options: []string{"Keep the index fixed at start", "Rebuild it on every start"},
		Recommendation: "Keep it fixed at start.",
	}, managerAt)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := r.AnswerQuestion(ctx, "chief", manager, string(thirdStream)+":1", "Fixed; see the logging guide.", []string{"kb/logging.md"}, managerAt); err != nil {
		t.Fatal(err)
	}

	qa, err := r.Questions(streamID)
	if err != nil || len(qa) != 1 || qa[0].State != QuestionEscalated {
		t.Fatalf("A's question: %+v err=%v", qa, err)
	}
	qb, err := r.Questions(secondStream)
	if err != nil || len(qb) != 1 || qb[0].State != QuestionEscalated {
		t.Fatalf("B's question: %+v err=%v", qb, err)
	}
	qc, err := r.Questions(thirdStream)
	if err != nil || len(qc) != 1 || qc[0].State != QuestionAnswered || qc[0].Ruling == nil || qc[0].Ruling.Actor != (Actor{Kind: "agent", ID: "chief"}) {
		t.Fatalf("C's question should be answered directly, unaffected: %+v err=%v", qc, err)
	}

	inbox, err := r.Inbox()
	if err != nil || len(inbox) != 1 {
		t.Fatalf("inbox: %+v err=%v", inbox, err)
	}
	entry := inbox[0]
	if entry.Batch != batch || entry.Number != 1 || len(entry.Questions) != 2 {
		t.Fatalf("consolidated inbox entry: %+v", entry)
	}
	if entry.Questions[0].Asked.Workstream != streamID || entry.Questions[1].Asked.Workstream != secondStream {
		t.Fatalf("inbox entry questions do not name both affected workstreams: %+v", entry.Questions)
	}

	ownerText := "Keep the index fixed at start."
	rulingAt := at.Add(2 * time.Minute)
	ruled, err := r.Rule(ctx, entry.Number, ownerText, owner, rulingAt)
	if err != nil {
		t.Fatal(err)
	}
	if ruled.Number != entry.Number || len(ruled.Questions) != 2 {
		t.Fatalf("ruled entry: %+v", ruled)
	}

	relayedText := "The index does not rebuild on start."
	relayAt := at.Add(3 * time.Minute)
	relayed, err := r.RelayRuling(ctx, "chief", manager, "1", relayedText, ScopeLocal, relayAt)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(relayed, []string{"1", "1"}) {
		t.Fatalf("relayed batch: %+v", relayed)
	}

	qa, err = r.Questions(streamID)
	if err != nil || len(qa) != 1 || qa[0].State != QuestionAnswered || qa[0].Ruling == nil || qa[0].Ruling.ReturnedAnswer != relayedText {
		t.Fatalf("A after the relay: %+v err=%v", qa, err)
	}
	qb, err = r.Questions(secondStream)
	if err != nil || len(qb) != 1 || qb[0].State != QuestionAnswered || qb[0].Ruling == nil || qb[0].Ruling.ReturnedAnswer != relayedText {
		t.Fatalf("B after the relay: %+v err=%v", qb, err)
	}
	qc, err = r.Questions(thirdStream)
	if err != nil || len(qc) != 1 || qc[0].Ruling == nil || qc[0].Ruling.ReturnedAnswer != "Fixed; see the logging guide." {
		t.Fatalf("C after the relay: %+v err=%v", qc, err)
	}
}
