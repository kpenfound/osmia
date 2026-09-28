package trace

import (
	"context"
	"errors"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
)

func TestWorkstreamBasesAreAcyclicPinnedAndDurable(t *testing.T) {
	ctx := context.Background()
	r, root, p := create(t)
	second := config.WorkstreamID("w_00000000000000000000000000000002")
	third := config.WorkstreamID("w_00000000000000000000000000000003")
	for _, id := range []config.WorkstreamID{second, third} {
		if err := r.CreateWorkstream(ctx, id, at, owner); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.SetWorkstreamBase(ctx, second, streamID, 0, at); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetWorkstreamBase(ctx, third, second, 0, at); err != nil {
		t.Fatal(err)
	}
	for _, base := range []config.WorkstreamID{streamID, third, "w_ffffffffffffffffffffffffffffffff"} {
		if _, err := r.SetWorkstreamBase(ctx, streamID, base, 0, at); err == nil {
			t.Fatalf("invalid base %s accepted", base)
		}
	}
	if _, err := r.SetWorkstreamBase(ctx, second, third, 0, at); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale revision: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	base, err := r.WorkstreamBase(third)
	if err != nil || base.Base != second || base.Revision != 1 {
		t.Fatalf("recovered %+v %v", base, err)
	}
	if _, err := r.SetWorkstreamBase(ctx, third, "", 1, at); err != nil {
		t.Fatal(err)
	}
	h := header("transition", "ratify")
	h.Workstream = third
	if _, err := r.SetFeatureState(ctx, h, "ratified", "Owner ratified"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetWorkstreamBase(ctx, third, second, 2, at); !errors.Is(err, ErrConflict) {
		t.Fatalf("sealed base edit: %v", err)
	}
}
