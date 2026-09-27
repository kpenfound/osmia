package trace

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

// WorkstreamBase is the owner's same-project workstream dependency. Empty Base
// selects the project's upstream base. Revision pins owner updates.
type WorkstreamBase struct {
	Workstream config.WorkstreamID `json:"workstream"`
	Base       config.WorkstreamID `json:"base,omitempty"`
	Revision   int                 `json:"revision"`
}

func bases(records []Record) (map[config.WorkstreamID]WorkstreamBase, error) {
	out := map[config.WorkstreamID]WorkstreamBase{}
	for _, record := range records {
		d, ok := record.(Document)
		if !ok || d.ID != "workstream-base" || d.Path != "base.json" {
			continue
		}
		var base WorkstreamBase
		if err := json.Unmarshal([]byte(d.Content), &base); err != nil {
			return nil, err
		}
		if base.Workstream != d.Workstream || base.Revision != d.Revision {
			return nil, fmt.Errorf("base record identity differs")
		}
		out[d.Workstream] = base
	}
	return out, nil
}

func (r *Repository) WorkstreamBase(stream config.WorkstreamID) (WorkstreamBase, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	records, streams, err := r.scan()
	if err != nil {
		return WorkstreamBase{}, err
	}
	if !slices.Contains(streams, stream) {
		return WorkstreamBase{}, fmt.Errorf("unknown workstream")
	}
	all, err := bases(records)
	if err != nil {
		return WorkstreamBase{}, err
	}
	if base, ok := all[stream]; ok {
		return base, nil
	}
	return WorkstreamBase{Workstream: stream}, nil
}

// SetWorkstreamBase validates the graph and document-state gate in the same
// transaction as the owner revision. Sealed workstreams keep their declared base.
func (r *Repository) SetWorkstreamBase(ctx context.Context, stream, base config.WorkstreamID, revision int, at time.Time) (WorkstreamBase, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, view, err := r.loadWorkflow(stream)
	if err != nil {
		return WorkstreamBase{}, err
	}
	if !slices.Contains([]string{"", "handed", "sketched", "in-shed"}, view.states[FeatureSubject].Value) {
		return WorkstreamBase{}, fmt.Errorf("%w: a sealed workstream's base cannot be edited", ErrConflict)
	}
	records, streams, err := r.scan()
	if err != nil {
		return WorkstreamBase{}, err
	}
	for _, record := range records {
		if d, ok := record.(Document); ok && d.Workstream == stream && strings.HasPrefix(d.Path, "shed/round-") && strings.HasSuffix(d.Path, "/ratification.json") {
			return WorkstreamBase{}, fmt.Errorf("%w: the owner has ratified this workstream's base", ErrConflict)
		}
	}
	all, err := bases(records)
	if err != nil {
		return WorkstreamBase{}, err
	}
	current := all[stream]
	if current.Revision != revision {
		return WorkstreamBase{}, ErrConflict
	}
	if base != "" {
		if !slices.Contains(streams, base) {
			return WorkstreamBase{}, fmt.Errorf("base must be a workstream in this project")
		}
		_, target, err := r.loadWorkflow(base)
		if err != nil {
			return WorkstreamBase{}, err
		}
		if target.states[FeatureSubject].Value == "abandoned" {
			return WorkstreamBase{}, fmt.Errorf("base workstream is abandoned")
		}
		seen := map[config.WorkstreamID]bool{stream: true}
		for next := base; next != ""; next = all[next].Base {
			if seen[next] {
				return WorkstreamBase{}, fmt.Errorf("workstream bases must form an acyclic graph")
			}
			seen[next] = true
		}
	}
	if current.Base == base {
		return WorkstreamBase{Workstream: stream, Base: base, Revision: revision}, nil
	}
	next := WorkstreamBase{Workstream: stream, Base: base, Revision: revision + 1}
	data, err := json.Marshal(next)
	if err != nil {
		return WorkstreamBase{}, err
	}
	d := Document{Header: Header{Schema: "osmia.trace.document", Version: Version, ID: "workstream-base", Revision: next.Revision, Project: r.project, Workstream: stream, At: at, Actor: ownerActor, Cause: "owner-base"}, Path: "base.json", Content: string(data)}
	files, removed, err := r.documentFiles(ctx, []Document{d})
	if err != nil {
		return WorkstreamBase{}, err
	}
	return next, r.publishTree(ctx, files, removed)
}
