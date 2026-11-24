package trace

import (
	"context"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

func TestNewestWorkstreamCreated(t *testing.T) {
	ctx := context.Background()
	root, p := fixture(t)
	r, err := Create(ctx, root, p, at, owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	if _, found, err := r.NewestWorkstreamCreated(); err != nil || found {
		t.Fatalf("no workstreams: found %v, %v", found, err)
	}
	if err := r.CreateWorkstream(ctx, streamID, at.Add(time.Hour), owner); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateSessionWorkstream(ctx, secondStream, "default", "base", "branch", "", at.Add(3*time.Hour), owner); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateWorkstream(ctx, thirdStream, at.Add(2*time.Hour), owner); err != nil {
		t.Fatal(err)
	}
	decoded := func() (n uint64) {
		for _, id := range []string{"", string(streamID), string(secondStream), string(thirdStream)} {
			count, err := r.RecordDecodeCount(config.WorkstreamID(id))
			if err != nil {
				t.Fatal(err)
			}
			n += count
		}
		return n
	}
	before := decoded()
	for i := 0; i < 2; i++ {
		got, found, err := r.NewestWorkstreamCreated()
		if err != nil || !found || !got.Equal(at.Add(3*time.Hour)) {
			t.Fatalf("read %d: %v %v %v", i, got, found, err)
		}
	}
	if after := decoded(); after != before {
		t.Fatalf("the newest creation time decoded %d stored records", after-before)
	}
}
