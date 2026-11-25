package trace

import (
	"context"
	"errors"
	"testing"
)

func TestStoredEmptyKindReadsAsFeature(t *testing.T) {
	r, _, _ := create(t)
	kind, err := r.Kind(streamID)
	if err != nil || kind != FeatureKind {
		t.Fatalf("kind of a workstream that records none: %q %v", kind, err)
	}
	if recorded, err := r.RecordedKind(streamID); err != nil || recorded != "" {
		t.Fatalf("recorded kind: %q %v", recorded, err)
	}
	statuses, err := r.Statuses()
	if err != nil || len(statuses) != 1 || statuses[0].Kind != "" || statuses[0].WorkKind != FeatureKind {
		t.Fatalf("status kinds: %+v %v", statuses, err)
	}
}

func TestSessionKindIsNotAWorkstreamKind(t *testing.T) {
	r, _, _ := create(t)
	ctx := context.Background()
	if err := r.CreateSessionWorkstream(ctx, secondStream, "default", "base", "branch", "", at, owner); err != nil {
		t.Fatal(err)
	}
	if kind, err := r.Kind(secondStream); err != nil || kind != SessionKind {
		t.Fatalf("session kind: %q %v", kind, err)
	}
	if err := r.CreateWorkstreamKind(ctx, thirdStream, "", SessionKind, at, owner); err == nil {
		t.Fatal("a workstream was created with the session kind")
	}
	statuses, err := r.Statuses()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range statuses {
		if s.Workstream == secondStream && (s.Kind != SessionKind || s.WorkKind != "") {
			t.Fatalf("session status kinds: %+v", s)
		}
	}
}

func TestCreateWorkstreamKindRecordsTheKindInTheCreatingCommit(t *testing.T) {
	r, _, _ := create(t)
	ctx := context.Background()
	if err := r.CreateWorkstreamKind(ctx, secondStream, "", "feature", at, owner); err != nil {
		t.Fatal(err)
	}
	if recorded, err := r.RecordedKind(secondStream); err != nil || recorded != "feature" {
		t.Fatalf("recorded kind: %q %v", recorded, err)
	}
	if err := r.CreateWorkstreamKind(ctx, thirdStream, "", "mystery", at, owner); err != nil {
		t.Fatal(err)
	}
	if kind, err := r.Kind(thirdStream); err != nil || kind != "mystery" {
		t.Fatalf("a kind this package does not know is returned as stored: %q %v", kind, err)
	}
}

func handedDocument() Document {
	d := streamDocument("handed", "handed/design.md", "# Design\n", 1)
	d.Source = "stdin"
	return d
}

func TestAppendWithKindRecordsKindAndDocumentInOneCommit(t *testing.T) {
	r, _, _ := create(t)
	ctx := context.Background()
	s := mustScoped(t, r, streamID)
	before := commits(t, s)
	handed := handedDocument()
	if err := r.AppendWithKind(ctx, handed, "feature"); err != nil {
		t.Fatal(err)
	}
	if got := commits(t, s) - before; got != 1 {
		t.Fatalf("recording the document and the kind made %d commits", got)
	}
	if recorded, err := r.RecordedKind(streamID); err != nil || recorded != "feature" {
		t.Fatalf("recorded kind: %q %v", recorded, err)
	}
	docs, err := Read[Document](r, streamID)
	if err != nil || len(docs) != 1 || docs[0].ID != "handed" {
		t.Fatalf("documents: %+v %v", docs, err)
	}
}

func TestAppendWithKindRefusesAChangedKindAndWritesNothing(t *testing.T) {
	r, _, _ := create(t)
	ctx := context.Background()
	s := mustScoped(t, r, streamID)
	if err := r.AppendWithKind(ctx, handedDocument(), "feature"); err != nil {
		t.Fatal(err)
	}
	before := commits(t, s)
	err := r.AppendWithKind(ctx, streamDocument("spec", "spec.md", "# Spec\n", 1), "mystery")
	if !errors.Is(err, ErrKindRecorded) || !errors.Is(err, ErrConflict) {
		t.Fatalf("changing the recorded kind: %v", err)
	}
	if commits(t, s) != before {
		t.Fatal("a refused kind change wrote a commit")
	}
	if recorded, _ := r.RecordedKind(streamID); recorded != "feature" {
		t.Fatalf("recorded kind: %q", recorded)
	}
	if err := r.AppendWithKind(ctx, streamDocument("spec", "spec.md", "# Spec\n", 1), "feature"); err != nil {
		t.Fatalf("recording the same kind again: %v", err)
	}
}

func TestAppendWithKindRefusesASession(t *testing.T) {
	r, _, _ := create(t)
	ctx := context.Background()
	if err := r.CreateSessionWorkstream(ctx, secondStream, "default", "base", "branch", "", at, owner); err != nil {
		t.Fatal(err)
	}
	d := handedDocument()
	d.Workstream = secondStream
	if err := r.AppendWithKind(ctx, d, "feature"); !errors.Is(err, ErrKindRecorded) {
		t.Fatalf("recording a kind on a session: %v", err)
	}
}
