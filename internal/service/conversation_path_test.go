package service

import (
	"context"
	"os"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestConversationQueueSkipsUnreadableWorkstream(t *testing.T) {
	t.Parallel()
	opts, cfg := conversationFixture(t, "conversation-scan-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	t.Cleanup(func() { repo.Close() })
	// The unreadable trace sorts before the live thread. One scan failure
	// must not keep the remaining workstreams from publishing their turns.
	broken := config.WorkstreamID("w_00000000000000000000000000000000")
	dir, err := cfg.Root.WorkstreamTrace(cfg.Project.ID, broken)
	must(t, err)
	must(t, os.MkdirAll(dir, 0700))
	profile, err := cfg.NamedProfile("default")
	must(t, err)
	ctx := context.Background()
	q, err := repo.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_scan", Project: repo.Project(), Workstream: stream, At: opts.Reconciliation.Now(), Actor: ownerActor, Cause: "owner-message"}, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "message_scan", Profile: profile, Prompt: "Hello"})
	must(t, err)
	p := &conversationPath{s: &Service{options: opts}, repository: repo}
	must(t, p.queue(ctx, repo))
	records, err := repo.Operations(stream)
	must(t, err)
	if len(records) != 1 {
		t.Fatalf("published operations: %+v", records)
	}
	in, err := thread.DecodeTurn(records[0].Operation)
	must(t, err)
	if in.Turn != q.Request.TurnID {
		t.Fatalf("published turn: %+v", in)
	}
}

func TestConversationRecoveryOrdersPendingHooksByThreadSequence(t *testing.T) {
	t.Parallel()
	th := trace.Thread{Identity: trace.Agent{Header: trace.Header{ID: trace.ChiefOfStaff}}, Turns: []trace.QueuedTurn{
		{Request: trace.TurnRequest{TurnID: "earlier"}, CompletedAt: demoStart},
		{Request: trace.TurnRequest{TurnID: "later"}},
	}}
	operation := func(turn string) trace.OperationRecord {
		op, err := thread.TurnOperation(project, turn, thread.TurnInput{Workstream: stream, Agent: trace.ChiefOfStaff, Turn: turn})
		must(t, err)
		return trace.OperationRecord{Operation: op}
	}
	// Durable operation scans sort by hashed event ID. A later turn can be
	// encountered first even when an earlier completed turn's hook is pending.
	records := []trace.OperationRecord{operation("later"), operation("earlier")}
	if !pendingConversation(records, th, "later") {
		t.Fatal("later turn passed the earlier turn's unsettled hook")
	}
	if pendingConversation(records, th, "earlier") {
		t.Fatal("later intent prevented the earlier hook from settling")
	}
	records[1].Acknowledged = true
	if pendingConversation(records, th, "later") {
		t.Fatal("settled earlier hook held the later turn")
	}
}
