package beekeeper

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

const relayProject config.ProjectID = "p_11111111111111111111111111111111"

func successResult(text string) coreadapter.SessionResult {
	return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session"}, FinalResponse: text}
}

// Recording a chief of staff's reply to the Beekeeper message its turn
// request identifies writes it once, and recording it again for the same
// project, workstream and request leaves exactly one, with the result
// still reported as success.
func TestRecordRelayedReplyIsRecordedOnce(t *testing.T) {
	ctx := context.Background()
	repo := openShadow(t)
	if err := RecordRelayedReply(ctx, repo, relayProject, "w_11111111111111111111111111111111", "request_1", "Two masons are building.", at); err != nil {
		t.Fatal(err)
	}
	if err := RecordRelayedReply(ctx, repo, relayProject, "w_11111111111111111111111111111111", "request_1", "Two masons are building.", at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	replies, err := RelayedReplies(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(replies) != 1 || replies[0].Text != "Two masons are building." || replies[0].Workstream != "w_11111111111111111111111111111111" {
		t.Fatalf("relayed replies: %+v", replies)
	}
}

// RelayedReplies returns every relayed reply in time order, whatever order
// they were recorded in, and a different request, project or workstream
// records a distinct reply.
func TestRelayedRepliesAreOrderedByTime(t *testing.T) {
	ctx := context.Background()
	repo := openShadow(t)
	later := at.Add(time.Hour)
	earlier := at.Add(time.Minute)
	must(t, RecordRelayedReply(ctx, repo, relayProject, "w_11111111111111111111111111111111", "request_late", "Later reply.", later))
	must(t, RecordRelayedReply(ctx, repo, relayProject, "w_22222222222222222222222222222222", "request_early", "Earlier reply.", earlier))
	replies, err := RelayedReplies(repo)
	must(t, err)
	if len(replies) != 2 || replies[0].Text != "Earlier reply." || replies[1].Text != "Later reply." {
		t.Fatalf("relayed replies not in time order: %+v", replies)
	}
}

// Post's next turn is given every relayed reply recorded since its
// previous turn, in time order, and no earlier one: a reply recorded
// before the Beekeeper's first message is not repeated on a later turn,
// and one recorded between two turns reaches only the next one.
func TestPostIncludesRelayedRepliesSinceThePreviousTurnOnly(t *testing.T) {
	ctx := context.Background()
	repo := openShadow(t)
	now := at
	clock := func() time.Time { return now }

	// Before the Beekeeper's first message: included in its first turn.
	must(t, RecordRelayedReply(ctx, repo, relayProject, "w_11111111111111111111111111111111", "request_before", "Before the first turn.", now.Add(-time.Minute)))

	first := &recordingTurns{result: successResult("Noted.")}
	if _, err := Post(ctx, repo, testProfile(), first, testPrepare(t), clock, "Status?"); err != nil {
		t.Fatal(err)
	}
	calls := first.Calls()
	if len(calls) != 1 || !strings.Contains(calls[0].SystemPrompt, "Before the first turn.") {
		t.Fatalf("first turn system prompt: %+v", calls)
	}

	// Recorded between the first and second turns: included in the second,
	// not repeated afterward.
	now = at.Add(time.Hour)
	must(t, RecordRelayedReply(ctx, repo, relayProject, "w_22222222222222222222222222222222", "request_between", "Between the turns.", now))

	second := &recordingTurns{result: successResult("Noted again.")}
	now = at.Add(2 * time.Hour)
	if _, err := Post(ctx, repo, testProfile(), second, testPrepare(t), clock, "Anything new?"); err != nil {
		t.Fatal(err)
	}
	calls = second.Calls()
	if len(calls) != 1 || strings.Contains(calls[0].SystemPrompt, "Before the first turn.") || !strings.Contains(calls[0].SystemPrompt, "Between the turns.") {
		t.Fatalf("second turn system prompt: %+v", calls)
	}

	third := &recordingTurns{result: successResult("Still quiet.")}
	now = at.Add(3 * time.Hour)
	if _, err := Post(ctx, repo, testProfile(), third, testPrepare(t), clock, "Still nothing?"); err != nil {
		t.Fatal(err)
	}
	calls = third.Calls()
	if len(calls) != 1 || calls[0].SystemPrompt != RolePrompt {
		t.Fatalf("third turn system prompt carried an earlier reply: %+v", calls)
	}
}
