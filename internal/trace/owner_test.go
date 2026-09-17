package trace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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

func writeOwnerFile(t *testing.T, root, path, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "workstreams", string(streamID), path), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// drafted records the architect's first revision of both owner-edited
// documents and returns the trace directory.
func drafted(t *testing.T, r *Repository, root string, spec, plan string) {
	t.Helper()
	if err := r.RecordDocuments(context.Background(), []Document{
		streamDocument("spec", "spec.md", spec, 1),
		streamDocument("plan", "plan.json", plan, 1),
	}); err != nil {
		t.Fatal(err)
	}
}

// An owner edit becomes the next revision, by the owner, on the next read; a
// read that finds no edit records nothing.
func TestOwnerDocumentsRecordAnEditOnceAndNothingWithoutOne(t *testing.T) {
	r, root, p := create(t)
	ctx := context.Background()
	dir, err := root.ProjectTrace(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	drafted(t, r, dir, "# Uploads\n", "{}\n")
	commits := func() string { return gitOutput(t, r, "rev-list", "--count", "HEAD") }
	before := commits()
	got, err := r.OwnerDocuments(ctx, streamID, at, nil)
	if err != nil || got["spec"].Revision != 1 || got["plan"].Revision != 1 || got["spec"].Actor.Kind != "agent" {
		t.Fatalf("unedited read %+v %v", got, err)
	}
	if commits() != before {
		t.Fatalf("a read with no edit committed: %s, %s before", commits(), before)
	}
	edited := "# Uploads\n\n## Acceptance criteria\n\n1. Uploads resume.\n"
	writeOwnerFile(t, dir, "spec.md", edited)
	got, err = r.OwnerDocuments(ctx, streamID, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if spec := got["spec"]; spec.Revision != 2 || spec.Content != edited || spec.Actor != ownerActor || spec.Cause != "owner-edit" || spec.ID != "spec" || spec.Depth != 0 {
		t.Fatalf("edited read %+v", spec)
	}
	if got["plan"].Revision != 1 {
		t.Fatalf("the plan was recorded again: %+v", got["plan"])
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
	again, err := r.OwnerDocuments(ctx, streamID, at, nil)
	if err != nil || again["spec"].Revision != 2 || commits() != strconv.Itoa(count+1) {
		t.Fatalf("second read %+v %v, commits %s", again, err, commits())
	}
	// Both edited at once: one revision each, and the check sees both files.
	writeOwnerFile(t, dir, "spec.md", edited+"2. Progress is reported.\n")
	writeOwnerFile(t, dir, "plan.json", `{"version":1}`+"\n")
	seen := map[string]string{}
	both, err := r.OwnerDocuments(ctx, streamID, at, func(content map[string]string) error {
		seen = content
		return nil
	})
	if err != nil || both["spec"].Revision != 3 || both["plan"].Revision != 2 {
		t.Fatalf("both edited %+v %v", both, err)
	}
	if want := map[string]string{"spec": edited + "2. Progress is reported.\n", "plan": `{"version":1}` + "\n"}; !reflect.DeepEqual(seen, want) {
		t.Fatalf("the check read %+v", seen)
	}
}

// The two documents are one draft: an edit the check refuses records neither,
// and the check sees the edited file next to the recorded revision of the one
// that was not edited.
func TestOwnerDocumentsRefuseAnInvalidEditOfEither(t *testing.T) {
	r, root, p := create(t)
	ctx := context.Background()
	dir, err := root.ProjectTrace(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	drafted(t, r, dir, "# Uploads\n", "{}\n")
	writeOwnerFile(t, dir, "plan.json", "{not json\n")
	writeOwnerFile(t, dir, "spec.md", "# Uploads, edited\n")
	got, err := r.OwnerDocuments(ctx, streamID, at, func(content map[string]string) error {
		if content["plan"] != "{not json\n" || content["spec"] != "# Uploads, edited\n" {
			t.Fatalf("the check read %+v", content)
		}
		return errors.New("plan.json: invalid JSON")
	})
	if !errors.Is(err, ErrOwnerEdit) || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("refused edit %v", err)
	}
	if got["spec"].Revision != 1 || got["spec"].Content != "# Uploads\n" || got["plan"].Revision != 1 {
		t.Fatalf("refused edit returned %+v", got)
	}
	docs, err := Read[Document](r, streamID)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 {
		t.Fatalf("the refused edit was recorded: %+v", docs)
	}
	// A spec edit alone is checked against the recorded plan.
	writeOwnerFile(t, dir, "plan.json", "{}\n")
	next, err := r.OwnerDocuments(ctx, streamID, at, func(content map[string]string) error {
		if content["plan"] != "{}\n" {
			t.Fatalf("the check read plan %q", content["plan"])
		}
		return nil
	})
	if err != nil || next["spec"].Revision != 2 || next["plan"].Revision != 1 {
		t.Fatalf("spec edit alone %+v %v", next, err)
	}
}

// Only a workstream the trace holds, with both documents recorded, is read.
func TestOwnerDocumentsRefuseWhatItDoesNotOwn(t *testing.T) {
	r, root, p := create(t)
	ctx := context.Background()
	dir, err := root.ProjectTrace(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.OwnerDocuments(ctx, streamID, at, nil); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("documents with no revision: %v", err)
	}
	if err := r.RecordDocuments(ctx, []Document{streamDocument("spec", "spec.md", "# Uploads\n", 1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.OwnerDocuments(ctx, streamID, at, nil); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("only the spec recorded: %v", err)
	}
	drafted := streamDocument("plan", "plan.json", "{}\n", 1)
	if err := r.RecordDocuments(ctx, []Document{drafted}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.OwnerDocuments(ctx, "w_00000000000000000000000000000009", at, nil); err == nil {
		t.Fatal("an unknown workstream was read")
	}
	if got := ownerFile(t, dir, "plan.json"); got != "{}\n" {
		t.Fatalf("plan.json %q", got)
	}
}

// OwnerEdits names the files that hold an edit no revision records.
func TestOwnerEditsNamesUnrecordedEdits(t *testing.T) {
	r, root, p := create(t)
	ctx := context.Background()
	dir, err := root.ProjectTrace(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	drafted(t, r, dir, "# Uploads\n", "{}\n")
	if edited, err := r.OwnerEdits(streamID); err != nil || len(edited) != 0 {
		t.Fatalf("edits with none made: %+v %v", edited, err)
	}
	writeOwnerFile(t, dir, "plan.json", `{"version":1}`+"\n")
	if edited, err := r.OwnerEdits(streamID); err != nil || !reflect.DeepEqual(edited, []string{"plan.json"}) {
		t.Fatalf("one edit: %+v %v", edited, err)
	}
	writeOwnerFile(t, dir, "spec.md", "# Uploads, edited\n")
	if edited, err := r.OwnerEdits(streamID); err != nil || !reflect.DeepEqual(edited, []string{"plan.json", "spec.md"}) {
		t.Fatalf("both edited: %+v %v", edited, err)
	}
	if _, err := r.OwnerDocuments(ctx, streamID, at, nil); err != nil {
		t.Fatal(err)
	}
	if edited, err := r.OwnerEdits(streamID); err != nil || len(edited) != 0 {
		t.Fatalf("edits after recording them: %+v %v", edited, err)
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
	drafted(t, r, dir, "# Uploads\n", "{}\n")
	writeOwnerFile(t, dir, "spec.md", "# Uploads, edited\n")
	err = r.RecordDocuments(ctx, []Document{streamDocument("spec", "spec.md", "# The architect's redraft\n", 2)})
	if !errors.Is(err, ErrConflict) || !errors.Is(err, ErrOwnerEdit) || !strings.Contains(err.Error(), "unrecorded owner edit") {
		t.Fatalf("recording over an owner edit: %v", err)
	}
	if got := ownerFile(t, dir, "spec.md"); got != "# Uploads, edited\n" {
		t.Fatalf("spec.md %q", got)
	}
	// Once the edit is recorded, the redraft goes on top of it.
	edit, err := r.OwnerDocuments(ctx, streamID, at, nil)
	if err != nil || edit["spec"].Revision != 2 {
		t.Fatalf("edit %+v %v", edit, err)
	}
	if err := r.RecordDocuments(ctx, []Document{streamDocument("spec", "spec.md", "# The architect's redraft\n", 3)}); err != nil {
		t.Fatal(err)
	}
	if got := ownerFile(t, dir, "spec.md"); got != "# The architect's redraft\n" {
		t.Fatalf("spec.md after the redraft %q", got)
	}
}
