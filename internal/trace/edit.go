package trace

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

// EditCharter replaces the revision the owner read. An unrecorded file edit
// is a conflict, so an API write cannot overwrite an owner's local work.
func (r *Repository) EditCharter(ctx context.Context, revision int, content string, at time.Time) (Document, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkHistory(ctx); err != nil {
		return Document{}, err
	}
	records, _, err := r.scan()
	if err != nil {
		return Document{}, err
	}
	latest, ok := latestCharter(records)
	if !ok || latest.Revision != revision {
		return Document{}, ErrConflict
	}
	current, err := r.readFile("charter.md")
	if err != nil {
		return Document{}, err
	}
	if string(current) != latest.Content {
		return Document{}, ErrConflict
	}
	if content == latest.Content {
		return latest, nil
	}
	next := charterRevision(r.project, at, ownerActor, "owner-edit", revision+1, content)
	if err := validate(next); err != nil {
		return Document{}, err
	}
	files := map[string][]byte{"charter.md": []byte(content)}
	if err := r.appendRecords(files, []Record{next}); err != nil {
		return Document{}, err
	}
	return next, r.publish(ctx, files)
}

// EditDraft replaces the owner's pinned spec and plan together before
// ratification. Validation and the feature-state gate share the write lock.
func (r *Repository) EditDraft(ctx context.Context, stream config.WorkstreamID, specRevision, planRevision int, spec, plan string, at time.Time, check func(map[string]string) error) (map[string]Document, error) {
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
	records, _, err := r.scan()
	if err != nil {
		return nil, err
	}
	latest := map[string]Document{}
	for _, record := range records {
		if d, ok := record.(Document); ok && d.Workstream == stream && strings.HasPrefix(d.Path, "shed/round-") && strings.HasSuffix(d.Path, "/ratification.json") {
			return nil, fmt.Errorf("%w: the owner has ratified these documents", ErrConflict)
		}
		if d, ok := record.(Document); ok && d.Workstream == stream && ownerEditable[d.ID] == d.Path {
			latest[d.ID] = d
		}
	}
	if latest["spec"].Revision != specRevision || latest["plan"].Revision != planRevision || specRevision < 1 || planRevision < 1 {
		return nil, ErrConflict
	}
	for _, id := range []string{"spec", "plan"} {
		current, err := r.readFile("workstreams/" + string(stream) + "/" + latest[id].Path)
		if err != nil {
			return nil, err
		}
		if string(current) != latest[id].Content {
			return nil, ErrConflict
		}
	}
	content := map[string]string{"spec": spec, "plan": plan}
	if err := check(content); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrOwnerEdit, err)
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
	files, removed, err := r.documentFiles(ctx, docs)
	if err != nil {
		return nil, err
	}
	return latest, r.publishTree(ctx, files, removed)
}
