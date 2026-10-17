package trace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"sync"

	"github.com/kpenfound/osmia/internal/config"
)

// recordCache holds every record of a trace decoded and checked, in seq
// order. Records are immutable once committed, so the cache only ever loads
// the rows committed after the last one it holds, and each row is decoded
// once per handle. Its own lock, not the handle's, guards it, so reads never
// wait on a write in progress.
type recordCache struct {
	mu sync.Mutex
	// loaded reports that the cache's maps are set.
	loaded bool
	// seq is the highest row loaded.
	seq     int64
	records []Record
	errs    []error
	// latest holds each record's latest valid revision by recordKey, and
	// documents each document path's identity by its scoped path.
	latest    map[string]Record
	documents map[string]string
	// origins holds the conversation import of each workstream adopted from
	// another project, by workstream.
	origins map[config.WorkstreamID]*conversationImport
}

// decodeRow decodes and validates one stored record body, counting it.
func (r *Repository) decodeRow(body string) (Record, error) {
	r.decoded.Add(1)
	v, err := decodeRecord([]byte(body))
	if err == nil {
		err = validate(v)
	}
	return v, err
}

// refresh requires r.cache.mu. It checks the trace's manifest and loads and
// checks the rows committed since the last refresh.
func (r *Repository) refresh() error {
	c := &r.cache
	if r.reads == nil {
		return fmt.Errorf("%w: trace repository is closed", ErrLocked)
	}
	ctx := context.Background()
	if err := r.checkManifest(); err != nil {
		return err
	}
	if !c.loaded {
		c.loaded = true
		c.latest, c.documents, c.origins = map[string]Record{}, map[string]string{}, map[config.WorkstreamID]*conversationImport{}
	}
	rows, err := r.reads.QueryContext(ctx, "SELECT seq, path, body FROM records WHERE seq > ? ORDER BY seq", c.seq)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var seq int64
		var name, body string
		if err := rows.Scan(&seq, &name, &body); err != nil {
			return err
		}
		c.seq = seq
		v, err := r.decodeRow(body)
		if err == nil {
			h := v.header()
			if (h.Project != r.project && !r.importedConversationRecord(ctx, v)) || (h.Workstream != "" && h.Workstream != r.stream) || recordPath(v) != name {
				err = fmt.Errorf("record identity does not match its trace path")
			}
		}
		if err == nil {
			err = revision(c.latest[recordKey(v)], v)
		}
		if err == nil {
			if d, ok := v.(Document); ok {
				p := scopePath(d.Header) + d.Path
				if id, ok := c.documents[p]; ok && id != d.ID {
					err = fmt.Errorf("document path has multiple identities")
				}
				c.documents[p] = d.ID
			}
		}
		if err != nil {
			c.errs = append(c.errs, fmt.Errorf("%s#%d: %w", name, seq, err))
			continue
		}
		c.latest[recordKey(v)] = v
		c.records = append(c.records, v)
	}
	return rows.Err()
}

// checkManifest reads and checks the trace's manifest.
func (r *Repository) checkManifest() error {
	if r.stream == "" {
		_, err := r.readManifest("project.json", "osmia.trace.project", "")
		return err
	}
	_, err := r.workstreamManifest(r.stream)
	return err
}

// records returns a copy of every valid record of the trace that keep
// accepts, in seq order, and the diagnostics of every damaged row. It takes
// only the cache's lock.
func (r *Repository) records(keep func(Record) bool) ([]Record, error) {
	c := &r.cache
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := r.refresh(); err != nil {
		return nil, err
	}
	var records []Record
	for _, v := range c.records {
		if keep == nil || keep(v) {
			records = append(records, cloneRecord(v))
		}
	}
	return records, errors.Join(c.errs...)
}

// scan returns every valid record of this trace, in seq order, and the
// workstreams it holds: a workstream instance holds its own, the project's
// own instance none. A damaged row is reported in the error alongside the
// valid records.
func (r *Repository) scan() ([]Record, []config.WorkstreamID, error) {
	records, err := r.records(nil)
	var streams []config.WorkstreamID
	if r.stream != "" {
		streams = []config.WorkstreamID{r.stream}
	}
	return records, streams, err
}

// resetCache drops every cached record. It requires r.cache.mu.
func (r *Repository) resetCache() {
	r.cache.loaded, r.cache.seq, r.cache.records, r.cache.errs, r.cache.latest, r.cache.documents, r.cache.origins = false, 0, nil, nil, nil, nil, nil
}

// querier runs a query on a database or in a transaction.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// latestRevision returns the latest committed revision of the record v
// revises, or nil when none is committed. It decodes only that revision.
func (r *Repository) latestRevision(ctx context.Context, q querier, v Record) (Record, error) {
	h := v.header()
	var body string
	err := q.QueryRowContext(ctx, "SELECT body FROM records WHERE path = ? AND schema = ? AND id = ? ORDER BY revision DESC LIMIT 1", recordPath(v), h.Schema, h.ID).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	previous, err := r.decodeRow(body)
	if err != nil {
		return nil, fmt.Errorf("%s: latest revision of %s: %w", recordPath(v), h.ID, err)
	}
	return previous, nil
}

