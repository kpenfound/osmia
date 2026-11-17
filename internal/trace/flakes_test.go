package trace

import (
	"context"
	"errors"
	"testing"
)

func TestFlakeRecordIsAProjectDocumentWithRevisions(t *testing.T) {
	r, _, _ := create(t)
	ctx := context.Background()
	const group = "0123456789abcdef0123456789abcdef"
	doc := func(path string, revision int, stream string) Document {
		h := Header{Schema: "osmia.trace.document", Version: Version, ID: FlakeDocumentID(group), Revision: revision, Project: projectID, At: at, Actor: Actor{Kind: "service", ID: "flakes"}, Cause: "test"}
		if stream != "" {
			h.Workstream = streamID
		}
		return Document{Header: h, Path: path, Content: "{}\n"}
	}
	for revision := 1; revision <= 2; revision++ {
		if err := r.Append(ctx, doc(FlakePath(group), revision, "")); err != nil {
			t.Fatalf("revision %d: %v", revision, err)
		}
	}
	got, found, err := r.LatestProjectDocument(FlakeDocumentID(group), FlakePath(group))
	if err != nil || !found || got.Revision != 2 {
		t.Fatalf("latest: %+v %v %v", got, found, err)
	}
	if err := r.Append(ctx, doc(FlakePath(group), 2, "")); !errors.Is(err, ErrConflict) {
		t.Fatalf("a revision recorded twice: %v", err)
	}
	for _, path := range []string{"flakes/nested/" + group + ".json", "flakes/" + group + ".md", "flakes/.json", "flake/" + group + ".json"} {
		if err := r.Append(ctx, doc(path, 1, "")); err == nil {
			t.Errorf("recorded the project document %s", path)
		}
	}
	if err := r.Append(ctx, doc(FlakePath(group), 1, "workstream")); err == nil {
		t.Error("recorded a flake record inside a workstream")
	}
}
