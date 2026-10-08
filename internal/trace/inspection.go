package trace

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

// CodeInspection fixes the source behind a chief's code-based answer.
type CodeInspection struct {
	Commit  string `json:"commit"`
	Path    string `json:"path"`
	Start   int    `json:"start"`
	Content string `json:"content"`
	SHA256  string `json:"sha256"`
}

func (r *Repository) RecordCodeInspection(ctx context.Context, scope coreadapter.Scope, inspection CodeInspection, at time.Time) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	agent := ChiefOfStaff
	if scope.Role == "architect" {
		agent = "agent_architect"
	}
	_, turn, err := r.turnScope(agent, scope, true)
	if err != nil {
		return "", err
	}
	if scope.Role != ChiefOfStaff && scope.Role != "architect" {
		return "", fmt.Errorf("code inspection is not granted")
	}
	data, err := json.Marshal(inspection)
	if err != nil {
		return "", err
	}
	id := fmt.Sprintf("inspection-%x", sha256.Sum256(append([]byte(turn.Request.ID), data...)))
	records, _, err := r.scan()
	if err != nil {
		return "", err
	}
	for _, record := range records {
		if d, ok := record.(Document); ok && d.ID == id && d.Workstream == config.WorkstreamID(scope.Workstream) {
			return "inspection#" + id, nil
		}
	}
	h := turn.Request.Header
	h.Schema, h.ID, h.Revision, h.At, h.Cause = "osmia.trace.document", id, 1, at, turn.Request.ID
	h.Actor = Actor{Kind: "agent", ID: agent}
	h.Depth++
	doc := Document{Header: h, Path: "inspections/" + id + ".json", Content: string(data)}
	files, removed, err := r.documentFiles(ctx, []Document{doc})
	if err != nil {
		return "", err
	}
	return "inspection#" + id, r.publishTree(ctx, files, removed)
}

func (r *Repository) CodeInspection(stream config.WorkstreamID, id string) (CodeInspection, error) {
	docs, err := Read[Document](r, stream)
	if err != nil {
		return CodeInspection{}, err
	}
	for _, d := range docs {
		if d.ID == id && d.Path == "inspections/"+id+".json" {
			var inspected CodeInspection
			err := json.Unmarshal([]byte(d.Content), &inspected)
			return inspected, err
		}
	}
	return CodeInspection{}, fmt.Errorf("inspection is not recorded")
}
