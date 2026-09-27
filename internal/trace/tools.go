package trace

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

// BeginTool records the call before its handler runs. A missing completion
// after restart means the effect is uncertain, never that it did not happen.
func (r *Repository) BeginTool(ctx context.Context, scope coreadapter.Scope, tool coreadapter.Tool, raw json.RawMessage, now func() time.Time) (func(context.Context, json.RawMessage, error) error, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	stream := config.WorkstreamID(scope.Workstream)
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
	records, _, err := r.scan()
	if err != nil {
		return nil, err
	}
	prefix := fmt.Sprintf("tool-%x-", sha256.Sum256([]byte(turn.Request.ID)))
	sequence := 1
	for _, record := range records {
		if d, ok := record.(Document); ok && d.Workstream == stream && strings.HasPrefix(d.ID, prefix) && d.Revision == 1 {
			sequence++
		}
	}
	id := fmt.Sprintf("%s%d", prefix, sequence)
	call := ToolCall{Scope: scope, Name: tool.Name, Effect: tool.Effect, Input: toolPayload(raw), State: "started"}
	if strings.HasPrefix(tool.Name, "notes_") {
		call.Input.Content = ""
	}
	data, err := json.Marshal(call)
	if err != nil {
		return nil, err
	}
	doc := Document{Header: Header{Schema: "osmia.trace.document", Version: Version, ID: id, Revision: 1, Project: r.project, Workstream: stream, Unit: scope.Unit, At: now(), Actor: Actor{Kind: "agent", ID: actor}, Cause: turn.Request.ID, Depth: turn.Request.Depth + 1}, Path: "tools/" + id + ".json", Content: string(data)}
	files, removed, err := r.documentFiles(ctx, []Document{doc})
	if err != nil {
		return nil, err
	}
	if err := r.publishTree(ctx, files, removed); err != nil {
		return nil, err
	}
	return func(ctx context.Context, output json.RawMessage, callErr error) error {
		call.State, call.Failed = "completed", callErr != nil
		value := toolPayload(output)
		if strings.HasPrefix(tool.Name, "notes_") {
			value.Content = ""
		}
		call.Output = &value
		data, err := json.Marshal(call)
		if err != nil {
			return err
		}
		doc.Revision, doc.At, doc.Content = 2, now(), string(data)
		return r.RecordDocuments(ctx, []Document{doc})
	}, nil
}
