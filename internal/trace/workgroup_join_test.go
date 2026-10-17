package trace

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
)

func TestJoinCrossRepoWorkgroupPublishesLocatorAndChiefTogether(t *testing.T) {
	for _, step := range []string{boundaryBeforeCommit, boundaryCommitted} {
		t.Run(step, func(t *testing.T) {
			r, root, p := create(t)
			sr := mustScoped(t, r, streamID)
			const group config.WorkgroupID = "g_00000000000000000000000000000001"
			injected := errors.New("interrupted member transfer")
			sr.failPublication = func(name string) error {
				if name == step {
					return injected
				}
				return nil
			}
			if err := r.JoinCrossRepoWorkgroup(context.Background(), streamID, group, at, owner, "draft-workgroup"); !errors.Is(err, injected) {
				t.Fatalf("interruption: %v", err)
			}
			sr.failPublication = nil
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(root, p)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			pointer, err := reopened.CrossRepoWorkgroupOf(streamID)
			if err != nil {
				t.Fatal(err)
			}
			_, chiefErr := reopened.ChiefOfStaffThread(streamID)
			if step == boundaryBeforeCommit {
				if pointer != "" || chiefErr != nil {
					t.Fatalf("partial unpublished transfer: %s %v", pointer, chiefErr)
				}
			} else if pointer != group || !errors.Is(chiefErr, os.ErrNotExist) {
				t.Fatalf("partial published transfer: %s %v", pointer, chiefErr)
			}
			if err := reopened.JoinCrossRepoWorkgroup(context.Background(), streamID, group, at, owner, "draft-workgroup"); err != nil {
				t.Fatal(err)
			}
			before, err := Read[Transition](reopened, streamID)
			if err != nil {
				t.Fatal(err)
			}
			if err := reopened.JoinCrossRepoWorkgroup(context.Background(), streamID, group, at, owner, "draft-workgroup"); err != nil {
				t.Fatal(err)
			}
			after, err := Read[Transition](reopened, streamID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("retry changed join provenance")
			}
			if err := reopened.JoinCrossRepoWorkgroup(context.Background(), streamID, "g_00000000000000000000000000000002", at, owner, "draft-workgroup"); err == nil {
				t.Fatal("member joined a second group")
			}
		})
	}
}

func TestPendingCrossRepoWorkgroupRequiresRecordedRetractionAndAuthority(t *testing.T) {
	root, r1, r2, _, _ := crossRepoFixture(t)
	ctx := context.Background()
	_, err := CreateWorkgroup(ctx, r1, []config.WorkstreamID{crossMember1}, at, owner, "handin")
	if err != nil {
		t.Fatal(err)
	}
	authority, g, err := CreateWorkgroupTrace(ctx, root, []CrossRepoMember{{crossMember1, r1}, {crossMember2, r2}}, at, owner, "draft-workgroup")
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	if id, err := r1.PendingCrossRepoWorkgroup(crossMember1); err != nil || id != "" {
		t.Fatalf("membership alone authenticated join: %s %v", id, err)
	}
	if err := r1.RetractWorkgroup(ctx, crossMember1, at, owner, "draft-workgroup"); err != nil {
		t.Fatal(err)
	}
	if id, err := r1.PendingCrossRepoWorkgroup(crossMember1); err != nil || id != g.ID {
		t.Fatalf("recorded join: %s %v", id, err)
	}
	// The group exists and is readable, but doesn't contain this other retraction.
	bare := config.WorkstreamID("w_00000000000000000000000000000129")
	if err := r1.CreateWorkstream(ctx, bare, at, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateWorkgroup(ctx, r1, []config.WorkstreamID{bare}, at, owner, "handin"); err != nil {
		t.Fatal(err)
	}
	if err := r1.RetractWorkgroup(ctx, bare, at, owner, "draft-workgroup"); err != nil {
		t.Fatal(err)
	}
	if id, err := r1.PendingCrossRepoWorkgroup(bare); err == nil || id != "" {
		t.Fatalf("unmatched authority accepted: %s %v", id, err)
	}
}
