package trace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

const missingNotesSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// NotesTools binds private project/role memory to a queued turn. Handlers accept
// no path or scope selectors and recheck the active turn on every access.
func (r *Repository) NotesTools(agent string, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, _, err := r.turnScope(agent, scope, false); err != nil {
		return nil, err
	}
	read := coreadapter.Tool{Name: "notes_read", Description: "Read this role's private project notes before writing; use the returned sha256 as the write precondition.", Effect: coreadapter.ToolRead,
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
		if _, _, err := r.turnScope(agent, scope, true); err != nil {
			return nil, err
		}
		content, err := r.readFile("notes/" + scope.Role + ".md")
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		digest := missingNotesSHA256
		if err == nil {
			sum := sha256.Sum256(content)
			digest = hex.EncodeToString(sum[:])
		}
		return json.Marshal(struct {
			Text   string `json:"text"`
			SHA256 string `json:"sha256"`
		}{string(content), digest})
	}
	write := coreadapter.Tool{Name: "notes_write", Description: "Replace this role's private project notes using the sha256 from notes_read as expected_sha256. On a conflict, read again, merge the new notes with your changes, and retry using the new sha256.", Effect: coreadapter.ToolMemory,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"},"expected_sha256":{"type":"string"}},"required":["text","expected_sha256"],"additionalProperties":false}`)}
	write.Handle = func(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
		var args struct {
			Text           *string `json:"text"`
			ExpectedSHA256 *string `json:"expected_sha256"`
		}
		if err := decode(input, &args); err != nil {
			return nil, err
		}
		if args.Text == nil || args.ExpectedSHA256 == nil || len(*args.Text) > 64*1024 {
			return nil, fmt.Errorf("notes require text of at most 65536 bytes")
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, _, err := r.turnScope(agent, scope, true); err != nil {
			return nil, err
		}
		name := "notes/" + scope.Role + ".md"
		if err := r.checked(name); err != nil {
			return nil, err
		}
		current, err := r.readFile(name)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		currentSHA256 := missingNotesSHA256
		if err == nil {
			sum := sha256.Sum256(current)
			currentSHA256 = hex.EncodeToString(sum[:])
		}
		if *args.ExpectedSHA256 != currentSHA256 {
			return json.Marshal(struct {
				Written bool   `json:"written"`
				Reason  string `json:"reason"`
				SHA256  string `json:"sha256"`
			}{false, "notes changed since they were read; re-read, merge, and retry", currentSHA256})
		}
		if err := r.publish(ctx, map[string][]byte{name: []byte(*args.Text)}); err != nil {
			return nil, err
		}
		return json.RawMessage(`{"written":true}`), nil
	}
	return []coreadapter.Tool{read, write}, nil
}

// turnScope requires r.mu. It returns the thread and turn a tool scope names;
// active also requires this session's uncaptured claim on that turn.
func (r *Repository) turnScope(agent string, scope coreadapter.Scope, active bool) (Thread, QueuedTurn, error) {
	if scope.Project != string(r.project) || !key(agent) || !key(scope.Role) || !key(scope.Thread) || !key(scope.Turn) {
		return Thread{}, QueuedTurn{}, fmt.Errorf("turn scope denied")
	}
	stream := config.WorkstreamID(scope.Workstream)
	if err := config.CheckWorkstreamIDs(stream); err != nil {
		return Thread{}, QueuedTurn{}, err
	}
	log, _, err := r.loadWorkflow(stream)
	if err != nil {
		return Thread{}, QueuedTurn{}, err
	}
	t, ok := log.Threads[agent]
	if !ok || t.Identity.Role != scope.Role || t.Identity.ThreadID != scope.Thread || (active && t.Active != scope.Turn) {
		return Thread{}, QueuedTurn{}, fmt.Errorf("turn scope denied")
	}
	for _, q := range t.Turns {
		if q.Request.TurnID == scope.Turn && q.Request.Unit == scope.Unit {
			if active && (q.Claim == nil || q.Claim.ServiceSession != r.session || q.Response != nil) {
				return Thread{}, QueuedTurn{}, ErrClaim
			}
			return t, q, nil
		}
	}
	return Thread{}, QueuedTurn{}, fmt.Errorf("turn not found")
}
