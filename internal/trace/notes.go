package trace

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

// NotesTools binds private project/role memory to a queued turn. Handlers accept
// no path or scope selectors and recheck the active turn on every access.
func (r *Repository) NotesTools(agent string, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.notesScope(agent, scope, false); err != nil {
		return nil, err
	}
	read := coreadapter.Tool{Name: "notes_read", Description: "Read this role's private project notes.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)}
	read.Handle = func(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
		var args struct{}
		if err := decode(input, &args); err != nil {
			return nil, err
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := r.notesScope(agent, scope, true); err != nil {
			return nil, err
		}
		content, err := r.readFile("notes/" + scope.Role + ".md")
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		return json.Marshal(struct {
			Text string `json:"text"`
		}{string(content)})
	}
	write := coreadapter.Tool{Name: "notes_write", Description: "Replace this role's private project notes.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}`)}
	write.Handle = func(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
		var args struct {
			Text *string `json:"text"`
		}
		if err := decode(input, &args); err != nil {
			return nil, err
		}
		if args.Text == nil || len(*args.Text) > 64*1024 {
			return nil, fmt.Errorf("notes require text of at most 65536 bytes")
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := r.notesScope(agent, scope, true); err != nil {
			return nil, err
		}
		if err := r.checked("notes/" + scope.Role + ".md"); err != nil {
			return nil, err
		}
		if err := r.publish(ctx, map[string][]byte{"notes/" + scope.Role + ".md": []byte(*args.Text)}); err != nil {
			return nil, err
		}
		return json.RawMessage(`{"written":true}`), nil
	}
	return []coreadapter.Tool{read, write}, nil
}

func (r *Repository) notesScope(agent string, scope coreadapter.Scope, active bool) error {
	if scope.Project != string(r.project) || !key(agent) || !key(scope.Role) || !key(scope.Thread) || !key(scope.Turn) {
		return fmt.Errorf("notes scope denied")
	}
	stream := config.WorkstreamID(scope.Workstream)
	if err := config.CheckWorkstreamIDs(stream); err != nil {
		return err
	}
	log, _, err := r.loadWorkflow(stream)
	if err != nil {
		return err
	}
	t, ok := log.Threads[agent]
	if !ok || t.Identity.Role != scope.Role || t.Identity.ThreadID != scope.Thread || (active && t.Active != scope.Turn) {
		return fmt.Errorf("notes scope denied")
	}
	for _, q := range t.Turns {
		if q.Request.TurnID == scope.Turn && q.Request.Unit == scope.Unit {
			if active && (q.Claim == nil || q.Claim.ServiceSession != r.session || q.Response != nil) {
				return ErrClaim
			}
			return nil
		}
	}
	return fmt.Errorf("notes turn not found")
}
