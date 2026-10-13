package service

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

const projectContextTool = "project_context"

// projectContext exposes shared knowledge, never another workstream's private
// records or a role's notes. Offsets page sorted latest records or exact content.
func projectContext(repo *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	return coreadapter.Tool{Name: projectContextTool, Effect: coreadapter.ToolRead,
		Description: "List, search or read this project's charter, KB and informational notices. No path lists up to 20 current records; query filters path/content and prefix narrows the namespace (kb/ or notices/). Listing offset is a record index. With path, offset is a byte offset and content is limited to 16 KiB; reuse the returned revision for consistent continuation. next_offset continues either operation; -1 means complete. revision retrieves historical evidence. Notices are informational, not owner rulings. Use factory_context for your workstream's records.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"prefix":{"type":"string"},"query":{"type":"string","maxLength":256},"revision":{"type":"integer","minimum":1},"offset":{"type":"integer","minimum":0}},"additionalProperties":false}`),
		Handle: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			if err := repo.ActiveTurn(scope); err != nil {
				return nil, err
			}
			var in struct {
				Path, Prefix, Query string
				Revision, Offset    int
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, err
			}
			if in.Offset < 0 || in.Revision < 0 || len(in.Query) > 1024 {
				return nil, errors.New("invalid project context range")
			}
			docs, err := trace.Read[trace.Document](repo, "")
			if err != nil {
				return nil, err
			}
			latest := map[string]trace.Document{}
			for _, d := range docs {
				if d.Path != "charter.md" && !strings.HasPrefix(d.Path, "kb/") && !strings.HasPrefix(d.Path, "notices/") {
					continue
				}
				if in.Path != "" && in.Revision > 0 && d.Revision != in.Revision {
					continue
				}
				if old := latest[d.Path]; d.Revision > old.Revision {
					latest[d.Path] = d
				}
			}
			if in.Path != "" {
				d, ok := latest[in.Path]
				if !ok {
					return nil, errors.New("project document revision not found")
				}
				offset := min(in.Offset, len(d.Content))
				for offset < len(d.Content) && !utf8.RuneStart(d.Content[offset]) {
					offset++
				}
				text := contextExcerpt(d.Content[offset:], 16384)
				next := -1
				if offset+len(text) < len(d.Content) {
					next = offset + len(text)
				}
				return json.Marshal(map[string]any{"path": d.Path, "revision": d.Revision, "offset": offset, "next_offset": next, "total_bytes": len(d.Content), "content": text})
			}
			var paths []string
			query := strings.ToLower(in.Query)
			for path, d := range latest {
				if d.Content != "" && strings.HasPrefix(path, in.Prefix) && (query == "" || strings.Contains(strings.ToLower(path), query) || strings.Contains(strings.ToLower(d.Content), query)) {
					paths = append(paths, path)
				}
			}
			sort.Strings(paths)
			start := min(in.Offset, len(paths))
			end := min(start+20, len(paths))
			items := make([]map[string]any, 0, end-start)
			for _, path := range paths[start:end] {
				d := latest[path]
				items = append(items, map[string]any{"path": path, "revision": d.Revision, "bytes": len(d.Content), "at": d.At})
			}
			next := -1
			if end < len(paths) {
				next = end
			}
			return json.Marshal(map[string]any{"records": items, "next_offset": next, "total": len(paths)})
		}}
}

func contextExcerpt(text string, limit int) string {
	end := min(len(text), limit)
	for end < len(text) && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end]
}
