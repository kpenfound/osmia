package trace

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

func reclaimManagerTurn(t *testing.T, r *Repository, stream config.WorkstreamID, thread, turn, next string, at time.Time) coreadapter.Scope {
	t.Helper()
	ctx := context.Background()
	if err := r.AbandonTurn(ctx, stream, "manager", turn, at); err != nil {
		t.Fatal(err)
	}
	req := TurnRequest{Header: header("turn-request", "request_"+next), AgentID: "manager", ThreadID: thread, TurnID: next,
		Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "fake-model-7"}, Prompt: "Owner message"}
	if _, err := r.EnqueueTurn(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ClaimTurn(ctx, stream, "manager", "token_"+next, "/owned/"+next, at); err != nil {
		t.Fatal(err)
	}
	return coreadapter.Scope{Project: string(projectID), Workstream: string(stream), Thread: thread, Turn: next, Role: StatusRole}
}

func escalatedOnly(entries []InboxEntry) []InboxEntry {
	var out []InboxEntry
	for _, e := range entries {
		if e.State == QuestionEscalated {
			out = append(out, e)
		}
	}
	return out
}

const partialRuleText = "Keep the index fixed at start."

type crashedRuling struct {
	r        *Repository
	root     config.Root
	project  config.Project
	snapA    int
	snapB    int
	rulingAt time.Time
}

func noticesOf(t *testing.T, r *Repository, stream config.WorkstreamID) int {
	t.Helper()
	out, err := r.Outbox(stream)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range out {
		if e.Event.Kind == NoticeKind {
			n++
		}
	}
	return n
}

