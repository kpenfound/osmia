package trace

import (
	"context"
	"fmt"
	"testing"
)

func TestLatestProjectDocumentDecodesOnlyTheLatestRevision(t *testing.T) {
	r, _, _ := create(t)
	ctx := context.Background()
	if _, found, err := r.LatestProjectDocument(EntitiesDocument, EntitiesPath); err != nil || found {
		t.Fatalf("no revision: found %v, %v", found, err)
	}
	const revisions = 40
	for i := 1; i <= revisions; i++ {
		d := Document{Header: Header{Schema: "osmia.trace.document", Version: Version, ID: EntitiesDocument, Revision: i, Project: projectID, At: at, Actor: owner, Cause: "test"}, Path: EntitiesPath, Content: fmt.Sprintf("{\"version\": 1, \"entities\": [], \"n\": %d}\n", i)}
		if err := r.Append(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	before := r.decoded.Load()
	d, found, err := r.LatestProjectDocument(EntitiesDocument, EntitiesPath)
	if err != nil || !found || d.Revision != revisions {
		t.Fatalf("latest: %+v found %v, %v", d, found, err)
	}
	if n := r.decoded.Load() - before; n != 1 {
		t.Fatalf("reading the latest of %d revisions decoded %d records, want 1", revisions, n)
	}
}
