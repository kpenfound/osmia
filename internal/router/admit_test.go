package router_test

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/router"
)

func TestAdmitHoldsAPinnedExitWithoutRecordingAnything(t *testing.T) {
	pinned := func(f *fx) router.Exit {
		x := f.exit()
		x.Trigger, x.Actor = "unit.move_owner", owner
		x.Authority = &router.Authority{Kind: router.AuthorityOwner, Target: "implementing"}
		x.Targets[0].Works = true
		return x
	}
	f := newFx(t, &jevfixture.Transitions{})
	f.seed("checking")
	f.track("implementing")
	ctx := context.Background()

	if adm, err := f.router.Admit(ctx, pinned(f)); err != nil || !adm.Admitted {
		t.Fatalf("a pinned exit with nothing in its way is %+v: %v", adm, err)
	}

	f.router.Paused = func(context.Context, router.Exit) (string, error) { return "work is paused", nil }
	if adm, err := f.router.Admit(ctx, pinned(f)); err != nil || adm.Admitted || adm.Hold != router.HoldPaused {
		t.Fatalf("a pause is %+v: %v", adm, err)
	}
	f.router.Paused = nil

	f.router.Stops = func(context.Context, router.Exit) (string, error) { return "the budget is spent", nil }
	if adm, err := f.router.Admit(ctx, pinned(f)); err != nil || adm.Admitted || adm.Hold != router.HoldBusy {
		t.Fatalf("a spent budget is %+v: %v", adm, err)
	}
	f.router.Stops = nil

	busy := pinned(f)
	busy.Prereq.Operations = func(context.Context, router.Target) (string, error) { return "a landing is in flight", nil }
	if adm, err := f.router.Admit(ctx, busy); err != nil || adm.Admitted || adm.Hold != router.HoldBusy || adm.Reason != "a landing is in flight" {
		t.Fatalf("an operation in flight is %+v: %v", adm, err)
	}

	stale := pinned(f)
	stale.Version++
	if adm, err := f.router.Admit(ctx, stale); err != nil || adm.Admitted || adm.Hold != router.HoldStale {
		t.Fatalf("a subject that moved is %+v: %v", adm, err)
	}

	proposal := f.exit()
	if _, err := f.router.Admit(ctx, proposal); err == nil {
		t.Fatal("an exit without a pinned authority was admitted ahead of its effect")
	}

	if len(f.moves()) != 0 || f.ran("implementing") != 0 || len(f.notices()) != 0 || len(f.provider.Requests()) != 0 {
		t.Fatalf("admitting recorded %d moves, ran %d effects, wrote %d notices and asked Jev %d times", len(f.moves()), f.ran("implementing"), len(f.notices()), len(f.provider.Requests()))
	}
}
