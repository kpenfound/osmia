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
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/trace"
)

func inspectCode(cfg *config.Config, repo *trace.Repository, scope coreadapter.Scope, now func() time.Time) coreadapter.Tool {
	description := "Read a committed file or list a directory of the project's code. This returns code, not factory records; use factory_context for those. " +
		"path defaults to the project root: a file returns its text and a directory returns its file names. start (a one-based line) and lines (default 200, maximum 1000) select part of a file or of a listing; an excerpt over 65536 bytes is refused, so ask for fewer lines. " +
		"Set unit to read that unit's latest recorded candidate. Set captured to true, with unit, to read its unfinished changes since its last report. A captured read waits for the unit's writer to finish its turn, and the service then takes the snapshot itself. A unit with no completed report is read the same way. The returned commit identifies the evidence. " +
		"Cite the returned inspection citation when you answer from code."
	if scope.Role == trace.ChiefOfStaff {
		description += " workstream: a member workstream ID of your workgroup whose code and units to read. Omit it for this conversation's own workstream."
	}
	return coreadapter.Tool{Name: "inspect_code", Effect: coreadapter.ToolMemory, Description: description, InputSchema: memberSchema(scope, `"captured":{"type":"boolean"},"unit":{"type":"string"},"path":{"type":"string"},"start":{"type":"integer"},"lines":{"type":"integer"}`), Handle: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		if scope.Role != trace.ChiefOfStaff && scope.Role != architectRole {
			return nil, errors.New("inspect_code is not granted to this role")
		}
		if err := repo.ActiveTurn(scope); err != nil {
			return nil, err
		}
		var in struct {
			Captured   bool                `json:"captured"`
			Unit       string              `json:"unit"`
			Path       string              `json:"path"`
			Start      int                 `json:"start"`
			Lines      int                 `json:"lines"`
			Workstream config.WorkstreamID `json:"workstream"`
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&in); err != nil {
			return nil, err
		}
		target, stream, refusal, err := chiefMember(repo, scope, in.Workstream)
		if err != nil {
			return nil, err
		}
		if refusal != "" {
			return priorityRefusal(refusal)
		}
		project := cfg
		if target != repo {
			project = cfg.For(target.Project())
		}
		if in.Captured && in.Unit == "" {
			return nil, errors.New("captured needs unit: name the unit whose unfinished changes to read")
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
			return nil, errors.New("invalid code range: path must be a relative path inside the project, start and lines at least 1, lines at most 1000")
		}
		g, err := featureWorkspaces(project, target).Of(stream)
		if err != nil {
			return nil, err
		}
		commit, exists, err := g.Branch(ctx, featureBranch(stream))
		if err != nil {
			return nil, err
		}
		if !exists {
			cmd := exec.CommandContext(ctx, "git", "-C", project.Project.Clone, "rev-parse", "--verify", "HEAD^{commit}")
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0"}
			out, err := cmd.Output()
			if err != nil {
				return nil, err
			}
			commit = strings.TrimSpace(string(out))
		}
		if in.Unit != "" {
			if _, err := config.ParseWorkstreamID(string(stream)); err != nil {
				return nil, err
			}
			docs, err := trace.Read[trace.Document](target, stream)
			if err != nil {
				return nil, err
			}
			var report UnitReport
			for _, doc := range docs {
				if doc.ID == reportDocument(in.Unit) {
					if err := json.Unmarshal([]byte(doc.Content), &report); err != nil {
						return nil, err
					}
				}
			}
			if report.Candidate == "" || in.Captured {
				idle, err := revisionWorkersIdle(target, stream, []string{in.Unit})
				if err != nil {
					return nil, err
				}
				if !idle {
					return nil, errors.New("unit writer is still running; inspect its durable activity with factory_context and retry when it finishes")
				}
				_, _, commit, err = newUnitWorkspaces(project, target).snapshot(ctx, stream, in.Unit)
				if err != nil {
					return nil, err
				}
			} else {
				commit = report.Candidate
			}
		}
		read, err := intake.ReadCommitPath(ctx, project.Project.Clone, commit, in.Path)
		if err != nil {
			return nil, err
		}
		if read.Dir {
			names := read.Names
			start := min(len(names), in.Start-1)
			end := min(len(names), start+in.Lines)
			return json.Marshal(map[string]any{"commit": commit, "path": in.Path, "files": names[start:end], "total": len(names)})
		}
		data := read.Data
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
