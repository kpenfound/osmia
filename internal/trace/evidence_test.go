package trace

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestRetainedEvidenceIsReadableOnceKeptAndSurvivesARestart(t *testing.T) {
	t.Parallel()
	r, root, p := create(t)
	files := []EvidenceInput{
		{Path: "report.json", Data: []byte(`{"version":1}`)},
		{Path: "shots/cart.png", Data: []byte("\x89PNG\r\n\x1a\nimage"), Image: true},
		{Path: "logs/console.txt", Data: []byte("text")},
	}
	meta := json.RawMessage(`{"note":"first"}`)
	manifest, err := r.RetainEvidence(streamID, "run-1", files, meta)
	if err != nil || len(manifest.Files) != 3 || !manifest.Files[1].Image || manifest.Files[1].SHA256 == "" {
		t.Fatalf("manifest %+v: %v", manifest, err)
	}
	again, err := r.RetainEvidence(streamID, "run-1", []EvidenceInput{{Path: "other.txt", Data: []byte("x")}}, json.RawMessage(`{"note":"second"}`))
	if err != nil || len(again.Files) != 3 || string(again.Meta) != `{"note":"first"}` {
		t.Fatalf("a second retention changed the first: %+v %v", again, err)
	}
	for _, f := range files {
		got, err := r.ReadEvidence(streamID, "run-1", f.Path)
		if err != nil || !bytes.Equal(got, f.Data) {
			t.Fatalf("%s: %q %v", f.Path, got, err)
		}
	}
	if _, err := r.ReadEvidence(streamID, "run-1", "other.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("an unlisted file was read: %v", err)
	}
	if _, ok, err := r.RetainedEvidence(streamID, "run-2"); ok || err != nil {
		t.Fatalf("a run that kept nothing: %v %v", ok, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err = Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got, err := r.ReadEvidence(streamID, "run-1", "logs/console.txt"); err != nil || string(got) != "text" {
		t.Fatalf("after a restart: %q %v", got, err)
	}
	directory, err := root.WorkstreamTrace(p.ID, streamID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directory, "evidence", "run-1", "manifest.json")); err != nil {
		t.Fatalf("evidence is not kept in the workstream's trace directory: %v", err)
	}
}

func TestRetainEvidenceRefusesPathsOutsideTheRunAndOversizedRuns(t *testing.T) {
	t.Parallel()
	r, _, _ := create(t)
	for _, path := range []string{"", "../x", "/etc/passwd", "a/../../x", "a//b", `a\b`} {
		if _, err := r.RetainEvidence(streamID, "run-path", []EvidenceInput{{Path: path, Data: []byte("x")}}, nil); err == nil {
			t.Fatalf("kept %q", path)
		}
	}
	if _, err := r.RetainEvidence(streamID, "../escape", nil, nil); err == nil {
		t.Fatal("accepted a run ID that is a path")
	}
	if _, err := r.RetainEvidence(streamID, "run-big", []EvidenceInput{{Path: "big.bin", Data: make([]byte, MaxEvidenceBytes+1)}}, nil); err == nil {
		t.Fatal("kept more than the evidence limit")
	}
	if _, ok, _ := r.RetainedEvidence(streamID, "run-big"); ok {
		t.Fatal("a refused run left evidence")
	}
}
