package trace

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestCancelThreadTurnsPreservesOtherWorkAndLiveClaims(t *testing.T) {
	for _, scenario := range []string{"queued", "live claim", "other active turn", "previous session", "captured result"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			r, root, p := create(t)
			if err := r.CreateThread(ctx, threadAgent()); err != nil {
				t.Fatal(err)
			}
			enqueue(t, r, "one")
			enqueue(t, r, "two")
			enqueue(t, r, "three")
			selected := []string{"one", "two"}
			if scenario != "queued" {
				q := claimTurn(t, r, "claim")
				if scenario == "captured result" {
					if err := r.CaptureTurn(ctx, q.Claim.Token, threadResponse(q)); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "other active turn" {
					selected = []string{"two"}
				}
			}
			if scenario == "previous session" || scenario == "captured result" {
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
			n, err := r.CancelThreadTurns(ctx, streamID, "mason", selected, at.Add(time.Minute), Actor{Kind: "service", ID: "amendments"}, "retired")
			if err != nil {
				t.Fatal(err)
			}
			after := mustThread(t, r)
			if scenario != "queued" && scenario != "previous session" {
				if n != 0 || !reflect.DeepEqual(before, after) {
					t.Fatalf("protected work changed: %d %+v", n, after)
				}
			} else {
				if n != 2 || !reflect.DeepEqual(before.Turns[2], after.Turns[2]) {
					t.Fatalf("scoped cancellation %d %+v", n, after)
				}
				for _, q := range after.Turns[:2] {
					if q.CompletedAt.IsZero() || q.Response == nil || !q.Response.Result.Cancelled || q.Response.Actor.ID != "amendments" {
						t.Fatalf("unfinished turn %+v", q)
					}
				}
			}
		})
	}
}
