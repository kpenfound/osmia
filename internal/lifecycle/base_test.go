package lifecycle_test

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

func TestHandInBaseIsPinnedToItsIdempotencyKey(t *testing.T) {
	t.Parallel()
	opts, _ := journey.ConversationFixture(t, "base-hand-")
	s, c := journey.StartT(t, opts)
	text := "Build on the earlier feature"
	req := api.HandInRequest{Project: fixturekit.Project, Key: "dependent", Stdin: &text, Base: fixturekit.Stream}
	first, err := c.HandIn(context.Background(), req)
	fixturekit.Must(t, err)
	second, err := c.HandIn(context.Background(), req)
	fixturekit.Must(t, err)
	if first.Workstream != second.Workstream {
		t.Fatal("retry created another workstream")
	}
	req.Base = fixturekit.Quiet
	_, err = c.HandIn(context.Background(), req)
	fixturekit.AssertCode(t, err, api.Conflict)
	repo, err := s.Repository(fixturekit.Project)
	fixturekit.Must(t, err)
	base, err := repo.WorkstreamBase(first.Workstream)
	fixturekit.Must(t, err)
	if base.Base != fixturekit.Stream || base.Revision != 1 {
		t.Fatalf("base %+v", base)
	}
	_, err = repo.SetWorkstreamBase(context.Background(), first.Workstream, fixturekit.Quiet, base.Revision, s.Now())
	fixturekit.Must(t, err)
	req.Base = fixturekit.Stream
	_, err = c.HandIn(context.Background(), req)
	fixturekit.Must(t, err)
	req.Base = fixturekit.Quiet
	_, err = c.HandIn(context.Background(), req)
	fixturekit.AssertCode(t, err, api.Conflict)
	req.Key = "missing"
	req.Base = config.WorkstreamID("w_ffffffffffffffffffffffffffffffff")
	_, err = c.HandIn(context.Background(), req)
	fixturekit.AssertCode(t, err, api.Validation)
}
