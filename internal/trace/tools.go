package trace

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

// ToolPayload retains a bounded body and the digest and size of the complete
// value. Large results remain identifiable without bloating every trace read.
type ToolPayload struct {
	SHA256  string `json:"sha256"`
	Bytes   int    `json:"bytes"`
	Content string `json:"content,omitempty"`
}

type ToolCall struct {
	Scope  coreadapter.Scope      `json:"scope"`
	Name   string                 `json:"name"`
	Effect coreadapter.ToolEffect `json:"effect"`
	Input  ToolPayload            `json:"input"`
	Output *ToolPayload           `json:"output,omitempty"`
	State  string                 `json:"state"`
	Failed bool                   `json:"failed,omitempty"`
}

func toolPayload(raw []byte) ToolPayload {
	out := ToolPayload{SHA256: fmt.Sprintf("%x", sha256.Sum256(raw)), Bytes: len(raw)}
	if len(raw) <= 65536 {
		out.Content = string(raw)
	}
	return out
}

// ToolCallID gives an idempotent operation a stable identity across retries of
// the same turn and JSON call. Object property ordering does not affect it.
func ToolCallID(scope coreadapter.Scope, name string, raw json.RawMessage) string {
	var value any
	if json.Unmarshal(raw, &value) == nil {
		raw, _ = json.Marshal(value)
	}
	data, _ := json.Marshal([]string{scope.Project, scope.Workstream, scope.Thread, scope.Turn, name, string(raw)})
	return fmt.Sprintf("tool-%x", sha256.Sum256(data))
}

// BeginTool records the call before its handler runs. A missing completion
// after restart means the effect is uncertain, never that it did not happen.
func (r *Repository) BeginTool(ctx context.Context, scope coreadapter.Scope, tool coreadapter.Tool, raw json.RawMessage, now func() time.Time) (func(context.Context, json.RawMessage, error) error, error) {
	stream := config.WorkstreamID(scope.Workstream)
	r, err := r.scoped(stream)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	log, _, err := r.loadWorkflow(stream)
	if err != nil {
		return nil, err
	}
	var actor string
	var turn QueuedTurn
	for id, t := range log.Threads {
		if t.Identity.ThreadID == scope.Thread && t.Identity.Role == scope.Role {
			_, turn, err = r.turnScope(id, scope, true)
			if err != nil {
				return nil, err
			}
			actor = id
			break
		}
	}
	if actor == "" {
		return nil, errors.New("tool call has no active turn")
	}
	if r.db == nil {
		return nil, fmt.Errorf("%w: trace repository is closed", ErrLocked)
	}
	documents := scopePath(Header{Workstream: stream}) + "documents.jsonl"
	prefix := fmt.Sprintf("tool-%x-", sha256.Sum256([]byte(turn.Request.ID)))
	sequence, err := toolCallsBegun(ctx, r.db, documents, prefix)
	if err != nil {
		return nil, err
	}
	id := fmt.Sprintf("%s%d", prefix, sequence+1)
	if tool.Name == "preview" {
		id = ToolCallID(scope, tool.Name, raw)
	}
	call := ToolCall{Scope: scope, Name: tool.Name, Effect: tool.Effect, Input: toolPayload(raw), State: "started"}
	data, err := json.Marshal(call)
	if err != nil {
		return nil, err
	}
	doc := Document{Header: Header{Schema: "osmia.trace.document", Version: Version, ID: id, Revision: 1, Project: r.project, Workstream: stream, Unit: scope.Unit, At: now(), Actor: Actor{Kind: "agent", ID: actor}, Cause: turn.Request.ID, Depth: turn.Request.Depth + 1}, Path: "tools/" + id + ".json", Content: string(data)}
	existing := false
	if tool.Name == "preview" {
		previous, err := r.latestRevision(ctx, r.db, doc)
		if err != nil {
			return nil, err
		}
		if d, ok := previous.(Document); ok {
			if d.Revision == 2 {
				return func(context.Context, json.RawMessage, error) error { return nil }, nil
			}
			doc, existing = d, true
		}
	}
	if !existing {
		files, err := r.documentFiles(ctx, []Document{doc})
		if err != nil {
			return nil, err
		}
		if err := r.publish(ctx, files); err != nil {
			return nil, err
		}
	}
	return func(ctx context.Context, output json.RawMessage, callErr error) error {
		if tool.Name == "preview" {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.db == nil {
				return fmt.Errorf("%w: trace repository is closed", ErrLocked)
			}
			previous, err := r.latestRevision(ctx, r.db, doc)
			if err != nil {
				return err
			}
			if previous != nil && previous.header().Revision == 2 {
				return nil
			}
		}
		call.State, call.Failed = "completed", callErr != nil
		value := toolPayload(output)
		call.Output = &value
		data, err := json.Marshal(call)
		if err != nil {
			return err
		}
		doc.Revision, doc.At, doc.Content = 2, now(), string(data)
		if tool.Name == "preview" {
			files, err := r.documentFiles(ctx, []Document{doc})
			if err != nil {
				return err
			}
			return r.publish(ctx, files)
		}
		return r.RecordDocuments(ctx, []Document{doc})
	}, nil
}

func toolCallsBegun(ctx context.Context, q querier, documents, prefix string) (int, error) {
	upper := prefix[:len(prefix)-1] + "."
	var n int
	err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM records WHERE path = ? AND id >= ? AND id < ? AND revision = 1 AND schema = 'osmia.trace.document'", documents, prefix, upper).Scan(&n)
	return n, err
}
