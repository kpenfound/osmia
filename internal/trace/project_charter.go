package trace

import (
	"fmt"

	"github.com/kpenfound/osmia/internal/config"
)

// SetCharterResolver supplies service-owned open repo handles. The resolver
// neither opens nor closes a handle, and must remain available while turns
// using this repository drain. It is only called for a non-default repo.
func (r *Repository) SetCharterResolver(resolve func(config.ProjectID) (*Repository, error)) {
	r.charterResolver.Store(&resolve)
}

// CharterRepository returns the one project charter authority. KB, entities
// and workstream records continue to belong to this repo. Callers borrow the
// returned handle and must not close it or hold a repo lock across this call.
func (r *Repository) CharterRepository() (*Repository, error) {
	if r.parent != nil {
		return r.parent.CharterRepository()
	}
	if r.charterProject == "" || r.charterProject == r.project {
		return r, nil
	}
	resolve := r.charterResolver.Load()
	if resolve == nil || *resolve == nil {
		return nil, fmt.Errorf("project %s charter authority is unavailable", r.charterProject)
	}
	authority, err := (*resolve)(r.charterProject)
	if err != nil {
		return nil, err
	}
	if authority == nil || authority.root != r.root || authority.project != r.charterProject || authority.stream != "" || authority.charterProject != authority.project {
		return nil, fmt.Errorf("invalid charter authority for project %s", r.charterProject)
	}
	return authority, nil
}

// ProjectDocuments reads repo-level document history with the project's
// authoritative charter history. Historical trace reads remain local, including
// any legacy member charter revisions; those never enter active context.
func (r *Repository) ProjectDocuments() ([]Document, error) {
	authority, err := r.CharterRepository()
	if err != nil {
		return nil, err
	}
	docs, err := Read[Document](r, "")
	if err != nil || authority == r {
		return docs, err
	}
	shared, err := Read[Document](authority, "")
	if err != nil {
		return nil, err
	}
	var out []Document
	for _, d := range docs {
		if d.Path != "charter.md" {
			out = append(out, d)
		}
	}
	for _, d := range shared {
		if d.Path == "charter.md" {
			out = append(out, d)
		}
	}
	return out, nil
}
