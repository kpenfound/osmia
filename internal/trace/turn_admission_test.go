package trace

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestTurnAdmissionOrdersWithRetirement(t *testing.T) {
	t.Parallel()
	for _, claimFirst := range []bool{false, true} {
		name := "retirement first"
		if claimFirst {
			name = "claim first"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			r, root, project := create(t)
			if err := r.CreateThread(ctx, threadAgent()); err != nil {
				t.Fatal(err)
			}
			tx := transaction("draft-amendment", 0, "", "drafting", 0)
			tx.Transition.Subject = "amendment_1"
			transact(t, r, tx)
			// The operation supplies the admission condition when delivering
			// a queued request, including when it replays after a restart.
			enqueue(t, r, "one")
			admitted := WithTurnAdmission(ctx, streamID, "amendment_1", "drafting")
			var claimed QueuedTurn
			if claimFirst {
				var err error
				claimed, err = r.ClaimTurn(admitted, streamID, "mason", "claim", "/session", at)
				if err != nil {
					t.Fatal(err)
				}
			}
			tx = transaction("retire-amendment", 1, "drafting", "rejected", 0)
			tx.Transition.Subject = "amendment_1"
			decision := Document{Header: header("document", "decision"), Path: "amendments/1/decision.json", Content: "rejected"}
			if _, err := r.RecordDocumentsWith(ctx, []Document{decision}, tx); err != nil {
				t.Fatal(err)
			}
			if claimFirst {
				// Retirement does not revoke a claim that won the lock, nor
				// prevent its captured result from completing durably.
				again, err := r.ClaimTurn(admitted, streamID, "mason", "claim", "/session", at)
				if err != nil || !reflect.DeepEqual(again, claimed) {
					t.Fatalf("claim replay: %+v %v", again, err)
				}
				if err := r.CaptureTurn(admitted, "claim", threadResponse(claimed)); err != nil {
					t.Fatal(err)
				}
				if err := r.CompleteTurn(admitted, streamID, "mason", "one", "claim", at.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			for _, restart := range []bool{false, true} {
				if restart {
					if err := r.Close(); err != nil {
						t.Fatal(err)
					}
					var err error
					r, err = Open(root, project)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { r.Close() })
					admitted = WithTurnAdmission(ctx, streamID, "amendment_1", "drafting")
				}
				before := mustThread(t, r)
				writes := 0
				r.Observe(func(Commit) { writes++ })
				if _, err := r.EnqueueTurn(admitted, threadRequest("two")); !errors.Is(err, ErrConflict) {
					t.Fatalf("enqueue after retirement (restart %v): %v", restart, err)
				}
				if _, err := r.ClaimTurn(admitted, streamID, "mason", "next", "/next", at); !errors.Is(err, ErrConflict) {
					t.Fatalf("claim after retirement (restart %v): %v", restart, err)
				}
				if _, err := r.EnqueueTurn(admitted, threadRequest("one")); err != nil {
					t.Fatalf("accepted request replay: %v", err)
				}
				if after := mustThread(t, r); writes != 0 || !reflect.DeepEqual(before, after) {
					t.Fatalf("refused admission recorded work: %d writes, %+v -> %+v", writes, before, after)
				}
			}
		})
	}
}