func newCrashedRuling(t *testing.T) crashedRuling {
	t.Helper()
	ctx := context.Background()
	r, root, p := create(t)
	if err := r.CreateWorkstream(ctx, secondStream, at, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateWorkgroup(ctx, r, []config.WorkstreamID{streamID, secondStream}, at, owner, "crash-workgroup"); err != nil {
		t.Fatal(err)
	}
	askerA := askerThread(t, r, "agent_a", "mason", "build_a")
	if _, err := r.Ask(ctx, "agent_a", askerA, "Which store holds the upload index?", at); err != nil {
		t.Fatal(err)
	}
	askerB := askerThreadOn(t, r, secondStream, "agent_b", "mason", "build_b")
	if _, err := r.Ask(ctx, "agent_b", askerB, "Does the index rebuild on start?", at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	manager := managerThread(t, r, "manager1")
	managerAt := at.Add(time.Minute)
	if _, err := r.EscalateQuestions(ctx, "manager", manager, EscalationRequest{
		Questions: []string{"1", string(secondStream) + ":1"}, Rephrasing: "Where does the upload index live, and does it rebuild on start?",
		Blocked: "Both workstreams' upload units.", Options: []string{"Keep it fixed", "Rebuild it"}, Recommendation: "Keep it fixed.",
	}, managerAt); err != nil {
		t.Fatal(err)
	}

	ruleInterrupt = func(ws config.WorkstreamID) error {
		if ws == streamID {
			return errors.New("injected crash between members' commits")
		}
		return nil
	}
	t.Cleanup(func() { ruleInterrupt = nil })

	c := crashedRuling{r: r, root: root, project: p, snapA: noticesOf(t, r, streamID), snapB: noticesOf(t, r, secondStream), rulingAt: at.Add(2 * time.Minute)}
	if _, err := r.Rule(ctx, 1, partialRuleText, owner, c.rulingAt); err == nil {
		t.Fatal("the injected crash reported success")
	}
	return c
}

func (c crashedRuling) reopened(t *testing.T) *Repository {
	t.Helper()
	ruleInterrupt = nil
	if err := c.r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(c.root, c.project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	return reopened
}

func TestARulingThatCrashesBetweenMembersLeavesTheBatchPartlyRuled(t *testing.T) {
	c := newCrashedRuling(t)
	if qa := questionStates(t, c.r)["1"]; qa.State != QuestionRuled {
		t.Fatalf("A after the crash: %+v", qa)
	}
	qb, err := c.r.Questions(secondStream)
	if err != nil || len(qb) != 1 || qb[0].State != QuestionEscalated {
		t.Fatalf("B after the crash: %+v err=%v", qb, err)
	}
	inbox, err := c.r.Inbox()
	if err != nil || len(inbox) != 1 || inbox[0].State != QuestionEscalated || len(inbox[0].Questions) != 2 {
		t.Fatalf("inbox after the crash: %+v %v", inbox, err)
	}
	if n := noticesOf(t, c.r, streamID); n != c.snapA {
		t.Fatalf("A's own notices after the crash: %d, want unchanged from the snapshot %d (no ruling notice yet: the batch is not whole)", n, c.snapA)
	}
	if n := noticesOf(t, c.r, secondStream); n != c.snapB {
		t.Fatalf("B's own notices after the crash: %d, want unchanged from the snapshot %d (no ruling notice yet: the batch is not whole)", n, c.snapB)
	}
}

func TestADifferentRulingOnAPartlyRuledEntryIsRefused(t *testing.T) {
	c := newCrashedRuling(t)
	if _, err := c.r.Rule(context.Background(), 1, "Rebuild it on every start.", owner, c.rulingAt); !errors.Is(err, ErrRuled) {
		t.Fatalf("different text on a partly ruled entry: %v", err)
	}
	if qa := questionStates(t, c.r)["1"]; qa.Ruling.Revision != 1 {
		t.Fatalf("the refused different-text retry rewrote A's own ruling: %+v", qa.Ruling)
	}
}

func TestRetriedRuleCompletesRemainingMembersAfterACrashBetweenCommits(t *testing.T) {
	ctx := context.Background()
	c := newCrashedRuling(t)
	reopened := c.reopened(t)
	before, err := reopened.Outbox(streamID)
	if err != nil {
		t.Fatal(err)
	}
	ruled, err := reopened.Rule(ctx, 1, partialRuleText, owner, c.rulingAt)
	if err != nil || ruled.Number != 1 {
		t.Fatalf("retried ruling: %+v %v", ruled, err)
	}
	after, err := reopened.Outbox(streamID)
	if err != nil || len(after) != len(before) {
		t.Fatalf("the retry raised %d events on A's own trace, want 0 (no second notice, and the completing notice belongs to B): %v", len(after)-len(before), err)
	}
	if qa := questionStates(t, reopened)["1"]; qa.Ruling == nil || qa.Ruling.Revision != 1 {
		t.Fatalf("the retry rewrote A's own, already-ruled question: %+v", qa.Ruling)
	}
	qb, err := reopened.Questions(secondStream)
	if err != nil || len(qb) != 1 || qb[0].State != QuestionRuled || qb[0].Ruling == nil || qb[0].Ruling.OwnerResponse != partialRuleText {
		t.Fatalf("B after the retry: %+v err=%v", qb, err)
	}
	if n := noticesOf(t, reopened, secondStream); n != c.snapB+1 {
		t.Fatalf("B's own notices after the completing retry: %d, want the snapshot %d plus exactly 1 (the ruling that finished the batch)", n, c.snapB)
	}
	inbox, err := reopened.Inbox()
	if err != nil || len(inbox) != 1 || inbox[0].State != QuestionRuled {
		t.Fatalf("inbox after the retry completed the batch: %+v %v", inbox, err)
	}
}

func TestRelayAfterARetriedRuleAnswersBothMembersOnce(t *testing.T) {
	ctx := context.Background()
	c := newCrashedRuling(t)
	reopened := c.reopened(t)
	if _, err := reopened.Rule(ctx, 1, partialRuleText, owner, c.rulingAt); err != nil {
		t.Fatal(err)
	}
	managerScope2 := reclaimManagerTurn(t, reopened, streamID, "manager_thread", "manager1", "manager2", c.rulingAt)
	relayAt := c.rulingAt.Add(time.Minute)
	relayed, err := reopened.RelayRuling(ctx, "manager", managerScope2, "1", "The index stays fixed at start.", ScopeLocal, relayAt)
	if err != nil || len(relayed) != 2 {
		t.Fatalf("relay of the completed batch: %+v %v", relayed, err)
	}
	if qa := questionStates(t, reopened)["1"]; qa.State != QuestionAnswered || qa.Ruling.Revision != 2 {
		t.Fatalf("A resumed: %+v", qa)
	}
	qb, err := reopened.Questions(secondStream)
	if err != nil || len(qb) != 1 || qb[0].State != QuestionAnswered || qb[0].Ruling == nil || qb[0].Ruling.Revision != 2 {
		t.Fatalf("B resumed: %+v err=%v", qb, err)
	}
	inbox, err := reopened.Inbox()
	if err != nil || len(inbox) != 1 || inbox[0].Number != 1 || inbox[0].State != QuestionAnswered || len(inbox[0].Questions) != 2 {
		t.Fatalf("inbox after the relay, want the entry recorded as answered: %+v %v", inbox, err)
	}
	for _, q := range inbox[0].Questions {
		if q.State != QuestionAnswered {
			t.Fatalf("inbox after the relay, want both questions answered: %+v", inbox[0].Questions)
		}
	}
	if open := escalatedOnly(inbox); len(open) != 0 {
		t.Fatalf("the owner's open-inbox view after the relay, want no escalated entries: %+v", open)
	}
	if _, err := reopened.RelayRuling(ctx, "manager", managerScope2, "1", "Again.", ScopeLocal, relayAt); err == nil {
		t.Fatal("a second relay of an already-answered question reported success")
	}
}

func TestRetriedRelayRulingCompletesRemainingMembersAfterACrashBetweenCommits(t *testing.T) {
	ctx := context.Background()
	r, root, p := create(t)
	if err := r.CreateWorkstream(ctx, secondStream, at, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateWorkgroup(ctx, r, []config.WorkstreamID{streamID, secondStream}, at, owner, "relay-crash-workgroup"); err != nil {
		t.Fatal(err)
	}
	askerA := askerThread(t, r, "agent_a", "mason", "build_a")
	if _, err := r.Ask(ctx, "agent_a", askerA, "Which store holds the upload index?", at); err != nil {
		t.Fatal(err)
	}
	askerB := askerThreadOn(t, r, secondStream, "agent_b", "mason", "build_b")
	if _, err := r.Ask(ctx, "agent_b", askerB, "Does the index rebuild on start?", at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	manager := managerThread(t, r, "manager1")
	managerAt := at.Add(time.Minute)
	if _, err := r.EscalateQuestions(ctx, "manager", manager, EscalationRequest{
		Questions: []string{"1", string(secondStream) + ":1"}, Rephrasing: "Where does the upload index live, and does it rebuild on start?",
		Blocked: "Both workstreams' upload units.", Options: []string{"Keep it fixed", "Rebuild it"}, Recommendation: "Keep it fixed.",
	}, managerAt); err != nil {
		t.Fatal(err)
	}
	rulingAt := at.Add(2 * time.Minute)
	ownerText := "Keep the index fixed at start."
	if _, err := r.Rule(ctx, 1, ownerText, owner, rulingAt); err != nil {
		t.Fatal(err)
	}

	relayInterrupt = func(ws config.WorkstreamID) error {
		if ws == streamID {
			return errors.New("injected crash between members' relay commits")
		}
		return nil
	}
	t.Cleanup(func() { relayInterrupt = nil })

	relayAt := rulingAt.Add(time.Minute)
	relayText := "The index stays fixed at start."
	if _, err := r.RelayRuling(ctx, "manager", manager, "1", relayText, ScopeLocal, relayAt); err == nil {
		t.Fatal("the injected crash reported success")
	}

	if qa := questionStates(t, r)["1"]; qa.State != QuestionAnswered || qa.Ruling.Revision != 2 {
		t.Fatalf("A after the crash: %+v", qa)
	}
	qb, err := r.Questions(secondStream)
	if err != nil || len(qb) != 1 || qb[0].State != QuestionRuled {
		t.Fatalf("B after the crash: %+v err=%v", qb, err)
	}

	if _, err := r.RelayRuling(ctx, "manager", manager, "1", "A different answer.", ScopeLocal, relayAt); err == nil {
		t.Fatal("a relay with a different text on a half-relayed batch reported success")
	}
	if qa := questionStates(t, r)["1"]; qa.Ruling.ReturnedAnswer != relayText {
		t.Fatalf("the refused different-text retry rewrote A's own relay: %+v", qa.Ruling)
	}
	if qb, err := r.Questions(secondStream); err != nil || len(qb) != 1 || qb[0].State != QuestionRuled {
		t.Fatalf("the refused different-text retry changed B: %+v err=%v", qb, err)
	}

	relayInterrupt = nil
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	managerScope2 := reclaimManagerTurn(t, reopened, streamID, "manager_thread", "manager1", "manager2", relayAt)
	relayed, err := reopened.RelayRuling(ctx, "manager", managerScope2, "1", relayText, ScopeLocal, relayAt)
	if err != nil || len(relayed) != 2 {
		t.Fatalf("retried relay: %+v %v", relayed, err)
	}
	if qa := questionStates(t, reopened)["1"]; qa.State != QuestionAnswered || qa.Ruling.Revision != 2 {
		t.Fatalf("the retry rewrote A's own, already-answered question: %+v", qa.Ruling)
	}
	qb, err = reopened.Questions(secondStream)
	if err != nil || len(qb) != 1 || qb[0].State != QuestionAnswered || qb[0].Ruling == nil || qb[0].Ruling.Revision != 2 || qb[0].Ruling.ReturnedAnswer != relayText {
		t.Fatalf("B resumed by the retry: %+v err=%v", qb, err)
	}
	inbox, err := reopened.Inbox()
	if err != nil || len(inbox) != 1 || inbox[0].Number != 1 || inbox[0].State != QuestionAnswered || len(inbox[0].Questions) != 2 {
		t.Fatalf("inbox after the retried relay, want the entry recorded as answered: %+v %v", inbox, err)
	}
	for _, q := range inbox[0].Questions {
		if q.State != QuestionAnswered {
			t.Fatalf("inbox after the retried relay, want both questions answered: %+v", inbox[0].Questions)
		}
	}
	if open := escalatedOnly(inbox); len(open) != 0 {
		t.Fatalf("the owner's open-inbox view after the retried relay, want no escalated entries: %+v", open)
	}

	if _, err := reopened.RelayRuling(ctx, "manager", managerScope2, "1", "Different.", ScopeLocal, relayAt); err == nil {
		t.Fatal("a relay with a different text on an already-answered question reported success")
	}
}
