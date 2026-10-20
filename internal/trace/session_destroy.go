package trace

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kpenfound/osmia/internal/config"
)

// SessionDestroy locates every local resource even after its trace is gone.
// These records are temporary recovery intents, independent of archive cleanup.
type SessionDestroy struct {
	Project    config.Project      `json:"project"`
	Workstream config.WorkstreamID `json:"workstream"`
	Kind       string              `json:"kind"`
}

func sessionDestroyDirectory(root config.Root, project config.ProjectID) string {
	return filepath.Join(root.String(), "session-destroys", string(project))
}
func sessionDestroyPath(root config.Root, project config.ProjectID, stream config.WorkstreamID) string {
	return filepath.Join(sessionDestroyDirectory(root, project), string(stream)+".json")
}

// SessionDestroyPending reports the durable admission fence for a session.
func SessionDestroyPending(root config.Root, project config.ProjectID, stream config.WorkstreamID) (bool, error) {
	_, err := os.Stat(sessionDestroyPath(root, project, stream))
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

// BeginSessionDestroy persists the intent before any resource is removed.
func (r *Repository) BeginSessionDestroy(intent SessionDestroy) error {
	root := r.root
	if r.project != intent.Project.ID || r.stream != "" {
		return fmt.Errorf("destroy intent belongs to another project")
	}
	child, err := r.scoped(intent.Workstream)
	if err != nil {
		return err
	}
	child.mu.Lock()
	defer child.mu.Unlock()
	if err := config.CheckWorkstreamIDs(intent.Workstream); err != nil {
		return err
	}
	dir := sessionDestroyDirectory(root, intent.Project.ID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	// Persist the new directory entries as well as the eventual intent file.
	if err := syncDestroyDirectory(root.String()); err != nil {
		return err
	}
	if err := syncDestroyDirectory(filepath.Dir(dir)); err != nil {
		return err
	}
	data, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".intent-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(f.Name(), sessionDestroyPath(root, intent.Project.ID, intent.Workstream)); err != nil {
		return err
	}
	return syncDestroyDirectory(dir)
}

func syncDestroyDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// PendingSessionDestroys reads recovery intents without opening live traces.
func PendingSessionDestroys(root config.Root, project config.ProjectID) ([]SessionDestroy, error) {
	dir := sessionDestroyDirectory(root, project)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []SessionDestroy
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var intent SessionDestroy
		if err := json.Unmarshal(data, &intent); err != nil {
			return nil, err
		}
		if intent.Project.ID != project || config.CheckWorkstreamIDs(intent.Workstream) != nil || e.Name() != string(intent.Workstream)+".json" || (intent.Kind != "design" && intent.Kind != SessionKind) {
			return nil, fmt.Errorf("invalid session destroy intent %s", e.Name())
		}
		out = append(out, intent)
	}
	return out, nil
}

// FinishSessionDestroy forgets the recovery intent only after all removals.
func FinishSessionDestroy(root config.Root, project config.ProjectID, stream config.WorkstreamID) error {
	if err := os.Remove(sessionDestroyPath(root, project, stream)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syncDestroyDirectory(sessionDestroyDirectory(root, project))
}
