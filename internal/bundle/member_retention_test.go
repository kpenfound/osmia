package bundle_test

import (
	"context"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestBundleRetiresResolvedMemberSliceBeforeProjectSpec(t *testing.T) {
	for _, tc := range []struct {
		name, state, publication, outcome string
		owner, retired                    bool
	}{
		{name: "cancelled", state: "abandoned", retired: true},
		{name: "owner merged", state: "delivered", owner: true, retired: true},
		{name: "current PR merged", state: "delivered", publication: `{"status":"opened","pull_request":42}`, outcome: `{"pull_request":42,"outcome":"merged"}`, retired: true},
		{name: "ready unmerged", state: "delivered", publication: `{"status":"opened","pull_request":42}`},
		{name: "draft unmerged", state: "delivered", publication: `{"status":"opened","pull_request":42,"draft":true}`},
		{name: "closed unmerged", state: "delivered", publication: `{"status":"opened","pull_request":42}`, outcome: `{"pull_request":42,"outcome":"closed"}`},
		{name: "earlier PR merged", state: "delivered", publication: `{"status":"opened","pull_request":43}`, outcome: `{"pull_request":42,"outcome":"merged"}`},
		{name: "no evidence", state: "delivered"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			if err := f.repo.CreateWorkstream(ctx, first, timestamp, owner); err != nil {
				t.Fatal(err)
			}
			if err := f.repo.CreateWorkstream(ctx, second, timestamp, owner); err != nil {
				t.Fatal(err)
			}
			wg, err := trace.CreateWorkgroup(ctx, f.repo, []config.WorkstreamID{first, second}, timestamp, owner, "retention")
			if err != nil {
				t.Fatal(err)
			}
			spec := trace.ProjectSpec{Behavior: "Shared behavior remains available.", Slices: []trace.Slice{{Workstream: first, Spec: "private resolved spec", Plan: "private resolved plan"}, {Workstream: second, Spec: "survivor spec", Plan: "survivor plan"}}}
			if err := trace.RecordProjectSpec(ctx, f.repo, wg, spec, timestamp, owner, "retention"); err != nil {
				t.Fatal(err)
			}
			h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "terminal", Revision: 1, Project: project, Workstream: first, At: timestamp, Actor: owner, Cause: "retention"}
			if _, err := f.repo.SetFeatureState(ctx, h, tc.state, "Member outcome"); err != nil {
				t.Fatal(err)
			}
			record := func(id, path, content string) {
				h.Schema, h.ID = "osmia.trace.document", id
				if err := f.repo.RecordDocuments(ctx, []trace.Document{{Header: h, Path: path, Content: content}}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.owner {
				record("final-merge", "final/merge.json", `{}`)
			}
			if tc.publication != "" {
				record("final-publication", "final/publication.json", tc.publication)
			}
			if tc.outcome != "" {
				record("own-pull-request-outcome", "own-pull-request-outcome.json", tc.outcome)
			}
			for _, role := range []string{"mason", trace.ChiefOfStaff} {
				b := f.assemble(t, bundle.Scope{Role: role, Workstream: first})
				if b.ProjectSpec == nil || b.ProjectSpec.Behavior != spec.Behavior || (b.ProjectSpec.Slice == nil) != tc.retired {
					t.Fatalf("member bundle: %+v", b.ProjectSpec)
				}
				if tc.retired && (strings.Contains(b.Render(), "private resolved spec") || strings.Contains(b.Render(), "private resolved plan")) {
					t.Fatal("resolved member documents remain in rendered bundle")
				}
				survivor := f.assemble(t, bundle.Scope{Role: role, Workstream: second})
				if survivor.ProjectSpec == nil || survivor.ProjectSpec.Slice == nil || survivor.ProjectSpec.Slice.Spec != "survivor spec" {
					t.Fatalf("survivor bundle: %+v", survivor.ProjectSpec)
				}
			}
			history, _, found, err := f.repo.ProjectSpecOf(first)
			if err != nil || !found || history.Slices[0].Spec != spec.Slices[0].Spec {
				t.Fatalf("history changed: %+v %v", history, err)
			}
		})
	}
}