// documentIdentity returns the record ID of the document at path in scope's
// documents, if any revision of one is committed.
func documentIdentity(ctx context.Context, q querier, scope config.WorkstreamID, path string) (string, bool, error) {
	var id string
	err := q.QueryRowContext(ctx, "SELECT id FROM records WHERE schema = 'osmia.trace.document' AND path = ? AND CASE WHEN json_valid(body) THEN json_extract(body, '$.path') END = ? ORDER BY seq LIMIT 1", scopePath(Header{Workstream: scope})+"documents.jsonl", path).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

// latestDocument returns the latest revision of the document at path in
// scope's documents. A missing document is fs.ErrNotExist.
func (r *Repository) latestDocument(ctx context.Context, q querier, scope config.WorkstreamID, path string) (Document, error) {
	var body string
	err := q.QueryRowContext(ctx, "SELECT body FROM records WHERE schema = 'osmia.trace.document' AND path = ? AND CASE WHEN json_valid(body) THEN json_extract(body, '$.path') END = ? ORDER BY seq DESC LIMIT 1", scopePath(Header{Workstream: scope})+"documents.jsonl", path).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return Document{}, fmt.Errorf("%s%s: %w", scopePath(Header{Workstream: scope}), path, fs.ErrNotExist)
	}
	if err != nil {
		return Document{}, err
	}
	v, err := r.decodeRow(body)
	if err != nil {
		return Document{}, err
	}
	d, ok := v.(Document)
	if !ok {
		return Document{}, fmt.Errorf("%s: not a document", path)
	}
	return d, nil
}

// recordedIDs returns which of ids have a record committed at path.
func recordedIDs(ctx context.Context, q querier, path string, ids []string) (map[string]bool, error) {
	found := map[string]bool{}
	for _, id := range ids {
		var one int
		err := q.QueryRowContext(ctx, "SELECT 1 FROM records WHERE path = ? AND id = ? LIMIT 1", path, id).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		found[id] = true
	}
	return found, nil
}

// file reads the trace file name: a row of files, or the latest revision of
// the document at that logical path. A document whose latest revision is
// empty knowledge-base prose was removed. A missing file is fs.ErrNotExist.
func (r *Repository) file(name string) ([]byte, error) {
	db := r.reads
	if db == nil {
		return nil, fmt.Errorf("%w: trace repository is closed", ErrLocked)
	}
	ctx := context.Background()
	var content []byte
	err := db.QueryRowContext(ctx, "SELECT content FROM files WHERE path = ?", name).Scan(&content)
	if err == nil {
		return content, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	scope, path := config.WorkstreamID(""), name
	if rest, ok := cutWorkstreamPrefix(name); ok {
		scope, path = rest.stream, rest.path
	}
	if documentPath(path, scope != "") != nil {
		return nil, fmt.Errorf("%s: %w", name, fs.ErrNotExist)
	}
	d, err := r.latestDocument(ctx, db, scope, path)
	if err != nil {
		return nil, err
	}
	if scope == "" && prose(d.Path) && d.Content == "" {
		return nil, fmt.Errorf("%s: %w", name, fs.ErrNotExist)
	}
	return []byte(d.Content), nil
}

type scopedPath struct {
	stream config.WorkstreamID
	path   string
}

// cutWorkstreamPrefix splits workstreams/<w>/<path> into its workstream and
// path.
func cutWorkstreamPrefix(name string) (scopedPath, bool) {
	const prefix = "workstreams/"
	if len(name) <= len(prefix) || name[:len(prefix)] != prefix {
		return scopedPath{}, false
	}
	rest := name[len(prefix):]
	for i := 0; i < len(rest); i++ {
		if rest[i] == '/' {
			id, err := config.ParseWorkstreamID(rest[:i])
			if err != nil {
				return scopedPath{}, false
			}
			return scopedPath{id, rest[i+1:]}, true
		}
	}
	return scopedPath{}, false
}

func clonePointer[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func cloneRecord(record Record) Record {
	switch v := record.(type) {
	case Question:
		v.Escalation = clonePointer(v.Escalation)
		if v.Escalation != nil {
			v.Escalation.Questions = slices.Clone(v.Escalation.Questions)
			v.Escalation.Options = slices.Clone(v.Escalation.Options)
		}
		return v
	case Amendment:
		v.Citations = slices.Clone(v.Citations)
		v.Upstream = clonePointer(v.Upstream)
		return v
	case Ruling:
		v.Citations = slices.Clone(v.Citations)
		v.Changes = slices.Clone(v.Changes)
		return v
	case TurnRequest:
		v.Resume = clonePointer(v.Resume)
		return v
	case TurnResponse:
		v.Result.Outcome = clonePointer(v.Result.Outcome)
		if v.Result.Outcome != nil {
			v.Result.Outcome.Card = clonePointer(v.Result.Outcome.Card)
		}
		v.Result.ToolCounts = maps.Clone(v.Result.ToolCounts)
		v.Result.Limit = clonePointer(v.Result.Limit)
		v.Classification = clonePointer(v.Classification)
		if v.Classification != nil {
			v.Classification.ToolCounts = maps.Clone(v.Classification.ToolCounts)
		}
		v.ClassifierUsage = slices.Clone(v.ClassifierUsage)
		v.Stop = clonePointer(v.Stop)
		return v
	case Status:
		v.Agents = slices.Clone(v.Agents)
		return v
	case PriorityChange:
		v.Order = slices.Clone(v.Order)
		return v
	case Document, Transition, Agent, Cost, nil:
		return record
	default:
		panic(fmt.Sprintf("unsupported cached record %T", record))
	}
}
