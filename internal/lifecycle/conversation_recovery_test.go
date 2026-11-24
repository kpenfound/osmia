package lifecycle_test

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestConversationPathSharedChiefRestartReconcilesHostOperation(t *testing.T) {
	t.Parallel()
	for _, completed := range []bool{false, true} {
		t.Run(map[bool]string{false: "captured", true: "completed"}[completed], func(t *testing.T) {
			ctx := context.Background()
			f := newCrossRepoWiringFixture(t, map[string]string{trace.ChiefOfStaff: trace.ChiefOfStaff})
			s, err := Start(ctx, f.opts)
			fixturekit.Must(t, err)
			t.Cleanup(func() { fixturekit.Must(t, s.Close()) })
			_, apiFault := s.lifecycle().DraftWorkgroup(ctx, []config.WorkstreamID{f.stream1, f.stream2})
			if apiFault != nil {
				t.Fatal(apiFault)
			}
			InspectForTest(s).DrainConversations()
			client := NewClient(s.Socket())
			t.Cleanup(func() { client.Close() })
			sent, err := client.Send(ctx, f.stream1, "Shared captured message")
			fixturekit.Must(t, err)
			repo, err := s.repository(f.project1)
			fixturekit.Must(t, err)
			wt, host, _, closeTarget, err := s.lifecycle().ChiefRepository(ctx, repo, f.stream1)
			fixturekit.Must(t, err)
			th, err := wt.Thread(host, trace.ChiefOfStaff)
			fixturekit.Must(t, err)
			if len(th.Turns) != 1 {
				closeTarget()
				t.Fatalf("shared queue: %+v", th)
			}
			q := th.Turns[0]
			at := f.clock.Now()
			fixturekit.Must(t, scheduler.Dispatch(ctx, wt, scheduler.Candidate{Project: wt.Project(), Workstream: host, Thread: th, Turn: q}, at))
			records, err := wt.Operations(host)
			fixturekit.Must(t, err)
			attempt, _, err := wt.BeginOperation(ctx, host, records[0].EventID, trace.ConversationOperation, intake.ServiceActor, f.clock.Now)
			fixturekit.Must(t, err)
			if attempt == nil {
				closeTarget()
				t.Fatal("host operation not claimed")
			}
			q, err = wt.ClaimTurn(ctx, host, trace.ChiefOfStaff, "shared-token", "/owned-shared", at)
			fixturekit.Must(t, err)
			result := coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: q.Request.Profile.Backend, ID: "shared-session"}, FinalResponse: "Shared captured reply", SessionDirectory: "/owned-shared", StartedAt: at}
			a := trace.TurnAttempt{Number: 1, At: at, Profile: q.Request.Profile, Path: "replay", Reason: "fresh"}
			fixturekit.Must(t, wt.RecordAttempt(ctx, host, trace.ChiefOfStaff, q.Request.TurnID, q.Claim.Token, a))
			a.Result = &result
			fixturekit.Must(t, wt.RecordAttempt(ctx, host, trace.ChiefOfStaff, q.Request.TurnID, q.Claim.Token, a))
			h := q.Request.Header
			h.Schema, h.ID, h.Actor = "osmia.trace.turn-response", trace.EventID(q.Request.ID, "response"), intake.ServiceActor
			fixturekit.Must(t, wt.CaptureTurn(ctx, q.Claim.Token, trace.TurnResponse{Header: h, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: q.Request.TurnID, RequestID: q.Request.ID, RequestRevision: 1, Result: result}))
			if completed {
				fixturekit.Must(t, wt.CompleteTurn(ctx, host, trace.ChiefOfStaff, q.Request.TurnID, q.Claim.Token, at))
			}
			attempt.Close()
			closeTarget()
			fixturekit.Must(t, s.Close())
			s, err = Start(ctx, f.opts)
			fixturekit.Must(t, err)
			repo, err = s.repository(f.project1)
			fixturekit.Must(t, err)
			wt, host, _, closeTarget, err = s.lifecycle().ChiefRepository(ctx, repo, f.stream1)
			fixturekit.Must(t, err)
			t.Cleanup(closeTarget)
			fixturekit.ConversationAwait(t, func() bool { records, _ := wt.Operations(host); return len(records) == 1 && records[0].Acknowledged })
			th, err = wt.Thread(host, trace.ChiefOfStaff)
			fixturekit.Must(t, err)
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
