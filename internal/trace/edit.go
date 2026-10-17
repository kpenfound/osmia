package trace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

// ErrInvalidEdit reports an owner edit that is refused as invalid: nothing is
// recorded, and the latest recorded revision stays the current one.
var ErrInvalidEdit = errors.New("owner edit is invalid")

// ownerEditable maps the record ID of each workstream document the owner
// edits to its path.
var ownerEditable = map[string]string{"spec": "spec.md", "plan": "plan.json"}

// EditCharter records content as the owner's next charter revision when
// revision is the latest; otherwise it is a conflict. Unchanged content
// records nothing.
func (r *Repository) EditCharter(ctx context.Context, revision int, content string, at time.Time) (Document, error) {
	r, err := r.CharterRepository()
	if err != nil {
		return Document{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Document{}, err
	}
	latest, err := r.latestDocument(ctx, r.db, "", "charter.md")
	if errors.Is(err, fs.ErrNotExist) {
		return Document{}, ErrConflict
	}
	if err != nil {
		return Document{}, err
	}
	if latest.Revision != revision {
		return Document{}, ErrConflict
	}
	if content == latest.Content {
		return latest, nil
	}
	next := charterRevision(r.project, at, ownerActor, "owner-edit", revision+1, content)
	if err := r.append(ctx, next); err != nil {
		return Document{}, err
	}
	return next, nil
}

// EditDraft replaces the owner's pinned spec and plan together before
// ratification. Validation and the feature-state gate share the write lock.
// check refuses invalid content with an error wrapped in ErrInvalidEdit.
func (r *Repository) EditDraft(ctx context.Context, stream config.WorkstreamID, specRevision, planRevision int, spec, plan string, at time.Time, check func(map[string]string) error) (map[string]Document, error) {
	r, err := r.scoped(stream)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, view, err := r.loadWorkflow(stream)
	if err != nil {
		return nil, err
	}
	switch view.states[FeatureSubject].Value {
	case "sketched", "in-shed":
	default:
		return nil, fmt.Errorf("%w: drafts can only be edited before ratification", ErrConflict)
	}
	records, err := r.records(func(v Record) bool {
		d, ok := v.(Document)
		return ok && d.Workstream == stream && (ownerEditable[d.ID] == d.Path || strings.HasPrefix(d.Path, "shed/round-") && strings.HasSuffix(d.Path, "/ratification.json"))
	})
	if err != nil {
		return nil, err
	}
	latest := map[string]Document{}
	for _, record := range records {
		d := record.(Document)
		if strings.HasPrefix(d.Path, "shed/round-") {
			return nil, fmt.Errorf("%w: the owner has ratified these documents", ErrConflict)
		}
		latest[d.ID] = d
	}
	if latest["spec"].Revision != specRevision || latest["plan"].Revision != planRevision || specRevision < 1 || planRevision < 1 {
		return nil, ErrConflict
	}
	content := map[string]string{"spec": spec, "plan": plan}
	if err := check(content); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidEdit, err)
	}
	var docs []Document
	for _, id := range []string{"spec", "plan"} {
		d := latest[id]
		if d.Content == content[id] {
			continue
		}
		d.Revision++
		d.Content, d.At, d.Actor, d.Cause, d.Depth = content[id], at, ownerActor, "owner-edit", 0
		docs = append(docs, d)
		latest[id] = d
	}
	if len(docs) == 0 {
		return latest, nil
	}
	c, err := r.documentFiles(ctx, docs)
	if err != nil {
		return nil, err
	}
	return latest, r.publish(ctx, c)
}
