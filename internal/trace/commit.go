package trace

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	sqlite3 "modernc.org/sqlite/lib"
)

// Commit names the logical paths one trace commit wrote, and what it changed
// in its workstream's workflow. Observers must not modify it.
type Commit struct {
	// Paths are the logical paths written or removed: each record's
	// RecordPath, each document's own path under its scope, each file, and
	// workstreams/<w>/workflow.json when the workflow changed.
	Paths []string
	// Workflow is nil when the commit did not change workflow state.
	Workflow *WorkflowChange
}

// WorkflowChange is what one commit changed in a workstream's workflow.
type WorkflowChange struct {
	// Threads are the agent IDs whose thread changed.
	Threads []string
	// Deliveries reports whether delivery actions were appended.
	Deliveries bool
}

type change struct {
	replace  []string
	records  []Record
	files    map[string][]byte
	stream   config.WorkstreamID
	workflow *workflowLog
	view     *workflowView
	message  string
}

func (c *change) add(o change) {
	c.records = append(c.records, o.records...)
	if len(o.files) > 0 {
		if c.files == nil {
			c.files = map[string][]byte{}
		}
		maps.Copy(c.files, o.files)
	}
	if o.workflow != nil {
		c.stream, c.workflow, c.view = o.stream, o.workflow, o.view
	}
	if c.message == "" {
		c.message = o.message
	}
}

func (c *change) setFile(name string, data []byte) {
	if c.files == nil {
		c.files = map[string][]byte{}
	}
	c.files[name] = data
}

const (
	boundaryBeforeCommit = "before-commit"
	boundaryCommitted    = "committed"
)

func (r *Repository) boundary(name string) error {
	if r.failPublication != nil {
		return r.failPublication(name)
	}
	return nil
}

