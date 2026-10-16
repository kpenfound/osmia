package trace

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

// askerThreadOn is askerThread for a workstream other than streamID: it
// creates its asker's thread with one claimed turn on stream and returns the
// scope of that turn.
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

// TestConsolidatedEscalationPausesOnlyItsMembersWhileAThirdContinues builds a
// three-workstream workgroup, shares the first member (its host) as the
// chief of staff for all three, and shows a genuine gap affecting two of
// them escalated together as one consolidated inbox entry (spec#18): the
// third member's own independent question is answered directly in the same
// pass and never waits on it. The owner's one ruling on that one entry
// resumes both paused askers, and only them: the third member's already-
// answered question is untouched.
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

	chief := chiefThread(t, r, "chief_turn")
	chiefAt := at.Add(time.Minute)

	// The gap affects both A's and B's questions together: the chief
	// escalates them as one consolidated batch, naming B's question
	// qualified since it belongs to another member of the shared workgroup.
	batch, err := r.EscalateQuestions(ctx, "chief", chief, EscalationRequest{
		Questions: []string{"1", string(secondStream) + ":1"}, Rephrasing: "Where does the upload index live, and does it rebuild on start?",
		Section: "spec#3", Conflict: "The spec says the index is derived, but doesn't say whether a restart may recompute it.",
		Blocked: "Both workstreams' upload units.", Options: []string{"Keep the index fixed at start", "Rebuild it on every start"},
		Recommendation: "Keep it fixed at start.",
	}, chiefAt)
	if err != nil {
		t.Fatal(err)
	}

	// C's own question is unaffected by the gap: the chief answers it
	// directly, in the same pass, without waiting for the owner.
	if _, err := r.AnswerQuestion(ctx, "chief", chief, string(thirdStream)+":1", "Fixed; see the logging guide.", []string{"kb/logging.md"}, chiefAt); err != nil {
		t.Fatal(err)
	}

	// Only A and B paused: their questions are escalated, and only theirs.
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

	// The owner sees one consolidated inbox entry, spanning A and B, not two
	// separate ones, and nothing naming C.
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

	// The owner answers the one consolidated entry, once.
	ownerText := "Keep the index fixed at start."
	rulingAt := at.Add(2 * time.Minute)
	ruled, err := r.Rule(ctx, entry.Number, ownerText, owner, rulingAt)
	if err != nil {
		t.Fatal(err)
	}
	if ruled.Number != entry.Number || len(ruled.Questions) != 2 {
		t.Fatalf("ruled entry: %+v", ruled)
	}

	// The chief relays the ruling, naming only one of the batch's questions:
	// the relay still resumes every question of that one batch.
	relayedText := "The index does not rebuild on start."
	relayAt := at.Add(3 * time.Minute)
	relayed, err := r.RelayRuling(ctx, "chief", chief, "1", relayedText, ScopeLocal, relayAt)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(relayed, []string{"1", "1"}) {
		t.Fatalf("relayed batch: %+v", relayed)
	}

	// Both A and B are resumed, with the same relayed answer. C, never part
	// of the gap, keeps the independent answer it already had and is
	// untouched by the relay.
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
