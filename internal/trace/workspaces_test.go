package trace

import (
	"context"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
)

// A workstream's manifest records the workspace backend it was created on,
// which survives reopening the trace; one that records none is on Git, and
// an unknown backend is refused on creation and when read.
func TestWorkstreamRecordsItsWorkspaceBackend(t *testing.T) {
	ctx := context.Background()
	r, root, p := create(t)
	const jj config.WorkstreamID = "w_00000000000000000000000000000002"
	const git config.WorkstreamID = "w_00000000000000000000000000000003"
	if err := r.CreateWorkstreamOn(ctx, jj, config.WorkspacesJujutsu, at, owner); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateWorkstreamOn(ctx, git, config.WorkspacesGit, at, owner); err != nil {
		t.Fatal(err)
	}
	const other config.WorkstreamID = "w_00000000000000000000000000000004"
	if err := r.CreateWorkstreamOn(ctx, other, config.WorkspacesAuto, at, owner); err == nil || !strings.Contains(err.Error(), `unknown workspace backend "auto"`) {
		t.Fatalf("a workstream on auto: %v", err)
	}
	want := map[config.WorkstreamID]string{streamID: config.WorkspacesGit, jj: config.WorkspacesJujutsu, git: config.WorkspacesGit}
	r.Close()
	r, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	streams, err := r.Workstreams()
	if err != nil || len(streams) != len(want) {
		t.Fatalf("workstreams %v %v", streams, err)
	}
	for stream, backend := range want {
		if got, err := r.Workspaces(stream); err != nil || got != backend {
			t.Fatalf("workstream %s on %q %v, want %q", stream, got, err, backend)
		}
	}
	statuses, err := r.Statuses()
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range statuses {
		if st.Workspaces != want[st.Workstream] {
			t.Fatalf("status of %s reports %q, want %q", st.Workstream, st.Workspaces, want[st.Workstream])
		}
	}

	manifest := "workstreams/" + string(jj) + "/workstream.json"
	data, err := r.readFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"workspaces": "jujutsu"`) {
		t.Fatalf("the manifest does not record the backend:\n%s", data)
	}
	if err := r.writeFile(manifest, []byte(strings.Replace(string(data), `"jujutsu"`, `"svn"`, 1))); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Workspaces(jj); err == nil || !strings.Contains(err.Error(), `invalid workspace backend "svn"`) {
		t.Fatalf("a manifest with an unknown backend: %v", err)
	}
}
