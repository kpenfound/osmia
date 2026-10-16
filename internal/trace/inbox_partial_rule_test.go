package trace

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

// reclaimChiefTurn abandons turn's own claim on stream, the way a real
// restart's own recovery pass does, and enqueues and claims a fresh turn,
// next, on the same chief-of-staff thread, returning its scope. A claim
// made before a Close/Open pair is tied to the service session that made
// it (turnScope checks Claim.ServiceSession against the repository's own
// current session); reopening starts a new session, so relaying a ruling
// after a reopen needs a turn freshly claimed on the reopened instance,
// the same way the cross-repo restart test
// (TestCrossRepoConsolidatedEscalationSurvivesRestartAndRelayResumesBothOrigins)
// does for its own chief thread.
func reclaimChiefTurn(t *testing.T, r *Repository, stream config.WorkstreamID, thread, turn, next string, at time.Time) coreadapter.Scope {
	t.Helper()
	ctx := context.Background()
	if err := r.AbandonTurn(ctx, stream, "chief", turn, at); err != nil {
		t.Fatal(err)
	}
	req := TurnRequest{Header: header("turn-request", "request_"+next), AgentID: "chief", ThreadID: thread, TurnID: next,
		Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "fake-model-7"}, Prompt: "Owner message"}
	if _, err := r.EnqueueTurn(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ClaimTurn(ctx, stream, "chief", "token_"+next, "/owned/"+next, at); err != nil {
		t.Fatal(err)
	}
	return coreadapter.Scope{Project: string(projectID), Workstream: string(stream), Thread: thread, Turn: next, Role: StatusRole}
}

// escalatedOnly filters entries to the ones still escalated, the same
// filter the service applies before showing the owner's open-inbox view;
// Inbox() itself returns every escalation whatever became of it since.
func escalatedOnly(entries []InboxEntry) []InboxEntry {
	var out []InboxEntry
	for _, e := range entries {
		if e.State == QuestionEscalated {
			out = append(out, e)
		}
	}
	return out
}

