package trace

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
)

// PurgeWorkstream(s) rewrote the project trace's own history to erase a
// workstream's content, under the shared layout where every workstream's
// files lived inside the project's own trace repository. Now that each
// workstream has its own dedicated trace repository, the project repository
// never has a "workstreams/<id>" path to rewrite, so the project trace
// instance's history-rewriting purge is a safe no-op: it neither errs nor
// changes anything. Replacing it with deletion of a workstream's independent
// storage, and the deferral rules that gate it, belongs to
// archive-independent-deletion.
func TestPurgeWorkstreamIsANoOpOnTheProjectTrace(t *testing.T) {
	r, _, _ := create(t)
	ctx := context.Background()
	before, err := r.git(ctx, nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.PurgeWorkstream(ctx, streamID); err != nil {
		t.Fatal(err)
	}
	after, err := r.git(ctx, nil, "rev-parse", "HEAD")
	if err != nil || after != before {
		t.Fatalf("project trace changed by a no-op purge: %s, was %s (%v)", after, before, err)
	}
	s := mustScoped(t, r, streamID)
	if _, err := s.dir.Stat("workstreams/" + string(streamID)); err != nil {
		t.Fatalf("workstream trace removed by a no-op purge: %v", err)
	}
	docs, err := Read[Document](r, streamID)
	if err != nil {
		t.Fatalf("workstream unreadable after a no-op purge: %v", err)
	}
	_ = docs
}

func TestPurgeWorkstreamsIsANoOpOnTheProjectTrace(t *testing.T) {
	r, _, _ := create(t)
	ctx := context.Background()
	other := config.WorkstreamID("w_00000000000000000000000000000002")
	if err := r.CreateWorkstream(ctx, other, at, owner); err != nil {
		t.Fatal(err)
	}
	before, err := r.git(ctx, nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.PurgeWorkstreams(ctx, []config.WorkstreamID{streamID, other}); err != nil {
		t.Fatal(err)
	}
	after, err := r.git(ctx, nil, "rev-parse", "HEAD")
	if err != nil || after != before {
		t.Fatalf("project trace changed by a no-op purge: %s, was %s (%v)", after, before, err)
	}
	if streams, err := r.Workstreams(); err != nil || len(streams) != 2 {
		t.Fatalf("workstreams removed by a no-op purge: %v %v", streams, err)
	}
}
