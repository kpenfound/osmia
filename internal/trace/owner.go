package trace

import (
	"context"
	"errors"
	"fmt"
	"os"
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

// OwnerDocument returns the latest recorded revision of a workstream document
// the owner edits on disk, spec.md or plan.json. When the file differs from
// that revision, the file is first recorded as a new revision by the owner,
// so every owner edit is versioned before anything reads it. A read with no
// edit records nothing.
//
// check reads the edited content and refuses it: nothing is recorded, and the
// latest recorded revision is returned with an error wrapping ErrOwnerEdit
// and check's own. It runs while the repository is held, so it must not read
// the trace. A document with no recorded revision is not the owner's to
// create, and is reported as missing.
func (r *Repository) OwnerDocument(ctx context.Context, stream config.WorkstreamID, id string, at time.Time, check func(string) error) (Document, error) {
	path, ok := ownerEditable[id]
	if !ok {
		return Document{}, fmt.Errorf("document %s is not owner-edited", id)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Document{}, err
	}
	if err := config.CheckWorkstreamIDs(stream); err != nil {
		return Document{}, err
	}
	records, streams, err := r.scan()
	if err != nil {
		return Document{}, err
	}
	found := false
	for _, s := range streams {
		found = found || s == stream
	}
	if !found {
		return Document{}, fmt.Errorf("unknown workstream %s", stream)
	}
	var latest Document
	for _, v := range records {
		if d, ok := v.(Document); ok && d.Workstream == stream && d.Path == path {
			latest = d
		}
	}
	if latest.Revision == 0 {
		return Document{}, fmt.Errorf("workstream %s records no %s: %w", stream, path, os.ErrNotExist)
	}
	data, err := r.readFile(scopePath(latest.Header) + path)
	if os.IsNotExist(err) {
		return latest, nil
	}
	if err != nil {
		return Document{}, err
	}
	if string(data) == latest.Content {
		return latest, nil
	}
	if check != nil {
		if err := check(string(data)); err != nil {
			return latest, fmt.Errorf("%w: %s: %w", ErrOwnerEdit, path, err)
		}
	}
	next := latest
	next.Header.Revision, next.Header.At, next.Header.Actor, next.Header.Cause = latest.Revision+1, at, ownerActor, "owner-edit"
	next.Content = string(data)
	// The file already holds this content; rewriting it could overwrite a
	// newer edit, which the next read then records. The commit takes the
	// recorded bytes, not the file.
	if err := r.append(ctx, next, false); err != nil {
		return Document{}, err
	}
	return next, nil
}