func (r *Repository) publish(ctx context.Context, c change) error {
	stream := r.stream
	if stream == "" {
		stream = c.stream
	}
	if stream != "" {
		if pending, err := SessionDestroyPending(r.root, r.project, stream); err != nil {
			return err
		} else if pending {
			return fmt.Errorf("%w: session is being destroyed", os.ErrNotExist)
		}
	}

	if r.db == nil {
		return fmt.Errorf("%w: trace repository is closed", ErrLocked)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	message := c.message
	if message == "" {
		message = "Record workflow transaction"
	}
	ctx = context.WithoutCancel(ctx)
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "INSERT INTO commits (at, message) VALUES (?, ?)", time.Now().UTC().Format(time.RFC3339Nano), message)
	if err != nil {
		return err
	}
	commit, err := result.LastInsertId()
	if err != nil {
		return err
	}
	var paths []string
	seen := map[string]bool{}
	path := func(name string) {
		if !seen[name] {
			seen[name] = true
			paths = append(paths, name)
		}
	}
	for _, name := range c.replace {
		if _, err := tx.ExecContext(ctx, "DELETE FROM records WHERE path = ?", name); err != nil {
			return err
		}
		path(name)
	}
	for _, rec := range c.records {
		body, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		h := rec.header()
		name := recordPath(rec)
		if _, err := tx.ExecContext(ctx, "INSERT INTO records (commit_seq, path, schema, id, revision, body) VALUES (?, ?, ?, ?, ?, ?)", commit, name, h.Schema, h.ID, h.Revision, string(body)); sqliteCode(err) == sqlite3.SQLITE_CONSTRAINT {
			return fmt.Errorf("%w: record %s %s revision %d is already recorded", ErrConflict, kind(rec), h.ID, h.Revision)
		} else if err != nil {
			return err
		}
		path(name)
		if d, ok := rec.(Document); ok {
			path(scopePath(d.Header) + d.Path)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(c.files)) {
		if _, err := tx.ExecContext(ctx, "INSERT INTO files (path, content) VALUES (?, ?) ON CONFLICT (path) DO UPDATE SET content = excluded.content", name, c.files[name]); err != nil {
			return err
		}
		path(name)
	}
	var persisted *cachedWorkflow
	var workflow *WorkflowChange
	if c.workflow != nil {
		persisted, workflow, err = r.writeWorkflow(ctx, tx, c.stream, *c.workflow, c.view)
		if err != nil {
			return err
		}
		if workflow != nil {
			path("workstreams/" + string(c.stream) + "/workflow.json")
		}
	}
	if err := r.boundary(boundaryBeforeCommit); err != nil {
		return err
	}
	if len(c.replace) > 0 {
		r.cache.mu.Lock()
	}
	err = tx.Commit()
	if len(c.replace) > 0 {
		if err == nil {
			r.resetCache()
		}
		r.cache.mu.Unlock()
	}
	if err != nil {
		return err
	}
	r.generation.Add(1)
	var detail *CommitDetail
	if r.spansObserved() {
		d := r.commitDetail(c, stream, r.workflows[c.stream])
		detail = &d
	}
	if persisted != nil {
		r.workflows[c.stream] = persisted
	}
	r.observed(Commit{Paths: paths, Workflow: workflow})
	if detail != nil {
		r.reportSpans(*detail)
	}
	return r.boundary(boundaryCommitted)
}

func (r *Repository) writeWorkflow(ctx context.Context, tx *sql.Tx, stream config.WorkstreamID, log workflowLog, view *workflowView) (*cachedWorkflow, *WorkflowChange, error) {
	old := r.workflows[stream]
	if old == nil {
		return nil, nil, fmt.Errorf("workstream %s: workflow written before it was loaded", stream)
	}
	if len(log.Transactions) < len(old.log.Transactions) || len(log.Deliveries) < len(old.log.Deliveries) || len(log.Operations) < len(old.log.Operations) {
		return nil, nil, fmt.Errorf("workstream %s: workflow lists are append-only", stream)
	}
	change := &WorkflowChange{}
	changed := false
	appendList := func(list string, entries []any) error {
		for _, entry := range entries {
			body, err := json.Marshal(entry)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO workflow_log (list, body) VALUES (?, ?)", list, string(body)); err != nil {
				return err
			}
			changed = true
		}
		return nil
	}
	if err := appendList("transactions", anySlice(log.Transactions[len(old.log.Transactions):])); err != nil {
		return nil, nil, err
	}
	if err := appendList("deliveries", anySlice(log.Deliveries[len(old.log.Deliveries):])); err != nil {
		return nil, nil, err
	}
	change.Deliveries = len(log.Deliveries) > len(old.log.Deliveries)
	if err := appendList("operations", anySlice(log.Operations[len(old.log.Operations):])); err != nil {
		return nil, nil, err
	}
	stored := storeThreads(log.Threads)
	threads := make(map[string]string, len(stored))
	for id, t := range stored {
		body, err := json.Marshal(t)
		if err != nil {
			return nil, nil, err
		}
		threads[id] = string(body)
		if old.threads[id] == string(body) {
			continue
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO workflow_threads (agent_id, body) VALUES (?, ?) ON CONFLICT (agent_id) DO UPDATE SET body = excluded.body", id, string(body)); err != nil {
			return nil, nil, err
		}
		change.Threads = append(change.Threads, id)
	}
	for id := range old.threads {
		if _, ok := threads[id]; ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM workflow_threads WHERE agent_id = ?", id); err != nil {
			return nil, nil, err
		}
		change.Threads = append(change.Threads, id)
	}
	slices.Sort(change.Threads)
	if !changed && len(change.Threads) == 0 {
		return nil, nil, nil
	}
	switch {
	case view != nil:
		view = view.clone()
	case !changed:
		view = old.view
	default:
		var err error
		if view, err = r.viewOf(stream, log); err != nil {
			return nil, nil, err
		}
	}
	return &cachedWorkflow{log: log.clone(), view: view, threads: threads}, change, nil
}

func anySlice[T any](s []T) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}
