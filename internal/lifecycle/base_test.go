package lifecycle_test

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
)

func TestHandInBaseIsPinnedToItsIdempotencyKey(t *testing.T) {
	t.Parallel()
	opts, _ := conversationFixture(t, "base-hand-")
	s, c := start(t, opts)
	text := "Build on the earlier feature"
	req := HandInRequest{Project: project, Key: "dependent", Stdin: &text, Base: stream}
	first, err := c.HandIn(context.Background(), req)
	must(t, err)
	second, err := c.HandIn(context.Background(), req)
	must(t, err)
	if first.Workstream != second.Workstream {
		t.Fatal("retry created another workstream")
	}
	req.Base = quiet
	_, err = c.HandIn(context.Background(), req)
	assertCode(t, err, Conflict)
	repo, err := s.repository(project)
	must(t, err)
	base, err := repo.WorkstreamBase(first.Workstream)
	must(t, err)
	if base.Base != stream || base.Revision != 1 {
		t.Fatalf("base %+v", base)
	}
	_, err = repo.SetWorkstreamBase(context.Background(), first.Workstream, quiet, base.Revision, s.now())
	must(t, err)
	req.Base = stream
	_, err = c.HandIn(context.Background(), req)
	must(t, err)
	req.Base = quiet
	_, err = c.HandIn(context.Background(), req)
	assertCode(t, err, Conflict)
	req.Key = "missing"
	req.Base = config.WorkstreamID("w_ffffffffffffffffffffffffffffffff")
	_, err = c.HandIn(context.Background(), req)
	assertCode(t, err, Validation)
}
