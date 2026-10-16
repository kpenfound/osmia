package events

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestEventDeliveryRechecksChiefTransferredAfterResolution(t *testing.T) {
	t.Parallel()
	for _, transfer := range []bool{true, false} {
		name := "unexplained-missing-chief"
		if transfer {
			name = "transferred-chief"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f, repo := setup(t)
			defer repo.Close()
			other := config.WorkstreamID("w_00000000000000000000000000000002")
			must(t, repo.CreateWorkstream(ctx, other, start, owner))
			members := []trace.CrossRepoMember{{Workstream: stream, Project: repo}, {Workstream: other, Project: repo}}
			wt, g, err := trace.CreateWorkgroupTrace(ctx, f.root, members, start, owner, "draft-workgroup")
			must(t, err)
			defer wt.Close()
			f.notify(t, repo, "join-notice")
			f.clock.Advance(time.Minute)
			d, err := New(repo, Options{Now: f.clock.Now, Window: time.Second, Profile: func() (coreadapter.Profile, error) { return profile, nil }, ChiefTarget: func(context.Context, config.WorkstreamID) (*trace.Repository, func(), error) {
				id, err := repo.CrossRepoWorkgroupOf(stream)
				if err != nil {
					return nil, func() {}, err
				}
				if id != "" {
					return wt, func() {}, nil
				}
				return repo, func() {}, nil
			}})
			must(t, err)
			fired := false
			d.boundary = func(step string) error {
				if step != "chief-target-resolved" || fired {
					return nil
				}
				fired = true
				if transfer {
					return repo.JoinCrossRepoWorkgroup(ctx, stream, g.ID, f.clock.Now(), owner, "draft-workgroup")
				}
				return repo.RetractChiefOfStaff(ctx, stream)
			}
			err = d.Pass(ctx)
			if !fired {
				t.Fatal("delivery did not resolve its chief")
			}
			if !transfer {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unexplained absence was hidden: %v", err)
				}
				return
			}
			must(t, err)
			out, err := repo.Outbox(stream)
			must(t, err)
			if len(out) != 1 || out[0].Acknowledged || out[0].Claim == nil {
				t.Fatalf("transfer lost its retryable event: %+v", out)
			}
			must(t, d.Pass(ctx))
			chief, err := wt.ChiefOfStaffThread(stream)
			must(t, err)
			if len(chief.Turns) != 1 {
				t.Fatalf("shared delivery count = %d", len(chief.Turns))
			}
			must(t, d.Pass(ctx))
			chief, err = wt.ChiefOfStaffThread(stream)
			must(t, err)
			if len(chief.Turns) != 1 {
				t.Fatal("shared delivery repeated")
			}
		})
	}
}
