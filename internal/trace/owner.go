package trace

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

// ErrOwnerEdit reports an owner edit that is refused: it is not recorded, and
// the latest recorded revision stays the current one.
var ErrOwnerEdit = errors.New("owner edit refused")

// ownerEditable maps the record ID of each workstream document the owner
// edits on disk to its path.
var ownerEditable = map[string]string{"spec": "spec.md", "plan": "plan.json"}

// ownerEdited reports whether a trace file is one the owner edits in place,
// so that a difference from the committed history is an edit to record and
// not damage.
func ownerEdited(name string) bool {
	if name == "charter.md" {
		return true
	}
	parts := strings.Split(name, "/")
	if len(parts) != 3 || parts[0] != "workstreams" {
		return false
	}
	for _, path := range ownerEditable {
		if parts[2] == path {
			return true
		}
	}
	return false
}

// OwnerEdits returns the paths of the workstream's owner-edited documents
// whose file differs from the latest revision recorded for it: the edits no
// revision holds yet.
func (r *Repository) OwnerEdits(stream config.WorkstreamID) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	records, _, err := r.scan()
	if err != nil {
		return nil, err
	}
	latest := map[string]Document{}
	for _, v := range records {
		if d, ok := v.(Document); ok && d.Workstream == stream && ownerEditable[d.ID] == d.Path {
			latest[d.ID] = d
		}
	}
	var edited []string
	for _, id := range slices.Sorted(maps.Keys(ownerEditable)) {
		doc, ok := latest[id]
		if !ok {
			continue
		}
		data, err := r.readFile(scopePath(doc.Header) + doc.Path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if string(data) != doc.Content {
			edited = append(edited, doc.Path)
		}
	}
	return edited, nil
}

// OwnerDocuments returns the latest recorded revision of every workstream
// document the owner edits on disk, spec.md and plan.json, by record ID. A
// file that differs from its latest revision is first recorded as a new
// revision by the owner, so every owner edit is versioned before anything
// reads it. A read with no edit records nothing.
//
// The two documents are one draft, so they are refused and recorded together.
// check is given the content of both as the files leave them, by record ID,
// and refuses the edit: nothing is recorded, the latest recorded revisions are
// returned, and the error wraps ErrOwnerEdit and check's own. It runs while
// the repository is held, so it must not read the trace. A document with no
// recorded revision is not the owner's to create, and is reported as missing.
func (r *Repository) OwnerDocuments(ctx context.Context, stream config.WorkstreamID, at time.Time, check func(map[string]string) error) (map[string]Document, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := config.CheckWorkstreamIDs(stream); err != nil {
		return nil, err
	}
	records, streams, err := r.scan()
	if err != nil {
		return nil, err
	}
	if !slices.Contains(streams, stream) {
		return nil, fmt.Errorf("unknown workstream %s", stream)
	}
	latest := map[string]Document{}
	for _, v := range records {
		if d, ok := v.(Document); ok && d.Workstream == stream && ownerEditable[d.ID] == d.Path {
			latest[d.ID] = d
		}
	}
	content := map[string]string{}
	var edited []string
	for _, id := range slices.Sorted(maps.Keys(ownerEditable)) {
		doc, ok := latest[id]
		if !ok {
			return nil, fmt.Errorf("workstream %s records no %s: %w", stream, ownerEditable[id], os.ErrNotExist)
		}
		content[id] = doc.Content
		data, err := r.readFile(scopePath(doc.Header) + doc.Path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if string(data) != doc.Content {
			content[id] = string(data)
			edited = append(edited, id)
		}
	}
	if len(edited) == 0 {
		return latest, nil
	}
	if check != nil {
		if err := check(content); err != nil {
			return latest, fmt.Errorf("%w: %w", ErrOwnerEdit, err)
		}
	}
	recorded := maps.Clone(latest)
	for _, id := range edited {
		next := latest[id]
		next.Header.Revision, next.Header.At, next.Header.Actor, next.Header.Cause, next.Header.Depth = next.Revision+1, at, ownerActor, "owner-edit", 0
		next.Content = content[id]
		// The file already holds this content; rewriting it could overwrite a
		// newer edit, which the next read then records. The commit takes the
		// recorded bytes, not the file.
		if err := r.append(ctx, next, false); err != nil {
			return nil, err
		}
		recorded[id] = next
	}
	return recorded, nil
}
