package trace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kpenfound/osmia/internal/coreadapter"
)

// ProjectNotice carries the chief's message and the exact turn that published it.
// It informs subsequent turns; it is not an owner ruling or a charter rule.
type ProjectNotice struct {
	Text  string            `json:"text"`
	Scope coreadapter.Scope `json:"scope"`
}

// NotifyTool binds project-wide notices to a chief-of-staff turn. Repeating the
// same call in the same turn returns the existing record.
func (r *Repository) NotifyTool(scope coreadapter.Scope, now func() time.Time) coreadapter.Tool {
	tool := coreadapter.Tool{Name: "notify", Effect: coreadapter.ToolMemory,
		Description: "Publish an informational notice to subsequent turns throughout this project. Cite the evidence in text. This does not make an owner ruling, edit the charter, or send an external message.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}`)}
	tool.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var in struct {
			Text string `json:"text"`
		}
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if strings.TrimSpace(in.Text) == "" || len(in.Text) > 16384 || !utf8.ValidString(in.Text) || strings.ContainsRune(in.Text, 0) {
			return nil, errors.New("notice requires UTF-8 text of at most 16384 bytes")
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if scope.Role != ChiefOfStaff {
			return nil, errors.New("only the chief of staff can publish notices")
		}
		_, turn, err := r.turnScope(ChiefOfStaff, scope, true)
		if err != nil {
			return nil, err
		}
		data, err := json.Marshal(ProjectNotice{Text: in.Text, Scope: scope})
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		id := "notice-" + hex.EncodeToString(sum[:16])
		records, _, err := r.scan()
		if err != nil {
			return nil, err
		}
		for _, record := range records {
			if d, ok := record.(Document); ok && d.Workstream == "" && d.ID == id {
				return json.Marshal(d)
			}
		}
		h := turn.Request.Header
		h.Schema, h.ID, h.Revision, h.Workstream, h.Unit, h.At, h.Cause = "osmia.trace.document", id, 1, "", "", now().UTC(), turn.Request.ID
		h.Actor = Actor{Kind: "agent", ID: ChiefOfStaff}
		h.Depth++
		d := Document{Header: h, Path: "notices/" + id + ".json", Content: string(data)}
		files, removed, err := r.documentFiles(ctx, []Document{d})
		if err != nil {
			return nil, err
		}
		if err := r.publishTree(ctx, files, removed); err != nil {
			return nil, err
		}
		return json.Marshal(d)
	}
	return tool
}
