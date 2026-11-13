package trace

import (
	"context"
	"testing"
	"time"
)

func TestHearsayKeptRecordsPersistAcrossReopenAndAreReleased(t *testing.T) {
	ctx := context.Background()
	r, root, p := create(t)
	a := KeptRecord{Source: "work", Key: "workstream/w_1", Data: []byte(`{"source":"work","key":"workstream/w_1"}`)}
	b := KeptRecord{Source: "owner", Key: "document/w_1-spec", Data: []byte(`{"source":"owner","key":"document/w_1-spec"}`)}
	if err := r.KeepHearsay(ctx, []KeptRecord{a, b}, at); err != nil {
		t.Fatal(err)
	}
	before := commits(t, r)
	if err := r.KeepHearsay(ctx, []KeptRecord{a, b}, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if commits(t, r) != before {
		t.Fatal("keeping unchanged records wrote a commit")
	}
	if err := r.KeepHearsay(ctx, []KeptRecord{{Source: a.Source, Key: a.Key, Released: true}}, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	kept, err := r.HearsayKept()
	if err != nil || len(kept) != 2 {
		t.Fatalf("kept %v %v", kept, err)
	}
	if got := kept[KeptID("work", "workstream/w_1")]; !got.Released || len(got.Data) != 0 {
		t.Fatalf("released entry %+v", got)
	}
	if got := kept[KeptID("owner", "document/w_1-spec")]; got.Released || string(got.Data) != string(b.Data) {
		t.Fatalf("kept entry %+v", got)
	}
	// Kept records belong to the project's own trace and are not digests.
	if posted, err := r.HearsayPosted(); err != nil || len(posted) != 0 {
		t.Fatalf("posted %v %v", posted, err)
	}
}
