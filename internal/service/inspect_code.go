package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

func inspectCode(cfg *config.Config, repo *trace.Repository, scope coreadapter.Scope, now func() time.Time) coreadapter.Tool {
	return coreadapter.Tool{Name: "inspect_code", Effect: coreadapter.ToolRead, Description: "Read committed project code or list a directory. Use the returned inspection citation when answering from code; this queues a librarian knowledge-gap refresh. No checkout or branch can be changed. Path defaults to the root; start is a one-based line, lines defaults to 200 (maximum 1000).", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"start":{"type":"integer"},"lines":{"type":"integer"}},"additionalProperties":false}`), Handle: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		if scope.Role != trace.ChiefOfStaff {
			return nil, errors.New("only the chief of staff inspects code")
		}
		if err := repo.ActiveTurn(scope); err != nil {
			return nil, err
		}
		var in struct {
			Path  string `json:"path"`
			Start int    `json:"start"`
			Lines int    `json:"lines"`
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&in); err != nil {
			return nil, err
		}
		if in.Path == "" {
			in.Path = "."
		}
		if in.Start == 0 {
			in.Start = 1
		}
		if in.Lines == 0 {
			in.Lines = 200
		}
		if !fs.ValidPath(in.Path) || in.Start < 1 || in.Lines < 1 || in.Lines > 1000 {
			return nil, errors.New("invalid code range")
		}
		g, err := featureWorkspaces(cfg, repo).of(config.WorkstreamID(scope.Workstream))
		if err != nil {
			return nil, err
		}
		commit, exists, err := g.Branch(ctx, featureBranch(config.WorkstreamID(scope.Workstream)))
		if err != nil {
			return nil, err
		}
		if !exists {
			cmd := exec.CommandContext(ctx, "git", "-C", cfg.Project.Clone, "rev-parse", "--verify", "HEAD^{commit}")
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0"}
			out, err := cmd.Output()
			if err != nil {
				return nil, err
			}
			commit = strings.TrimSpace(string(out))
		}
		dir, err := os.MkdirTemp("", "osmia-inspection-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(dir)
		if err := copyCommit(ctx, cfg.Project.Clone, commit, dir); err != nil {
			return nil, err
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		stat, err := root.Stat(in.Path)
		if err != nil {
			return nil, err
		}
		if stat.IsDir() {
			entries, err := os.ReadDir(filepath.Join(dir, filepath.FromSlash(in.Path)))
			if err != nil {
				return nil, err
			}
			names := []string{}
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
			slices.Sort(names)
			start := min(len(names), in.Start-1)
			end := min(len(names), start+in.Lines)
			return json.Marshal(map[string]any{"commit": commit, "path": in.Path, "files": names[start:end], "total": len(names)})
		}
		data, err := root.ReadFile(in.Path)
		if err != nil {
			return nil, err
		}
		lines := strings.Split(string(data), "\n")
		start := min(len(lines), in.Start-1)
		end := min(len(lines), start+in.Lines)
		content := strings.Join(lines[start:end], "\n")
		if len(content) > 65536 {
			return nil, errors.New("code excerpt exceeds 65536 bytes; request fewer lines")
		}
		inspection := trace.CodeInspection{Commit: commit, Path: in.Path, Start: in.Start, Content: content, SHA256: fmt.Sprintf("%x", sha256.Sum256(data))}
		citation, err := repo.RecordCodeInspection(ctx, scope, inspection, now())
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct {
			trace.CodeInspection
			Citation string `json:"citation"`
		}{inspection, citation})
	}}
}
