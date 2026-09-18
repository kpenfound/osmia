package trace

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
)

// prose reports whether p is a knowledge-base prose file, kb/<subsystem>.md.
func prose(p string) bool {
	parts := strings.Split(p, "/")
	return len(parts) == 2 && parts[0] == "kb" && strings.HasSuffix(parts[1], ".md")
}

// RecordDocuments records document revisions of one scope, the project or one
// workstream, as one commit: after a failure or an interruption either every
// revision is recorded or none is. A kb/<subsystem>.md revision with empty
// content records the subsystem's removal, and its file is deleted. The
// charter is owner-edited and handed input is immutable, so neither can be
// recorded this way, and a revision of spec.md or plan.json is refused with
// ErrConflict while the file holds an owner edit no revision records: the
// edit is read and recorded first. Revisions are checked against the recorded
// history before anything is written.
func (r *Repository) RecordDocuments(ctx context.Context, docs []Document) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	files, removed, err := r.documentFiles(ctx, docs)
	if err != nil {
		return err
	}
	return r.publishTree(ctx, files, removed)
}

// RecordDocumentsWith records document revisions of one workstream as
// RecordDocuments does, and applies txs to the workstream's workflow in the
// same commit: either the revisions and every transaction are recorded, or
// none is. It returns the state each transaction produced.
func (r *Repository) RecordDocumentsWith(ctx context.Context, docs []Document, txs ...Transaction) ([]WorkflowState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	files, removed, err := r.documentFiles(ctx, docs)
	if err != nil {
		return nil, err
	}
	stream := docs[0].Workstream
	if stream == "" {
		return nil, fmt.Errorf("project documents have no workflow")
	}
	for _, tx := range txs {
		if tx.Transition.Workstream != stream {
			return nil, fmt.Errorf("transition %s belongs to another workstream", tx.Transition.ID)
		}
	}
	log, v, err := r.loadWorkflow(stream)
	if err != nil {
		return nil, err
	}
	staged, states, err := r.stage(stream, log, v, nil, txs...)
	if err != nil {
		return nil, err
	}
	maps.Copy(files, staged)
	if err := r.publishTree(ctx, files, removed); err != nil {
		return nil, err
	}
	_ = r.wake.Notify(context.Background())
	return states, nil
}

// documentFiles requires r.mu. It checks the document revisions of one scope
// against the recorded history and returns the files of the commit that
// records them and the paths it removes.
func (r *Repository) documentFiles(ctx context.Context, docs []Document) (map[string][]byte, []string, error) {
	if len(docs) == 0 {
		return nil, nil, fmt.Errorf("no documents to record")
	}
	scope := docs[0].Workstream
	keys := map[string]bool{}
	for _, d := range docs {
		if err := validate(d); err != nil {
			return nil, nil, err
		}
		if d.Project != r.project {
			return nil, nil, fmt.Errorf("document belongs to another project")
		}
		if d.Workstream != scope {
			return nil, nil, fmt.Errorf("documents belong to different scopes")
		}
		if d.Path == "charter.md" {
			return nil, nil, fmt.Errorf("%w: the charter is recorded through Charter", ErrConflict)
		}
		if keys[recordKey(d)] {
			return nil, nil, fmt.Errorf("%w: document %s appears twice", ErrConflict, d.ID)
		}
		keys[recordKey(d)] = true
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := r.checkHistory(ctx); err != nil {
		return nil, nil, err
	}
	records, streams, err := r.scan()
	if err != nil {
		return nil, nil, err
	}
	if scope != "" && !slices.Contains(streams, scope) {
		return nil, nil, fmt.Errorf("unknown workstream %s", scope)
	}
	previous := map[string]Record{}
	identities := map[string]string{}
	for _, old := range records {
		if d, ok := old.(Document); ok && d.Workstream == scope {
			previous[recordKey(d)] = d
			identities[d.Path] = d.ID
		}
	}
	prefix := scopePath(docs[0].Header)
	paths := map[string]bool{}
	files := map[string][]byte{}
	var removed []string
	for _, d := range docs {
		if id, ok := identities[d.Path]; ok && id != d.ID {
			return nil, nil, fmt.Errorf("%w: document path already has an identity", ErrConflict)
		}
		if paths[d.Path] {
			return nil, nil, fmt.Errorf("%w: document path %s appears twice", ErrConflict, d.Path)
		}
		paths[d.Path] = true
		if err := revision(previous[recordKey(d)], d); err != nil {
			return nil, nil, err
		}
		// The owner edits spec.md and plan.json directly. Writing over an
		// edit OwnerDocuments has not recorded would lose it.
		if prev, ok := previous[recordKey(d)].(Document); ok && ownerEditable[d.ID] == d.Path {
			current, err := r.readFile(prefix + d.Path)
			if err != nil && !os.IsNotExist(err) {
				return nil, nil, err
			}
			if err == nil && string(current) != prev.Content {
				return nil, nil, fmt.Errorf("%w: %w: %s has an unrecorded owner edit; read it first", ErrConflict, ErrOwnerEdit, d.Path)
			}
		}
		if err := publicationPath(prefix + d.Path); err != nil {
			return nil, nil, err
		}
		if err := r.checked(prefix + d.Path); err != nil {
			return nil, nil, err
		}
		if scope == "" && prose(d.Path) && d.Content == "" {
			removed = append(removed, d.Path)
		} else {
			files[prefix+d.Path] = []byte(d.Content)
		}
	}
	data, err := r.readFile(prefix + "documents.jsonl")
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	for _, d := range docs {
		line, err := json.Marshal(d)
		if err != nil {
			return nil, nil, err
		}
		data = append(data, append(line, '\n')...)
	}
	files[prefix+"documents.jsonl"] = data
	return files, removed, nil
}
