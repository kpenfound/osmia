package trace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

func TestCharterRecordsOwnerEdits(t *testing.T) {
	r, root, p := create(t)
	ctx := context.Background()
	path := filepath.Join(root.String(), "projects", string(p.ID), "charter.md")
	later := at.Add(time.Hour)

	first, err := r.Charter(ctx, later)
	if err != nil {
		t.Fatal(err)
	}
	want := Document{Header: Header{Schema: "osmia.trace.document", Version: Version, ID: "charter", Revision: 1, Project: p.ID, At: at, Actor: owner, Cause: "project-create"}, Path: "charter.md", Content: CharterTemplate}
	if first != want {
		t.Fatalf("creation revision: %+v", first)
	}
	for range 3 {
		again, err := r.Charter(ctx, later.Add(time.Hour))
		if err != nil || again != first {
			t.Fatalf("read without an edit: %+v %v", again, err)
		}
	}
	if docs := charterRevisions(t, r); len(docs) != 1 {
		t.Fatalf("reads without an edit recorded revisions: %+v", docs)
	}

	edited := CharterTemplate + "\n1. Keep changes small.\n"
	if err := os.WriteFile(path, []byte(edited), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := r.Charter(ctx, later)
	if err != nil || second.Revision != 2 || second.Content != edited || second.Actor != owner || second.ID != "charter" || second.Cause != "owner-edit" || !second.At.Equal(later) {
		t.Fatalf("edit: %+v %v", second, err)
	}
	if again, err := r.Charter(ctx, later); err != nil || again != second {
		t.Fatalf("read after edit: %+v %v", again, err)
	}
	docs := charterRevisions(t, r)
	if len(docs) != 2 || docs[0] != first || docs[1] != second {
		t.Fatalf("revisions: %+v", docs)
	}
	committed, err := r.git(ctx, nil, "show", "HEAD:charter.md")
	if err != nil || committed+"\n" != edited && committed != edited {
		t.Fatalf("edit not committed: %q %v", committed, err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != edited {
		t.Fatalf("file changed: %q %v", data, err)
	}

	// Reopening finds the recorded revision and records nothing new.
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	if got, err := reopened.Charter(ctx, later); err != nil || got != second {
		t.Fatalf("after reopen: %+v %v", got, err)
	}
}

func TestCharterContinuesAnAppendedRevision(t *testing.T) {
	r, root, p := create(t)
	ctx := context.Background()
	d := Document{Header: header("document", "charter"), Path: "charter.md", Content: "1. Recorded rule.\n"}
	d.Workstream = ""
	d.Revision = 2
	if err := r.Append(ctx, d); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Charter(ctx, at); err != nil || got != d {
		t.Fatalf("recorded content re-recorded: %+v %v", got, err)
	}
	path := filepath.Join(root.String(), "projects", string(p.ID), "charter.md")
	if err := os.WriteFile(path, []byte("1. Owner rule.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := r.Charter(ctx, at)
	if err != nil || got.Revision != 3 || got.Content != "1. Owner rule.\n" || got.Actor != owner {
		t.Fatalf("%+v %v", got, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Charter(ctx, at); err == nil {
		t.Fatal("missing charter read")
	}
	if docs := charterRevisions(t, r); len(docs) != 3 {
		t.Fatalf("%+v", docs)
	}
}

func TestUnrecordedCharterEditDoesNotBlockTrace(t *testing.T) {
	r, root, p := create(t)
	ctx := context.Background()
	path := filepath.Join(root.String(), "projects", string(p.ID), "charter.md")
	if err := os.WriteFile(path, []byte("1. Pending rule.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	kb := Document{Header: header("document", "kb-build"), Path: "kb/build.md", Content: "Build notes\n"}
	kb.Workstream = ""
	if err := r.Append(ctx, kb); err != nil {
		t.Fatal(err)
	}
	if committed, err := r.git(ctx, nil, "show", "HEAD:charter.md"); err != nil || committed+"\n" != CharterTemplate {
		t.Fatalf("unrelated append committed the edit: %q %v", committed, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	got, err := reopened.Charter(ctx, at)
	if err != nil || got.Revision != 2 || got.Content != "1. Pending rule.\n" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestCharterWithoutRecordedRevision(t *testing.T) {
	r, _, p := create(t)
	ctx := context.Background()
	if err := r.writeFile("documents.jsonl", nil); err != nil {
		t.Fatal(err)
	}
	if err := r.commit(ctx, []string{"documents.jsonl"}, "Drop charter record"); err != nil {
		t.Fatal(err)
	}
	got, err := r.Charter(ctx, at)
	want := charterRevision(p.ID, at, owner, "owner-edit", 1, CharterTemplate)
	if err != nil || got != want {
		t.Fatalf("%+v %v", got, err)
	}
	if docs := charterRevisions(t, r); len(docs) != 1 || docs[0] != want {
		t.Fatalf("%+v", docs)
	}
}

func TestCharterComparesWithLatestRevision(t *testing.T) {
	r, root, p := create(t)
	ctx := context.Background()
	path := filepath.Join(root.String(), "projects", string(p.ID), "charter.md")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(path, []byte("1. Rule.\n"), 0600))
	if got, err := r.Charter(ctx, at); err != nil || got.Revision != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	// Reverting to the template is an edit of revision 2.
	must(os.WriteFile(path, []byte(CharterTemplate), 0600))
	got, err := r.Charter(ctx, at)
	if err != nil || got.Revision != 3 || got.Content != CharterTemplate || got.Cause != "owner-edit" {
		t.Fatalf("revert: %+v %v", got, err)
	}
	if again, err := r.Charter(ctx, at); err != nil || again != got {
		t.Fatalf("read after revert: %+v %v", again, err)
	}
	if docs := charterRevisions(t, r); len(docs) != 3 {
		t.Fatalf("%+v", docs)
	}
}

func TestAppendRefusesToOverwriteOwnerEdit(t *testing.T) {
	r, root, p := create(t)
	ctx := context.Background()
	path := filepath.Join(root.String(), "projects", string(p.ID), "charter.md")
	if err := os.WriteFile(path, []byte("1. Owner rule.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	d := charterRevision(p.ID, at, Actor{Kind: "service", ID: "controller"}, "amendment", 2, "1. Amended.\n")
	if err := r.Append(ctx, d); !errors.Is(err, ErrConflict) {
		t.Fatalf("owner edit overwritten: %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "1. Owner rule.\n" {
		t.Fatalf("file: %q %v", data, err)
	}
	if docs := charterRevisions(t, r); len(docs) != 1 {
		t.Fatalf("%+v", docs)
	}
	// Once recorded, the next revision follows it.
	if _, err := r.Charter(ctx, at); err != nil {
		t.Fatal(err)
	}
	d.Revision = 3
	if err := r.Append(ctx, d); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "1. Amended.\n" {
		t.Fatalf("file: %q %v", data, err)
	}
}

func TestAppendRefusesCharterWithoutRecordedRevision(t *testing.T) {
	r, _, p := create(t)
	ctx := context.Background()
	if err := r.writeFile("documents.jsonl", nil); err != nil {
		t.Fatal(err)
	}
	if err := r.commit(ctx, []string{"documents.jsonl"}, "Drop charter record"); err != nil {
		t.Fatal(err)
	}
	d := charterRevision(p.ID, at, owner, "amendment", 1, "1. Rule.\n")
	if err := r.Append(ctx, d); !errors.Is(err, ErrConflict) {
		t.Fatalf("unrecorded charter overwritten: %v", err)
	}
}

// A record commits the bytes it records, even when the file changes before
// the commit reads the work tree.
func TestRecordCommitsRecordedBytes(t *testing.T) {
	r, root, p := create(t)
	ctx := context.Background()
	path := filepath.Join(root.String(), "projects", string(p.ID), "charter.md")
	if err := os.WriteFile(path, []byte("1. Recorded.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	next := charterRevision(p.ID, at, owner, "owner-edit", 2, "1. Recorded.\n")
	if err := os.WriteFile(path, []byte("1. Later edit.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	err := r.append(ctx, next, false)
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if committed, err := r.git(ctx, nil, "show", "HEAD:charter.md"); err != nil || committed != "1. Recorded." {
		t.Fatalf("committed %q %v", committed, err)
	}
	got, err := r.Charter(ctx, at)
	if err != nil || got.Revision != 3 || got.Content != "1. Later edit.\n" {
		t.Fatalf("later edit: %+v %v", got, err)
	}
}
