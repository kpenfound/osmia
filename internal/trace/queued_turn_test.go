package trace

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestCancelQueuedTurnProtectsClaimsAndCapturedResponses(t *testing.T) {
	for _, scenario := range []string{"queued", "claimed", "attempted", "captured", "previous session", "active predecessor"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			r, root, p := create(t)
			if err := r.CreateThread(ctx, threadAgent()); err != nil {
				t.Fatal(err)
			}
			enqueue(t, r, "one")
			enqueue(t, r, "two")
			if scenario != "queued" {
				q := claimTurn(t, r, "claim1")
				if scenario == "attempted" {
					a := TurnAttempt{Number: 1, Profile: q.Request.Profile, Path: "replay", Reason: "fresh", At: at}
					if err := r.RecordAttempt(ctx, streamID, "mason", "one", q.Claim.Token, a); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "captured" {
					if err := r.CaptureTurn(ctx, q.Claim.Token, threadResponse(q)); err != nil {
						t.Fatal(err)
					}
				}
			}
			if scenario == "previous session" {
				if err := r.Close(); err != nil {
					t.Fatal(err)
				}
				var err error
				r, err = Open(root, p)
				if err != nil {
					t.Fatal(err)
				}
				defer r.Close()
			}
			before := mustThread(t, r)
			actor := Actor{Kind: "service", ID: "abandon"}
			for _, target := range []struct{ agent, turn string }{{"other", "one"}, {"mason", "missing"}} {
				if n, err := r.CancelQueuedTurn(ctx, streamID, target.agent, target.turn, at, actor, "gone"); err != nil || n != 0 {
					t.Fatalf("other turn: %d %v", n, err)
				}
			}
			target := "one"
			if scenario == "active predecessor" {
				target = "two"
			}
			n, err := r.CancelQueuedTurn(ctx, streamID, "mason", target, at.Add(time.Minute), actor, "gone")
			if err != nil {
				t.Fatal(err)
			}
			th := mustThread(t, r)
			if scenario != "queued" {
				if n != 0 || !reflect.DeepEqual(before, th) {
					t.Fatalf("protected turn changed: %d %+v", n, th)
				}
				return
			}
			q := th.Turns[0]
			if n != 1 || q.CompletedAt.IsZero() || q.Response == nil || !q.Response.Result.Cancelled || q.Response.Actor != actor || q.Response.Failure != "gone" || len(q.Attempts) != 0 || !reflect.DeepEqual(before.Turns[1], th.Turns[1]) {
				t.Fatalf("queued cancellation: %d %+v", n, th)
			}
			if n, err := r.CancelQueuedTurn(ctx, streamID, "mason", target, at, actor, "gone"); err != nil || n != 0 {
				t.Fatalf("cancellation retry: %d %v", n, err)
			}
		})
	}
}
