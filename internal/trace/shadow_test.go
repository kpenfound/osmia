package trace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
)

// The Beekeeper's shadow project has no target repository: Create and Open
// accept config.ShadowProjectID with an empty Clone, and its trace lives
// under the service state directory exactly as a registered project's does.
func TestShadowProjectHasNoTargetRepository(t *testing.T) {
	base := t.TempDir()
	root, err := config.ResolveRoot(filepath.Join(base, "osmia"), "")
	if err != nil {
		t.Fatal(err)
	}
	p := config.Project{ID: config.ShadowProjectID}
	r, err := Create(context.Background(), root, p, at, owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.CreateWorkstream(context.Background(), config.BeekeeperWorkstreamID, at, owner); err != nil {
		t.Fatal(err)
	}
	directory, err := root.ProjectTrace(config.ShadowProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directory, ".git")); err != nil {
		t.Fatalf("shadow trace not under the service state directory: %v", err)
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "osmia" {
		t.Fatalf("unexpected target repository cloned beside the shadow trace: %v", entries)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	streams, err := reopened.Workstreams()
	if err != nil {
		t.Fatal(err)
	}
	if len(streams) != 1 || streams[0] != config.BeekeeperWorkstreamID {
		t.Fatalf("shadow workstreams: %v", streams)
	}
}

// The shadow project's target repository may never be configured: Create
// and Open refuse a non-empty Clone for config.ShadowProjectID, and ordinary
// projects still require one.
func TestShadowProjectRefusesATargetClone(t *testing.T) {
	base := t.TempDir()
	root, err := config.ResolveRoot(filepath.Join(base, "osmia"), "")
	if err != nil {
		t.Fatal(err)
	}
	p := config.Project{ID: config.ShadowProjectID, Clone: filepath.Join(base, "clone")}
	if _, err := Create(context.Background(), root, p, at, owner); err == nil {
		t.Fatal("shadow project accepted a target clone on Create")
	}
	if _, err := Open(root, p); err == nil {
		t.Fatal("shadow project accepted a target clone on Open")
	}
	if _, err := Create(context.Background(), root, config.Project{ID: projectID}, at, owner); err == nil {
		t.Fatal("a registered project accepted an empty target clone")
	}
}
