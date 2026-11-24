package trace

import (
	"context"
	"errors"
	"testing"
)

func TestWorkflowReadsFollowCommitsFromTheCachedWorkflowAndManifest(t *testing.T) {
	r, root, p := create(t)
	transact(t, r, transaction("first", 0, "", "active", 1))
	s := mustScoped(t, r, streamID)
	decodes := s.manifestDecodes.Load()

	for range 20 {
		if got, err := r.Workflow(streamID, "feature"); err != nil || got != (WorkflowState{1, "active"}) {
			t.Fatalf("feature: %v, %v", got, err)
		}
		if _, err := r.Kind(streamID); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.manifestDecodes.Load() - decodes; got != 0 {
		t.Fatalf("the manifest was decoded %d more times after it was cached", got)
	}

	manifest := "workstreams/" + string(streamID) + "/workstream.json"
	var original []byte
	if err := s.db.QueryRowContext(context.Background(), "SELECT content FROM files WHERE path = ?", manifest).Scan(&original); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(context.Background(), "UPDATE files SET content = ? WHERE path = ?", []byte("damaged manifest\n"), manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Workflow(streamID, "feature"); err == nil {
		t.Fatal("a damaged manifest was masked by the cached one")
	}
	if _, err := s.db.ExecContext(context.Background(), "UPDATE files SET content = ? WHERE path = ?", original, manifest); err != nil {
		t.Fatal(err)
	}

	states, err := r.WorkflowStates(streamID)
	if err != nil {
		t.Fatal(err)
	}
	states["feature"] = WorkflowState{9, "changed by the caller"}
	if got, err := r.RawWorkflow(streamID, "feature"); err != nil || got != (WorkflowState{1, "active"}) {
		t.Fatalf("a caller's change reached the cached view: %v, %v", got, err)
	}

	transact(t, r, transaction("second", 1, "active", "waiting", 0))
	if got, err := r.Workflow(streamID, "feature"); err != nil || got != (WorkflowState{2, "waiting"}) {
		t.Fatalf("after a commit: %v, %v", got, err)
	}
	if got, err := r.RawWorkflow(streamID, "feature"); err != nil || got != (WorkflowState{2, "waiting"}) {
		t.Fatalf("raw after a commit: %v, %v", got, err)
	}

	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Kind(streamID); !errors.Is(err, ErrLocked) {
		t.Fatalf("a closed handle answered from its cached manifest: %v", err)
	}
	reopened, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got, err := reopened.Workflow(streamID, "feature"); err != nil || got != (WorkflowState{2, "waiting"}) {
		t.Fatalf("reopened: %v, %v", got, err)
	}
}
