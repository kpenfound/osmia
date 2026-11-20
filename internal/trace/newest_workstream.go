package trace

import (
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

// NewestWorkstreamCreated returns the creation time of the project's newest
// workstream of any kind, and false when the project has no workstream whose
// creation time can be read. It reads each workstream's manifest timestamp
// once and keeps it, never a workstream's record history. A workstream that
// is retired or whose manifest cannot be read is left out; a failure to list
// the workstreams is returned.
func (r *Repository) NewestWorkstreamCreated() (time.Time, bool, error) {
	ids, err := r.diskStreams()
	if err != nil {
		return time.Time{}, false, err
	}
	known := make(map[config.WorkstreamID]bool, len(ids))
	var newest time.Time
	found := false
	for _, id := range ids {
		known[id] = true
		at, ok := r.createdAt(id)
		if ok && (!found || at.After(newest)) {
			newest, found = at, true
		}
	}
	r.createdMu.Lock()
	for id := range r.created {
		if !known[id] {
			delete(r.created, id)
		}
	}
	r.createdMu.Unlock()
	return newest, found, nil
}

// createdAt returns the workstream's creation time from the cache, reading
// its manifest on the first use.
func (r *Repository) createdAt(id config.WorkstreamID) (time.Time, bool) {
	r.createdMu.Lock()
	at, ok := r.created[id]
	r.createdMu.Unlock()
	if ok {
		return at, true
	}
	at, err := r.WorkstreamCreated(id)
	if err != nil {
		return time.Time{}, false
	}
	r.createdMu.Lock()
	if r.created == nil {
		r.created = map[config.WorkstreamID]time.Time{}
	}
	r.created[id] = at
	r.createdMu.Unlock()
	return at, true
}
