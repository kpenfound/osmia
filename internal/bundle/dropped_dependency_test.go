package bundle_test

import (
	"context"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

func recordDocument(t *testing.T, f fixture, stream config.WorkstreamID, id, path, content string) {
	t.Helper()
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: project, Workstream: stream, At: timestamp, Actor: owner, Cause: "test"}
	if err := f.repo.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: path, Content: content}}); err != nil {
		t.Fatal(err)
	}
}

// A dependency the workstream dropped is still named by the shared project spec
// and its edge observation. The bundle says it was dropped and never that the
// upstream merged, for the dependent only.
func TestBundleShowsADroppedDependencyAsDroppedNotMerged(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for _, s := range []config.WorkstreamID{first, second} {
		if err := f.repo.CreateWorkstream(ctx, s, timestamp, owner); err != nil {
			t.Fatal(err)
		}
	}
	wg, err := trace.CreateWorkgroup(ctx, f.repo, []config.WorkstreamID{first, second}, timestamp, owner, "test-workgroup")
	if err != nil {
		t.Fatal(err)
	}
	spec := trace.ProjectSpec{
		Behavior: "Two repos exchange a fixed interface.",
		Graph:    []trace.Edge{{From: first, To: second, Type: trace.EdgeHardBlock}},
		Slices:   []trace.Slice{{Workstream: first, Spec: "spec.md", Plan: "plan.json"}, {Workstream: second, Spec: "spec.md", Plan: "plan.json"}},
	}
	if err := trace.RecordProjectSpec(ctx, f.repo, wg, spec, timestamp, owner, "owner-ratified"); err != nil {
		t.Fatal(err)
	}
	observation := `{"edges":[{"upstream":"` + string(first) + `","type":"hard_block","merged":false}]}`
	recordDocument(t, f, second, "edge-observation", "edge-observation.json", observation)

	before := f.assemble(t, bundle.Scope{Workstream: second})
	if len(before.ProjectSpec.Dropped) != 0 || len(before.Dependencies) != 1 || before.Dependencies[0].Waived {
		t.Fatalf("an edge nobody dropped is shown as dropped: %+v %+v", before.ProjectSpec, before.Dependencies)
	}

	recordDocument(t, f, second, "edge-waivers", "edge-waivers.json", `{"waivers":[{"upstream":"`+string(first)+`","type":"hard_block","reason":"abandoned"}]}`)

	after := f.assemble(t, bundle.Scope{Workstream: second})
	if len(after.ProjectSpec.Graph) != 1 || len(after.ProjectSpec.Dropped) != 1 || after.ProjectSpec.Dropped[0].From != first {
		t.Fatalf("the spec's edge is not marked dropped: %+v", after.ProjectSpec)
	}
	if len(after.Dependencies) != 1 || !after.Dependencies[0].Waived || after.Dependencies[0].Merged {
		t.Fatalf("the dependency is not shown as dropped and unmerged: %+v", after.Dependencies)
	}
	text := after.Render()
	if !strings.Contains(text, "dropped by "+string(second)) || !strings.Contains(text, "dropped, not merged") || strings.Contains(text, "merged: true") {
		t.Fatalf("the rendered context does not say the dependency was dropped:\n%s", text)
	}

	// The upstream's own bundle names no dropped edge: the waiver is the dependent's.
	if b := f.assemble(t, bundle.Scope{Workstream: first}); len(b.ProjectSpec.Dropped) != 0 {
		t.Fatalf("the upstream's context shows a dependent's waiver: %+v", b.ProjectSpec.Dropped)
	}
}