// TestRetriedRuleCompletesRemainingMembersAfterACrashBetweenCommits is
// discovery-34b761bd01b530d9823a41cfd1735c27bbac1cc4e55771b11dea879039366d64's
// own evidence. Rule commits a consolidated escalation's ruling one member
// workstream at a time; this stops it, with a package-level hook, right
// after the first member's own commit and before the second's, the same way
// ensureWorkgroupsInterrupt (internal/service) tests a crash mid-pass. It
// shows the half-ruled entry still reads escalated, a different retried
// text is refused, no ruling notice exists in any member's outbox while the
// batch is still half-ruled, and a same-text retry after reopening the
// trace, through Rule, the production entry point the owner's inbox
// endpoint calls, completes only the member not yet ruled, writes the one
// notice exactly once, and, once the chief relays it with a freshly claimed
// turn, resumes both askers exactly once and leaves the entry gone from the
// inbox.
//
// serial: sets the package-level hook ruleInterrupt.
func TestRetriedRuleCompletesRemainingMembersAfterACrashBetweenCommits(t *testing.T) {
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
	chief := chiefThread(t, r, "chief1")
	chiefAt := at.Add(time.Minute)
	if _, err := r.EscalateQuestions(ctx, "chief", chief, EscalationRequest{
		Questions: []string{"1", string(secondStream) + ":1"}, Rephrasing: "Where does the upload index live, and does it rebuild on start?",
		Blocked: "Both workstreams' upload units.", Options: []string{"Keep it fixed", "Rebuild it"}, Recommendation: "Keep it fixed.",
	}, chiefAt); err != nil {
		t.Fatal(err)
	}

	noticesSoFar := func(r *Repository, stream config.WorkstreamID) int {
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

	ruleInterrupt = func(ws config.WorkstreamID) error {
		if ws == streamID {
			return errors.New("injected crash between members' commits")
		}
		return nil
	}
	t.Cleanup(func() { ruleInterrupt = nil })

	// EscalateQuestions already wrote its own notice to A's own outbox (A is
	// the batch's first member); snapshot both members' own notice counts
	// right before the interrupted Rule call, so the crash and the later
	// retry are each checked against what actually changed, not a fixed
	// total that would also count that unrelated, earlier notice.
	snapA := noticesSoFar(r, streamID)
	snapB := noticesSoFar(r, secondStream)

	rulingAt := at.Add(2 * time.Minute)
	ownerText := "Keep the index fixed at start."
	if _, err := r.Rule(ctx, 1, ownerText, owner, rulingAt); err == nil {
		t.Fatal("the injected crash reported success")
	}

	// A's own question is ruled; B's is still escalated, and the entry as a
	// whole still reads escalated, so the owner's inbox still lists it, and
	// the ruling notice, which belongs to whichever commit finishes the
	// batch, was never written: the batch is not yet whole.
	if qa := questionStates(t, r)["1"]; qa.State != QuestionRuled {
		t.Fatalf("A after the crash: %+v", qa)
	}
	qb, err := r.Questions(secondStream)
	if err != nil || len(qb) != 1 || qb[0].State != QuestionEscalated {
		t.Fatalf("B after the crash: %+v err=%v", qb, err)
	}
	inbox, err := r.Inbox()
	if err != nil || len(inbox) != 1 || inbox[0].State != QuestionEscalated || len(inbox[0].Questions) != 2 {
		t.Fatalf("inbox after the crash: %+v %v", inbox, err)
	}
	if n := noticesSoFar(r, streamID); n != snapA {
		t.Fatalf("A's own notices after the crash: %d, want unchanged from the snapshot %d (no ruling notice yet: the batch is not whole)", n, snapA)
	}
	if n := noticesSoFar(r, secondStream); n != snapB {
		t.Fatalf("B's own notices after the crash: %d, want unchanged from the snapshot %d (no ruling notice yet: the batch is not whole)", n, snapB)
	}

	// A different ruling text on the half-ruled entry is refused, and
	// changes nothing further.
	if _, err := r.Rule(ctx, 1, "Rebuild it on every start.", owner, rulingAt); !errors.Is(err, ErrRuled) {
		t.Fatalf("different text on a partly ruled entry: %v", err)
	}
	if qa := questionStates(t, r)["1"]; qa.Ruling.Revision != 1 {
		t.Fatalf("the refused different-text retry rewrote A's own ruling: %+v", qa.Ruling)
	}

	ruleInterrupt = nil
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	before, err := reopened.Outbox(streamID)
	if err != nil {
		t.Fatal(err)
	}
	ruled, err := reopened.Rule(ctx, 1, ownerText, owner, rulingAt)
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
	qb, err = reopened.Questions(secondStream)
	if err != nil || len(qb) != 1 || qb[0].State != QuestionRuled || qb[0].Ruling == nil || qb[0].Ruling.OwnerResponse != ownerText {
		t.Fatalf("B after the retry: %+v err=%v", qb, err)
	}
	if n := noticesSoFar(reopened, secondStream); n != snapB+1 {
		t.Fatalf("B's own notices after the completing retry: %d, want the snapshot %d plus exactly 1 (the ruling that finished the batch)", n, snapB)
	}
	inbox, err = reopened.Inbox()
	if err != nil || len(inbox) != 1 || inbox[0].State != QuestionRuled {
		t.Fatalf("inbox after the retry completed the batch: %+v %v", inbox, err)
	}

	// A real restart recovers the chief's own interrupted turn before any
	// new one claims its thread, the way the service's own startup does,
	// and a claim from before the reopen is tied to the service session
	// that made it, so the chief relays with a freshly claimed turn.
	chiefScope2 := reclaimChiefTurn(t, reopened, streamID, "chief_thread", "chief1", "chief2", rulingAt)
	relayAt := rulingAt.Add(time.Minute)
	relayed, err := reopened.RelayRuling(ctx, "chief", chiefScope2, "1", "The index stays fixed at start.", ScopeLocal, relayAt)
	if err != nil || len(relayed) != 2 {
		t.Fatalf("relay of the completed batch: %+v %v", relayed, err)
	}
	if qa := questionStates(t, reopened)["1"]; qa.State != QuestionAnswered || qa.Ruling.Revision != 2 {
		t.Fatalf("A resumed: %+v", qa)
	}
	qb, err = reopened.Questions(secondStream)
	if err != nil || len(qb) != 1 || qb[0].State != QuestionAnswered || qb[0].Ruling == nil || qb[0].Ruling.Revision != 2 {
		t.Fatalf("B resumed: %+v err=%v", qb, err)
	}
	// Inbox() is every escalation of the project, whatever became of it
	// since (its own doc comment), so the entry stays in it, now answered;
	// only the owner's open-inbox view, filtered to still-escalated
	// entries the way the service filters it, has it gone.
	inbox, err = reopened.Inbox()
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

	// A second relay of the same batch is refused: each asker is resumed
	// exactly once.
	if _, err := reopened.RelayRuling(ctx, "chief", chiefScope2, "1", "Again.", ScopeLocal, relayAt); err == nil {
		t.Fatal("a second relay of an already-answered question reported success")
	}
}

// TestRetriedRelayRulingCompletesRemainingMembersAfterACrashBetweenCommits
// covers the companion gap in RelayRuling: it commits a consolidated
// escalation's relay one member workstream at a time too, so a crash
// between two members' own commits leaves one asker resumed and the other
// still only ruled. This stops it, with the relayInterrupt hook, right
// after the first member's own commit, shows the still-ruled member is
// untouched and the other already answered, reopens the trace, claims a
// fresh chief turn and retries the relay with the same text through
// RelayRuling, the production entry point, and shows it completes only the
// member not yet answered, so both askers end up resumed exactly once with
// no question rewritten.
//
// serial: sets the package-level hook relayInterrupt.
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
	chief := chiefThread(t, r, "chief1")
	chiefAt := at.Add(time.Minute)
	if _, err := r.EscalateQuestions(ctx, "chief", chief, EscalationRequest{
		Questions: []string{"1", string(secondStream) + ":1"}, Rephrasing: "Where does the upload index live, and does it rebuild on start?",
		Blocked: "Both workstreams' upload units.", Options: []string{"Keep it fixed", "Rebuild it"}, Recommendation: "Keep it fixed.",
	}, chiefAt); err != nil {
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
	if _, err := r.RelayRuling(ctx, "chief", chief, "1", relayText, ScopeLocal, relayAt); err == nil {
		t.Fatal("the injected crash reported success")
	}

	if qa := questionStates(t, r)["1"]; qa.State != QuestionAnswered || qa.Ruling.Revision != 2 {
		t.Fatalf("A after the crash: %+v", qa)
	}
	qb, err := r.Questions(secondStream)
	if err != nil || len(qb) != 1 || qb[0].State != QuestionRuled {
		t.Fatalf("B after the crash: %+v err=%v", qb, err)
	}

	// A different text, while A is already answered and B is still only
	// ruled, is refused rather than completing B with a second owner
	// ruling the chief never actually relayed.
	if _, err := r.RelayRuling(ctx, "chief", chief, "1", "A different answer.", ScopeLocal, relayAt); err == nil {
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

	chiefScope2 := reclaimChiefTurn(t, reopened, streamID, "chief_thread", "chief1", "chief2", relayAt)
	relayed, err := reopened.RelayRuling(ctx, "chief", chiefScope2, "1", relayText, ScopeLocal, relayAt)
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
	// Inbox() is every escalation of the project, whatever became of it
	// since, so the entry stays in it, now answered; only the owner's
	// open-inbox view, filtered to still-escalated entries, has it gone.
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

	// A retry with a different text while a member is already answered is
	// refused, and a repeat once every member is answered is refused too.
	if _, err := reopened.RelayRuling(ctx, "chief", chiefScope2, "1", "Different.", ScopeLocal, relayAt); err == nil {
		t.Fatal("a relay with a different text on an already-answered question reported success")
	}
}
