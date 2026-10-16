package events

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestRetiredContextKeepsEventsPending(t *testing.T) {
	for _, phase := range []string{"target", "system"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			f, repo := setup(t)
			defer repo.Close()
			f.notify(t, repo, "pending")
			f.clock.Advance(time.Minute)
			before, err := repo.Outbox(stream)
			must(t, err)
			unavailable := fmt.Errorf("%w: no live member context", trace.ErrMemberRetired)
			opts := Options{Now: f.clock.Now, Window: time.Second, Profile: func() (coreadapter.Profile, error) { return profile, nil }}
			if phase == "target" {
				opts.ChiefTarget = func(context.Context, config.WorkstreamID) (*trace.Repository, func(), error) {
					return nil, func() {}, unavailable
				}
			} else {
				opts.System = func(context.Context, config.WorkstreamID) (string, error) { return "", unavailable }
			}
			d, err := New(repo, opts)
			must(t, err)
			must(t, d.Pass(ctx))
			after, err := repo.Outbox(stream)
			must(t, err)
			if !reflect.DeepEqual(before, after) || len(eventTurns(t, repo)) != 0 {
				t.Fatal("held context consumed events or queued a turn")
			}
			unavailable = errors.New("live authority is corrupt")
			if err := d.Pass(ctx); !errors.Is(err, unavailable) {
				t.Fatalf("live authority failure hidden: %v", err)
			}
		})
	}
}
