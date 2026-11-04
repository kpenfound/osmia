package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/trace"
)

// showImageTool reads from the active view. os.Root also confines the open if a
// symlink changes between resolution and reading.
func showImageTool(repository *trace.Repository, scope coreadapter.Scope, view *isolation.FileView, now func() time.Time) coreadapter.Tool {
	return coreadapter.Tool{Name: "show_image", Description: "Attach a PNG or JPEG from your current worktree to the session chat (at most 5 MiB)", Effect: coreadapter.ToolMemory,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`),
		Handle: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			if err := repository.ActiveTurn(scope); err != nil {
				return nil, err
			}
			var in struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, err
			}
			data, err := activeImage(view.Workspace().Directory, in.Path)
			if err != nil {
				return nil, err
			}
			kind := http.DetectContentType(data)
			// Bind the key to the turn, path and bytes, so retrying a call is harmless,
			// while replacing a screenshot within a turn can attach a new image.
			key := fmt.Sprintf("%s/%s/%s/%x", scope.Thread, scope.Turn, in.Path, sha256.Sum256(data))
			h := trace.Header{Project: repository.Project(), Workstream: config.WorkstreamID(scope.Workstream), At: now(), Actor: trace.Actor{Kind: "agent", ID: scope.Thread}, Cause: scope.Turn}
			v, err := repository.RecordImage(ctx, h, key, kind, data, scope.Turn)
			if err != nil {
				return nil, err
			}
			return json.Marshal(imageFeedEntry(v).Image)
		}}
}

func activeImage(directory, name string) ([]byte, error) {
	base, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return nil, err
	}
	path := name
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(base, resolved)
	if name == "" || err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return nil, fmt.Errorf("image path must resolve inside the worktree")
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if isolation.VCSMetadata(part) {
			return nil, fmt.Errorf("VCS metadata is not exposed")
		}
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > trace.MaxImageBytes {
		return nil, fmt.Errorf("image must be a regular file of at most 5 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(f, trace.MaxImageBytes+1))
	if err != nil {
		return nil, err
	}
	if err := trace.ValidateImage(http.DetectContentType(data), data); err != nil {
		return nil, err
	}
	return data, nil
}
