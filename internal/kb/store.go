package kb

import (
	"context"
	"time"

	"github.com/kpenfound/osmia/internal/trace"
)

// Load returns the latest entity map revision recorded in the trace, or an
// empty map when none is recorded.
func Load(r *trace.Repository) (Map, error) {
	m, _, err := LoadRevision(r)
	return m, err
}

// LoadRevision is Load that also returns the revision it read, 0 when none is
// recorded.
func LoadRevision(r *trace.Repository) (Map, int, error) {
	content, revision, err := latest(r)
	if err != nil {
		return Map{}, 0, err
	}
	m, err := Parse([]byte(content))
	return m, revision, err
}

// Store validates m and records it as the next Document revision of
// kb/entities.json. Nothing is written when m is invalid.
func Store(ctx context.Context, r *trace.Repository, m Map, at time.Time, actor trace.Actor, cause string) error {
	data, err := Encode(m)
	if err != nil {
		return err
	}
	_, revision, err := latest(r)
	if err != nil {
		return err
	}
	return r.Append(ctx, trace.Document{
		Header:  trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: trace.EntitiesDocument, Revision: revision + 1, Project: r.Project(), At: at, Actor: actor, Cause: cause},
		Path:    trace.EntitiesPath,
		Content: string(data),
	})
}

func latest(r *trace.Repository) (string, int, error) {
	documents, err := trace.Read[trace.Document](r, "")
	if err != nil {
		return "", 0, err
	}
	content, revision := "", 0
	for _, d := range documents {
		if d.Path == trace.EntitiesPath && d.Revision > revision {
			content, revision = d.Content, d.Revision
		}
	}
	return content, revision, nil
}
