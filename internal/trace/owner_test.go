package trace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func ownerFile(t *testing.T, root string, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "workstreams", string(streamID), path))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// An owner edit of spec.md becomes the next revision, by the owner, on the
// next read; a read that finds no edit records nothing.
func TestOwnerDocumentRecordsAnEditOnceAndNothingWithoutOne(t *testing.T) {
	r, root, p := create(t)
	ctx := context.Background()
	dir, err := root.ProjectTrace(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.RecordDocuments(ctx, []Document{streamDocument("spec", "spec.md", "# Uploads\n", 1)}); err != nil {
		t.Fatal(err)
	}
	commits := func() string { return gitOutput(t, r, "rev-list", "--count", "HEAD") }
	before := commits()
	got, err := r.OwnerDocument(ctx, streamID, "spec", at, nil)
	if err != nil || got.Revision != 1 || got.Actor.Kind != "agent" {
		t.Fatalf("unedited read %+v %v", got, err)
	}
	if commits() != before {
		t.Fatalf("a read with no edit committed: %s, %s before", commits(), before)
	}
	edited := "# Uploads\n\n## Acceptance criteria\n\n1. Uploads resume.\n"
	if err := os.WriteFile(filepath.Join(dir, "workstreams", string(streamID), "spec.md"), []byte(edited), 0600); err != nil {
		t.Fatal(err)
	}
	got, err = r.OwnerDocument(ctx, streamID, "spec", at, nil)
	if err != nil || got.Revision != 2 || got.Content != edited || got.Actor != ownerActor || got.Cause != "owner-edit" || got.ID != "spec" {
		t.Fatalf("edited read %+v %v", got, err)
	}
	count, err := strconv.Atoi(before)
	if err != nil {
		t.Fatal(err)
	}
	if commits() != strconv.Itoa(count+1) {
		t.Fatalf("recording the edit took %s commits, %s before", commits(), before)
	}
	// The commit takes the recorded bytes, so the edit is in the trace, and
	// a second read records nothing again.
	if got := gitOutput(t, r, "cat-file", "blob", "HEAD:workstreams/"+string(streamID)+"/spec.md"); !strings.Contains(got, "Uploads resume.") {
		t.Fatalf("committed spec.md %q", got)
	}
	again, err := r.OwnerDocument(ctx, streamID, "spec", at, nil)
	if err != nil || again.Revision != 2 || commits() != strconv.Itoa(count+1) {
		t.Fatalf("second read %+v %v, commits %s", again, err, commits())
	}
}

// An edit the check refuses is not recorded, and the latest recorded revision
// stays the current one.
func TestOwnerDocumentRefusesAnInvalidEdit(t *testing.T) {
	r, root, p := create(t)
	ctx := context.Background()
	dir, err := root.ProjectTrace(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.RecordDocuments(ctx, []Document{streamDocument("plan", "plan.json", "{}\n", 1)}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "workstreams", string(streamID), "plan.json")
	if err := os.WriteFile(path, []byte("{not json\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := r.OwnerDocument(ctx, streamID, "plan", at, func(content string) error {
		if content != "{not json\n" {
			t.Fatalf("the check read %q", content)
		}
		return errors.New("plan.json: invalid JSON")
	})
	if !errors.Is(err, ErrOwnerEdit) || !strings.Contains(err.Error(), "invalid JSON") || got.Revision != 1 || got.Content != "{}\n" {
		t.Fatalf("refused edit %+v %v", got, err)
	}
	docs, err := Read[Document](r, streamID)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 {
		t.Fatalf("the refused edit was recorded: %+v", docs)
	}
	// A valid edit of the same file is recorded by the same read.
	if err := os.WriteFile(path, []byte(`{"version":1}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	next, err := r.OwnerDocument(ctx, streamID, "plan", at, func(string) error { return nil })
	if err != nil || next.Revision != 2 {
		t.Fatalf("valid edit %+v %v", next, err)
	}
}

// Only spec.md and plan.json are owner-edited, and only of a workstream the
// trace holds.
func TestOwnerDocumentRefusesWhatItDoesNotOwn(t *testing.T) {
	r, _, _ := create(t)
	ctx := context.Background()
	if _, err := r.OwnerDocument(ctx, streamID, "charter", at, nil); err == nil {
		t.Fatal("the charter was read as an owner-edited workstream document")
	}
	if _, err := r.OwnerDocument(ctx, streamID, "spec", at, nil); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a document with no revision: %v", err)
	}
	if _, err := r.OwnerDocument(ctx, "w_00000000000000000000000000000009", "spec", at, nil); err == nil {
		t.Fatal("an unknown workstream was read")
	}
}

// A controller's revision of an owner-edited document is refused while the
// file holds an edit no revision records, so a redraft never writes over it.
func TestRecordDocumentsRefusesAnUnrecordedOwnerEdit(t *testing.T) {
	r, root, p := create(t)
	ctx := context.Background()
	dir, err := root.ProjectTrace(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.RecordDocuments(ctx, []Document{streamDocument("spec", "spec.md", "# Uploads\n", 1)}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "workstreams", string(streamID), "spec.md")
	if err := os.WriteFile(path, []byte("# Uploads, edited\n"), 0600); err != nil {
		t.Fatal(err)
	}
	err = r.RecordDocuments(ctx, []Document{streamDocument("spec", "spec.md", "# The architect's redraft\n", 2)})
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "unrecorded owner edit") {
		t.Fatalf("recording over an owner edit: %v", err)
	}
	if got := ownerFile(t, dir, "spec.md"); got != "# Uploads, edited\n" {
		t.Fatalf("spec.md %q", got)
	}
	// Once the edit is recorded, the redraft goes on top of it.
	edit, err := r.OwnerDocument(ctx, streamID, "spec", at, nil)
	if err != nil || edit.Revision != 2 {
		t.Fatalf("edit %+v %v", edit, err)
	}
	if err := r.RecordDocuments(ctx, []Document{streamDocument("spec", "spec.md", "# The architect's redraft\n", 3)}); err != nil {
		t.Fatal(err)
	}
	if got := ownerFile(t, dir, "spec.md"); got != "# The architect's redraft\n" {
		t.Fatalf("spec.md after the redraft %q", got)
	}
}
