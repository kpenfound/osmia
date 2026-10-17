package trace

import (
	"context"
	"errors"
	"io/fs"
	"testing"
	"time"
)

func charterRevisions(t *testing.T, r *Repository) []Document {
	t.Helper()
	docs, err := Read[Document](r, "")
	if err != nil {
		t.Fatal(err)
	}
	var charter []Document
	for _, d := range docs {
		if d.ID == "charter" {
			charter = append(charter, d)
		}
	}
	return charter
}

func TestCharterReturnsTheLatestRevision(t *testing.T) {
	r, root, p := create(t)
	ctx := context.Background()
	later := at.Add(time.Hour)

	first, err := r.Charter(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := Document{Header: Header{Schema: "osmia.trace.document", Version: Version, ID: "charter", Revision: 1, Project: p.ID, At: at, Actor: owner, Cause: "project-create"}, Path: "charter.md", Content: CharterTemplate}
	if first != want {
		t.Fatalf("creation revision: %+v", first)
	}
	before := commits(t, r)
	for range 3 {
		again, err := r.Charter(ctx)
		if err != nil || again != first {
			t.Fatalf("read: %+v %v", again, err)
		}
	}
	if docs := charterRevisions(t, r); len(docs) != 1 || commits(t, r) != before {
		t.Fatalf("reads recorded revisions: %+v", docs)
	}

	edited := CharterTemplate + "\n1. Keep changes small.\n"
	second, err := r.EditCharter(ctx, 1, edited, later)
	if err != nil || second.Revision != 2 || second.Content != edited || second.Actor != owner || second.ID != "charter" || second.Cause != "owner-edit" || !second.At.Equal(later) {
		t.Fatalf("edit: %+v %v", second, err)
	}
	if again, err := r.Charter(ctx); err != nil || again != second {
		t.Fatalf("read after edit: %+v %v", again, err)
	}
	docs := charterRevisions(t, r)
	if len(docs) != 2 || docs[0] != first || docs[1] != second {
		t.Fatalf("revisions: %+v", docs)
	}
	if got := fileOf(t, r, "charter.md"); got != edited {
		t.Fatalf("charter.md reads %q", got)
	}

	// Reopening finds the recorded revision.
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	if got, err := reopened.Charter(ctx); err != nil || got != second {
		t.Fatalf("after reopen: %+v %v", got, err)
	}
}

func TestEditCharterComparesRevisions(t *testing.T) {
	r, _, _ := create(t)
	ctx := context.Background()
	before := commits(t, r)
	// Unchanged content records nothing.
	if got, err := r.EditCharter(ctx, 1, CharterTemplate, at); err != nil || got.Revision != 1 {
		t.Fatalf("unchanged edit: %+v %v", got, err)
	}
	if commits(t, r) != before {
		t.Fatal("an unchanged edit committed")
	}
	// An edit of any revision but the latest is a conflict.
	for _, revision := range []int{0, 2, 3} {
		if _, err := r.EditCharter(ctx, revision, "1. Rule.\n", at); !errors.Is(err, ErrConflict) {
			t.Fatalf("revision %d: %v", revision, err)
		}
	}
	if _, err := r.EditCharter(ctx, 1, "1. Rule.\n", at); err != nil {
		t.Fatal(err)
	}
	if _, err := r.EditCharter(ctx, 1, "1. Stale rule.\n", at); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale edit: %v", err)
	}
	// Reverting to the template is a new revision.
	got, err := r.EditCharter(ctx, 2, CharterTemplate, at)
	if err != nil || got.Revision != 3 || got.Content != CharterTemplate || got.Cause != "owner-edit" {
		t.Fatalf("revert: %+v %v", got, err)
	}
	if docs := charterRevisions(t, r); len(docs) != 3 || commits(t, r) != before+2 {
		t.Fatalf("revisions: %+v", docs)
	}
}

func TestCharterContinuesAnAppendedRevision(t *testing.T) {
	r, _, p := create(t)
	ctx := context.Background()
	d := Document{Header: header("document", "charter"), Path: "charter.md", Content: "1. Recorded rule.\n"}
	d.Workstream = ""
	d.Revision = 2
	if err := r.Append(ctx, d); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Charter(ctx); err != nil || got != d {
		t.Fatalf("appended revision: %+v %v", got, err)
	}
	next := charterRevision(p.ID, at, Actor{Kind: "service", ID: "controller"}, "amendment", 2, "1. Amended.\n")
	if err := r.Append(ctx, next); !errors.Is(err, ErrConflict) {
		t.Fatalf("a repeated revision was appended: %v", err)
	}
	next.Revision = 3
	if err := r.Append(ctx, next); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Charter(ctx); err != nil || got != next {
		t.Fatalf("after amendment: %+v %v", got, err)
	}
	if docs := charterRevisions(t, r); len(docs) != 3 {
		t.Fatalf("%+v", docs)
	}
}

func TestCharterWithoutRecordedRevision(t *testing.T) {
	r, _, _ := create(t)
	ctx := context.Background()
	storeExec(t, r, "DELETE FROM records WHERE path = 'documents.jsonl' AND id = 'charter'")
	if got, err := r.Charter(ctx); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a charter with no revision: %+v %v", got, err)
	}
	if _, err := r.EditCharter(ctx, 1, "1. Rule.\n", at); !errors.Is(err, ErrConflict) {
		t.Fatalf("edit of a missing charter: %v", err)
	}
}
