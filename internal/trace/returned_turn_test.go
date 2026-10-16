package trace

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestCancelReturnedTurnProtectsOtherClaimsAndCapturedResults(t *testing.T) {
	for _, scenario := range []string{"uncaptured", "captured", "unclaimed", "previous session", "unfinished attempt", "recorded result"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			r, root, p := create(t)
			if err := r.CreateThread(ctx, threadAgent()); err != nil {
				t.Fatal(err)
			}
			enqueue(t, r, "one")
			enqueue(t, r, "two")
			var q QueuedTurn
			if scenario != "unclaimed" {
				q = claimTurn(t, r, "claim1")
			}
			if scenario == "captured" {
				if err := r.CaptureTurn(ctx, q.Claim.Token, threadResponse(q)); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "unfinished attempt" || scenario == "recorded result" {
				a := TurnAttempt{Number: 1, Profile: q.Request.Profile, Path: "replay", Reason: "fresh", At: at}
				if err := r.RecordAttempt(ctx, streamID, "mason", "one", q.Claim.Token, a); err != nil {
					t.Fatal(err)
				}
				if scenario == "recorded result" {
					result := threadResponse(q).Result
					a.Result = &result
					if err := r.RecordAttempt(ctx, streamID, "mason", "one", q.Claim.Token, a); err != nil {
						t.Fatal(err)
					}
				}
			}
			if scenario == "previous session" {
				r.Close()
				var err error
				r, err = Open(root, p)
				if err != nil {
					t.Fatal(err)
				}
				defer r.Close()
			}
			before := mustThread(t, r)
			actor := Actor{Kind: "service", ID: "abandon"}
			for _, target := range []struct{ agent, turn, token string }{{"other", "one", "claim1"}, {"mason", "two", "claim1"}, {"mason", "one", "other-claim"}, {"mason", "one", ""}} {
				if n, err := r.CancelReturnedTurn(ctx, streamID, target.agent, target.turn, target.token, at, actor, "gone"); err != nil || n != 0 {
					t.Fatalf("other turn: %d %v", n, err)
				}
				if th := mustThread(t, r); !reflect.DeepEqual(before, th) {
					t.Fatalf("other invocation changed turn: %+v", th)
				}
			}
			n, err := r.CancelReturnedTurn(ctx, streamID, "mason", "one", "claim1", at.Add(time.Minute), actor, "gone")
			if err != nil {
				t.Fatal(err)
			}
			th := mustThread(t, r)
			if scenario == "captured" || scenario == "unclaimed" || scenario == "previous session" {
				if n != 0 || !reflect.DeepEqual(before, th) {
					t.Fatalf("protected turn changed: %d %+v", n, th)
				}
				return
			}
			q = th.Turns[0]
			if n != 1 || q.CompletedAt.IsZero() || q.Response == nil || th.Active != "" || th.Turns[1].Response != nil || q.Claim.Token != "claim1" {
				t.Fatalf("returned turn: %d %+v", n, th)
			}
			if scenario == "recorded result" {
				if !reflect.DeepEqual(q.Response.Result, threadResponse(q).Result) {
					t.Fatalf("recorded result lost: %+v", q.Response)
				}
			} else if !q.Response.Result.Cancelled || q.Response.Actor != actor || q.Response.Failure != "gone" {
				t.Fatalf("cancellation: %+v", q.Response)
			}
			if len(q.Attempts) != 0 && (q.Attempts[0].Result == nil || !reflect.DeepEqual(*q.Attempts[0].Result, q.Response.Result)) {
				t.Fatalf("attempt and response differ: %+v", q)
			}
			if n, err := r.CancelReturnedTurn(ctx, streamID, "mason", "one", "claim1", at, actor, "gone"); err != nil || n != 0 {
				t.Fatalf("retry: %d %v", n, err)
			}
		})
	}
}
