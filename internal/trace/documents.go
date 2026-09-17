package trace

import (
	"context"
	"encoding/json"
	"fmt"
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
// recorded this way. Revisions are checked against the recorded history
// before anything is written.
func (r *Repository) RecordDocuments(ctx context.Context, docs []Document) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(docs) == 0 {
		return fmt.Errorf("no documents to record")
	}
	scope := docs[0].Workstream
	keys := map[string]bool{}
	for _, d := range docs {
		if err := validate(d); err != nil {
			return err
		}
		if d.Project != r.project {
			return fmt.Errorf("document belongs to another project")
		}
		if d.Workstream != scope {
			return fmt.Errorf("documents belong to different scopes")
		}
		if d.Path == "charter.md" {
			return fmt.Errorf("%w: the charter is recorded through Charter", ErrConflict)
		}
		if keys[recordKey(d)] {
			return fmt.Errorf("%w: document %s appears twice", ErrConflict, d.ID)
		}
		keys[recordKey(d)] = true
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.checkHistory(ctx); err != nil {
		return err
	}
	records, streams, err := r.scan()
	if err != nil {
		return err
	}
	if scope != "" && !slices.Contains(streams, scope) {
		return fmt.Errorf("unknown workstream %s", scope)
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
			return fmt.Errorf("%w: document path already has an identity", ErrConflict)
		}
		if paths[d.Path] {
			return fmt.Errorf("%w: document path %s appears twice", ErrConflict, d.Path)
		}
		paths[d.Path] = true
		if err := revision(previous[recordKey(d)], d); err != nil {
			return err
		}
		if err := publicationPath(prefix + d.Path); err != nil {
			return err
		}
		if err := r.checked(prefix + d.Path); err != nil {
			return err
		}
		if scope == "" && prose(d.Path) && d.Content == "" {
			removed = append(removed, d.Path)
		} else {
			files[prefix+d.Path] = []byte(d.Content)
		}
	}
	data, err := r.readFile(prefix + "documents.jsonl")
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, d := range docs {
		line, err := json.Marshal(d)
		if err != nil {
			return err
		}
		data = append(data, append(line, '\n')...)
	}
	files[prefix+"documents.jsonl"] = data
	return r.publishTree(ctx, files, removed)
}
