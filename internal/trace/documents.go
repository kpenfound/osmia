package trace

import (
	"context"
	"fmt"
	"strings"
)

// prose reports whether p is a knowledge-base prose file, kb/<subsystem>.md.
func prose(p string) bool {
	parts := strings.Split(p, "/")
	return len(parts) == 2 && parts[0] == "kb" && strings.HasSuffix(parts[1], ".md")
}

// RecordDocuments records document revisions of one scope, the project or one
// workstream, as one commit: either every revision is recorded or none is. A
// kb/<subsystem>.md revision with empty content records the subsystem's
// removal. The charter is recorded through EditCharter and handed input is
// immutable, so neither can be recorded this way. Revisions are checked
// against the recorded history of their own documents before anything is
// written.
func (r *Repository) RecordDocuments(ctx context.Context, docs []Document) error {
	if len(docs) == 0 {
		return fmt.Errorf("no documents to record")
	}
	r, err := r.scoped(docs[0].Workstream)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	c, err := r.documentFiles(ctx, docs)
	if err != nil {
		return err
	}
	return r.publish(ctx, c)
}

// RecordDocumentsWith records document revisions of one workstream as
// RecordDocuments does, and applies txs to the workstream's workflow in the
// same commit: either the revisions and every transaction are recorded, or
// none is. It returns the state each transaction produced.
func (r *Repository) RecordDocumentsWith(ctx context.Context, docs []Document, txs ...Transaction) ([]WorkflowState, error) {
	if len(docs) == 0 {
		return nil, fmt.Errorf("no documents to record")
	}
	stream := docs[0].Workstream
	if stream == "" {
		return nil, fmt.Errorf("project documents have no workflow")
	}
	r, err := r.scoped(stream)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	c, err := r.documentFiles(ctx, docs)
	if err != nil {
		return nil, err
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
	staged, states, err := r.stage(ctx, stream, log, v, nil, txs...)
	if err != nil {
		return nil, err
	}
	c.add(staged)
	if err := r.publish(ctx, c); err != nil {
		return nil, err
	}
	_ = r.wake.Notify(context.Background())
	return states, nil
}

// documentFiles requires r.mu. It checks the document revisions of one scope
// against the recorded history of their own documents and paths, and returns
// the commit that records them.
func (r *Repository) documentFiles(ctx context.Context, docs []Document) (change, error) {
	if len(docs) == 0 {
		return change{}, fmt.Errorf("no documents to record")
	}
	scope := docs[0].Workstream
	keys := map[string]bool{}
	paths := map[string]bool{}
	records := make([]Record, 0, len(docs))
	for _, d := range docs {
		if err := validate(d); err != nil {
			return change{}, err
		}
		if d.Project != r.project {
			return change{}, fmt.Errorf("document belongs to another project")
		}
		if d.Workstream != scope {
			return change{}, fmt.Errorf("documents belong to different scopes")
		}
		if d.Path == "charter.md" {
			return change{}, fmt.Errorf("%w: the charter is recorded through EditCharter", ErrConflict)
		}
		if parts := strings.Split(d.Path, "/"); scope == "" && len(parts) == 2 && (parts[0] == "kb" || parts[0] == "notes") && strings.HasSuffix(parts[1], ".md") && !key(strings.TrimSuffix(parts[1], ".md")) {
			return change{}, fmt.Errorf("invalid document path %q: the name must be a key", d.Path)
		}
		if keys[recordKey(d)] {
			return change{}, fmt.Errorf("%w: document %s appears twice", ErrConflict, d.ID)
		}
		keys[recordKey(d)] = true
		if paths[d.Path] {
			return change{}, fmt.Errorf("%w: document path %s appears twice", ErrConflict, d.Path)
		}
		paths[d.Path] = true
		records = append(records, d)
	}
	if err := ctx.Err(); err != nil {
		return change{}, err
	}
	if scope != "" && scope != r.stream {
		return change{}, fmt.Errorf("unknown workstream %s", scope)
	}
	if err := r.checkRevisions(ctx, records); err != nil {
		return change{}, err
	}
	return change{records: records, message: "Record documents"}, nil
}
