package plan

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestDocumentsRoundTripThroughTrace(t *testing.T) {
	const project config.ProjectID = "p_00000000000000000000000000000001"
	const stream config.WorkstreamID = "w_00000000000000000000000000000001"
	at := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	architect := trace.Actor{Kind: "agent", ID: "architect"}
	ctx := context.Background()

	base := t.TempDir()
	root, err := config.ResolveRoot(filepath.Join(base, "osmia"), "")
	if err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(base, "target")
	cmd := exec.Command("git", "init", "--quiet", clone)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TEMPLATE_DIR="}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	p := config.Project{ID: project, Clone: clone}
	r, err := trace.Create(ctx, root, p, at, trace.Actor{Kind: "owner", ID: "local"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { r.Close() }()
	if err := r.CreateWorkstream(ctx, stream, at, architect); err != nil {
		t.Fatal(err)
	}
	document := func(id, path, content string, revision int) trace.Document {
		return trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: revision, Project: project, Workstream: stream, At: at, Actor: architect, Cause: "architect-turn-1"}, Path: path, Content: content}
	}

	first := sample()
	encoded, err := Encode(first)
	if err != nil {
		t.Fatal(err)
	}
	revised := sample()
	revised.Units[1].Title = "Validate plans"
	reencoded, err := Encode(revised)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []trace.Document{
		document(SpecDocument, SpecPath, validSpec, 1),
		document(PlanDocument, PlanPath, string(encoded), 1),
		document(PlanDocument, PlanPath, string(reencoded), 2),
	} {
		if err := r.Append(ctx, d); err != nil {
			t.Fatalf("append %s revision %d: %v", d.ID, d.Revision, err)
		}
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err = trace.Open(root, p)
	if err != nil {
		t.Fatal(err)
	}

	spec, err := trace.Get[trace.Document](r, stream, SpecDocument, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := ParseSpec(spec.Content); len(got.Criteria) != 2 || len(got.Diagnostics) != 0 {
		t.Fatalf("spec: %#v", got)
	}
	for revision, want := range map[int]Plan{1: first, 2: revised} {
		d, err := trace.Get[trace.Document](r, stream, PlanDocument, revision)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Parse([]byte(d.Content))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, normalize(want)) {
			t.Fatalf("plan revision %d: got %#v", revision, got)
		}
		if problems := Validate(ParseSpec(spec.Content), got, entities); len(problems) != 0 {
			t.Fatalf("plan revision %d: %v", revision, problems)
		}
	}
	docs, err := trace.Read[trace.Document](r, stream)
	if err != nil || len(docs) != 3 {
		t.Fatalf("revisions: %d %v", len(docs), err)
	}
	current, err := os.ReadFile(filepath.Join(root.String(), "projects", string(project), "workstreams", string(stream), PlanPath))
	if err != nil || string(current) != string(reencoded) {
		t.Fatalf("latest plan file: %v\n%s", err, current)
	}
	stale := document(PlanDocument, PlanPath, string(encoded), 2)
	if err := r.Append(ctx, stale); err == nil {
		t.Fatal("a second revision 2 was accepted")
	}
}
