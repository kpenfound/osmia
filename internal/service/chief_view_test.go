package service

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/trace"
)

// The chief of staff's view holds the latest revision of each workstream
// document, never the tool-call or inspection records, and each staging
// replaces the one before it.
func TestChiefOfStaffViewStagesLatestDocuments(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home := t.TempDir()
	opts := fixtureAt(t, home)
	cfg, err := config.Load(opts.Config)
	must(t, err)
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, demoStart, owner)
	must(t, err)
	defer repo.Close()
	must(t, repo.CreateWorkstream(ctx, stream, demoStart, owner))
	document := func(id string, revision int, path, content string) trace.Document {
		return trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: revision, Project: project, Workstream: stream, At: demoStart, Actor: architectActor, Cause: "fixture", Depth: 1},
			Path: path, Content: content}
	}
	must(t, repo.RecordDocuments(ctx, []trace.Document{
		document(plan.SpecDocument, 1, plan.SpecPath, "# First draft\n"),
		document(plan.PlanDocument, 1, plan.PlanPath, `{"version":1}`),
		document("tool-call", 1, "tools/tool-call.json", `{"secret":"call"}`),
		document("inspection-1", 1, "inspections/inspection-1.json", `{"secret":"inspection"}`),
	}))
	must(t, repo.RecordDocuments(ctx, []trace.Document{document(plan.SpecDocument, 2, plan.SpecPath, "# Second draft\n")}))

	workspace := filepath.Join(home, "staged")
	must(t, os.MkdirAll(filepath.Join(workspace, "stale"), 0700))
	paths, err := stageChiefDocuments(repo, stream, workspace)
	must(t, err)
	if !slices.Equal(paths, []string{plan.PlanPath, plan.SpecPath}) {
		t.Fatalf("paths: %v", paths)
	}
	for name, want := range map[string]string{plan.SpecPath: "# Second draft\n", plan.PlanPath: `{"version":1}`} {
		if data, err := os.ReadFile(filepath.Join(workspace, name)); err != nil || string(data) != want {
			t.Fatalf("%s: %q %v", name, data, err)
		}
	}
	for _, name := range []string{"stale", "tools", "inspections"} {
		if _, err := os.Stat(filepath.Join(workspace, name)); !os.IsNotExist(err) {
			t.Fatalf("%s is staged: %v", name, err)
		}
	}
}
