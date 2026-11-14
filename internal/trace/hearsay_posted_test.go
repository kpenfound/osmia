package trace

import (
	"context"
	"testing"
	"time"
)

func TestHearsayPostedDigestsPersistAcrossReopenAndSkipUnchangedEntries(t *testing.T) {
	ctx := context.Background()
	r, root, p := create(t)
	entry := PostedRecord{Source: "work", Key: "project/p_1", Digest: "d1", UpdatedAt: at}
	other := PostedRecord{Source: "owner", Key: "document/charter", Digest: "d2", UpdatedAt: at}
	if err := r.RecordHearsayPosted(ctx, []PostedRecord{entry, other}, at); err != nil {
		t.Fatal(err)
	}
	before := commits(t, r)
	if err := r.RecordHearsayPosted(ctx, []PostedRecord{entry, other}, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if commits(t, r) != before {
		t.Fatal("recording unchanged digests wrote a commit")
	}
	entry.Digest = "d3"
	if err := r.RecordHearsayPosted(ctx, []PostedRecord{entry}, at.Add(time.Hour)); err != nil {
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
	posted, err := r.HearsayPosted()
	if err != nil || len(posted) != 2 {
		t.Fatalf("posted %v %v", posted, err)
	}
	if got := posted[PostedID("work", "project/p_1")]; got.Digest != "d3" {
		t.Fatalf("latest digest %+v", got)
	}
	if got := posted[PostedID("owner", "document/charter")]; got.Digest != "d2" || !got.UpdatedAt.Equal(at) {
		t.Fatalf("other digest %+v", got)
	}
	// The same key on another source is another artifact.
	if PostedID("work", "k") == PostedID("owner", "k") {
		t.Fatal("source is not part of the identity")
	}
}

func TestHearsayPostedDigestsBelongToTheProjectTrace(t *testing.T) {
	r, _, _ := create(t)
	defer r.Close()
	scoped, err := r.scoped(streamID)
	if err != nil {
		t.Fatal(err)
	}
	if err := scoped.RecordHearsayPosted(context.Background(), []PostedRecord{{Source: "work", Key: "k", Digest: "d"}}, at); err == nil {
		t.Fatal("a workstream trace recorded a posted digest")
	}
}

func TestHearsayPostedKeepsTheLastOfSeveralEntriesForOneArtifact(t *testing.T) {
	ctx := context.Background()
	r, _, _ := create(t)
	defer r.Close()
	older := PostedRecord{Source: "owner", Key: "document/charter", Digest: "d1", UpdatedAt: at}
	newer := PostedRecord{Source: "owner", Key: "document/charter", Digest: "d2", UpdatedAt: at.Add(time.Hour)}
	if err := r.RecordHearsayPosted(ctx, []PostedRecord{older, newer}, at); err != nil {
		t.Fatal(err)
	}
	posted, err := r.HearsayPosted()
	if err != nil {
		t.Fatal(err)
	}
	if got := posted[PostedID("owner", "document/charter")]; got.Digest != "d2" || !got.UpdatedAt.Equal(at.Add(time.Hour)) {
		t.Fatalf("recorded %+v, want the last entry", got)
	}
	before := commits(t, r)
	if err := r.RecordHearsayPosted(ctx, []PostedRecord{older, newer}, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if commits(t, r) != before {
		t.Fatal("recording the same entries again wrote a commit")
	}
}
