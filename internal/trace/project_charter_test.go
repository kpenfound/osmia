package trace

import (
	"context"
	"testing"
)

func TestProjectsOwnIndependentCharters(t *testing.T) {
	primary, root, p := create(t)
	p.ID = "p_00000000000000000000000000000002"
	repo, err := Create(context.Background(), root, p, at, owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	initial, err := repo.Charter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if initial.Project != p.ID || initial.Revision != 1 {
		t.Fatalf("project charter: %+v", initial)
	}
	_, err = primary.EditCharter(context.Background(), 1, "1. First project policy.\n", at)
	if err != nil {
		t.Fatal(err)
	}
	got, err := repo.Charter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != initial {
		t.Fatal("another project's edit changed the charter")
	}
	edited, err := repo.EditCharter(context.Background(), 1, "1. Second project policy.\n", at)
	if err != nil {
		t.Fatal(err)
	}
	if edited.Project != p.ID || edited.Revision != 2 {
		t.Fatalf("project edit: %+v", edited)
	}
	if docs := charterRevisions(t, repo); len(docs) != 2 || docs[0] != initial || docs[1] != edited {
		t.Fatalf("history: %+v", docs)
	}
}
