package lifecycle_test

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

var conversationAwait = fixturekit.ConversationAwait

func TestConversationPathSharedChiefRestartReconcilesHostOperation(t *testing.T) {
	t.Parallel()
	for _, completed := range []bool{false, true} {
		t.Run(map[bool]string{false: "captured", true: "completed"}[completed], func(t *testing.T) {
			ctx := context.Background()
			f := newCrossRepoWiringFixture(t, map[string]string{trace.ChiefOfStaff: trace.ChiefOfStaff})
			s, err := Start(ctx, f.opts)
			must(t, err)
			t.Cleanup(func() { must(t, s.Close()) })
			_, api := s.DraftWorkgroup(ctx, []config.WorkstreamID{f.stream1, f.stream2})
			if api != nil {
				t.Fatal(api)
			}
			InspectForTest(s).DrainConversations()
			client := NewClient(s.Socket())
			t.Cleanup(func() { client.Close() })
			sent, err := client.Send(ctx, f.stream1, "Shared captured message")
			must(t, err)
			repo, err := s.repository(f.project1)
			must(t, err)
			wt, host, _, closeTarget, err := s.chiefRepository(ctx, repo, f.stream1)
			must(t, err)
			th, err := wt.Thread(host, trace.ChiefOfStaff)
			must(t, err)
			if len(th.Turns) != 1 {
				closeTarget()
				t.Fatalf("shared queue: %+v", th)
			}
			q := th.Turns[0]
			at := f.clock.Now()
			must(t, scheduler.Dispatch(ctx, wt, scheduler.Candidate{Project: wt.Project(), Workstream: host, Thread: th, Turn: q}, at))
			records, err := wt.Operations(host)
			must(t, err)
			attempt, _, err := wt.BeginOperation(ctx, host, records[0].EventID, trace.ConversationOperation, serviceActor, f.clock.Now)
			must(t, err)
			if attempt == nil {
				closeTarget()
				t.Fatal("host operation not claimed")
			}
			q, err = wt.ClaimTurn(ctx, host, trace.ChiefOfStaff, "shared-token", "/owned-shared", at)
			must(t, err)
			result := coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: q.Request.Profile.Backend, ID: "shared-session"}, FinalResponse: "Shared captured reply", SessionDirectory: "/owned-shared", StartedAt: at}
			a := trace.TurnAttempt{Number: 1, At: at, Profile: q.Request.Profile, Path: "replay", Reason: "fresh"}
			must(t, wt.RecordAttempt(ctx, host, trace.ChiefOfStaff, q.Request.TurnID, q.Claim.Token, a))
			a.Result = &result
			must(t, wt.RecordAttempt(ctx, host, trace.ChiefOfStaff, q.Request.TurnID, q.Claim.Token, a))
			h := q.Request.Header
			h.Schema, h.ID, h.Actor = "osmia.trace.turn-response", trace.EventID(q.Request.ID, "response"), serviceActor
			must(t, wt.CaptureTurn(ctx, q.Claim.Token, trace.TurnResponse{Header: h, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: q.Request.TurnID, RequestID: q.Request.ID, RequestRevision: 1, Result: result}))
			if completed {
				must(t, wt.CompleteTurn(ctx, host, trace.ChiefOfStaff, q.Request.TurnID, q.Claim.Token, at))
			}
			attempt.Close()
			closeTarget()
			must(t, s.Close())
			s, err = Start(ctx, f.opts)
			must(t, err)
			repo, err = s.repository(f.project1)
			must(t, err)
			wt, host, _, closeTarget, err = s.chiefRepository(ctx, repo, f.stream1)
			must(t, err)
			t.Cleanup(closeTarget)
			conversationAwait(t, func() bool { records, _ := wt.Operations(host); return len(records) == 1 && records[0].Acknowledged })
			th, err = wt.Thread(host, trace.ChiefOfStaff)
			must(t, err)
			if len(th.Turns) != 1 || th.Turns[0].Request.TurnID != sent.Turn || th.Turns[0].CompletedAt.IsZero() || th.Turns[0].Response.Result.FinalResponse != result.FinalResponse || len(th.Turns[0].Attempts) != 1 {
				t.Fatalf("shared recovery: %+v", th)
			}
			f.engine.Mu.Lock()
			calls := len(f.engine.Runs)
			f.engine.Mu.Unlock()
			if calls != 0 {
				t.Fatalf("captured shared reply made %d backend calls", calls)
			}
		})
	}
}
